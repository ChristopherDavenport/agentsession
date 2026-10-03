package agentsession

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// ErrNoRef is returned by ResolveRef when the store holds no ref of the
// name.
var ErrNoRef = errors.New("agentsession: no such ref")

// ErrRefMoved is returned by UpdateRef when the ref does not hold the
// expected target. The error wraps it as a [RefMovedError], which
// carries the target the ref holds.
var ErrRefMoved = errors.New("agentsession: ref moved")

// ErrRefName is returned for a name or prefix RFC 0002 does not allow,
// and for a name that conflicts with a ref the store holds.
var ErrRefName = errors.New("agentsession: invalid ref name")

// ErrNoRefs is returned by the helpers that need refs when the store
// does not keep them.
var ErrNoRefs = errors.New("agentsession: store does not keep refs")

// RefMovedError is the error UpdateRef returns when the ref holds
// another target than the one expected. errors.Is(err, ErrRefMoved)
// holds, and errors.As finds the holder's target.
type RefMovedError struct {
	Name string
	// Current is the target the ref holds, the zero RefTarget when it
	// holds none.
	Current RefTarget
}

// Error implements error.
func (e *RefMovedError) Error() string {
	if e.Current.IsZero() {
		return fmt.Sprintf("%v: %s holds no target", ErrRefMoved, e.Name)
	}
	return fmt.Sprintf("%v: %s holds %s", ErrRefMoved, e.Name, e.Current)
}

// Unwrap returns ErrRefMoved.
func (e *RefMovedError) Unwrap() error { return ErrRefMoved }

// RefTarget is what a ref points to: a session, and optionally an
// entry of it that the ref pins. The zero RefTarget is "no ref", as an
// expected value and as a next one.
type RefTarget struct {
	Session string
	// Entry is the entry the ref pins, or "". It must be an entry the
	// session holds: its base, one of its own, or one on its prefix.
	Entry string
}

// IsZero reports whether t names nothing.
func (t RefTarget) IsZero() bool { return t == RefTarget{} }

// String renders the target as the session, then @ and the entry when
// one is pinned.
func (t RefTarget) String() string {
	if t.Entry == "" {
		return t.Session
	}
	return t.Session + "@" + t.Entry
}

// Ref is a name and the target it holds.
type Ref struct {
	Name   string
	Target RefTarget
}

// RefUpdate is one accepted update, as the ref log records it. Old is
// the zero RefTarget for a creation and New is for a deletion.
type RefUpdate struct {
	Name     string
	Old, New RefTarget
	Time     time.Time
	Reason   string
}

// RefStore is implemented by a store that keeps refs, as RFC 0002's
// refs section defines them. A store opened read-only resolves and
// lists, and reports [ErrReadOnly] from UpdateRef.
type RefStore interface {
	// ResolveRef returns the ref's target, or [ErrNoRef]. A ref whose
	// session is gone is dangling: ResolveRef returns its target
	// together with an error wrapping [ErrNoSession], so a caller can
	// tell which session it was and move the ref.
	ResolveRef(ctx context.Context, name string) (RefTarget, error)
	// UpdateRef moves the ref from expected to next, the zero RefTarget
	// meaning none: none as expected creates the ref if it is absent,
	// none as next deletes it. On a mismatch it returns [ErrRefMoved]
	// as a [*RefMovedError] carrying the target the ref holds. A next
	// whose session the store does not hold is [ErrNoSession], and one
	// whose entry that session does not hold is [ErrNoEntry]. An update
	// that changes nothing is not logged. reason is recorded in the ref
	// log and may be empty.
	UpdateRef(ctx context.Context, name string, expected, next RefTarget, reason string) error
	// ListRefs lists the refs whose name begins with prefix, in name
	// order. It does not check that the sessions exist.
	ListRefs(ctx context.Context, prefix string) iter.Seq2[Ref, error]
	// RefLog lists the ref's updates, newest first, and nothing for a
	// name never updated. It outlives the ref's deletion.
	RefLog(ctx context.Context, name string) iter.Seq2[RefUpdate, error]
}

// MaxRefName is the longest ref name, in bytes.
const MaxRefName = 200

func refNameChar(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// ValidRefName reports whether name is a ref name by RFC 0002's rules,
// which are about the name alone: segments of [A-Za-z0-9._-] joined by
// "/", none empty, "." or "..", at most [MaxRefName] bytes. A store
// checks what depends on the refs it holds with [RefConflict].
func ValidRefName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrRefName)
	}
	if len(name) > MaxRefName {
		return fmt.Errorf("%w: %d bytes, at most %d", ErrRefName, len(name), MaxRefName)
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "":
			return fmt.Errorf("%w: %q has an empty segment", ErrRefName, name)
		case ".", "..":
			return fmt.Errorf("%w: %q has a %q segment", ErrRefName, name, seg)
		}
		for i := 0; i < len(seg); i++ {
			if !refNameChar(seg[i]) {
				return fmt.Errorf("%w: %q has the character %q", ErrRefName, name, seg[i])
			}
		}
	}
	return nil
}

