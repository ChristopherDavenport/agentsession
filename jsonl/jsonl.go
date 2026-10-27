// Package jsonl is the default session store: one append-only JSONL
// file per session under a root directory, laid out as
//
//	<root>/<project-key>/<created-at>_<session-id>.jsonl
//
// where the project key derives from the session's working directory.
// Each Append writes one line; a SyncPolicy decides when the file is
// fsynced. Opening a session whose last line was cut short by a crash
// drops that line so later appends produce a valid file.
//
// A session read back gives members as they were written, member order
// inside opaque JSON such as a tool's parameter schema included. That
// is a property of this store, not of the format: a cas store gives the
// same members in canonical order. See [agentsession.CanonicalRequest].
package jsonl

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
	"github.com/ChristopherDavenport/agentsession/internal/wake"
	"github.com/ChristopherDavenport/openresponses"
)

// SyncPolicy says when the store fsyncs a session file.
type SyncPolicy int

const (
	// SyncEveryAppend fsyncs after every line. It is the default.
	SyncEveryAppend SyncPolicy = iota
	// SyncOnResponse fsyncs after a response entry and a function call
	// output, which the format recommends as the minimum, after a
	// compaction or a branch summary, which are as expensive to lose,
	// and after any record entry whose type the header names in
	// records, which the format requires to be durable before the side
	// effect it precedes.
	SyncOnResponse
	// SyncNever leaves syncing to the operating system and to explicit
	// calls to Store.Sync, apart from a record entry whose type the
	// header names in records, which the format requires to be durable
	// before the side effect it precedes and which this policy too
	// fsyncs.
	SyncNever
)

// Option configures a Store.
type Option func(*Store)

// WithSync sets the sync policy.
func WithSync(p SyncPolicy) Option {
	return func(s *Store) { s.policy = p }
}

// WithReadOnly opens the store for reading: Open takes no lock on a
// session, so a session another process is writing can be read while
// it writes, and Create, Import, Append, Delete, Sync and BreakLock return
// [agentsession.ErrReadOnly]. Nothing is written, the store's root
// directory included, and a file whose last line was cut short is
// reported through Session.Truncated and left alone, since trimming
// it is a write. It is what show, verify and export want, and what a
// host wants when an operator asks about a session the agent holds.
//
// An open session is cached as it is in a writing store, so a session
// read while another process appends to it shows what it held when it
// was opened; call [Store.Release] and open it again to see the rest.
// [Store.Read], on any store, reads a session without a hold and
// caches nothing, so it reads what the store holds at each call.
func WithReadOnly() Option {
	return func(s *Store) { s.readOnly = true }
}

// WithStaleLockReport sets a function the store calls when Create or
// Open takes over the lock of a process on this host that no longer
// runs, with the dead holder's details. A session file left by a
// crash is a valid prefix and cannot say by itself whether the last
// writer exited cleanly; the stale lock is the only durable sign that
// it did not, and the takeover would otherwise consume it silently. A
// host wires its crash recovery to this. The takeover itself is not
// changed and the function must not block on the store.
func WithStaleLockReport(report func(LockInfo)) Option {
	return func(s *Store) { s.staleReport = report }
}

// WithRedactedOnly sets the store to hold only redacted entries, as
// RFC 0002 lets a store be set: it refuses, with
// [agentsession.ErrNotRedacted], a session whose header does not carry
// redacted, at Create or Import, and every entry that carries no
// redacted member, however it arrives: an Append, an Import, the prefix
// of a fork Create makes. A fork's own entries and a resumed redacted
// session's native entries are refused alike, since neither was
// redacted. Open with the option reads every session under the root and
// refuses, with ErrNotRedacted, a root that already holds one whose
// header lacks redacted or an entry without it. The option is this
// process's: a file written under
// the root by hand, and a store opened on it without the option, are
// outside what it can refuse, so a store that must fail closed against
// every process is a cas store set this way.
func WithRedactedOnly() Option {
	return func(s *Store) { s.redactedOnly = true }
}

