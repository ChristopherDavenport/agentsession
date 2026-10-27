package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testHead exercises RFC 0002's head compare-and-swap, which every store
// offers ([agentsession.HeadStore]): a move from the expected head to
// the base or an own entry, refused when the head is another, when the
// target is not held and when it is a leaf label, seen by the session
// the store shares, by a read and, for a store that persists, after a
// reopen; and a root appended to a session with a head, which is a
// branch that the compare-and-swap then moves the head onto.
func testHead(t *testing.T, opts Options) {
	t.Run("CompareAndSwap", func(t *testing.T) { headSwap(t, opts) })
	t.Run("Root", func(t *testing.T) { headRoot(t, opts) })
	t.Run("AtTheRoot", func(t *testing.T) { headAtTheRoot(t, opts) })
	if opts.Reopen != nil {
		t.Run("BranchKeepsHead", func(t *testing.T) { headBranchKept(t, opts) })
	}
	if opts.RedactedOnly != nil {
		t.Run("RedactedOnly", func(t *testing.T) { headRedactedOnly(t, opts) })
	}
}

// headRedactedOnly: a store set to hold only redacted entries offers the
// compare-and-swap as every store does. Redaction is a property of what
// was run over an entry's content, and the leaf label a head move writes
// carries none, so the store takes it, and still holds only redacted
// entries by its own check.
func headRedactedOnly(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.RedactedOnly(t)
	hs := headStore(t, st)
	orig := original(t, "orig-head")
	r := newRedactor(orig, "test.secret@1", agentsession.Header{})
	if _, err := st.Create(ctx, r.red.Header()); err != nil {
		t.Fatal(err)
	}
	appendAll(t, st, r.red.ID(), r.redact(t, orig))
	id := r.red.ID()
	head := headOf(t, st, id)
	root := r.red.Roots()[0]
	if head == root {
		t.Fatalf("the redacted session's head is its root %s", root)
	}
	if err := hs.SetHead(ctx, id, head, root); err != nil {
		t.Fatalf("SetHead in a store that holds only redacted entries: %v", err)
	}
	if got := headOf(t, st, id); got != root {
		t.Errorf("head after SetHead = %s, want %s", got, root)
	}
	if err := agentsession.CheckHoldsRedacted(ctx, st); err != nil {
		t.Errorf("the store holds a head move it calls unredacted: %v", err)
	}
}

// headBranchKept: a branch leaves the head where it is, and a store that
// persists keeps it there across a reopen, a root among the branches.
func headBranchKept(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	var ids []string
	for _, text := range []string{"a", "b", "c"} {
		e, err := st.Append(ctx, id, user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e)
	}
	head := ids[2]
	side := user("a branch off a")
	side.Base().Parent = ids[0]
	if _, err := st.Append(ctx, id, side); err != nil {
		t.Fatal(err)
	}
	root := agentsession.NewItemEntry(openresponses.UserText("a root"))
	if _, err := agentsession.New(agentsession.Header{}).Prepare(root); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, root); err != nil {
		t.Fatal(err)
	}
	if got := headOf(t, st, id); got != head {
		t.Errorf("head after two branches = %s, want %s", got, head)
	}
	if got := headOf(t, opts.Reopen(t, st), id); got != head {
		t.Errorf("head after two branches and a reopen = %s, want %s", got, head)
	}
}

func headStore(t *testing.T, st agentsession.Store) agentsession.HeadStore {
	t.Helper()
	hs, ok := st.(agentsession.HeadStore)
	if !ok {
		t.Fatalf("%T does not implement agentsession.HeadStore; RFC 0002 asks every store for the head compare-and-swap", st)
	}
	return hs
}

