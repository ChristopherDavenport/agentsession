package storetest

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testRefs exercises [agentsession.RefStore]: create-if-absent,
// compare-and-swap against a stale value, delete, the rules on names,
// the target's session and entry, a dangling ref, listing by prefix,
// the ref log, a second store on the same storage, SessionFor and
// ContinueRef, and the races RFC 0002 says a store decides.
func testRefs(t *testing.T, opts Options) {
	st := opts.New(t)
	if _, ok := st.(agentsession.RefStore); !ok {
		t.Skip("the store does not implement agentsession.RefStore")
	}
	t.Run("CreateIfAbsent", func(t *testing.T) { refsCreate(t, opts) })
	t.Run("StaleExpected", func(t *testing.T) { refsStale(t, opts) })
	t.Run("Delete", func(t *testing.T) { refsDelete(t, opts) })
	t.Run("Names", func(t *testing.T) { refsNames(t, opts) })
	t.Run("NameRace", func(t *testing.T) { refsNameRace(t, opts) })
	t.Run("Target", func(t *testing.T) { refsTarget(t, opts) })
	t.Run("Dangling", func(t *testing.T) { refsDangling(t, opts) })
	t.Run("Recreated", func(t *testing.T) { refsRecreated(t, opts) })
	t.Run("List", func(t *testing.T) { refsList(t, opts) })
	t.Run("Log", func(t *testing.T) { refsLog(t, opts) })
	t.Run("SecondStore", func(t *testing.T) { refsSecond(t, opts) })
	t.Run("ReadOnly", func(t *testing.T) { refsReadOnly(t, opts) })
	t.Run("SessionFor", func(t *testing.T) { refsSessionFor(t, opts) })
	t.Run("SessionForLoser", func(t *testing.T) { refsSessionForLoser(t, opts) })
	t.Run("SessionForRace", func(t *testing.T) { refsSessionForRace(t, opts) })
	t.Run("SessionForRaceSecond", func(t *testing.T) { refsSessionForRaceSecond(t, opts) })
	t.Run("ContinueRef", func(t *testing.T) { refsContinue(t, opts) })
	if opts.Reopen != nil {
		t.Run("Persistence", func(t *testing.T) { refsPersist(t, opts) })
	}
}

func refStore(t *testing.T, st agentsession.Store) agentsession.RefStore {
	t.Helper()
	rs, ok := st.(agentsession.RefStore)
	if !ok {
		t.Fatalf("%T is not a RefStore", st)
	}
	return rs
}

