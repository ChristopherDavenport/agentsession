// Package jsonl is the default session store: one append-only JSONL
// file per session under a root directory, laid out as
//
//	<root>/<project-key>/<created-at>_<session-id>.jsonl
//
// where the project key derives from the session's working directory.
// Each Append writes one line; a SyncPolicy decides when the file is
// fsynced. Opening a session whose last line was cut short by a crash
// drops that line so later appends produce a valid file.
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

	"github.com/ChristopherDavenport/agentsession"
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
	// calls to Store.Sync.
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
// it writes, and Create, Append, Delete, Sync and BreakLock return
// [agentsession.ErrReadOnly]. Nothing is written, the store's root
// directory included, and a file whose last line was cut short is
// reported through Session.Truncated and left alone, since trimming
// it is a write. It is what show, verify and export want, and what a
// host wants when an operator asks about a session the agent holds.
//
// An open session is cached as it is in a writing store, so a session
// read while another process appends to it shows what it held when it
// was opened; call [Store.Release] and open it again to see the rest.
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

	mu   sync.Mutex
	open map[string]*handle
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
	return s, nil
}

// Root returns the store's directory.
func (s *Store) Root() string { return s.root }

// ProjectKey derives the directory name for a working directory: the
// leading separator stripped and path separators and colons replaced
// by "-", so "/home/u/proj" becomes "home-u-proj". An empty directory
// maps to "default".
func ProjectKey(cwd string) string {
	if cwd == "" {
		return "default"
	}
	key := filepath.ToSlash(cwd)
	key = strings.TrimLeft(key, "/")
	key = strings.NewReplacer("/", "-", "\\", "-", ":", "-").Replace(key)
	if key == "" {
		return "default"
	}
	return key
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
// can be made from a session another process is writing.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.readOnly {
		return nil, agentsession.ErrReadOnly
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
	}
	h = sess.Header()
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	if _, err := s.find(h.ID); err == nil {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	path := sessionPath(s.root, h)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("jsonl: create project directory: %w", err)
	}
	if err := acquireLock(path, s.staleReport); err != nil {
		return nil, err
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
		return nil, err
	}
	t, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
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
// the named session holds base, else one whose own entries include it,
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
	if named != "" {
		if path, err := s.find(named); err == nil {
			if sess := read(named, path); sess != nil {
				if _, ok := sess.Entry(base); ok {
					return sess, nil
				}
			}
		}
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
	if s.shouldSync(h.session.Header(), e) {
		if err := h.file.Sync(); err != nil {
			return "", fmt.Errorf("jsonl: sync entry %s: %w", id, err)
		}
	}
	return id, nil
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
	hdr.Format = agentsession.Format
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

var _ agentsession.Store = (*Store)(nil)
