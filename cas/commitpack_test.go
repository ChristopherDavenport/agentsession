package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// countSyncs counts the object and directory fsyncs made until the
// returned function restores the hooks.
func countSyncs(t *testing.T) (objects, dirs *atomic.Int64) {
	t.Helper()
	objects, dirs = new(atomic.Int64), new(atomic.Int64)
	oldObj, oldDir := syncObject, syncDirFile
	syncObject = func(f *os.File) error { objects.Add(1); return oldObj(f) }
	syncDirFile = func(d *os.File) error { dirs.Add(1); return oldDir(d) }
	t.Cleanup(func() { syncObject, syncDirFile = oldObj, oldDir })
	return objects, dirs
}

// TestCommitPack: a commit owing enough lazily written objects writes
// them as one pack, in a few fsyncs rather than one for each object and
// its directory, and removes their loose copies; the session reads the
// same after reopening, and Verify finds nothing (#142).
func TestCommitPack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root, WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	const n = 20
	for i := range n {
		mustAppend(t, st, "s", item(fmt.Sprintf("lazy %d", i)))
	}
	packs := len(st.objs.packList())
	objects, dirs := countSyncs(t)
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := objects.Load(); got != 0 {
		t.Errorf("%d object fsyncs, want none", got)
	}
	if got := dirs.Load(); got > 4 {
		t.Errorf("%d directory fsyncs for one pack", got)
	}
	if got := len(st.objs.packList()); got != packs+1 {
		t.Errorf("%d packs after the commit, want %d", got, packs+1)
	}
	if got := looseCount(t, st); got != 0 {
		t.Errorf("%d loose objects left beside the pack", got)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != n {
		t.Errorf("reopened with %d entries, want %d", s.Len(), n)
	}
	rep, err := st.Verify(ctx)
	if err != nil || !rep.OK() {
		t.Errorf("Verify: %v %v", rep.Problems, err)
	}
}

// TestCommitPackBelowMin: a commit owing few objects fsyncs each, and
// writes no pack.
func TestCommitPackBelowMin(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("lazy"))
	objects, _ := countSyncs(t)
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := objects.Load(); got != 2 {
		t.Errorf("%d object fsyncs, want 2", got)
	}
	if got := len(st.objs.packList()); got != 0 {
		t.Errorf("%d packs", got)
	}
}

// TestCommitPackUnderRecords: under a header whose records name runs, a
// lazy policy commits at each run, owing only the few objects since the
// last, and those still go as one pack (#170).
func TestCommitPackUnderRecords(t *testing.T) {
	ctx := context.Background()
	old := autoPackPacks
	autoPackPacks = 1 << 30
	defer func() { autoPackPacks = old }()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s", Records: agentsession.AllRecords}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("lazy 0"))
	mustAppend(t, st, "s", item("lazy 1"))
	objects, _ := countSyncs(t)
	r, err := st.Write(ctx, "s", agentsession.NewRunStart("run-1", agentsession.SourceInput, ""))
	if err != nil || !r.Durable {
		t.Fatalf("a run the header records: durable %v, %v", r.Durable, err)
	}
	if got := objects.Load(); got > 1 {
		t.Errorf("%d object fsyncs; want the pack's index alone", got)
	}
	if got := len(st.objs.packList()); got != 1 {
		t.Errorf("%d packs after the run's commit, want 1", got)
	}
	// The run's own objects, written durably, stay loose.
	if got := looseCount(t, st); got != 2 {
		t.Errorf("%d loose objects left beside the pack, want the run's 2", got)
	}
}

// TestCommitPacksMerge: once commits have written autoPackPacks packs,
// the store packs on its own after the next commit and merges them,
// without waiting for autoPackEvery appends or Close.
func TestCommitPacksMerge(t *testing.T) {
	ctx := context.Background()
	old := autoPackPacks
	autoPackPacks = 4
	defer func() { autoPackPacks = old }()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	most := 0
	for c := range 3 * autoPackPacks {
		for i := range commitPackMin {
			mustAppend(t, st, "s", item(fmt.Sprintf("commit %d lazy %d", c, i)))
		}
		if err := st.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		st.background.Wait()
		most = max(most, st.objs.packCount())
	}
	if most > autoPackPacks {
		t.Errorf("the store reached %d packs, past %d", most, autoPackPacks)
	}
}