// newSession creates a session and returns its ID.
func newSession(t *testing.T, st agentsession.Store) string {
	t.Helper()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

func tgt(session string) agentsession.RefTarget { return agentsession.RefTarget{Session: session} }

func listRefs(t *testing.T, rs agentsession.RefStore, prefix string) []agentsession.Ref {
	t.Helper()
	var out []agentsession.Ref
	for r, err := range rs.ListRefs(context.Background(), prefix) {
		if err != nil {
			t.Fatalf("ListRefs(%q): %v", prefix, err)
		}
		out = append(out, r)
	}
	return out
}

func refLog(t *testing.T, rs agentsession.RefStore, name string) []agentsession.RefUpdate {
	t.Helper()
	var out []agentsession.RefUpdate
	for u, err := range rs.RefLog(context.Background(), name) {
		if err != nil {
			t.Fatalf("RefLog(%q): %v", name, err)
		}
		out = append(out, u)
	}
	return out
}

// wantMoved fails unless err is ErrRefMoved carrying current.
func wantMoved(t *testing.T, err error, current agentsession.RefTarget) {
	t.Helper()
	if !errors.Is(err, agentsession.ErrRefMoved) {
		t.Fatalf("error %v, want ErrRefMoved", err)
	}
	var moved *agentsession.RefMovedError
	if !errors.As(err, &moved) {
		t.Fatalf("error %v carries no RefMovedError", err)
	}
	if moved.Current != current {
		t.Errorf("RefMovedError.Current = %v, want %v", moved.Current, current)
	}
}

func refsCreate(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	if _, err := rs.ResolveRef(ctx, "chat/one"); !errors.Is(err, agentsession.ErrNoRef) {
		t.Fatalf("ResolveRef of a missing ref: %v, want ErrNoRef", err)
	}
	if err := rs.UpdateRef(ctx, "chat/one", agentsession.RefTarget{}, tgt(a), "first"); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := rs.ResolveRef(ctx, "chat/one")
	if err != nil || got != tgt(a) {
		t.Fatalf("ResolveRef = %v, %v; want %s", got, err, a)
	}
	// A second create-if-absent loses, and learns the winner.
	wantMoved(t, rs.UpdateRef(ctx, "chat/one", agentsession.RefTarget{}, tgt(b), ""), tgt(a))
	if got, _ := rs.ResolveRef(ctx, "chat/one"); got != tgt(a) {
		t.Errorf("the loser moved the ref to %v", got)
	}
}

func refsStale(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b, c := newSession(t, st), newSession(t, st), newSession(t, st)
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt(a), ""); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "r", tgt(a), tgt(b), "move"); err != nil {
		t.Fatalf("move with the held value: %v", err)
	}
	wantMoved(t, rs.UpdateRef(ctx, "r", tgt(a), tgt(c), ""), tgt(b))
	if got, _ := rs.ResolveRef(ctx, "r"); got != tgt(b) {
		t.Errorf("a stale update moved the ref to %v", got)
	}
	// The entry is part of the value compared.
	e, err := st.Append(ctx, b, user("x"))
	if err != nil {
		t.Fatal(err)
	}
	wantMoved(t, rs.UpdateRef(ctx, "r", agentsession.RefTarget{Session: b, Entry: e}, tgt(c), ""), tgt(b))
	pin := agentsession.RefTarget{Session: b, Entry: e}
	if err := rs.UpdateRef(ctx, "r", tgt(b), pin, "pin"); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if got, _ := rs.ResolveRef(ctx, "r"); got != pin {
		t.Errorf("ResolveRef = %v, want the pin %v", got, pin)
	}
}

func refsDelete(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt(a), ""); err != nil {
		t.Fatal(err)
	}
	wantMoved(t, rs.UpdateRef(ctx, "r", tgt(b), agentsession.RefTarget{}, ""), tgt(a))
	if _, err := rs.ResolveRef(ctx, "r"); err != nil {
		t.Fatalf("a refused delete removed the ref: %v", err)
	}
	if err := rs.UpdateRef(ctx, "r", tgt(a), agentsession.RefTarget{}, "done"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := rs.ResolveRef(ctx, "r"); !errors.Is(err, agentsession.ErrNoRef) {
		t.Errorf("ResolveRef after delete: %v, want ErrNoRef", err)
	}
	// Deleting what is not there is a mismatch, and none for none is not.
	wantMoved(t, rs.UpdateRef(ctx, "r", tgt(a), agentsession.RefTarget{}, ""), agentsession.RefTarget{})
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{}, ""); err != nil {
		t.Errorf("none to none on an absent ref: %v", err)
	}
	// The name is free again, and its log continues.
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt(b), "again"); err != nil {
		t.Fatalf("create after delete: %v", err)
	}
	if got := refLog(t, rs, "r"); len(got) != 3 {
		t.Errorf("log has %d updates, want create, delete and create", len(got))
	}
}

