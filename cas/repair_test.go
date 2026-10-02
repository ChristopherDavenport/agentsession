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
	if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Hidden, ids[2:3]) || len(rep.Salvaged) != 0 || len(rep.Dropped) != 0 {
		t.Errorf("kept %v hidden %v salvaged %v dropped %v", rep.Kept, rep.Hidden, rep.Salvaged, rep.Dropped)
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
	if !slices.Equal(rep.Kept, []string{a, y}) || !slices.Equal(droppedIDs(rep), []string{x, z}) {
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
	if !slices.Equal(rep.Kept, ids[:2]) || !slices.Equal(droppedIDs(rep), ids[2:]) || !errors.Is(rep.Dropped[0].Err, os.ErrNotExist) {
		t.Errorf("kept %v dropped %v", rep.Kept, rep.Dropped)
	}
	if r := dropReason(rep, ids[4]); !strings.Contains(r, "parent "+ids[3]) {
		t.Errorf("the damaged last record's entry: %s", r)
	}
	if rep.Head != ids[1] {
		t.Errorf("head %s", rep.Head)
	}
}

// TestRepairConvergence: an entry converging one that is not kept is
// dropped, rather than failing the repair; one converging an entry
// salvaged from the damaged line before it is kept.
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
	rep, err := st2.Repair(ctx, "c", RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, []string{a, b, m}) || !slices.Equal(rep.Salvaged, []string{b}) || len(rep.Hidden) != 0 || len(rep.Dropped) != 0 {
		t.Errorf("kept %v salvaged %v dropped %v", rep.Kept, rep.Salvaged, rep.Dropped)
	}
	st2.Close()
	removeObject(t, st, b, false)
	st3, _ := Open(root)
	defer st3.Close()
	rep, err = st3.Repair(ctx, "c", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, []string{a}) || !slices.Equal(droppedIDs(rep), []string{b, m}) {
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
	// The fork's commit leaves its base's objects loose, for the test to
	// damage one.
	old := commitPackMin
	commitPackMin = 1 << 30
	defer func() { commitPackMin = old }()
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

// damageField changes one hex digit of the hash the last append record
// of session id spells as field, keeping it well-formed.
func damageField(t *testing.T, root, id, field string) {
	t.Helper()
	lines, appends := journalLines(t, root, id)
	l := []byte(lines[appends[len(appends)-1]])
	k := bytes.Index(l, []byte(`"`+field+`":"sha256:`))
	if k < 0 {
		t.Fatalf("the last append record has no %s", field)
	}
	at := k + len(`"`+field+`":"sha256:`) + 10
	if l[at] == '0' {
		l[at] = '1'
	} else {
		l[at] = '0'
	}
	lines[appends[len(appends)-1]] = string(l)
	if err := os.WriteFile(filepath.Join(root, "sessions", id, logName), []byte(strings.Join(lines, "")), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestRepairSalvagesLastAppend: damage to the last append record's
// entry leaves its head naming the entry, which no child names; the
// repair salvages it from the hash the line still spells, reports it,
// and keeps it as the head, and a sweep after removes nothing of it
// (#167).
func TestRepairSalvagesLastAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 28)
	st.Close()
	damageField(t, root, "a", "entry")

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Salvaged, ids[27:]) || len(rep.Dropped) != 1 {
		t.Fatalf("kept %d, salvaged %v, dropped %v", len(rep.Kept), rep.Salvaged, rep.Dropped)
	}
	if d := rep.Dropped[0]; slices.Contains(ids, d.Entry) || !strings.Contains(d.Err.Error(), "made it of "+ids[27]) {
		t.Errorf("the hash the damage made: %s: %v", d.Entry, d.Err)
	}
	if rep.Head != ids[27] || rep.Named != ids[27] || rep.Unread {
		t.Errorf("head %s named %s unread %v, want %s", rep.Head, rep.Named, rep.Unread, ids[27])
	}
	if _, err := st2.Sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if _, err := st2.loadLine(id); err != nil {
			t.Errorf("entry %s after the sweep: %v", id, err)
		}
	}
	s, err := st2.Open(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 28 || s.Leaf() != ids[27] {
		t.Errorf("repaired: len %d leaf %s", s.Len(), s.Leaf())
	}
}

// TestRepairLastAppendHeadDamaged: damage to the last append record's
// head salvages the entry, which the line no longer names as the head;
// the report says a damaged line follows the last head read, and does
// not claim the head the log last named (#167).
func TestRepairLastAppendHeadDamaged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 5)
	st.Close()
	damageField(t, root, "a", "head")

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Salvaged, ids[4:]) || len(rep.Dropped) != 1 || !strings.Contains(rep.Dropped[0].Err.Error(), "made it of "+ids[4]) {
		t.Fatalf("kept %v, salvaged %v, dropped %v", rep.Kept, rep.Salvaged, rep.Dropped)
	}
	if !rep.Unread || rep.Named != "" || rep.Head != ids[3] {
		t.Errorf("head %s named %q unread %v, want %s, unknown", rep.Head, rep.Named, rep.Unread, ids[3])
	}
}

