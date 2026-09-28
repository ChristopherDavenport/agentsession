// Package cas is the content-addressed session store RFC 0002 describes,
// laid out on a filesystem the way git lays out a repository:
//
//	<root>/
//	  objects/contents/<2 hex>/<62 hex>   a body, canonical bytes, held once
//	  objects/entries/<2 hex>/<62 hex>    an envelope: type, parent, parents, ts, content
//	  sessions/<session id>/
//	    header                            the header line, written once
//	    log                               one entry hash per line, in append order
//	    HEAD                              the head's hash, or empty
//	    record                            "record" or "mirror"
//	  locks/<session id>                  the lock of the process holding the session
//	  sweep.lock                          the lock of a running sweep
//	  journal                             one line per commit: the commit point
//
// An entry is stored as two objects, its body under the content hash
// and its envelope under the id, so a body shared by many entries is
// held once and a chain of envelopes verifies without its bodies. A
// session is a ref: a header, a base, a head, and a log of the entries
// it appended.
//
// Every write follows one order. The objects go first, idempotently,
// since their bytes are their names. Then one journal record is
// appended and fsynced: that is the commit point, and nothing is
// visible before it, in memory or on disk. Then the log line and the
// head, which are indexes; Open replays the journal against them, in
// journal order, so a crash after the commit point leaves no
// acknowledged append missing and a crash before it leaves nothing
// behind. Every rename and creation is followed by an fsync of its
// directory, so a durable journal record never points at a file the
// power loss took.
//
// Several processes may share a store on one machine. Each session is
// held by one process at a time through a lock the kernel drops when
// the process exits (flock on unix), which RFC 0002 permits: a store
// may take writers in turn provided it never re-parents an accepted
// append. The journal is shared: a record is one line written in one
// call with O_APPEND, so records from different processes do not
// interleave, and the journal is never truncated by a reader; a record
// a crash cut short is skipped and the records after it still count.
// Recovery is per session, done by the process that holds the
// session's lock when it opens it, so no process rewrites the indexes
// of a session another process has open. The sweep needs no lock on
// writers: it keeps everything the journal names and every object
// younger than a grace period, as git's collector spares a young loose
// object, so an object written ahead of its record is safe. A store on
// a network filesystem is not supported: O_APPEND is not atomic across
// NFS clients, so records could interleave.
package cas

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
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
// holds the session, or a sweep holds the store.
var ErrSessionLocked = agentsession.ErrSessionLocked

// ErrModified is returned when a session the store handed out was
// changed behind its back — an entry appended on it directly, or its
// leaf moved to an entry the store never committed — so the store's
// record and the session disagree and nothing can be built on it.
var ErrModified = errors.New("cas: session was modified outside the store")

// Store is a content-addressed store rooted at a directory.
type Store struct {
	root string
	mu   sync.Mutex
	open map[string]*handle
	// owner maps each committed own entry to the session whose log holds
	// it; prefix holds the entries some session's base path needs.
	// Together they are what the store holds: an object written ahead
	// of its journal record is in neither.
	owner  map[string]string
	prefix map[string]bool
	// faulty records sessions the index could not read, with why, so
	// one fault is reported at that session's Open and hides no other.
	faulty map[string]error
}

type handle struct {
	session *agentsession.Session
	dir     string
	lock    *dirLock
	mark    string
	head    string // the head as the HEAD file has it
	count   int    // entries the store has committed into the session
}

// Open opens or creates the store at root, and replays the journal
// against the logs and heads so a crash between the commit point and
// the indexes leaves no acknowledged append missing.
func Open(root string) (*Store, error) {
	for _, d := range []string{filepath.Join(root, "objects", "contents"), filepath.Join(root, "objects", "entries"), filepath.Join(root, "sessions"), filepath.Join(root, "locks")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cas: %w", err)
		}
	}
	s := &Store{root: root, open: map[string]*handle{}, owner: map[string]string{}, prefix: map[string]bool{}, faulty: map[string]error{}}
	if err := s.index(); err != nil {
		return nil, err
	}
	return s, nil
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
// delete or a failed create removed from under it.
func (s *Store) lockSession(id string) (*dirLock, error) {
	return lockFile(filepath.Join(s.root, "locks", id))
}

