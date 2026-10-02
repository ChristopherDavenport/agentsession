// Package followtest is the helper the Follow tests of every store
// share: a follow ranged over in a goroutine, so a case can wait for
// the next change with a timeout and check that nothing else comes.
package followtest

import (
	"context"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Timeout bounds how long a case waits for a change that should
// arrive, or for the end of a follow that should end. A change that
// does arrive does so in a poll interval or two.
const Timeout = 10 * time.Second

// Tail ranges over a follow in a goroutine, so a case can wait for the
// next change with a timeout.
type Tail struct {
	t      *testing.T
	cancel context.CancelFunc
	ch     chan followed
	done   chan struct{}
	// ack lets the follow take its next step: it waits for one after
	// each change, since a change's session is extended in place by the
	// step after.
	ack     chan struct{}
	holding bool
	// last is the cursor of the last change next returned.
	last agentsession.Cursor
}

type followed struct {
	c   agentsession.Change
	err error
}

func Start(t *testing.T, f agentsession.Follower, id string, from agentsession.Cursor) *Tail {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &Tail{t: t, cancel: cancel, ch: make(chan followed), done: make(chan struct{}), ack: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer close(w.ch)
		for c, err := range f.Follow(ctx, id, from) {
			select {
			case w.ch <- followed{c, err}:
			case <-ctx.Done():
				return
			}
			select {
			case <-w.ack:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(w.Stop)
	return w
}

func (w *Tail) Stop() {
	w.cancel()
	<-w.done
}

// release lets the follow take its next step.
func (w *Tail) release() {
	if w.holding {
		w.holding = false
		select {
		case w.ack <- struct{}{}:
		case <-w.done:
		}
	}
}

// Next returns the next change, and fails the case when none comes.
func (w *Tail) Next() agentsession.Change {
	w.t.Helper()
	c, err, ok := w.NextOrEnd()
	if !ok {
		w.t.Fatal("the follow ended; want another change")
	}
	if err != nil {
		w.t.Fatalf("the follow failed: %v", err)
	}
	return c
}

// NextOrEnd returns the next change or error, and false when the
// follow has ended.
func (w *Tail) NextOrEnd() (agentsession.Change, error, bool) {
	w.t.Helper()
	w.release()
	select {
	case f, ok := <-w.ch:
		if ok {
			w.holding = true
		}
		if ok && f.err == nil {
			w.last = f.c.Cursor
		}
		return f.c, f.err, ok
	case <-time.After(Timeout):
		w.t.Fatal("no change arrived")
		return agentsession.Change{}, nil, false
	}
}

// NextKind returns the next change and checks its kind.
func (w *Tail) NextKind(k agentsession.ChangeKind) agentsession.Change {
	w.t.Helper()
	c := w.Next()
	if c.Kind != k {
		w.t.Fatalf("change %v, want %v", c.Kind, k)
	}
	return c
}

// Quiet fails when a change arrives within a short while.
func (w *Tail) Quiet() {
	w.t.Helper()
	w.release()
	select {
	case f, ok := <-w.ch:
		if ok {
			w.t.Fatalf("unexpected change %v (%v)", f.c.Kind, f.err)
		}
		w.t.Fatal("the follow ended")
	case <-time.After(300 * time.Millisecond):
	}
}

// End waits for the follow to end and returns its last error.
func (w *Tail) End() error {
	w.t.Helper()
	c, err, ok := w.NextOrEnd()
	if !ok {
		return nil
	}
	if err == nil {
		w.t.Fatalf("change %v, want the follow to end", c.Kind)
	}
	if _, _, ok := w.NextOrEnd(); ok {
		w.t.Fatal("the follow went on after an error")
	}
	return err
}

// Last is the cursor of the last change Next or NextOrEnd returned.
func (w *Tail) Last() agentsession.Cursor { return w.last }