// TestRepairSalvageKeepsLaterHead: an entry salvaged from a damaged
// append record is not the head when a readable head record after it
// moved the head elsewhere.
func TestRepairSalvageKeepsLaterHead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 4)
	if err := st.SetHead(ctx, "a", ids[3], ids[1]); err != nil {
		t.Fatal(err)
	}
	st.Close()
	damageField(t, root, "a", "entry")

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Salvaged, ids[3:]) || rep.Head != ids[1] || rep.Named != ids[1] || rep.Unread || len(rep.Dropped) != 1 {
		t.Errorf("salvaged %v head %s named %s unread %v dropped %v, want the head at %s", rep.Salvaged, rep.Head, rep.Named, rep.Unread, rep.Dropped, ids[1])
	}
}

// TestRepairSalvagesOnlyWhatReads: a damaged last record whose entry's
// objects are gone salvages nothing, and the one entry of a session so
// damaged is salvaged from its record alone.
func TestRepairSalvagesOnlyWhatReads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 3)
	one := fill(t, st, "b", 1)
	st.Close()
	damageField(t, root, "a", "entry")
	removeObject(t, st, ids[2], true)
	damageField(t, root, "b", "entry")

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, ids[:2]) || len(rep.Salvaged) != 0 || !rep.Unread || len(rep.Dropped) != 2 {
		t.Errorf("kept %v, salvaged %v, unread %v, dropped %v", rep.Kept, rep.Salvaged, rep.Unread, rep.Dropped)
	}
	if r := dropReason(rep, ids[2]); !strings.Contains(r, "do not read") {
		t.Errorf("the entry whose content is gone: %s", r)
	}
	rep, err = st2.Repair(ctx, "b", RepairOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rep.Kept, one) || !slices.Equal(rep.Salvaged, one) || rep.Head != one[0] || len(rep.Dropped) != 1 {
		t.Errorf("kept %v, salvaged %v, head %s, dropped %v", rep.Kept, rep.Salvaged, rep.Head, rep.Dropped)
	}
}

// TestSweepKeepsWhatDamagedLinesSpell: while a damaged log is kept, a
// sweep keeps every object a hash in it names, though only a damaged
// record names it and the repaired log does not (#167).
func TestSweepKeepsWhatDamagedLinesSpell(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 3)
	st.Close()
	damageField(t, root, "a", "entry")
	// The last entry's content goes, so the repair drops it; its
	// envelope stays, named only by the damaged line.
	removeObject(t, st, ids[2], true)

	st2, _ := Open(root)
	defer st2.Close()
	rep, err := st2.Repair(ctx, "a", RepairOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(rep.Kept, ids[2]) {
		t.Fatalf("kept %v", rep.Kept)
	}
	if _, err := st2.Sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.objs.read(spaceEntries, ids[2]); err != nil {
		t.Errorf("the envelope only a damaged line names, after the sweep: %v", err)
	}
}

