// Package storetest is the conformance suite for [agentsession.Store]
// implementations. Every store in this repository runs it; a store
// elsewhere can too.
package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// Options configure a run.
type Options struct {
	// New returns a fresh, empty store.
	New func(t *testing.T) agentsession.Store
	// Reopen returns a second handle on the same underlying storage as
	// s, for stores that persist. It is nil for a store that does not.
	Reopen func(t *testing.T, s agentsession.Store) agentsession.Store
	// Second returns another writing store on the same storage as s,
	// open beside it, as another process's would be: one that is
	// refused a session s holds. It is nil for a store that is not
	// shared. The Read case uses it to show that Read takes no hold.
	Second func(t *testing.T, s agentsession.Store) agentsession.Store
	// ReadOnly returns a store on the same storage as s opened
	// read-only, as another process's would be. It is nil for a store
	// that has no read-only form; the Follow cases use Second then, and
	// skip the second-store case when that is nil too.
	ReadOnly func(t *testing.T, s agentsession.Store) agentsession.Store
}

// Run exercises a store through the whole interface.
func Run(t *testing.T, opts Options) {
	t.Helper()
	if opts.New == nil {
		t.Fatal("storetest: Options.New is required")
	}
	t.Run("CreateAndOpen", func(t *testing.T) { testCreateAndOpen(t, opts) })
	t.Run("Append", func(t *testing.T) { testAppend(t, opts) })
	t.Run("HeldAppend", func(t *testing.T) { testHeldAppend(t, opts) })
	t.Run("ReadLine", func(t *testing.T) { testReadLine(t, opts) })
	t.Run("List", func(t *testing.T) { testList(t, opts) })
	t.Run("Continue", func(t *testing.T) { testContinue(t, opts) })
	t.Run("Delete", func(t *testing.T) { testDelete(t, opts) })
	t.Run("Fork", func(t *testing.T) { testFork(t, opts) })
	t.Run("ForkPrefix", func(t *testing.T) { testForkPrefix(t, opts) })
	t.Run("Read", func(t *testing.T) { testRead(t, opts) })
	t.Run("Follow", func(t *testing.T) { testFollow(t, opts) })
	if opts.Reopen != nil {
		t.Run("Persistence", func(t *testing.T) { testPersistence(t, opts) })
		t.Run("DurableLeaf", func(t *testing.T) { testDurableLeaf(t, opts) })
		t.Run("Convergence", func(t *testing.T) { testConvergence(t, opts) })
	}
}

