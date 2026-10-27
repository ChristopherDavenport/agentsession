package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"sort"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/wake"
)

// ErrNoSession is returned when a session ID is not in the store.
var ErrNoSession = errors.New("agentsession: no such session")

// ErrSessionExists is returned when Create is given an ID already in
// the store.
var ErrSessionExists = errors.New("agentsession: session already exists")

// ErrSessionLocked is returned by a store whose session is held by
// another process. It is the one sentinel for that condition: each
// store wraps it, so a host written against the Store interface can
// tell "another process has this session", which means leave the
// conversation alone, from a store that is broken, which means the
// daemon is unhealthy, without knowing which store it was given.
var ErrSessionLocked = errors.New("agentsession: session is open in another process")

// ErrReadOnly is returned by a store opened read-only when a caller
// tries to write: such a store takes no hold on the sessions it
// opens, so it must not append to them either. Reading a session
// while a harness writes it is what it is for.
var ErrReadOnly = errors.New("agentsession: store is read-only")

// Store persists sessions. Append is the only write to a session's
// content; the entry is added to the in-memory tree of the open session
// and made durable according to the store's policy. Sessions returned
// by Create and Open are shared with the store, so leaf moves through
// Session.Branch and Session.ResetLeaf are seen by later appends.
//
// A store that guards a session against a second writing process
// reports [ErrSessionLocked] from the calls that need the hold, and a
// store opened read-only reports [ErrReadOnly] from the calls that
// write.
type Store interface {
	// Create starts a new session from h, filling empty header fields.
	// A header whose Base is set makes a fork, as [Fork] does: the
	// session opens with the path to the base, taken from the session
	// ParentSession names when that session holds it and otherwise from
	// any session the store holds it in. Create refuses a base the store
	// does not hold with ErrNoEntry, and a base that is a leaf label or a
	// media form other than the origin's, rather than letting the first
	// append fail.
	Create(ctx context.Context, h Header) (*Session, error)
	// Open loads the session with the given ID. A store that guards
	// sessions against a second writing process takes the session's
	// hold here and keeps it until the store lets it go, at its Release
	// or Close: one hold per session per store, which a Release frees
	// whoever opened it. A process that reads sessions it does not
	// write, such as a search across a history beside the harness
	// writing it, reads them through [Reader] or through a second store
	// opened read-only, never by Open and Release on the writing store,
	// which would keep each session from other processes or free one
	// its own writer is using.
	Open(ctx context.Context, id string) (*Session, error)
	// Append adds e to the session and returns its ID.
	Append(ctx context.Context, sessionID string, e Entry) (string, error)
	// List enumerates sessions matching f, newest first.
	List(ctx context.Context, f ListFilter) iter.Seq2[Summary, error]
	// Delete removes the session.
	Delete(ctx context.Context, id string) error
}

// Reader is implemented by a store that can read a session without
// holding it. Read takes no hold, writes nothing, the recovery an Open
// would write included, and keeps nothing: it returns a session of its
// own, read from what the store holds, which no later append reaches
// and whose leaf moves move no other session's. A session this store
// or another process is writing is read as a store opened read-only
// would read it, and stays the writer's. A missing session is
// [ErrNoSession].
//
// What Read sees of a session being written is what the store held
// when it read: an append in flight may not be there yet, and a leaf a
// writer moved through Session.Branch and has not recorded is not.
// Nor need what it sees be durable: Read can show an append its writer
// has not yet made durable, under a lazy sync policy or while its
// fsync runs, which a crash can take back. Each store says what, if
// anything, reads only what is durable.
type Reader interface {
	Read(ctx context.Context, id string) (*Session, error)
}

// Writer is implemented by a store that reports what an append did, as
// RFC 0002 asks of a store: the outcome (continued, branched, held, a
// leaf label's move or not), whether the entry was durable when the
// call returned, and the references it could not resolve. Write is
// Append with that [Result]; a store that implements it keeps the two
// the same append. A store over a network (RFC 0003's remote client)
// returns what the server answered.
type Writer interface {
	Write(ctx context.Context, sessionID string, e Entry) (Result, error)
}

