package storetest

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// followTimeout bounds how long a case waits for a change that should
// arrive, or for the end of a follow that should end. A change that
// does arrive does so in a poll interval or two.
const followTimeout = 10 * time.Second

// tail ranges over a follow in a goroutine, so a case can wait for the
// next change with a timeout.
type tail struct {
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

func startTail(t *testing.T, f agentsession.Follower, id string, from agentsession.Cursor) *tail {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	w := &tail{t: t, cancel: cancel, ch: make(chan followed), done: make(chan struct{}), ack: make(chan struct{})}
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
	t.Cleanup(w.stop)
	return w
}

func (w *tail) stop() {
	w.cancel()
	<-w.done
}

// release lets the follow take its next step.
func (w *tail) release() {
	if w.holding {
		w.holding = false
		select {
		case w.ack <- struct{}{}:
		case <-w.done:
		}
	}
}

// next returns the next change, and fails the case when none comes.
func (w *tail) next() agentsession.Change {
	w.t.Helper()
	c, err, ok := w.nextOrEnd()
	if !ok {
		w.t.Fatal("the follow ended; want another change")
	}
	if err != nil {
		w.t.Fatalf("the follow failed: %v", err)
	}
	return c
}

// nextOrEnd returns the next change or error, and false when the
// follow has ended.
func (w *tail) nextOrEnd() (agentsession.Change, error, bool) {
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
	case <-time.After(followTimeout):
		w.t.Fatal("no change arrived")
		return agentsession.Change{}, nil, false
	}
}

// nextKind returns the next change and checks its kind.
func (w *tail) nextKind(k agentsession.ChangeKind) agentsession.Change {
	w.t.Helper()
	c := w.next()
	if c.Kind != k {
		w.t.Fatalf("change %v, want %v", c.Kind, k)
	}
	return c
}

