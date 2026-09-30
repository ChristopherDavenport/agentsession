// Package cas is the content-addressed session store RFC 0002 describes,
// laid out on a filesystem the way git lays out a repository:
//
//	<root>/
//	  objects/contents/<2 hex>/<62 hex>   a loose body, canonical bytes, held once
//	  objects/entries/<2 hex>/<62 hex>    a loose envelope: type, parent, parents, ts, content
//	  objects/pack/pack-<hex>.{pack,idx}  objects packed together, with their index
//	  sessions/<session id>/
//	    header                            the header line
//	    log                               one entry hash and its size per line, in append order
//	    HEAD                              the head's hash, or empty
//	    record                            "record" or "mirror"
//	  locks/<session id>                  the lock of the process holding the session
//	  sweep.lock                          held shared by writers, exclusive by a sweep's last step
//	  gc.lock                             the lock of a running sweep or pack
//	  journal                             one line per commit: the commit point
//
// An entry is stored as two objects, its body under the content hash
// and its envelope under the id, so a body shared by many entries is
// held once and a chain of envelopes verifies without its bodies. A
// session is a ref: a header, a base, a head, and a log of the entries
// it appended. Every object is checked against its name when it is
// read, and one that fails is reported as corrupt by name and place.
//
// Objects are written loose, one file each, and packed later, as git
// does: [Store.Pack] moves loose objects into a pack, and [Store.Sweep]
// repacks everything the store holds into one pack and removes what
// nothing needs. A store of small entries otherwise pays a filesystem
// block for every envelope and every body.
//
// Every write follows one order. The objects go first, idempotently,
// since their bytes are their names. Then one journal record is
// appended, which carries a checksum: that is the commit point, and
// nothing is visible before it, in memory or on disk. Then the log line
// and the head, which are indexes, written without an fsync; Open
// replays the journal against them, in journal order, so a crash after
// the commit point leaves no acknowledged append missing and a crash
// before it leaves nothing behind. Recovery only adds: a log holding an
// entry the journal lacks keeps it, since only damage to the journal
// can cause that, and the damage is reported by [Store.Verify].
//
// Under the default sync policy an append is durable when it returns:
// its objects and their directories are fsynced before the record, and
// the record after. [WithSync] lets a writer acknowledge some appends
// before they are durable, as RFC 0002 allows, and [agentsession.Result]
// says which each append got. A lazy append's objects and record are
// fsynced by this store's next durable commit, objects first. Another
// process's durable commit fsyncs the shared journal too, and may make
// a lazy record durable ahead of its objects; the record says it was
// lazy, so after a crash one whose objects did not survive, or were
// left empty, is an append that was lost, with what its session
// appended after it. A store that recovers a session holding lazy
// appends another process never synced syncs their objects and
// journals that it did, before it appends anything durable after them.
//
// Several processes may share a store on one machine. Each session is
// held by one process at a time through a lock the kernel drops when
// the process exits (flock on unix), which RFC 0002 permits: a store
// may take writers in turn provided it never re-parents an accepted
// append. [WithReadOnly] takes no session lock, so a session another
// process holds can be read, projected and verified while it writes.
// The journal is shared: a record is one line written in one call with
// O_APPEND, so records from different processes do not interleave, and
// the journal is never truncated by a reader; a record a crash cut short
// is skipped and the records after it still count. Recovery is per
// session, done by the process that holds the session's lock when it
// opens it, so no process rewrites the indexes of a session another
// process has open. The sweep keeps everything the journal names and
// every loose object younger than a grace period, as git's collector
// spares a young loose object, so an object written ahead of its record
// is safe. A store on a network filesystem is not supported: O_APPEND is
// not atomic across NFS clients, so records could interleave.
package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/openresponses"
)

// Marks a session carries: whether this store may advance it.
const (
	MarkRecord = "record"
	MarkMirror = "mirror"
)

// ErrMirror is returned by Append and SetHead for a session this store
// holds as a mirror, which only exchange from the record may advance.
var ErrMirror = errors.New("cas: session is a mirror here; only the record may advance it")

// ErrHeadMoved is returned by SetHead when the head is not the expected
// entry, which is how a writer learns that another moved it.
var ErrHeadMoved = errors.New("cas: head is not the expected entry")

// ErrBadName is returned for a session ID or a hash that cannot be used
// as a path: a session ID is letters, digits, '.', '_' and '-' and is
// neither "." nor ".."; a hash is "sha256:" and 64 lowercase hex.
var ErrBadName = errors.New("cas: not a usable name")

// ErrSynthetic is returned by Append for a leaf label carrying
// `synthetic`, which marks a projection's own marker and is never an
// entry a session appends.
var ErrSynthetic = errors.New("cas: a synthetic leaf marker is the projection's, not the session's")

// ErrSessionLocked is [agentsession.ErrSessionLocked]: another process
// holds the session.
var ErrSessionLocked = agentsession.ErrSessionLocked

// ErrSweepRunning is returned by Sweep and Pack when another sweep or
// pack is running on the store.
var ErrSweepRunning = errors.New("cas: a sweep or pack is already running on this store")

// ErrModified is returned when a session the store handed out was
// changed behind its back — an entry appended on it directly, or its
// leaf moved to an entry the store never committed — so the store's
// record and the session disagree and nothing can be built on it.
var ErrModified = errors.New("cas: session was modified outside the store")

func errReadOnly() error { return agentsession.ErrReadOnly }

// SyncPolicy says when an append is durable before it returns.
type SyncPolicy int

const (
	// SyncEveryAppend makes every append durable before it returns. It
	// is the default.
	SyncEveryAppend SyncPolicy = iota
	// SyncOnResponse makes durable a response entry and a function call
	// output, which RFC 0002 asks a writer to, a compaction and a branch
	// summary, which are as expensive to lose, and any entry whose type
	// the header names in records, which RFC 0001 requires to be durable
	// before the side effect it precedes. Each durable append also makes
	// durable every lazy append before it.
	SyncOnResponse
	// SyncNever leaves every append lazy: durable at the next call to
	// [Store.Sync] or [Store.Close], or when the operating system writes
	// it back.
	SyncNever
)

// Option configures a Store.
type Option func(*Store)

// WithSync sets the sync policy.
func WithSync(p SyncPolicy) Option {
	return func(s *Store) { s.policy = p }
}

// WithReadOnly opens the store for reading: Open takes no lock on a
// session, so a session another process is writing can be read,
// projected, exported and verified while it writes, and nothing is
// written, the store's directories and the indexes a crash left stale
// included; recovery is done in memory. Create, Append, Write, SetHead,
// Delete, Import, DeclareRecord, Sweep, Pack, PutBlob, Sync and the
// receiving side of an exchange return [agentsession.ErrReadOnly].
//
// An open session is cached as it is in a writing store, so a session
// read while another process appends to it shows what it held when it
// was opened; call [Store.Release] and open it again to see the rest.
func WithReadOnly() Option {
	return func(s *Store) { s.readOnly = true }
}

// Store is a content-addressed store rooted at a directory.
type Store struct {
	root     string
	readOnly bool
	policy   SyncPolicy
	objs     *objects

	mu   sync.Mutex
	open map[string]*handle
	// owners maps each committed own entry to the sessions whose logs
	// hold it — one entry can be in two, when two sessions append the
	// same entry under the same parent — and prefix holds the entries
	// some session's base path needs. Together they are what the store
	// holds: an object written ahead of its journal record is in neither.
	owners map[string]map[string]bool
	prefix map[string]bool
	// faulty records sessions the index could not read, with why, so
	// one fault is reported at that session's Open and hides no other.
	faulty map[string]error
	// journalDirty is set while a lazy commit's record is unsynced.
	journalDirty bool

	// scan is the journal as far as this store has read it, which
	// replay reads on from. scanMu guards it apart from mu, since List,
	// Verify and a sweep replay without holding mu.
	scanMu sync.Mutex
	scan   *journalScan
}

type handle struct {
	session    *agentsession.Session
	dir        string
	lock       *dirLock
	mark       string
	head       string // the head as the HEAD file has it
	count      int    // entries the store has committed into the session
	diskFormat string // the format the header file names
}