// Releaser is implemented by a store that holds the sessions it opens,
// as [Store.Open] describes: Release lets the hold on the session go,
// for every caller that opened it, and forgets the session, so the next
// Open reads it again. An error says the hold could not be let go; the
// session is forgotten all the same. Releasing a session the store does
// not hold is not an error. A program that serves a store, as RFC 0003's
// server does, releases a session through it when a lease ends.
type Releaser interface {
	Release(id string) error
}

// HeadStore is implemented by a store that moves a session's head by
// compare-and-swap, which RFC 0002 (Head) asks of every store and every
// store this module offers implements. SetHead moves the head of the
// session from expected to the entry to, and returns once the move is
// durable, as RFC 0002's Durability asks of a durable head move. ""
// is a valid expected value, for a session with no head; to names an
// entry, since a head is not moved to none. A head that is not
// expected is [ErrHeadMoved]; a target that is not the base or one of
// the session's own entries is [ErrNoEntry], and one that is a leaf
// label [ErrHeadTarget].
//
// A store that keeps sessions as RFC 0001 files, or as their lines,
// moves the head by appending a leaf label for to under its lock, after
// the compare; the session the store shares with its callers sees the
// move as an append. A store with a head of its own (the cas store)
// records the head. Either way a writer that appended a root to a
// session with a head, which is a branch, moves the head onto it here.
type HeadStore interface {
	SetHead(ctx context.Context, sessionID, expected, to string) error
}

// ErrHeadMoved is returned by [HeadStore.SetHead] when the session's
// head is not the expected entry, which is how a writer learns that
// another moved it.
var ErrHeadMoved = errors.New("agentsession: head is not the expected entry")

// ErrHeadTarget is returned by [HeadStore.SetHead] for a target the
// head never rests on: a leaf label.
var ErrHeadTarget = errors.New("agentsession: the head never rests on a leaf label")

// ListFilter narrows a List. Zero fields do not filter, so the zero
// filter lists every session. The header fields cost no scan: every
// store reads the header to list a session, and Matches decides on it
// alone; WithNames and Current read the session's entries.
type ListFilter struct {
	// CWD matches the header's working directory exactly.
	CWD string
	// ParentSession matches sessions forked or spawned from the given
	// one. A header cannot say which: a judge, a subagent and a fork
	// all carry the session they came from, so they are listed
	// together. The link entries of the parent say which each is, as
	// [Judges] reads for judges.
	ParentSession string
	// Harness matches the name of the header's harness exactly; a
	// header naming no harness matches nothing when it is set. A
	// scheduled routine finds its own sessions by it without an index
	// of its own beside the store.
	Harness string
	// Extra matches the header's extra members: each member named here
	// must be in the header with the same value, compared in canonical
	// form so key order and spacing do not matter; members the header
	// has beyond these do not count. A harness that writes its user or
	// platform into the header selects by them here.
	Extra map[string]json.RawMessage
	// TopLevel keeps only sessions with no spawned_by: those a person or
	// a schedule started, and not the subsessions a call spawned.
	TopLevel bool
	// After and Before bound created_at, exclusive.
	After, Before time.Time
	// Limit caps the number of results; zero means no cap.
	Limit int
	// WithNames asks for Summary.Name. A store that keeps names
	// indexed fills it regardless; a file store must scan each
	// session's entries to find it, so it does so only when asked.
	WithNames bool
	// Current excludes sessions a continued_in link has retired, so a
	// listing shows the successor and not the session it replaced.
	// A file store scans each session's entries to know, as for
	// WithNames.
	Current bool
}

