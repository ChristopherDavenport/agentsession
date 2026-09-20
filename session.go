package agentsession

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// ErrNoEntry is returned when an entry ID is not in the session.
var ErrNoEntry = errors.New("agentsession: no such entry")

// ErrDuplicateEntry is returned when an appended entry reuses an ID.
var ErrDuplicateEntry = errors.New("agentsession: duplicate entry id")

// Session is one session in memory: the header, the entries in file
// order, the tree they form and the current leaf. It is safe for
// concurrent use. Entries are shared, not copied, and must not be
// modified after they are appended.
type Session struct {
	mu        sync.RWMutex
	header    Header
	entries   []Entry
	byID      map[string]Entry
	children  map[string][]string
	leaf      string
	truncated *TruncatedLine
	now       func() time.Time
}

// New creates an empty session. Header fields left empty are filled:
// a UUIDv7 ID, the current time, this package's format and payload.
// Times the session assigns are in UTC so a file carries one offset
// however the writer's clock is configured; times the caller supplies
// are kept as given.
func New(h Header) *Session {
	s := &Session{now: utcNow}
	h.fill(s.now())
	s.header = h
	s.byID = map[string]Entry{}
	s.children = map[string][]string{}
	return s
}

// Header returns a copy of the header.
func (s *Session) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header
}

// ID returns the session ID.
func (s *Session) ID() string { return s.Header().ID }

// Leaf returns the ID of the entry the next append will name as its
// parent, or "" when the next append starts a new root.
func (s *Session) Leaf() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaf
}

// Len returns the number of entries.
func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Entry returns the entry with the given ID.
func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	return e, ok
}

// Entries returns the entries in file order. The slice is a copy; the
// entries are shared.
func (s *Session) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Entry(nil), s.entries...)
}

// Children returns the IDs of the entries whose parent is id, in file
// order. Pass "" for the roots.
func (s *Session) Children(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.children[id]...)
}

// Roots returns the IDs of the root entries in file order.
func (s *Session) Roots() []string { return s.Children("") }

// Leaves returns the IDs of the entries that have no children, in file
// order.
func (s *Session) Leaves() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, e := range s.entries {
		if len(s.children[e.Base().ID]) == 0 {
			out = append(out, e.Base().ID)
		}
	}
	return out
}

// Path returns the entries from the root to id, root first, or nil when
// id is not in the session.
func (s *Session) Path(id string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path(id)
}

func (s *Session) path(id string) []Entry {
	var rev []Entry
	for id != "" {
		e, ok := s.byID[id]
		if !ok {
			return nil
		}
		rev = append(rev, e)
		id = e.Base().Parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Branch moves the leaf to id, so the next append becomes a child of
// that entry.
func (s *Session) Branch(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNoEntry, id)
	}
	s.leaf = id
	return nil
}

// ResetLeaf clears the leaf, so the next append starts a new root.
func (s *Session) ResetLeaf() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaf = ""
}

// Append adds e to the tree and makes it the leaf. An empty ID is
// assigned; an empty Parent is set to the current leaf, so an explicit
// Parent branches in place; a zero Timestamp is set to now. The parent
// must exist and the ID must be new. On success e is owned by the
// session and must not be modified.
func (s *Session) Append(e Entry) (string, error) {
	if err := validateEntry(e); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := e.Base()
	if b.Parent == "" {
		b.Parent = s.leaf
	}
	if b.Parent != "" {
		if _, ok := s.byID[b.Parent]; !ok {
			return "", fmt.Errorf("%w: parent %s", ErrNoEntry, b.Parent)
		}
	}
	if b.ID == "" {
		b.ID = newUniqueEntryID(func(id string) bool { _, taken := s.byID[id]; return taken })
	} else if _, taken := s.byID[b.ID]; taken {
		return "", fmt.Errorf("%w: %s", ErrDuplicateEntry, b.ID)
	}
	if b.Timestamp.IsZero() {
		b.Timestamp = s.now()
	}
	if u, ok := e.(*UnknownEntry); ok {
		// The raw line is what gets written; keep it in step with the
		// envelope that was just filled in.
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return "", err
		}
		u.Raw = raw
	}
	s.add(e)
	s.leaf = b.ID
	return b.ID, nil
}

