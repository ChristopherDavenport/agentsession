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
//	  journal                             one line per append: the commit point
//
// An entry is stored as two objects, its body under the content hash
// and its envelope under the id, so a body shared by many entries is
// held once and a chain of envelopes verifies without its bodies. A
// session is a ref: a header, a base, a head, and a log of the entries
// it appended. Append writes the objects first, idempotently, then one
// journal record, which is the commit point and is fsynced, then the
// log line and the head, which are indexes the journal rebuilds on open
// if a crash left them behind.
//
// Two writers in one process append through one Store, which
// serialises them. A second process is refused the session by an
// advisory lock, which RFC 0002 permits: a store may take writers in
// turn provided it never re-parents an accepted append.
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
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/procs"
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

// ErrSessionLocked is [agentsession.ErrSessionLocked].
var ErrSessionLocked = agentsession.ErrSessionLocked

// Store is a content-addressed store rooted at a directory.
type Store struct {
	root string
	mu   sync.Mutex
	open map[string]*handle
}

type handle struct {
	session *agentsession.Session
	dir     string
	mark    string
}

// Open opens or creates the store at root, and replays the journal's
// tail against the logs and heads so a crash between the commit point
// and the indexes leaves no acknowledged append missing.
func Open(root string) (*Store, error) {
	for _, d := range []string{filepath.Join(root, "objects", "contents"), filepath.Join(root, "objects", "entries"), filepath.Join(root, "sessions")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cas: %w", err)
		}
	}
	s := &Store{root: root, open: map[string]*handle{}}
	if err := s.recover(); err != nil {
		return nil, err
	}
	return s, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

func (s *Store) sessionDir(id string) string { return filepath.Join(s.root, "sessions", id) }
func objectPath(dir, hash string) string {
	hex := strings.TrimPrefix(hash, agentsession.HashPrefix)
	return filepath.Join(dir, hex[:2], hex[2:])
}
func (s *Store) contentPath(hash string) string {
	return objectPath(filepath.Join(s.root, "objects", "contents"), hash)
}
func (s *Store) entryPath(hash string) string {
	return objectPath(filepath.Join(s.root, "objects", "entries"), hash)
}

// writeObject stores data under path, idempotently: an object already
// present is left as it is, since its bytes are its name. The write
// goes to a temporary file and is renamed into place, so a reader never
// sees a partial object.
func writeObject(path string, data []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
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
		if _, serr := os.Stat(path); serr == nil {
			return nil // another writer stored the same object
		}
		return err
	}
	return nil
}

// writeAtomic replaces the file at path with data, through a rename.
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
	return os.Rename(tmp.Name(), path)
}

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
	if err := writeObject(s.contentPath(e.Base().ContentHash()), body); err != nil {
		return fmt.Errorf("cas: store content: %w", err)
	}
	if err := writeObject(s.entryPath(e.Base().ID), env); err != nil {
		return fmt.Errorf("cas: store entry: %w", err)
	}
	return nil
}

// loadLine reads an entry's two objects and rebuilds its line.
func (s *Store) loadLine(id string) ([]byte, error) {
	env, err := os.ReadFile(s.entryPath(id))
	if err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	var e struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(env, &e); err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	body, err := os.ReadFile(s.contentPath(e.Content))
	if err != nil {
		return nil, fmt.Errorf("cas: content %s of entry %s: %w", e.Content, id, err)
	}
	return join(id, env, body)
}

// parentOf reads an entry's parent from its envelope object alone, the
// walk a chain of envelopes allows without bodies.
func (s *Store) parentOf(id string) (string, bool, error) {
	env, err := os.ReadFile(s.entryPath(id))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	var e struct {
		Parent *string `json:"parent"`
	}
	if err := json.Unmarshal(env, &e); err != nil {
		return "", false, err
	}
	if e.Parent == nil {
		return "", true, nil
	}
	return *e.Parent, true, nil
}

// holds reports whether the store has the entry's envelope.
func (s *Store) holds(id string) bool {
	_, err := os.Stat(s.entryPath(id))
	return err == nil
}

// journalRecord is one line of the journal: one append's commit.
type journalRecord struct {
	Session string `json:"session"`
	Entry   string `json:"entry"`
	Head    string `json:"head"`
	Seq     int    `json:"seq"`
}

