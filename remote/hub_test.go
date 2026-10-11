package remote

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// A client that does not keep up with its session's follower is cut
// with `overflow` rather than waited on, and the others go on.
func TestAClientThatFallsBehindIsCut(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(st, WithInsecureNoAuth(), WithFollowBuffer(1))
	defer h.Close()
	hb := h.acquire(s.ID())
	defer h.release(hb)
	if err := hb.wait(ctx); err != nil {
		t.Fatal(err)
	}
	slow, _, ok, err := hb.attach("")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	keeping, _, _, _ := hb.attach("")
	got := 0
	for i := range 3 {
		if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText(fmt.Sprint(i)))); err != nil {
			t.Fatal(err)
		}
		select {
		case <-keeping.ch:
			got++
		case <-time.After(10 * time.Second):
			t.Fatal("the client that keeps up was not sent the append")
		}
	}
	// The slow client's one slot holds the first append; the second
	// found it full.
	deadline := time.After(10 * time.Second)
	for n := 0; ; n++ {
		select {
		case _, open := <-slow.ch:
			if !open {
				if slow.err == nil || slow.err.Code != CodeOverflow {
					t.Fatalf("the slow client ended with %v, want overflow", slow.err)
				}
				if n != 1 {
					t.Errorf("the slow client was sent %d events before the cut, want 1", n)
				}
				if got != 3 {
					t.Errorf("the client that keeps up got %d appends, want 3", got)
				}
				return
			}
		case <-deadline:
			t.Fatal("the slow client was never cut")
		}
	}
}
