package cas

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

func setRef(t *testing.T, st *Store, name string, target agentsession.RefTarget) {
	t.Helper()
	if err := st.UpdateRef(context.Background(), name, agentsession.RefTarget{}, target, "test"); err != nil {
		t.Fatal(err)
	}
}

func resolve(t *testing.T, st *Store, name string) agentsession.RefTarget {
	t.Helper()
	got, err := st.ResolveRef(context.Background(), name)
	if err != nil && !errors.Is(err, agentsession.ErrNoRef) {
		t.Fatal(err)
	}
	return got
}

// TestPushCarriesRefs: a push names a ref, and the receiver, which has
// none of that name, creates it, pointing at the pushed session and
// pinning the entry the sender's does.
func TestPushCarriesRefs(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	ids := fill(t, rec, "s", 3)
	setRef(t, rec, "conv/1", agentsession.RefTarget{Session: "s"})
	setRef(t, rec, "base/1", agentsession.RefTarget{Session: "s", Entry: ids[1]})
	x, err := rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "conv/1"}, {Name: "base/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(x.Refs) != 2 || !x.Refs[0].Moved || !x.Refs[1].Moved {
		t.Fatalf("refs: %+v", x.Refs)
	}
	if got := resolve(t, mir, "conv/1"); got != (agentsession.RefTarget{Session: "s"}) {
		t.Errorf("conv/1 on the receiver = %v", got)
	}
	if got := resolve(t, mir, "base/1"); got != (agentsession.RefTarget{Session: "s", Entry: ids[1]}) {
		t.Errorf("base/1 on the receiver = %v, want the pin", got)
	}
	// The receiver's log says where the ref came from.
	for u, err := range mir.RefLog(ctx, "conv/1") {
		if err != nil {
			t.Fatal(err)
		}
		if u.Reason != "exchange: push" {
			t.Errorf("receiver's log reason %q", u.Reason)
		}
	}
}

// TestPushRefMovedAtReceiver: the receiver's ref has gone elsewhere
// since the sender last saw it. The ref is refused with the holder
// reported, the closure and the other refs are not undone, and the
// right expected value moves it.
func TestPushRefMovedAtReceiver(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	fill(t, rec, "s", 2)
	fill(t, mir, "other", 1)
	setRef(t, rec, "conv/1", agentsession.RefTarget{Session: "s"})
	setRef(t, rec, "conv/2", agentsession.RefTarget{Session: "s"})
	// The receiver's conv/1 is another session's.
	otherTarget := agentsession.RefTarget{Session: "other"}
	setRef(t, mir, "conv/1", otherTarget)
	before := 0
	for range mir.RefLog(ctx, "conv/1") {
		before++
	}

	x, err := rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "conv/1"}, {Name: "conv/2"}}})
	if err != nil {
		t.Fatalf("a refused ref failed the push: %v", err)
	}
	if x.Admitted != 2 || !x.Created {
		t.Errorf("the closure was not kept: %+v", x)
	}
	if r := x.Refs[0]; r.Moved || r.Current != otherTarget || r.Why == "" {
		t.Errorf("refused ref: %+v, want not moved, holder %v, with a reason", r, otherTarget)
	}
	if r := x.Refs[1]; !r.Moved {
		t.Errorf("the other ref was held back: %+v", x.Refs[1])
	}
	if got := resolve(t, mir, "conv/1"); got != otherTarget {
		t.Errorf("the refused ref moved to %v", got)
	}
	after := 0
	for range mir.RefLog(ctx, "conv/1") {
		after++
	}
	if after != before {
		t.Errorf("a refused ref was logged: %d records, was %d", after, before)
	}
	if _, err := mir.Mark(ctx, "s"); err != nil {
		t.Errorf("the receiver does not hold the session: %v", err)
	}

	// With the holder as the expected value, it moves.
	x, err = rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "conv/1", Expected: otherTarget}}})
	if err != nil || !x.Refs[0].Moved {
		t.Fatalf("push with the right expected value: %+v, %v", x.Refs, err)
	}
	if got := resolve(t, mir, "conv/1"); got != (agentsession.RefTarget{Session: "s"}) {
		t.Errorf("conv/1 = %v", got)
	}
}

// TestPushRefRefusedByName: a ref the receiver cannot take for its name,
// since a ref is already there that this one would be under, is
// refused in the result and the closure stays.
func TestPushRefRefusedByName(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	fill(t, rec, "s", 1)
	fill(t, mir, "other", 1)
	setRef(t, rec, "conv/1", agentsession.RefTarget{Session: "s"})
	setRef(t, mir, "conv", agentsession.RefTarget{Session: "other"})
	x, err := rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "conv/1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if x.Refs[0].Moved || x.Refs[0].Why == "" || x.Admitted != 1 {
		t.Errorf("%+v", x)
	}
}

// TestPushRefOfAnotherSession: a ref that does not point at the session
// pushed is a mistake of the caller, refused before anything is sent.
func TestPushRefOfAnotherSession(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	fill(t, rec, "s", 1)
	fill(t, rec, "t", 1)
	setRef(t, rec, "conv/1", agentsession.RefTarget{Session: "t"})
	if _, err := rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "conv/1"}}}); err == nil {
		t.Fatal("a push carried a ref to another session")
	}
	if _, err := rec.Push(ctx, mir, "s", PushOptions{Refs: []RefPush{{Name: "missing"}}}); !errors.Is(err, agentsession.ErrNoRef) {
		t.Errorf("a push of a ref the sender lacks: %v, want ErrNoRef", err)
	}
	if _, err := mir.Mark(ctx, "s"); err == nil {
		t.Error("a refused push left the session at the receiver")
	}
}

// TestFetchCarriesRefs: a fetch moves the fetcher's ref to what the
// source's names, by compare-and-swap from the value the fetcher named.
func TestFetchCarriesRefs(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	fill(t, rec, "s", 2)
	fill(t, mir, "other", 1)
	setRef(t, rec, "conv/1", agentsession.RefTarget{Session: "s"})
	setRef(t, mir, "conv/1", agentsession.RefTarget{Session: "other"})
	x, err := mir.Fetch(ctx, rec, "s", RefPush{Name: "conv/1"})
	if err != nil {
		t.Fatal(err)
	}
	if x.Refs[0].Moved || x.Refs[0].Current != (agentsession.RefTarget{Session: "other"}) {
		t.Errorf("a fetch moved a ref from a stale expected value: %+v", x.Refs[0])
	}
	x, err = mir.Fetch(ctx, rec, "s", RefPush{Name: "conv/1", Expected: agentsession.RefTarget{Session: "other"}})
	if err != nil || !x.Refs[0].Moved {
		t.Fatalf("%+v, %v", x.Refs, err)
	}
	if got := resolve(t, mir, "conv/1"); got.Session != "s" {
		t.Errorf("conv/1 = %v", got)
	}
}
