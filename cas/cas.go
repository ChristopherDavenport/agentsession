// Package cas is the content-addressed session store RFC 0002 describes,
// laid out on a filesystem the way git lays out a repository:
//
//	<root>/
//	  objects/contents/<2 hex>/<62 hex>   a loose body, canonical bytes, held once
//	  objects/entries/<2 hex>/<62 hex>    a loose envelope: type, parent, parents, ts, content
//	  objects/pack/pack-<hex>.{pack,idx}  objects packed together, with their index
//	  sessions/<session id>/
//	    header                            the header line
//	    log                               the session's write-ahead log and its record
//	    HEAD                              an index of the head the log names
//	    record                            an index of the mark the log names
//	    summary                           its size and name, kept for a listing
//	  locks/<session id>                  the lock of the process holding the session
//	  tmp/                                sessions being created, renamed into place whole
//	  trash/                              sessions a delete renamed away, being removed
//	  sweep.lock                          held shared by writers, exclusive by a sweep's last step
//	  gc.lock                             the lock of a running sweep or pack
//
// An entry is stored as two objects, its body under the content hash
// and its envelope under the id, so a body shared by many entries is
// held once and a chain of envelopes verifies without its bodies. A
// session is a ref: a header, a base, a head, and a log. Every object is
// checked against its name when it is read, and one that fails is
// reported as corrupt by name and place.
//
// A session's log is its write-ahead log and its record, as RFC 0002
// says: nothing wider than the session is written to accept an append
// or read to recover one, so nothing a process pays follows the size of
// the store. Every append follows one order. Its objects go first,
// idempotently, since their bytes are their names. Then one record goes
// to the session's log, carrying a checksum: the append is accepted,
// and nothing of it is visible before. A commit fsyncs the objects and
// then the log. HEAD and the mark are indexes of the log, rebuilt from
// it when a crash leaves them stale.
//
// Under the default sync policy every append is committed before it
// returns. [WithSync] lets a writer leave appends as the session's
// working state, accepted and not yet committed, as RFC 0002 allows, and
// [agentsession.Result] says which each append got; the working state
// is committed with the next durable append, by [Store.Sync], or when the
// session is released. A commit covers every object its appends name,
// including one another session wrote lazily. After a crash, a lazy
// append whose objects did not survive, or were left empty, is lost,
// with every record the session accepted after it; recovery logs the
// loss, and commits the working state that survived, before anything
// more is accepted.
//
// A session is created by writing its header, log, head and mark under
// tmp and renaming the directory into sessions, and deleted by renaming
// it into trash, each in one step. A fork is created only once the log
// holding its base is committed through it, since the fork's recovery
// reads its own log alone.
//
// Objects are written loose, one file each, and packed later, as git
// does: [Store.Pack] moves loose objects into a pack and merges the
// smallest packs, and [Store.Sweep] repacks everything the store holds
// into one pack and removes what nothing needs. A store of small
// entries otherwise pays a filesystem block for every envelope and
// every body. A writing store packs on its own once its loose objects
// look to pass a few thousand, as git's gc --auto does. Pack indexes are
// mapped, not read, and a listing reads the summary kept beside each
// session's log.
//
// Several processes may share a store on one machine. Each session is
// held by one process at a time through a lock the kernel drops when
// the process exits (flock on unix), which RFC 0002 permits: a store
// may take writers in turn provided it never re-parents an accepted
// append. Only the holder writes a session's log, so a record a crash
// cut short is at its end, and the next holder cuts it before writing.
// [WithReadOnly] takes no session lock, so a session another process
// holds can be read, projected and verified while it writes. The sweep
// keeps everything a log names and every loose object younger than a
// grace period, as git's collector spares a young loose object, so an
// object written ahead of its record is safe.
//
// A failed fsync stops the store. Linux reports a failed writeback once
// to each file opened before it, marks the pages it failed to write
// clean, and keeps them in memory, so an fsync of the same file opened
// later succeeds and a read returns bytes the disk does not hold.
// After any fsync of its data fails, a store runs no more fsyncs and
// writes nothing, returning [ErrStopped], until it is opened again; it
// still reads. Three rules keep what such pages hold from being
// committed later, by a reopened store or another process. One fsync of
// a file runs at a time in a process, and a failure stops the store
// before the next. A commit fsyncs only files its own process wrote: an
// object this process did not write is written again as a file of its
// own rather than reused. And recovery that changes a
// session's log, or keeps its working state, writes the log, and the
// objects of that state, as new files rather than fsync what it found.
//
// What these leave open lies between processes. A file this process
// wrote lazily, whose writeback failed and whose failure another
// process's fsync took, is fsynced here successfully. A pack another
// process wrote is trusted as written durably. A fork's commit of a
// log another process holds is an fsync of that process's file. And a
// directory every process writes into, an object's fan-out directory
// or sessions, is fsynced by each: one process's failed fsync of it is
// reported to that process, and another's later fsync of it can
// succeed though a rename the first depended on never reached the
// disk; on ext4 and XFS a directory's changes go through the journal,
// whose failure usually takes the whole filesystem read-only. Each
// needs a failed fsync in one process and a commit by another before
// the machine restarts; a host that wants none of them restarts the
// machine, or drops the page cache, before a store stopped by a failed
// fsync is opened again.
//
// A store from before logs were per session kept a store-wide journal.
// Its first writing open migrates it, holding every session's lock so a
// writer of the earlier version still running is found rather than
// raced; a read-only open reads the sessions already migrated and
// reports [ErrLegacyStore] for the rest.
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
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

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

// writable returns why the store writes nothing, if it does not: it was
// opened read-only, or an fsync of its data failed.
func (s *Store) writable() error {
	if s.readOnly {
		return errReadOnly()
	}
	return s.objs.stopped()
}

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
	// SyncNever leaves every append lazy, durable at the next call to
	// [Store.Sync] or [Store.Close], apart from an entry whose type the
	// header names in records, which RFC 0001 requires to be durable
	// before the side effect it precedes and which this policy too makes
	// durable, with every lazy append before it.
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

// Store is a content-addressed store rooted at a directory. It is safe
// for concurrent use: calls on one session take turns, and calls on
// different sessions run at once, commits and opens included.
type Store struct {
	root     string
	readOnly bool
	policy   SyncPolicy
	objs     *objects

	// mu guards open, faulty and appends, and is held only to read or
	// change them: never while waiting on a session, and never across a
	// write to disk. A session's own work runs under its handle's lock,
	// so sessions of one store go at once, as sessions of different
	// processes do; the order is a handle's lock, then idx, then mu.
	mu   sync.Mutex
	open map[string]*handle
	// idx guards owners, prefix, indexed, building and deltas, and is
	// held only to read or change them. An index is built by reading
	// every session with no lock held, one build at a time under
	// indexing; what changes meanwhile is recorded in deltas and applied
	// after.
	idx      sync.Mutex
	indexing sync.Mutex
	building bool
	deltas   []func(owners map[string]map[string]bool, prefix map[string]bool)
	// owners maps each committed own entry to the sessions whose logs
	// hold it — one entry can be in two, when two sessions append the
	// same entry under the same parent — and prefix holds the entries
	// some session's base path needs. Together they are what the store
	// holds: an object written ahead of its log record is in neither.
	// They cost a read of every session, so they are built only when a
	// lookup needs them, and kept from then on; indexed says they are.
	owners  map[string]map[string]bool
	prefix  map[string]bool
	indexed bool
	// faulty records sessions the index could not read, with why, so
	// one fault is reported at that session's Open and hides no other.
	faulty map[string]error
	// appends counts this store's appends, which decides when it looks
	// at whether to pack.
	appends int
	// packing is set while a pack runs in the background, which Close
	// waits for through background; closing, under mu, says no other
	// may start.
	packing    atomic.Bool
	background sync.WaitGroup
	closing    bool
}