func objectPath(dir, hash string) (string, error) {
	if !agentsession.ValidHash(hash) {
		return "", fmt.Errorf("%w: hash %q", ErrBadName, hash)
	}
	hex := strings.TrimPrefix(hash, agentsession.HashPrefix)
	return filepath.Join(dir, hex[:2], hex[2:]), nil
}

func (s *Store) contentPath(hash string) (string, error) {
	return objectPath(filepath.Join(s.root, "objects", "contents"), hash)
}

func (s *Store) entryPath(hash string) (string, error) {
	return objectPath(filepath.Join(s.root, "objects", "entries"), hash)
}

// --- durable file writes ---

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

// writeObject stores data under path, idempotently: an object already
// present is left as it is, since its bytes are its name. The write
// goes to a temporary file, is fsynced, renamed into place, and the
// directory is fsynced, so a reader never sees a partial object and a
// durable journal record never names an object that did not survive.
func writeObject(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		// Freshen it, as git does a loose object it finds it already has:
		// the sweep spares a young object, and this write is what makes
		// the object needed again, perhaps before its record lands.
		now := time.Now()
		if err := os.Chtimes(path, now, now); err != nil {
			return err
		}
		return nil
	}
	if _, err := os.Stat(filepath.Dir(path)); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(filepath.Dir(path))); err != nil {
			return err
		}
	}
	if err := writeAtomic(path, data); err != nil {
		if _, serr := os.Stat(path); serr == nil {
			return nil // another writer stored the same object
		}
		return err
	}
	return nil
}

