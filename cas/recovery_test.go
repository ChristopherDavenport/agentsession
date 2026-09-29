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
	"github.com/ChristopherDavenport/openresponses"
)

// journalLines returns the journal's lines and the index of each append
// record of session id.
func journalLines(t *testing.T, root, id string) ([]string, []int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "journal"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.SplitAfter(string(data), "\n")
	var appends []int
	for i, l := range lines {
		if strings.Contains(l, `"op":"append"`) && strings.Contains(l, `"session":"`+id+`"`) {
			appends = append(appends, i)
		}
	}
	return lines, appends
}

// TestJournalRecord round-trips a record through its checksum, finds a
// flipped byte, and reads a record written before checksums.
func TestJournalRecord(t *testing.T) {
	r := journalRecord{Op: "append", Session: "s", Entry: "sha256:" + strings.Repeat("a", 64), Seq: 2, Size: 10}
	line, err := r.encode()
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := decodeRecord(line); err != nil || got != r {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	bad := bytes.Replace(line, []byte(`"seq":2`), []byte(`"seq":3`), 1)
	if _, _, err := decodeRecord(bad); err == nil {
		t.Error("a changed record passed its checksum")
	}
	if _, torn, _ := decodeRecord(line[:len(line)/2]); !torn {
		t.Error("half a record is not torn")
	}
	legacy := []byte(`{"op":"append","session":"s","entry":"sha256:x","seq":1}` + "\n")
	if got, _, err := decodeRecord(legacy); err != nil || got.Entry != "sha256:x" {
		t.Errorf("a record without a checksum: %+v, %v", got, err)
	}
}

// TestJournalDamage flips one byte in one append record. Recovery keeps
// every entry the log holds and the head, never shrinking the log, and
// Verify names the damaged line.
func TestJournalDamage(t *testing.T) {
	for _, which := range []int{4, 2} { // the last append's record, then the third's
		t.Run("", func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st, _ := Open(root)
			ids := fill(t, st, "a", 5)
			st.Close()
			logBefore, _ := os.ReadFile(filepath.Join(root, "sessions", "a", "log"))
			lines, appends := journalLines(t, root, "a")
			l := []byte(lines[appends[which]])
			l[0] = 'z'
			lines[appends[which]] = string(l)
			os.WriteFile(filepath.Join(root, "journal"), []byte(strings.Join(lines, "")), 0o600)

			st2, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer st2.Close()
			s, err := st2.Open(ctx, "a")
			if err != nil {
				t.Fatal(err)
			}
			if s.Len() != 5 || s.Leaf() != ids[4] {
				t.Errorf("after damage: len %d leaf %s, want 5 %s", s.Len(), s.Leaf(), ids[4])
			}
			if logAfter, _ := os.ReadFile(filepath.Join(root, "sessions", "a", "log")); !bytes.Equal(logBefore, logAfter) {
				t.Errorf("recovery rewrote the log:\n%s\nwas\n%s", logAfter, logBefore)
			}
			rep, err := st2.Verify(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var journal int
			for _, p := range rep.Problems {
				if p.Kind == "journal" {
					journal++
				}
			}
			if journal != 2 { // the line, and the session whose log it leaves ahead
				t.Errorf("Verify: %v", rep.Problems)
			}
		})
	}
}

// TestLazyAppends writes under SyncOnResponse: an item is lazy, a
// response durable, and a lazy append whose objects a crash took is
// lost with what the session appended after it, and nothing else.
func TestLazyAppends(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncOnResponse))
	st.Create(ctx, agentsession.Header{ID: "l"})
	kept, err := st.Write(ctx, "l", &agentsession.ResponseEntry{ResponseID: "resp", Status: openresponses.ResponseStatusCompleted})
	if err != nil || !kept.Durable {
		t.Fatalf("a response: %+v, %v", kept, err)
	}
	lost, err := st.Write(ctx, "l", agentsession.NewItemEntry(openresponses.UserText("lazy")))
	if err != nil || lost.Durable {
		t.Fatalf("an item: %+v, %v", lost, err)
	}
	after, _ := st.Write(ctx, "l", agentsession.NewItemEntry(openresponses.UserText("after")))
	lines, appends := journalLines(t, root, "l")
	if strings.Contains(lines[appends[0]], `"lazy"`) || !strings.Contains(lines[appends[1]], `"lazy":true`) {
		t.Errorf("journal:\n%s", strings.Join(lines, ""))
	}
	st.Close()
	// A crash took the lazy entry's envelope; the log line survived.
	p, _ := st.objs.loosePath(spaceEntries, lost.ID)
	os.Remove(p)
	st2, _ := Open(root)
	defer st2.Close()
	s, err := st2.Open(ctx, "l")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != kept.ID {
		t.Errorf("after losing a lazy append: len %d leaf %s, want 1 %s", s.Len(), s.Leaf(), kept.ID)
	}
	if _, ok := s.Entry(after.ID); ok {
		t.Error("an append after the lost one survived without its parent")
	}
	// The session goes on from what survived.
	mustAppend(t, st2, "l", agentsession.NewItemEntry(openresponses.UserText("again")))
}