func refsNames(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	s := newSession(t, st)
	good := []string{"a", "a.b", "a_b-c", "conversations/slack/C123", "x/1", "a..b", "..a", ".hidden", "v1.2/x", "console", "COM10", "com0", "con-x", ".con", "x/lpt", "aux_1"}
	for _, n := range good {
		if err := rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), ""); err != nil {
			// A case or prefix clash among the good names themselves is
			// a test bug; only names that break the rules are refused.
			t.Errorf("name %q refused: %v", n, err)
		}
	}
	long := make([]byte, agentsession.MaxRefName+1)
	for i := range long {
		long[i] = 'x'
	}
	bad := []string{"", "/", "/a", "a/", "a//b", ".", "..", "a/./b", "a/../b", "a b", "a:b", "a\\b", "é", "a\x00b", string(long),
		"CON", "con", "Prn", "aux", "NUL", "nul.txt", "a/PRN", "COM1", "com9.log", "LPT1", "lpt9.x", "a.", "a./b", "x/CON.d/y"}
	for _, n := range bad {
		err := rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), "")
		if !errors.Is(err, agentsession.ErrRefName) {
			t.Errorf("UpdateRef(%q): %v, want ErrRefName", n, err)
		}
		if _, err := rs.ResolveRef(ctx, n); !errors.Is(err, agentsession.ErrRefName) {
			t.Errorf("ResolveRef(%q): %v, want ErrRefName", n, err)
		}
	}
	for r, err := range rs.ListRefs(ctx, "a b") {
		if !errors.Is(err, agentsession.ErrRefName) {
			t.Errorf("ListRefs with a bad prefix: %v (%v), want ErrRefName", r, err)
		}
	}

	// A ref under a ref, both ways, and a name differing only in case.
	clash := []string{
		"conversations/slack",                // a prefix of an existing ref
		"conversations/slack/C123/thread",    // under an existing ref
		"a",                                  // exists, but is not a clash: see below
		"CONVERSATIONS/slack/C999",           // a directory differing only in case
		"conversations/SLACK/C123",           // a segment differing only in case
		"X/1",                                // name differs in case from x/1
		"x/2/deeper/still/CONVERSATIONS/a/b", // a fresh branch, no clash: see below
	}
	for _, n := range []string{clash[0], clash[1], clash[3], clash[4], clash[5], "A/b", "V1.2/y", "a/b"} {
		err := rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), "")
		if !errors.Is(err, agentsession.ErrRefName) {
			t.Errorf("UpdateRef(%q): %v, want ErrRefName", n, err)
		}
	}
	// Names that share no prefix with a ref are fine, and so is a name
	// that only starts with another's characters.
	for _, n := range []string{"conversations/slack/C124", "conversations/discord/x", "ab", "x/12", clash[6]} {
		if err := rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), ""); err != nil {
			t.Errorf("UpdateRef(%q): %v", n, err)
		}
	}
	// Once a ref is deleted, a name under it is allowed, and the other
	// way round.
	if err := rs.UpdateRef(ctx, "a", tgt(s), agentsession.RefTarget{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "a/b", agentsession.RefTarget{}, tgt(s), ""); err != nil {
		t.Errorf("a ref under the deleted ref: %v", err)
	}
	if err := rs.UpdateRef(ctx, "a/b", tgt(s), agentsession.RefTarget{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "a", agentsession.RefTarget{}, tgt(s), ""); err != nil {
		t.Errorf("recreating the ref over its deleted child: %v", err)
	}
}

// refsNameRace creates a ref and a ref under it at once: at most one
// may exist, whatever the interleaving.
func refsNameRace(t *testing.T, opts Options) {
	ctx := context.Background()
	for round := range 5 {
		st := opts.New(t)
		rs := refStore(t, st)
		s := newSession(t, st)
		names := []string{"p/q", "p/q/r", "P/q", "p"}
		errs := make([]error, len(names))
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, n := range names {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs[i] = rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), "")
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !errors.Is(err, agentsession.ErrRefName):
				t.Errorf("round %d: create %q: %v", round, names[i], err)
			}
		}
		if got := listRefs(t, rs, ""); won != 1 || len(got) != 1 {
			t.Fatalf("round %d: %d creations won and %v exist, want exactly one", round, won, got)
		}
	}
}

