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
	"github.com/ChristopherDavenport/agentsession/internal/followtest"
	"github.com/ChristopherDavenport/openresponses"
)

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
	t.Run("LeafAsRead", func(t *testing.T) { followLeafAsRead(t, opts) })
}

// sameLeafAsRead fails when the follower's leaf after c is not the leaf
// a Read of the session gives, which is the leaf the follower promises.
func sameLeafAsRead(t *testing.T, st agentsession.Store, id string, c agentsession.Change, when string) {
	t.Helper()
	r, ok := st.(agentsession.Reader)
	if !ok {
		return
	}
	got, err := r.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if c.Session.Leaf() != got.Leaf() {
		t.Errorf("follower leaf %s %s, Read gives %s", c.Session.Leaf(), when, got.Leaf())
	}
}

// followLeafAsRead follows a writer that moves its leaf with
// Session.Branch, which records nothing, and appends under it, then
// marks a leaf and appends off the marked branch. After every change
// the follower's leaf is the one Read gives: the newest entry while no
// mark is in force, and the mark's newest descendant once one is.
func followLeafAsRead(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 3)
	w := followtest.Start(t, st.(agentsession.Follower), s.ID(), "")
	w.NextKind(agentsession.Snapshot)
	step := func(what string, e agentsession.Entry) string {
		t.Helper()
		id, err := st.Append(ctx, s.ID(), e)
		if err != nil {
			t.Fatal(err)
		}
		for {
			c := w.Next()
			if c.Kind == agentsession.Head {
				// A head the store logged before the entry, as cas
				// logs a writer's Branch; Read already has the entry,
				// so the leaves are compared after it.
				continue
			}
			if c.Kind != agentsession.Appended || c.ID != id {
				t.Fatalf("%s: change %v %s, want the append %s", what, c.Kind, c.ID, id)
			}
			// A store that records a head after the entry yields it
			// next; compare once the follower is quiet.
			if next, ok := w.Poll(); ok {
				if next.Kind != agentsession.Head {
					t.Fatalf("%s: change %v after the append, want a head or nothing", what, next.Kind)
				}
				c = next
			}
			sameLeafAsRead(t, st, s.ID(), c, what)
			return id
		}
	}
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	a := step("after a writer's Branch and an append", user("a"))
	step("after a second append on the branch", user("b"))
	other := user("other")
	other.Base().Parent = ids[2]
	step("after an append back on the first branch", other)
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	step("after a leaf label", mark)
	off := user("off")
	off.Base().Parent = ids[1]
	step("after an append off the marked branch", off)
	under := user("under")
	under.Base().Parent = a
	step("after an append under the mark", under)
	w.Quiet()
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
	w := followtest.Start(t, st.(agentsession.Follower), s.ID(), "")
	snap := w.NextKind(agentsession.Snapshot)
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
	w.Quiet()
	prev := snap.Cursor
	for _, text := range []string{"c", "d", "e"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
		c := w.NextKind(agentsession.Appended)
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
	w.Quiet()
	// What the follower holds is its own: the writer's session did not
	// take its entries, nor it the writer's moves.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	w.Quiet()
}