// TestSyncNever leaves everything to Sync and Close, and a durable
// append flushes what lazy ones left.
func TestSyncNever(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "n"})
	r, err := st.Write(ctx, "n", &agentsession.ResponseEntry{ResponseID: "resp", Status: openresponses.ResponseStatusCompleted})
	if err != nil || r.Durable {
		t.Fatalf("%+v, %v", r, err)
	}
	if len(st.objs.pendFiles) == 0 || !st.journalDirty {
		t.Fatal("nothing pending after a lazy append")
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.objs.pendFiles) != 0 || len(st.objs.pendDirs) != 0 || st.journalDirty {
		t.Error("Sync left something pending")
	}
}

// TestReadOnly reads, projects and verifies a session another store
// holds, recovers in memory without writing, and refuses every write.
func TestReadOnly(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root)
	defer w.Close()
	ids := fill(t, w, "held", 3)
	// A stale HEAD, as a crash after the commit point leaves it.
	head := filepath.Join(root, "sessions", "held", "HEAD")
	writeHead(filepath.Dir(head), ids[0])
	stale, _ := os.ReadFile(head)

	r, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	s, err := r.Open(ctx, "held")
	if err != nil {
		t.Fatalf("a read-only open of a held session: %v", err)
	}
	if s.Len() != 3 || s.Leaf() != ids[2] {
		t.Errorf("read-only: len %d leaf %s", s.Len(), s.Leaf())
	}
	if now, _ := os.ReadFile(head); !bytes.Equal(now, stale) {
		t.Error("a read-only open rewrote HEAD")
	}
	var buf bytes.Buffer
	if err := r.Project(ctx, &buf, "held"); err != nil {
		t.Fatal(err)
	}
	if rep, err := r.Verify(ctx); err != nil || !rep.OK() || rep.Sessions != 1 {
		t.Errorf("Verify: %+v, %v", rep, err)
	}
	// The writer is not refused its session by the reader.
	mustAppend(t, w, "held", agentsession.NewItemEntry(openresponses.UserText("still mine")))
	for name, err := range map[string]error{
		"Create": func() error { _, err := r.Create(ctx, agentsession.Header{ID: "x"}); return err }(),
		"Append": func() error {
			_, err := r.Append(ctx, "held", agentsession.NewItemEntry(openresponses.UserText("no")))
			return err
		}(),
		"SetHead": r.SetHead(ctx, "held", ids[2], ids[1]),
		"Delete":  r.Delete(ctx, "held"),
		"Import":  func() error { _, err := r.Import(ctx, &buf, false); return err }(),
		"Declare": r.DeclareRecord(ctx, "held"),
		"Sweep":   func() error { _, err := r.Sweep(ctx, 0); return err }(),
		"Pack":    func() error { _, err := r.Pack(ctx); return err }(),
		"PutBlob": func() error { _, err := r.PutBlob(ctx, []byte("b")); return err }(),
		"Sync":    r.Sync(ctx),
	} {
		if !errors.Is(err, agentsession.ErrReadOnly) {
			t.Errorf("%s on a read-only store: %v", name, err)
		}
	}
	if _, err := Open(filepath.Join(root, "nothing"), WithReadOnly()); err == nil {
		t.Error("a read-only open of a directory that is no store")
	}
}