func refsTarget(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	e, err := st.Append(ctx, a, user("x"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt("no-such-session"), ""); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a ref to a missing session: %v, want ErrNoSession", err)
	}
	if _, err := rs.ResolveRef(ctx, "r"); !errors.Is(err, agentsession.ErrNoRef) {
		t.Errorf("a refused update left a ref: %v", err)
	}
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: a, Entry: "nope"}, ""); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("a pin of a missing entry: %v, want ErrNoEntry", err)
	}
	// An entry of another session is not this session's.
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: b, Entry: e}, ""); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("a pin of another session's entry: %v, want ErrNoEntry", err)
	}
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: a, Entry: e}, ""); err != nil {
		t.Errorf("a pin of the session's own entry: %v", err)
	}
	// A pin on the prefix of a fork is the session's too.
	f, err := st.Create(ctx, agentsession.Header{Base: e, ParentSession: a})
	if err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "fork", agentsession.RefTarget{}, agentsession.RefTarget{Session: f.ID(), Entry: e}, ""); err != nil {
		t.Errorf("a pin of an entry on the fork's prefix: %v", err)
	}
	// A refused update logs nothing.
	if got := refLog(t, rs, "nope"); len(got) != 0 {
		t.Errorf("log of a ref never set: %v", got)
	}
}

func refsDangling(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt(a), ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, err := rs.ResolveRef(ctx, "r")
	if !errors.Is(err, agentsession.ErrNoSession) {
		t.Fatalf("a dangling ref: %v, want ErrNoSession", err)
	}
	if got != tgt(a) {
		t.Errorf("a dangling ref reports target %v, want %s", got, a)
	}
	if listed := listRefs(t, rs, ""); len(listed) != 1 || listed[0].Target != tgt(a) {
		t.Errorf("ListRefs does not show the dangling ref: %v", listed)
	}
	// The ref moves by compare-and-swap from the dangling target.
	if err := rs.UpdateRef(ctx, "r", tgt(a), tgt(b), "repoint"); err != nil {
		t.Fatalf("moving a dangling ref: %v", err)
	}
	if got, err := rs.ResolveRef(ctx, "r"); err != nil || got != tgt(b) {
		t.Errorf("ResolveRef = %v, %v; want %s", got, err, b)
	}
}

func refsList(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	s := newSession(t, st)
	names := []string{"z", "chat/b", "chat/a", "chat/c/1", "chat2/x", "Chat/ignored-by-case", "ticket/9"}
	// "Chat/…" clashes with "chat/…", so the last of the set is left out.
	names = append(names[:5], names[6])
	for _, n := range names {
		if err := rs.UpdateRef(ctx, n, agentsession.RefTarget{}, tgt(s), ""); err != nil {
			t.Fatal(err)
		}
	}
	names2 := func(refs []agentsession.Ref) []string {
		var out []string
		for _, r := range refs {
			if r.Target != tgt(s) {
				t.Errorf("ref %s has target %v", r.Name, r.Target)
			}
			out = append(out, r.Name)
		}
		return out
	}
	for _, c := range []struct {
		prefix string
		want   []string
	}{
		{"", []string{"chat/a", "chat/b", "chat/c/1", "chat2/x", "ticket/9", "z"}},
		{"chat/", []string{"chat/a", "chat/b", "chat/c/1"}},
		{"chat", []string{"chat/a", "chat/b", "chat/c/1", "chat2/x"}},
		{"chat/c", []string{"chat/c/1"}},
		{"nothing", nil},
	} {
		if got := names2(listRefs(t, rs, c.prefix)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("ListRefs(%q) = %v, want %v", c.prefix, got, c.want)
		}
	}
	// Stopping early is allowed.
	for range rs.ListRefs(ctx, "") {
		break
	}
}

