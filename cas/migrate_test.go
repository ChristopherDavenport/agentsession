package cas

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// legacyStore lays out a store as one from before per-session logs left
// it after a crash: a session whose log lags the journal, with a lazy
// append that survived and one whose envelope the crash took; a session
// the journal deleted whose directory remained; and a directory a create
// never finished. It returns the root and the entries session "a" should
// hold after migrating, in order.
func legacyStore(t *testing.T) (string, []string) {
	t.Helper()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	scratch := agentsession.New(agentsession.Header{ID: "a"})
	var es []agentsession.Entry
	for _, text := range []string{"one", "two", "three", "lost"} {
		e := item(text)
		if _, err := scratch.Append(e); err != nil {
			t.Fatal(err)
		}
		if _, _, err := st.storeEntry(e, true, nil); err != nil {
			t.Fatal(err)
		}
		es = append(es, e)
	}
	g := item("gone")
	gs := agentsession.New(agentsession.Header{ID: "gone"})
	gs.Append(g)
	st.storeEntry(g, true, nil)
	st.Close()
	// The crash took the last lazy append's envelope.
	p, _ := st.objs.loosePath(spaceEntries, es[3].Base().ID)
	os.Remove(p)

	id := func(i int) string { return es[i].Base().ID }
	var journal []byte
	for _, r := range []logRecord{
		{Op: "create", Session: "a"},
		{Op: "mark", Session: "a", Mark: MarkRecord},
		{Op: "append", Session: "a", Entry: id(0), Head: id(0), Seq: 1},
		{Op: "append", Session: "a", Entry: id(1), Head: id(1), Seq: 2},
		{Op: "create", Session: "gone"},
		{Op: "append", Session: "gone", Entry: g.Base().ID, Head: g.Base().ID, Seq: 1},
		{Op: "append", Session: "a", Entry: id(2), Head: id(2), Seq: 3, Lazy: true},
		{Op: "append", Session: "a", Entry: id(3), Head: id(3), Seq: 4, Lazy: true},
		{Op: "delete", Session: "gone"},
	} {
		line, err := r.encode()
		if err != nil {
			t.Fatal(err)
		}
		journal = append(journal, line...)
	}
	// The store was opened by this release, which left its tombstone
	// where the journal goes.
	os.Remove(filepath.Join(root, journalFile))
	if err := os.WriteFile(filepath.Join(root, journalFile), journal, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []struct {
		id, log, head string
	}{
		{"a", id(0) + "\n" + id(1) + "\n", id(0)},
		{"gone", g.Base().ID + "\n", g.Base().ID},
	} {
		dir := filepath.Join(root, "sessions", sess.id)
		os.MkdirAll(dir, 0o755)
		if err := writeHeader(nil, dir, agentsession.New(agentsession.Header{ID: sess.id}).Header()); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, logName), []byte(sess.log), 0o600)
		os.WriteFile(filepath.Join(dir, "HEAD"), []byte(sess.head+"\n"), 0o600)
		os.WriteFile(filepath.Join(dir, "record"), []byte(MarkRecord+"\n"), 0o600)
	}
	os.MkdirAll(filepath.Join(root, "sessions", "half"), 0o755)
	// A release with a journal wrote no layout file.
	os.Remove(filepath.Join(root, layoutFile))
	return root, []string{id(0), id(1), id(2)}
}

// TestMigrate: the first writing open of a legacy store rewrites each
// session's log from what the journal and the old log together said,
// recovering it as that store would have, and retires the journal; a
// read-only open refuses the store until then.
func TestMigrate(t *testing.T) {
	ctx := context.Background()
	root, want := legacyStore(t)
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatalf("a read-only open of a legacy store: %v", err)
	}
	if _, err := ro.Open(ctx, "a"); !errors.Is(err, ErrLegacyStore) {
		t.Errorf("a read-only open of a session not yet migrated: %v", err)
	}
	ro.Close()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkMigrated(t, root, want)
	s, err := st.Open(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 3 || s.Leaf() != want[2] {
		t.Errorf("after migrating: %d entries at %s, want 3 at %s", s.Len(), s.Leaf(), want[2])
	}
	if rep, err := st.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify after migrating: %v %v", err, rep.Problems)
	}
}

