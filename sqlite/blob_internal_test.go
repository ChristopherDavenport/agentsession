package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestSweepKeepsBlobOfUndecodableLine: a stored line that does not
// decode names every reference its text holds, so a sweep keeps the
// blob it may name rather than lose it.
func TestSweepKeepsBlobOfUndecodableLine(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, err := st.PutBlob(ctx, []byte("a picture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "s", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	e := agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputImage{ImageURL: "sidecar:" + h}))
	if _, err := st.Append(ctx, "s", e); err != nil {
		t.Fatal(err)
	}
	if _, err := st.w.ExecContext(ctx, `UPDATE entries SET line = line || ',['`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	if n, err := st.SweepBlobs(ctx, time.Millisecond); err != nil || n != 0 {
		t.Errorf("a sweep with a torn line naming the blob: %d, %v; want 0", n, err)
	}
	if _, err := st.Blob(ctx, h); err != nil {
		t.Errorf("the blob a torn line names: %v", err)
	}
}
