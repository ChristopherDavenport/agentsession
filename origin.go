package agentsession

import (
	"context"
	"errors"
	"fmt"
)

// MaxOriginDepth bounds the chain of forks [OriginDispatches] walks: a
// fork of a fork of a fork, each made at an entry of the prefix the one
// before carried. The header's parent_session is provenance the format
// does not validate, so a chain may fail to end where a store is
// damaged, and a walk that reaches the bound reports it rather than
// going on.
const MaxOriginDepth = 64

// ErrOriginChain is returned by [OriginDispatches] when the sessions a
// fork was made from do not end: parent_session names a session already
// on the walk, or the walk reaches [MaxOriginDepth].
var ErrOriginChain = errors.New("agentsession: the chain of forks does not end")

// OriginDispatches returns the dispatches for the call whose function
// call is the entry callEntry, and the session holding them. They are
// s's own, as [Session.Dispatches] has them, when s holds any. A call in
// a fork's prefix has its dispatches, if any, in the session the fork
// was made from, which the fork's file does not carry: for such a call
// with none in s, the session the header's ParentSession names is read
// through r and asked the same, and so up a chain of forks, each made
// at an entry on the prefix the one before carried, to [MaxOriginDepth].
// A dispatch found there is read as one off the path, which a rebase
// above a dispatch leaves: the call may have run, under that dispatch's
// key and with the arguments it handed over, and its output, when the
// origin holds one, is on that origin's branch. It returns nil, nil and
// no error when no session on the walk holds one, when the walk
// reaches a session that names no parent, when r does not hold the
// session named ([ErrNoSession]), or when r is nil, which reads s alone.
// Any other error reading an origin is returned, wrapping the store's,
// and so is [ErrOriginChain] for a chain that does not end.
//
// The store is passed, not read behind the caller's back: an Open or a
// Read of a fork does not go and read other sessions, so a reader that
// wants a prefix call's dispatches asks for them here, with the store
// it holds. [Call.State] reads a prefix call with no dispatch on its
// path as [CallUnknown] either way; what this adds is the dispatch
// itself.
func OriginDispatches(ctx context.Context, r Reader, s *Session, callEntry string) (*Session, []*DispatchEntry, error) {
	visited := map[string]bool{}
	for depth := 0; s != nil; depth++ {
		if ds := s.Dispatches(callEntry); len(ds) > 0 {
			return s, ds, nil
		}
		if !s.Prefix(callEntry) || r == nil {
			return nil, nil, nil
		}
		h := s.Header()
		if h.ParentSession == "" {
			return nil, nil, nil
		}
		visited[h.ID] = true
		if visited[h.ParentSession] {
			return nil, nil, fmt.Errorf("%w: %s names %s, already on the walk", ErrOriginChain, h.ID, h.ParentSession)
		}
		if depth >= MaxOriginDepth {
			return nil, nil, fmt.Errorf("%w: more than %d forks above %s", ErrOriginChain, MaxOriginDepth, h.ID)
		}
		origin, err := r.Read(ctx, h.ParentSession)
		if errors.Is(err, ErrNoSession) {
			return nil, nil, nil
		}
		if err != nil {
			return nil, nil, fmt.Errorf("agentsession: read origin %s of %s: %w", h.ParentSession, h.ID, err)
		}
		s = origin
	}
	return nil, nil, nil
}