// Keep reports whether a summary passes the filter: the header checks
// of Matches, and Current against Summary.SupersededBy.
func (f ListFilter) Keep(sum Summary) bool {
	if !f.Matches(sum.Header) {
		return false
	}
	if f.Current && sum.SupersededBy != "" {
		return false
	}
	return true
}

// Matches reports whether a header passes the filter: the fields of the
// filter that read the header alone, which is every one but WithNames
// and Current.
func (f ListFilter) Matches(h Header) bool {
	if f.CWD != "" && h.CWD != f.CWD {
		return false
	}
	if f.ParentSession != "" && h.ParentSession != f.ParentSession {
		return false
	}
	if f.Harness != "" && (h.Harness == nil || h.Harness.Name != f.Harness) {
		return false
	}
	if f.TopLevel && h.SpawnedBy != "" {
		return false
	}
	for k, want := range f.Extra {
		got, ok := h.Extra[k]
		if !ok || !sameCanonical(want, got) {
			return false
		}
	}
	if !f.After.IsZero() && !h.CreatedAt.After(f.After) {
		return false
	}
	if !f.Before.IsZero() && !h.CreatedAt.Before(f.Before) {
		return false
	}
	return true
}

// sameCanonical reports whether two JSON values are the same in canonical
// form, the form the format hashes, so two spellings of one value match.
// A value that is not JSON matches nothing.
func sameCanonical(a, b json.RawMessage) bool {
	ca, err := jcs.Transform(a)
	if err != nil {
		return false
	}
	cb, err := jcs.Transform(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// Summary describes a stored session without loading it.
type Summary struct {
	Header Header
	// Name is the session's display name, from the last info entry
	// that set one, when the store provides it: see
	// ListFilter.WithNames.
	Name string
	// SupersededBy names the successor the session was continued in,
	// from its last continued_in link, when the store provides it: a
	// store that indexes links fills it always, a file store when
	// ListFilter.WithNames or Current asks it to scan.
	SupersededBy string
	// Path is where the session lives, for file-backed stores.
	Path string
	// Size is the stored size in bytes, when known.
	Size int64
	// Modified is when the session last changed, when known.
	Modified time.Time
}

// MemoryStore keeps sessions in memory. It is the reference store for
// tests and for sessions that are never persisted.
type MemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	// gens names each session's creation, so a follower can tell a
	// session created again under a deleted ID from the one it follows.
	gens map[string]uint64
	hub  wake.Hub
	refs refState
	// wmu makes each append, and each head move, one step: the rules a
	// writer is held to are checked and the entry added under it.
	wmu sync.Mutex
	// redactedOnly says the store holds only redacted entries.
	redactedOnly bool
}

// MemoryOption configures a [MemoryStore].
type MemoryOption func(*MemoryStore)

// MemoryRedactedOnly sets the store to hold only redacted entries, as
// RFC 0002 lets a store be set: it refuses, with [ErrNotRedacted], a
// session whose header does not carry redacted, at Create or Import,
// and every entry that carries no redacted member, however it arrives:
// an Append, an Import, the prefix of a fork Create makes. A fork's own
// entries and a resumed redacted session's native entries are refused
// alike, since neither was redacted.
//
// That holds for what comes through the store. The store hands out its
// own sessions, from Create, Open and Import, and an entry added through
// one of them, by [Session.Append], Commit or Extend, never reaches the
// store's checks, so such a store holds whatever its callers write that
// way. A store that must fail closed against its callers is a cas store
// set with its WithRedactedOnly.
func MemoryRedactedOnly() MemoryOption {
	return func(m *MemoryStore) { m.redactedOnly = true }
}

// NewMemoryStore returns an empty store.
func NewMemoryStore(opts ...MemoryOption) *MemoryStore {
	m := &MemoryStore{sessions: map[string]*Session{}, gens: map[string]uint64{},
		refs: refState{refs: map[string]RefTarget{}, idents: map[string]string{}, logs: map[string][]RefUpdate{}}}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Create implements Store. A header whose Base is set makes a fork of
// the session holding the base, as [Fork] does; a redacted session is
// created as any other is.
// The session returned is the store's own, shared with it, so a write
// through it, by [Session.Append], Commit or Extend, bypasses every rule
// the store holds an append to, MemoryRedactedOnly's among them.
func (m *MemoryStore) Create(_ context.Context, h Header) (*Session, error) {
	if err := CheckCreate(h, m.redactedOnly); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := New(h)
	if h.Base != "" {
		origin := m.forkOrigin(h.ParentSession, h.Base)
		if origin == nil {
			return nil, fmt.Errorf("%w: base %s is not held by this store", ErrNoEntry, h.Base)
		}
		var err error
		if s, err = Fork(origin, h.Base, h); err != nil {
			return nil, err
		}
		if m.redactedOnly {
			if err := CheckRedacted(s.Header(), s.Entries()); err != nil {
				return nil, err
			}
		}
	}
	if _, exists := m.sessions[s.ID()]; exists {
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, s.ID())
	}
	m.sessions[s.ID()] = s
	m.gens[s.ID()] = memoryGen.Add(1)
	return s, nil
}

// Import implements [Importer]: the file is read with [ReadForImport],
// and the session it holds is kept as read, its leaf the resume rule's,
// and returned as the store holds it, as Create returns one. A session
// this store holds is refused, as is a fork whose media differs from
// that of a held session whose own entries include the base, and, in a
// store set with MemoryRedactedOnly, a session whose header does not
// carry redacted or that holds an entry without it.
// The store keeps no record mark, so asRecord is not read.
// The session returned is the store's own, shared with it, so a write
// through it, by [Session.Append], Commit or Extend, bypasses every rule
// the store holds an append to, MemoryRedactedOnly's among them.
func (m *MemoryStore) Import(ctx context.Context, r io.Reader, asRecord bool) (*Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := ReadForImport(r)
	if err != nil {
		return nil, err
	}
	h := s.Header()
	if m.redactedOnly {
		if err := CheckRedacted(h, s.Entries()); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[h.ID]; exists {
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, h.ID)
	}
	if h.Base != "" {
		if err := CheckImportMedia(h, m.forkOrigin(h.ParentSession, h.Base)); err != nil {
			return nil, err
		}
	}
	m.sessions[h.ID] = s
	m.gens[h.ID] = memoryGen.Add(1)
	m.hub.Notify(h.ID)
	return s, nil
}

// forkOrigin returns the session a fork at base continues from: named, when
// the named session's own entries include base, or the session up its
// parent_session line that owns it when named holds it on its prefix,
// else one whose own entries include it,
// else one that holds it on its prefix, the first by ID when several
// do. The caller holds m.mu.
func (m *MemoryStore) forkOrigin(named, base string) *Session {
	// Named on its prefix, the base is its own origin's: parent_session
	// is followed up to the session whose own entries include it, as
	// cas finds it, since an import holds a fork to that one's media.
	for seen := 0; named != "" && seen < 64; seen++ {
		s, ok := m.sessions[named]
		if !ok {
			break
		}
		if _, ok := s.Entry(base); !ok {
			break
		}
		if !s.Prefix(base) || s.Header().ParentSession == "" {
			return s
		}
		named = s.Header().ParentSession
	}
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var onPrefix *Session
	for _, id := range ids {
		s := m.sessions[id]
		if _, ok := s.Entry(base); !ok {
			continue
		}
		if !s.Prefix(base) {
			return s
		}
		if onPrefix == nil {
			onPrefix = s
		}
	}
	return onPrefix
}

// Open implements Store.
// The session returned is the store's own, shared with it, so a write
// through it, by [Session.Append], Commit or Extend, bypasses every rule
// the store holds an append to, MemoryRedactedOnly's among them.
func (m *MemoryStore) Open(_ context.Context, id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSession, id)
	}
	return s, nil
}

