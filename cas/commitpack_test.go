package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

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
	for i := range 3 {
		mustAppend(t, st, "s", item(fmt.Sprintf("lazy %d", i)))
	}
	objects, _ := countSyncs(t)
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := objects.Load(); got != 6 {
		t.Errorf("%d object fsyncs, want 6", got)
	}
	if got := len(st.objs.packList()); got != 0 {
		t.Errorf("%d packs", got)
	}
}

// TestCommitPacksMerge: once commits have written autoPackPacks packs,
// the store packs on its own and merges them.
func TestCommitPacksMerge(t *testing.T) {
	ctx := context.Background()
	old := autoPackPacks
	autoPackPacks = 4
	defer func() { autoPackPacks = old }()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	for c := range autoPackPacks {
		for i := range commitPackMin {
			mustAppend(t, st, "s", item(fmt.Sprintf("commit %d lazy %d", c, i)))
		}
		if err := st.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}
	before := len(st.objs.packList())
	if before < autoPackPacks {
		t.Fatalf("%d packs, want %d", before, autoPackPacks)
	}
	st.maybePack()
	if after := len(st.objs.packList()); after >= before {
		t.Errorf("%d packs after an automatic pack, %d before", after, before)
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
	bad := []byte("not what it is named")
	q, _ := st.objs.loosePath(spaceContents, hashBytes([]byte("something else")))
	os.MkdirAll(filepath.Dir(q), 0o755)
	os.WriteFile(q, bad, 0o644)
	if rep, _ := st.Verify(ctx); rep.OK() {
		t.Error("a corrupt object with no good copy passed")
	}
}