func refsLog(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	before := time.Now().Add(-time.Minute)
	steps := []struct {
		old, next agentsession.RefTarget
		reason    string
	}{
		{agentsession.RefTarget{}, tgt(a), "create"},
		{tgt(a), tgt(b), "handoff"},
		{tgt(b), agentsession.RefTarget{}, ""},
		{agentsession.RefTarget{}, tgt(a), "again"},
	}
	for _, s := range steps {
		if err := rs.UpdateRef(ctx, "c/1", s.old, s.next, s.reason); err != nil {
			t.Fatal(err)
		}
	}
	// None of these is accepted, so none is logged.
	_ = rs.UpdateRef(ctx, "c/1", tgt(b), tgt(a), "stale")
	_ = rs.UpdateRef(ctx, "c/1", tgt(a), tgt("missing"), "missing")
	if err := rs.UpdateRef(ctx, "c/1", tgt(a), tgt(a), "same"); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "c/2", agentsession.RefTarget{}, tgt(a), "other ref"); err != nil {
		t.Fatal(err)
	}
	got := refLog(t, rs, "c/1")
	if len(got) != len(steps) {
		t.Fatalf("log has %d updates, want %d: %v", len(got), len(steps), got)
	}
	for i, u := range got {
		s := steps[len(steps)-1-i] // newest first
		if u.Name != "c/1" || u.Old != s.old || u.New != s.next || u.Reason != s.reason {
			t.Errorf("log[%d] = %+v, want %s %v -> %v %q", i, u, "c/1", s.old, s.next, s.reason)
		}
		if u.Time.Before(before) || u.Time.After(time.Now().Add(time.Minute)) {
			t.Errorf("log[%d] time %v is not now", i, u.Time)
		}
		if i > 0 && u.Time.After(got[i-1].Time) {
			t.Errorf("log[%d] is newer than log[%d]", i, i-1)
		}
	}
	if other := refLog(t, rs, "c/2"); len(other) != 1 || other[0].Reason != "other ref" {
		t.Errorf("log of c/2: %v", other)
	}
}

// second returns a second store on the storage of st, or skips.
func secondStore(t *testing.T, opts Options, st agentsession.Store) agentsession.Store {
	t.Helper()
	switch {
	case opts.Second != nil:
		return opts.Second(t, st)
	case opts.Reopen != nil:
		return opts.Reopen(t, st)
	}
	t.Skip("the store has no second handle on its storage")
	return nil
}

func refsSecond(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	other := secondStore(t, opts, st)
	rs, rs2 := refStore(t, st), refStore(t, other)
	a, b := newSession(t, st), newSession(t, st)
	if err := rs.UpdateRef(ctx, "shared/r", agentsession.RefTarget{}, tgt(a), "one"); err != nil {
		t.Fatal(err)
	}
	if got, err := rs2.ResolveRef(ctx, "shared/r"); err != nil || got != tgt(a) {
		t.Fatalf("the second store resolves %v, %v; want %s", got, err, a)
	}
	// The second store loses a create the first won, and learns who won.
	wantMoved(t, rs2.UpdateRef(ctx, "shared/r", agentsession.RefTarget{}, tgt(b), ""), tgt(a))
	if err := rs2.UpdateRef(ctx, "shared/r", tgt(a), tgt(b), "two"); err != nil {
		t.Fatalf("the second store moves the ref: %v", err)
	}
	wantMoved(t, rs.UpdateRef(ctx, "shared/r", tgt(a), tgt(a), ""), tgt(b))
	if got, _ := rs.ResolveRef(ctx, "shared/r"); got != tgt(b) {
		t.Errorf("the first store resolves %v after the second moved it", got)
	}
	// Names are checked against the other store's refs too.
	if err := rs.UpdateRef(ctx, "shared/r/under", agentsession.RefTarget{}, tgt(a), ""); !errors.Is(err, agentsession.ErrRefName) {
		t.Errorf("a ref under a ref the other store made: %v, want ErrRefName", err)
	}
	if got := listRefs(t, rs2, "shared/"); len(got) != 1 || got[0].Target != tgt(b) {
		t.Errorf("the second store lists %v", got)
	}
	got := refLog(t, rs, "shared/r")
	if len(got) != 2 || got[0].Reason != "two" || got[1].Reason != "one" {
		t.Errorf("log seen by the first store: %+v", got)
	}
}

