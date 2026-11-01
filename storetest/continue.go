package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testContinueOnce holds a store to RFC 0002's rule that a session
// continues once: the store refuses a second continued_in link naming
// another successor, under the lock it appends under, so two writers
// that continue one session at once land one link between them, and
// the loser's successor is deleted.
func testContinueOnce(t *testing.T, opts Options) {
	t.Run("SecondLink", func(t *testing.T) { continueSecondLink(t, opts) })
	t.Run("Concurrent", func(t *testing.T) { continueConcurrent(t, opts) })
}

func continueSecondLink(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), user("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewLinkEntry(agentsession.RelContinuedIn, "successor-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewLinkEntry(agentsession.RelContinuedIn, "successor-2")); !errors.Is(err, agentsession.ErrSuperseded) {
		t.Errorf("a second continued_in link: %v, want ErrSuperseded", err)
	}
}

func continueConcurrent(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	for round := range 4 {
		s, err := st.Create(ctx, agentsession.Header{})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, s.ID(), &agentsession.ConfigEntry{Model: "gpt-5"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText("hello"))); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		var nexts [2]*agentsession.Session
		var errs [2]error
		for i := range 2 {
			wg.Go(func() { nexts[i], errs[i] = agentsession.Continue(ctx, st, s.ID(), nil) })
		}
		wg.Wait()
		won := 0
		var winner string
		for i := range 2 {
			switch {
			case errs[i] == nil:
				won++
				winner = nexts[i].ID()
			case !errors.Is(errs[i], agentsession.ErrSuperseded):
				t.Fatalf("round %d: Continue: %v", round, errs[i])
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d continuations landed, want 1", round, won)
		}
		r, ok := st.(agentsession.Reader)
		if !ok {
			continue
		}
		old, err := r.Read(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		links := 0
		for _, e := range old.Entries() {
			if l, ok := e.(*agentsession.LinkEntry); ok && l.Rel == agentsession.RelContinuedIn {
				links++
			}
		}
		if links != 1 || old.SupersededBy() != winner {
			t.Errorf("round %d: %d continued_in links, superseded by %s; want one, to %s", round, links, old.SupersededBy(), winner)
		}
		n := 0
		for sum, err := range st.List(ctx, agentsession.ListFilter{ParentSession: s.ID()}) {
			if err != nil {
				t.Fatal(err)
			}
			if sum.Header.ID != winner {
				t.Errorf("round %d: the loser's successor %s is left", round, sum.Header.ID)
			}
			n++
		}
		if n != 1 {
			t.Errorf("round %d: %d successors listed, want 1", round, n)
		}
	}
}
