package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/followtest"
)

func followRO(t *testing.T, root string) *Store {
	t.Helper()
	st, err := Open(root, WithReadOnly(), WithFollowInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func lens(c agentsession.Change) int { return c.Session.Len() }

// TestFollowAcrossPackAndSweep: a pack and a sweep move objects and not
// the log, and a follower goes on through both without a reset, whether
// it reads an entry loose or from a pack.
func TestFollowAcrossPackAndSweep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	ids := fill(t, st, "s", 3)
	w := followtest.Start(t, followRO(t, root), "s", "")
	start := w.NextKind(agentsession.Snapshot).Cursor
	w.Quiet()
	for round, step := range []func() error{
		func() error { _, err := st.Pack(ctx); return err },
		func() error { _, err := st.Sweep(ctx, 0); return err },
		func() error { _, err := st.Pack(ctx); return err },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
		id := mustAppend(t, st, "s", item(fmt.Sprintf("after %d", round)))
		ids = append(ids, id)
		c := w.NextKind(agentsession.Appended)
		if c.ID != id || lens(c) != len(ids) {
			t.Errorf("round %d: appended %s into %d entries, want %s into %d", round, c.ID, lens(c), id, len(ids))
		}
	}
	// A snapshot after the objects were packed reads through the packs.
	r := followtest.Start(t, followRO(t, root), "s", "")
	if c := r.NextKind(agentsession.Snapshot); lens(c) != len(ids) {
		t.Errorf("snapshot after a pack holds %d entries, want %d", lens(c), len(ids))
	}
	// And a resume from a cursor taken before them, which reads the
	// entries it is owed from the packs.
	m := followtest.Start(t, followRO(t, root), "s", start)
	for i := 3; i < len(ids); i++ {
		if c := m.NextKind(agentsession.Appended); c.ID != ids[i] || lens(c) != i+1 {
			t.Errorf("resumed change %d is %s into %d entries, want %s into %d", i, c.ID, lens(c), ids[i], i+1)
		}
	}
	m.Quiet()
}

// TestFollowResetOnRecovery: a writer's open recovers what a crash left
// of another's lazy appends, writing the log anew, and a follower that
// saw an append the crash took gets a reset without it.
func TestFollowResetOnRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, _ := Open(root)
	d.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, d, "s", item("durable"))
	d.Close()
	a, _ := Open(root, WithSync(SyncNever))
	e1 := mustAppend(t, a, "s", item("lazy one"))
	e2 := mustAppend(t, a, "s", item("lazy two"))

	w := followtest.Start(t, followRO(t, root), "s", "")
	snap := w.NextKind(agentsession.Snapshot)
	if snap.Session.Len() != 3 {
		t.Fatalf("the follower sees %d entries, want the 3 written, lazy ones included", snap.Session.Len())
	}
	// The crash takes the second lazy append's objects.
	p, _ := a.objs.loosePath(spaceEntries, e2)
	os.Remove(p)
	die(a)

	b, _ := Open(root)
	defer b.Close()
	if _, err := b.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	r := w.NextKind(agentsession.Reset)
	if r.Session.Len() != 2 {
		t.Errorf("reset holds %d entries, want the durable one and the first lazy one", r.Session.Len())
	}
	if _, ok := r.Session.Entry(e1); !ok {
		t.Error("the reset lacks the lazy append that survived")
	}
	if _, ok := r.Session.Entry(e2); ok {
		t.Error("the reset holds the append the crash took")
	}
	w.Quiet()
	next := mustAppend(t, b, "s", item("after recovery"))
	if c := w.NextKind(agentsession.Appended); c.ID != next || lens(c) != 3 {
		t.Errorf("appended %s into %d entries after the reset, want %s into 3", c.ID, lens(c), next)
	}
}

// TestFollowResetOnRepair: Repair writes the session a new log, which
// is a reset to a follower that was reading the old one.
func TestFollowResetOnRepair(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 5)
	st.Close()
	w := followtest.Start(t, followRO(t, root), "a", "")
	w.NextKind(agentsession.Snapshot)
	damageAppend(t, root, "a", 2)
	w.Quiet() // damaged in the middle, and unchanged where the follower is

	rp, _ := Open(root)
	defer rp.Close()
	if _, err := rp.Repair(ctx, "a", RepairOptions{}); err != nil {
		t.Fatal(err)
	}
	r := w.NextKind(agentsession.Reset)
	if lens(r) != len(ids) {
		t.Errorf("reset holds %d entries, want %d", lens(r), len(ids))
	}
	next := mustAppend(t, rp, "a", item("after the repair"))
	if c := w.NextKind(agentsession.Appended); c.ID != next {
		t.Errorf("appended %s, want %s", c.ID, next)
	}
}

// TestFollowResetOnMigrate: a cursor into the log a session had before
// a migration rewrote it is not a place in the new one.
func TestFollowResetOnMigrate(t *testing.T) {
	root, want := legacyStore(t)
	ro := followRO(t, root)
	w := followtest.Start(t, ro, "a", "")
	if err := w.End(); !errors.Is(err, ErrLegacyStore) {
		t.Fatalf("follow of a session not yet migrated: %v, want ErrLegacyStore", err)
	}
	// A cursor taken at the end of the old log.
	src := &source{store: ro, id: "a", dir: filepath.Join(root, "sessions", "a")}
	old, fi, err := src.identity()
	if err != nil {
		t.Fatal(err)
	}
	old.off, old.tail = fi.Size(), "0.0"

	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	r := followtest.Start(t, followRO(t, root), "a", old.cursor())
	c := r.NextKind(agentsession.Reset)
	if lens(c) != len(want) {
		t.Errorf("reset after the migration holds %d entries, want %d", lens(c), len(want))
	}
}