func followBranch(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 3)
	w := followtest.Start(t, st.(agentsession.Follower), s.ID(), "")
	w.NextKind(agentsession.Snapshot)

	// A branch: an append under an entry that is not the leaf. It is
	// seen in log order, with its own parent, and the follower's leaf is
	// the one Read gives.
	side := user("side")
	side.Base().Parent = ids[0]
	sid, err := st.Append(ctx, s.ID(), side)
	if err != nil {
		t.Fatal(err)
	}
	c := w.NextKind(agentsession.Appended)
	if c.ID != sid || c.Entry.Base().Parent != ids[0] {
		t.Errorf("branch entry %s under %s, want %s under %s", c.ID, c.Entry.Base().Parent, sid, ids[0])
	}
	sameLeafAsRead(t, st, s.ID(), c, "after a branch")
	if p := c.Session.Path(sid); len(p) != 2 {
		t.Errorf("path to the branch has %d entries, want 2", len(p))
	}
	w.Quiet()

	// A recorded head: the leaf label is an entry, then the head it
	// records. A leaf moved and not recorded is not seen.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	w.Quiet()
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	mid, err := st.Append(ctx, s.ID(), mark)
	if err != nil {
		t.Fatal(err)
	}
	h := labelAndHead(t, w, mid, ids[0])
	if h.Session.Leaf() != ids[0] {
		t.Errorf("session leaf %s after the head, want %s", h.Session.Leaf(), ids[0])
	}
	if h.Cursor == "" {
		t.Error("the head has no cursor")
	}
	w.Quiet()
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
	w := followtest.Start(t, other.(agentsession.Follower), s.ID(), "")
	snap := w.NextKind(agentsession.Snapshot)
	if got := entryIDs(snap.Session); !reflect.DeepEqual(got, ids) {
		t.Fatalf("snapshot holds %v, want %v", got, ids)
	}
	for _, text := range []string{"c", "d"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		c := w.NextKind(agentsession.Appended)
		if c.ID != id {
			t.Errorf("appended %s, want %s", c.ID, id)
		}
	}
	// The writer still holds the session: the follower took nothing.
	if _, err := st.Append(ctx, s.ID(), user("e")); err != nil {
		t.Errorf("the writer's append beside a follower: %v", err)
	}
	w.NextKind(agentsession.Appended)
	// A branch and a head cross too.
	if err := s.Branch(ids[0]); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	mid, err := st.Append(ctx, s.ID(), mark)
	if err != nil {
		t.Fatal(err)
	}
	labelAndHead(t, w, mid, ids[0])
}

func followResume(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, ids := seeded(t, opts, 2)
	f := st.(agentsession.Follower)
	w := followtest.Start(t, f, s.ID(), "")
	w.NextKind(agentsession.Snapshot)
	id3, _ := st.Append(ctx, s.ID(), user("c"))
	mid := w.NextKind(agentsession.Appended)
	w.Stop()
	ids = append(ids, id3)

	// Two more land while nobody follows.
	for _, text := range []string{"d", "e"} {
		id, err := st.Append(ctx, s.ID(), user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	r := followtest.Start(t, f, s.ID(), mid.Cursor)
	for i := 3; i < 5; i++ {
		c := r.NextKind(agentsession.Appended)
		if c.ID != ids[i] {
			t.Errorf("resumed change %d is %s, want %s", i, c.ID, ids[i])
		}
		if got := entryIDs(c.Session); !reflect.DeepEqual(got, ids[:i+1]) {
			t.Errorf("session at change %d holds %v, want %v", i, got, ids[:i+1])
		}
	}
	r.Quiet()
	id6, _ := st.Append(ctx, s.ID(), user("f"))
	if c := r.NextKind(agentsession.Appended); c.ID != id6 {
		t.Errorf("appended %s, want %s", c.ID, id6)
	}

	// A cursor at the very end waits for what comes next.
	end := r.Last()
	r.Stop()
	e := followtest.Start(t, f, s.ID(), end)
	e.Quiet()
	id7, _ := st.Append(ctx, s.ID(), user("g"))
	if c := e.NextKind(agentsession.Appended); c.ID != id7 {
		t.Errorf("appended %s, want %s", c.ID, id7)
	}
}

func followStale(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 3)
	f := st.(agentsession.Follower)
	// A cursor that is not the store's.
	w := followtest.Start(t, f, s.ID(), "not-a-cursor")
	c := w.NextKind(agentsession.Reset)
	if c.Session.Len() != 3 || c.Cursor == "" {
		t.Errorf("reset holds %d entries at %q, want the 3 the store holds", c.Session.Len(), c.Cursor)
	}
	w.Stop()

	// A cursor into a log the session has since replaced.
	snap := followtest.Start(t, f, s.ID(), "")
	old := snap.NextKind(agentsession.Snapshot)
	snap.Stop()
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
	r := followtest.Start(t, f, s.ID(), old.Cursor)
	c = r.NextKind(agentsession.Reset)
	if got := entryIDs(c.Session); !reflect.DeepEqual(got, []string{nid}) {
		t.Errorf("reset holds %v, want the new session's [%s]", got, nid)
	}
}

func followDelete(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 1)
	w := followtest.Start(t, st.(agentsession.Follower), s.ID(), "")
	w.NextKind(agentsession.Snapshot)
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err := w.End(); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("follow of a deleted session ended with %v, want ErrNoSession", err)
	}
}