// Store is a file-backed [agentsession.Store]. It is safe for
// concurrent use within one process. A store opened with
// [WithReadOnly] takes no lock and refuses every write. Across
// processes each open session is guarded by an advisory lock file
// beside it,
// <file>.lock, so a second process that opens the same session gets
// ErrSessionLocked instead of interleaving lines with the first. The
// lock is released by Release, Delete and Close; a lock left by a
// process on this host that no longer runs is taken over on the next
// open, and BreakLock clears one from any other holder.
type Store struct {
	root        string
	policy      SyncPolicy
	staleReport func(LockInfo)
	readOnly    bool
	// redactedOnly says the store holds only redacted entries.
	redactedOnly bool
	followEvery  time.Duration
	// hub rings the followers of a session this store appends to.
	hub wake.Hub

	mu   sync.Mutex
	open map[string]*handle
	// refMu keeps this process's updates of the refs files in turn, so
	// they do not poll the lock file against each other.
	refMu sync.Mutex
}

type handle struct {
	session *agentsession.Session
	file    *os.File
	path    string
	// diskFormat is the format the file's header line names, which the
	// session's own header does not tell once Read has migrated it.
	diskFormat string
}

// Open returns a store over root, creating the directory if needed.
// A read-only store creates nothing: a missing root is an empty
// listing and a session that is not there.
func Open(root string, opts ...Option) (*Store, error) {
	s := &Store{root: root, open: map[string]*handle{}}
	for _, opt := range opts {
		opt(s)
	}
	if !s.readOnly {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return nil, fmt.Errorf("jsonl: create root: %w", err)
		}
	}
	if s.redactedOnly {
		if err := agentsession.CheckHoldsRedacted(context.Background(), s); err != nil {
			return nil, fmt.Errorf("jsonl: not set to hold only redacted entries: %w", err)
		}
	}
	return s, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// ProjectKey derives the directory name for a working directory: the
// leading separator stripped and path separators and colons replaced
// by "-", so "/home/u/proj" becomes "home-u-proj". An empty directory,
// and one whose key would be "." or "..", which name a directory other
// than a project's own, maps to "default".
func ProjectKey(cwd string) string {
	if cwd == "" {
		return "default"
	}
	key := filepath.ToSlash(cwd)
	key = strings.TrimLeft(key, "/")
	key = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(key)
	if key == "" || key == "." || key == ".." {
		return "default"
	}
	return key
}

// ErrBadName is returned by Create for a session ID that cannot be
// used in a file name: an ID is letters, digits, '.', '_' and '-', at
// most 200 of them, and is neither "." nor "..", the rule the cas store
// keeps. A header comes from whoever creates the session, a remote
// client or an imported file among them, so its ID is checked before
// it names a path.
var ErrBadName = errors.New("jsonl: not a usable name")

// validSessionID is cas's rule for a session ID used as a path.
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

// within reports whether path lies under root, which every file the
// store writes must.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Path returns the file a session lives in, whether or not it is open.
func (s *Store) Path(id string) (string, error) {
	s.mu.Lock()
	if h, ok := s.open[id]; ok {
		s.mu.Unlock()
		return h.path, nil
	}
	s.mu.Unlock()
	return s.find(id)
}

// find locates a session file by ID.
func (s *Store) find(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, `/\*?[`) {
		return "", fmt.Errorf("%w: %q", agentsession.ErrNoSession, id)
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "*", "*_"+id+".jsonl"))
	if err != nil {
		return "", fmt.Errorf("jsonl: glob: %w", err)
	}
	if len(matches) == 0 {
		return "", fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	return matches[0], nil
}

func sessionPath(root string, h agentsession.Header) string {
	stamp := h.CreatedAt.UTC().Format("2006-01-02T15-04-05.000Z")
	return filepath.Join(root, ProjectKey(h.CWD), stamp+"_"+h.ID+".jsonl")
}

