package cas

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// sameScan reports how two scans differ, or "" when they do not.
func sameScan(a, b *journalScan) string {
	if a.end != b.end || a.lines != b.lines {
		return fmt.Sprintf("end %d/%d, lines %d/%d", a.end, b.end, a.lines, b.lines)
	}
	if len(a.damage) != len(b.damage) {
		return fmt.Sprintf("%d/%d damaged", len(a.damage), len(b.damage))
	}
	for i := range a.damage {
		x, y := a.damage[i], b.damage[i]
		if x.Line != y.Line || x.Offset != y.Offset || x.Err.Error() != y.Err.Error() {
			return fmt.Sprintf("damage %d: %v / %v", i, x, y)
		}
	}
	if len(a.states) != len(b.states) {
		return fmt.Sprintf("%d/%d sessions", len(a.states), len(b.states))
	}
	for id, x := range a.states {
		y, ok := b.states[id]
		if !ok {
			return "no session " + id
		}
		if x.deleted != y.deleted || x.created != y.created || x.base != y.base || len(x.recs) != len(y.recs) {
			return fmt.Sprintf("session %s: %+v / %+v", id, *x, *y)
		}
		for i := range x.recs {
			if x.recs[i] != y.recs[i] {
				return fmt.Sprintf("session %s record %d: %+v / %+v", id, i, x.recs[i], y.recs[i])
			}
		}
	}
	return ""
}

func lowCheckpoints(t *testing.T) {
	old := checkpointEvery
	checkpointEvery = 1
	t.Cleanup(func() { checkpointEvery = old })
}

// checkpointStore fills a store with what the journal can say: creates,
// lazy and durable appends, heads, marks, a fork, a delete, a record
// whose strings are not hashes, and a damaged line.
func checkpointStore(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root, WithSync(SyncOnResponse))
	if err != nil {
		t.Fatal(err)
	}
	st.Create(ctx, agentsession.Header{ID: "a"})
	first := mustAppend(t, st, "a", item("one"))
	mustAppend(t, st, "a", item("two"))
	if _, err := st.Write(ctx, "a", &agentsession.ResponseEntry{ResponseID: "r", Status: openresponses.ResponseStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetHead(ctx, "a", st.open["a"].head, first); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "f", Base: first, ParentSession: "a"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "f", item("forked"))
	st.Create(ctx, agentsession.Header{ID: "gone"})
	mustAppend(t, st, "gone", item("x"))
	if err := st.Delete(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if err := st.commit(true, journalRecord{Op: "mark", Session: "a", Mark: "not-a-hash", Head: "also:not"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(root, "journal"), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString("{\"op\":\"append\",\"session\":\"a\",\"crc\":\"0\"}\n")
	f.Close()
	if err := st.commit(true, journalRecord{Op: "mark", Session: "a", Mark: MarkRecord}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestCheckpoint: a store writes a checkpoint, the next open starts from
// it, and what it replays equals a replay of the whole journal, with
// the records written after the checkpoint read on.
func TestCheckpoint(t *testing.T) {
	lowCheckpoints(t)
	ctx := context.Background()
	root := checkpointStore(t)
	if _, err := os.Stat(filepath.Join(root, checkpointName)); err != nil {
		t.Fatalf("no checkpoint after Close: %v", err)
	}
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.checkpointed == 0 {
		t.Error("open did not start from the checkpoint")
	}
	got, _ := st.replay()
	want, _ := st.replayFrom(0)
	if d := sameScan(got, want); d != "" {
		t.Errorf("from the checkpoint and from the start differ: %s", d)
	}
	if len(want.damage) != 1 {
		t.Errorf("%d damaged lines, want 1", len(want.damage))
	}
	// Records after the checkpoint, from another store.
	other, _ := Open(root)
	mustAppend(t, other, "f", item("later"))
	other.Release("f")
	got, _ = st.replay()
	want, _ = st.replayFrom(0)
	if d := sameScan(got, want); d != "" {
		t.Errorf("after reading on: %s", d)
	}
	other.Close()
	st.Close()

	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	s, err := ro.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 {
		t.Errorf("fork from a checkpointed store: %d entries, want 3", s.Len())
	}
}

// TestCheckpointIgnored: a checkpoint that does not match the journal,
// or does not read, is not used, and the store replays from the start.
func TestCheckpointIgnored(t *testing.T) {
	lowCheckpoints(t)
	for _, tc := range []struct {
		name   string
		break_ func(t *testing.T, root string)
	}{
		{"flipped byte", func(t *testing.T, root string) {
			p := filepath.Join(root, checkpointName)
			data, _ := os.ReadFile(p)
			data[len(data)/2] ^= 1
			os.WriteFile(p, data, 0o644)
		}},
		{"cut short", func(t *testing.T, root string) {
			p := filepath.Join(root, checkpointName)
			data, _ := os.ReadFile(p)
			os.WriteFile(p, data[:len(data)-1], 0o644)
		}},
		{"empty", func(t *testing.T, root string) {
			os.WriteFile(filepath.Join(root, checkpointName), nil, 0o644)
		}},
		{"journal shorter", func(t *testing.T, root string) {
			p := filepath.Join(root, "journal")
			data, _ := os.ReadFile(p)
			os.WriteFile(p, data[:len(data)/2], 0o600)
		}},
		{"journal rewritten", func(t *testing.T, root string) {
			p := filepath.Join(root, "journal")
			data, _ := os.ReadFile(p)
			// Same length, different bytes before the checkpoint's end:
			// a mark record's session renamed.
			i := len(data) - 20
			for data[i] != 'a' {
				i--
			}
			data[i] = 'b'
			os.WriteFile(p, data, 0o600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := checkpointStore(t)
			tc.break_(t, root)
			st, err := Open(root, WithReadOnly())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			if st.checkpointed != 0 {
				t.Error("used a checkpoint that does not match")
			}
			got, _ := st.replay()
			want, _ := st.replayFrom(0)
			if d := sameScan(got, want); d != "" {
				t.Errorf("differs from a replay from the start: %s", d)
			}
		})
	}
}

// TestCheckpointNotWrittenReadOnly: a read-only store writes nothing,
// checkpoints included.
func TestCheckpointNotWrittenReadOnly(t *testing.T) {
	lowCheckpoints(t)
	root := checkpointStore(t)
	os.Remove(filepath.Join(root, checkpointName))
	st, _ := Open(root, WithReadOnly())
	st.replay()
	st.Close()
	if _, err := os.Stat(filepath.Join(root, checkpointName)); err == nil {
		t.Error("a read-only store wrote a checkpoint")
	}
}
