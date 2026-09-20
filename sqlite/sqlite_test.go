package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
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

// TestMigrateAddsName opens a database laid out as v0.0.2 wrote it,
// without the name column, and expects the column added and filled
// from the stored info entries.
func TestMigrateAddsName(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	const old = `
CREATE TABLE sessions (
	id TEXT PRIMARY KEY, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
	cwd TEXT NOT NULL DEFAULT '', parent_session TEXT NOT NULL DEFAULT '', header TEXT NOT NULL
) STRICT;
CREATE TABLE entries (
	session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL, id TEXT NOT NULL, parent TEXT, type TEXT NOT NULL, line TEXT NOT NULL,
	UNIQUE (session_id, seq), UNIQUE (session_id, id)
) STRICT;`
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"named", "anonymous"} {
		header := `{"type":"session","format":"agentsession/0.1","id":"` + id + `","created_at":"2026-09-17T12:00:00Z","payload":"openresponses/2026-04-24","cwd":"/p"}`
		if _, err := db.Exec(`INSERT INTO sessions (id, created_at, updated_at, cwd, header) VALUES (?, ?, ?, ?, ?)`,
			id, "2026-09-17T12:00:00.000000000Z", "2026-09-17T12:00:00.000000000Z", "/p", header); err != nil {
			t.Fatal(err)
		}
	}
	lines := []struct {
		seq        int
		id, parent string
		typ, line  string
	}{
		{1, "n1", "", "info", `{"type":"info","id":"n1","parent":null,"ts":"2026-09-17T12:00:01Z","name":"Old name"}`},
		{2, "n2", "n1", "info", `{"type":"info","id":"n2","parent":"n1","ts":"2026-09-17T12:00:02Z","name":"New name"}`},
		{3, "n3", "n2", "info", `{"type":"info","id":"n3","parent":"n2","ts":"2026-09-17T12:00:03Z"}`},
	}
	for _, l := range lines {
		var parent any
		if l.parent != "" {
			parent = l.parent
		}
		if _, err := db.Exec(`INSERT INTO entries (session_id, seq, id, parent, type, line) VALUES (?, ?, ?, ?, ?, ?)`,
			"named", l.seq, l.id, parent, l.typ, l.line); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	for round := 0; round < 2; round++ { // the second open finds the column and does nothing
		st, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		got := map[string]string{}
		for sum, err := range st.List(ctx, agentsession.ListFilter{WithNames: true}) {
			if err != nil {
				t.Fatal(err)
			}
			got[sum.Header.ID] = sum.Name
		}
		want := map[string]string{"named": "New name", "anonymous": ""}
		if round == 1 {
			want["named"] = "Newer" // set by the first round's append
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round %d: names = %v, want %v", round, got, want)
		}
		// The migrated session still appends and updates its name.
		if _, err := st.Append(ctx, "named", &agentsession.InfoEntry{Name: "Newer"}); err != nil {
			t.Fatal(err)
		}
		for sum, err := range st.List(ctx, agentsession.ListFilter{CWD: "/p", WithNames: true}) {
			if err != nil {
				t.Fatal(err)
			}
			if sum.Header.ID == "named" && sum.Name != "Newer" {
				t.Errorf("round %d: name after append = %q", round, sum.Name)
			}
		}
		st.Close()
	}
}