// Open opens or creates the store at root, and replays the journal
// against the logs and heads so a crash between the commit point and
// the indexes leaves no acknowledged append missing. A read-only store
// must exist already.
func Open(root string, opts ...Option) (*Store, error) {
	s := &Store{root: root, objs: newObjects(root), open: map[string]*handle{}, owners: map[string]map[string]bool{}, prefix: map[string]bool{}, faulty: map[string]error{}}
	for _, o := range opts {
		o(s)
	}
	dirs := []string{filepath.Join(root, "objects", "contents"), filepath.Join(root, "objects", "entries"), filepath.Join(root, "sessions"), filepath.Join(root, "locks")}
	if s.readOnly {
		if info, err := os.Stat(filepath.Join(root, "sessions")); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("cas: %s is not a store", root)
		}
	} else {
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return nil, fmt.Errorf("cas: %w", err)
			}
		}
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return nil, fmt.Errorf("cas: packs: %w", err)
	}
	if err := s.index(); err != nil {
		return nil, err
	}
	return s, nil
}

// IsStore reports whether dir looks like the root of a store: it holds
// the sessions and objects directories.
func IsStore(dir string) bool {
	for _, d := range []string{"sessions", "objects"} {
		if info, err := os.Stat(filepath.Join(dir, d)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// --- names and paths ---

func validSessionID(id string) bool {
	if id == "" || id == "." || id == ".." || len(id) > 200 {
		return false
	}
	for _, c := range id {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

func (s *Store) sessionDir(id string) (string, error) {
	if !validSessionID(id) {
		return "", fmt.Errorf("%w: session id %q", ErrBadName, id)
	}
	return filepath.Join(s.root, "sessions", id), nil
}

// mediaOf returns a header's media with the format's default applied,
// so "" and "inline" compare as one value.
func mediaOf(h agentsession.Header) string {
	if h.Media == "" {
		return agentsession.MediaInline
	}
	return h.Media
}

// lockSession takes a session's lock. Lock files live under locks/ and
// are never unlinked, so a holder cannot be left locking an inode that a
// delete or a failed create removed from under it. A read-only store
// takes none.
func (s *Store) lockSession(id string) (*dirLock, error) {
	if s.readOnly {
		return nil, nil
	}
	return lockFile(filepath.Join(s.root, "locks", id))
}

// writeGuard holds the sweep's lock shared, from an object write
// through the commit that names it, so a sweep's last step never runs
// between the two.
func (s *Store) writeGuard(ctx context.Context) (*dirLock, error) {
	return lockShared(ctx, filepath.Join(s.root, "sweep.lock"))
}

func objectPath(dir, hash string) (string, error) {
	if !agentsession.ValidHash(hash) {
		return "", fmt.Errorf("%w: hash %q", ErrBadName, hash)
	}
	hex := strings.TrimPrefix(hash, agentsession.HashPrefix)
	return filepath.Join(dir, hex[:2], hex[2:]), nil
}

// --- file writes ---

// syncDir fsyncs a directory, so a rename or a creation in it survives
// a power loss once the call returns.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// writeFile replaces the file at path with data through a temporary
// file and a rename, so a reader never sees it partial, fsyncing the
// file before the rename when durable is set. The directory is the
// caller's to sync.
func writeFile(path string, data []byte, durable bool) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if durable {
		if serr := tmp.Sync(); werr == nil {
			werr = serr
		}
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return werr
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// writeAtomic replaces the file at path with data durably: through a
// temporary file, an fsync, a rename and an fsync of the directory.
func writeAtomic(path string, data []byte) error {
	if err := writeFile(path, data, true); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeIndex replaces an index file, which the journal rebuilds after a
// crash, without an fsync: a crash leaves the old one or the new one.
func writeIndex(path string, data []byte) error {
	return writeFile(path, data, false)
}

// --- objects ---

// split takes an entry's encoded line apart into the envelope object
// the id hashes and the body the content hash hashes, both canonical.
func split(e agentsession.Entry) (env, body []byte, err error) {
	data, err := agentsession.MarshalEntry(e)
	if err != nil {
		return nil, nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, nil, err
	}
	b := e.Base()
	envObj := map[string]json.RawMessage{
		"type":    all["type"],
		"parent":  all["parent"],
		"ts":      all["ts"],
		"content": json.RawMessage(`"` + b.ContentHash() + `"`),
	}
	if envObj["parent"] == nil {
		envObj["parent"] = json.RawMessage("null")
	}
	if p, ok := all["parents"]; ok && len(b.Parents) > 0 {
		envObj["parents"] = p
	}
	for _, k := range []string{"id", "type", "parent", "parents", "ts", "content"} {
		delete(all, k)
	}
	envJSON, err := json.Marshal(envObj)
	if err != nil {
		return nil, nil, err
	}
	bodyJSON, err := json.Marshal(all)
	if err != nil {
		return nil, nil, err
	}
	if env, err = jcs.Transform(envJSON); err != nil {
		return nil, nil, err
	}
	if body, err = jcs.Transform(bodyJSON); err != nil {
		return nil, nil, err
	}
	return env, body, nil
}

// join rebuilds an entry's line from its envelope object and body, with
// the id set, for a reader to verify and decode.
func join(id string, env, body []byte) ([]byte, error) {
	var e, b map[string]json.RawMessage
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	delete(e, "content")
	for k, v := range e {
		b[k] = v
	}
	b["id"] = json.RawMessage(`"` + id + `"`)
	return json.Marshal(b)
}

// storeEntry writes an entry's two objects loose and returns their
// combined size.
func (s *Store) storeEntry(e agentsession.Entry, durable bool) (int64, error) {
	env, body, err := split(e)
	if err != nil {
		return 0, err
	}
	if err := s.objs.write(spaceContents, e.Base().ContentHash(), body, durable); err != nil {
		return 0, fmt.Errorf("cas: store content: %w", err)
	}
	if err := s.objs.write(spaceEntries, e.Base().ID, env, durable); err != nil {
		return 0, fmt.Errorf("cas: store entry: %w", err)
	}
	return int64(len(env) + len(body)), nil
}

// unpackLimit is how many objects a transfer brings before they arrive
// as a pack rather than loose, as git's fetch.unpackLimit is: a pack for
// every small transfer would leave the store many packs to search.
const unpackLimit = 100

// packEntries stores the objects of entries and blobs a transfer
// brings: what the database lacks as one pack, the way git takes in a
// fetch, or loose when that is fewer than unpackLimit objects; and
// what it holds already through write, which freshens it, writes it
// loose if a sweep removed it, and has the commit that names it sync
// it. It returns each entry's size.
func (s *Store) packEntries(entries []agentsession.Entry, blobs map[string][]byte) (map[string]int64, error) {
	sizes := map[string]int64{}
	var all []packObject
	for _, e := range entries {
		env, body, err := split(e)
		if err != nil {
			return nil, err
		}
		id, ch := e.Base().ID, e.Base().ContentHash()
		sizes[id] = int64(len(env) + len(body))
		all = append(all, packObject{spaceContents, ch, body}, packObject{spaceEntries, id, env})
	}
	for h, data := range blobs {
		all = append(all, packObject{spaceContents, h, data})
	}
	var missing, held []packObject
	for _, o := range all {
		if s.objs.hasQuick(o.sp, o.hash) {
			held = append(held, o)
		} else {
			missing = append(missing, o)
		}
	}
	if len(missing) >= unpackLimit {
		if _, err := writePack(s.objs.packDir(), missing); err != nil {
			return nil, fmt.Errorf("cas: pack: %w", err)
		}
		if err := s.objs.reloadPacks(true); err != nil {
			return nil, err
		}
	} else {
		held = all
	}
	for _, o := range held {
		if err := s.objs.write(o.sp, o.hash, o.data, true); err != nil {
			return nil, fmt.Errorf("cas: store: %w", err)
		}
	}
	return sizes, nil
}

// freshenPath freshens the objects on the path to id, as a write of
// each would, so a fork's prefix is young to the sweep until the fork's
// header lands, and an object a sweep removed from under this store's
// view is written back loose.
func (s *Store) freshenPath(id string) error {
	for id != "" {
		env, err := s.objs.read(spaceEntries, id)
		if err != nil {
			return err
		}
		if err := s.objs.write(spaceEntries, id, env, true); err != nil {
			return err
		}
		c, ok := envelopeContent(env)
		if !ok {
			return fmt.Errorf("%w: entry %s names no content", ErrCorrupt, id)
		}
		body, err := s.objs.read(spaceContents, c)
		if err != nil {
			return err
		}
		if err := s.objs.write(spaceContents, c, body, true); err != nil {
			return err
		}
		if id, err = s.parentOf(id); err != nil {
			return err
		}
	}
	return nil
}

// envelope reads an entry's envelope object.
func (s *Store) envelope(id string) (map[string]json.RawMessage, error) {
	env, err := s.objs.read(spaceEntries, id)
	if err != nil {
		return nil, err
	}
	var e map[string]json.RawMessage
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	return e, nil
}

// contentOf returns the content hash an entry's envelope names.
func (s *Store) contentOf(id string) (string, error) {
	e, err := s.envelope(id)
	if err != nil {
		return "", err
	}
	var c string
	if err := json.Unmarshal(e["content"], &c); err != nil {
		return "", fmt.Errorf("cas: entry %s: content: %w", id, err)
	}
	return c, nil
}

// present reports whether both of an entry's objects are held. Only an
// object that is not there makes it false; any other failure to read
// one is returned, since recovery must not take an unreadable object
// for a lost one and remove what names it.
func (s *Store) present(id string) (bool, error) {
	c, err := s.contentOf(id)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if _, err := s.objs.read(spaceContents, c); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, nil
}

// loadLine reads an entry's two objects, each checked against its name,
// and rebuilds its line.
func (s *Store) loadLine(id string) ([]byte, error) {
	env, err := s.objs.read(spaceEntries, id)
	if err != nil {
		return nil, err
	}
	var e struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	if !agentsession.ValidHash(e.Content) {
		return nil, fmt.Errorf("%w: entry %s names content %q", ErrCorrupt, id, e.Content)
	}
	body, err := s.objs.read(spaceContents, e.Content)
	if err != nil {
		return nil, fmt.Errorf("%w (the content of entry %s)", err, id)
	}
	if err := ijson.Check(env); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	if err := ijson.Check(body); err != nil {
		return nil, fmt.Errorf("cas: content %s: %w", e.Content, err)
	}
	return join(id, env, body)
}

// parentOf reads an entry's parent from its envelope object alone, the
// walk a chain of envelopes allows without bodies.
func (s *Store) parentOf(id string) (string, error) {
	e, err := s.envelope(id)
	if err != nil {
		return "", err
	}
	var parent *string
	if err := json.Unmarshal(e["parent"], &parent); err != nil {
		return "", err
	}
	if parent == nil {
		return "", nil
	}
	return *parent, nil
}

// isLeafLabel reports whether the stored entry is a label carrying the
// reserved leaf value.
func (s *Store) isLeafLabel(id string) (bool, error) {
	line, err := s.loadLine(id)
	if err != nil {
		return false, err
	}
	e, err := agentsession.UnmarshalEntry(line)
	if err != nil {
		return false, err
	}
	l, ok := e.(*agentsession.LabelEntry)
	return ok && l.Label != nil && *l.Label == agentsession.LeafLabel, nil
}

// holds reports whether the store holds the entry: it is in a session's
// log or on a session's prefix. An object written ahead of its journal
// record is in neither.
func (s *Store) holds(id string) bool {
	return len(s.owners[id]) > 0 || s.prefix[id]
}

// own records that session's log holds entry.
func (s *Store) own(entry, session string) {
	set := s.owners[entry]
	if set == nil {
		set = map[string]bool{}
		s.owners[entry] = set
	}
	set[session] = true
}

// anyOwner returns one session whose log holds the entry, or "".
func (s *Store) anyOwner(entry string) (string, bool) {
	for id := range s.owners[entry] {
		return id, true
	}
	return "", false
}

// --- media blobs ---

// sidecarRef matches a sidecar reference inside a content, the URL RFC
// 0001 gives an item that names a blob beside the file.
var sidecarRef = regexp.MustCompile(`sidecar:sha256:[0-9a-f]{64}`)

// PutBlob stores a media blob under its hash, durably, and returns the
// hash. An item names it with a sidecar: URL carrying the hash, and a
// projection of a sidecar session writes it beside the file as the file
// named by the hex digest. A blob is a content object, held once.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.readOnly {
		return "", errReadOnly()
	}
	hash := hashBytes(data)
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return "", err
	}
	defer guard.release()
	if err := s.objs.write(spaceContents, hash, data, true); err != nil {
		return "", fmt.Errorf("cas: store blob: %w", err)
	}
	if err := s.objs.flush(); err != nil {
		return "", fmt.Errorf("cas: store blob: %w", err)
	}
	return hash, nil
}

// Blob returns a media blob by its hash.
func (s *Store) Blob(ctx context.Context, hash string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := s.objs.read(spaceContents, hash)
	if err != nil {
		return nil, fmt.Errorf("cas: blob %s: %w", hash, err)
	}
	return data, nil
}

// blobsNamedBy returns the blob hashes the content names through sidecar
// references.
func blobsNamedBy(content []byte) []string {
	var out []string
	for _, m := range sidecarRef.FindAll(content, -1) {
		out = append(out, strings.TrimPrefix(string(m), "sidecar:"))
	}
	return out
}

// --- recovery ---

// view is one session as the journal and its indexes together say.
type view struct {
	exists  bool
	deleted bool
	log     []string
	sizes   map[string]int64
	head    string
	mark    string
	// damaged is set when the log holds entries the journal lacks, which
	// only damage to the journal, or a journal restored from before the
	// log, can cause. The log is kept.
	damaged bool

	// adopt lists the entries of lazy records the journal has not yet
	// said are durable: a writing store that recovers the session syncs
	// their objects and says so, since a durable append it makes next
	// must not be cut with them by a later crash.
	adopt []string
	// dropped lists the appends found lost that no lost record names
	// yet: a writing store that recovers the session commits one for
	// each, so a sync record after them, or a durable append that goes
	// on from what survived, does not bring them back or cut with them.
	dropped []string

	logChanged, headChanged, markChanged bool
}

// reconcile works out one session's state from the journal scan and the
// session's files, without writing anything. Recovery only adds: the
// log keeps every entry it holds, in its order, and gains what the
// journal says it lacks; the head is the journal's unless the journal
// is behind the log; a lazy append whose objects a crash took is lost,
// with everything the session appended after it, since a durable append
// would have made those objects durable first.
func (s *Store) reconcile(id, dir string, scan *journalScan) (view, error) {
	var v view
	st := scan.states[id]
	if st != nil && st.deleted {
		v.deleted = true
		return v, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
		return v, nil // never finished creating
	}
	v.exists = true
	have, sizes, err := readLog(dir)
	if err != nil {
		return v, err
	}
	fileHead, err := readHead(dir)
	if err != nil {
		return v, err
	}
	fileMark := readMark(dir)
	v.sizes = sizes
	if st == nil {
		v.log, v.head, v.mark = have, fileHead, fileMark
		return v, nil
	}
	// The journal's own view, cut at the first lazy append that was lost.
	var jEntries []string
	seen := map[string]bool{}
	lost := map[string]bool{}
	jHead, hasJHead := "", false
	cut := false
	// A sync record says every lazy record before it is durable, apart
	// from the ones a lost record already said were lost.
	synced := -1
	// lostAt is where the last lost record naming each entry is: an
	// append of it before that is gone, one after it is kept.
	lostAt := map[string]int{}
	for i, r := range st.recs {
		switch r.Op {
		case "sync":
			synced = i
		case "lost":
			lostAt[r.Entry] = i
		}
	}
	gone := func(entry string, i int) bool {
		at, ok := lostAt[entry]
		return ok && at > i
	}
	var dropped []string
	for i, r := range st.recs {
		if cut {
			if r.Op == "append" && r.Entry != "" {
				lost[r.Entry] = true
				if !gone(r.Entry, i) {
					dropped = append(dropped, r.Entry)
				}
			}
			continue
		}
		switch r.Op {
		case "append":
			if r.Entry != "" && gone(r.Entry, i) {
				lost[r.Entry] = true // lost, and recorded so, earlier
				continue
			}
			if r.Lazy && r.Entry != "" && i > synced {
				ok, err := s.present(r.Entry)
				if errors.Is(err, ErrCorrupt) {
					ok, err = false, nil // a lazy object a crash left torn
				}
				if err != nil {
					return v, fmt.Errorf("cas: session %s: %w", id, err)
				}
				if !ok {
					cut = true
					lost[r.Entry] = true
					dropped = append(dropped, r.Entry)
					continue
				}
				v.adopt = append(v.adopt, r.Entry)
			}
			if r.Entry != "" && !seen[r.Entry] {
				seen[r.Entry] = true
				jEntries = append(jEntries, r.Entry)
				if r.Size > 0 {
					v.sizes[r.Entry] = r.Size
				}
			}
			if r.Head != "" {
				jHead, hasJHead = r.Head, true
			}
		case "head":
			if gone(r.Head, i) {
				continue
			}
			jHead, hasJHead = r.Head, true
		}
	}
	for _, e := range jEntries {
		delete(lost, e) // appended again after the loss, and kept
	}
	for _, e := range dropped {
		if lost[e] {
			v.dropped = append(v.dropped, e)
		}
	}
	inLog := map[string]bool{}
	for _, e := range have {
		if lost[e] {
			v.logChanged = true
			continue
		}
		if !seen[e] {
			ok, err := s.present(e)
			if errors.Is(err, ErrCorrupt) && s.emptyLoose(e) {
				// Written with a lazy record the crash took, and left empty
				// by it, as a file renamed before it was synced can be. A
				// corrupt object with bytes in it is damage, and fails.
				ok, err = false, nil
			}
			if err != nil {
				return v, fmt.Errorf("cas: session %s: %w", id, err)
			}
			if !ok {
				// A log line that reached the disk ahead of a lazy record
				// and objects the crash took: an append that was lost.
				lost[e] = true
				v.logChanged = true
				continue
			}
			v.damaged = true
		}
		inLog[e] = true
		v.log = append(v.log, e)
	}
	for _, e := range jEntries {
		if !inLog[e] {
			v.log = append(v.log, e)
			v.logChanged = true
		}
	}
	base := st.base
	if base == "" {
		if hdr, err := readHeader(dir); err == nil {
			base = hdr.Base
		}
	}
	v.head = fileHead
	if hasJHead && !(v.damaged && headIn(fileHead, v.log, base)) {
		v.head = jHead
	}
	if lost[v.head] {
		v.head = jHead
	}
	if v.head == "" && !hasJHead {
		v.head = base // a fork's first head, whose HEAD a crash took
	}
	v.headChanged = v.head != fileHead
	v.mark = fileMark
	if m := st.mark(); m != "" {
		v.mark = m
	}
	v.markChanged = v.mark != fileMark
	return v, nil
}

func headIn(head string, log []string, base string) bool {
	if head == "" {
		return false
	}
	if head == base {
		return true
	}
	for _, e := range log {
		if e == head {
			return true
		}
	}
	return false
}

// recoverSession makes one session's indexes say what reconcile says,
// under that session's lock. A session the journal says was deleted has
// its directory removed. A read-only store writes nothing.
func (s *Store) recoverSession(id, dir string) (view, error) {
	scan, err := s.replay()
	if err != nil {
		return view{}, err
	}
	v, err := s.reconcile(id, dir, scan)
	if err != nil || s.readOnly {
		return v, err
	}
	if v.deleted {
		if err := os.RemoveAll(dir); err != nil {
			return v, err
		}
		return v, nil
	}
	if !v.exists {
		return v, nil
	}
	if v.logChanged {
		var buf bytes.Buffer
		for _, h := range v.log {
			writeLogLine(&buf, h, v.sizes[h])
		}
		if err := writeIndex(filepath.Join(dir, "log"), buf.Bytes()); err != nil {
			return v, err
		}
	}
	if v.headChanged {
		if err := writeHead(dir, v.head); err != nil {
			return v, err
		}
	}
	if v.markChanged {
		if err := writeIndex(filepath.Join(dir, "record"), []byte(v.mark+"\n")); err != nil {
			return v, err
		}
	}
	if len(v.dropped) > 0 {
		// Before the sync record adopt writes, which would otherwise
		// say the lost appends were durable.
		recs := make([]journalRecord, len(v.dropped))
		for i, e := range v.dropped {
			recs[i] = journalRecord{Op: "lost", Session: id, Entry: e}
		}
		if err := s.commit(true, recs...); err != nil {
			return v, err
		}
	}
	if len(v.adopt) > 0 {
		if err := s.adopt(id, v.adopt); err != nil {
			return v, err
		}
	}
	return v, nil
}

// emptyLoose reports whether one of an entry's loose objects is empty.
func (s *Store) emptyLoose(id string) bool {
	check := func(sp space, hash string) bool {
		p, err := s.objs.loosePath(sp, hash)
		if err != nil {
			return false
		}
		info, err := os.Stat(p)
		return err == nil && info.Size() == 0
	}
	if check(spaceEntries, id) {
		return true
	}
	c, err := s.contentOf(id)
	return err == nil && check(spaceContents, c)
}

// adopt makes durable the objects of lazy appends a session holds that
// the journal does not yet say are durable, as the holder recovering it,
// and commits a sync record saying so. A process that wrote them and
// crashed synced nothing; without this, the next durable append made
// here could be cut with them by a later crash.
func (s *Store) adopt(id string, entries []string) error {
	for _, e := range entries {
		if err := s.objs.freshen(spaceEntries, e); err != nil {
			return fmt.Errorf("cas: session %s: %w", id, err)
		}
		c, err := s.contentOf(e)
		if err != nil {
			return fmt.Errorf("cas: session %s: %w", id, err)
		}
		if err := s.objs.freshen(spaceContents, c); err != nil {
			return fmt.Errorf("cas: session %s: %w", id, err)
		}
	}
	return s.commit(true, journalRecord{Op: "sync", Session: id})
}

// index builds what the store holds from the journal, the logs and the
// bases. The journal counts as well as the logs, since a log may lag an
// acknowledged append until its session is next opened.
func (s *Store) index() error {
	scan, err := s.replay()
	if err != nil {
		return err
	}
	for id, st := range scan.states {
		if st.deleted {
			continue
		}
		for _, e := range st.entries() {
			s.own(e, id)
		}
	}
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || !validSessionID(d.Name()) {
			continue
		}
		if st := scan.states[d.Name()]; st != nil && st.deleted {
			continue
		}
		dir := filepath.Join(s.root, "sessions", d.Name())
		hashes, _, err := readLog(dir)
		if err != nil {
			s.faulty[d.Name()] = err
			continue
		}
		for _, h := range hashes {
			s.own(h, d.Name())
		}
		h, err := readHeader(dir)
		if err != nil {
			continue
		}
		if h.Base != "" {
			if err := s.markPrefix(h.Base); err != nil {
				s.faulty[d.Name()] = err
				continue
			}
		}
		delete(s.faulty, d.Name())
	}
	return nil
}

// markPrefix records the path to base as held.
func (s *Store) markPrefix(base string) error {
	for id := base; id != ""; {
		s.prefix[id] = true
		parent, err := s.parentOf(id)
		if err != nil {
			return err
		}
		id = parent
	}
	return nil
}

// --- per-session files ---

// readLog reads a session's log: each line an entry hash, followed by
// the entry's size in bytes when the line was written with one.
func readLog(dir string) ([]string, map[string]int64, error) {
	sizes := map[string]int64{}
	data, err := os.ReadFile(filepath.Join(dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, sizes, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cas: log: %w", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		hash, size, _ := strings.Cut(l, " ")
		if hash == "" {
			continue
		}
		out = append(out, hash)
		if n, err := strconv.ParseInt(size, 10, 64); err == nil {
			sizes[hash] = n
		}
	}
	return out, sizes, nil
}

func writeLogLine(buf *bytes.Buffer, hash string, size int64) {
	buf.WriteString(hash)
	if size > 0 {
		buf.WriteByte(' ')
		buf.WriteString(strconv.FormatInt(size, 10))
	}
	buf.WriteByte('\n')
}

func appendLog(dir string, hashes []string, sizes map[string]int64) error {
	var buf bytes.Buffer
	for _, h := range hashes {
		writeLogLine(&buf, h, sizes[h])
	}
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cas: log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(buf.Bytes()); err != nil {
		return fmt.Errorf("cas: log: %w", err)
	}
	return nil
}

func readHead(dir string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "HEAD"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// writeHead writes the HEAD index. It is not fsynced: the journal has
// every head move, and recovery writes a HEAD a crash left stale.
func writeHead(dir, head string) error {
	return writeIndex(filepath.Join(dir, "HEAD"), []byte(head+"\n"))
}

// readMark reads the mark; a session without one is a mirror, so that
// nothing this store did not finish marking can be advanced here.
func readMark(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "record"))
	if err != nil {
		return MarkMirror
	}
	return strings.TrimSpace(string(data))
}

func readHeader(dir string) (agentsession.Header, error) {
	var h agentsession.Header
	data, err := os.ReadFile(filepath.Join(dir, "header"))
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &h); err != nil {
		return h, fmt.Errorf("cas: header: %w", err)
	}
	return h, nil
}

// writeHeader writes the header durably: it is not in the journal.
func writeHeader(dir string, h agentsession.Header) error {
	hdr, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if hdr, err = jcs.Transform(hdr); err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, "header"), append(hdr, '\n'))
}