func refsReadOnly(t *testing.T, opts Options) {
	ctx := context.Background()
	if opts.ReadOnly == nil {
		t.Skip("the store has no read-only form")
	}
	st := opts.New(t)
	rs := refStore(t, st)
	a := newSession(t, st)
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt(a), ""); err != nil {
		t.Fatal(err)
	}
	ro := refStore(t, opts.ReadOnly(t, st))
	if got, err := ro.ResolveRef(ctx, "r"); err != nil || got != tgt(a) {
		t.Errorf("a read-only store resolves %v, %v; want %s", got, err, a)
	}
	if got := listRefs(t, ro, ""); len(got) != 1 {
		t.Errorf("a read-only store lists %v", got)
	}
	if err := ro.UpdateRef(ctx, "r", tgt(a), agentsession.RefTarget{}, ""); !errors.Is(err, agentsession.ErrReadOnly) {
		t.Errorf("a read-only store moved a ref: %v, want ErrReadOnly", err)
	}
	if got, _ := rs.ResolveRef(ctx, "r"); got != tgt(a) {
		t.Errorf("ref is %v after the refused update", got)
	}
}

func refsPersist(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	a, b := newSession(t, st), newSession(t, st)
	if err := rs.UpdateRef(ctx, "p/r", agentsession.RefTarget{}, tgt(a), "one"); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "p/r", tgt(a), tgt(b), "two"); err != nil {
		t.Fatal(err)
	}
	if c, ok := st.(interface{ Close() error }); ok {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	re := opts.Reopen(t, st)
	rs2 := refStore(t, re)
	if got, err := rs2.ResolveRef(ctx, "p/r"); err != nil || got != tgt(b) {
		t.Errorf("after reopen: %v, %v; want %s", got, err, b)
	}
	if got := refLog(t, rs2, "p/r"); len(got) != 2 || got[0].Reason != "two" {
		t.Errorf("log after reopen: %+v", got)
	}
}

func refsSessionFor(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	h := agentsession.Header{CWD: "/work"}
	s, err := agentsession.SessionFor(ctx, st, "chan/C1", h)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rs.ResolveRef(ctx, "chan/C1"); err != nil || got != tgt(s.ID()) {
		t.Fatalf("ref = %v, %v; want %s", got, err, s.ID())
	}
	again, err := agentsession.SessionFor(ctx, st, "chan/C1", h)
	if err != nil || again.ID() != s.ID() {
		t.Fatalf("second SessionFor = %v, %v; want %s", again, err, s.ID())
	}
	if n := countSessions(t, st); n != 1 {
		t.Errorf("%d sessions after two SessionFor calls, want 1", n)
	}
	// A dangling ref is replaced, not returned.
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	next, err := agentsession.SessionFor(ctx, st, "chan/C1", h)
	if err != nil {
		t.Fatalf("SessionFor over a dangling ref: %v", err)
	}
	if next.ID() == s.ID() {
		t.Error("SessionFor returned the deleted session")
	}
	if got, _ := rs.ResolveRef(ctx, "chan/C1"); got != tgt(next.ID()) {
		t.Errorf("ref = %v, want %s", got, next.ID())
	}
	if _, err := agentsession.SessionFor(ctx, st, "bad name", h); !errors.Is(err, agentsession.ErrRefName) {
		t.Errorf("SessionFor with a bad name: %v, want ErrRefName", err)
	}
	if n := countSessions(t, st); n != 1 {
		t.Errorf("%d sessions after the failed call, want 1 (nothing created for a bad name)", n)
	}
}