// TestFollowResetOnFailedAppend: an append whose fsync fails is cut back
// out of the log, after a follower may have seen it.
func TestFollowResetOnFailedAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	first := mustAppend(t, st, "s", item("one"))
	w := followtest.Start(t, followRO(t, root), "s", "")
	w.NextKind(agentsession.Snapshot)

	reached, release := make(chan struct{}), make(chan struct{})
	old := syncLog
	syncLog = func(*os.File) error {
		close(reached)
		<-release
		return errors.New("injected fsync failure")
	}
	failed := make(chan error, 1)
	go func() {
		_, err := st.Append(ctx, "s", item("failed"))
		failed <- err
	}()
	<-reached
	// The record is in the log, its fsync pending: it is visible.
	c := w.NextKind(agentsession.Appended)
	if c.ID == first || lens(c) != 2 {
		t.Fatalf("the follower saw %s into %d entries before the fsync ended", c.ID, lens(c))
	}
	close(release)
	if err := <-failed; !errors.Is(err, ErrStopped) {
		t.Fatalf("the append: %v", err)
	}
	syncLog = old
	r := w.NextKind(agentsession.Reset)
	if lens(r) != 1 || r.Session.Leaf() != first {
		t.Errorf("reset holds %d entries at %s, want the 1 at %s", lens(r), r.Session.Leaf(), first)
	}
	w.Quiet()
}

// TestFollowResetOnCutAndRegrown: a log cut back and grown past the
// follower's offset again, with other records, is not the log it was
// reading, though its identity and its size say nothing against it.
func TestFollowResetOnCutAndRegrown(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "s", 3)
	st.Close()
	w := followtest.Start(t, followRO(t, root), "s", "")
	w.NextKind(agentsession.Snapshot)

	logPath := filepath.Join(root, "sessions", "s", logName)
	lines, appends := journalLines(t, root, "s")
	cut := 0
	for _, l := range lines[:appends[2]] {
		cut += len(l)
	}
	if err := os.Truncate(logPath, int64(cut)); err != nil {
		t.Fatal(err)
	}
	st2, _ := Open(root)
	defer st2.Close()
	s, err := st2.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 {
		t.Fatalf("after the cut %d entries, want 2", s.Len())
	}
	mustAppend(t, st2, "s", item("other three"))
	mustAppend(t, st2, "s", item("other four"))
	r := w.NextKind(agentsession.Reset)
	if lens(r) != 4 {
		t.Errorf("reset holds %d entries, want 4", lens(r))
	}
	if _, ok := r.Session.Entry(ids[2]); ok {
		t.Error("the reset holds the entry that was cut")
	}
}

// BenchmarkFollowSnapshot compares the snapshot a follow starts with
// to a Read of the same session, loose and packed.
func BenchmarkFollowSnapshot(b *testing.B) {
	for _, n := range []int{1000, 10000} {
		for _, packed := range []bool{false, true} {
			root := b.TempDir()
			st, _ := Open(root, WithSync(SyncNever))
			ctx := context.Background()
			st.Create(ctx, agentsession.Header{ID: "s"})
			for i := range n {
				if _, err := st.Append(ctx, "s", item(fmt.Sprintf("entry number %d with some text in it", i))); err != nil {
					b.Fatal(err)
				}
			}
			st.Close()
			if packed {
				p, _ := Open(root)
				if _, err := p.Pack(ctx); err != nil {
					b.Fatal(err)
				}
				p.Close()
			}
			ro, _ := Open(root, WithReadOnly())
			name := fmt.Sprintf("%d/packed=%t", n, packed)
			b.Run("read/"+name, func(b *testing.B) {
				for range b.N {
					if _, err := ro.Read(ctx, "s"); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("follow/"+name, func(b *testing.B) {
				for range b.N {
					for c, err := range ro.Follow(ctx, "s", "") {
						if err != nil || c.Kind != agentsession.Snapshot {
							b.Fatal(c.Kind, err)
						}
						break
					}
				}
			})
			ro.Close()
		}
	}
}

// TestFollowYieldsWholeRecordsBeforeDamage: a damaged line ends the
// follow with an error, after the appends whose whole records came
// before it, which a Read shows too.
func TestFollowYieldsWholeRecordsBeforeDamage(t *testing.T) {
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "a", 3)
	st.Close()
	w := followtest.Start(t, followRO(t, root), "a", "")
	from := w.NextKind(agentsession.Snapshot).Cursor
	w.Stop()

	wr, _ := Open(root)
	fourth := mustAppend(t, wr, "a", item("fourth"))
	mustAppend(t, wr, "a", item("fifth"))
	wr.Close()
	damageAppend(t, root, "a", 4)

	w = followtest.Start(t, followRO(t, root), "a", from)
	if c := w.NextKind(agentsession.Appended); c.ID != fourth {
		t.Errorf("appended %s before the damage, want the fourth, %s", c.ID, fourth)
	}
	if err := w.End(); err == nil {
		t.Error("the follow ended without an error at the damaged line")
	}
}