// raiseFormat writes the format this package writes into the session's
// header before the first append that package makes to a session whose
// header names an earlier minor, as RFC 0002 requires, so a reader of
// that earlier minor refuses the session rather than reading entries it
// cannot represent. Nothing hashed changes: the header is not an entry.
func (s *Store) raiseFormat(h *handle) error {
	_, minor, err := agentsession.ParseFormat(h.diskFormat)
	if err == nil && minor >= agentsession.FormatMinor {
		return nil
	}
	hdr, err := readHeader(h.dir)
	if err != nil {
		return err
	}
	hdr.Format = agentsession.Format
	if err := writeHeader(h.dir, hdr); err != nil {
		return fmt.Errorf("cas: raise the header's format: %w", err)
	}
	h.diskFormat = agentsession.Format
	return nil
}

// --- Store ---

// Create implements agentsession.Store. A header with a Base makes a
// fork: the base must be an entry the store holds, that is, one in a
// session's log or on a session's prefix, and not a leaf label; the
// media must match that of the session whose own entries include the
// base, when the store holds that session; and the head starts at the
// base. The session is the record here.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createLocked(ctx, h, MarkRecord)
}

func (s *Store) createLocked(ctx context.Context, h agentsession.Header, mark string) (*agentsession.Session, error) {
	var (
		sess   *agentsession.Session
		prefix [][]byte
	)
	if h.Base != "" {
		if !s.holds(h.Base) {
			// Another process may have committed it since the index was
			// built; look again before refusing.
			if err := s.index(); err != nil {
				return nil, err
			}
		}
		if !s.holds(h.Base) {
			return nil, fmt.Errorf("%w: base %s is not held by this store", agentsession.ErrNoEntry, h.Base)
		}
		if isLabel, err := s.isLeafLabel(h.Base); err != nil {
			return nil, err
		} else if isLabel {
			return nil, errors.New("cas: a base may not be a leaf label")
		}
		if owner, ok := s.anyOwner(h.Base); ok {
			if h.ParentSession == "" {
				h.ParentSession = owner
			}
			if dir, err := s.sessionDir(owner); err == nil {
				if oh, err := readHeader(dir); err == nil {
					// The fork's header takes this package's format, which
					// must be no earlier than the origin's.
					if laterFormat(oh.Format, agentsession.Format) {
						return nil, fmt.Errorf("%w: the origin is %s, later than this writer's %s", agentsession.ErrUnsupportedFormat, oh.Format, agentsession.Format)
					}
					if h.Media != "" && mediaOf(h) != mediaOf(oh) {
						return nil, errors.New("cas: a fork's media must equal its origin's")
					}
					h.Media = oh.Media
					if h.Payload == "" {
						h.Payload = oh.Payload
					}
				}
			}
		}
		var err error
		if prefix, err = s.pathLines(h.Base); err != nil {
			return nil, err
		}
	}
	tmp := agentsession.New(h)
	h = tmp.Header()
	dir, err := s.sessionDir(h.ID)
	if err != nil {
		return nil, err
	}
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	lk, err := s.lockSession(h.ID)
	if err != nil {
		return nil, err
	}
	// Nothing is removed until this call knows the directory is its own:
	// a failure before the existence check leaves whatever is there.
	fail := func(err error) (*agentsession.Session, error) {
		lk.release()
		return nil, err
	}
	if v, err := s.recoverSession(h.ID, dir); err != nil {
		return fail(err)
	} else if v.exists {
		return fail(fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID))
	}
	fail = func(err error) (*agentsession.Session, error) {
		lk.release()
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fail(fmt.Errorf("cas: %w", err))
	}
	if h.Base != "" {
		sess, err = s.assemble(h, prefix, nil, h.Base)
		if err != nil {
			return fail(err)
		}
	} else {
		sess = tmp
	}
	// No sweep's last step from here until the header names the prefix.
	// The prefix is freshened too, so it is young once the lock is
	// dropped, and the create record names the base, so a sweep that
	// read the headers before this one landed keeps it.
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return fail(err)
	}
	defer guard.release()
	if h.Base != "" {
		if err := s.freshenPath(h.Base); err != nil {
			return fail(err)
		}
	}
	// The create record is the boundary: whatever the journal said about
	// this ID before belongs to a session that is gone. The header is
	// written last, so a directory without one was never finished.
	if err := s.commit(true, journalRecord{Op: "create", Session: h.ID, Base: h.Base}, journalRecord{Op: "mark", Session: h.ID, Mark: mark}); err != nil {
		return fail(err)
	}
	if err := writeIndex(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
		return fail(err)
	}
	if h.Base != "" {
		if err := writeHead(dir, h.Base); err != nil {
			return fail(err)
		}
	}
	if err := writeHeader(dir, h); err != nil {
		return fail(fmt.Errorf("cas: header: %w", err))
	}
	if err := syncDir(filepath.Join(s.root, "sessions")); err != nil {
		return fail(err)
	}
	if h.Base != "" {
		if err := s.markPrefix(h.Base); err != nil {
			return fail(err)
		}
	}
	s.open[h.ID] = &handle{session: sess, dir: dir, lock: lk, mark: mark, head: h.Base, count: sess.Len(), diskFormat: h.Format}
	return sess, nil
}