// headOf is the session's head as the store holds it: as a read sees it
// where the store reads, and as Open returns it otherwise.
func headOf(t *testing.T, st agentsession.Store, id string) string {
	t.Helper()
	if r, ok := st.(agentsession.Reader); ok {
		s, err := r.Read(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return s.Leaf()
	}
	s, err := st.Open(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return s.Leaf()
}

func headSwap(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	hs := headStore(t, st)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	var ids []string
	for _, text := range []string{"a", "b", "c"} {
		e, err := st.Append(ctx, id, user(text))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e)
	}
	a, b, c := ids[0], ids[1], ids[2]

	if err := hs.SetHead(ctx, id, b, a); !errors.Is(err, agentsession.ErrHeadMoved) {
		t.Errorf("SetHead from a head that is not the session's: %v, want ErrHeadMoved", err)
	}
	missing := "sha256:" + "00000000000000000000000000000000000000000000000000000000000000aa"
	if err := hs.SetHead(ctx, id, c, missing); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("SetHead to an entry the session lacks: %v, want ErrNoEntry", err)
	}
	if got := headOf(t, st, id); got != c {
		t.Fatalf("a refused move moved the head to %s, want %s", got, c)
	}

	if err := hs.SetHead(ctx, id, c, a); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	if got := headOf(t, st, id); got != a {
		t.Errorf("head after SetHead = %s, want %s", got, a)
	}
	if err := hs.SetHead(ctx, id, c, b); !errors.Is(err, agentsession.ErrHeadMoved) {
		t.Errorf("SetHead from the head before the move: %v, want ErrHeadMoved", err)
	}
	// The next append continues from where the head was moved.
	d, err := st.Append(ctx, id, user("d"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := live.Entry(d); !ok || e.Base().Parent != a {
		t.Errorf("the append after the move hangs from %v, want %s", e, a)
	}

	// A leaf label is never a head. Appending one moves the head to its
	// target, as RFC 0002's Head says.
	label, err := st.Append(ctx, id, agentsession.NewLabelEntry(b, agentsession.LeafLabel))
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.SetHead(ctx, id, b, label); !errors.Is(err, agentsession.ErrHeadTarget) {
		t.Errorf("SetHead to a leaf label: %v, want ErrHeadTarget", err)
	}
	if err := hs.SetHead(ctx, id, b, d); err != nil {
		t.Fatalf("SetHead after a leaf label: %v", err)
	}
	if opts.Reopen != nil {
		if got := headOf(t, opts.Reopen(t, st), id); got != d {
			t.Errorf("head after a reopen = %s, want %s: the move was not durable", got, d)
		}
	}
}

func headRoot(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	hs := headStore(t, st)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if _, err := st.Append(ctx, id, user("a")); err != nil {
		t.Fatal(err)
	}
	b, err := st.Append(ctx, id, user("b"))
	if err != nil {
		t.Fatal(err)
	}
	// A complete root: its ID set over a null parent, as a writer builds
	// one whose parent it means to be null.
	root := agentsession.NewItemEntry(openresponses.UserText("a new root"))
	if _, err := agentsession.New(agentsession.Header{}).Prepare(root); err != nil {
		t.Fatal(err)
	}
	if root.Parent != "" || root.ID == "" {
		t.Fatalf("the root was prepared with parent %q and id %q", root.Parent, root.ID)
	}
	want := root.ID
	var got string
	if w, ok := st.(agentsession.Writer); ok {
		r, err := w.Write(ctx, id, root)
		if err != nil {
			t.Fatalf("Write of a root: %v", err)
		}
		if r.Outcome != agentsession.Branched {
			t.Errorf("a root in a session with a head: %s, want branched", r.Outcome)
		}
		got = r.ID
	} else if got, err = st.Append(ctx, id, root); err != nil {
		t.Fatalf("Append of a root: %v", err)
	}
	if got != want {
		t.Fatalf("the root was appended as %s, want %s", got, want)
	}
	live, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := live.Entry(want); !ok || e.Base().Parent != "" {
		t.Fatalf("the session holds the root as %v", e)
	}
	if live.Leaf() != b {
		t.Errorf("a root branched and moved the head to %s, want %s", live.Leaf(), b)
	}
	// The writer that wants the head on the root moves it there.
	if err := hs.SetHead(ctx, id, b, want); err != nil {
		t.Fatalf("SetHead onto the root: %v", err)
	}
	if got := headOf(t, st, id); got != want {
		t.Errorf("head after SetHead = %s, want the root %s", got, want)
	}
	next, err := st.Append(ctx, id, user("under the root"))
	if err != nil {
		t.Fatal(err)
	}
	if live, err = st.Open(ctx, id); err != nil {
		t.Fatal(err)
	}
	if e, ok := live.Entry(next); !ok || e.Base().Parent != want {
		t.Errorf("the append after the move hangs from %v, want the root", e)
	}
}

// headAtTheRoot: the head may rest on the universal root, the empty
// context, "" in Go and null on the wire. The compare-and-swap moves it
// there and back, an append with a null parent then continues, a read
// and a reopen see the head where it was moved, and a session with a
// base has no head at the root.
func headAtTheRoot(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	hs := headStore(t, st)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	a, err := st.Append(ctx, id, user("a"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := st.Append(ctx, id, user("b"))
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.SetHead(ctx, id, b, ""); err != nil {
		t.Fatalf("SetHead to the root: %v", err)
	}
	if got := headOf(t, st, id); got != "" {
		t.Errorf("head after a move to the root = %s, want the root", got)
	}
	if opts.Reopen != nil {
		st = opts.Reopen(t, st)
		hs = headStore(t, st)
		if got := headOf(t, st, id); got != "" {
			t.Errorf("head at the root after a reopen = %s", got)
		}
	}
	if err := hs.SetHead(ctx, id, b, a); !errors.Is(err, agentsession.ErrHeadMoved) {
		t.Errorf("SetHead from b with the head at the root: %v, want ErrHeadMoved", err)
	}
	// A null parent continues from the root.
	c, err := st.Append(ctx, id, user("c, at the root"))
	if err != nil {
		t.Fatal(err)
	}
	live, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := live.Entry(c); !ok || e.Base().Parent != "" || live.Leaf() != c {
		t.Errorf("the append after the move: %v, leaf %s; want a first entry that is the head", e, live.Leaf())
	}
	if err := hs.SetHead(ctx, id, c, ""); err != nil {
		t.Fatalf("SetHead to the root again: %v", err)
	}
	if err := hs.SetHead(ctx, id, "", b); err != nil {
		t.Fatalf("SetHead from the root: %v", err)
	}
	if got := headOf(t, st, id); got != b {
		t.Errorf("head after a move from the root = %s, want %s", got, b)
	}
	if opts.Reopen != nil {
		st = opts.Reopen(t, st)
		hs = headStore(t, st)
		if got := headOf(t, st, id); got != b {
			t.Errorf("head after a move from the root and a reopen = %s, want %s", got, b)
		}
	}
	// A fork's entries all hang from its base, so its head never rests
	// on the root.
	fork, err := st.Create(ctx, agentsession.Header{Base: b, ParentSession: id})
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.SetHead(ctx, fork.ID(), headOf(t, st, fork.ID()), ""); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("SetHead to the root in a fork: %v, want ErrNoEntry", err)
	}
}