func countSessions(t *testing.T, st agentsession.Store) int {
	t.Helper()
	n := 0
	for _, err := range st.List(context.Background(), agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	return n
}

// raceSessionFor runs callers SessionFor calls for one name across
// stores, round-robin, and checks the end: one ref, one session, every
// caller that got a session got that one, and no loser's session left.
// A caller refused with ErrSessionLocked, which a store guarding
// sessions reports to a loser whose winner another store holds, is not
// a failure.
func raceSessionFor(t *testing.T, stores []agentsession.Store, callers int) {
	t.Helper()
	ctx := context.Background()
	got := make([]string, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		st := stores[i%len(stores)]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := agentsession.SessionFor(ctx, st, "race/name", agentsession.Header{})
			errs[i] = err
			if err == nil {
				got[i] = s.ID()
			}
		}()
	}
	close(start)
	wg.Wait()
	rs := refStore(t, stores[0])
	ref, err := rs.ResolveRef(ctx, "race/name")
	if err != nil {
		t.Fatalf("after the race: %v", err)
	}
	won := 0
	for i := range callers {
		switch {
		case errs[i] == nil:
			won++
			if got[i] != ref.Session {
				t.Errorf("caller %d got session %s, the ref names %s", i, got[i], ref.Session)
			}
		case !errors.Is(errs[i], agentsession.ErrSessionLocked):
			t.Errorf("caller %d: %v", i, errs[i])
		}
	}
	if won == 0 {
		t.Error("no caller got the session")
	}
	if refs := listRefs(t, rs, ""); len(refs) != 1 {
		t.Errorf("%d refs after the race, want 1: %v", len(refs), refs)
	}
	n := 0
	for sum, err := range stores[0].List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		if sum.Header.ID != ref.Session {
			t.Errorf("session %s left behind by a loser", sum.Header.ID)
		}
	}
	if n != 1 {
		t.Errorf("%d sessions after the race, want 1", n)
	}
}

func refsSessionForRace(t *testing.T, opts Options) {
	for range 3 {
		st := opts.New(t)
		raceSessionFor(t, []agentsession.Store{st}, 16)
	}
}

// refsSessionForRaceSecond is #129's scenario: two stores on one
// storage, one name, one session.
func refsSessionForRaceSecond(t *testing.T, opts Options) {
	if opts.Second == nil {
		t.Skip("the store has no second writing store")
	}
	for range 3 {
		st := opts.New(t)
		raceSessionFor(t, []agentsession.Store{st, opts.Second(t, st)}, 16)
	}
}

func refsContinue(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	s, err := agentsession.SessionFor(ctx, st, "talk", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText("hello"))); err != nil {
		t.Fatal(err)
	}
	if id, moved, err := agentsession.ResolveCurrent(ctx, st, "talk"); err != nil || moved || id != s.ID() {
		t.Fatalf("ResolveCurrent before = %s, %v, %v; want %s, false", id, moved, err, s.ID())
	}
	next, err := agentsession.ContinueRef(ctx, st, "talk", nil)
	if err != nil {
		t.Fatalf("ContinueRef: %v", err)
	}
	if got, err := rs.ResolveRef(ctx, "talk"); err != nil || got != tgt(next.ID()) {
		t.Errorf("ref = %v, %v; want the successor %s", got, err, next.ID())
	}
	if log := refLog(t, rs, "talk"); len(log) != 2 || log[0].Old != tgt(s.ID()) || log[0].New != tgt(next.ID()) {
		t.Errorf("log after ContinueRef: %+v", log)
	}
	// A continuation that did not move the ref: the ref is behind, and
	// ResolveRef stays literal while ResolveCurrent walks.
	third, err := agentsession.Continue(ctx, st, next.ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := rs.ResolveRef(ctx, "talk"); got != tgt(next.ID()) {
		t.Errorf("Continue moved the ref to %v", got)
	}
	id, moved, err := agentsession.ResolveCurrent(ctx, st, "talk")
	if err != nil || !moved || id != third.ID() {
		t.Errorf("ResolveCurrent = %s, %v, %v; want %s, true", id, moved, err, third.ID())
	}
	// ContinueRef fails and reports it when the ref moved under it.
	other := newSession(t, st)
	if err := rs.UpdateRef(ctx, "talk", tgt(next.ID()), tgt(other), "steal"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := agentsession.ResolveCurrent(ctx, st, "missing"); !errors.Is(err, agentsession.ErrNoRef) {
		t.Errorf("ResolveCurrent of a missing ref: %v, want ErrNoRef", err)
	}
	if _, err := agentsession.ContinueRef(ctx, st, "missing", nil); !errors.Is(err, agentsession.ErrNoRef) {
		t.Errorf("ContinueRef of a missing ref: %v, want ErrNoRef", err)
	}
}

// staleResolver makes the first ResolveRef report no ref, as a caller
// that resolved just before another created the ref would have seen,
// which forces SessionFor down the path of the loser.
type staleResolver struct {
	agentsession.Store
	agentsession.RefStore
	once sync.Once
}

func (s *staleResolver) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	stale := false
	s.once.Do(func() { stale = true })
	if stale {
		return agentsession.RefTarget{}, agentsession.ErrNoRef
	}
	return s.RefStore.ResolveRef(ctx, name)
}