// quiet fails when a change arrives within a short while.
func (w *tail) quiet() {
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

// end waits for the follow to end and returns its last error.
func (w *tail) end() error {
	w.t.Helper()
	c, err, ok := w.nextOrEnd()
	if !ok {
		return nil
	}
	if err == nil {
		w.t.Fatalf("change %v, want the follow to end", c.Kind)
	}
	if _, _, ok := w.nextOrEnd(); ok {
		w.t.Fatal("the follow went on after an error")
	}
	return err
}

func user(text string) agentsession.Entry {
	return agentsession.NewItemEntry(openresponses.UserText(text))
}

// testFollow exercises [agentsession.Follower]: the snapshot, the
// appends that follow it, a branch and a recorded head, a second store
// on the same storage, a resume, a resume from a cursor the log no
// longer holds, a delete, a cancel and a reader that stops early.
func testFollow(t *testing.T, opts Options) {
	st := opts.New(t)
	if _, ok := st.(agentsession.Follower); !ok {
		t.Skip("the store does not implement agentsession.Follower")
	}
	t.Run("SnapshotThenAppends", func(t *testing.T) { followAppends(t, opts) })
	t.Run("BranchThenHead", func(t *testing.T) { followBranch(t, opts) })
	t.Run("SecondStore", func(t *testing.T) { followSecond(t, opts) })
	t.Run("Resume", func(t *testing.T) { followResume(t, opts) })
	t.Run("ResumeStale", func(t *testing.T) { followStale(t, opts) })
	t.Run("Delete", func(t *testing.T) { followDelete(t, opts) })
	t.Run("DeleteThenCreate", func(t *testing.T) { followRecreate(t, opts) })
	t.Run("Cancel", func(t *testing.T) { followCancel(t, opts) })
	t.Run("Break", func(t *testing.T) { followBreak(t, opts) })
	t.Run("Missing", func(t *testing.T) { followMissing(t, opts) })
	t.Run("Fork", func(t *testing.T) { followFork(t, opts) })
	t.Run("Burst", func(t *testing.T) { followBurst(t, opts) })
}

// seeded makes a session of n entries and returns the store, the
// writer's session and the entries' IDs.
func seeded(t *testing.T, opts Options, n int) (agentsession.Store, *agentsession.Session, []string) {
	t.Helper()
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range n {
		id, err := st.Append(ctx, s.ID(), user(string(rune('a'+i))))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return st, s, ids
}

func followAppends(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 2)
	w := startTail(t, st.(agentsession.Follower), s.ID(), "")
	snap := w.nextKind(agentsession.Snapshot)
	if got := entryIDs(snap.Session); !reflect.DeepEqual(got, ids) {
		t.Fatalf("snapshot holds %v, want %v", got, ids)
	}
	if snap.Cursor == "" {
		t.Error("the snapshot has no cursor")
	}
	if snap.Session == s {
		t.Fatal("the follower's session is the writer's")
	}
	if snap.Session.Leaf() != ids[1] {
		t.Errorf("snapshot leaf %s, want %s", snap.Session.Leaf(), ids[1])
	}
	w.quiet()
	prev := snap.Cursor
	for _, text := range []string{"c", "d", "e"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		c := w.nextKind(agentsession.Appended)
		if c.ID != id || c.Entry == nil || c.Entry.Base().ID != id {
			t.Errorf("appended %q, want %s", c.ID, id)
		}
		if got := entryIDs(c.Session); !reflect.DeepEqual(got, ids) {
			t.Errorf("session after the change holds %v, want %v", got, ids)
		}
		if c.Cursor == "" || c.Cursor == prev {
			t.Errorf("cursor %q does not move on from %q", c.Cursor, prev)
		}
		prev = c.Cursor
		if c.Session.Leaf() != id {
			t.Errorf("leaf %s after the append, want %s", c.Session.Leaf(), id)
		}
	}
	w.quiet()
	// What the follower holds is its own: the writer's session did not
	// take its entries, nor it the writer's moves.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	w.quiet()
}

func followBranch(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 3)
	w := startTail(t, st.(agentsession.Follower), s.ID(), "")
	w.nextKind(agentsession.Snapshot)

	// A branch: an append under an entry that is not the leaf. It is
	// seen in log order, with its own parent, and the leaf stays.
	side := user("side")
	side.Base().Parent = ids[0]
	sid, err := st.Append(ctx, s.ID(), side)
	if err != nil {
		t.Fatal(err)
	}
	c := w.nextKind(agentsession.Appended)
	if c.ID != sid || c.Entry.Base().Parent != ids[0] {
		t.Errorf("branch entry %s under %s, want %s under %s", c.ID, c.Entry.Base().Parent, sid, ids[0])
	}
	if c.Session.Leaf() != ids[2] {
		t.Errorf("leaf %s after a branch, want %s", c.Session.Leaf(), ids[2])
	}
	if p := c.Session.Path(sid); len(p) != 2 {
		t.Errorf("path to the branch has %d entries, want 2", len(p))
	}
	w.quiet()

	// A recorded head: the leaf label is an entry, then the head it
	// records. A leaf moved and not recorded is not seen.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	w.quiet()
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	mid, err := st.Append(ctx, s.ID(), mark)
	if err != nil {
		t.Fatal(err)
	}
	c = w.nextKind(agentsession.Appended)
	if c.ID != mid {
		t.Errorf("appended %s, want the label %s", c.ID, mid)
	}
	h := w.nextKind(agentsession.Head)
	if h.Leaf != ids[0] || h.Session.Leaf() != ids[0] {
		t.Errorf("head %s (session leaf %s), want %s", h.Leaf, h.Session.Leaf(), ids[0])
	}
	if h.Cursor == "" {
		t.Error("the head has no cursor")
	}
	w.quiet()
}

// second returns the store a second follower runs on: read-only when
// the store has that form, else a second writing store, else nil.
func second(t *testing.T, opts Options, st agentsession.Store) agentsession.Store {
	t.Helper()
	var o agentsession.Store
	switch {
	case opts.ReadOnly != nil:
		o = opts.ReadOnly(t, st)
	case opts.Second != nil:
		o = opts.Second(t, st)
	default:
		return nil
	}
	if _, ok := o.(agentsession.Follower); !ok {
		t.Fatal("the second store does not implement agentsession.Follower")
	}
	return o
}

