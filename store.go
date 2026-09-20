package agentsession

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sort"
	"sync"
	"time"
)

// ErrNoSession is returned when a session ID is not in the store.
var ErrNoSession = errors.New("agentsession: no such session")

// ErrSessionExists is returned when Create is given an ID already in
// the store.
var ErrSessionExists = errors.New("agentsession: session already exists")

// Store persists sessions. Append is the only write to a session's
// content; the entry is added to the in-memory tree of the open session
// and made durable according to the store's policy. Sessions returned
// by Create and Open are shared with the store, so leaf moves through
// Session.Branch and Session.ResetLeaf are seen by later appends.
type Store interface {
	// Create starts a new session from h, filling empty header fields.
	Create(ctx context.Context, h Header) (*Session, error)
	// Open loads the session with the given ID.
	Open(ctx context.Context, id string) (*Session, error)
	// Append adds e to the session and returns its ID.
	Append(ctx context.Context, sessionID string, e Entry) (string, error)
	// List enumerates sessions matching f, newest first.
	List(ctx context.Context, f ListFilter) iter.Seq2[Summary, error]
	// Delete removes the session.
	Delete(ctx context.Context, id string) error
}

// ListFilter narrows a List. Zero fields do not filter.
type ListFilter struct {
	// CWD matches the header's working directory exactly.
	CWD string
	// ParentSession matches sessions forked or spawned from the given
	// one.
	ParentSession string
	// After and Before bound created_at, exclusive.
	After, Before time.Time
	// Limit caps the number of results; zero means no cap.
	Limit int
	// WithNames asks for Summary.Name. A store that keeps names
	// indexed fills it regardless; a file store must scan each
	// session's entries to find it, so it does so only when asked.
	WithNames bool
}

// Matches reports whether a header passes the filter.
func (f ListFilter) Matches(h Header) bool {
	if f.CWD != "" && h.CWD != f.CWD {
		return false
	}
	if f.ParentSession != "" && h.ParentSession != f.ParentSession {
		return false
	}
	if !f.After.IsZero() && !h.CreatedAt.After(f.After) {
		return false
	}
	if !f.Before.IsZero() && !h.CreatedAt.Before(f.Before) {
		return false
	}
	return true
}

// Summary describes a stored session without loading it.
type Summary struct {
	Header Header
	// Name is the session's display name, from the last info entry
	// that set one, when the store provides it: see
	// ListFilter.WithNames.
	Name string
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
}

// NewMemoryStore returns an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{sessions: map[string]*Session{}}
}

// Create implements Store.
func (m *MemoryStore) Create(_ context.Context, h Header) (*Session, error) {
	s := New(h)
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.sessions[s.ID()]; exists {
		return nil, fmt.Errorf("%w: %s", ErrSessionExists, s.ID())
	}
	m.sessions[s.ID()] = s
	return s, nil
}

// Open implements Store.
func (m *MemoryStore) Open(_ context.Context, id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoSession, id)
	}
	return s, nil
}

// Append implements Store.
func (m *MemoryStore) Append(ctx context.Context, sessionID string, e Entry) (string, error) {
	s, err := m.Open(ctx, sessionID)
	if err != nil {
		return "", err
	}
	return s.Append(e)
}

// List implements Store.
func (m *MemoryStore) List(_ context.Context, f ListFilter) iter.Seq2[Summary, error] {
	m.mu.RLock()
	var out []Summary
	for _, s := range m.sessions {
		h := s.Header()
		if f.Matches(h) {
			out = append(out, Summary{Header: h, Name: s.Name()})
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
	return nil
}

var _ Store = (*MemoryStore)(nil)