// Create implements agentsession.Store. The file is created with the
// header line, and for a header with a base the prefix after it, and
// synced before Create returns. The origin is read, not claimed: a fork
// can be made from a session another process is writing. A redacted
// session is created as any other is.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, agentsession.ErrReadOnly
	}
	if err := agentsession.CheckCreate(h, s.redactedOnly); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := agentsession.New(h)
	if h.Base != "" {
		origin, err := s.forkOrigin(ctx, h.ParentSession, h.Base)
		if err != nil {
			return nil, err
		}
		if sess, err = agentsession.Fork(origin, h.Base, h); err != nil {
			return nil, err
		}
		if s.redactedOnly {
			if err := agentsession.CheckRedacted(sess.Header(), sess.Entries()); err != nil {
				return nil, err
			}
		}
	}
	return s.place(sess)
}

// Import implements [agentsession.Importer]: the file is read with
// [agentsession.ReadForImport] and written as a session file under the
// root, its header and its lines, the prefix first, synced and linked
// into place before Import returns, as Create writes one; the session
// is then held open as Create holds it. A session the store holds is
// refused, as is a fork whose media differs from that of a held session
// whose own entries include the base, and, in a store set with
// [WithRedactedOnly], a session whose header does not carry redacted or
// that holds an entry without it.
// This store keeps a session's head only in its lines, so a file whose
// head only a projection's synthetic leaf marker names is refused. It
// keeps no record mark, so asRecord is not read.
func (s *Store) Import(ctx context.Context, r io.Reader, asRecord bool) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, agentsession.ErrReadOnly
	}
	sess, err := agentsession.ReadForImport(r)
	if err != nil {
		return nil, err
	}
	h := sess.Header()
	if s.redactedOnly {
		if err := agentsession.CheckRedacted(h, sess.Entries()); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if h.Base != "" {
		origin, err := s.forkOrigin(ctx, h.ParentSession, h.Base)
		if err != nil && !errors.Is(err, agentsession.ErrNoEntry) {
			return nil, err
		}
		if err := agentsession.CheckImportMedia(h, origin); err != nil {
			return nil, err
		}
	}
	return s.place(sess)
}

// placeFault, when set, runs once place has made the project directory
// and before it writes anything there, and a non-nil error it returns
// refuses the create: a test's way to refuse a create after the
// directory is made.
var placeFault func() error

// place writes a new session's file, its header and every entry the
// session holds, beside its place and links it into place once synced,
// and holds it open for append. The caller holds s.mu.
func (s *Store) place(sess *agentsession.Session) (*agentsession.Session, error) {
	h := sess.Header()
	if !validSessionID(h.ID) {
		return nil, fmt.Errorf("%w: session id %q", ErrBadName, h.ID)
	}
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if _, err := s.find(h.ID); err == nil {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	path := sessionPath(s.root, h)
	if !within(s.root, path) {
		return nil, fmt.Errorf("%w: session %q in %q is outside the store", ErrBadName, h.ID, h.CWD)
	}
	// A project directory this call makes is removed again on a
	// refusal, when nothing else has come to live in it, so a refused
	// create or import leaves nothing under the root.
	dir := filepath.Dir(path)
	_, statErr := os.Stat(dir)
	made := errors.Is(statErr, os.ErrNotExist)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("jsonl: create project directory: %w", err)
	}
	unmake := func() {
		if made {
			os.Remove(dir) // fails, harmlessly, if it is not empty
		}
	}
	// Another process's refused create may remove the directory between
	// this one's making it and writing in it, when it made it first: it
	// is made again, a few times, rather than fail.
	again := func(err error) bool {
		if !errors.Is(err, os.ErrNotExist) {
			return false
		}
		return os.MkdirAll(dir, 0o755) == nil
	}
	if placeFault != nil {
		if err := placeFault(); err != nil {
			unmake()
			return nil, err
		}
	}
	lockErr := acquireLock(path, s.staleReport)
	for try := 0; try < 3 && again(lockErr); try++ {
		lockErr = acquireLock(path, s.staleReport)
	}
	if lockErr != nil {
		unmake()
		return nil, lockErr
	}
	// The header and any prefix are written beside the file and linked
	// into place once synced, so a crash never leaves a session file
	// that opens with a header naming a base and a prefix cut short. A
	// temporary file left by an earlier crash is ours to replace, since
	// the lock is held.
	tmp := path + ".tmp"
	os.Remove(tmp)
	fail := func(err error) (*agentsession.Session, error) {
		os.Remove(tmp)
		releaseLock(path)
		unmake()
		return nil, err
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	for try := 0; try < 3 && again(err); try++ {
		t, err = os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	}
	if err != nil {
		return fail(fmt.Errorf("jsonl: create session file: %w", err))
	}
	w := bufio.NewWriter(t)
	lines := []any{h}
	for _, e := range sess.Entries() {
		lines = append(lines, e)
	}
	for _, v := range lines {
		if err := writeLine(w, v); err != nil {
			t.Close()
			return fail(fmt.Errorf("jsonl: write header and prefix: %w", err))
		}
	}
	if err := w.Flush(); err != nil {
		t.Close()
		return fail(fmt.Errorf("jsonl: write header and prefix: %w", err))
	}
	if err := t.Sync(); err != nil {
		t.Close()
		return fail(fmt.Errorf("jsonl: sync header: %w", err))
	}
	if err := t.Close(); err != nil {
		return fail(fmt.Errorf("jsonl: close session file: %w", err))
	}
	if err := os.Link(tmp, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fail(fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID))
		}
		return fail(fmt.Errorf("jsonl: create session file: %w", err))
	}
	os.Remove(tmp)
	syncDir(filepath.Dir(path))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		releaseLock(path)
		return nil, fmt.Errorf("jsonl: open %s for append: %w", path, err)
	}
	s.open[h.ID] = &handle{session: sess, file: f, path: path, diskFormat: h.Format}
	return sess, nil
}