// pathLines rebuilds the lines of the path to id, root first, from the
// envelopes and bodies the store holds.
func (s *Store) pathLines(id string) ([][]byte, error) {
	var rev [][]byte
	for id != "" {
		line, err := s.loadLine(id)
		if err != nil {
			return nil, err
		}
		rev = append(rev, line)
		parent, err := s.parentOf(id)
		if err != nil {
			return nil, err
		}
		id = parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev, nil
}

// assemble builds a session from a header, the prefix lines and the own
// lines through the format's reader, so every line is verified as a
// file's would be, and sets the head.
func (s *Store) assemble(h agentsession.Header, prefix, own [][]byte, head string) (*agentsession.Session, error) {
	var buf bytes.Buffer
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	buf.Write(hdr)
	buf.WriteByte('\n')
	for _, l := range prefix {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	for _, l := range own {
		buf.Write(l)
		buf.WriteByte('\n')
	}
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return nil, fmt.Errorf("cas: session %s: %w", h.ID, err)
	}
	if head != "" {
		if err := sess.Branch(head); err != nil {
			return nil, fmt.Errorf("cas: session %s: head: %w", h.ID, err)
		}
	} else if h.Base == "" && sess.Len() > 0 {
		sess.ResetLeaf()
	}
	return sess, nil
}

// Open implements agentsession.Store.
func (s *Store) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(id)
	if err != nil {
		return nil, err
	}
	return h.session, nil
}

