package cas

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// damageAppend flips a bit in the record of session id's which'th
// append, and returns the log as it was before and the damaged line's
// number.
func damageAppend(t *testing.T, root, id string, which int) (before []byte, line int) {
	t.Helper()
	lines, appends := journalLines(t, root, id)
	path := filepath.Join(root, "sessions", id, logName)
	l := []byte(lines[appends[which]])
	l[len(l)/2] ^= 1
	lines[appends[which]] = string(l)
	data := []byte(strings.Join(lines, ""))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return data, appends[which] + 1
}

// removeObject removes one of an entry's loose objects: its envelope, or
// with content set its body.
func removeObject(t *testing.T, st *Store, id string, content bool) {
	t.Helper()
	sp, hash := spaceEntries, id
	if content {
		c, err := st.contentOf(id)
		if err != nil {
			t.Fatal(err)
		}
		sp, hash = spaceContents, c
	}
	p, _ := st.objs.loosePath(sp, hash)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func droppedIDs(rep RepairReport) []string {
	var out []string
	for _, d := range rep.Dropped {
		out = append(out, d.Entry)
	}
	return out
}

// TestRepair: a damaged middle record hides an entry, which the later
// ones name as their parent by its hash, so it is kept with them; the
// damaged log is kept beside the new one, and the session opens and
// appends again.
func TestRepair(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 5)
	st.Close()
	damaged, line := damageAppend(t, root, "a", 2)

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Damage) != 1 || rep.Damage[0].Line != line {
		t.Errorf("damage: %v, want line %d", rep.Damage, line)
	}
	if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Hidden, ids[2:3]) || len(rep.Dropped) != 0 {
		t.Errorf("kept %v hidden %v dropped %v", rep.Kept, rep.Hidden, rep.Dropped)
	}
	if rep.Head != ids[4] || rep.Named != ids[4] || rep.Mark != MarkRecord {
		t.Errorf("head %s named %s mark %s", rep.Head, rep.Named, rep.Mark)
	}
	if kept, err := os.ReadFile(rep.DamagedLog); err != nil || !bytes.Equal(kept, damaged) {
		t.Errorf("the damaged log is not kept as it was: %v", err)
	}
	s, err := st2.Open(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 5 || s.Leaf() != ids[4] {
		t.Errorf("repaired: len %d leaf %s", s.Len(), s.Leaf())
	}
	next := mustAppend(t, st2, "a", item("after the repair"))
	if p, _ := st2.parentOf(next); p != ids[4] {
		t.Errorf("an append after the repair hangs from %s", p)
	}
	vr, err := st2.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(vr.Problems) != 1 || vr.Problems[0].Kind != "log" || !strings.Contains(vr.Problems[0].Err.Error(), filepath.Base(rep.DamagedLog)) {
		t.Errorf("Verify after a repair: %v", vr.Problems)
	}
	st2.Release("a")
	if _, err := st2.Repair(ctx, "a", RepairOptions{}); !errors.Is(err, ErrNotDamaged) {
		t.Errorf("a second repair: %v", err)
	}
}

// TestRepairDropsWhatHangsFromALostEntry: an entry hanging from one the
// damage hid, whose objects are gone, is dropped; one that does not
// hang from it is kept, wherever it is in the log, and the head the log
// named being dropped, the latest kept leaf takes its place.
func TestRepairDropsWhatHangsFromALostEntry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "b"})
	a := mustAppend(t, st, "b", item("a"))
	x := mustAppend(t, st, "b", item("x"))
	side := agentsession.NewItemEntry(openresponses.UserText("y"))
	side.Parent = a
	y := mustAppend(t, st, "b", side)
	z := mustAppend(t, st, "b", item("z"))
	st.Close()
	damageAppend(t, root, "b", 1)
	removeObject(t, st, x, false)

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "b", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, []string{a, y}) || !slices.Equal(droppedIDs(rep), []string{z}) {
		t.Errorf("kept %v dropped %v", rep.Kept, droppedIDs(rep))
	}
	if rep.Head != y || rep.Named != z {
		t.Errorf("head %s named %s, want the latest kept leaf %s", rep.Head, rep.Named, y)
	}
	if s, err := st2.Open(ctx, "b"); err != nil || s.Leaf() != y {
		t.Errorf("open after the repair: %v", err)
	}
}