// syncDir makes a new directory entry durable. A failure is ignored:
// the file's contents are synced, and a file system that cannot sync a
// directory does not offer more.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// forkOrigin returns the session a fork at base continues from: named, when
// the named session's own entries include base, or the session up its
// parent_session line that owns it when named holds it on its prefix,
// else one whose own entries include it,
// else one that holds it on its prefix. Finding it without a name reads
// every session under the root. A session not open here is read without
// taking its lock. The caller holds s.mu.
func (s *Store) forkOrigin(ctx context.Context, named, base string) (*agentsession.Session, error) {
	read := func(id, path string) *agentsession.Session {
		if h, ok := s.open[id]; ok {
			return h.session
		}
		h, err := loadHandle(path, id, true)
		if err != nil {
			return nil
		}
		return h.session
	}
	// Named on its prefix, the base is its own origin's: parent_session
	// is followed up to the session whose own entries include it, as
	// cas finds it, since an import holds a fork to that one's media.
	for seen := 0; named != "" && seen < 64; seen++ {
		path, err := s.find(named)
		if err != nil {
			break
		}
		sess := read(named, path)
		if sess == nil {
			break
		}
		if _, ok := sess.Entry(base); !ok {
			break
		}
		if !sess.Prefix(base) || sess.Header().ParentSession == "" {
			return sess, nil
		}
		named = sess.Header().ParentSession
	}
	matches, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("jsonl: glob: %w", err)
	}
	sort.Strings(matches)
	var onPrefix *agentsession.Session
	for _, path := range matches {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(path), ".jsonl")
		_, id, ok := strings.Cut(name, "_")
		if !ok {
			continue
		}
		sess := read(id, path)
		if sess == nil {
			continue
		}
		if _, ok := sess.Entry(base); !ok {
			continue
		}
		if !sess.Prefix(base) {
			return sess, nil
		}
		if onPrefix == nil {
			onPrefix = sess
		}
	}
	if onPrefix == nil {
		return nil, fmt.Errorf("%w: base %s is not held by this store", agentsession.ErrNoEntry, base)
	}
	return onPrefix, nil
}