func (s *Store) openLocked(id string) (*handle, error) {
	if h, ok := s.open[id]; ok {
		return h, nil
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if err, ok := s.faulty[id]; ok {
		return nil, fmt.Errorf("cas: session %s could not be indexed: %w", id, err)
	}
	lk, err := s.lockSession(id)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*handle, error) {
		lk.release()
		return nil, err
	}
	v, err := s.recoverSession(id, dir)
	if err != nil {
		return fail(err)
	}
	if !v.exists {
		return fail(fmt.Errorf("%w: %s", agentsession.ErrNoSession, id))
	}
	hdr, err := readHeader(dir)
	if err != nil {
		return fail(err)
	}
	diskFormat := hdr.Format
	var prefix [][]byte
	if hdr.Base != "" {
		if prefix, err = s.pathLines(hdr.Base); err != nil {
			return fail(err)
		}
	}
	own := make([][]byte, 0, len(v.log))
	for _, h := range v.log {
		line, err := s.loadLine(h)
		if err != nil {
			return fail(fmt.Errorf("cas: session %s: %w", id, err))
		}
		own = append(own, line)
	}
	sess, err := s.assemble(hdr, prefix, own, v.head)
	if err != nil {
		return fail(err)
	}
	for _, h := range v.log {
		s.own(h, id) // another process may have appended since the index was built
	}
	h := &handle{session: sess, dir: dir, lock: lk, mark: v.mark, head: v.head, count: sess.Len(), diskFormat: diskFormat}
	s.open[id] = h
	return h, nil
}