// testRead reads sessions through [agentsession.Reader], for a store
// that implements it: one the store is writing, which Read copies and
// leaves held, one nobody holds, which Read leaves free, and one that is
// not there.
func testRead(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	r, ok := st.(agentsession.Reader)
	if !ok {
		t.Skip("the store does not implement agentsession.Reader")
	}
	if _, err := r.Read(ctx, "no-such-session"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Read of a missing session: %v, want ErrNoSession", err)
	}
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	var ids []string
	for _, text := range []string{"root", "a", "b"} {
		e, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText(text)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e)
	}
	root, leaf := ids[0], ids[2]

	// Read while this store writes the session: a copy of it, not the
	// session itself.
	snap, err := r.Read(ctx, id)
	if err != nil {
		t.Fatalf("Read of a session the store is writing: %v", err)
	}
	if snap == s {
		t.Fatal("Read returned the session the store writes through")
	}
	if got := entryIDs(snap); !reflect.DeepEqual(got, ids) {
		t.Errorf("Read holds %v, want %v", got, ids)
	}
	if snap.Leaf() != leaf || snap.ID() != id {
		t.Errorf("Read: session %s at leaf %s, want %s at %s", snap.ID(), snap.Leaf(), id, leaf)
	}
	// Its leaf is its own, both ways.
	if err := snap.Branch(root); err != nil {
		t.Fatal(err)
	}
	snap.ResetLeaf()
	if s.Leaf() != leaf {
		t.Errorf("moving the copy's leaf moved the writer's to %q", s.Leaf())
	}
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	if snap.Leaf() != "" {
		t.Errorf("moving the writer's leaf moved the copy's to %q", snap.Leaf())
	}
	// A leaf the writer moved and has not recorded is not read.
	if moved, err := r.Read(ctx, id); err != nil || moved.Leaf() != leaf {
		t.Errorf("Read after an unrecorded Branch: leaf %v (%v), want the recorded %s", leafOf(moved), err, leaf)
	}
	if err := s.Branch(leaf); err != nil {
		t.Fatal(err)
	}
	// The writer appends on, and the copy does not follow.
	next, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("c")))
	if err != nil {
		t.Fatalf("Append after Read: %v", err)
	}
	if e, _ := s.Entry(next); e == nil || e.Base().Parent != leaf {
		t.Errorf("the append after Read is not under the writer's leaf %s", leaf)
	}
	if snap.Len() != 3 {
		t.Errorf("the copy holds %d entries after the writer's append, want 3", snap.Len())
	}
	if again, err := r.Read(ctx, id); err != nil || again.Len() != 4 {
		t.Errorf("a second Read: %v, want the 4 entries the store holds", err)
	}
	if opts.Second == nil {
		return
	}

	// Read left the session held: another process is refused it, and
	// reads it all the same, which takes it from no one.
	other := opts.Second(t, st)
	if _, err := other.Open(ctx, id); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Errorf("another store's Open of the session after Read: %v, want ErrSessionLocked", err)
	}
	or, ok := other.(agentsession.Reader)
	if !ok {
		t.Fatal("the second store does not implement agentsession.Reader")
	}
	if got, err := or.Read(ctx, id); err != nil || got.Len() != 4 {
		t.Errorf("another store's Read of a held session: %v", err)
	}
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("d"))); err != nil {
		t.Errorf("the writer's Append after another store's Read: %v", err)
	}
	if opts.Reopen == nil {
		return
	}

	// A session nobody holds: Read takes no hold on it, so another
	// process can open it for writing after.
	st = opts.Reopen(t, st)
	r = st.(agentsession.Reader)
	got, err := r.Read(ctx, id)
	if err != nil {
		t.Fatalf("Read of a session nobody holds: %v", err)
	}
	if got.Len() != 5 {
		t.Errorf("Read of a session nobody holds: %d entries, want 5", got.Len())
	}
	other = opts.Second(t, st)
	if _, err := other.Open(ctx, id); err != nil {
		t.Fatalf("another store's Open after Read: %v", err)
	}
	if _, err := other.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("e"))); err != nil {
		t.Errorf("another store's Append after Read: %v", err)
	}
	if _, err := st.Open(ctx, id); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Errorf("Open of a session another store holds: %v, want ErrSessionLocked", err)
	}
	if got, err := r.Read(ctx, id); err != nil || got.Len() != 6 {
		t.Errorf("Read of a session another store holds: %v", err)
	}
}

func leafOf(s *agentsession.Session) string {
	if s == nil {
		return ""
	}
	return s.Leaf()
}

func entryIDs(s *agentsession.Session) []string {
	var out []string
	for _, e := range s.Entries() {
		out = append(out, e.Base().ID)
	}
	return out
}

// testHeldAppend appends an entry the session already holds: the same
// type, content and parent at the same ts, which is the same entry. RFC
// 0002 makes the second append a no-op the store reports as such, so a
// writer that retries an append whose acknowledgement it lost gets the
// entry's id and no error, and nothing is stored twice.
func testHeldAppend(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	root, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("root")))
	if err != nil {
		t.Fatal(err)
	}
	first := agentsession.NewItemEntry(openresponses.UserText("once"))
	first.Parent = root
	want, err := st.Append(ctx, id, first)
	if err != nil {
		t.Fatal(err)
	}
	again := agentsession.NewItemEntry(openresponses.UserText("once"))
	again.Parent = root
	again.Timestamp = first.Timestamp
	got, err := st.Append(ctx, id, again)
	if err != nil {
		t.Fatalf("appending an entry the session holds: %v", err)
	}
	if got != want {
		t.Errorf("the second append's id %s, want %s", got, want)
	}
	if n := s.Len(); n != 2 {
		t.Errorf("session holds %d entries, want 2", n)
	}
	if opts.Reopen == nil {
		return
	}
	st2 := opts.Reopen(t, st)
	s2, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Len() != 2 || len(s2.Repeated()) != 0 {
		t.Errorf("after reopening: %d entries, repeated %v; want 2 and none", s2.Len(), s2.Repeated())
	}
}

