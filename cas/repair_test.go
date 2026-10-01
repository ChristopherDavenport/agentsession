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

func droppedIDs(rep RepairReport) []string {
	var out []string
	for _, d := range rep.Dropped {
		out = append(out, d.Entry)
	}
	return out
}

// TestRepair: a damaged middle record hides an entry the later ones
// hang from, so they are dropped with it; the head the log named is
// gone, so the latest kept leaf takes its place; the damaged log is
// kept beside the new one, and the session opens and appends again.
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
	if !slices.Equal(rep.Kept, ids[:2]) || !slices.Equal(droppedIDs(rep), ids[3:]) {
		t.Errorf("kept %v dropped %v", rep.Kept, droppedIDs(rep))
	}
	if rep.Head != ids[1] || rep.Named != ids[4] || rep.Mark != MarkRecord {
		t.Errorf("head %s named %s mark %s", rep.Head, rep.Named, rep.Mark)
	}
	if kept, err := os.ReadFile(rep.DamagedLog); err != nil || !bytes.Equal(kept, damaged) {
		t.Errorf("the damaged log is not kept as it was: %v", err)
	}
	s, err := st2.Open(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != ids[1] {
		t.Errorf("repaired: len %d leaf %s", s.Len(), s.Leaf())
	}
	next := mustAppend(t, st2, "a", item("after the repair"))
	if p, _ := st2.parentOf(next); p != ids[1] {
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

// TestRepairKeepsIndependentEntries: an entry that does not hang from
// the hidden one is kept, wherever it is in the log.
func TestRepairKeepsIndependentEntries(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "b"})
	a := mustAppend(t, st, "b", item("a"))
	mustAppend(t, st, "b", item("x"))
	side := agentsession.NewItemEntry(openresponses.UserText("y"))
	side.Parent = a
	y := mustAppend(t, st, "b", side)
	z := mustAppend(t, st, "b", item("z"))
	st.Close()
	damageAppend(t, root, "b", 1)

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
	p, _ := st.objs.loosePath(spaceEntries, ids[2])
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

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
		if !slices.Equal(rep.Kept, ids[:1]) || !slices.Equal(droppedIDs(rep), ids[2:]) || rep.DamagedLog != "" {
			t.Errorf("dry run: kept %v dropped %v kept as %q", rep.Kept, droppedIDs(rep), rep.DamagedLog)
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

// TestRepairRefuses a session with nothing to repair, one with nothing
// to keep, one held open, and a store that cannot write.
func TestRepairRefuses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "sound", 2)
	fill(t, st, "lost", 2)
	fill(t, st, "held", 2)
	st.Close()
	damageAppend(t, root, "lost", 0)
	damageAppend(t, root, "held", 1)

	ro, _ := Open(root, WithReadOnly())
	if _, err := ro.Repair(ctx, "held", RepairOptions{}); !errors.Is(err, agentsession.ErrReadOnly) {
		t.Errorf("repair on a read-only store: %v", err)
	}
	ro.Close()

	st2, _ := Open(root)
	defer st2.Close()
	if _, err := st2.Repair(ctx, "sound", RepairOptions{}); !errors.Is(err, ErrNotDamaged) {
		t.Errorf("repair of a sound log: %v", err)
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

	st2, _ := Open(root)
	defer st2.Close()
	if _, err := st2.Sweep(ctx, 0); !errors.As(err, new(LogDamage)) {
		t.Fatalf("a sweep before the repair: %v", err)
	}
	if _, err := st2.Repair(ctx, "a", RepairOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Sweep(ctx, 0); err != nil {
		t.Fatalf("a sweep after the repair: %v", err)
	}
	for _, id := range ids {
		if _, err := st2.loadLine(id); err != nil {
			t.Errorf("entry %s after the sweep: %v", id, err)
		}
	}
}