// TestRepairMissingObject: an entry whose object is gone is dropped,
// and so is what hangs from it, though its record reads.
func TestRepairMissingObject(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 5)
	st.Close()
	damageAppend(t, root, "a", 4)
	removeObject(t, st, ids[2], false)

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids[:2]) || !slices.Equal(droppedIDs(rep), ids[2:4]) || !errors.Is(rep.Dropped[0].Err, os.ErrNotExist) {
		t.Errorf("kept %v dropped %v", rep.Kept, rep.Dropped)
	}
	if rep.Head != ids[1] {
		t.Errorf("head %s", rep.Head)
	}
}

// TestRepairConvergence: an entry converging one that is not kept is
// dropped, rather than failing the repair.
func TestRepairConvergence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "c"})
	a := mustAppend(t, st, "c", item("a"))
	b := mustAppend(t, st, "c", item("b"))
	merge := agentsession.NewItemEntry(openresponses.UserText("merge"))
	merge.Parent = a
	merge.Parents = []agentsession.EntryRef{{Entry: b}}
	m := mustAppend(t, st, "c", merge)
	st.Close()
	damageAppend(t, root, "c", 1)

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "c", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, []string{a}) || !slices.Equal(droppedIDs(rep), []string{m}) {
		t.Errorf("kept %v dropped %v", rep.Kept, rep.Dropped)
	}
}

// TestRepairDryRun reports what a repair would do and writes nothing,
// on a read-only store too.
func TestRepairDryRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 3)
	st.Close()
	damaged, _ := damageAppend(t, root, "a", 1)
	dir := filepath.Join(root, "sessions", "a")
	before, _ := os.ReadDir(dir)

	for _, opts := range [][]Option{nil, {WithReadOnly()}} {
		st2, _ := Open(root, opts...)
		rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Hidden, ids[1:2]) || rep.DamagedLog != "" {
			t.Errorf("dry run: kept %v hidden %v kept as %q", rep.Kept, rep.Hidden, rep.DamagedLog)
		}
		if _, err := st2.Open(ctx, "a"); !errors.As(err, new(LogDamage)) {
			t.Errorf("open after a dry run: %v", err)
		}
		st2.Close()
	}
	if after, _ := os.ReadFile(filepath.Join(dir, logName)); !bytes.Equal(after, damaged) {
		t.Error("a dry run changed the log")
	}
	if after, _ := os.ReadDir(dir); len(after) != len(before) {
		t.Errorf("a dry run wrote files: %v", after)
	}
}

