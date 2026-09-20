package jsonl_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

// TestStaleLockReport leaves a lock naming a process that cannot exist
// on this host and expects Open to take it over and say so.
func TestStaleLockReport(t *testing.T) {
	root := t.TempDir()
	st, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(context.Background(), agentsession.Header{ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	st.Release(s.ID())
	path, err := st.Path(s.ID())
	if err != nil {
		t.Fatal(err)
	}
	host, _ := os.Hostname()
	dead := jsonl.LockInfo{PID: 2147483000, Host: host, Since: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)}
	data, _ := json.Marshal(dead)
	if err := os.WriteFile(path+".lock", data, 0o600); err != nil {
		t.Fatal(err)
	}

	var reported []jsonl.LockInfo
	st2, err := jsonl.Open(root, jsonl.WithStaleLockReport(func(l jsonl.LockInfo) { reported = append(reported, l) }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Open(context.Background(), s.ID()); err != nil {
		t.Fatalf("Open over a stale lock: %v", err)
	}
	if len(reported) != 1 || reported[0].PID != dead.PID || !reported[0].Since.Equal(dead.Since) {
		t.Errorf("reported = %+v, want the dead holder", reported)
	}
	// A clean open reports nothing.
	st2.Release(s.ID())
	reported = nil
	if _, err := st2.Open(context.Background(), s.ID()); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 0 {
		t.Errorf("clean open reported %+v", reported)
	}
}