// ValidRefPrefix reports whether prefix can begin a ref name: the empty
// prefix, or characters a name allows and "/". It need not end at a
// segment boundary.
func ValidRefPrefix(prefix string) error {
	if len(prefix) > MaxRefName {
		return fmt.Errorf("%w: prefix of %d bytes, at most %d", ErrRefName, len(prefix), MaxRefName)
	}
	for i := 0; i < len(prefix); i++ {
		if c := prefix[i]; c != '/' && !refNameChar(c) {
			return fmt.Errorf("%w: prefix %q has the character %q", ErrRefName, prefix, c)
		}
	}
	return nil
}

// RefConflict reports whether creating name beside the refs in
// existing breaks RFC 0002's rules on names: name is an existing ref's
// name, or a prefix of one, or has one as a prefix, at a segment
// boundary (a ref under a ref), or some prefix of it at a segment
// boundary differs only in ASCII case from the same-length prefix of
// an existing name. It returns nil for a name that is itself in
// existing and spelled the same, which creates nothing new.
func RefConflict(name string, existing []string) error {
	segs := strings.Split(name, "/")
	for _, other := range existing {
		if other == name {
			continue
		}
		osegs := strings.Split(other, "/")
		n := min(len(segs), len(osegs))
		for i := 0; i < n; i++ {
			if segs[i] == osegs[i] {
				continue
			}
			if strings.EqualFold(segs[i], osegs[i]) {
				return fmt.Errorf("%w: %q differs only in case from the existing %q", ErrRefName, name, other)
			}
			break
		}
		if len(segs) != len(osegs) && strings.EqualFold(strings.Join(segs[:n], "/"), strings.Join(osegs[:n], "/")) {
			if len(segs) > len(osegs) {
				return fmt.Errorf("%w: %q is under the ref %q", ErrRefName, name, other)
			}
			return fmt.Errorf("%w: %q is a ref and %q is under it", ErrRefName, name, other)
		}
	}
	return nil
}

func refsOf(st Store) (RefStore, error) {
	rs, ok := st.(RefStore)
	if !ok {
		return nil, ErrNoRefs
	}
	return rs, nil
}

// SessionFor returns the session ref name points to, creating one from
// h and setting the ref when there is none. It is the answer to two
// harnesses that each create a session for one name: the ref is
// created by compare-and-swap, so one caller's session wins, and the
// other opens the winner's and deletes its own, which nothing else has
// seen. A dangling ref, whose session is gone, is replaced the same
// way.
//
// The session comes from Open, so a store that guards sessions against
// a second writing process reports [ErrSessionLocked] to a caller
// whose winner is held by another process.
func SessionFor(ctx context.Context, st Store, name string, h Header) (s *Session, err error) {
	rs, err := refsOf(st)
	if err != nil {
		return nil, err
	}
	// own is the session this call created, which nothing else has
	// seen. Whatever the call returns but own, it deletes.
	var own *Session
	defer func() {
		if own == nil || (s != nil && s == own) {
			return
		}
		id := own.ID()
		if derr := st.Delete(ctx, id); derr != nil && !errors.Is(derr, ErrNoSession) {
			s, err = nil, errors.Join(err, fmt.Errorf("agentsession: SessionFor %s: discarding %s: %w", name, id, derr))
		}
	}()
	for {
		var expected RefTarget
		cur, err := rs.ResolveRef(ctx, name)
		switch {
		case err == nil:
			s, oerr := st.Open(ctx, cur.Session)
			if errors.Is(oerr, ErrNoSession) {
				// Deleted since it was resolved: resolve again.
				continue
			}
			if oerr != nil {
				return nil, oerr
			}
			return s, nil
		case errors.Is(err, ErrNoRef):
		case errors.Is(err, ErrNoSession):
			expected = cur
		default:
			return nil, err
		}
		if own == nil {
			if own, err = st.Create(ctx, h); err != nil {
				return nil, err
			}
		}
		err = rs.UpdateRef(ctx, name, expected, RefTarget{Session: own.ID()}, "created")
		if err == nil {
			return own, nil
		}
		if !errors.Is(err, ErrRefMoved) {
			return nil, err
		}
		// Moved: resolve again and take what is there.
	}
}

// ContinueRef continues the session ref name points to, as [Continue]
// does, and moves the ref from the old session to the successor by
// compare-and-swap, so a named conversation that rolls over keeps its
// name. The ref's pinned entry, which names an entry of the old
// session, is not carried.
//
// When the ref moved while the continuation ran, the error is
// [ErrRefMoved] and the successor is returned with it: it exists, the
// old session is retired, and the caller decides where the name goes.
func ContinueRef(ctx context.Context, st Store, name string, summary openresponses.Item) (*Session, error) {
	rs, err := refsOf(st)
	if err != nil {
		return nil, err
	}
	cur, err := rs.ResolveRef(ctx, name)
	if err != nil {
		return nil, err
	}
	next, err := Continue(ctx, st, cur.Session, summary)
	if err != nil {
		return nil, err
	}
	if err := rs.UpdateRef(ctx, name, cur, RefTarget{Session: next.ID()}, "continued"); err != nil {
		return next, err
	}
	return next, nil
}