// TestRepairRefuses a session with nothing to repair, one whose only
// damage lost nothing, one with nothing to keep, one held open, and a
// store that cannot write.
func TestRepairRefuses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "sound", 2)
	lost := fill(t, st, "lost", 2)
	fill(t, st, "held", 2)
	fill(t, st, "newline", 2)
	st.Close()
	damageAppend(t, root, "lost", 0)
	removeObject(t, st, lost[0], false)
	damageAppend(t, root, "held", 1)
	{
		// A record's newline damaged into another byte, both records
		// reading: damage that lost nothing.
		lines, appends := journalLines(t, root, "newline")
		l := []byte(lines[appends[0]])
		l[len(l)-1] = ' '
		lines[appends[0]] = string(l)
		os.WriteFile(filepath.Join(root, "sessions", "newline", logName), []byte(strings.Join(lines, "")), 0o600)
	}

	ro, _ := Open(root, WithReadOnly())
	if _, err := ro.Repair(ctx, "held", RepairOptions{}); !errors.Is(err, agentsession.ErrReadOnly) {
		t.Errorf("repair on a read-only store: %v", err)
	}
	ro.Close()

	st2, _ := Open(root)
	defer st2.Close()
	for _, id := range []string{"sound", "newline"} {
		if _, err := st2.Repair(ctx, id, RepairOptions{}); !errors.Is(err, ErrNotDamaged) {
			t.Errorf("repair of %s: %v", id, err)
		}
	}
	before, _ := os.ReadFile(filepath.Join(root, "sessions", "lost", logName))
	if _, err := st2.Repair(ctx, "lost", RepairOptions{}); !errors.Is(err, ErrUnrecoverable) || !strings.Contains(err.Error(), "Delete") {
		t.Errorf("repair with nothing to keep: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "sessions", "lost", logName)); !bytes.Equal(before, after) {
		t.Error("a repair that kept nothing changed the log")
	}
	if _, err := st2.Repair(ctx, "nope", RepairOptions{}); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("repair of no session: %v", err)
	}
	other, _ := Open(root)
	defer other.Close()
	if _, err := other.Open(ctx, "held"); !errors.As(err, new(LogDamage)) {
		t.Fatalf("open of a damaged session: %v", err)
	}
	if _, err := other.Open(ctx, "sound"); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Repair(ctx, "sound", RepairOptions{}); !errors.Is(err, ErrSessionLocked) {
		t.Errorf("repair of a session another store holds: %v", err)
	}
	if _, err := other.Repair(ctx, "sound", RepairOptions{}); err == nil || !strings.Contains(err.Error(), "release it") {
		t.Errorf("repair of a session this store holds: %v", err)
	}
}

// TestRepairThenSweep: after a repair the sweep runs again, and keeps
// every object the damaged log's readable records name, and the
// ancestors they hang from, though the repaired log names none of them.
func TestRepairThenSweep(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 5)
	st.Close()
	damageAppend(t, root, "a", 2)
	removeObject(t, st, ids[2], true)

	st2, _ := Open(root)
	defer st2.Close()
	if _, err := st2.Sweep(ctx, 0); !errors.As(err, new(LogDamage)) {
		t.Fatalf("a sweep before the repair: %v", err)
	}
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids[:2]) {
		t.Fatalf("kept %v dropped %v", rep.Kept, rep.Dropped)
	}
	if _, err := st2.Sweep(ctx, 0); err != nil {
		t.Fatalf("a sweep after the repair: %v", err)
	}
	for _, id := range append(ids[:2:2], ids[3:]...) {
		if _, err := st2.loadLine(id); err != nil {
			t.Errorf("entry %s after the sweep: %v", id, err)
		}
	}
	if _, err := st2.objs.read(spaceEntries, ids[2]); err != nil {
		t.Errorf("the hidden entry's envelope after the sweep: %v", err)
	}
}

// TestSweepAfterRepairStopsAtCorruption: what a damaged log names is
// kept whether or not it reads, but a fork's prefix needing the same
// entry whole still stops the sweep at its corrupt object.
func TestSweepAfterRepairStopsAtCorruption(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 4)
	if _, err := st.Create(ctx, agentsession.Header{ID: "b", Base: ids[2]}); err != nil {
		t.Fatal(err)
	}
	c, _ := st.contentOf(ids[1])
	st.Close()
	damageAppend(t, root, "a", 3)
	p, _ := st.objs.loosePath(spaceContents, c)
	if err := os.WriteFile(p, []byte(`{"corrupt":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids[:1]) {
		t.Fatalf("kept %v dropped %v", rep.Kept, rep.Dropped)
	}
	if _, err := st2.Sweep(ctx, 0); !errors.Is(err, ErrCorrupt) {
		t.Errorf("a sweep past a fork's corrupt prefix: %v", err)
	}
}
