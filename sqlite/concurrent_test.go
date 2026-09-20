package sqlite_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/sqlite"
	"github.com/ChristopherDavenport/openresponses"
)

// TestConcurrentWriter opens one session from two stores over one
// file, as two processes would, and expects the second writer's append
// to fail loudly and stay failed until it releases and reloads.
func TestConcurrentWriter(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sess, err := a.Create(ctx, agentsession.Header{ID: "shared"})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	if _, err := b.Open(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("from a"))); err != nil {
		t.Fatal(err)
	}
	_, err = b.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("from b")))
	if !errors.Is(err, sqlite.ErrConcurrentWriter) {
		t.Fatalf("b.Append = %v, want ErrConcurrentWriter", err)
	}
	// Still refused: nothing was silently re-parented.
	if _, err := b.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("again"))); !errors.Is(err, sqlite.ErrConcurrentWriter) {
		t.Errorf("second b.Append = %v, want ErrConcurrentWriter", err)
	}
	if _, err := b.Open(ctx, id); !errors.Is(err, sqlite.ErrConcurrentWriter) {
		t.Errorf("b.Open while refused = %v, want ErrConcurrentWriter", err)
	}
	// After an explicit reload the writer sees a's entry and continues.
	b.Release(id)
	reloaded, err := b.Open(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded has %d entries, want 1", reloaded.Len())
	}
	if _, err := b.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("after reload"))); err != nil {
		t.Errorf("append after reload: %v", err)
	}
	final, _ := a.Open(ctx, id)
	_ = final
}
