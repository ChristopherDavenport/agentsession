package sqlite_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/followtest"
	"github.com/ChristopherDavenport/agentsession/sqlite"
	"github.com/ChristopherDavenport/openresponses"
)

func followDB(t *testing.T) (string, *sqlite.Store, []string) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, text := range []string{"a", "b", "c"} {
		id, err := st.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText(text)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return path, st, ids
}

func followRO(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	ro, err := sqlite.Open(path, sqlite.WithReadOnly(), sqlite.WithFollowInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	return ro
}

// TestFollowResetOnRebuild: rows that go away leave the newest seq below
// the cursor's, which only a rebuild of the rows makes, and is a reset.
func TestFollowResetOnRebuild(t *testing.T) {
	path, _, ids := followDB(t)
	w := followtest.Start(t, followRO(t, path), "s", "")
	snap := w.NextKind(agentsession.Snapshot)
	if snap.Session.Len() != 3 {
		t.Fatalf("snapshot holds %d entries", snap.Session.Len())
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`DELETE FROM entries WHERE session_id = 's' AND seq > 1`); err != nil {
		t.Fatal(err)
	}
	r := w.NextKind(agentsession.Reset)
	if r.Session.Len() != 1 {
		t.Errorf("reset holds %d entries, want 1", r.Session.Len())
	}
	if _, ok := r.Session.Entry(ids[2]); ok {
		t.Error("the reset holds a deleted row")
	}
	w.Quiet()
}

// TestFollowAcrossRaisedFormat: the first append of this writer to a
// session an earlier minor wrote raises the stored header, which changes
// nothing the follower holds, so it goes on without a reset.
func TestFollowAcrossRaisedFormat(t *testing.T) {
	ctx := context.Background()
	path, st, _ := followDB(t)
	st.Close()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var stored string
	if err := db.QueryRow(`SELECT header FROM sessions WHERE id = 's'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	current := `"format":"` + agentsession.Format + `"`
	if _, err := db.Exec(`UPDATE sessions SET header = ? WHERE id = 's'`, strings.Replace(stored, current, `"format":"agentsession/0.10"`, 1)); err != nil {
		t.Fatal(err)
	}
	w := followtest.Start(t, followRO(t, path), "s", "")
	w.NextKind(agentsession.Snapshot)
	writer, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	id, err := writer.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText("d")))
	if err != nil {
		t.Fatal(err)
	}
	if c := w.NextKind(agentsession.Appended); c.ID != id || c.Session.Len() != 4 {
		t.Errorf("appended %s into %d entries, want %s into 4", c.ID, c.Session.Len(), id)
	}
	w.Quiet()
}

// TestFollowWokenByAppend: a follower of a session its own store writes
// does not wait out the poll interval.
func TestFollowWokenByAppend(t *testing.T) {
	path, first, _ := followDB(t)
	first.Close()
	st, err := sqlite.Open(path, sqlite.WithFollowInterval(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	w := followtest.Start(t, st, "s", "")
	w.NextKind(agentsession.Snapshot)
	if _, err := st.Append(context.Background(), "s", agentsession.NewItemEntry(openresponses.UserText("d"))); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	w.NextKind(agentsession.Appended)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the append took %v to reach the follower", d)
	}
}