// Append implements agentsession.Store; see Write for what it reports.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	r, err := s.Write(ctx, sessionID, e)
	return r.ID, err
}

// durableFor says whether the policy makes this append durable.
func (s *Store) durableFor(hdr agentsession.Header, e agentsession.Entry) bool {
	switch s.policy {
	case SyncEveryAppend:
		return true
	case SyncOnResponse:
		switch v := e.(type) {
		case *agentsession.ResponseEntry, *agentsession.CompactionEntry, *agentsession.BranchSummaryEntry:
			return true
		case *agentsession.ItemEntry:
			_, ok := v.Item.(*openresponses.FunctionCallOutput)
			return ok
		}
		return hdr.HasRecord(e.EntryType())
	}
	return false
}

// Write appends an entry and reports what happened, as the format asks
// a store to: whether the entry continued the head, branched, was
// already held, or as a leaf label moved the head or could not, and
// whether the append was durable when it returned. The order is the
// store's one order: the entry is prepared, its objects are written,
// the journal record is committed, the log and the head follow, and
// only then is the entry visible in the session. A session held as a
// mirror is refused, as is a leaf label carrying `synthetic`. A head
// moved in memory through Session.Branch since the last commit is
// journaled first, so a head move never bypasses the journal. The first
// append to a session whose header names an earlier minor raises the
// header's format first.
func (s *Store) Write(ctx context.Context, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
	if err := ctx.Err(); err != nil {
		return agentsession.Result{}, err
	}
	if s.readOnly {
		return agentsession.Result{}, errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return agentsession.Result{}, err
	}
	if h.mark != MarkRecord {
		return agentsession.Result{}, fmt.Errorf("%w: %s", ErrMirror, sessionID)
	}
	if l, ok := e.(*agentsession.LabelEntry); ok && isSynthetic(l) {
		return agentsession.Result{}, ErrSynthetic
	}
	if err := s.untouched(h); err != nil {
		return agentsession.Result{}, err
	}
	if err := s.syncHead(h, sessionID); err != nil {
		return agentsession.Result{}, err
	}
	leafAtPrepare := h.session.Leaf()
	r, err := h.session.Prepare(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	if r.Outcome == agentsession.Held {
		return r, nil
	}
	if err := s.raiseFormat(h); err != nil {
		return agentsession.Result{}, err
	}
	durable := s.durableFor(h.session.Header(), e)
	// From the object write through the commit no sweep's last step may
	// run: the object may be one the sweep would otherwise find old and
	// unnamed.
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return agentsession.Result{}, err
	}
	size, err := s.storeEntry(e, durable)
	if err != nil {
		guard.release()
		return agentsession.Result{}, err
	}
	// A sidecar blob the entry names is freshened while the lock is held,
	// as a reused content is, so a sweep that gathered it as old and
	// unreferenced sees it young at its second look, and one whose pack a
	// sweep removed from under this store is written back.
	// One the store does not hold is reported, as the format allows,
	// rather than refused; a projection of the session will fail until
	// it arrives.
	if _, body, err := split(e); err == nil {
		for _, b := range blobsNamedBy(body) {
			if err := s.objs.freshen(spaceContents, b); err != nil {
				r.Unresolved = append(r.Unresolved, b)
			}
		}
	}
	head := ""
	switch r.Outcome {
	case agentsession.Continued:
		head = r.ID
	case agentsession.LeafMoved:
		head = e.(*agentsession.LabelEntry).Target
	}
	err = s.commit(durable, journalRecord{Op: "append", Session: sessionID, Entry: r.ID, Head: head, Seq: h.session.Len() + 1, Size: size})
	guard.release()
	if err != nil {
		return agentsession.Result{}, err
	}
	r.Durable = durable
	// The append is committed from here. The log and the head are
	// indexes the next open rebuilds from the journal, so a failure to
	// write them is not a failed append and is not reported as one.
	_ = appendLog(h.dir, []string{r.ID}, map[string]int64{r.ID: size})
	if head != "" {
		_ = writeHead(h.dir, head)
		h.head = head
	}
	s.own(r.ID, sessionID)
	if h.session.Leaf() != leafAtPrepare {
		// A reader moved the leaf between Prepare and Commit, through
		// Session.Branch, which does not take the store's lock. The
		// journal has spoken; the store is authoritative, and the move
		// is undone rather than recorded as something it was not.
		if leafAtPrepare == "" {
			h.session.ResetLeaf()
		} else if err := h.session.Branch(leafAtPrepare); err != nil {
			return s.committedButNotApplied(sessionID, r)
		}
	}
	if _, err := h.session.Commit(e); err != nil {
		return s.committedButNotApplied(sessionID, r)
	}
	h.count++
	return r, nil
}

// committedButNotApplied is what Write returns when the journal record is
// committed and the in-memory session could not be brought in step: the
// append happened, so it is reported as a success, and the handle is
// dropped so the next Open rebuilds the session from what the store
// holds. Nothing after the commit point may turn a committed append into
// a reported failure.
func (s *Store) committedButNotApplied(sessionID string, r agentsession.Result) (agentsession.Result, error) {
	if h, ok := s.open[sessionID]; ok {
		delete(s.open, sessionID)
		h.lock.release()
	}
	r.Reopen = true
	return r, nil
}