func followSecond(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 2)
	other := second(t, opts, st)
	if other == nil {
		t.Skip("the store has no second handle on its storage")
	}
	w := startTail(t, other.(agentsession.Follower), s.ID(), "")
	snap := w.nextKind(agentsession.Snapshot)
	if got := entryIDs(snap.Session); !reflect.DeepEqual(got, ids) {
		t.Fatalf("snapshot holds %v, want %v", got, ids)
	}
	for _, text := range []string{"c", "d"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		c := w.nextKind(agentsession.Appended)
		if c.ID != id {
			t.Errorf("appended %s, want %s", c.ID, id)
		}
	}
	// The writer still holds the session: the follower took nothing.
	if _, err := st.Append(ctx, s.ID(), user("e")); err != nil {
		t.Errorf("the writer's append beside a follower: %v", err)
	}
	w.nextKind(agentsession.Appended)
	// A branch and a head cross too.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), mark); err != nil {
		t.Fatal(err)
	}
	w.nextKind(agentsession.Appended)
	if h := w.nextKind(agentsession.Head); h.Leaf != ids[0] {
		t.Errorf("head %s, want %s", h.Leaf, ids[0])
	}
}

func followResume(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 2)
	f := st.(agentsession.Follower)
	w := startTail(t, f, s.ID(), "")
	w.nextKind(agentsession.Snapshot)
	id3, _ := st.Append(ctx, s.ID(), user("c"))
	mid := w.nextKind(agentsession.Appended)
	w.stop()
	ids = append(ids, id3)

	// Two more land while nobody follows.
	for _, text := range []string{"d", "e"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	r := startTail(t, f, s.ID(), mid.Cursor)
	for i := 3; i < 5; i++ {
		c := r.nextKind(agentsession.Appended)
		if c.ID != ids[i] {
			t.Errorf("resumed change %d is %s, want %s", i, c.ID, ids[i])
		}
		if got := entryIDs(c.Session); !reflect.DeepEqual(got, ids[:i+1]) {
			t.Errorf("session at change %d holds %v, want %v", i, got, ids[:i+1])
		}
	}
	r.quiet()
	id6, _ := st.Append(ctx, s.ID(), user("f"))
	if c := r.nextKind(agentsession.Appended); c.ID != id6 {
		t.Errorf("appended %s, want %s", c.ID, id6)
	}

	// A cursor at the very end waits for what comes next.
	end := r.last
	r.stop()
	e := startTail(t, f, s.ID(), end)
	e.quiet()
	id7, _ := st.Append(ctx, s.ID(), user("g"))
	if c := e.nextKind(agentsession.Appended); c.ID != id7 {
		t.Errorf("appended %s, want %s", c.ID, id7)
	}
}

func followStale(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 3)
	f := st.(agentsession.Follower)
	// A cursor that is not the store's.
	w := startTail(t, f, s.ID(), "not-a-cursor")
	c := w.nextKind(agentsession.Reset)
	if c.Session.Len() != 3 || c.Cursor == "" {
		t.Errorf("reset holds %d entries at %q, want the 3 the store holds", c.Session.Len(), c.Cursor)
	}
	w.stop()

	// A cursor into a log the session has since replaced.
	snap := startTail(t, f, s.ID(), "")
	old := snap.nextKind(agentsession.Snapshot)
	snap.stop()
	created := old.Session.Header().CreatedAt
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	again, err := st.Create(ctx, agentsession.Header{ID: s.ID(), CreatedAt: created.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	nid, err := st.Append(ctx, again.ID(), user("new"))
	if err != nil {
		t.Fatal(err)
	}
	r := startTail(t, f, s.ID(), old.Cursor)
	c = r.nextKind(agentsession.Reset)
	if got := entryIDs(c.Session); !reflect.DeepEqual(got, []string{nid}) {
		t.Errorf("reset holds %v, want the new session's [%s]", got, nid)
	}
}

func followDelete(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 1)
	w := startTail(t, st.(agentsession.Follower), s.ID(), "")
	w.nextKind(agentsession.Snapshot)
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err := w.end(); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("follow of a deleted session ended with %v, want ErrNoSession", err)
	}
}

// followRecreate: a session created again under the ID is another
// session, which a follower of the first never continues into, even
// when the new one is written before the follower looks.
func followRecreate(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 2)
	w := startTail(t, st.(agentsession.Follower), s.ID(), "")
	snap := w.nextKind(agentsession.Snapshot)
	created := snap.Session.Header().CreatedAt
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	again, err := st.Create(ctx, agentsession.Header{ID: s.ID(), CreatedAt: created.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"x", "y", "z"} {
		if _, err := st.Append(ctx, again.ID(), user(text)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.end(); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("follow across a delete and create ended with %v, want ErrNoSession", err)
	}
}

func followCancel(t *testing.T, opts Options) {
	st, s, _ := seeded(t, opts, 1)
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan error, 1)
	got := make(chan agentsession.Change)
	go func() {
		var last error
		for c, err := range st.(agentsession.Follower).Follow(ctx, s.ID(), "") {
			if err != nil {
				last = err
				break
			}
			got <- c
		}
		ch <- last
	}()
	select {
	case <-got:
	case <-time.After(followTimeout):
		t.Fatal("no snapshot")
	}
	cancel()
	select {
	case err := <-ch:
		if err != nil {
			t.Errorf("a cancelled follow ended with %v, want no error", err)
		}
	case <-time.After(followTimeout):
		t.Fatal("the follow did not end when its context was cancelled")
	}
}