// handle is a session this store holds. Its mu is held by whoever works
// on the session, opening it included; gone says the handle was let go
// while a caller waited on mu, which then looks the session up again.
type handle struct {
	mu   sync.Mutex
	gone bool

	session    *agentsession.Session
	dir        string
	lock       *dirLock
	mark       string
	head       string // the head as the HEAD file has it
	count      int    // entries the store has committed into the session
	diskFormat string // the format the header file names
	// own holds the entries the store accepted into the session's log,
	// so the head is checked against them and not against an entry
	// appended to the session behind the store's back.
	own map[string]bool
	// logf is the session's log, open for appending, in a writing store.
	logf *os.File
	// pend is what the session's next commit flushes: the objects its
	// appends wrote or found lazily, and no other session's.
	pend *pendSet
	// lazy is set while the session holds a lazy append of this store's
	// that no sync record covers. The store holds the session's lock,
	// so every lazy record in it is this store's or was adopted at open,
	// and the commit that makes them durable says so with a sync record;
	// otherwise every later open checks each one's objects again.
	lazy bool
}

// Open opens or creates the store at root. It reads nothing store-wide:
// each session recovers from its own log when it is opened. A store
// from before per-session logs is migrated here, and a read-only store
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
	// A store from before per-session logs is migrated by its first
	// writing open; a read-only open reads the sessions already migrated,
	// and reports ErrLegacyStore for the rest.
	if _, err := os.Stat(filepath.Join(root, journalFile)); err == nil && !s.readOnly {
		if err := s.migrate(); err != nil {
			return nil, err
		}
	}
	// Nothing else store-wide is read here: each session recovers when
	// it is opened, and the index of which session holds what is built
	// only if a lookup needs it, so opening a store costs the same at
	// any size.
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
	var sync func(*os.File) error
	if durable {
		sync = func(f *os.File) error { return f.Sync() }
	}
	_, err := writeFileWith(path, data, sync, true)
	return err
}

// writeFileWith is writeFile, fsyncing with sync when it is set, and
// returning the file it wrote, as a rename over it since cannot change.
// Unless replace is set, the file takes its place only if none is there,
// by a hard link: an error wrapping os.ErrExist says one was, and one
// wrapping errNoLink that the filesystem made no link.
func writeFileWith(path string, data []byte, sync func(*os.File) error, replace bool) (os.FileInfo, error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return nil, err
	}
	_, werr := tmp.Write(data)
	if sync != nil && werr == nil {
		werr = sync(tmp)
	}
	// place puts the file at path: by a rename, or, when it must not
	// replace one, by a link, which fails if a file is there, and the
	// temporary name removed after.
	place := func() error {
		if replace {
			return os.Rename(tmp.Name(), path)
		}
		if err := linkFile(tmp.Name(), path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return err
			}
			return fmt.Errorf("%w: %v", errNoLink, err)
		}
		return os.Remove(tmp.Name())
	}
	if werr == nil && renameOpen {
		// Placed while open, so the file's version, which a rename or a
		// link may change, is read after it from the descriptor holding
		// the file.
		if werr = place(); werr == nil {
			info, serr := tmp.Stat()
			if cerr := tmp.Close(); serr == nil {
				serr = cerr
			}
			return info, serr
		}
	}
	info, serr := tmp.Stat()
	if werr == nil {
		werr = serr
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = place()
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return nil, werr
	}
	return info, nil
}

// errNoLink is a hard link a filesystem would not make: vfat and exFAT
// make none, nor do some FUSE, network and sandboxed mounts.
var errNoLink = errors.New("cas: the filesystem made no hard link")

// linkFile makes a hard link; a variable so a test can fail it.
var linkFile = os.Link