// isSynthetic reports whether a label carries synthetic: true, the
// projection's own marker.
func isSynthetic(l *agentsession.LabelEntry) bool {
	raw, ok := l.Unknown["synthetic"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

// untouched checks that the session holds exactly the entries the store
// committed into it: an entry appended on the session directly is not
// in any log, and nothing may build on it.
func (s *Store) untouched(h *handle) error {
	if h.session.Len() != h.count {
		return fmt.Errorf("%w: %d entries, %d committed", ErrModified, h.session.Len(), h.count)
	}
	return nil
}

// syncHead journals a head the session moved in memory, through
// Session.Branch, since the last commit, so the move is recorded before
// anything builds on it.
func (s *Store) syncHead(h *handle, sessionID string) error {
	leaf := h.session.Leaf()
	if leaf == h.head {
		return nil
	}
	if leaf != "" {
		if err := s.mayRestOn(h.session, leaf); err != nil {
			return err
		}
		// The head must be something the store committed: the base or
		// an entry in this session's log, never one appended on the
		// session behind the store's back.
		if !s.owners[leaf][sessionID] && leaf != h.session.Header().Base {
			return fmt.Errorf("%w: head %s is not in the log", ErrModified, leaf)
		}
	} else if h.session.Header().Base != "" {
		return fmt.Errorf("%w: a session with a base has a head", agentsession.ErrNoEntry)
	}
	if err := s.commit(true, journalRecord{Op: "head", Session: sessionID, Head: leaf, Seq: h.session.Len()}); err != nil {
		return err
	}
	_ = writeHead(h.dir, leaf) // an index; the journal has it
	h.head = leaf
	return nil
}

// SetHead moves a session's head from expected to the entry to, or
// returns ErrHeadMoved when the head is not expected. "" is a valid
// expected value for a session with no head. The target must be the
// base or an own entry and not a leaf label; a mirror is refused. The
// journal record is committed, durably, before the head moves anywhere.
func (s *Store) SetHead(ctx context.Context, sessionID, expected, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return err
	}
	if h.mark != MarkRecord {
		return fmt.Errorf("%w: %s", ErrMirror, sessionID)
	}
	if err := s.untouched(h); err != nil {
		return err
	}
	if h.head != expected {
		return fmt.Errorf("%w: head is %s", ErrHeadMoved, orNone(h.head))
	}
	if err := s.mayRestOn(h.session, to); err != nil {
		return err
	}
	return s.moveHead(h, sessionID, to)
}

// moveHead commits a head move and applies it.
func (s *Store) moveHead(h *handle, sessionID, to string) error {
	if err := s.commit(true, journalRecord{Op: "head", Session: sessionID, Head: to, Seq: h.session.Len()}); err != nil {
		return err
	}
	_ = writeHead(h.dir, to) // an index; the journal has it
	h.head = to
	if err := h.session.Branch(to); err != nil {
		// Committed; the session is rebuilt on the next Open.
		delete(s.open, sessionID)
		h.lock.release()
	}
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// mayRestOn holds a head target to the rule: the base or an own entry,
// and not a leaf label.
func (s *Store) mayRestOn(sess *agentsession.Session, id string) error {
	e, ok := sess.Entry(id)
	if !ok {
		return fmt.Errorf("%w: %s", agentsession.ErrNoEntry, id)
	}
	if b := sess.Header().Base; b != "" && id != b && sess.Prefix(id) {
		return fmt.Errorf("%w: %s is on the prefix above the base", agentsession.ErrNoEntry, id)
	}
	if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
		return errors.New("cas: the head never rests on a leaf label")
	}
	return nil
}

// Mark returns whether this store is the record for the session or a
// mirror of it.
func (s *Store) Mark(ctx context.Context, sessionID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return "", err
	}
	return h.mark, nil
}

// setMark commits a mark and applies it.
func (s *Store) setMark(h *handle, sessionID, mark string) error {
	if err := s.commit(true, journalRecord{Op: "mark", Session: sessionID, Mark: mark}); err != nil {
		return err
	}
	_ = writeIndex(filepath.Join(h.dir, "record"), []byte(mark+"\n")) // an index; the journal has it
	h.mark = mark
	return nil
}

// DeclareRecord makes this store the record for a session it holds as
// a mirror: what a mirror does when the record deleted the session
// without handing it over, or an importer does for a file it knows to
// be the only copy.
func (s *Store) DeclareRecord(ctx context.Context, sessionID string) error {
	if s.readOnly {
		return errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return err
	}
	if err := s.setMark(h, sessionID, MarkRecord); err != nil {
		return err
	}
	// A head moved in memory while the session was a mirror bypassed
	// the compare-and-swap; the record's head is the HEAD file's.
	if h.head != "" {
		return h.session.Branch(h.head)
	}
	if h.session.Header().Base == "" {
		h.session.ResetLeaf()
	}
	return nil
}

// List implements agentsession.Store. A directory a crash left without
// a header, and a session the journal says was deleted, are not
// sessions and are not listed. Summary.Size is the bytes of the
// session's own entries, envelopes and contents, as stored; a content
// several entries share counts once for each.
func (s *Store) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(agentsession.Summary{}, err)
			return
		}
		dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
		if err != nil {
			yield(agentsession.Summary{}, fmt.Errorf("cas: %w", err))
			return
		}
		scan, err := s.replay()
		if err != nil {
			yield(agentsession.Summary{}, err)
			return
		}
		var out []agentsession.Summary
		var errs []error
		for _, d := range dirs {
			if !d.IsDir() || !validSessionID(d.Name()) {
				continue
			}
			dir := filepath.Join(s.root, "sessions", d.Name())
			v, err := s.reconcile(d.Name(), dir, scan)
			if err != nil {
				errs = append(errs, fmt.Errorf("cas: %s: %w", d.Name(), err))
				continue
			}
			if !v.exists {
				continue
			}
			sum, err := s.summarize(d.Name(), dir, v, f.WithNames || f.Current)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if f.Keep(sum) {
				out = append(out, sum)
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Header.CreatedAt.After(out[j].Header.CreatedAt) })
		if f.Limit > 0 && len(out) > f.Limit {
			out = out[:f.Limit]
		}
		for _, sum := range out {
			if !yield(sum, nil) {
				return
			}
		}
		for _, err := range errs {
			if !yield(agentsession.Summary{}, err) {
				return
			}
		}
	}
}

func (s *Store) summarize(id, dir string, v view, withMeta bool) (agentsession.Summary, error) {
	h, err := readHeader(dir)
	if err != nil {
		return agentsession.Summary{}, fmt.Errorf("cas: %s: %w", id, err)
	}
	sum := agentsession.Summary{Header: h, Path: dir}
	if info, err := os.Stat(filepath.Join(dir, "log")); err == nil {
		sum.Modified = info.ModTime()
	}
	for _, hash := range v.log {
		if n, ok := v.sizes[hash]; ok {
			sum.Size += n
			continue
		}
		// A line written before the log carried sizes.
		if n, ok := s.objs.size(spaceEntries, hash); ok {
			sum.Size += n
		}
		if c, err := s.contentOf(hash); err == nil {
			if n, ok := s.objs.size(spaceContents, c); ok {
				sum.Size += n
			}
		}
	}
	if withMeta {
		for _, hash := range v.log {
			line, err := s.loadLine(hash)
			if err != nil {
				return sum, fmt.Errorf("cas: %s: %w", id, err)
			}
			e, err := agentsession.UnmarshalEntry(line)
			if err != nil {
				continue
			}
			switch v := e.(type) {
			case *agentsession.InfoEntry:
				if v.Name != "" {
					sum.Name = v.Name
				}
			case *agentsession.LinkEntry:
				if v.Rel == agentsession.RelContinuedIn && v.Session != "" {
					sum.SupersededBy = v.Session
				}
			}
		}
	}
	return sum, nil
}

// Delete implements agentsession.Store: it commits a delete record, so
// a later session under the same ID starts from nothing, then removes
// the session's ref, header and log. Objects stay; what no log and no
// prefix needs is swept by Sweep. Once the record is committed the
// delete has happened: a failure to remove the directory is left to
// the next recovery of that ID and is not reported.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.sessionDir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	var lk *dirLock
	if h, ok := s.open[id]; ok {
		delete(s.open, id)
		lk = h.lock
	} else if lk, err = s.lockSession(id); err != nil {
		return err
	}
	defer lk.release()
	if err := s.commit(true, journalRecord{Op: "delete", Session: id}); err != nil {
		return err
	}
	for h, set := range s.owners {
		delete(set, id)
		if len(set) == 0 {
			delete(s.owners, h)
		}
	}
	if err := os.RemoveAll(dir); err == nil {
		_ = syncDir(filepath.Join(s.root, "sessions"))
	}
	return nil
}

// Release closes a session this process holds, freeing its lock. Lazy
// appends are made durable first: the next holder's durable commit
// syncs the journal, and must not make this store's lazy records
// durable ahead of their objects.
func (s *Store) Release(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.open[id]
	if !ok {
		return nil
	}
	var err error
	if !s.readOnly {
		err = s.syncJournal()
	}
	delete(s.open, id)
	if rerr := h.lock.release(); err == nil {
		err = rerr
	}
	return err
}

