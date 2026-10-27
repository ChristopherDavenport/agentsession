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