// writeAtomic replaces the file at path with data through a temporary
// file, an fsync, a rename and an fsync of the directory.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	_, werr := tmp.Write(data)
	if serr := tmp.Sync(); werr == nil {
		werr = serr
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
	return syncDir(filepath.Dir(path))
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

// storeEntry writes an entry's two objects.
func (s *Store) storeEntry(e agentsession.Entry) error {
	env, body, err := split(e)
	if err != nil {
		return err
	}
	cp, err := s.contentPath(e.Base().ContentHash())
	if err != nil {
		return err
	}
	ep, err := s.entryPath(e.Base().ID)
	if err != nil {
		return err
	}
	if err := writeObject(cp, body); err != nil {
		return fmt.Errorf("cas: store content: %w", err)
	}
	if err := writeObject(ep, env); err != nil {
		return fmt.Errorf("cas: store entry: %w", err)
	}
	return nil
}

// freshenPath touches the objects on the path to id, so a fork's prefix
// is young to the sweep until the fork's header lands.
func (s *Store) freshenPath(id string) error {
	now := time.Now()
	for id != "" {
		ep, err := s.entryPath(id)
		if err != nil {
			return err
		}
		if err := os.Chtimes(ep, now, now); err != nil {
			return err
		}
		e, err := s.envelope(id)
		if err != nil {
			return err
		}
		var c string
		if json.Unmarshal(e["content"], &c) == nil {
			if cp, err := s.contentPath(c); err == nil {
				if err := os.Chtimes(cp, now, now); err != nil {
					return err
				}
			}
		}
		var parent *string
		if err := json.Unmarshal(e["parent"], &parent); err != nil || parent == nil {
			return err
		}
		id = *parent
	}
	return nil
}

// envelope reads an entry's envelope object.
func (s *Store) envelope(id string) (map[string]json.RawMessage, error) {
	ep, err := s.entryPath(id)
	if err != nil {
		return nil, err
	}
	env, err := os.ReadFile(ep)
	if err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	var e map[string]json.RawMessage
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	return e, nil
}

// loadLine reads an entry's two objects and rebuilds its line, checked
// as written.
func (s *Store) loadLine(id string) ([]byte, error) {
	ep, err := s.entryPath(id)
	if err != nil {
		return nil, err
	}
	env, err := os.ReadFile(ep)
	if err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	var e struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	cp, err := s.contentPath(e.Content)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(cp)
	if err != nil {
		return nil, fmt.Errorf("cas: content %s of entry %s: %w", e.Content, id, err)
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
	_, own := s.owner[id]
	return own || s.prefix[id]
}

// --- the journal ---

// journalRecord is one line of the journal: one commit.
type journalRecord struct {
	Op      string `json:"op"` // create, append, head, delete
	Session string `json:"session"`
	Entry   string `json:"entry,omitempty"`
	Head    string `json:"head,omitempty"`
	Seq     int    `json:"seq,omitempty"`
}

// commit appends the journal record and fsyncs it: the commit point.
func (s *Store) commit(rec journalRecord) error {
	path := filepath.Join(s.root, "journal")
	_, existed := os.Stat(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("cas: journal: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("cas: journal: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if existed != nil {
		return syncDir(s.root)
	}
	return nil
}

// sessionState is what the journal says about one session.
type sessionState struct {
	entries []string // own entries in journal order, each once
	seen    map[string]bool
	head    string
	hasHead bool
	deleted bool // the last thing the journal says is that it was deleted
}

// replay reads the journal and returns each session's state as the
// records say, in order, with a create or delete record clearing what
// came before it. A line that does not parse — a record a crashed
// process cut short — is skipped, and if another process's record
// landed on the same line after the torn bytes, that record is found
// and counted. Nothing is ever truncated: the journal is shared, and a
// record another process fsynced is not this process's to remove.
func (s *Store) replay() (map[string]*sessionState, error) {
	f, err := os.Open(filepath.Join(s.root, "journal"))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]*sessionState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	states := map[string]*sessionState{}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cas: journal: %w", err)
		}
		if len(line) == 0 {
			break
		}
		var rec journalRecord
		if json.Unmarshal(line, &rec) != nil {
			// Torn bytes; a whole record may follow them on this line.
			if k := bytes.LastIndex(line, []byte(`{"op":"`)); k > 0 {
				if json.Unmarshal(line[k:], &rec) != nil {
					continue
				}
			} else {
				continue
			}
		}
		st := states[rec.Session]
		if st == nil {
			st = &sessionState{seen: map[string]bool{}}
			states[rec.Session] = st
		}
		switch rec.Op {
		case "delete":
			states[rec.Session] = &sessionState{seen: map[string]bool{}, deleted: true}
		case "create":
			// The boundary a session starts from: whatever the journal
			// said about this ID before belongs to a session that is gone.
			states[rec.Session] = &sessionState{seen: map[string]bool{}}
		case "append":
			if rec.Entry != "" && !st.seen[rec.Entry] {
				st.entries = append(st.entries, rec.Entry)
				st.seen[rec.Entry] = true
			}
			if rec.Head != "" {
				st.head, st.hasHead = rec.Head, true
			}
		case "head":
			st.head, st.hasHead = rec.Head, true
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return states, nil
}

// recoverSession makes one session's indexes say what the journal says,
// under that session's lock: the journal is what a crash cannot take,
// the log and the head are what it can leave behind. A session the
// journal says was deleted has its directory removed. It reports
// whether the session exists.
func (s *Store) recoverSession(id, dir string) (bool, error) {
	states, err := s.replay()
	if err != nil {
		return false, err
	}
	st := states[id]
	if st == nil {
		_, err := os.Stat(filepath.Join(dir, "header"))
		return err == nil, nil
	}
	if st.deleted {
		if err := os.RemoveAll(dir); err != nil {
			return false, err
		}
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
		return false, nil // never finished creating
	}
	have, err := readLog(dir)
	if err != nil {
		return false, err
	}
	if !equalStrings(have, st.entries) {
		var buf bytes.Buffer
		for _, h := range st.entries {
			buf.WriteString(h + "\n")
		}
		if err := writeAtomic(filepath.Join(dir, "log"), buf.Bytes()); err != nil {
			return false, err
		}
	}
	if st.hasHead {
		cur, _ := readHead(dir)
		if cur != st.head {
			if err := writeHead(dir, st.head); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// index builds what the store holds from the journal, the logs and the
// bases. The journal counts as well as the logs, since a log may lag an
// acknowledged append until its session is next opened.
func (s *Store) index() error {
	states, err := s.replay()
	if err != nil {
		return err
	}
	for id, st := range states {
		if st.deleted {
			continue
		}
		for _, e := range st.entries {
			s.owner[e] = id
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
		dir := filepath.Join(s.root, "sessions", d.Name())
		hashes, err := readLog(dir)
		if err != nil {
			s.faulty[d.Name()] = err
			continue
		}
		for _, h := range hashes {
			s.owner[h] = d.Name()
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

func readLog(dir string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cas: log: %w", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out, nil
}

func appendLog(dir string, hashes ...string) error {
	f, err := os.OpenFile(filepath.Join(dir, "log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cas: log: %w", err)
	}
	defer f.Close()
	for _, h := range hashes {
		if _, err := f.WriteString(h + "\n"); err != nil {
			return fmt.Errorf("cas: log: %w", err)
		}
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

func writeHead(dir, head string) error {
	return writeAtomic(filepath.Join(dir, "HEAD"), []byte(head+"\n"))
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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createLocked(h, MarkRecord)
}

func (s *Store) createLocked(h agentsession.Header, mark string) (*agentsession.Session, error) {
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
		if owner, ok := s.owner[h.Base]; ok {
			if h.ParentSession == "" {
				h.ParentSession = owner
			}
			if dir, err := s.sessionDir(owner); err == nil {
				if oh, err := readHeader(dir); err == nil {
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
	if exists, err := s.recoverSession(h.ID, dir); err != nil {
		return fail(err)
	} else if exists {
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
	// No sweep from here until the header names the prefix. The prefix
	// is freshened too, so it is young once the lock is dropped.
	guard, err := lockShared(filepath.Join(s.root, "sweep.lock"))
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
	if err := s.commit(journalRecord{Op: "create", Session: h.ID}); err != nil {
		return fail(err)
	}
	if err := writeAtomic(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
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
	s.open[h.ID] = &handle{session: sess, dir: dir, lock: lk, mark: mark, head: h.Base, count: sess.Len()}
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
	exists, err := s.recoverSession(id, dir)
	if err != nil {
		return fail(err)
	}
	if !exists {
		return fail(fmt.Errorf("%w: %s", agentsession.ErrNoSession, id))
	}
	hdr, err := readHeader(dir)
	if err != nil {
		return fail(err)
	}
	var prefix [][]byte
	if hdr.Base != "" {
		if prefix, err = s.pathLines(hdr.Base); err != nil {
			return fail(err)
		}
	}
	hashes, err := readLog(dir)
	if err != nil {
		return fail(err)
	}
	own := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		line, err := s.loadLine(h)
		if err != nil {
			return fail(err)
		}
		own = append(own, line)
	}
	head, err := readHead(dir)
	if err != nil {
		return fail(err)
	}
	sess, err := s.assemble(hdr, prefix, own, head)
	if err != nil {
		return fail(err)
	}
	for _, h := range hashes {
		s.owner[h] = id // another process may have appended since the index was built
	}
	h := &handle{session: sess, dir: dir, lock: lk, mark: readMark(dir), head: head, count: sess.Len()}
	s.open[id] = h
	return h, nil
}

// Append implements agentsession.Store; see Write for what it reports.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	r, err := s.Write(ctx, sessionID, e)
	return r.ID, err
}

// Write appends an entry and reports what happened, as the format asks
// a store to: whether the entry continued the head, branched, was
// already held, or as a leaf label moved the head or could not. The
// order is the store's one order: the entry is prepared, its objects
// are written, the journal record is committed, the log and the head
// follow, and only then is the entry visible in the session. A session
// held as a mirror is refused, as is a leaf label carrying `synthetic`.
// A head moved in memory through Session.Branch since the last commit
// is journaled first, so a head move never bypasses the journal.
func (s *Store) Write(ctx context.Context, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
	if err := ctx.Err(); err != nil {
		return agentsession.Result{}, err
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
	if l, ok := e.(*agentsession.LabelEntry); ok {
		if _, synthetic := l.Unknown["synthetic"]; synthetic {
			return agentsession.Result{}, ErrSynthetic
		}
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
	// From the object write through the commit no sweep may run: the
	// object may be one the sweep would otherwise find old and unnamed.
	guard, err := lockShared(filepath.Join(s.root, "sweep.lock"))
	if err != nil {
		return agentsession.Result{}, err
	}
	if err := s.storeEntry(e); err != nil {
		guard.release()
		return agentsession.Result{}, err
	}
	head := ""
	switch r.Outcome {
	case agentsession.Continued:
		head = r.ID
	case agentsession.LeafMoved:
		head = e.(*agentsession.LabelEntry).Target
	}
	err = s.commit(journalRecord{Op: "append", Session: sessionID, Entry: r.ID, Head: head, Seq: h.session.Len() + 1})
	guard.release()
	if err != nil {
		return agentsession.Result{}, err
	}
	// The append is durable from here. The log and the head are
	// indexes the next open rebuilds from the journal, so a failure to
	// write them is not a failed append and is not reported as one.
	_ = appendLog(h.dir, r.ID)
	if head != "" {
		_ = writeHead(h.dir, head)
		h.head = head
	}
	s.owner[r.ID] = sessionID
	if h.session.Leaf() != leafAtPrepare {
		// A reader moved the leaf between Prepare and Commit, through
		// Session.Branch, which does not take the store's lock. The
		// journal has spoken; the store is authoritative, and the move
		// is undone rather than recorded as something it was not.
		if err := h.session.Branch(leafAtPrepare); err != nil && leafAtPrepare != "" {
			return agentsession.Result{}, err
		}
	}
	got, err := h.session.Commit(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	h.count++
	return got, nil
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
		if owner, ok := s.owner[leaf]; (!ok || owner != sessionID) && leaf != h.session.Header().Base {
			return fmt.Errorf("%w: head %s is not in the log", ErrModified, leaf)
		}
	} else if h.session.Header().Base != "" {
		return fmt.Errorf("%w: a session with a base has a head", agentsession.ErrNoEntry)
	}
	if err := s.commit(journalRecord{Op: "head", Session: sessionID, Head: leaf, Seq: h.session.Len()}); err != nil {
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
// journal record is committed before the head moves anywhere.
func (s *Store) SetHead(ctx context.Context, sessionID, expected, to string) error {
	if err := ctx.Err(); err != nil {
		return err
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
	if err := s.commit(journalRecord{Op: "head", Session: sessionID, Head: to, Seq: h.session.Len()}); err != nil {
		return err
	}
	_ = writeHead(h.dir, to) // an index; the journal has it
	h.head = to
	return h.session.Branch(to)
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

// DeclareRecord makes this store the record for a session it holds as
// a mirror: what a mirror does when the record deleted the session
// without handing it over, or an importer does for a file it knows to
// be the only copy.
func (s *Store) DeclareRecord(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(h.dir, "record"), []byte(MarkRecord+"\n")); err != nil {
		return err
	}
	h.mark = MarkRecord
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

// List implements agentsession.Store.
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
		for _, d := range dirs {
			if !d.IsDir() || !validSessionID(d.Name()) {
				continue
			}
			sum, err := s.summarize(d.Name(), f.WithNames || f.Current)
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

func (s *Store) summarize(id string, withMeta bool) (agentsession.Summary, error) {
	dir := filepath.Join(s.root, "sessions", id)
	h, err := readHeader(dir)
	if err != nil {
		return agentsession.Summary{}, fmt.Errorf("cas: %s: %w", id, err)
	}
	sum := agentsession.Summary{Header: h, Path: dir}
	if info, err := os.Stat(filepath.Join(dir, "log")); err == nil {
		sum.Size = info.Size()
		sum.Modified = info.ModTime()
	}
	if withMeta {
		hashes, err := readLog(dir)
		if err != nil {
			return sum, err
		}
		for _, hash := range hashes {
			line, err := s.loadLine(hash)
			if err != nil {
				return sum, err
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
// prefix needs is swept by Sweep.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
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
	if err := s.commit(journalRecord{Op: "delete", Session: id}); err != nil {
		return err
	}
	for h, owner := range s.owner {
		if owner == id {
			delete(s.owner, h)
		}
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return syncDir(filepath.Join(s.root, "sessions"))
}

// Release closes a session this process holds, freeing its lock.
func (s *Store) Release(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.open[id]
	if !ok {
		return nil
	}
	delete(s.open, id)
	return h.lock.release()
}

// Close releases every session this store holds.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for id, h := range s.open {
		if err := h.lock.release(); err != nil && first == nil {
			first = err
		}
		delete(s.open, id)
	}
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
// and join no log; own entries join the log in file order, each
// committed to the journal before the log. A session the store already
// holds is refused.
func (s *Store) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
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
	var own []agentsession.Entry
	for i, e := range entries {
		if l, ok := e.(*agentsession.LabelEntry); ok {
			if _, synthetic := l.Unknown["synthetic"]; synthetic {
				if i == len(entries)-1 && l.Label != nil && *l.Label == agentsession.LeafLabel {
					continue // the projection's marker, discarded
				}
				return nil, fmt.Errorf("%w: at line %d", ErrSynthetic, i+2)
			}
		}
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
		if owner, ok := s.owner[h.Base]; ok {
			if odir, err := s.sessionDir(owner); err == nil {
				if oh, err := readHeader(odir); err == nil && mediaOf(oh) != mediaOf(h) {
					return nil, errors.New("cas: import: media differs from the session holding the base")
				}
			}
		}
	}
	head := sess.Leaf()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cas: %w", err)
	}
	lk, err := s.lockSession(h.ID)
	if err != nil {
		return nil, err
	}
	if exists, err := s.recoverSession(h.ID, dir); err != nil {
		lk.release()
		return nil, err
	} else if exists {
		lk.release()
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		lk.release()
		return nil, fmt.Errorf("cas: %w", err)
	}
	// The prefix objects are written below, which makes them young to
	// the sweep; a fork made by Create freshens them instead.
	// The create record is the boundary; a failure after it commits a
	// delete, so the ID is free again and nothing half-imported counts.
	if err := s.commit(journalRecord{Op: "create", Session: h.ID}); err != nil {
		lk.release()
		return nil, err
	}
	fail := func(err error) (*agentsession.Session, error) {
		s.commit(journalRecord{Op: "delete", Session: h.ID})
		lk.release()
		os.RemoveAll(dir)
		return nil, err
	}
	guard, err := lockShared(filepath.Join(s.root, "sweep.lock"))
	if err != nil {
		return fail(err)
	}
	defer guard.release()
	for _, e := range entries {
		if l, ok := e.(*agentsession.LabelEntry); ok {
			if _, synthetic := l.Unknown["synthetic"]; synthetic {
				continue
			}
		}
		if err := s.storeEntry(e); err != nil {
			return fail(err)
		}
	}
	mark := MarkMirror
	if asRecord {
		mark = MarkRecord
	}
	if err := writeAtomic(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
		return fail(err)
	}
	for i, e := range own {
		if err := s.commit(journalRecord{Op: "append", Session: h.ID, Entry: e.Base().ID, Seq: i + 1}); err != nil {
			return fail(err)
		}
	}
	if head != "" {
		if err := s.commit(journalRecord{Op: "head", Session: h.ID, Head: head, Seq: len(own)}); err != nil {
			return fail(err)
		}
	}
	hashes := make([]string, 0, len(own))
	for _, e := range own {
		hashes = append(hashes, e.Base().ID)
		s.owner[e.Base().ID] = h.ID
	}
	if len(hashes) > 0 {
		if err := appendLog(dir, hashes...); err != nil {
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
	// What the store returns is what it holds: the session rebuilt from
	// the objects and the log, marker gone, head from HEAD.
	lk.release()
	hd, err := s.openLocked(h.ID)
	if err != nil {
		return nil, err
	}
	return hd.session, nil
}

// Sweep removes objects nothing needs, following references down: an
// envelope is kept while the journal, a log or a prefix names it, a
// content while a kept envelope names it. It works from the journal, so
// an append acknowledged as durable whose log line never reached disk
// is kept, and it keeps every object younger than grace, as git's
// collector spares a young loose object, so an object written ahead of
// its journal record is safe. It therefore needs no lock on writers and
// runs alongside live sessions; a store-wide lock keeps two sweeps
// apart. A grace of zero is safe only when no writer is active
// anywhere, as git says of pruning with an expiry of now: with one, it
// sweeps objects written ahead of their records and temporary files
// mid-write. An hour is a reasonable grace for a live store. Media
// blobs are contents. It returns how many objects went.
func (s *Store) Sweep(ctx context.Context, grace time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, err := lockFile(filepath.Join(s.root, "sweep.lock"))
	if err != nil {
		return 0, err
	}
	defer lk.release()
	states, err := s.replay()
	if err != nil {
		return 0, err
	}
	keepEntry := map[string]bool{}
	for _, st := range states {
		if st.deleted {
			continue
		}
		for _, e := range st.entries {
			keepEntry[e] = true
		}
		if st.head != "" {
			keepEntry[st.head] = true
		}
	}
	s.owner = map[string]string{}
	s.prefix = map[string]bool{}
	if err := s.index(); err != nil {
		return 0, err
	}
	for id := range s.owner {
		keepEntry[id] = true
	}
	for id := range s.prefix {
		keepEntry[id] = true
	}
	// A fork being created has its prefix freshened before its header
	// lands, so those objects are young and spared.
	keepContent := map[string]bool{}
	for id := range keepEntry {
		e, err := s.envelope(id)
		if err != nil {
			continue
		}
		var c string
		if json.Unmarshal(e["content"], &c) == nil {
			keepContent[c] = true
		}
	}
	swept := 0
	young := time.Now().Add(-grace)
	for _, space := range []struct {
		dir  string
		keep map[string]bool
	}{{filepath.Join(s.root, "objects", "entries"), keepEntry}, {filepath.Join(s.root, "objects", "contents"), keepContent}} {
		err := filepath.WalkDir(space.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			if info.ModTime().After(young) {
				return nil // young: perhaps written ahead of its record
			}
			if strings.HasPrefix(d.Name(), ".tmp-") {
				return os.Remove(path)
			}
			rel, _ := filepath.Rel(space.dir, path)
			hash := agentsession.HashPrefix + strings.ReplaceAll(rel, string(filepath.Separator), "")
			if !space.keep[hash] {
				// Writers hold the lock shared through their commit, so
				// none is between an object write and its record now; the
				// second stat is for a writer on a platform without flock.
				if again, err := os.Stat(path); err != nil || again.ModTime().After(young) {
					return nil
				}
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
				swept++
			}
			return nil
		})
		if err != nil {
			return swept, err
		}
	}
	return swept, nil
}

var _ agentsession.Store = (*Store)(nil)