// writeAtomic replaces the file at path with data durably: through a
// temporary file, an fsync, a rename and an fsync of the directory.
func writeAtomic(path string, data []byte) error {
	if err := writeFile(path, data, true); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// writeIndex replaces an index file, which the log rebuilds after a
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
func (s *Store) storeEntry(e agentsession.Entry, durable bool, pend *pendSet) (int64, []byte, error) {
	env, body, err := split(e)
	if err != nil {
		return 0, nil, err
	}
	if err := s.objs.writeTo(spaceContents, e.Base().ContentHash(), body, durable, pend); err != nil {
		return 0, nil, fmt.Errorf("cas: store content: %w", err)
	}
	if err := s.objs.writeTo(spaceEntries, e.Base().ID, env, durable, pend); err != nil {
		return 0, nil, fmt.Errorf("cas: store entry: %w", err)
	}
	return int64(len(env) + len(body)), body, nil
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
// What it leaves to sync goes in pend, which the caller flushes.
func (s *Store) packEntries(entries []agentsession.Entry, blobs map[string][]byte, pend *pendSet) (map[string]int64, error) {
	sizes := map[string]int64{}
	var all []packObject
	for _, e := range entries {
		env, body, err := split(e)
		if err != nil {
			return nil, err
		}
		id, ch := e.Base().ID, e.Base().ContentHash()
		sizes[id] = int64(len(env) + len(body))
		all = append(all, packObject{sp: spaceContents, hash: ch, data: body}, packObject{sp: spaceEntries, hash: id, data: env})
	}
	for h, data := range blobs {
		all = append(all, packObject{sp: spaceContents, hash: h, data: data})
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
		if _, err := writePack(s.objs, s.objs.packDir(), missing); err != nil {
			return nil, fmt.Errorf("cas: pack: %w", err)
		}
		if err := s.objs.reloadPacks(true); err != nil {
			return nil, err
		}
	} else {
		held = all
	}
	for _, o := range held {
		if err := s.objs.writeTo(o.sp, o.hash, o.data, true, pend); err != nil {
			return nil, fmt.Errorf("cas: store: %w", err)
		}
	}
	return sizes, nil
}

// freshenPath freshens the objects on the path to id, as a write of
// each would, so a fork's prefix is young to the sweep until the fork's
// header lands, and an object a sweep removed from under this store's
// view is written back loose.
func (s *Store) freshenPath(id string, pend *pendSet) error {
	for id != "" {
		env, err := s.objs.read(spaceEntries, id)
		if err != nil {
			return err
		}
		if err := s.objs.writeTo(spaceEntries, id, env, true, pend); err != nil {
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
		if err := s.objs.writeTo(spaceContents, c, body, true, pend); err != nil {
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
// log or on a session's prefix. An object written ahead of its log
// record is in neither. It builds the index if nothing has.
func (s *Store) holds(id string) bool {
	if err := s.ensureIndex(); err != nil {
		return false
	}
	s.idx.Lock()
	defer s.idx.Unlock()
	return len(s.owners[id]) > 0 || s.prefix[id]
}

// ensureIndex builds the index once.
func (s *Store) ensureIndex() error {
	s.idx.Lock()
	indexed := s.indexed
	s.idx.Unlock()
	if indexed {
		return nil
	}
	return s.index(false)
}

// holderOf finds a session that holds the entry, in its log or on its
// prefix, looking first at the session a caller names: the session a
// fork names as its parent almost always holds its base, and reading
// that one session is what the lookup then costs. Only an entry that
// session does not hold is looked up in the index. owner is the session
// whose log holds the entry, when the lookup finds one; the index may
// find the entry held on a prefix alone.
func (s *Store) holderOf(entry, parent string) (owner string, held bool, err error) {
	for seen := 0; parent != "" && seen < 64; seen++ {
		dir, derr := s.sessionDir(parent)
		if derr != nil {
			break
		}
		l, lerr := readSessionLog(dir, 0)
		if lerr != nil || l.legacy {
			break
		}
		if slices.Contains(heldEntries(l.recs), entry) {
			return parent, true, nil
		}
		hdr, herr := readHeader(dir)
		if herr != nil || hdr.Base == "" {
			break
		}
		onPrefix, perr := s.onPath(entry, hdr.Base)
		if perr != nil || !onPrefix {
			break
		}
		// On the parent's prefix: the parent's own origin holds it in
		// its log, or on its prefix in turn.
		if hdr.ParentSession == "" {
			return "", true, nil
		}
		parent = hdr.ParentSession
	}
	if err := s.ensureIndex(); err != nil {
		return "", false, err
	}
	lookup := func() (string, bool) {
		s.idx.Lock()
		defer s.idx.Unlock()
		owner, ok := s.anyOwner(entry)
		return owner, ok || s.prefix[entry]
	}
	if owner, ok := lookup(); ok {
		return owner, true, nil
	}
	// Another process may have committed it since the index was built;
	// look again before saying no.
	if err := s.index(true); err != nil {
		return "", false, err
	}
	owner, ok := lookup()
	return owner, ok, nil
}

// onPath reports whether entry is on the path to tip.
func (s *Store) onPath(entry, tip string) (bool, error) {
	for id := tip; id != ""; {
		if id == entry {
			return true, nil
		}
		parent, err := s.parentOf(id)
		if err != nil {
			return false, err
		}
		id = parent
	}
	return false, nil
}

// own records that session's log holds entry, in the index once there
// is one.
func (s *Store) own(entry, session string) {
	s.idx.Lock()
	defer s.idx.Unlock()
	if s.building {
		s.deltas = append(s.deltas, func(owners map[string]map[string]bool, _ map[string]bool) {
			addOwner(owners, entry, session)
		})
	}
	if s.indexed {
		addOwner(s.owners, entry, session)
	}
}

func addOwner(owners map[string]map[string]bool, entry, session string) {
	set := owners[entry]
	if set == nil {
		set = map[string]bool{}
		owners[entry] = set
	}
	set[session] = true
}

// disown takes a deleted session out of the index.
func (s *Store) disown(session string) {
	drop := func(owners map[string]map[string]bool, _ map[string]bool) {
		for e, set := range owners {
			delete(set, session)
			if len(set) == 0 {
				delete(owners, e)
			}
		}
	}
	s.idx.Lock()
	defer s.idx.Unlock()
	if s.building {
		s.deltas = append(s.deltas, drop)
	}
	if s.indexed {
		drop(s.owners, s.prefix)
	}
}

// anyOwner returns one session whose log holds the entry, or "". The
// caller holds idx.
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
	if err := s.writable(); err != nil {
		return "", err
	}
	hash := hashBytes(data)
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return "", err
	}
	defer guard.release()
	pend := newPendSet()
	if err := s.objs.writeTo(spaceContents, hash, data, true, pend); err != nil {
		return "", fmt.Errorf("cas: store blob: %w", err)
	}
	if err := s.objs.flushSet(pend); err != nil {
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

// view is one session as its log says, beside what its indexes say.
type view struct {
	exists bool
	log    []string
	sizes  map[string]int64
	head   string
	mark   string

	// adopt lists the entries of lazy appends no sync record covers
	// whose objects are present: a writing store that recovers the
	// session commits them, since a durable append it makes next must
	// not be cut with them by a later crash.
	adopt []string
	// dropped lists the appends found lost that no lost record names
	// yet: a writing store that recovers the session records each, so a
	// sync record after them does not bring them back.
	dropped []string
	// whole is where the log's whole lines end; a longer log has a torn
	// tail, which the holder truncates, and an unterminated one a last
	// record without its newline, which the holder ends.
	whole, size  int64
	unterminated bool
	// damage is damage that lost no record, which Verify reports.
	damage []LogDamage

	headChanged, markChanged bool
}

// reconcile works out one session's state from its log, without writing
// anything. The log is the record: its appends are the session's
// entries, in order, and its last head is the head, except that a lazy
// append whose objects a crash took is lost with every record the
// session accepted after it, head moves and marks included, since a
// commit would have made those objects durable first.
func (s *Store) reconcile(id, dir string) (view, error) {
	return s.reconcileAs(id, dir, false)
}

// committedView is reconcile's view of what a session has committed:
// every lazy append no sync record covers is taken as cut, whether or
// not its objects are there, and nothing is written. It is what a store
// that cannot commit another process's working state serves.
func (s *Store) committedView(id, dir string) (view, error) {
	return s.reconcileAs(id, dir, true)
}

func (s *Store) reconcileAs(id, dir string, committedOnly bool) (view, error) {
	var v view
	hdr, err := readHeader(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		return v, err
	}
	v.exists = true
	l, err := readSessionLog(dir, 0)
	if err != nil {
		return v, err
	}
	if l.legacy {
		return v, fmt.Errorf("cas: session %s: %w", id, ErrLegacyStore)
	}
	cutAt := len(l.recs)
	if l.lost && !tailLoss(l) {
		// The log is the only record of the session: a damaged record
		// is reported, never passed over as if it were not there.
		return v, fmt.Errorf("cas: session %s: %w", id, firstLoss(l))
	}
	v.whole, v.size, v.unterminated, v.damage = l.whole, l.size, l.unterminated, l.damage
	if l.lost {
		// Damage in the uncommitted tail: the tail is cut from the
		// damaged line, as a crash's loss, and the holder truncates the
		// log there.
		cutAt, v.whole, v.unterminated, v.damage = l.lossRec, l.lossOff, false, nil
		for _, d := range l.damage {
			if d.Offset < l.lossOff {
				v.damage = append(v.damage, d)
			}
		}
	}
	fileHead, err := readHead(dir)
	if err != nil {
		return v, err
	}
	fileMark := readMark(dir)
	v.sizes = map[string]int64{}
	v.head, v.mark = hdr.Base, ""
	seen := map[string]bool{}
	lost := map[string]bool{}
	// A sync record says every lazy record before it is durable, apart
	// from the ones a lost record already said were lost.
	synced := -1
	// lostAt is where the last lost record naming each entry is: an
	// append of it before that is gone, one after it is kept.
	lostAt := map[string]int{}
	for i, r := range l.recs {
		switch r.Op {
		case opSync:
			synced = i
		case opLost:
			lostAt[r.Entry] = i
		}
	}
	gone := func(entry string, i int) bool {
		at, ok := lostAt[entry]
		return ok && at > i
	}
	cut := false
	var dropped []string
	for i, r := range l.recs {
		if i >= cutAt {
			cut = true
		}
		if cut {
			if r.Op == opAppend && r.Entry != "" {
				lost[r.Entry] = true
				if !gone(r.Entry, i) {
					dropped = append(dropped, r.Entry)
				}
			}
			continue
		}
		switch r.Op {
		case opAppend:
			if r.Entry == "" {
				continue
			}
			if gone(r.Entry, i) {
				lost[r.Entry] = true // lost, and recorded so, earlier
				continue
			}
			if r.Lazy && i > synced && committedOnly {
				cut = true
				continue
			}
			if r.Lazy && i > synced {
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
			if !seen[r.Entry] {
				seen[r.Entry] = true
				v.log = append(v.log, r.Entry)
				if r.Size > 0 {
					v.sizes[r.Entry] = r.Size
				}
			}
			if r.Head != "" {
				v.head = r.Head
			}
		case opHead:
			if !gone(r.Head, i) {
				v.head = r.Head
			}
		case opMark:
			v.mark = r.Mark
		}
	}
	for _, e := range v.log {
		delete(lost, e) // appended again after the loss, and kept
	}
	for _, e := range dropped {
		if lost[e] {
			v.dropped = append(v.dropped, e)
		}
	}
	if v.mark == "" {
		v.mark = fileMark
	}
	v.headChanged = v.head != fileHead
	v.markChanged = v.mark != fileMark
	return v, nil
}

// recoverSession makes one session's log and indexes say what reconcile
// says, under that session's lock: a torn tail is cut, lost appends are
// recorded, lazy appends that survived are committed, and HEAD and the
// mark are rewritten. A read-only store writes nothing.
func (s *Store) recoverSession(id, dir string) (view, error) {
	v, err := s.reconcile(id, dir)
	if err != nil || s.readOnly || !v.exists || s.objs.stopped() != nil {
		// A read-only store, and one stopped by a failed fsync, recover
		// in memory and write nothing.
		return v, err
	}
	if v.whole < v.size || v.unterminated || len(v.dropped) > 0 || len(v.adopt) > 0 {
		if err := s.writeRecovered(id, dir, v); err != nil {
			return v, fmt.Errorf("cas: session %s: %w", id, err)
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

// writeRecovered writes the log recovery found as a new file: its
// whole lines, a torn tail cut, a lost record for each append it found
// lost and, when it keeps working state, a sync record, after the
// objects of that state are written afresh. Nothing is fsynced where it
// lies. A process whose fsync of the log or an object failed was told
// so once, and the pages it failed to write stay in memory marked
// written: an fsync of the old file now would succeed for them, and a
// sync record after it would claim them. Written again, they reach the
// disk, or this store stops.
func (s *Store) writeRecovered(id, dir string, v view) error {
	pend := newPendSet()
	for _, e := range v.adopt {
		if err := s.freshenEntry(e, pend); err != nil {
			return err
		}
	}
	if err := s.objs.flushSet(pend); err != nil {
		return err
	}
	path := filepath.Join(dir, logName)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) < v.whole {
		return fmt.Errorf("the log is %d bytes, shorter than the %d recovery read", len(data), v.whole)
	}
	data = data[:v.whole:v.whole]
	if l, err := parseSessionLog(bytes.NewReader(data), 0, v.whole); err != nil || l.lost || l.whole != v.whole {
		return errors.New("the log changed as recovery read it")
	}
	if v.unterminated {
		data = append(data, '\n')
	}
	var recs []logRecord
	for _, e := range v.dropped {
		// Before the sync record, which would otherwise say the lost
		// appends were durable.
		recs = append(recs, logRecord{Op: opLost, Session: id, Entry: e})
	}
	if len(v.adopt) > 0 {
		recs = append(recs, logRecord{Op: opSync, Session: id})
	}
	more, err := encodeRecords(true, recs)
	if err != nil {
		return err
	}
	return s.objs.writeAtomic(path, append(data, more...))
}

// index builds what the store holds from every session's log and base.
// It reads every session, so it is built only when a lookup needs it.
// index builds the index by reading every session, with no lock held,
// so appends and opens of every session go on meanwhile; what they
// record while it runs is applied to what it built. Unless force is
// set, a build is not repeated once one has finished.
func (s *Store) index(force bool) error {
	s.indexing.Lock()
	defer s.indexing.Unlock()
	s.idx.Lock()
	if s.indexed && !force {
		s.idx.Unlock()
		return nil
	}
	s.building, s.deltas = true, nil
	s.idx.Unlock()
	owners, prefix, err := s.scanIndex()
	s.idx.Lock()
	defer s.idx.Unlock()
	deltas := s.deltas
	s.building, s.deltas = false, nil
	if err != nil {
		return err
	}
	for _, d := range deltas {
		d(owners, prefix)
	}
	s.owners, s.prefix, s.indexed = owners, prefix, true
	return nil
}

// scanning, when set, is called as scanIndex reads each session, so a
// test can hold a build part way.
var scanning func()

// scanIndex reads what every session's log holds and every session's
// prefix.
func (s *Store) scanIndex() (map[string]map[string]bool, map[string]bool, error) {
	owners, prefix := map[string]map[string]bool{}, map[string]bool{}
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return nil, nil, fmt.Errorf("cas: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || !validSessionID(d.Name()) {
			continue
		}
		if scanning != nil {
			scanning()
		}
		dir := filepath.Join(s.root, "sessions", d.Name())
		l, err := readSessionLog(dir, 0)
		if err == nil && l.legacy {
			err = ErrLegacyStore
		}
		if err != nil {
			s.setFaulty(d.Name(), err)
			continue
		}
		for _, h := range heldEntries(l.recs) {
			addOwner(owners, h, d.Name())
		}
		h, err := readHeader(dir)
		if err != nil {
			continue
		}
		if h.Base != "" {
			path, err := s.pathTo(h.Base)
			if err != nil {
				s.setFaulty(d.Name(), err)
				continue
			}
			for _, e := range path {
				prefix[e] = true
			}
		}
		s.setFaulty(d.Name(), nil)
	}
	return owners, prefix, nil
}

// setFaulty records why a session could not be read, or with nil that
// it could.
func (s *Store) setFaulty(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.faulty[id] = err
	} else {
		delete(s.faulty, id)
	}
}

// notePrefix records the path to a new session's base in the index,
// once there is one; the path is read with no lock held.
func (s *Store) notePrefix(base string) error {
	path, err := s.pathTo(base)
	if err != nil {
		return err
	}
	mark := func(_ map[string]map[string]bool, prefix map[string]bool) {
		for _, e := range path {
			prefix[e] = true
		}
	}
	s.idx.Lock()
	defer s.idx.Unlock()
	if s.building {
		s.deltas = append(s.deltas, mark)
	}
	if s.indexed {
		mark(s.owners, s.prefix)
	}
	return nil
}

// pathTo returns the entries on the path to base, base first.
func (s *Store) pathTo(base string) ([]string, error) {
	var path []string
	for id := base; id != ""; {
		path = append(path, id)
		parent, err := s.parentOf(id)
		if err != nil {
			return nil, err
		}
		id = parent
	}
	return path, nil
}

// --- per-session files ---

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

// writeHead writes the HEAD index. It is not fsynced: the log has every
// head move, and recovery writes a HEAD a crash left stale.
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

// writeHeader writes the header durably: it is not in the log.
func writeHeader(o *objects, dir string, h agentsession.Header) error {
	hdr, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if hdr, err = jcs.Transform(hdr); err != nil {
		return err
	}
	return o.writeAtomic(filepath.Join(dir, "header"), append(hdr, '\n'))
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
	if err := writeHeader(s.objs, h.dir, hdr); err != nil {
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
	if err := s.writable(); err != nil {
		return nil, err
	}
	return s.createLocked(ctx, h, MarkRecord)
}

func (s *Store) createLocked(ctx context.Context, h agentsession.Header, mark string) (*agentsession.Session, error) {
	var (
		sess   *agentsession.Session
		prefix [][]byte
		owner  string
	)
	if h.Base != "" {
		o, held, err := s.holderOf(h.Base, h.ParentSession)
		owner = o
		if err != nil {
			return nil, err
		}
		if !held {
			return nil, fmt.Errorf("%w: base %s is not held by this store", agentsession.ErrNoEntry, h.Base)
		}
		if isLabel, err := s.isLeafLabel(h.Base); err != nil {
			return nil, err
		} else if isLabel {
			return nil, errors.New("cas: a base may not be a leaf label")
		}
		if owner != "" {
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
	slot, held := s.claim(h.ID)
	if held {
		slot.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	lk, err := s.lockSession(h.ID)
	if err != nil {
		s.abandon(h.ID, slot)
		return nil, err
	}
	fail := func(err error) (*agentsession.Session, error) {
		lk.release()
		s.abandon(h.ID, slot)
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		return fail(fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID))
	} else if !errors.Is(err, os.ErrNotExist) {
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
	// No sweep's last step from here until the session names the
	// prefix. The prefix is freshened, so it is durable and young once
	// the lock is dropped, and the origin's log is committed through the
	// base, since the fork's recovery reads only its own log.
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return fail(err)
	}
	defer guard.release()
	if h.Base != "" {
		pend := newPendSet()
		if err := s.freshenPath(h.Base, pend); err != nil {
			return fail(err)
		}
		// The path's objects are durable before any log that names the
		// base is, whichever process wrote them.
		if err := s.objs.flushSet(pend); err != nil {
			return fail(err)
		}
		if err := s.commitOrigin(owner, h.Base); err != nil {
			return fail(err)
		}
	}
	recs := []logRecord{{Op: opCreate, Session: h.ID, Base: h.Base}, {Op: opMark, Session: h.ID, Mark: mark}}
	if err := s.placeSession(dir, h, recs, h.Base, mark); err != nil {
		return fail(err)
	}
	if h.Base != "" {
		if err := s.notePrefix(h.Base); err != nil {
			return fail(err)
		}
	}
	logf, err := openLog(dir)
	if err != nil {
		return fail(fmt.Errorf("cas: log: %w", err))
	}
	slot.take(&handle{session: sess, dir: dir, lock: lk, logf: logf, pend: newPendSet(), mark: mark, head: h.Base, count: sess.Len(), diskFormat: h.Format, own: map[string]bool{}})
	slot.mu.Unlock()
	return sess, nil
}

// placeSession creates a session in one step, as RFC 0002 requires:
// its header, log, head and mark are written and fsynced in a directory
// of their own under tmp, which is then renamed to the session's place.
// The caller holds the session's lock and has checked the place free.
func (s *Store) placeSession(dir string, h agentsession.Header, recs []logRecord, head, mark string) error {
	stageRoot := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(stageRoot, 0o755); err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	stage, err := os.MkdirTemp(stageRoot, h.ID+"-")
	if err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	fail := func(err error) error {
		os.RemoveAll(stage)
		return err
	}
	if err := writeHeader(s.objs, stage, h); err != nil {
		return fail(fmt.Errorf("cas: header: %w", err))
	}
	data, err := encodeRecords(true, recs)
	if err != nil {
		return fail(err)
	}
	for _, f := range []struct {
		name string
		data []byte
	}{{logName, data}, {"HEAD", []byte(head + "\n")}, {"record", []byte(mark + "\n")}} {
		if _, err := s.objs.writeFile(filepath.Join(stage, f.name), f.data, true); err != nil {
			return fail(fmt.Errorf("cas: %s: %w", f.name, err))
		}
	}
	if err := s.objs.fsyncDir(stage); err != nil {
		return fail(err)
	}
	if err := os.Rename(stage, dir); err != nil {
		return fail(fmt.Errorf("cas: %w", err))
	}
	// A failed sync here stops the store, so nothing is committed into
	// a session whose place a crash could still take.
	return s.objs.fsyncDir(filepath.Join(s.root, "sessions"))
}

// commitOrigin commits the log of the session holding a fork's base
// through the base, so a crash cannot take the base from under a fork
// that hangs from it. A session this store holds commits its working
// state. For one another process holds, the objects of every lazy
// append up to the base's record are made durable here, since recovery
// cuts the log at the first lazy append whose objects are gone, whether
// or not it is the base's ancestor; then its log is fsynced. A lazy
// append whose objects are gone already means the base will not
// survive recovery, and the fork is refused.
func (s *Store) commitOrigin(owner, base string) error {
	if owner == "" {
		return nil
	}
	if h := s.held(owner); h != nil {
		defer h.mu.Unlock()
		return s.commitHandle(owner, h)
	}
	dir, err := s.sessionDir(owner)
	if err != nil {
		return nil
	}
	l, err := readSessionLog(dir, 0)
	if err == nil && l.legacy {
		err = ErrLegacyStore
	}
	if err == nil && l.lost {
		err = firstLoss(l)
	}
	if err != nil {
		return fmt.Errorf("cas: session %s: %w", owner, err)
	}
	at, synced := -1, -1
	for i, r := range l.recs {
		switch {
		case r.Op == opSync:
			synced = i
		case r.Op == opAppend && r.Entry == base && at < 0:
			at = i
		}
	}
	lostAt := map[string]int{}
	for i, r := range l.recs {
		if r.Op == opLost {
			lostAt[r.Entry] = i
		}
	}
	pend := newPendSet()
	for i := synced + 1; i <= at; i++ {
		r := l.recs[i]
		if r.Op != opAppend || !r.Lazy || r.Entry == "" {
			continue
		}
		if k, ok := lostAt[r.Entry]; ok && k > i {
			continue
		}
		if err := s.freshenEntry(r.Entry, pend); err != nil {
			return fmt.Errorf("%w: base %s: its origin %s holds an uncommitted append before it that is gone: %v", agentsession.ErrNoEntry, base, owner, err)
		}
	}
	if err := s.objs.flushSet(pend); err != nil {
		return err
	}
	return s.objs.fsyncFile(filepath.Join(dir, logName))
}

// freshenEntry writes an entry's two objects durably, rewriting any a
// crash or a sweep took.
func (s *Store) freshenEntry(id string, pend *pendSet) error {
	if err := s.objs.freshenTo(spaceEntries, id, pend); err != nil {
		return err
	}
	c, err := s.contentOf(id)
	if err != nil {
		return err
	}
	return s.objs.freshenTo(spaceContents, c, pend)
}

// withSync appends a sync record to records a durable commit writes when
// the session has working state, which the commit makes durable.
func (s *Store) withSync(h *handle, id string, recs ...logRecord) []logRecord {
	if h.lazy {
		recs = append(recs, logRecord{Op: opSync, Session: id})
	}
	return recs
}

// commitHandle commits a held session's working state: its objects, a
// sync record, and the log.
func (s *Store) commitHandle(id string, h *handle) error {
	if !h.lazy {
		return nil
	}
	if err := s.appendRecords(h, h.dir, true, logRecord{Op: opSync, Session: id}); err != nil {
		return err
	}
	h.lazy = false
	return nil
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
	h, err := s.hold(id)
	if err != nil {
		return nil, err
	}
	defer h.mu.Unlock()
	return h.session, nil
}

// hold returns the handle of a session, opening it if this store does
// not hold it, with the handle's lock taken; the caller lets it go.
func (s *Store) hold(id string) (*handle, error) {
	h, held := s.claim(id)
	if held {
		return h, nil
	}
	n, err := s.openSession(id)
	if err != nil {
		s.abandon(id, h)
		return nil, err
	}
	h.take(n)
	return h, nil
}

// held returns the handle of a session this store holds, its lock
// taken, or nil when it holds none.
func (s *Store) held(id string) *handle {
	h, held := s.claim(id)
	if !held {
		s.abandon(id, h)
		return nil
	}
	return h
}

// claim returns the handle of a session with its lock taken, and true,
// when this store holds the session; otherwise a new, empty handle in
// its place, its lock taken, which the caller fills with take and lets
// go, or gives up with abandon. A second caller waits on the first.
func (s *Store) claim(id string) (*handle, bool) {
	for {
		s.mu.Lock()
		h, ok := s.open[id]
		if !ok {
			h = &handle{}
			h.mu.Lock()
			s.open[id] = h
			s.mu.Unlock()
			return h, false
		}
		s.mu.Unlock()
		h.mu.Lock()
		if !h.gone {
			return h, true
		}
		h.mu.Unlock()
	}
}

// abandon gives up a handle whose lock the caller holds: it leaves the
// store's sessions, and a caller waiting on it looks again.
func (s *Store) abandon(id string, h *handle) {
	s.forget(id, h)
	h.mu.Unlock()
}

// forget takes a handle out of the store's sessions.
func (s *Store) forget(id string, h *handle) {
	s.mu.Lock()
	if s.open[id] == h {
		delete(s.open, id)
	}
	s.mu.Unlock()
	h.gone = true
}

// take fills a claimed handle with an opened session.
func (h *handle) take(n *handle) {
	h.session, h.dir, h.lock, h.mark, h.head = n.session, n.dir, n.lock, n.mark, n.head
	h.count, h.diskFormat, h.own, h.logf, h.pend, h.lazy = n.count, n.diskFormat, n.own, n.logf, n.pend, n.lazy
}

// reopen rebuilds a held session from what the store holds, keeping
// its lock; the handle is let go if that fails.
func (s *Store) reopen(id string, h *handle) error {
	if h.logf != nil {
		h.logf.Close()
		h.logf = nil
	}
	n, err := s.openHeld(id, h.dir, h.lock)
	if err != nil {
		s.forget(id, h) // openHeld let the lock go
		return err
	}
	h.take(n)
	return nil
}

// handles returns the sessions this store holds.
func (s *Store) handles() map[string]*handle {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]*handle, len(s.open))
	for id, h := range s.open {
		out[id] = h
	}
	return out
}

// openSession recovers and opens a session this store does not hold,
// taking its lock.
func (s *Store) openSession(id string) (*handle, error) {
	dir, err := s.sessionDir(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	s.mu.Lock()
	ferr, faulty := s.faulty[id]
	s.mu.Unlock()
	if faulty {
		return nil, fmt.Errorf("cas: session %s could not be indexed: %w", id, ferr)
	}
	lk, err := s.lockSession(id)
	if err != nil {
		return nil, err
	}
	return s.openHeld(id, dir, lk)
}

// openHeld recovers and opens a session whose lock the caller has
// taken, which it keeps, or lets go on failure.
func (s *Store) openHeld(id, dir string, lk *dirLock) (*handle, error) {
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
	sess, hdr, err := s.build(id, dir, v)
	if err != nil {
		return fail(err)
	}
	diskFormat := hdr.Format
	committed := make(map[string]bool, len(v.log))
	for _, h := range v.log {
		committed[h] = true
		s.own(h, id) // another process may have appended since the index was built
	}
	var logf *os.File
	if !s.readOnly {
		if logf, err = openLog(dir); err != nil {
			return fail(fmt.Errorf("cas: log: %w", err))
		}
	}
	return &handle{session: sess, dir: dir, lock: lk, logf: logf, pend: newPendSet(), mark: v.mark, head: v.head, count: sess.Len(), diskFormat: diskFormat, own: committed}, nil
}

// build assembles the session a view says: the path to its base, then
// the entries of its log.
func (s *Store) build(id, dir string, v view) (*agentsession.Session, agentsession.Header, error) {
	hdr, err := readHeader(dir)
	if err != nil {
		return nil, hdr, err
	}
	var prefix [][]byte
	if hdr.Base != "" {
		if prefix, err = s.pathLines(hdr.Base); err != nil {
			return nil, hdr, err
		}
	}
	own := make([][]byte, 0, len(v.log))
	for _, h := range v.log {
		line, err := s.loadLine(h)
		if err != nil {
			return nil, hdr, fmt.Errorf("cas: session %s: %w", id, err)
		}
		own = append(own, line)
	}
	sess, err := s.assemble(hdr, prefix, own, v.head)
	return sess, hdr, err
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
	case SyncNever:
		return hdr.HasRecord(e.EntryType())
	}
	return false
}

// Write appends an entry and reports what happened, as the format asks
// a store to: whether the entry continued the head, branched, was
// already held, or as a leaf label moved the head or could not, and
// whether the append was durable when it returned. The order is the
// store's one order: the entry is prepared, its objects are written,
// the log record is accepted, committed when durable, the head index
// follows at a commit, and only then is the entry visible in the
// session. A session held as a
// mirror is refused, as is a leaf label carrying `synthetic`. A head
// moved in memory through Session.Branch since the last commit is
// logged first, so a head move never bypasses the log. The first
// append to a session whose header names an earlier minor raises the
// header's format first.
func (s *Store) Write(ctx context.Context, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
	if err := ctx.Err(); err != nil {
		return agentsession.Result{}, err
	}
	if err := s.writable(); err != nil {
		return agentsession.Result{}, err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return agentsession.Result{}, err
	}
	r, err := s.writeHeld(ctx, h, sessionID, e)
	h.mu.Unlock()
	if err == nil && r.Outcome != agentsession.Held {
		// A pack runs in the background, with no session's lock held,
		// as another process's would, so no append waits for it; one at
		// a time.
		s.mu.Lock()
		s.appends++
		pack := s.appends%autoPackEvery == 0 && !s.closing && s.packing.CompareAndSwap(false, true)
		if pack {
			s.background.Add(1)
		}
		s.mu.Unlock()
		if pack {
			go func() {
				defer s.background.Done()
				defer s.packing.Store(false)
				s.maybePack()
			}()
		}
	}
	return r, err
}

// writeHeld is Write on a session whose handle's lock the caller holds.
func (s *Store) writeHeld(ctx context.Context, h *handle, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
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
	size, body, err := s.storeEntry(e, durable, h.pend)
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
	for _, b := range blobsNamedBy(body) {
		if err := s.objs.freshenTo(spaceContents, b, h.pend); err != nil {
			r.Unresolved = append(r.Unresolved, b)
		}
	}
	head := ""
	switch r.Outcome {
	case agentsession.Continued:
		head = r.ID
	case agentsession.LeafMoved:
		head = e.(*agentsession.LabelEntry).Target
	}
	recs := []logRecord{{Op: opAppend, Session: sessionID, Entry: r.ID, Head: head, Seq: h.session.Len() + 1, Size: size}}
	if durable && h.lazy {
		// The commit flushes the working state before it, objects first.
		recs = append(recs, logRecord{Op: opSync, Session: sessionID})
	}
	err = s.appendRecords(h, h.dir, durable, recs...)
	guard.release()
	if err != nil {
		return agentsession.Result{}, err
	}
	h.lazy = !durable
	r.Durable = durable
	// The append is accepted from here, and committed if durable. HEAD
	// is an index the next open rebuilds from the log, written at a
	// commit and when the session is released; a failure to write it is
	// not a failed append and is not reported as one.
	if head != "" {
		if durable {
			_ = writeHead(h.dir, head)
		}
		h.head = head
	}
	s.own(r.ID, sessionID)
	h.own[r.ID] = true
	if h.session.Leaf() != leafAtPrepare {
		// A reader moved the leaf between Prepare and Commit, through
		// Session.Branch, which does not take the store's lock. The
		// log has spoken; the store is authoritative, and the move
		// is undone rather than recorded as something it was not.
		if leafAtPrepare == "" {
			h.session.ResetLeaf()
		} else if err := h.session.Branch(leafAtPrepare); err != nil {
			return s.committedButNotApplied(sessionID, h, r)
		}
	}
	if _, err := h.session.Commit(e); err != nil {
		return s.committedButNotApplied(sessionID, h, r)
	}
	h.count++
	return r, nil
}

// committedButNotApplied is what Write returns when the log record is
// accepted and the in-memory session could not be brought in step: the
// append happened, so it is reported as a success, and the handle is
// dropped so the next Open rebuilds the session from what the store
// holds. Nothing after the commit point may turn a committed append into
// a reported failure.
func (s *Store) committedButNotApplied(sessionID string, h *handle, r agentsession.Result) (agentsession.Result, error) {
	s.dropHandle(sessionID, h)
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

// syncHead logs a head the session moved in memory, through
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
		if !h.own[leaf] && leaf != h.session.Header().Base {
			return fmt.Errorf("%w: head %s is not in the log", ErrModified, leaf)
		}
	} else if h.session.Header().Base != "" {
		return fmt.Errorf("%w: a session with a base has a head", agentsession.ErrNoEntry)
	}
	if err := s.appendRecords(h, h.dir, true, s.withSync(h, sessionID, logRecord{Op: opHead, Session: sessionID, Head: leaf, Seq: h.session.Len()})...); err != nil {
		return err
	}
	h.lazy = false
	_ = writeHead(h.dir, leaf) // an index; the log has it
	h.head = leaf
	return nil
}

// SetHead moves a session's head from expected to the entry to, or
// returns ErrHeadMoved when the head is not expected. "" is a valid
// expected value for a session with no head. The target must be the
// base or an own entry and not a leaf label; a mirror is refused. The
// log record is committed, durably, before the head moves anywhere.
func (s *Store) SetHead(ctx context.Context, sessionID, expected, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
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
	if err := s.appendRecords(h, h.dir, true, s.withSync(h, sessionID, logRecord{Op: opHead, Session: sessionID, Head: to, Seq: h.session.Len()})...); err != nil {
		return err
	}
	h.lazy = false
	_ = writeHead(h.dir, to) // an index; the log has it
	h.head = to
	if err := h.session.Branch(to); err != nil {
		// Committed; the session is rebuilt on the next Open.
		s.dropHandle(sessionID, h)
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
	h, err := s.hold(sessionID)
	if err != nil {
		return "", err
	}
	defer h.mu.Unlock()
	return h.mark, nil
}

// setMark commits a mark and applies it.
func (s *Store) setMark(h *handle, sessionID, mark string) error {
	if err := s.appendRecords(h, h.dir, true, s.withSync(h, sessionID, logRecord{Op: opMark, Session: sessionID, Mark: mark})...); err != nil {
		return err
	}
	h.lazy = false
	_ = writeIndex(filepath.Join(h.dir, "record"), []byte(mark+"\n")) // an index; the log has it
	h.mark = mark
	return nil
}

// DeclareRecord makes this store the record for a session it holds as
// a mirror: what a mirror does when the record deleted the session
// without handing it over, or an importer does for a file it knows to
// be the only copy.
func (s *Store) DeclareRecord(ctx context.Context, sessionID string) error {
	if err := s.writable(); err != nil {
		return err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
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
// a header is no session and is not listed. Summary.Size is the bytes of the
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
		var out []agentsession.Summary
		var errs []error
		meta := f.WithNames || f.Current
		for _, d := range dirs {
			if !d.IsDir() || !validSessionID(d.Name()) {
				continue
			}
			id := d.Name()
			dir := filepath.Join(s.root, "sessions", id)
			// The header decides most filters, and a session it rules
			// out is not read further.
			hdr, err := readHeader(dir)
			if err != nil || !f.Matches(hdr) {
				continue // filtered out
			}
			if c, ok := cachedSummary(dir, meta); ok {
				sum := agentsession.Summary{Header: hdr, Path: dir, Size: c.Size, Name: c.Name, SupersededBy: c.SupersededBy}
				if info, err := os.Stat(filepath.Join(dir, logName)); err == nil {
					sum.Modified = info.ModTime()
				}
				if f.Keep(sum) {
					out = append(out, sum)
				}
				continue
			}
			size, modified := logStamp(dir)
			v, err := s.reconcile(id, dir)
			if err != nil {
				errs = append(errs, fmt.Errorf("cas: %s: %w", id, err))
				continue
			}
			if !v.exists {
				continue
			}
			sum, err := s.summarize(id, dir, v, meta)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			// A summary is kept only of what is committed: working state a
			// crash may take would otherwise outlive the crash in it,
			// the log's stamp unchanged until the session is next opened.
			if !s.readOnly && len(v.adopt) == 0 {
				keepSummary(dir, size, modified, sum, meta)
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

// Delete implements agentsession.Store: it renames the session's
// directory out of sessions, in one step after which nothing of it is
// read, so a later session under the same ID starts from nothing, and
// then removes it. Objects stay; what no log and no prefix needs is
// swept by Sweep. A directory a crash left in the trash is removed by
// the next sweep. A Delete that returns ErrStopped may have renamed the
// session away; the next open of the store finds it gone or not, as
// the disk holds it.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	h, held := s.claim(id)
	var lk *dirLock
	if held {
		if h.logf != nil {
			h.logf.Close()
			h.logf = nil
		}
		lk = h.lock
	} else if lk, err = s.lockSession(id); err != nil {
		s.abandon(id, h)
		return err
	}
	defer func() {
		lk.release()
		s.abandon(id, h)
	}()
	// One step, after which nothing of the session is read: its
	// directory is renamed out of sessions, and removed after.
	trash := filepath.Join(s.root, "trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	gone := filepath.Join(trash, fmt.Sprintf("%s-%d", id, time.Now().UnixNano()))
	if err := os.Rename(dir, gone); err != nil {
		return fmt.Errorf("cas: delete %s: %w", id, err)
	}
	if err := s.objs.fsyncDir(filepath.Join(s.root, "sessions")); err != nil {
		return err
	}
	s.disown(id)
	_ = os.RemoveAll(gone)
	return nil
}

// Release closes a session this process holds, freeing its lock. Its
// working state is committed first, so the next holder takes up a log
// it need not check.
func (s *Store) Release(id string) error {
	h := s.held(id)
	if h == nil {
		return nil
	}
	defer h.mu.Unlock()
	return s.releaseHandle(id, h)
}

// releaseHandle commits a held session's working state, brings its
// indexes up to date, and lets it go.
func (s *Store) releaseHandle(id string, h *handle) error {
	var err error
	if !s.readOnly {
		err = s.commitHandle(id, h)
		if err == nil {
			// Indexes of what was committed; after a failed commit, the
			// next open rebuilds them from the log.
			_ = writeHead(h.dir, h.head)
			s.summarizeHeld(id, h)
		}
	}
	s.dropHandle(id, h)
	return err
}

// dropHandle forgets a held session without writing anything: its log
// is closed and its lock let go. The caller holds the handle's lock, and
// lets it go after.
func (s *Store) dropHandle(id string, h *handle) {
	s.forget(id, h)
	if h.logf != nil {
		h.logf.Close()
		h.logf = nil
	}
	h.lock.release()
}

// Sync commits the working state of every session this store holds:
// the commit on its own that RFC 0002 asks a store to offer.
func (s *Store) Sync(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	var first error
	for id, h := range s.handles() {
		h.mu.Lock()
		if !h.gone {
			if err := s.commitHandle(id, h); err != nil && first == nil {
				first = err
			}
		}
		h.mu.Unlock()
	}
	return first
}

// Close commits and releases every session this store holds.
func (s *Store) Close() error {
	var first error
	for id, h := range s.handles() {
		h.mu.Lock()
		if !h.gone {
			if err := s.releaseHandle(id, h); err != nil && first == nil {
				first = err
			}
		}
		h.mu.Unlock()
	}
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.background.Wait()
	if !s.readOnly {
		if err := s.objs.flush(); err != nil && first == nil {
			first = err
		}
		s.maybePack()
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
	h, err := s.hold(sessionID)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
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
	h, err := s.hold(sessionID)
	if err != nil {
		return "", err
	}
	defer h.mu.Unlock()
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
	if err := s.writable(); err != nil {
		return nil, err
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
	dir, err := s.sessionDir(h.ID)
	if err != nil {
		return nil, err
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
		if owner, _, err := s.holderOf(h.Base, h.ParentSession); err == nil && owner != "" {
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
	slot, held := s.claim(h.ID)
	if held {
		slot.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	hd, err := s.admitNew(ctx, dir, h, mark, stored, own, nil, sess.Leaf())
	if err != nil {
		s.abandon(h.ID, slot)
		return nil, err
	}
	slot.take(hd)
	slot.mu.Unlock()
	return hd.session, nil
}

// admitNew takes in a session the store lacks: its objects as one pack,
// committed, then the session placed in one step with its create
// record, its mark, an append record per own entry in order and its
// head. Nothing is in place until the rename, so a failure before it
// leaves the ID free. It returns the session as the store holds it,
// rebuilt from the objects and the log, without letting its lock go.
func (s *Store) admitNew(ctx context.Context, dir string, h agentsession.Header, mark string, stored, own []agentsession.Entry, blobs map[string][]byte, head string) (*handle, error) {
	lk, err := s.lockSession(h.ID)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err == nil {
		lk.release()
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	} else if !errors.Is(err, os.ErrNotExist) {
		lk.release()
		return nil, fmt.Errorf("cas: %w", err)
	}
	guard, err := s.writeGuard(ctx)
	if err != nil {
		lk.release()
		return nil, err
	}
	defer guard.release()
	fail := func(err error) (*handle, error) {
		lk.release()
		return nil, err
	}
	pend := newPendSet()
	sizes, err := s.packEntries(stored, blobs, pend)
	if err != nil {
		return fail(err)
	}
	// What packEntries left loose reaches the disk before the log that
	// names it: the session is committed before it is acknowledged.
	if err := s.objs.flushSet(pend); err != nil {
		return fail(err)
	}
	recs := []logRecord{{Op: opCreate, Session: h.ID, Base: h.Base}, {Op: opMark, Session: h.ID, Mark: mark}}
	hashes := make([]string, 0, len(own))
	for i, e := range own {
		id := e.Base().ID
		hashes = append(hashes, id)
		recs = append(recs, logRecord{Op: opAppend, Session: h.ID, Entry: id, Seq: i + 1, Size: sizes[id]})
	}
	first := h.Base
	if head != "" {
		recs = append(recs, logRecord{Op: opHead, Session: h.ID, Head: head, Seq: len(own)})
		first = head
	}
	if err := s.placeSession(dir, h, recs, first, mark); err != nil {
		return fail(err)
	}
	for _, id := range hashes {
		s.own(id, h.ID)
	}
	if h.Base != "" {
		if err := s.notePrefix(h.Base); err != nil {
			return fail(err)
		}
	}
	return s.openHeld(h.ID, dir, lk)
}

var _ agentsession.Store = (*Store)(nil)
