package agentsession

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// failingReader is a store whose every read fails.
type failingReader struct{ err error }

func (r failingReader) Read(context.Context, string) (*Session, error) { return nil, r.err }

// originStore is a memory store holding an origin that made a call,
// handed it to its tool under key k1 and wrote its output, and a fork
// of it made at the call, so the call is in the fork's prefix and its
// dispatch in the origin alone (#185).
func originStore(t *testing.T) (st *MemoryStore, origin, fork *Session, call string) {
	t.Helper()
	ctx := context.Background()
	st = NewMemoryStore()
	var err error
	if origin, err = st.Create(ctx, Header{ID: "origin", Records: AllRecords}); err != nil {
		t.Fatal(err)
	}
	if call, err = origin.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fk", CallID: "call_k", Name: "deploy", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = origin.Append(NewDispatch("call_k", call).WithIdempotencyKey("k1")); err != nil {
		t.Fatal(err)
	}
	if _, err = origin.Append(NewItemEntry(openresponses.NewFunctionCallOutput("call_k", "deployed"))); err != nil {
		t.Fatal(err)
	}
	if fork, err = st.Create(ctx, Header{ID: "fork", ParentSession: "origin", Base: call, Records: AllRecords}); err != nil {
		t.Fatal(err)
	}
	return st, origin, fork, call
}

// TestOriginDispatches: a fork made at a call holds the call in its
// prefix and none of its dispatches, which stay in the origin; the fork
// reads the call unknown, and OriginDispatches finds the dispatch
// through the store, up a chain of forks, and says where it is. A fork
// whose origin the reader lacks, or that names none, reads as the fork
// alone; an origin that fails to read is an error (#185).
func TestOriginDispatches(t *testing.T) {
	ctx := context.Background()
	st, origin, fork, call := originStore(t)
	if ds := fork.Dispatches(call); len(ds) != 0 {
		t.Errorf("the fork's Dispatches = %d, want none: the dispatch is the origin's", len(ds))
	}
	pending, err := fork.PendingCalls(fork.Leaf())
	if err != nil || len(pending) != 1 || pending[0].State(fork.Header()) != CallUnknown {
		t.Fatalf("the fork's pending calls = %v, %v; want the prefix call, unknown", pending, err)
	}
	fork2, err := st.Create(ctx, Header{ID: "fork2", ParentSession: "fork", Base: call})
	if err != nil {
		t.Fatal(err)
	}
	diskGone := errors.New("disk gone")
	tests := []struct {
		name   string
		r      Reader
		s      *Session
		wantAt string // the session holding the dispatch, "" for none
		key    string
		err    error
	}{
		{name: "the origin's own", r: st, s: origin, wantAt: "origin", key: "k1"},
		{name: "a fork at the call", r: st, s: fork, wantAt: "origin", key: "k1"},
		{name: "a fork of the fork", r: st, s: fork2, wantAt: "origin", key: "k1"},
		{name: "no reader", r: nil, s: fork},
		{name: "the origin not held", r: NewMemoryStore(), s: fork},
		{name: "the origin fails to read", r: failingReader{diskGone}, s: fork, err: diskGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			at, ds, err := OriginDispatches(ctx, tt.r, tt.s, call)
			if tt.err != nil {
				if !errors.Is(err, tt.err) {
					t.Fatalf("err = %v, want one wrapping %v", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tt.wantAt == "" {
				if at != nil || ds != nil {
					t.Fatalf("found %d dispatches in %v, want none", len(ds), at)
				}
				return
			}
			if at == nil || at.ID() != tt.wantAt || len(ds) != 1 || ds[0].IdempotencyKey != tt.key || ds[0].CallID != "call_k" {
				t.Fatalf("found %d dispatches in %v, want one under %s in %s", len(ds), at, tt.key, tt.wantAt)
			}
			// The origin's branch through the dispatch holds the call's
			// output, which is what a reader wanting it reads next.
			calls, err := at.Calls(at.Leaf())
			if err != nil || len(calls) != 1 || calls[0].Output == nil || calls[0].IdempotencyKey() != tt.key {
				t.Errorf("the origin's call = %v, %v; want it completed under %s", calls, err, tt.key)
			}
		})
	}
	// A call the fork made itself is read from the fork alone, whatever
	// the origin holds.
	own, err := fork.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fo", CallID: "call_o", Name: "deploy", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if at, ds, err := OriginDispatches(ctx, st, fork, own); at != nil || ds != nil || err != nil {
		t.Errorf("the fork's own call: %v, %d, %v; want none", at, len(ds), err)
	}
}

// TestOriginDispatchesChain: parent_session is provenance the format does
// not validate, so a fork may name itself or a descendant; the walk
// reports the cycle rather than following it (#185).
func TestOriginDispatchesChain(t *testing.T) {
	ctx := context.Background()
	st, _, _, call := originStore(t)
	// A fork naming itself as its origin.
	if _, err := st.Create(ctx, Header{ID: "self", ParentSession: "self", Base: call}); err != nil {
		t.Fatal(err)
	}
	self, err := st.Open(ctx, "self")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := OriginDispatches(ctx, st, self, call); !errors.Is(err, ErrOriginChain) {
		t.Errorf("a fork naming itself: %v, want ErrOriginChain", err)
	}
}