// Open implements agentsession.Store. A session already open in this
// store is returned as is; otherwise its lock is taken and its file is
// read. A final line left incomplete by a crash is reported through
// Session.Truncated and removed from the file so later appends
// continue a valid file. Open returns ErrSessionLocked when another
// process holds the session; a store opened with [WithReadOnly]
// claims nothing and so is never refused.
//
// The lock is the store's, not the caller's: it is kept until
// [Store.Release], [Store.Delete] or [Store.Close], and a Release frees
// it whoever opened the session. A process that reads sessions it does
// not write uses [Store.Read], or a second store opened with
// [WithReadOnly], so that it neither keeps them from other processes
// nor frees one its own writer is using.
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

// Read implements [agentsession.Reader]: it reads the session's file as
// a read-only store's Open does, taking no lock, trimming nothing and
// keeping nothing, so a session this store or another process is
// writing is read and stays the writer's. The session returned is the
// caller's own, read afresh from the file, even when this store holds
// the session open. A line an append is writing as the file is read is
// reported through Session.Truncated, as a crash's would be; an entry
// is in the file once the Append that wrote it has returned, and a
// leaf moved through Session.Branch is never in it. Under
// [SyncOnResponse] or [SyncNever] such an entry may not be durable yet,
// and a power loss can take back what Read showed; nothing here reads only
// what is durable, short of the writer calling [Store.Sync] first.
func (s *Store) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.find(id)
	if err != nil {
		return nil, err
	}
	h, err := loadHandle(path, id, true)
	if errors.Is(err, os.ErrNotExist) {
		// Deleted since it was found.
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if err != nil {
		return nil, err
	}
	return h.session, nil
}

func (s *Store) openLocked(id string) (*handle, error) {
	if h, ok := s.open[id]; ok {
		return h, nil
	}
	path, err := s.find(id)
	if err != nil {
		return nil, err
	}
	if s.readOnly {
		h, err := loadHandle(path, id, true)
		if err != nil {
			return nil, err
		}
		s.open[id] = h
		return h, nil
	}
	if err := acquireLock(path, s.staleReport); err != nil {
		return nil, err
	}
	h, err := loadHandle(path, id, false)
	if err != nil {
		releaseLock(path)
		return nil, err
	}
	s.open[id] = h
	return h, nil
}

// loadHandle reads a session file and, unless the store is read-only,
// opens it for append. The caller holds the session's lock.
func loadHandle(path, id string, readOnly bool) (*handle, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("jsonl: read %s: %w", path, err)
	}
	sess, err := agentsession.Read(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("jsonl: %s: %w", path, err)
	}
	if sess.ID() != id {
		return nil, fmt.Errorf("jsonl: %s holds session %s, not %s", path, sess.ID(), id)
	}
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	var stored agentsession.Header
	if err := stored.UnmarshalJSON(bytes.TrimRight(line, "\r")); err != nil {
		return nil, fmt.Errorf("jsonl: %s: header: %w", path, err)
	}
	if readOnly {
		// No append, so no lock and no trimming of a broken tail: the
		// truncated line is reported and the file is left as it is.
		return &handle{session: sess, path: path, diskFormat: stored.Format}, nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("jsonl: open %s for append: %w", path, err)
	}
	if sess.Truncated() != nil {
		// Cut the broken tail so the file stays a valid prefix. The bytes
		// were never a complete entry, so nothing recorded is lost.
		keep := len(bytes.TrimRight(data, "\r\n"))
		keep = bytes.LastIndexByte(data[:keep], '\n') + 1
		if err := f.Truncate(int64(keep)); err != nil {
			f.Close()
			return nil, fmt.Errorf("jsonl: drop truncated line of %s: %w", path, err)
		}
	}
	return &handle{session: sess, file: f, path: path, diskFormat: stored.Format}, nil
}

// Append implements agentsession.Store: the entry joins the in-memory
// tree, then its line is written and synced according to the policy.
// The first append to a session whose header names an earlier minor
// raises the header's format first.
//
// The entry is the store's once Append returns, as [agentsession.Session.Append]
// has it: the session the store shares holds e itself, not a copy, so a
// caller that changes e afterwards changes what that session and its
// followers read, and not what the file holds. [agentsession.MemoryStore]
// keeps a copy instead.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return "", err
	}
	if s.redactedOnly {
		if err := agentsession.CheckRedacted(h.session.Header(), []agentsession.Entry{e}); err != nil {
			return "", err
		}
	}
	// Followers look again however the append ends: a rewrite for the
	// format may have landed before a write that failed.
	defer s.hub.Notify(sessionID)
	return s.appendLocked(h, sessionID, e, false)
}

