package agentsession_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// disagreeingFile is a file of the given format whose one env entry's
// checkout at "." carries a vcs other than the entry's, as a file
// raised from 0.21 may hold.
func disagreeingFile(t *testing.T, format string) string {
	t.Helper()
	body := map[string]any{
		"type": "env", "parent": nil, "ts": "2026-11-26T10:00:01Z", "cwd": "/home/u/work",
		"vcs":       map[string]any{"system": "git", "revision": "4c61d09aa1b2c3d4e5f60718293a4b5c6d7e8f90"},
		"checkouts": []any{map[string]any{"path": ".", "vcs": map[string]any{"system": "git", "revision": "9a1f0c3e5b7d9f1a3c5e7092b4d6f8a0c2e4f6a8"}}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := agentsession.EntryHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	body["id"] = id
	if data, err = json.Marshal(body); err != nil {
		t.Fatal(err)
	}
	header := `{"type":"session","format":"` + format + `","id":"s","created_at":"2026-11-26T10:00:00Z","payload":"openresponses/2026-04-24"}`
	return header + "\n" + string(data) + "\n"
}

// TestCheckoutVCSInStores: a file whose checkout at "." disagrees with
// its entry's vcs imports into a MemoryStore and a cas store, at 0.21
// and at 0.22, and reads back through cas's log read and a follow,
// since the agreement is a writer's rule and read paths never apply it.
func TestCheckoutVCSInStores(t *testing.T) {
	ctx := context.Background()
	for _, format := range []string{"agentsession/0.21", agentsession.Format} {
		file := disagreeingFile(t, format)
		mem := agentsession.NewMemoryStore()
		if _, err := mem.Import(ctx, strings.NewReader(file), true); err != nil {
			t.Fatalf("%s: MemoryStore import: %v", format, err)
		}
		st, err := cas.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Import(ctx, strings.NewReader(file), true); err != nil {
			t.Fatalf("%s: cas import: %v", format, err)
		}
		log, err := st.ReadLog(ctx, "s", "", false)
		if err != nil {
			t.Fatalf("%s: ReadLog: %v", format, err)
		}
		if n := len(log.Session.Entries()); n != 1 {
			t.Errorf("%s: ReadLog's session holds %d entries", format, n)
		}
		for name, follow := range map[string]func(context.Context) func(func(agentsession.Change, error) bool){
			"memory": func(ctx context.Context) func(func(agentsession.Change, error) bool) { return mem.Follow(ctx, "s", "") },
			"cas":    func(ctx context.Context) func(func(agentsession.Change, error) bool) { return st.Follow(ctx, "s", "") },
		} {
			fctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			for c, err := range follow(fctx) {
				if err != nil {
					t.Errorf("%s: %s follow: %v", format, name, err)
					break
				}
				if c.Kind == agentsession.Snapshot {
					if n := len(c.Session.Entries()); n != 1 {
						t.Errorf("%s: %s follow's snapshot holds %d entries", format, name, n)
					}
					break
				}
			}
			cancel()
		}
		st.Close()
	}
}