// TestConsolidatePastCorrupt: an object that fails its name in one pack
// keeps that pack, and only that one, from being merged away; the rest
// merge.
func TestConsolidatePastCorrupt(t *testing.T) {
	ctx := context.Background()
	old := autoPackPacks
	autoPackPacks = 1 << 30
	defer func() { autoPackPacks = old }()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	var bad string
	const rounds = 6
	for c := range rounds {
		for i := range commitPackMin {
			id := mustAppend(t, st, "s", item(fmt.Sprintf("commit %d lazy %d", c, i)))
			if bad == "" {
				bad = id
			}
		}
		if err := st.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.objs.packCount(); n != rounds {
		t.Fatalf("%d packs, want %d", n, rounds)
	}
	d, _ := digestOf(bad)
	var holder *pack
	for _, p := range st.objs.packList() {
		if _, _, ok := p.find(spaceEntries, d); ok {
			holder = p
		}
	}
	off, _, _ := holder.find(spaceEntries, d)
	f, err := os.OpenFile(holder.path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{'!'}, off)
	f.Close()
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, p := range st.objs.packList() {
		names[p.name] = true
	}
	if len(names) != 2 || !names[holder.name] {
		t.Errorf("after merging: %d packs, the damaged one kept: %v", len(names), names[holder.name])
	}
	rep, err := st.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() {
		t.Error("Verify no longer reports the damaged object")
	}
}

// TestFlushFailureKeepsDirs: a flush that fails on one file, for a
// reason that is no failed fsync, still owes the directories of the
// files it did fsync, since their renames are not yet durable.
func TestFlushFailureKeepsDirs(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root opens a file without permission")
	}
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o := st.objs
	var paths []string
	for i := range 3 {
		data := []byte(fmt.Sprintf("object %d", i))
		hash := hashBytes(data)
		if err := o.write(spaceContents, hash, data, false); err != nil {
			t.Fatal(err)
		}
		p, _ := o.loosePath(spaceContents, hash)
		paths = append(paths, p)
	}
	if err := os.Chmod(paths[0], 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(paths[0], 0o644)
	if err := o.flush(); err == nil {
		t.Fatal("a flush that could not open a file succeeded")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range paths {
		if !o.pendDirs[filepath.Dir(p)] {
			t.Errorf("the directory of %s is no longer owed", o.rel(p))
		}
	}
}

// TestVerifyLooseBesidePack: a loose copy that fails its name beside a
// pack holding the object, as a crash can leave the removal of a commit
// pack's unsynced copies, is no problem; one with no good copy is.
func TestVerifyLooseBesidePack(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	var ids []string
	for i := range commitPackMin {
		ids = append(ids, mustAppend(t, st, "s", item(fmt.Sprintf("lazy %d", i))))
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	p, _ := st.objs.loosePath(spaceEntries, ids[0])
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, 40), 0o644); err != nil {
		t.Fatal(err)
	}
	if rep, err := st.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("a zeroed duplicate of a packed object: %v %v", rep.Problems, err)
	}
	loose := mustAppend(t, st, "s", item("loose"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	c, _ := st.contentOf(loose)
	q, _ := st.objs.loosePath(spaceContents, c)
	if err := os.WriteFile(q, []byte("not what it is named"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep, _ := st.Verify(ctx)
	if rep.OK() || !slices.ContainsFunc(rep.Problems, func(p Problem) bool { return p.Kind == "corrupt" && p.Object == c }) {
		t.Errorf("a corrupt object a log needs, with no good copy: %v", rep.Problems)
	}
}

// commitPacks makes a store at root holding n commit packs, with the
// automatic pack held off, and closes it.
func commitPacks(t *testing.T, root string, n int) {
	t.Helper()
	ctx := context.Background()
	old := autoPackPacks
	autoPackPacks = 1 << 30
	defer func() { autoPackPacks = old }()
	st, err := Open(root, WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	for c := range n {
		for i := range commitPackMin {
			mustAppend(t, st, "s", item(fmt.Sprintf("commit %d lazy %d", c, i)))
		}
		if err := st.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestSweepBesideOwnPack: a caller's Sweep or Pack waits for the pack a
// store runs on its own, here the one Open starts for the packs a
// killed process left, rather than finding the gc lock taken.
func TestSweepBesideOwnPack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	commitPacks(t, root, 8)
	old := autoPackPacks
	autoPackPacks = 4
	defer func() { autoPackPacks = old }()
	for _, op := range []string{"sweep", "pack"} {
		st, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		if op == "sweep" {
			_, err = st.Sweep(ctx, time.Hour)
		} else {
			_, err = st.Pack(ctx)
		}
		if err != nil {
			t.Errorf("a %s beside the store's own: %v", op, err)
		}
		st.Close()
	}
}

// TestPackCountReloads: a store whose packs another process merged
// does not go on packing on a count it last saw.
func TestPackCountReloads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	commitPacks(t, root, 8)
	old := autoPackPacks
	autoPackPacks = 1 << 30
	a, err := Open(root, WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err := a.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	b, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	b.Close()
	autoPackPacks = 4
	defer func() { autoPackPacks = old }()
	if a.objs.packCount() < autoPackPacks {
		t.Fatalf("the first store already sees %d packs", a.objs.packCount())
	}
	if err := a.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	a.background.Wait()
	if n := a.objs.packCount(); n >= autoPackPacks {
		t.Errorf("after a look, the store still counts %d packs", n)
	}
}

// TestGeometricSplit: the split is taken from the largest pack down, as
// git's repack --geometric=2 takes it.
func TestGeometricSplit(t *testing.T) {
	for _, c := range []struct {
		sizes []int64
		want  int
	}{
		{nil, 0},
		{[]int64{5}, 0},
		{[]int64{1, 2, 4, 8}, 0},
		{[]int64{1, 1}, 2},
		{[]int64{1, 1, 4, 8}, 2},
		{[]int64{1, 1, 3, 8}, 4},
		{[]int64{1, 1, 3, 16}, 3},
		// One small pack under half the next stopped a merge walking up
		// from the smallest at its first step.
		{[]int64{2044, 7769, 7834, 7900, 7950}, 5},
		{[]int64{100, 1000, 1000, 1000, 100000}, 4},
	} {
		if got := geometricSplit(c.sizes); got != c.want {
			t.Errorf("geometricSplit(%v) = %d, want %d", c.sizes, got, c.want)
		}
	}
}

// smallPackThenCommits writes one small pack, by a Pack of a single
// append's objects, then commits commit packs, with st's automatic pack
// as it stands, and returns the most packs the store held after any
// commit.
func smallPackThenCommits(t *testing.T, st *Store, commits int) int {
	t.Helper()
	ctx := context.Background()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("small"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pack(ctx); err != nil {
		t.Fatal(err)
	}
	most := 0
	for c := range commits {
		for i := range commitPackMin {
			mustAppend(t, st, "s", item(fmt.Sprintf("commit %d lazy %d", c, i)))
		}
		if err := st.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		st.background.Wait()
		most = max(most, st.objs.packCount())
	}
	return most
}

// TestPackMergesPastSmallPack: one small pack, under half the size of
// the commit packs after it, does not keep Pack from merging them
// (#169).
func TestPackMergesPastSmallPack(t *testing.T) {
	old := autoPackPacks
	autoPackPacks = 1 << 30
	defer func() { autoPackPacks = old }()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const commits = 70
	smallPackThenCommits(t, st, commits)
	if n := st.objs.packCount(); n != commits+1 {
		t.Fatalf("%d packs before Pack, want %d", n, commits+1)
	}
	if _, err := st.Pack(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := st.objs.packCount(); n >= 64 {
		t.Errorf("%d packs after Pack", n)
	}
}

// TestAutoPackMergesPastSmallPack: nor does it keep the pack the store
// starts on its count from bringing the count down (#169).
func TestAutoPackMergesPastSmallPack(t *testing.T) {
	old := autoPackPacks
	autoPackPacks = 8
	defer func() { autoPackPacks = old }()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if most := smallPackThenCommits(t, st, 4*autoPackPacks); most > autoPackPacks {
		t.Errorf("the store reached %d packs, past %d", most, autoPackPacks)
	}
	if st.packStuck.Load() {
		t.Error("the store's own pack is stuck")
	}
}