// appendLocked appends e to the open session h, and syncs it when sync
// is set or the policy asks. The caller holds s.mu.
func (s *Store) appendLocked(h *handle, sessionID string, e agentsession.Entry, sync bool) (string, error) {
	// The entry is prepared first, so that one the session refuses or
	// already holds leaves the file as it was: a second append of an
	// entry the session holds is a no-op, as RFC 0002 has it, and writing
	// its line again would leave a repeated id every reader reports.
	r, err := h.session.Prepare(e)
	if err != nil {
		return "", err
	}
	if r.Outcome == agentsession.Held {
		return r.ID, nil
	}
	if raises(h.diskFormat) {
		if err := s.raiseFormat(sessionID, h); err != nil {
			return "", err
		}
	}
	id, err := h.session.Append(e)
	if err != nil {
		return "", err
	}
	if err := writeLine(h.file, e); err != nil {
		return "", fmt.Errorf("jsonl: write entry %s: %w", id, err)
	}
	if sync || s.shouldSync(h.session.Header(), e) {
		if err := syncEntry(h.file); err != nil {
			return "", fmt.Errorf("jsonl: sync entry %s: %w", id, err)
		}
	}
	return id, nil
}

// SetHead implements [agentsession.HeadStore]. The file has no head of
// its own, only the leaf its lines resolve to, so the move is a leaf
// label for to, appended under the session's lock after the compare
// ([agentsession.Session.HeadLabel]) and synced before SetHead returns
// whatever the sync policy, as RFC 0002 asks of a durable head move.
// The session Open returned sees it as that label's append. A store
// set with [WithRedactedOnly] refuses the move with
// [agentsession.ErrNotRedacted], since the label carries no redacted.
func (s *Store) SetHead(ctx context.Context, sessionID, expected, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, err := s.openLocked(sessionID)
	if err != nil {
		return err
	}
	if s.redactedOnly {
		return fmt.Errorf("%w: a head move in %s is a leaf label, which carries no redacted", agentsession.ErrNotRedacted, sessionID)
	}
	label, err := h.session.HeadLabel(expected, to)
	if err != nil {
		return err
	}
	defer s.hub.Notify(sessionID)
	_, err = s.appendLocked(h, sessionID, label, true)
	return err
}

// hashedMinor is the first minor whose entry ids are envelope hashes.
const hashedMinor = 5

// raises reports whether a header naming format is raised before this
// package appends to its session: one of an earlier minor, back to the
// first whose ids are hashes. A header before that is left as it is,
// since a reader rewrites the entries of such a file and would read
// them, under a raised header, as carrying hashes they do not.
func raises(format string) bool {
	_, minor, err := agentsession.ParseFormat(format)
	return err == nil && minor >= hashedMinor && minor < agentsession.FormatMinor
}