// TestListAfterCrash lists neither a directory a crash left without a
// header nor a session the journal deleted, and Delete succeeds once
// its record is committed.
func TestListAfterCrash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	fill(t, st, "live", 1)
	os.MkdirAll(filepath.Join(root, "sessions", "half"), 0o755)
	fill(t, st, "del", 1)
	st.Release("del")
	dir := filepath.Join(root, "sessions", "del")
	if os.Getuid() != 0 {
		os.Chmod(dir, 0o500) // the removal fails after the commit
		if err := st.Delete(ctx, "del"); err != nil {
			t.Errorf("Delete after its commit: %v", err)
		}
		os.Chmod(dir, 0o755)
	} else {
		st.commit(true, journalRecord{Op: "delete", Session: "del"})
	}
	var got []string
	for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Errorf("List: %v", err)
			continue
		}
		got = append(got, sum.Header.ID)
	}
	if len(got) != 1 || got[0] != "live" {
		t.Errorf("listed %v, want [live]", got)
	}
	if _, err := st.Open(ctx, "del"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("Open of the deleted session: %v", err)
	}
}

// TestCorruptObjectNamed flips a byte in a content object two sessions
// share: Open names the object and where it lies, and Verify reports it
// once, as corrupt.
func TestCorruptObjectNamed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "a"})
	st.Create(ctx, agentsession.Header{ID: "b"})
	e := agentsession.NewItemEntry(openresponses.UserText("shared"))
	mustAppend(t, st, "a", e)
	mustAppend(t, st, "b", agentsession.NewItemEntry(openresponses.UserText("shared")))
	st.Close()
	p, _ := st.objs.loosePath(spaceContents, e.ContentHash())
	data, _ := os.ReadFile(p)
	data[1] ^= 0x01
	os.WriteFile(p, data, 0o600)

	st2, _ := Open(root)
	defer st2.Close()
	for _, id := range []string{"a", "b"} {
		_, err := st2.Open(ctx, id)
		if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), e.ContentHash()) || !strings.Contains(err.Error(), filepath.Join("objects", "contents")) {
			t.Errorf("Open %s: %v", id, err)
		}
	}
	rep, err := st2.Verify(ctx)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := 0
	for _, p := range rep.Problems {
		if p.Kind == "corrupt" && p.Object == e.ContentHash() {
			corrupt++
		}
	}
	if corrupt == 0 {
		t.Errorf("Verify did not name the object: %v", rep.Problems)
	}
}

// TestFormatRaised has the first append of a later minor raise the
// header's format, so an earlier reader refuses the session rather than
// misreading it; an append of the same minor leaves the header alone.
func TestFormatRaised(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "old", 1)
	st.Close()
	hp := filepath.Join(root, "sessions", "old", "header")
	data, _ := os.ReadFile(hp)
	os.WriteFile(hp, bytes.Replace(data, []byte(agentsession.Format), []byte("agentsession/0.7"), 1), 0o600)

	st2, _ := Open(root)
	defer st2.Close()
	s, _ := st2.Open(ctx, "old")
	if s.Header().Format != agentsession.Format {
		t.Fatalf("in memory %s", s.Header().Format)
	}
	if h, _ := readHeader(filepath.Dir(hp)); h.Format != "agentsession/0.7" {
		t.Fatalf("an open raised the header: %s", h.Format)
	}
	mustAppend(t, st2, "old", agentsession.NewItemEntry(openresponses.UserText("new")))
	if h, _ := readHeader(filepath.Dir(hp)); h.Format != agentsession.Format {
		t.Errorf("after an append the header says %s", h.Format)
	}
	info, _ := os.Stat(hp)
	mustAppend(t, st2, "old", agentsession.NewItemEntry(openresponses.UserText("newer")))
	if again, _ := os.Stat(hp); !again.ModTime().Equal(info.ModTime()) {
		t.Error("a current header was rewritten")
	}
}
