package jsonl_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentsession/storetest"
	"github.com/ChristopherDavenport/openresponses"
)

func TestStoreSuite(t *testing.T) {
	for _, policy := range []jsonl.SyncPolicy{jsonl.SyncEveryAppend, jsonl.SyncOnResponse, jsonl.SyncNever} {
		t.Run(map[jsonl.SyncPolicy]string{0: "every", 1: "response", 2: "never"}[policy], func(t *testing.T) {
			storetest.Run(t, storetest.Options{
				New: func(t *testing.T) agentsession.Store {
					st, err := jsonl.Open(t.TempDir(), jsonl.WithSync(policy))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { st.Close() })
					return st
				},
				Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
					old := s.(*jsonl.Store)
					if err := old.Close(); err != nil {
						t.Fatal(err)
					}
					st, err := jsonl.Open(old.Root(), jsonl.WithSync(policy))
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { st.Close() })
					return st
				},
			})
		})
	}
}

func TestProjectKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"/home/u/proj", "home-u-proj"},
		{"/", "default"},
		{"", "default"},
		{"C:\\Users\\u\\proj", "C--Users-u-proj"},
		{"relative/dir", "relative-dir"},
	}
	for _, tt := range tests {
		if got := jsonl.ProjectKey(tt.in); got != tt.want {
			t.Errorf("ProjectKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLayout(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	created := time.Date(2026, 9, 17, 12, 0, 1, 500_000_000, time.UTC)
	s, err := st.Create(ctx, agentsession.Header{ID: "sess-1", CWD: "/home/u/proj", CreatedAt: created})
	if err != nil {
		t.Fatal(err)
	}
	path, err := st.Path(s.ID())
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(root, "home-u-proj", "2026-09-17T12-00-01.500Z_sess-1.jsonl")
	if path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"type":"session","format":"agentsession/0.1","id":"sess-1"`) || !strings.HasPrefix(lines[1], `{"type":"item","id":"`) {
		t.Errorf("file:\n%s", data)
	}
	// The file is a session Read understands.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	back, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if back.Len() != 1 || back.ID() != "sess-1" {
		t.Errorf("read back: %d entries, id %s", back.Len(), back.ID())
	}
	if _, err := st.Path("nope"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Path(nope) = %v", err)
	}
	if _, err := st.Path("../etc"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Path(../etc) = %v", err)
	}
}

// TestTruncatedRecovery simulates a crash mid-append: the file ends
// with a partial line. Open reports it, drops it, and the next append
// continues a valid file.
func TestTruncatedRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, agentsession.Header{ID: "crashy"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.Append(ctx, s.ID(), &agentsession.ConfigEntry{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	path, _ := st.Path(s.ID())
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"item","id":"partial","parent":"` + first + `","ts":"2026-09-17T12:00:00Z","item":{"type":"mess`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	st2, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	again, err := st2.Open(ctx, "crashy")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if again.Truncated() == nil || again.Len() != 1 || again.Leaf() != first {
		t.Fatalf("after crash: truncated %v len %d leaf %s", again.Truncated(), again.Len(), again.Leaf())
	}
	next, err := st2.Append(ctx, "crashy", agentsession.NewItemEntry(openresponses.UserText("continue")))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "partial") {
		t.Errorf("broken line still in file:\n%s", data)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	st3, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	clean, err := st3.Open(ctx, "crashy")
	if err != nil {
		t.Fatal(err)
	}
	if clean.Truncated() != nil || clean.Len() != 2 || clean.Leaf() != next {
		t.Errorf("after recovery: truncated %v len %d leaf %s", clean.Truncated(), clean.Len(), clean.Leaf())
	}
}

func TestListSkipsBadFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "good"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default", "bad_x.jsonl"), []byte("not json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var ids []string
	var errs int
	for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			errs++
			continue
		}
		ids = append(ids, sum.Header.ID)
		if sum.Path == "" || sum.Size == 0 || sum.Modified.IsZero() {
			t.Errorf("summary = %+v", sum)
		}
	}
	if len(ids) != 1 || ids[0] != "good" || errs != 1 {
		t.Errorf("ids %v errs %d", ids, errs)
	}
	// Opening the bad file by ID fails cleanly.
	if _, err := st.Open(ctx, "x"); err == nil {
		t.Error("Open(bad) succeeded")
	}
}

func TestContextCancelled(t *testing.T) {
	st, err := jsonl.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := st.Create(ctx, agentsession.Header{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Create = %v", err)
	}
	if _, err := st.Open(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Open = %v", err)
	}
	if _, err := st.Append(ctx, "x", &agentsession.InfoEntry{}); !errors.Is(err, context.Canceled) {
		t.Errorf("Append = %v", err)
	}
	if err := st.Delete(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete = %v", err)
	}
	for _, err := range st.List(ctx, agentsession.ListFilter{}) {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("List = %v", err)
		}
	}
	if err := st.Sync(ctx, "x"); !errors.Is(err, context.Canceled) {
		t.Errorf("Sync = %v", err)
	}
}

func TestSyncAndRelease(t *testing.T) {
	ctx := context.Background()
	st, err := jsonl.Open(t.TempDir(), jsonl.WithSync(jsonl.SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Sync(ctx, s.ID()); err != nil {
		t.Errorf("Sync: %v", err)
	}
	if err := st.Sync(ctx, "closed"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Sync(closed) = %v", err)
	}
	if err := st.Release(s.ID()); err != nil {
		t.Errorf("Release: %v", err)
	}
	if err := st.Release(s.ID()); err != nil {
		t.Errorf("second Release: %v", err)
	}
	// The session reopens from disk with the same content.
	again, err := st.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if again == s || again.Len() != 1 || again.Name() != "n" {
		t.Errorf("reopened session = %p (%d entries, name %q), original %p", again, again.Len(), again.Name(), s)
	}
}

func TestOpenRootFailure(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := jsonl.Open(filepath.Join(file, "sub")); err == nil {
		t.Error("Open under a file succeeded")
	}
}