// raiseFormat writes the format this package writes into the session's
// header before the first append that package makes to a session whose
// header names an earlier minor, so a reader of that earlier minor
// refuses the session rather than reading entries it cannot represent.
// Nothing hashed changes: the header is not an entry.
//
// The header is the file's first line, so the file is rewritten once:
// the raised header and every byte after it as it stands go to a file
// beside it, which is synced and renamed over it, and appends continue
// on the new file. A crash before the rename leaves the file as it was.
// The caller holds the session's lock.
//
// Raise sees the header alone. An entry holding redacted under a
// header before 0.13 also forbids the raise, and that is the
// session's Prepare to refuse, by Session.RaiseRefused, with
// agentsession.ErrNoRaise: every append prepares its entry on the
// session before it reaches here.
func (s *Store) raiseFormat(id string, h *handle) error {
	fail := func(err error) error {
		return fmt.Errorf("jsonl: raise the header's format of %s: %w", h.path, err)
	}
	data, err := os.ReadFile(h.path)
	if err != nil {
		return fail(err)
	}
	line, rest, _ := bytes.Cut(data, []byte{'\n'})
	var hdr agentsession.Header
	if err := hdr.UnmarshalJSON(bytes.TrimRight(line, "\r")); err != nil {
		return fail(err)
	}
	if err := hdr.Raise(agentsession.Format); err != nil {
		return fail(err)
	}
	info, err := os.Stat(h.path)
	if err != nil {
		return fail(err)
	}
	// A temporary file left by an earlier crash is ours to replace,
	// since the lock is held.
	tmp := h.path + ".tmp"
	os.Remove(tmp)
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, info.Mode().Perm())
	if err != nil {
		return fail(err)
	}
	w := bufio.NewWriter(t)
	err = writeLine(w, hdr)
	if err == nil {
		_, err = w.Write(rest)
	}
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		err = t.Sync()
	}
	if err != nil {
		t.Close()
		os.Remove(tmp)
		return fail(err)
	}
	// The old file is closed before the rename, which some systems
	// refuse over a file held open, and opened again if it fails.
	h.file.Close()
	if err := os.Rename(tmp, h.path); err != nil {
		t.Close()
		os.Remove(tmp)
		f, oerr := os.OpenFile(h.path, os.O_WRONLY|os.O_APPEND, 0o600)
		if oerr != nil {
			// Nothing is left to append through: forget the session so
			// the next open starts over.
			delete(s.open, id)
			releaseLock(h.path)
			return fail(errors.Join(err, oerr))
		}
		h.file = f
		return fail(err)
	}
	syncDir(filepath.Dir(h.path))
	h.file = t
	h.diskFormat = agentsession.Format
	return nil
}

func (s *Store) shouldSync(hdr agentsession.Header, e agentsession.Entry) bool {
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

// Sync fsyncs an open session's file.
func (s *Store) Sync(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.open[id]
	if !ok {
		return fmt.Errorf("%w: %s is not open", agentsession.ErrNoSession, id)
	}
	return h.file.Sync()
}

// List implements agentsession.Store by reading the header line of
// every session file under the root. Files that do not parse are
// reported as errors in the sequence and skipped.
func (s *Store) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		if err := ctx.Err(); err != nil {
			yield(agentsession.Summary{}, err)
			return
		}
		matches, err := filepath.Glob(filepath.Join(s.root, "*", "*.jsonl"))
		if err != nil {
			yield(agentsession.Summary{}, fmt.Errorf("jsonl: glob: %w", err))
			return
		}
		var out []agentsession.Summary
		var errs []error
		for _, path := range matches {
			sum, err := summarize(path, f.WithNames || f.Current)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if f.Keep(sum) {
				out = append(out, sum)
			}
		}
		sort.Slice(out, func(i, j int) bool {
			return out[i].Header.CreatedAt.After(out[j].Header.CreatedAt)
		})
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

// summarize reads a session file's header and stat, and scans the
// rest of the file for the name when asked.
func summarize(path string, withName bool) (agentsession.Summary, error) {
	file, err := os.Open(path)
	if err != nil {
		return agentsession.Summary{}, fmt.Errorf("jsonl: %s: %w", path, err)
	}
	defer file.Close()
	br := bufio.NewReader(file)
	line, err := br.ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return agentsession.Summary{}, fmt.Errorf("jsonl: %s: %w", path, err)
	}
	var h agentsession.Header
	if err := h.UnmarshalJSON(bytes.TrimRight(line, "\r\n")); err != nil {
		return agentsession.Summary{}, fmt.Errorf("jsonl: %s: header: %w", path, err)
	}
	if err := h.Validate(); err != nil {
		return agentsession.Summary{}, fmt.Errorf("jsonl: %s: %w", path, err)
	}
	info, err := file.Stat()
	if err != nil {
		return agentsession.Summary{}, fmt.Errorf("jsonl: %s: %w", path, err)
	}
	sum := agentsession.Summary{Header: h, Path: path, Size: info.Size(), Modified: info.ModTime()}
	if withName {
		if sum.Name, sum.SupersededBy, err = scanMeta(br); err != nil {
			return agentsession.Summary{}, fmt.Errorf("jsonl: %s: %w", path, err)
		}
	}
	return sum, nil
}