// followRecreate: a session created again under the ID is another
// session, which a follower of the first never continues into, even
// when the new one is written before the follower looks.
func followRecreate(t *testing.T, opts Options) {
	ctx := context.Background()
	st, s, _ := seeded(t, opts, 2)
	w := followtest.Start(t, st.(agentsession.Follower), s.ID(), "")
	snap := w.NextKind(agentsession.Snapshot)
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
	if err := w.End(); !errors.Is(err, agentsession.ErrNoSession) {
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
	case <-time.After(followtest.Timeout):
		t.Fatal("no snapshot")
	}
	cancel()
	select {
	case err := <-ch:
		if err != nil {
			t.Errorf("a cancelled follow ended with %v, want no error", err)
		}
	case <-time.After(followtest.Timeout):
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
	w := followtest.Start(t, f, s.ID(), "")
	if c := w.NextKind(agentsession.Snapshot); c.Session.Len() != 3 {
		t.Errorf("snapshot holds %d entries, want 3", c.Session.Len())
	}
}

func followMissing(t *testing.T, opts Options) {
	st := opts.New(t)
	w := followtest.Start(t, st.(agentsession.Follower), "no-such-session", "")
	if err := w.End(); !errors.Is(err, agentsession.ErrNoSession) {
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
	w := followtest.Start(t, st.(agentsession.Follower), f.ID(), "")
	snap := w.NextKind(agentsession.Snapshot)
	if snap.Session.Len() != 2 || snap.Session.Leaf() != ids[1] {
		t.Fatalf("fork snapshot holds %d entries at %s, want the 2 on the path to %s", snap.Session.Len(), snap.Session.Leaf(), ids[1])
	}
	own, err := st.Append(ctx, f.ID(), user("own"))
	if err != nil {
		t.Fatal(err)
	}
	c := w.NextKind(agentsession.Appended)
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
	w := followtest.Start(t, other.(agentsession.Follower), s.ID(), "")
	w.NextKind(agentsession.Snapshot)
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
		c := w.NextKind(agentsession.Appended)
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
	w.Quiet()
}

// labelAndHead takes the two changes a recorded head makes, in the
// order the store has them: the leaf label's append, and the head it
// records, once. A store whose log records the head a writer moved
// before the label has the head first. It returns the head change.
func labelAndHead(t *testing.T, w *followtest.Tail, label, leaf string) agentsession.Change {
	t.Helper()
	var head agentsession.Change
	var appended, heads int
	for range 2 {
		c := w.Next()
		switch c.Kind {
		case agentsession.Appended:
			appended++
			if c.ID != label {
				t.Errorf("appended %s, want the label %s", c.ID, label)
			}
		case agentsession.Head:
			heads++
			head = c
			if c.Leaf != leaf {
				t.Errorf("head %s, want %s", c.Leaf, leaf)
			}
		default:
			t.Fatalf("change %v, want the label's append and its head", c.Kind)
		}
	}
	if appended != 1 || heads != 1 {
		t.Fatalf("%d appends and %d heads, want one each", appended, heads)
	}
	return head
}