// testReadLine appends entries read from lines whose members the typed
// fields write back otherwise — a link with no session, a label with no
// target, where the fields add "" — with the ids the lines hash to. The
// id is the hash of the line, so a store writes the line back as read:
// the append keeps the id, and so does every read of what it stored.
func testReadLine(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	root, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("root")))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i, body := range []string{`"type":"link","rel":"subagent"`, `"type":"label","label":"x"`} {
		line := fmt.Sprintf(`{%s,"parent":%q,"ts":"2026-09-17T16:00:0%dZ"}`, body, root, i+1)
		want, _, err := agentsession.EntryHashes([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		e, err := agentsession.UnmarshalEntry([]byte(`{"id":"` + want + `",` + line[1:]))
		if err != nil {
			t.Fatal(err)
		}
		got, err := st.Append(ctx, id, e)
		if err != nil || got != want {
			t.Fatalf("Append of a read %s = %s, %v; want %s", body, got, err, want)
		}
		ids = append(ids, want)
	}
	check := func(when string, s *agentsession.Session) {
		t.Helper()
		for _, want := range ids {
			e, ok := s.Entry(want)
			if !ok {
				t.Errorf("%s: no entry %s", when, want)
				continue
			}
			data, err := agentsession.MarshalEntry(e)
			if err != nil {
				t.Fatal(err)
			}
			if got, _, err := agentsession.EntryHashes(data); err != nil || got != want {
				t.Errorf("%s: entry %s writes back as %s, %v", when, want, got, err)
			}
		}
	}
	again, err := st.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	check("after Open", again)
	if opts.Reopen == nil {
		return
	}
	again, err = opts.Reopen(t, st).Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	check("after reopening", again)
}

// testConvergence appends an entry that converges a branch of this
// session and the leaf of another, and reopens the store. The
// references are provenance: they survive the round trip, they are
// sorted however the caller supplied them, and they stay out of the
// context and off the path — so a store that dropped them would lose
// which leaf of a subagent answered, and one that walked them would
// put an abandoned branch's work back into the prompt.
func testConvergence(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if _, err := st.Append(ctx, id, &agentsession.ConfigEntry{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	kept, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("kept")))
	if err != nil {
		t.Fatal(err)
	}
	abandoned, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("abandoned")))
	if err != nil {
		t.Fatal(err)
	}
	join := agentsession.NewItemEntry(openresponses.UserText("join"))
	join.Parent = kept
	join.Parents = []agentsession.EntryRef{
		{Session: "01995b2a-0000-7000-8000-0000000000ff", Entry: "child-leaf"},
		{Entry: abandoned},
	}
	joinID, err := st.Append(ctx, id, join)
	if err != nil {
		t.Fatal(err)
	}

	again, err := opts.Reopen(t, st).Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after reopen: %v", err)
	}
	e, ok := again.Entry(joinID)
	if !ok {
		t.Fatalf("the join is gone after a reopen")
	}
	want := []agentsession.EntryRef{
		{Entry: abandoned},
		{Session: "01995b2a-0000-7000-8000-0000000000ff", Entry: "child-leaf"},
	}
	if got := e.Base().Parents; !reflect.DeepEqual(got, want) {
		t.Errorf("parents after reopen = %+v, want %+v", got, want)
	}
	if p := e.Base().Parent; p != kept {
		t.Errorf("parent after reopen = %s, want %s", p, kept)
	}
	c, err := again.ContextAt(joinID)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 2 { // kept and the join; never the converged branch
		t.Errorf("context at the join has %d items, want 2: %+v", len(c.Items), c.Items)
	}
	for _, pe := range again.Path(joinID) {
		if pe.Base().ID == abandoned {
			t.Error("the converged branch is on the path to the join")
		}
	}
}

// testDurableLeaf marks a branch, writes on it, and reopens the store.
// The mark names the branch that is live, not a fixed entry, so the
// reopened session resumes where the branch was written to. A store that
// rewinds to the mark loses every entry appended after it from the
// context and from every path-derived query.
func testDurableLeaf(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	first, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(first); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, mark); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("c"))); err != nil {
		t.Fatal(err)
	}
	last, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.AssistantText("d")))
	if err != nil {
		t.Fatal(err)
	}

	st2 := opts.Reopen(t, st)
	again, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after reopen: %v", err)
	}
	if again.Leaf() != last {
		t.Errorf("leaf after reopen = %s, want %s (the mark is %s)", again.Leaf(), last, first)
	}
	c, err := again.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Items) != 3 {
		t.Errorf("context after reopen = %d items, want 3", len(c.Items))
	}
}