// scanMeta reads the entry lines after the header and returns the
// name from the last info entry that set one and the successor from
// the last continued_in link. Lines that do not decode are skipped: a
// listing is not the place to report them.
func scanMeta(br *bufio.Reader) (name, supersededBy string, err error) {
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && bytes.Contains(line, []byte(`"link"`)) {
			var probe struct {
				Type    string `json:"type"`
				Rel     string `json:"rel"`
				Session string `json:"session"`
			}
			if json.Unmarshal(line, &probe) == nil && probe.Type == agentsession.TypeLink && probe.Rel == agentsession.RelContinuedIn && probe.Session != "" {
				supersededBy = probe.Session
			}
		}
		if len(line) > 0 && bytes.Contains(line, []byte(`"info"`)) {
			var probe struct {
				Type string `json:"type"`
				Name string `json:"name"`
			}
			if json.Unmarshal(line, &probe) == nil && probe.Type == agentsession.TypeInfo && probe.Name != "" {
				name = probe.Name
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return name, supersededBy, nil
			}
			return "", "", err
		}
	}
}

// Delete implements agentsession.Store: the file is removed and the
// session forgotten.
func (s *Store) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var path string
	if h, ok := s.open[id]; ok {
		path = h.path
		h.file.Close()
		delete(s.open, id)
	} else {
		var err error
		if path, err = s.find(id); err != nil {
			return err
		}
		if err := acquireLock(path, s.staleReport); err != nil {
			return err
		}
	}
	defer s.hub.Notify(id)
	if err := os.Remove(path); err != nil {
		releaseLock(path)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
		}
		return fmt.Errorf("jsonl: remove %s: %w", path, err)
	}
	return releaseLock(path)
}

// Release syncs and closes an open session's file and drops its lock
// without deleting it. The session can be opened again later, by this
// or another process. On a read-only store there is no lock and
// nothing to sync, and Release is how a reader drops a session it has
// cached so the next Open reads what has been appended since.
//
// The store holds a session once, however many callers opened it, so
// Release lets it go for all of them: a writer still appending through
// this store has the session read again at its next append, or is
// refused with ErrSessionLocked if another process took it meanwhile,
// and the Session it was handed no longer follows the store.
func (s *Store) Release(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.open[id]
	if !ok {
		return nil
	}
	delete(s.open, id)
	return h.close()
}

// close syncs and closes the file, then drops the lock. The first
// error is returned; the lock is dropped regardless. A read-only
// handle holds neither.
func (h *handle) close() error {
	if h.file == nil {
		return nil
	}
	err := h.file.Sync()
	if cerr := h.file.Close(); err == nil {
		err = cerr
	}
	if lerr := releaseLock(h.path); err == nil {
		err = lerr
	}
	return err
}

// Close syncs and closes every open session file.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for id, h := range s.open {
		if err := h.close(); err != nil && first == nil {
			first = err
		}
		delete(s.open, id)
	}
	return first
}

// writeLine writes v as one JSON line.
func writeLine(w io.Writer, v any) error {
	var data []byte
	var err error
	switch x := v.(type) {
	case agentsession.Entry:
		data, err = agentsession.MarshalEntry(x)
	case agentsession.Header:
		data, err = x.MarshalJSON()
	default:
		return fmt.Errorf("jsonl: cannot write %T", v)
	}
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}

var (
	_ agentsession.Store     = (*Store)(nil)
	_ agentsession.Reader    = (*Store)(nil)
	_ agentsession.Importer  = (*Store)(nil)
	_ agentsession.HeadStore = (*Store)(nil)
	_ agentsession.Releaser  = (*Store)(nil)
)

// syncEntry fsyncs a session's file after an append the policy makes
// durable; a test counts its calls.
var syncEntry = (*os.File).Sync