// TestMigrateResumes: a migration a crash stopped after rewriting some
// sessions finishes at the next writing open.
func TestMigrateResumes(t *testing.T) {
	root, want := legacyStore(t)
	st := &Store{root: root, objs: newObjects(root), open: map[string]*handle{}, faulty: map[string]error{}}
	scan, err := readJournal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.migrateSession("a", scan); err != nil {
		t.Fatal(err)
	}
	st.objs.close()
	st2, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	checkMigrated(t, root, want)
}

func checkMigrated(t *testing.T, root string, want []string) {
	t.Helper()
	if legacyJournal(root) {
		t.Error("the journal outlived the migration")
	}
	if got, err := os.Readlink(filepath.Join(root, journalFile)); err != nil || got != tombstone {
		t.Errorf("the journal's tombstone: %q %v", got, err)
	}
	for _, gone := range []string{"gone", "half"} {
		if _, err := os.Stat(filepath.Join(root, "sessions", gone)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("session %s outlived the migration", gone)
		}
	}
	data, _ := os.ReadFile(filepath.Join(root, "sessions", "a", logName))
	if !bytes.HasPrefix(data, []byte(`{"op":"create"`)) || strings.Contains(string(data), `"lazy"`) {
		t.Errorf("migrated log:\n%s", data)
	}
	l, err := readSessionLog(filepath.Join(root, "sessions", "a"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := ownEntries(l.recs); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("migrated entries %v, want %v", got, want)
	}
}

// TestMigrateRefusesRunningWriter: a writer of the earlier version still
// holding a session keeps the store from being migrated under it.
func TestMigrateRefusesRunningWriter(t *testing.T) {
	root, _ := legacyStore(t)
	old, err := lockFile(filepath.Join(root, "locks", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrMigrationBusy) {
		t.Errorf("migrating under a running writer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, journalFile)); err != nil {
		t.Error("the journal went while a writer held a session")
	}
	old.release()
	st, err := Open(root)
	if err != nil {
		t.Fatalf("after the writer stopped: %v", err)
	}
	st.Close()
}

// TestMigrateTornLegacyLine: a legacy log whose first line a crash cut
// short is migrated from the journal, not taken for one already
// migrated.
func TestMigrateTornLegacyLine(t *testing.T) {
	ctx := context.Background()
	root, want := legacyStore(t)
	p := filepath.Join(root, "sessions", "a", logName)
	data, _ := os.ReadFile(p)
	os.WriteFile(p, append([]byte("sha25\n"), data...), 0o600)
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Open(ctx, "a")
	if err != nil || s.Len() != len(want) {
		t.Fatalf("a torn legacy log: %v", err)
	}
}

// TestMigrateIsolatesFailure: a session that cannot be migrated is
// reported when it is opened, and the rest of the store migrates and
// opens, read-only too.
func TestMigrateIsolatesFailure(t *testing.T) {
	ctx := context.Background()
	root, want := partialStore(t)
	st, err := Open(root)
	if err != nil {
		t.Fatalf("one bad session closed the store: %v", err)
	}
	defer st.Close()
	if s, err := st.Open(ctx, "a"); err != nil || s.Len() != len(want) {
		t.Errorf("the good session: %v", err)
	}
	if _, err := st.Open(ctx, "bad"); err == nil {
		t.Error("the bad session opened")
	}
	if rep, err := st.Verify(ctx); err != nil || rep.OK() {
		t.Errorf("verify with a session unmigrated: %v %v", err, rep.Problems)
	}
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatalf("read-only after a partial migration: %v", err)
	}
	defer ro.Close()
	if _, err := ro.Open(ctx, "a"); err != nil {
		t.Errorf("read-only, the good session: %v", err)
	}
}

// partialStore is legacyStore with a second session, "bad", that
// cannot be migrated, so a writing open migrates "a" and keeps the
// journal.
func partialStore(t *testing.T) (string, []string) {
	t.Helper()
	root, want := legacyStore(t)
	// A second session whose one entry's envelope is corrupt, not gone,
	// and named only by its old log.
	scratch := agentsession.New(agentsession.Header{ID: "bad"})
	e := item("bad")
	scratch.Append(e)
	w := &Store{root: root, objs: newObjects(root)}
	w.storeEntry(e, true, nil)
	ep, _ := w.objs.loosePath(spaceEntries, e.Base().ID)
	os.WriteFile(ep, []byte("garbage"), 0o600)
	j, _ := os.OpenFile(filepath.Join(root, journalFile), os.O_WRONLY|os.O_APPEND, 0)
	line, _ := logRecord{Op: "create", Session: "bad"}.encode()
	j.Write(line)
	j.Close()
	// Its old log names the entry, which the journal does not: the old
	// recovery reads such an entry to tell loss from damage, and cannot.
	dir := filepath.Join(root, "sessions", "bad")
	os.MkdirAll(dir, 0o755)
	writeHeader(nil, dir, scratch.Header())
	os.WriteFile(filepath.Join(dir, logName), []byte(e.Base().ID+"\n"), 0o600)
	return root, want
}

// TestMigrateKeptJournalLeavesMigrated: while a journal is kept for a
// session that failed to migrate, a writing open neither waits on the
// lock of a session already migrated nor writes over its log. A
// damaged log stays as it is, for recovery to report.
func TestMigrateKeptJournalLeavesMigrated(t *testing.T) {
	ctx := context.Background()
	root, want := partialStore(t)
	st1, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()
	if _, err := os.Stat(filepath.Join(root, journalFile)); err != nil {
		t.Fatal("the journal went with a session unmigrated")
	}
	if _, err := st1.Open(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	st1.Create(ctx, agentsession.Header{ID: "fresh"})
	for _, text := range []string{"x", "y", "z"} {
		if _, err := st1.Append(ctx, "fresh", item(text)); err != nil {
			t.Fatal(err)
		}
	}
	st2, err := Open(root)
	if err != nil {
		t.Fatalf("a second writer, with sessions held: %v", err)
	}
	st2.Close()
	st1.Close()

	p := filepath.Join(root, "sessions", "fresh", logName)
	data, _ := os.ReadFile(p)
	lines := bytes.SplitAfter(data, []byte("\n"))
	mid := len(lines[0]) + len(lines[1]) + len(lines[2])/2
	data[mid] ^= 1
	os.WriteFile(p, data, 0o600)
	st3, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st3.Close()
	if after, _ := os.ReadFile(p); !bytes.Equal(after, data) {
		t.Errorf("migration rewrote a damaged log of this version:\n%s", after)
	}
	if s, err := st3.Open(ctx, "a"); err != nil || s.Len() != len(want) {
		t.Errorf("session a after another open: %v", err)
	}
	if _, err := st3.Open(ctx, "fresh"); !errors.As(err, new(LogDamage)) {
		t.Errorf("a damaged log opened with %v, want its damage reported", err)
	}
}

// TestMigrateDamagedJournalKeepsSession: a journal whose damage makes a
// recreated session read as deleted does not remove it.
func TestMigrateDamagedJournalKeepsSession(t *testing.T) {
	ctx := context.Background()
	root, want := legacyStore(t)
	j := filepath.Join(root, journalFile)
	data, _ := os.ReadFile(j)
	var extra []byte
	for _, r := range []logRecord{{Op: "delete", Session: "a"}, {Op: "create", Session: "a"}} {
		line, _ := r.encode()
		extra = append(extra, line...)
	}
	// The recreate's line is damaged, so the journal reads delete, then
	// records for a session it says is gone.
	extra[len(extra)-10] ^= 1
	head, _ := logRecord{Op: "head", Session: "a", Head: want[2]}.encode()
	os.WriteFile(j, append(append(data, extra...), head...), 0o600)
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Open(ctx, "a"); err != nil {
		t.Errorf("a session a damaged journal says was deleted: %v", err)
	}
}
