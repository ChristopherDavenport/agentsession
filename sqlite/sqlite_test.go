package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/sqlite"
	"github.com/ChristopherDavenport/agentsession/storetest"
	"github.com/ChristopherDavenport/openresponses"
)

func TestStoreSuite(t *testing.T) {
	paths := map[agentsession.Store]string{}
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store {
			path := filepath.Join(t.TempDir(), "sessions.db")
			st, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			paths[st] = path
			return st
		},
		Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
			old := s.(*sqlite.Store)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := sqlite.Open(paths[old])
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			paths[st] = paths[old]
			return st
		},
	})
}

func TestPragmasAndRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{ID: "s1", CWD: "/p"})
	if err != nil {
		t.Fatal(err)
	}
	cfgID, err := st.Append(ctx, s.ID(), &agentsession.ConfigEntry{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var mode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %s, want wal", mode)
	}
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM entries WHERE session_id = 's1'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("entries = %d", n)
	}
	var seq int
	var typ, parent string
	if err := db.QueryRow("SELECT seq, type, COALESCE(parent, '') FROM entries WHERE session_id = 's1' AND id = ?", cfgID).Scan(&seq, &typ, &parent); err != nil {
		t.Fatal(err)
	}
	if seq != 1 || typ != agentsession.TypeConfig || parent != "" {
		t.Errorf("config row: seq %d type %s parent %q", seq, typ, parent)
	}
	// Release drops the cache; the reload agrees with the rows.
	st.Release("s1")
	again, err := st.Open(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if again == s || again.Len() != 2 {
		t.Errorf("reloaded session %p len %d", again, again.Len())
	}
	// Deleting the session cascades to its entries.
	if err := st.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM entries").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("entries after delete = %d", n)
	}
}

func TestCorruptRow(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{ID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE entries SET line = '{"type":"info","id":"x","parent":"ghost","ts":"2026-09-17T12:00:00Z"}' WHERE session_id = 's1'`); err != nil {
		t.Fatal(err)
	}
	st.Release("s1")
	if _, err := st.Open(ctx, "s1"); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("Open(corrupt) = %v, want ErrNoEntry", err)
	}
	if _, err := sqlite.Open(filepath.Join(t.TempDir(), "missing", "dir", "x.db")); err == nil {
		t.Error("Open under a missing directory succeeded")
	}
}