// Sync makes every append this store acknowledged lazily durable. The
// journal is store-wide, so it covers every session.
func (s *Store) Sync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return errReadOnly()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncJournal()
}

// Close makes lazy appends durable and releases every session this
// store holds.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	if !s.readOnly {
		first = s.syncJournal()
	}
	for id, h := range s.open {
		if err := h.lock.release(); err != nil && first == nil {
			first = err
		}
		delete(s.open, id)
	}
	s.objs.close()
	return first
}

// Project writes the session as an RFC 0001 file: the header, the
// prefix, the own entries in log order, and a synthetic leaf marker
// when the file's resume rule would not land a reader on the head.
func (s *Store) Project(ctx context.Context, w io.Writer, sessionID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return err
	}
	if err := s.untouched(h); err != nil {
		return err
	}
	return project(w, h.session, h.head)
}

// ProjectDir writes the session's file into dir as <id>.jsonl and, for a
// sidecar session, every blob its entries name as <id>/<hex>, so the
// projection is self-contained as RFC 0001 requires. It returns the
// file's path.
func (s *Store) ProjectDir(ctx context.Context, dir, sessionID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return "", err
	}
	if err := s.untouched(h); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := project(&buf, h.session, h.head); err != nil {
		return "", err
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	if err := writeAtomic(path, buf.Bytes()); err != nil {
		return "", err
	}
	if mediaOf(h.session.Header()) != agentsession.MediaSidecar {
		return path, nil
	}
	blobDir := filepath.Join(dir, sessionID)
	for _, e := range h.session.Entries() {
		_, body, err := split(e)
		if err != nil {
			return "", err
		}
		for _, b := range blobsNamedBy(body) {
			data, err := s.Blob(ctx, b)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(blobDir, 0o755); err != nil {
				return "", err
			}
			if err := writeAtomic(filepath.Join(blobDir, strings.TrimPrefix(b, agentsession.HashPrefix)), data); err != nil {
				return "", err
			}
		}
	}
	return path, nil
}

// project is Project over a session in memory with the given head.
func project(w io.Writer, sess *agentsession.Session, head string) error {
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, sess); err != nil {
		return err
	}
	check, err := agentsession.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	if head != "" && check.Leaf() != head {
		last := sess.Entries()[sess.Len()-1]
		headEntry, _ := sess.Entry(head)
		marker := agentsession.NewLabelEntry(head, agentsession.LeafLabel)
		marker.Parent = last.Base().ID
		marker.Timestamp = headEntry.Base().Timestamp
		marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
		if _, err := check.Append(marker); err != nil {
			return err
		}
		buf.Reset()
		if err := agentsession.Write(&buf, check); err != nil {
			return err
		}
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Import reads an RFC 0001 file into the store as a mirror of the
// session it holds, unless asRecord says this file is the only copy. It
// is held to a push's checks: every line verifies, a truncated last
// line is refused, a redacted header is refused, every own entry hangs
// from the base or another own entry, and the media matches that of a
// held session whose own entries include the base. A trailing synthetic
// marker names the head and is discarded; prefix entries become objects
// and join no log; own entries join the log in file order. The objects
// arrive as one pack and the records as one commit. A session the store
// already holds is refused; one store takes a session from another by
// exchange, [Store.Fetch] and [Store.Push].
func (s *Store) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, errReadOnly()
	}
	sess, err := agentsession.Read(r)
	if err != nil {
		return nil, err
	}
	if t := sess.Truncated(); t != nil {
		return nil, fmt.Errorf("cas: import: line %d is cut short: %w", t.Line, t.Err)
	}
	h := sess.Header()
	if h.Redacted {
		return nil, errors.New("cas: a redacted projection is a record to read, not one to hold")
	}
	if migrated, unresolved := sess.Migrated(); migrated && len(unresolved) > 0 {
		return nil, agentsession.ErrUnresolvedMigration
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.sessionDir(h.ID)
	if err != nil {
		return nil, err
	}
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	entries := sess.Entries()
	var own, stored []agentsession.Entry
	for i, e := range entries {
		if l, ok := e.(*agentsession.LabelEntry); ok && isSynthetic(l) {
			if i == len(entries)-1 && l.Label != nil && *l.Label == agentsession.LeafLabel {
				continue // the projection's marker, discarded
			}
			return nil, fmt.Errorf("%w: at line %d", ErrSynthetic, i+2)
		}
		stored = append(stored, e)
		if sess.Prefix(e.Base().ID) {
			continue
		}
		// An own entry hangs from the base or another own entry, or from
		// null in a baseless session.
		p := e.Base().Parent
		if h.Base != "" && p != h.Base && (p == "" || sess.Prefix(p)) {
			return nil, fmt.Errorf("cas: import: own entry %s hangs from %s, above the base", e.Base().ID, orNone(p))
		}
		own = append(own, e)
	}
	if h.Base != "" {
		if owner, ok := s.anyOwner(h.Base); ok {
			if odir, err := s.sessionDir(owner); err == nil {
				if oh, err := readHeader(odir); err == nil && mediaOf(oh) != mediaOf(h) {
					return nil, errors.New("cas: import: media differs from the session holding the base")
				}
			}
		}
	}
	mark := MarkMirror
	if asRecord {
		mark = MarkRecord
	}
	hd, err := s.admitNew(ctx, dir, h, mark, stored, own, nil, sess.Leaf())
	if err != nil {
		return nil, err
	}
	return hd.session, nil
}

// admitNew takes in a session the store lacks: its objects as one pack,
// then one commit of its create record, its mark, an append record per
// own entry in order and its head, then the indexes and the header
// last. A failure after the create record commits a delete, so the ID
// is free again and nothing half-admitted counts. It returns the
// session as the store holds it, rebuilt from the objects and the log.
func (s *Store) admitNew(ctx context.Context, dir string, h agentsession.Header, mark string, stored, own []agentsession.Entry, blobs map[string][]byte, head string) (*handle, error) {
	lk, err := s.lockSession(h.ID)
	if err != nil {
		return nil, err
	}
	if v, err := s.recoverSession(h.ID, dir); err != nil {
		lk.release()
		return nil, err
	} else if v.exists {
		lk.release()
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lk.release()
		return nil, fmt.Errorf("cas: %w", err)
	}
	guard, err := s.writeGuard(ctx)
	if err != nil {
		lk.release()
		return nil, err
	}
	defer guard.release()
	created := false
	fail := func(err error) (*handle, error) {
		if created {
			s.commit(true, journalRecord{Op: "delete", Session: h.ID})
		}
		lk.release()
		os.RemoveAll(dir)
		return nil, err
	}
	sizes, err := s.packEntries(stored, blobs)
	if err != nil {
		return fail(err)
	}
	recs := []journalRecord{{Op: "create", Session: h.ID, Base: h.Base}, {Op: "mark", Session: h.ID, Mark: mark}}
	hashes := make([]string, 0, len(own))
	for i, e := range own {
		id := e.Base().ID
		hashes = append(hashes, id)
		recs = append(recs, journalRecord{Op: "append", Session: h.ID, Entry: id, Seq: i + 1, Size: sizes[id]})
	}
	if head != "" {
		recs = append(recs, journalRecord{Op: "head", Session: h.ID, Head: head, Seq: len(own)})
	}
	if err := s.commit(true, recs...); err != nil {
		return fail(err)
	}
	created = true
	if err := writeIndex(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
		return fail(err)
	}
	for _, id := range hashes {
		s.own(id, h.ID)
	}
	if len(hashes) > 0 {
		if err := appendLog(dir, hashes, sizes); err != nil {
			return fail(err)
		}
	}
	if head != "" {
		if err := writeHead(dir, head); err != nil {
			return fail(err)
		}
	}
	if h.Base != "" {
		if err := s.markPrefix(h.Base); err != nil {
			return fail(err)
		}
	}
	// The header last: a directory without one was never finished.
	if err := writeHeader(dir, h); err != nil {
		return fail(err)
	}
	if err := syncDir(filepath.Join(s.root, "sessions")); err != nil {
		return fail(err)
	}
	lk.release()
	return s.openLocked(h.ID)
}

var _ agentsession.Store = (*Store)(nil)