// testFork creates sessions whose header names a base. Create opens
// such a session with the path to the base and refuses a base it cannot
// honour, so the failure lands at the call that made it and never at
// the first append; the fork's appends reach neither the origin nor its
// file, and with a persistent store both read back as they were.
func testFork(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	o, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	origin := o.ID()
	var ids []string
	for _, e := range []agentsession.Entry{
		&agentsession.ConfigEntry{Model: "m"},
		agentsession.NewItemEntry(openresponses.UserText("q")),
		agentsession.NewItemEntry(openresponses.AssistantText("a")),
		agentsession.NewItemEntry(openresponses.UserText("later")),
	} {
		id, err := st.Append(ctx, origin, e)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	base := ids[2]
	f, err := st.Create(ctx, agentsession.Header{ID: "fork-1", ParentSession: origin, Base: base})
	if err != nil {
		t.Fatalf("Create with a base: %v", err)
	}
	if h := f.Header(); h.Base != base || h.ParentSession != origin {
		t.Errorf("fork header base %q parent %q", h.Base, h.ParentSession)
	}
	if f.Leaf() != base || f.Len() != 3 || !f.Prefix(base) || !f.Prefix(ids[0]) {
		t.Errorf("fork opens with leaf %s, %d entries; want the base and the 3 on its path", f.Leaf(), f.Len())
	}
	own, err := st.Append(ctx, "fork-1", agentsession.NewItemEntry(openresponses.UserText("instead")))
	if err != nil {
		t.Fatalf("fork's first append: %v", err)
	}
	if e, _ := f.Entry(own); e.Base().Parent != base || f.Leaf() != own || f.Prefix(own) {
		t.Errorf("own entry parent %s, leaf %s", e.Base().Parent, f.Leaf())
	}
	above := agentsession.NewItemEntry(openresponses.UserText("above"))
	above.Parent = ids[0]
	if _, err := st.Append(ctx, "fork-1", above); !errors.Is(err, agentsession.ErrBaseRule) || !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("append above the base = %v, want ErrBaseRule, and ErrNoEntry under it", err)
	}
	// A session with a base has one root: ResetLeaf and an append is a
	// second root, which the rule refuses, and the leaf is unchanged.
	f.ResetLeaf()
	if _, err := st.Append(ctx, "fork-1", agentsession.NewItemEntry(openresponses.UserText("a second root"))); !errors.Is(err, agentsession.ErrBaseRule) {
		t.Errorf("append a second root to a fork = %v, want ErrBaseRule", err)
	}
	if err := f.Branch(own); err != nil {
		t.Fatal(err)
	}
	// A header that names no parent session finds the holder; one on a
	// fork's prefix is the origin's entry wherever it is found.
	f2, err := st.Create(ctx, agentsession.Header{ID: "fork-2", Base: ids[1]})
	if err != nil {
		t.Fatalf("Create with a base and no parent session: %v", err)
	}
	if f2.Len() != 2 || f2.Leaf() != ids[1] {
		t.Errorf("fork-2 opens with %d entries, leaf %s", f2.Len(), f2.Leaf())
	}
	f3, err := st.Create(ctx, agentsession.Header{ID: "fork-3", ParentSession: "fork-1", Base: ids[1]})
	if err != nil {
		t.Fatalf("Create at a fork's prefix entry: %v", err)
	}
	if f3.Len() != 2 || f3.Leaf() != ids[1] {
		t.Errorf("fork-3 opens with %d entries, leaf %s", f3.Len(), f3.Leaf())
	}
	if _, err := st.Append(ctx, "fork-3", agentsession.NewItemEntry(openresponses.UserText("again"))); err != nil {
		t.Fatalf("fork-3's first append: %v", err)
	}
	// Refusals come from Create.
	if _, err := st.Create(ctx, agentsession.Header{ID: "fork-x", ParentSession: origin, Base: "sha256:" + strings.Repeat("0", 64)}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("Create with a base not held = %v, want ErrNoEntry", err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "fork-y", ParentSession: origin, Base: base, Media: agentsession.MediaSidecar}); err == nil {
		t.Error("Create with a media form other than the origin's succeeded")
	}
	label, err := st.Append(ctx, origin, agentsession.NewLabelEntry(ids[3], agentsession.LeafLabel))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "fork-z", ParentSession: origin, Base: label}); err == nil {
		t.Error("Create at a leaf label succeeded")
	}
	for _, id := range []string{"fork-x", "fork-y", "fork-z"} {
		if _, err := st.Open(ctx, id); !errors.Is(err, agentsession.ErrNoSession) {
			t.Errorf("a refused Create left %s: Open = %v", id, err)
		}
	}
	// The origin saw none of it.
	if o.Len() != 5 || o.Leaf() != ids[3] || o.Header().Base != "" {
		t.Errorf("origin changed: %d entries, leaf %s", o.Len(), o.Leaf())
	}
	if opts.Reopen == nil {
		return
	}
	st2 := opts.Reopen(t, st)
	again, err := st2.Open(ctx, "fork-1")
	if err != nil {
		t.Fatalf("reopen the fork: %v", err)
	}
	if again.Header().Base != base || again.Len() != 4 || again.Leaf() != own || !again.Prefix(base) || again.Prefix(own) {
		t.Errorf("fork read back: base %s, %d entries, leaf %s", again.Header().Base, again.Len(), again.Leaf())
	}
	if _, err := st2.Append(ctx, "fork-1", agentsession.NewItemEntry(openresponses.UserText("more"))); err != nil {
		t.Errorf("append to the reopened fork: %v", err)
	}
	// A fork made from a session this handle has not opened reads it
	// without claiming it.
	if _, err := st2.Create(ctx, agentsession.Header{ID: "fork-4", Base: ids[0]}); err != nil {
		t.Fatalf("Create a fork after reopen: %v", err)
	}
	ro, err := st2.Open(ctx, origin)
	if err != nil {
		t.Fatalf("reopen the origin: %v", err)
	}
	if ro.Len() != 5 || ro.Header().Base != "" {
		t.Errorf("origin read back with %d entries", ro.Len())
	}
	if _, err := st2.Append(ctx, origin, agentsession.NewItemEntry(openresponses.UserText("origin goes on"))); err != nil {
		t.Errorf("append to the origin after forking it: %v", err)
	}
}