// followBreak: a consumer that stops ranging mid-iteration releases
// everything the follow held. It is checked by the goroutine count and
// by a following that still works.
func followBreak(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 2)
	f := st.(agentsession.Follower)
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 5 {
		next, stop := iter.Pull2(f.Follow(ctx, s.ID(), ""))
		if c, err, ok := next(); !ok || err != nil || c.Kind != agentsession.Snapshot {
			t.Fatalf("first change %v (%v, %v)", c.Kind, err, ok)
		}
		stop()
	}
	for range 5 {
		for c, err := range f.Follow(ctx, s.ID(), "") {
			if err != nil || c.Kind != agentsession.Snapshot {
				t.Fatalf("first change %v (%v)", c.Kind, err)
			}
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		buf := make([]byte, 1<<16)
		t.Errorf("%d goroutines after stopped follows, %d before:\n%s", n, before, buf[:runtime.Stack(buf, true)])
	}
	// The session is still writable and followable.
	if _, err := st.Append(ctx, s.ID(), user("after")); err != nil {
		t.Errorf("append after stopped follows: %v", err)
	}
	w := startTail(t, f, s.ID(), "")
	if c := w.nextKind(agentsession.Snapshot); c.Session.Len() != 3 {
		t.Errorf("snapshot holds %d entries, want 3", c.Session.Len())
	}
}

func followMissing(t *testing.T, opts Options) {
	st := opts.New(t)
	w := startTail(t, st.(agentsession.Follower), "no-such-session", "")
	if err := w.end(); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("follow of a missing session: %v, want ErrNoSession", err)
	}
}

// followFork: a fork's prefix is in the snapshot, and its appends are
// its own entries.
func followFork(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 3)
	f, err := st.Create(ctx, agentsession.Header{ID: "fork-follow", ParentSession: s.ID(), Base: ids[1]})
	if err != nil {
		t.Fatal(err)
	}
	w := startTail(t, st.(agentsession.Follower), f.ID(), "")
	snap := w.nextKind(agentsession.Snapshot)
	if snap.Session.Len() != 2 || snap.Session.Leaf() != ids[1] {
		t.Fatalf("fork snapshot holds %d entries at %s, want the 2 on the path to %s", snap.Session.Len(), snap.Session.Leaf(), ids[1])
	}
	own, err := st.Append(ctx, f.ID(), user("own"))
	if err != nil {
		t.Fatal(err)
	}
	c := w.nextKind(agentsession.Appended)
	if c.ID != own || c.Session.Len() != 3 || c.Entry.Base().Parent != ids[1] {
		t.Errorf("fork append %s under %s (%d entries), want %s under %s", c.ID, c.Entry.Base().Parent, c.Session.Len(), own, ids[1])
	}
}

// followBurst: a writer appends as fast as it can while a follower
// reads, and the follower sees every entry once, in order. It is the
// case to run under the race detector.
func followBurst(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 1)
	other := second(t, opts, st)
	if other == nil {
		other = st
	}
	w := startTail(t, other.(agentsession.Follower), s.ID(), "")
	w.nextKind(agentsession.Snapshot)
	const n = 60
	var want []string
	done := make(chan error, 1)
	go func() {
		for i := range n {
			id, err := st.Append(ctx, s.ID(), user("burst "+string(rune('A'+i%26))+string(rune('a'+i/26))))
			if err != nil {
				done <- err
				return
			}
			want = append(want, id)
		}
		done <- nil
	}()
	var got []string
	for len(got) < n {
		c := w.nextKind(agentsession.Appended)
		got = append(got, c.ID)
		if c.Session.Len() != len(ids)+len(got) {
			t.Fatalf("session holds %d entries after change %d", c.Session.Len(), len(got))
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("follower saw %v, want %v", got, want)
	}
	w.quiet()
}
