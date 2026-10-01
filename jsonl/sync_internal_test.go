package jsonl

import (
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