// validateEntry rejects an entry that could not be written: the checks
// MarshalEntry makes, applied before the tree changes.
func validateEntry(e Entry) error {
	switch v := e.(type) {
	case nil:
		return errors.New("agentsession: nil entry")
	case *ItemEntry:
		if v.Item == nil {
			return errors.New("agentsession: item entry has no item")
		}
	case *CompactionEntry:
		if v.Summary == nil {
			return errors.New("agentsession: compaction entry has no summary")
		}
	case *BranchSummaryEntry:
		if v.Summary == nil {
			return errors.New("agentsession: branch_summary entry has no summary")
		}
	case *UnknownEntry:
		if len(v.Raw) == 0 {
			return errors.New("agentsession: unknown entry has no raw bytes")
		}
	}
	return nil
}

// add links an already-validated entry into the indexes.
func (s *Session) add(e Entry) {
	b := e.Base()
	s.entries = append(s.entries, e)
	s.byID[b.ID] = e
	s.children[b.Parent] = append(s.children[b.Parent], b.ID)
}

// Truncated reports the final line of the file this session was read
// from when that line did not parse, or nil. A truncated line is what a
// crash mid-append leaves behind; the entries before it are intact.
func (s *Session) Truncated() *TruncatedLine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.truncated
}

// Labels returns the current label of every labelled entry: the last
// label entry per target wins, and a null label clears.
func (s *Session) Labels() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for _, e := range s.entries {
		if l, ok := e.(*LabelEntry); ok {
			if l.Label == nil {
				delete(out, l.Target)
			} else {
				out[l.Target] = *l.Label
			}
		}
	}
	return out
}

// Name returns the display name from the last info entry that set one.
func (s *Session) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name := ""
	for _, e := range s.entries {
		if i, ok := e.(*InfoEntry); ok && i.Name != "" {
			name = i.Name
		}
	}
	return name
}

// Compact builds the compaction entry for the current leaf: FirstKept
// names the earliest entry on the path that stays in context, summary
// is the item that replaces everything before it, and the settings
// checkpoint is taken from the context at the leaf. The entry is not
// appended; set TokensBefore or Usage if known, then append it through
// the store.
func (s *Session) Compact(firstKept string, summary openresponses.Item) (*CompactionEntry, error) {
	if summary == nil {
		return nil, errors.New("agentsession: compaction needs a summary item")
	}
	leaf := s.Leaf()
	if leaf == "" {
		return nil, errors.New("agentsession: no leaf to compact")
	}
	ctx, err := s.ContextAt(leaf)
	if err != nil {
		return nil, err
	}
	onPath := false
	for _, e := range s.Path(leaf) {
		if e.Base().ID == firstKept {
			onPath = true
			break
		}
	}
	if !onPath {
		return nil, fmt.Errorf("%w: first_kept %s is not on the path to %s", ErrNoEntry, firstKept, leaf)
	}
	return &CompactionEntry{FirstKept: firstKept, Summary: summary, Config: ctx.Settings}, nil
}

// SummarizeBranch builds the branch summary that carries context from
// the abandoned leaf from to the current leaf, where the new branch
// continues. Move the leaf with Branch first, then append the result
// through the store; its parent is set on append.
func (s *Session) SummarizeBranch(from string, summary openresponses.Item) (*BranchSummaryEntry, error) {
	if summary == nil {
		return nil, errors.New("agentsession: branch summary needs a summary item")
	}
	if _, ok := s.Entry(from); !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoEntry, from)
	}
	if s.Leaf() == "" {
		return nil, errors.New("agentsession: no leaf to continue from")
	}
	return &BranchSummaryEntry{From: from, Summary: summary}, nil
}

// utcNow is the session clock: the current time in UTC with the
// monotonic reading dropped, so what is stamped is what reaches disk.
func utcNow() time.Time { return time.Now().UTC().Round(0) }