// commit appends the journal record and fsyncs it: the commit point.
func (s *Store) commit(rec journalRecord) error {
	f, err := os.OpenFile(filepath.Join(s.root, "journal"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	return f.Sync()
}

// recover replays the journal against the logs and heads: every record
// whose session still exists has its entry in the session's log and,
// when the record moved the head, the head at that entry unless a later
// record moved it again. Indexes are what a crash can leave behind; the
// journal is what it cannot.
func (s *Store) recover() error {
	f, err := os.Open(filepath.Join(s.root, "journal"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	type state struct {
		log  map[string]bool
		seq  int
		head string
		has  bool
	}
	seen := map[string]*state{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<26)
	for sc.Scan() {
		var rec journalRecord
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			break // a torn last line: nothing after it was acknowledged
		}
		st, ok := seen[rec.Session]
		if !ok {
			st = &state{log: map[string]bool{}}
			seen[rec.Session] = st
		}
		st.log[rec.Entry] = true
		st.seq = rec.Seq
		if rec.Head != "" {
			st.head, st.has = rec.Head, true
		}
	}
	for id, st := range seen {
		dir := s.sessionDir(id)
		if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
			continue // deleted since
		}
		have, err := readLog(dir)
		if err != nil {
			return err
		}
		held := map[string]bool{}
		for _, h := range have {
			held[h] = true
		}
		var missing []string
		for h := range st.log {
			if !held[h] {
				missing = append(missing, h)
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			if err := appendLog(dir, missing...); err != nil {
				return err
			}
		}
		if st.has {
			cur, _ := readHead(dir)
			if cur == "" && st.head != "" {
				if err := writeHead(dir, st.head); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

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

func readMark(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "record"))
	if err != nil {
		return MarkRecord
	}
	return strings.TrimSpace(string(data))
}

// Create implements agentsession.Store. A header with a Base makes a
// fork: the base must be an entry the store holds and not a leaf label,
// the session's media must match a held session whose own entries
// include the base, and the head starts at the base. The session is the
// record here.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createLocked(h, MarkRecord)
}

func (s *Store) createLocked(h agentsession.Header, mark string) (*agentsession.Session, error) {
	var sess *agentsession.Session
	if h.Base != "" {
		if !s.holds(h.Base) {
			return nil, fmt.Errorf("%w: base %s is not in this store", agentsession.ErrNoEntry, h.Base)
		}
		lines, err := s.pathLines(h.Base)
		if err != nil {
			return nil, err
		}
		if h.ParentSession == "" {
			return nil, errors.New("cas: a fork's header names its parent_session")
		}
		origin, err := s.openLocked(h.ParentSession)
		if err == nil && h.Media != "" && h.Media != origin.session.Header().Media {
			return nil, errors.New("cas: a fork's media must equal its origin's")
		}
		if err == nil && h.Media == "" {
			h.Media = origin.session.Header().Media
		}
		if err == nil && h.Payload == "" {
			h.Payload = origin.session.Header().Payload
		}
		tmp := agentsession.New(h)
		h = tmp.Header()
		sess, err = s.assemble(h, lines, nil, h.Base)
		if err != nil {
			return nil, err
		}
	} else {
		sess = agentsession.New(h)
		h = sess.Header()
	}
	dir := s.sessionDir(h.ID)
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); err == nil {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cas: %w", err)
	}
	if err := acquireLock(dir); err != nil {
		return nil, err
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	hdr, err = jcs.Transform(hdr)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "header"), append(hdr, '\n')); err != nil {
		releaseLock(dir)
		return nil, fmt.Errorf("cas: header: %w", err)
	}
	if err := writeAtomic(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
		releaseLock(dir)
		return nil, err
	}
	if h.Base != "" {
		if err := writeHead(dir, h.Base); err != nil {
			releaseLock(dir)
			return nil, err
		}
	}
	s.open[h.ID] = &handle{session: sess, dir: dir, mark: mark}
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
		parent, ok, err := s.parentOf(id)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: %s", agentsession.ErrNoEntry, id)
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
	dir := s.sessionDir(id)
	hdrData, err := os.ReadFile(filepath.Join(dir, "header"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if err != nil {
		return nil, fmt.Errorf("cas: %w", err)
	}
	var hdr agentsession.Header
	if err := json.Unmarshal(bytes.TrimSpace(hdrData), &hdr); err != nil {
		return nil, fmt.Errorf("cas: session %s: header: %w", id, err)
	}
	if err := acquireLock(dir); err != nil {
		return nil, err
	}
	var prefix [][]byte
	if hdr.Base != "" {
		if prefix, err = s.pathLines(hdr.Base); err != nil {
			releaseLock(dir)
			return nil, err
		}
	}
	hashes, err := readLog(dir)
	if err != nil {
		releaseLock(dir)
		return nil, err
	}
	own := make([][]byte, 0, len(hashes))
	for _, h := range hashes {
		line, err := s.loadLine(h)
		if err != nil {
			releaseLock(dir)
			return nil, err
		}
		own = append(own, line)
	}
	head, err := readHead(dir)
	if err != nil {
		releaseLock(dir)
		return nil, err
	}
	sess, err := s.assemble(hdr, prefix, own, head)
	if err != nil {
		releaseLock(dir)
		return nil, err
	}
	h := &handle{session: sess, dir: dir, mark: readMark(dir)}
	s.open[id] = h
	return h, nil
}

// Append implements agentsession.Store: the entry's objects are stored,
// the journal record is committed, then the log and the head follow.
// A session held as a mirror is refused. An entry the session already
// holds is a no-op that returns its id and moves nothing.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return "", err
	}
	if h.mark != MarkRecord {
		return "", fmt.Errorf("%w: %s", ErrMirror, sessionID)
	}
	before := h.session.Len()
	oldHead := h.session.Leaf()
	id, err := h.session.Append(e)
	if err != nil {
		return "", err
	}
	if h.session.Len() == before {
		return id, nil // already held
	}
	if err := s.storeEntry(e); err != nil {
		return "", err
	}
	head := ""
	if h.session.Leaf() != oldHead {
		head = h.session.Leaf()
	}
	if err := s.commit(journalRecord{Session: sessionID, Entry: id, Head: head, Seq: h.session.Len()}); err != nil {
		return "", err
	}
	if err := appendLog(h.dir, id); err != nil {
		return "", err
	}
	if head != "" {
		if err := writeHead(h.dir, head); err != nil {
			return "", err
		}
	}
	return id, nil
}

// SetHead moves a session's head from expected to the entry to, or
// returns ErrHeadMoved when the head is not expected. "" is a valid
// expected value for a session with no head. The target must be the
// base or an own entry and not a leaf label; a mirror is refused.
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
	if h.session.Leaf() != expected {
		return fmt.Errorf("%w: head is %s", ErrHeadMoved, h.session.Leaf())
	}
	if err := s.mayRestOn(h.session, to); err != nil {
		return err
	}
	if err := h.session.Branch(to); err != nil {
		return err
	}
	if err := s.commit(journalRecord{Session: sessionID, Entry: "", Head: to, Seq: h.session.Len()}); err != nil {
		return err
	}
	return writeHead(h.dir, to)
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
			if !d.IsDir() {
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
	dir := s.sessionDir(id)
	data, err := os.ReadFile(filepath.Join(dir, "header"))
	if err != nil {
		return agentsession.Summary{}, fmt.Errorf("cas: %s: %w", id, err)
	}
	var h agentsession.Header
	if err := json.Unmarshal(bytes.TrimSpace(data), &h); err != nil {
		return agentsession.Summary{}, fmt.Errorf("cas: %s: header: %w", id, err)
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

// Delete implements agentsession.Store: it removes the session's ref,
// header and log. Objects stay; what no log and no prefix needs is
// swept by Sweep.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.sessionDir(id)
	if _, err := os.Stat(filepath.Join(dir, "header")); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if h, ok := s.open[id]; ok {
		delete(s.open, id)
		_ = h
	} else if err := acquireLock(dir); err != nil {
		return err
	}
	return os.RemoveAll(dir)
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
	return releaseLock(h.dir)
}

// Close releases every session this store holds.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for id, h := range s.open {
		if err := releaseLock(h.dir); err != nil && first == nil {
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
	return project(w, h.session)
}

// project is Project over a session in memory.
func project(w io.Writer, sess *agentsession.Session) error {
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, sess); err != nil {
		return err
	}
	// Would the resume rule over these lines name the head?
	check, err := agentsession.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		return err
	}
	head := sess.Leaf()
	if check.Leaf() != head && head != "" {
		last := sess.Entries()[sess.Len()-1]
		headEntry, _ := sess.Entry(head)
		marker := agentsession.NewLabelEntry(head, agentsession.LeafLabel)
		marker.Parent = last.Base().ID
		marker.Timestamp = headEntry.Base().Timestamp
		marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
		// Hash it as a line of the file without adding it to the session.
		scratch, err := agentsession.Read(bytes.NewReader(buf.Bytes()))
		if err != nil {
			return err
		}
		if _, err := scratch.Append(marker); err != nil {
			return err
		}
		buf.Reset()
		if err := agentsession.Write(&buf, scratch); err != nil {
			return err
		}
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// Import reads an RFC 0001 file into the store as a mirror of the
// session it holds, unless asRecord says this file is the only copy.
// Every line is verified by the reader, a redacted header is refused, a
// trailing synthetic marker names the head and is discarded, prefix
// entries become objects and join no log, and the session's own entries
// join its log in file order. A session the store already holds is
// refused.
func (s *Store) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sess, err := agentsession.Read(r)
	if err != nil {
		return nil, err
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
	if _, err := os.Stat(filepath.Join(s.sessionDir(h.ID), "header")); err == nil {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	head := sess.Leaf()
	// Store every entry's objects, prefix included; the prefix joins no
	// log. A trailing synthetic marker is the projection's and is
	// dropped.
	var own []string
	entries := sess.Entries()
	for i, e := range entries {
		if l, ok := e.(*agentsession.LabelEntry); ok && i == len(entries)-1 {
			if _, synthetic := l.Unknown["synthetic"]; synthetic && l.Label != nil && *l.Label == agentsession.LeafLabel {
				continue
			}
		}
		if err := s.storeEntry(e); err != nil {
			return nil, err
		}
		if !sess.Prefix(e.Base().ID) {
			own = append(own, e.Base().ID)
		}
	}
	mark := MarkMirror
	if asRecord {
		mark = MarkRecord
	}
	dir := s.sessionDir(h.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cas: %w", err)
	}
	hdr, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	hdr, _ = jcs.Transform(hdr)
	if err := writeAtomic(filepath.Join(dir, "header"), append(hdr, '\n')); err != nil {
		return nil, err
	}
	if err := writeAtomic(filepath.Join(dir, "record"), []byte(mark+"\n")); err != nil {
		return nil, err
	}
	if len(own) > 0 {
		if err := appendLog(dir, own...); err != nil {
			return nil, err
		}
	}
	for i, id := range own {
		if err := s.commit(journalRecord{Session: h.ID, Entry: id, Seq: i + 1}); err != nil {
			return nil, err
		}
	}
	if head != "" {
		if err := writeHead(dir, head); err != nil {
			return nil, err
		}
		if err := s.commit(journalRecord{Session: h.ID, Head: head, Seq: len(own)}); err != nil {
			return nil, err
		}
	}
	hd, err := s.openLocked(h.ID)
	if err != nil {
		return nil, err
	}
	return hd.session, nil
}

// Sweep removes objects no session's log or prefix needs, following
// references down: an envelope is kept while a log or a prefix names it,
// a content while a kept envelope names it. Media blobs are contents.
// It returns how many objects went.
func (s *Store) Sweep(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	keepEntry := map[string]bool{}
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return 0, err
	}
	for _, d := range dirs {
		dir := s.sessionDir(d.Name())
		hashes, err := readLog(dir)
		if err != nil {
			return 0, err
		}
		for _, h := range hashes {
			keepEntry[h] = true
		}
		data, err := os.ReadFile(filepath.Join(dir, "header"))
		if err != nil {
			continue
		}
		var h agentsession.Header
		if json.Unmarshal(bytes.TrimSpace(data), &h) == nil && h.Base != "" {
			for id := h.Base; id != ""; {
				keepEntry[id] = true
				parent, ok, err := s.parentOf(id)
				if err != nil || !ok {
					break
				}
				id = parent
			}
		}
	}
	keepContent := map[string]bool{}
	for id := range keepEntry {
		env, err := os.ReadFile(s.entryPath(id))
		if err != nil {
			continue
		}
		var e struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(env, &e) == nil {
			keepContent[e.Content] = true
		}
	}
	swept := 0
	for _, space := range []struct {
		dir  string
		keep map[string]bool
	}{{filepath.Join(s.root, "objects", "entries"), keepEntry}, {filepath.Join(s.root, "objects", "contents"), keepContent}} {
		err := filepath.WalkDir(space.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, _ := filepath.Rel(space.dir, path)
			hash := agentsession.HashPrefix + strings.ReplaceAll(rel, string(filepath.Separator), "")
			if !space.keep[hash] {
				if err := os.Remove(path); err != nil {
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

// --- locking, as the jsonl store does it ---

type lockInfo struct {
	PID   int       `json:"pid"`
	Host  string    `json:"host,omitempty"`
	Since time.Time `json:"since"`
}

func acquireLock(dir string) error {
	path := filepath.Join(dir, "lock")
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			host, _ := os.Hostname()
			werr := json.NewEncoder(f).Encode(lockInfo{PID: os.Getpid(), Host: host, Since: time.Now().UTC().Round(0)})
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return fmt.Errorf("cas: write lock: %w", werr)
			}
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("cas: lock: %w", err)
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			if errors.Is(rerr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%w: %s", ErrSessionLocked, path)
		}
		var holder lockInfo
		if json.Unmarshal(data, &holder) != nil {
			return fmt.Errorf("%w: %s (unreadable lock)", ErrSessionLocked, path)
		}
		host, _ := os.Hostname()
		if holder.Host == host && holder.PID > 0 && !procs.Alive(holder.PID) {
			os.Remove(path)
			continue
		}
		return fmt.Errorf("%w: %s held by pid %d on %s", ErrSessionLocked, path, holder.PID, holder.Host)
	}
	return fmt.Errorf("%w: %s", ErrSessionLocked, path)
}

func releaseLock(dir string) error {
	if err := os.Remove(filepath.Join(dir, "lock")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var _ agentsession.Store = (*Store)(nil)