// Read implements [Reader]: a copy of the session, rebuilt from its
// encoding. Its leaf is the one the entries record, as a store reading
// a file finds it: a leaf moved through Session.Branch and not recorded
// is not there.
func (m *MemoryStore) Read(ctx context.Context, id string) (*Session, error) {
	live, err := m.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := Write(&buf, live); err != nil {
		return nil, err
	}
	return Read(&buf)
}

// Append implements Store.
func (m *MemoryStore) Append(ctx context.Context, sessionID string, e Entry) (string, error) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	s, err := m.Open(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if m.redactedOnly {
		if err := CheckRedacted(s.Header(), []Entry{e}); err != nil {
			return "", err
		}
	}
	// The store keeps an entry of its own. Prepare fills the caller's
	// entry, its parent, time and ID, as Append does; the session then
	// takes a copy, so a caller that writes to its entry afterwards, as
	// a recorder setting the ID it was given does, shares nothing with
	// what the store holds or what a follower is reading.
	pre, err := s.Prepare(e)
	if err != nil {
		return "", err
	}
	if pre.Outcome != Held {
		// The first append to a session whose header names an earlier
		// minor raises it, as RFC 0002 has a store do, so the entry is
		// read under the format it was written with. Prepare has
		// refused a session that may not be raised.
		if err := s.raiseHeader(Format); err != nil {
			return "", err
		}
	}
	b, err := MarshalEntry(e)
	if err != nil {
		return "", err
	}
	own, err := UnmarshalEntry(b)
	if err != nil {
		return "", err
	}
	r, err := s.Commit(own)
	m.hub.Notify(sessionID)
	if err != nil {
		return "", err
	}
	return r.ID, nil
}

