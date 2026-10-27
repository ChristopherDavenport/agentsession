package jsonl

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// TestSyncNeverSyncsRecords: SyncNever leaves an ordinary append to the
// operating system and fsyncs an entry the header names in records,
// which the format requires durable before the side effect it precedes.
func TestSyncNeverSyncsRecords(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s", Records: agentsession.AllRecords}); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	old := syncEntry
	defer func() { syncEntry = old }()
	syncEntry = func(f *os.File) error { syncs++; return f.Sync() }
	if _, err := st.Append(ctx, "s", &agentsession.InfoEntry{Name: "lazy"}); err != nil {
		t.Fatal(err)
	}
	if syncs != 0 {
		t.Errorf("an ordinary entry was synced")
	}
	if _, err := st.Append(ctx, "s", agentsession.NewRunStart("run-1", agentsession.SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Errorf("a run the header records: %d syncs", syncs)
	}
}

// TestKeptHeadLandsWithItsBranch: the leaf label that keeps the head
// where a branch leaves it is written and synced with the branch, one
// append and one sync, so a crash cannot keep the branch and lose the
// label (RFC 0002, Head).
func TestKeptHeadLandsWithItsBranch(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithSync(SyncEveryAppend))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	a, err := st.Append(ctx, "s", &agentsession.InfoEntry{Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "s", &agentsession.InfoEntry{Name: "b"}); err != nil {
		t.Fatal(err)
	}
	path, err := st.Path("s")
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	old := syncEntry
	defer func() { syncEntry = old }()
	syncEntry = func(f *os.File) error {
		data, _ := os.ReadFile(path)
		lines = append(lines, bytes.Count(data, []byte{'\n'}))
		return f.Sync()
	}
	branch := &agentsession.InfoEntry{Name: "branch"}
	branch.Parent = a
	if _, err := st.Append(ctx, "s", branch); err != nil {
		t.Fatal(err)
	}
	// The header, a, b, the branch and the label.
	if len(lines) != 1 || lines[0] != 5 {
		t.Errorf("syncs saw %v lines, want one sync of 5", lines)
	}
}

// TestATornBranchKeepsTheHead: the label that keeps the head is written
// before the branch it keeps it from, so a crash that tears the tail of
// the write loses the branch, never the label, and the reopened head is
// the one the branch left in place.
func TestATornBranchKeepsTheHead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, name := range []string{"a", "b", "c"} {
		id, err := st.Append(ctx, "s", &agentsession.InfoEntry{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	side := &agentsession.InfoEntry{Name: "side"}
	side.Parent = ids[0]
	if _, err := st.Append(ctx, "s", side); err != nil {
		t.Fatal(err)
	}
	path, err := st.Path("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.TrimRight(data, "\n")
	last := bytes.LastIndexByte(body, '\n') + 1
	torn := data[:last+(len(body)-last)/2]
	if err := os.WriteFile(path, torn, 0o600); err != nil {
		t.Fatal(err)
	}
	again, err := Open(dir, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	s, err := again.Read(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Leaf() != ids[2] {
		t.Errorf("head after a torn branch = %s, want %s", s.Leaf(), ids[2])
	}
}