// ResolveCurrent returns the session that is current for name: the
// ref's target, followed along continued_in links to the end of the
// chain. moved reports that it followed at least one, which says the
// ref is behind, as after a writer that continued the session and
// stopped before it moved the ref. It stops at a cycle with an error.
// The store's ResolveRef stays literal; this is the caller's choice.
//
// It reads sessions through [Reader] when the store is one, and
// otherwise with Open.
func ResolveCurrent(ctx context.Context, st Store, name string) (id string, moved bool, err error) {
	rs, err := refsOf(st)
	if err != nil {
		return "", false, err
	}
	cur, err := rs.ResolveRef(ctx, name)
	if err != nil {
		return "", false, err
	}
	seen := map[string]bool{}
	id = cur.Session
	for {
		if seen[id] {
			return "", moved, fmt.Errorf("agentsession: ref %s: continued_in cycle at %s", name, id)
		}
		seen[id] = true
		var s *Session
		if r, ok := st.(Reader); ok {
			s, err = r.Read(ctx, id)
		} else {
			s, err = st.Open(ctx, id)
		}
		if err != nil {
			return "", moved, err
		}
		next := s.SupersededBy()
		if next == "" {
			return id, moved, nil
		}
		id, moved = next, true
	}
}

// refState is a store's refs and their logs, for MemoryStore.
type refState struct {
	refs map[string]RefTarget
	logs map[string][]RefUpdate
}

// ResolveRef implements [RefStore].
func (m *MemoryStore) ResolveRef(_ context.Context, name string) (RefTarget, error) {
	if err := ValidRefName(name); err != nil {
		return RefTarget{}, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.refs.refs[name]
	if !ok {
		return RefTarget{}, fmt.Errorf("%w: %s", ErrNoRef, name)
	}
	if _, ok := m.sessions[t.Session]; !ok {
		return t, fmt.Errorf("%w: %s, which ref %s names", ErrNoSession, t.Session, name)
	}
	return t, nil
}

// UpdateRef implements [RefStore].
func (m *MemoryStore) UpdateRef(_ context.Context, name string, expected, next RefTarget, reason string) error {
	if err := ValidRefName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	cur := m.refs.refs[name]
	if cur != expected {
		return &RefMovedError{Name: name, Current: cur}
	}
	if !next.IsZero() {
		s, ok := m.sessions[next.Session]
		if !ok {
			return fmt.Errorf("%w: %s", ErrNoSession, next.Session)
		}
		if next.Entry != "" {
			if _, ok := s.Entry(next.Entry); !ok {
				return fmt.Errorf("%w: %s in session %s", ErrNoEntry, next.Entry, next.Session)
			}
		}
	}
	if cur == next {
		return nil
	}
	if cur.IsZero() {
		names := make([]string, 0, len(m.refs.refs))
		for n := range m.refs.refs {
			names = append(names, n)
		}
		if err := RefConflict(name, names); err != nil {
			return err
		}
	}
	if next.IsZero() {
		delete(m.refs.refs, name)
	} else {
		m.refs.refs[name] = next
	}
	m.refs.logs[name] = append(m.refs.logs[name], RefUpdate{
		Name: name, Old: cur, New: next, Time: time.Now().UTC(), Reason: reason,
	})
	return nil
}

// ListRefs implements [RefStore].
func (m *MemoryStore) ListRefs(_ context.Context, prefix string) iter.Seq2[Ref, error] {
	if err := ValidRefPrefix(prefix); err != nil {
		return func(yield func(Ref, error) bool) { yield(Ref{}, err) }
	}
	m.mu.RLock()
	var out []Ref
	for n, t := range m.refs.refs {
		if strings.HasPrefix(n, prefix) {
			out = append(out, Ref{Name: n, Target: t})
		}
	}
	m.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return func(yield func(Ref, error) bool) {
		for _, r := range out {
			if !yield(r, nil) {
				return
			}
		}
	}
}

// RefLog implements [RefStore].
func (m *MemoryStore) RefLog(_ context.Context, name string) iter.Seq2[RefUpdate, error] {
	if err := ValidRefName(name); err != nil {
		return func(yield func(RefUpdate, error) bool) { yield(RefUpdate{}, err) }
	}
	m.mu.RLock()
	log := append([]RefUpdate(nil), m.refs.logs[name]...)
	m.mu.RUnlock()
	return func(yield func(RefUpdate, error) bool) {
		for i := len(log) - 1; i >= 0; i-- {
			if !yield(log[i], nil) {
				return
			}
		}
	}
}

var _ RefStore = (*MemoryStore)(nil)