// SetHead implements [HeadStore]: the compare and the leaf label that
// records the move are one step on the store's session, so no append
// comes between them, and [MemoryStore.Read] and a follower see the
// move as that label. A store set with MemoryRedactedOnly refuses the
// move with [ErrNotRedacted], since the label carries no redacted.
func (m *MemoryStore) SetHead(ctx context.Context, sessionID, expected, to string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.wmu.Lock()
	defer m.wmu.Unlock()
	s, err := m.Open(ctx, sessionID)
	if err != nil {
		return err
	}
	if m.redactedOnly {
		return fmt.Errorf("%w: a head move in %s is a leaf label, which carries no redacted", ErrNotRedacted, sessionID)
	}
	// The header is raised, as for an append, only for a move the
	// compare would let through; commitHead compares again under the
	// session's lock.
	if _, err := s.HeadLabel(expected, to); err != nil {
		return err
	}
	if err := s.raiseHeader(Format); err != nil {
		return err
	}
	_, err = s.commitHead(expected, to)
	m.hub.Notify(sessionID)
	return err
}

// List implements Store.
func (m *MemoryStore) List(_ context.Context, f ListFilter) iter.Seq2[Summary, error] {
	m.mu.RLock()
	var out []Summary
	for _, s := range m.sessions {
		sum := Summary{Header: s.Header(), Name: s.Name(), SupersededBy: s.SupersededBy()}
		if f.Keep(sum) {
			out = append(out, sum)
		}
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		return out[i].Header.CreatedAt.After(out[j].Header.CreatedAt)
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return func(yield func(Summary, error) bool) {
		for _, s := range out {
			if !yield(s, nil) {
				return
			}
		}
	}
}

// Delete implements Store.
func (m *MemoryStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNoSession, id)
	}
	delete(m.sessions, id)
	delete(m.gens, id)
	m.hub.Notify(id)
	return nil
}

var (
	_ Store     = (*MemoryStore)(nil)
	_ Reader    = (*MemoryStore)(nil)
	_ Follower  = (*MemoryStore)(nil)
	_ Importer  = (*MemoryStore)(nil)
	_ HeadStore = (*MemoryStore)(nil)
)