// damageMember flips a bit in the first byte of member's value in the
// first line of session id's log that spells spell, so the record fails
// its checksum while every hash the line spells stays legible, and
// returns the line's number.
func damageMember(t *testing.T, root, id, spell, member string) int {
	t.Helper()
	path := filepath.Join(root, "sessions", id, logName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	for i, l := range lines {
		if !strings.Contains(l, spell) {
			continue
		}
		k := strings.Index(l, `"`+member+`":"`)
		if k < 0 {
			t.Fatalf("line %d of %s has no %s", i+1, id, member)
		}
		b := []byte(l)
		b[k+len(member)+4] ^= 1
		lines[i] = string(b)
		if err := os.WriteFile(path, []byte(strings.Join(lines, "")), 0o600); err != nil {
			t.Fatal(err)
		}
		return i + 1
	}
	t.Fatalf("no line of %s spells %s", id, spell)
	return 0
}

// dropReason returns why a repair dropped entry, or "" when it did not.
func dropReason(rep RepairReport, entry string) string {
	for _, d := range rep.Dropped {
		if d.Entry == entry {
			return d.Err.Error()
		}
	}
	return ""
}

// TestRepairSalvagesAnyDamagedRecord: an entry whose append record is
// damaged outside its hashes, in its session or op member, or whose
// record a readable append follows, is salvaged from the hashes the line
// still spells, kept, reported, and kept by a sweep after, with and
// without the damaged log beside the new one (#189). Before, only a
// damaged line after the last readable append that still spelled the
// session and an append was read.
func TestRepairSalvagesAnyDamagedRecord(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		member string
		// setup fills session id and returns its entries and which one's
		// record to damage; kept and head say what the repair keeps, in
		// log order, and which is the head.
		setup func(t *testing.T, st *Store, id string) (ids []string, target int)
		kept  func(ids []string) []string
		head  func(ids []string) string
	}{
		{"the one append of a session, damaged in session", "session",
			func(t *testing.T, st *Store, id string) ([]string, int) { return fill(t, st, id, 1), 0 },
			func(ids []string) []string { return ids },
			func(ids []string) string { return ids[0] }},
		{"the last of three, damaged in op", "op",
			func(t *testing.T, st *Store, id string) ([]string, int) { return fill(t, st, id, 3), 2 },
			func(ids []string) []string { return ids },
			func(ids []string) string { return ids[2] }},
		{"a branch leaf whose record a readable append follows", "session",
			func(t *testing.T, st *Store, id string) ([]string, int) {
				ids := fill(t, st, id, 3)
				if err := st.SetHead(ctx, id, ids[2], ids[1]); err != nil {
					t.Fatal(err)
				}
				return append(ids, mustAppend(t, st, id, item("four, from two"))), 2
			},
			func(ids []string) []string { return ids },
			func(ids []string) string { return ids[3] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			st, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			ids, target := c.setup(t, st, "a")
			st.Close()
			line := damageMember(t, root, "a", ids[target], c.member)

			st2, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer st2.Close()
			rep, err := st2.Repair(ctx, "a", RepairOptions{})
			if err != nil {
				t.Fatalf("repair: %v", err)
			}
			if len(rep.Damage) != 1 || rep.Damage[0].Line != line {
				t.Errorf("damage %v, want line %d", rep.Damage, line)
			}
			if !slices.Equal(rep.Kept, c.kept(ids)) || !slices.Equal(rep.Salvaged, ids[target:target+1]) || len(rep.Dropped) != 0 {
				t.Errorf("kept %v salvaged %v dropped %v, want kept %v salvaged %v", rep.Kept, rep.Salvaged, rep.Dropped, c.kept(ids), ids[target:target+1])
			}
			if head := c.head(ids); rep.Head != head || rep.Named != head || rep.Unread {
				t.Errorf("head %s named %s unread %v, want %s", rep.Head, rep.Named, rep.Unread, head)
			}
			// A sweep keeps the salvaged entry's objects while the damaged
			// log is kept, and once it is removed, since the repaired log
			// names it.
			for _, keepDamaged := range []bool{true, false} {
				if !keepDamaged {
					if err := os.Remove(rep.DamagedLog); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := st2.Sweep(ctx, 0); err != nil {
					t.Fatalf("sweep (damaged log kept %v): %v", keepDamaged, err)
				}
				for _, id := range ids {
					if _, err := st2.loadLine(id); err != nil {
						t.Errorf("entry %s after a sweep (damaged log kept %v): %v", id, keepDamaged, err)
					}
				}
			}
			s, err := st2.Open(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if s.Len() != len(ids) || s.Leaf() != c.head(ids) {
				t.Errorf("repaired: len %d leaf %s, want %d and %s", s.Len(), s.Leaf(), len(ids), c.head(ids))
			}
			if !slices.Contains(s.Leaves(), ids[target]) {
				t.Errorf("leaves %v lack the salvaged entry %s", s.Leaves(), ids[target])
			}
		})
	}
}

// TestRepairListsWhatItDoesNotSalvage: every hash a damaged line spells
// that the repair does not salvage is in Dropped with why: one the
// damage made of another, held by no object; one whose objects are
// gone; one whose parent is not kept; and one a damaged lost record
// still legibly names, which recovery told the writer was lost and a
// salvage would bring back (#189).
func TestRepairListsWhatItDoesNotSalvage(t *testing.T) {
	ctx := context.Background()
	t.Run("a hash the damage made of another", func(t *testing.T) {
		root := t.TempDir()
		st, _ := Open(root)
		ids := fill(t, st, "a", 2)
		st.Close()
		damageField(t, root, "a", "entry")
		st2, _ := Open(root)
		defer st2.Close()
		rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rep.Kept, ids) || !slices.Equal(rep.Salvaged, ids[1:]) || len(rep.Dropped) != 1 {
			t.Fatalf("kept %v salvaged %v dropped %v", rep.Kept, rep.Salvaged, rep.Dropped)
		}
		if d := rep.Dropped[0]; slices.Contains(ids, d.Entry) || !strings.Contains(d.Err.Error(), "made it of "+ids[1]) || !errors.Is(d.Err, os.ErrNotExist) {
			t.Errorf("dropped %s: %v", d.Entry, d.Err)
		}
	})
	t.Run("objects gone, and a child whose parent is not kept", func(t *testing.T) {
		root := t.TempDir()
		st, _ := Open(root)
		ids := fill(t, st, "a", 4)
		st.Close()
		damageMember(t, root, "a", ids[2], "session")
		damageMember(t, root, "a", ids[3], "session")
		removeObject(t, st, ids[2], false)
		st2, _ := Open(root)
		defer st2.Close()
		rep, err := st2.Repair(ctx, "a", RepairOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rep.Kept, ids[:2]) || len(rep.Salvaged) != 0 || !slices.Equal(droppedIDs(rep), ids[2:]) {
			t.Fatalf("kept %v salvaged %v dropped %v", rep.Kept, rep.Salvaged, rep.Dropped)
		}
		if r := dropReason(rep, ids[2]); !strings.Contains(r, "damaged record") || !errors.Is(rep.Dropped[0].Err, os.ErrNotExist) {
			t.Errorf("%s dropped: %s", ids[2], r)
		}
		if r := dropReason(rep, ids[3]); !strings.Contains(r, "parent "+ids[2]) || !strings.Contains(r, "not kept") {
			t.Errorf("%s dropped: %s", ids[3], r)
		}
		if rep.Head != ids[1] || !rep.Unread {
			t.Errorf("head %s unread %v, want %s and unread", rep.Head, rep.Unread, ids[1])
		}
	})
	t.Run("a damaged lost record", func(t *testing.T) {
		root := t.TempDir()
		st, _ := Open(root, WithSync(SyncNever))
		st.Create(ctx, agentsession.Header{ID: "l"})
		one := mustAppend(t, st, "l", item("one"))
		two := mustAppend(t, st, "l", item("two"))
		branch := agentsession.NewItemEntry(openresponses.UserText("three, from one"))
		branch.Parent = one
		three := mustAppend(t, st, "l", branch)
		die(st)
		// two's envelope is gone, so recovery records two lost and three,
		// accepted after it, with it, though three's objects are whole and
		// its parent, one, is kept: only the lost record stands against it.
		removeObject(t, st, two, false)
		st2, _ := Open(root)
		if _, err := st2.Open(ctx, "l"); err != nil {
			t.Fatal(err)
		}
		four := mustAppend(t, st2, "l", item("four"))
		st2.Close()
		if _, err := st2.loadLine(three); err != nil {
			t.Fatalf("three's objects: %v", err)
		}
		damageMember(t, root, "l", `"op":"lost","session":"l","entry":"`+three, "session")

		st3, _ := Open(root)
		defer st3.Close()
		rep, err := st3.Repair(ctx, "l", RepairOptions{DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(rep.Kept, []string{one, four}) || len(rep.Salvaged) != 0 || !slices.Equal(droppedIDs(rep), []string{three}) {
			t.Fatalf("kept %v salvaged %v dropped %v", rep.Kept, rep.Salvaged, rep.Dropped)
		}
		if r := dropReason(rep, three); !strings.Contains(r, "lost") {
			t.Errorf("three dropped: %s", r)
		}
		if rep.Head != four || rep.Named != four {
			t.Errorf("head %s named %s, want %s", rep.Head, rep.Named, four)
		}
	})
}