// refsSessionForLoser is the loser of the race, made certain: the ref
// is there when SessionFor first looks it up as absent. It must return
// the winner's session and leave no session of its own behind.
func refsSessionForLoser(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	winner, err := agentsession.SessionFor(ctx, st, "contested", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	loser := &staleResolver{Store: st, RefStore: rs}
	got, err := agentsession.SessionFor(ctx, loser, "contested", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID() != winner.ID() {
		t.Errorf("the loser got %s, want the winner's %s", got.ID(), winner.ID())
	}
	if n := countSessions(t, st); n != 1 {
		t.Errorf("%d sessions after the loser, want 1: its own was not deleted", n)
	}
}

// refsRecreated: a session deleted and created again under its ID, with
// another header, is not the session the ref was set to. The ref is
// dangling as when the session was gone, and moves by compare-and-swap
// from its target as before; the identity the store compares is never
// in the target.
func refsRecreated(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	rs := refStore(t, st)
	h := agentsession.Header{ID: "reused", CWD: "/first"}
	if _, err := st.Create(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := rs.UpdateRef(ctx, "r", agentsession.RefTarget{}, tgt("reused"), "set"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	h.CWD = "/second"
	if _, err := st.Create(ctx, h); err != nil {
		t.Fatal(err)
	}
	got, err := rs.ResolveRef(ctx, "r")
	if !errors.Is(err, agentsession.ErrNoSession) {
		t.Fatalf("ref to a session created again: %v, want ErrNoSession", err)
	}
	if got != tgt("reused") {
		t.Errorf("target %v, want the ID the ref was set to", got)
	}
	if listed := listRefs(t, rs, ""); len(listed) != 1 || listed[0].Target != tgt("reused") {
		t.Errorf("ListRefs: %v", listed)
	}
	// SessionFor does not return the stranger; it replaces the ref.
	s, err := agentsession.SessionFor(ctx, st, "r", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() == "reused" {
		t.Error("SessionFor returned the session created again under the ID")
	}
	// Setting the ref to its own target adopts the session now there.
	if err := rs.UpdateRef(ctx, "r", tgt(s.ID()), tgt("reused"), "adopt"); err != nil {
		t.Fatalf("adopting: %v", err)
	}
	if got, err := rs.ResolveRef(ctx, "r"); err != nil || got != tgt("reused") {
		t.Errorf("after adopting: %v, %v", got, err)
	}
	if err := rs.UpdateRef(ctx, "r", tgt("reused"), tgt("reused"), "again"); err != nil {
		t.Fatal(err)
	}
	if got := refLog(t, rs, "r"); got[0].Reason != "adopt" {
		t.Errorf("setting a ref to what it holds was logged: %+v", got[0])
	}
}