// testForkPrefix: a fork takes its origin's payload profile, since the
// prefix was written under it, and Create refuses a prefix that could
// not stand as a file of its own: one converging an entry of the origin
// off the path, which the fork would not hold.
func testForkPrefix(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	const payload = "openresponses/2020-01-01"
	if _, err := st.Create(ctx, agentsession.Header{ID: "o", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	app := func(e agentsession.Entry) string {
		t.Helper()
		id, err := st.Append(ctx, "o", e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	app(&agentsession.ConfigEntry{Model: "m"})
	u := app(agentsession.NewItemEntry(openresponses.UserText("q")))
	a := app(agentsession.NewItemEntry(openresponses.AssistantText("a")))
	b := agentsession.NewItemEntry(openresponses.AssistantText("b"))
	b.Parent = u
	bID := app(b)
	j := agentsession.NewItemEntry(openresponses.UserText("join"))
	j.Parent = bID
	j.Parents = []agentsession.EntryRef{{Entry: a}}
	jID := app(j)

	f, err := st.Create(ctx, agentsession.Header{ID: "f", ParentSession: "o", Base: bID})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Header().Payload; got != payload {
		t.Errorf("fork payload = %s, want the origin's %s", got, payload)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "g", ParentSession: "o", Base: jID}); !errors.Is(err, agentsession.ErrBadConvergence) {
		t.Errorf("Create at an entry converging one off its path = %v, want ErrBadConvergence", err)
	}
	if _, err := st.Open(ctx, "g"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a refused Create left g: Open = %v", err)
	}
	if opts.Reopen == nil {
		return
	}
	again, err := opts.Reopen(t, st).Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if got := again.Header().Payload; got != payload {
		t.Errorf("fork payload read back = %s", got)
	}
}

func testCreateAndOpen(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{CWD: "/p", Harness: &agentsession.Harness{Name: "test"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	h := s.Header()
	if h.ID == "" || h.Format != agentsession.Format || h.Payload != agentsession.Payload || h.CreatedAt.IsZero() {
		t.Errorf("Create did not fill the header: %+v", h)
	}
	opened, err := st.Open(ctx, h.ID)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if !sameHeader(opened.Header(), h) {
		t.Errorf("Open header = %+v, want %+v", opened.Header(), h)
	}
	if _, err := st.Open(ctx, "missing"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open(missing) = %v, want ErrNoSession", err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: h.ID}); !errors.Is(err, agentsession.ErrSessionExists) {
		t.Errorf("Create(existing) = %v, want ErrSessionExists", err)
	}
	// A supplied ID is kept.
	s2, err := st.Create(ctx, agentsession.Header{ID: "given-id"})
	if err != nil {
		t.Fatal(err)
	}
	if s2.ID() != "given-id" {
		t.Errorf("ID = %q", s2.ID())
	}
}

func testAppend(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	cfgID, err := st.Append(ctx, id, &agentsession.ConfigEntry{Model: "m"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if cfgID == "" {
		t.Fatal("Append returned no id")
	}
	userID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	// The session handed out by Create sees the appends.
	if s.Len() != 2 || s.Leaf() != userID {
		t.Errorf("session after Append: len %d leaf %s", s.Len(), s.Leaf())
	}
	e, ok := s.Entry(userID)
	if !ok || e.Base().Parent != cfgID {
		t.Errorf("entry %s = %+v", userID, e)
	}
	// Leaf moves on the shared session steer later appends.
	if err := s.Branch(cfgID); err != nil {
		t.Fatal(err)
	}
	altID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("other")))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Children(cfgID); len(got) != 2 || got[1] != altID {
		t.Errorf("children of config = %v", got)
	}
	s.ResetLeaf()
	rootID, err := st.Append(ctx, id, &agentsession.InfoEntry{Name: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Roots(); len(got) != 2 || got[1] != rootID {
		t.Errorf("roots = %v", got)
	}
	// An extension entry goes through unchanged.
	raw := `{"type":"acme:note","text":"x"}`
	extID, err := st.Append(ctx, id, &agentsession.UnknownEntry{Raw: []byte(raw)})
	if err != nil {
		t.Fatalf("Append unknown: %v", err)
	}
	if e, ok := s.Entry(extID); !ok || e.EntryType() != "acme:note" {
		t.Errorf("unknown entry = %+v", e)
	}
	// Errors are reported.
	if _, err := st.Append(ctx, "missing", &agentsession.InfoEntry{}); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Append(missing) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.InfoEntry{EntryBase: agentsession.EntryBase{ID: cfgID}}); !errors.Is(err, agentsession.ErrBadID) {
		t.Errorf("Append(duplicate) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.InfoEntry{EntryBase: agentsession.EntryBase{Parent: "ghost"}}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("Append(bad parent) = %v", err)
	}
	if _, err := st.Append(ctx, id, &agentsession.ItemEntry{}); err == nil {
		t.Error("Append(item without item) succeeded")
	}
	// A failed append leaves the tree untouched.
	if s.Len() != 5 {
		t.Errorf("len after failed appends = %d, want 5", s.Len())
	}
	// Context builds from the store's session.
	if err := s.Branch(userID); err != nil {
		t.Fatal(err)
	}
	c, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Model != "m" || len(c.Items) != 1 {
		t.Errorf("context = %+v", c)
	}
}

func testList(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	base := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	for i, cwd := range []string{"/a", "/b", "/a"} {
		h := agentsession.Header{ID: []string{"one", "two", "three"}[i], CWD: cwd, CreatedAt: base.Add(time.Duration(i) * time.Hour)}
		switch i {
		case 0:
			// A routine's session: its harness, the platform and user it
			// wrote into the header, with the members spelled as a writer
			// might.
			h.Harness = &agentsession.Harness{Name: "nightly", Version: "2"}
			h.Extra = map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`), "acme:user": json.RawMessage(`{"name": "u1", "id": 7}`)}
		case 1:
			h.Harness = &agentsession.Harness{Name: "cli"}
			h.Extra = map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`)}
		case 2:
			// A subsession a call of one spawned, under the same harness.
			h.ParentSession = "one"
			h.SpawnedBy = "call_1"
			h.Harness = &agentsession.Harness{Name: "nightly", Version: "2"}
		}
		if _, err := st.Create(ctx, h); err != nil {
			t.Fatal(err)
		}
	}
	// Names come from info entries; the last one that set a name wins.
	for _, name := range []string{"first name", "", "Two"} {
		if _, err := st.Append(ctx, "two", &agentsession.InfoEntry{Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	collect := func(f agentsession.ListFilter) []string {
		var ids []string
		for sum, err := range st.List(ctx, f) {
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			ids = append(ids, sum.Header.ID)
			if f.WithNames {
				want := map[string]string{"two": "Two"}[sum.Header.ID]
				if sum.Name != want {
					t.Errorf("List name of %s = %q, want %q", sum.Header.ID, sum.Name, want)
				}
			}
		}
		return ids
	}
	tests := []struct {
		name string
		f    agentsession.ListFilter
		want []string
	}{
		{"all newest first", agentsession.ListFilter{}, []string{"three", "two", "one"}},
		{"cwd", agentsession.ListFilter{CWD: "/a"}, []string{"three", "one"}},
		{"parent", agentsession.ListFilter{ParentSession: "one"}, []string{"three"}},
		{"after", agentsession.ListFilter{After: base}, []string{"three", "two"}},
		{"before", agentsession.ListFilter{Before: base.Add(2 * time.Hour)}, []string{"two", "one"}},
		{"limit", agentsession.ListFilter{Limit: 2}, []string{"three", "two"}},
		{"with names", agentsession.ListFilter{WithNames: true}, []string{"three", "two", "one"}},
		{"with names filtered", agentsession.ListFilter{WithNames: true, CWD: "/b"}, []string{"two"}},
		{"none", agentsession.ListFilter{CWD: "/z"}, nil},
		// The header-level filters (#83): the harness by name, the extra
		// members by canonical value, and the sessions nothing spawned.
		{"harness", agentsession.ListFilter{Harness: "nightly"}, []string{"three", "one"}},
		{"harness no session has", agentsession.ListFilter{Harness: "cron"}, nil},
		{"top level", agentsession.ListFilter{TopLevel: true}, []string{"two", "one"}},
		{"harness, top level", agentsession.ListFilter{Harness: "nightly", TopLevel: true}, []string{"one"}},
		{"extra", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`)}}, []string{"two", "one"}},
		{"extra spelled apart", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:user": json.RawMessage(`{"id":7,"name":"u1"}`)}}, []string{"one"}},
		{"extra two members", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`), "acme:user": json.RawMessage(`{"id":7,"name":"u1"}`)}}, []string{"one"}},
		{"extra differing", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:platform": json.RawMessage(`"slack"`)}}, nil},
		{"extra the header lacks", agentsession.ListFilter{Extra: map[string]json.RawMessage{"acme:zone": json.RawMessage(`1`)}}, nil},
		{"extra with names", agentsession.ListFilter{WithNames: true, Extra: map[string]json.RawMessage{"acme:platform": json.RawMessage(`"telegram"`)}, CWD: "/b"}, []string{"two"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := collect(tt.f)
			if len(got) != len(tt.want) {
				t.Fatalf("List = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("List = %v, want %v", got, tt.want)
				}
			}
		})
	}
	// Stopping early is allowed.
	for range st.List(ctx, agentsession.ListFilter{}) {
		break
	}
}

func testDelete(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Open(ctx, s.ID()); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open after Delete = %v", err)
	}
	if err := st.Delete(ctx, s.ID()); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("second Delete = %v", err)
	}
	for range st.List(ctx, agentsession.ListFilter{}) {
		t.Error("deleted session still listed")
	}
	// The ID can be reused.
	if _, err := st.Create(ctx, agentsession.Header{ID: s.ID()}); err != nil {
		t.Errorf("Create after Delete: %v", err)
	}
}

func testPersistence(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	s, err := st.Create(ctx, agentsession.Header{CWD: "/p", Harness: &agentsession.Harness{Name: "h", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	cfgID, err := st.Append(ctx, id, &agentsession.ConfigEntry{Model: "m", Instructions: ptr("i")})
	if err != nil {
		t.Fatal(err)
	}
	userID, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, id, &agentsession.UnknownEntry{Raw: []byte(`{"type":"acme:note","text":"<x>","z":[1,2]}`)}); err != nil {
		t.Fatal(err)
	}
	asst := &agentsession.ItemEntry{Item: openresponses.AssistantText("yo"), ResponseID: "resp_1"}
	if _, err := st.Append(ctx, id, asst); err != nil {
		t.Fatal(err)
	}
	respID, err := st.Append(ctx, id, &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}})
	if err != nil {
		t.Fatal(err)
	}

	st2 := opts.Reopen(t, st)
	again, err := st2.Open(ctx, id)
	if err != nil {
		t.Fatalf("Open after reopen: %v", err)
	}
	if !sameHeader(again.Header(), s.Header()) {
		t.Errorf("header after reopen = %+v, want %+v", again.Header(), s.Header())
	}
	if again.Len() != 5 || again.Leaf() != respID {
		t.Errorf("after reopen: len %d leaf %s, want 5 %s", again.Len(), again.Leaf(), respID)
	}
	c, err := again.ContextAt(userID)
	if err != nil {
		t.Fatal(err)
	}
	if c.Settings.Model != "m" || c.Settings.Instructions != "i" || len(c.Items) != 1 || len(c.Entries) != 2 || c.Entries[0].Base().ID != cfgID {
		t.Errorf("context after reopen = %+v", c)
	}
	e, _ := again.Entry(respID)
	if r, ok := e.(*agentsession.ResponseEntry); !ok || r.Usage == nil || r.Usage.TotalTokens != 2 {
		t.Errorf("response after reopen = %+v", e)
	}
	// Appending through the second handle continues the same tree.
	moreID, err := st2.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("more")))
	if err != nil {
		t.Fatal(err)
	}
	if e, _ := again.Entry(moreID); e.Base().Parent != respID {
		t.Errorf("append after reopen parent = %s, want %s", e.Base().Parent, respID)
	}
	found := false
	for sum, err := range st2.List(ctx, agentsession.ListFilter{CWD: "/p"}) {
		if err != nil {
			t.Fatal(err)
		}
		if sum.Header.ID == id {
			found = true
		}
	}
	if !found {
		t.Error("session not listed after reopen")
	}
	if err := st2.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := opts.Reopen(t, st2).Open(ctx, id); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open after Delete and reopen = %v", err)
	}
}

func ptr[T any](v T) *T { return &v }

// sameHeader compares headers field by field, with CreatedAt compared
// as an instant so a store that round-trips through RFC 3339 passes.
func sameHeader(a, b agentsession.Header) bool {
	return a.Format == b.Format && a.ID == b.ID && a.Payload == b.Payload &&
		a.CWD == b.CWD && a.ParentSession == b.ParentSession && a.Media == b.Media &&
		a.CreatedAt.Equal(b.CreatedAt) && reflect.DeepEqual(a.Harness, b.Harness) &&
		reflect.DeepEqual(a.Extra, b.Extra)
}

// testContinue rolls a session over and checks that the successor
// starts from the old settings, the old session is marked superseded,
// and a Current listing shows only the successor.
func testContinue(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	if _, err := st.Create(ctx, agentsession.Header{ID: "old", CWD: "/p"}); err != nil {
		t.Fatal(err)
	}
	instr := "Be brief."
	for _, e := range []agentsession.Entry{
		&agentsession.ConfigEntry{Model: "gpt-5", Instructions: &instr},
		&agentsession.InfoEntry{Name: "main"},
		agentsession.NewItemEntry(openresponses.UserText("hello")),
	} {
		if _, err := st.Append(ctx, "old", e); err != nil {
			t.Fatal(err)
		}
	}
	next, err := agentsession.Continue(ctx, st, "old", openresponses.DeveloperText("Earlier: the user said hello."))
	if err != nil {
		t.Fatalf("Continue: %v", err)
	}
	nh := next.Header()
	if nh.ParentSession != "old" || nh.CWD != "/p" || next.Name() != "main" {
		t.Errorf("successor header = %+v name %q", nh, next.Name())
	}
	cx, err := next.Context()
	if err != nil {
		t.Fatal(err)
	}
	if cx.Settings.Model != "gpt-5" || cx.Settings.Instructions != "Be brief." || len(cx.Items) != 1 {
		t.Errorf("successor context = %+v, %d items", cx.Settings, len(cx.Items))
	}
	if _, ok := next.Entries()[0].(*agentsession.ConfigEntry); !ok {
		t.Error("successor's first entry is not a config")
	}
	old, err := st.Open(ctx, "old")
	if err != nil {
		t.Fatal(err)
	}
	if old.SupersededBy() != next.ID() {
		t.Errorf("old.SupersededBy = %q, want %s", old.SupersededBy(), next.ID())
	}
	ids := func(f agentsession.ListFilter) map[string]agentsession.Summary {
		out := map[string]agentsession.Summary{}
		for sum, err := range st.List(ctx, f) {
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			out[sum.Header.ID] = sum
		}
		return out
	}
	all := ids(agentsession.ListFilter{WithNames: true})
	if len(all) != 2 || all["old"].SupersededBy != next.ID() || all[next.ID()].SupersededBy != "" {
		t.Errorf("listing = %+v", all)
	}
	current := ids(agentsession.ListFilter{Current: true})
	if len(current) != 1 || current[next.ID()].Header.ID == "" {
		t.Errorf("current listing = %+v, want only %s", current, next.ID())
	}
}
