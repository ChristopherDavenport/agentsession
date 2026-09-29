package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
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
	if _, err := st.Open(ctx, "s1"); !errors.Is(err, agentsession.ErrNoEntry) && !errors.Is(err, agentsession.ErrBadID) {
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

// TestForeignTable opens a database holding another program's entries
// table and expects a refusal naming the table and its columns, with
// none of the store's tables or indexes left behind. The same file
// without that table then opens.
func TestForeignTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE entries (scope TEXT, name TEXT, content TEXT, meta TEXT, hash TEXT, updated TEXT)`); err != nil {
		t.Fatal(err)
	}
	_, err = sqlite.Open(path)
	if err == nil {
		t.Fatal("Open succeeded over a foreign entries table")
	}
	for _, want := range []string{"table entries", "(scope, name, content, meta, hash, updated)", "(session_id, seq, id, parent, type, line)", "another program"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE name <> 'entries' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		left = append(left, name)
	}
	rows.Close()
	if len(left) != 0 {
		t.Errorf("refused Open left %v behind", left)
	}

	if _, err := db.Exec(`DROP TABLE entries`); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ { // the second open finds its own tables
		st, err := sqlite.Open(path)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if round == 0 {
			if _, err := st.Create(context.Background(), agentsession.Header{ID: "s1"}); err != nil {
				t.Fatal(err)
			}
		}
		st.Close()
	}
}

// TestRaiseFormat edits a session's stored header to name an earlier
// minor and expects the first append to raise it to the format this
// package writes, while a read-only open, an open alone and a session
// already current leave the row alone.
func TestRaiseFormat(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "new"} {
		s, err := st.Create(ctx, agentsession.Header{ID: id, CWD: "/p"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, s.ID(), &agentsession.ConfigEntry{Model: "m"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Append(ctx, s.ID(), agentsession.NewItemEntry(openresponses.UserText("hello"))); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	header := func(id string) string {
		t.Helper()
		var h string
		if err := db.QueryRow(`SELECT header FROM sessions WHERE id = ?`, id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	current := `"format":"` + agentsession.Format + `"`
	stored := header("old")
	if !strings.Contains(stored, current) {
		t.Fatalf("header %s does not name %s", stored, agentsession.Format)
	}
	earlier := strings.Replace(stored, current, `"format":"agentsession/0.7"`, 1)
	if _, err := db.Exec(`UPDATE sessions SET header = ? WHERE id = ?`, earlier, "old"); err != nil {
		t.Fatal(err)
	}
	fresh := header("new")

	ro, err := sqlite.Open(path, sqlite.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Open(ctx, "old"); err != nil {
		t.Fatal(err)
	}
	if _, err := ro.Append(ctx, "old", &agentsession.InfoEntry{Name: "n"}); !errors.Is(err, agentsession.ErrReadOnly) {
		t.Errorf("read-only Append = %v, want ErrReadOnly", err)
	}
	ro.Close()
	if got := header("old"); got != earlier {
		t.Fatalf("a read-only open changed the header to %s", got)
	}

	st2, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"old", "new"} {
		if _, err := st2.Open(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if got := header("old"); got != earlier {
		t.Fatalf("an open changed the header to %s", got)
	}
	for _, id := range []string{"old", "new"} {
		if _, err := st2.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("again"))); err != nil {
			t.Fatal(err)
		}
		if _, err := st2.Append(ctx, id, &agentsession.InfoEntry{Name: "after"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
	if got := header("old"); got != stored {
		t.Errorf("header after the append = %s, want %s", got, stored)
	}
	if got := header("new"); got != fresh {
		t.Errorf("a current header changed to %s", got)
	}

	st3, err := sqlite.Open(path, sqlite.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	sess, err := st3.Open(ctx, "old")
	if err != nil {
		t.Fatalf("Open of the raised session: %v", err)
	}
	if sess.Len() != 4 {
		t.Errorf("raised session holds %d entries, want 4", sess.Len())
	}
	var buf strings.Builder
	if err := agentsession.Write(&buf, sess); err != nil {
		t.Fatal(err)
	}
	back, err := agentsession.Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("Read of the written session: %v", err)
	}
	if back.Header().Format != agentsession.Format {
		t.Errorf("round trip format %s", back.Header().Format)
	}
	for i, e := range sess.Entries() {
		if back.Entries()[i].Base().ID != e.Base().ID {
			t.Errorf("entry %d: %s after a round trip, want %s", i, back.Entries()[i].Base().ID, e.Base().ID)
		}
	}
}
