package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// journalLines returns the lines of session id's log and the index of
// each append record.
func journalLines(t *testing.T, root, id string) ([]string, []int) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "sessions", id, logName))
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

// die ends a store as a crash does: its locks go, and nothing it left
// lazy is flushed or said to be durable.
func die(st *Store) {
	for id, h := range st.open {
		h.lock.release()
		delete(st.open, id)
	}
	st.objs.close()
}

// TestJournalRecord round-trips a record through its checksum, finds a
// flipped byte, and reads a record written before checksums.
func TestJournalRecord(t *testing.T) {
	r := logRecord{Op: "append", Session: "s", Entry: "sha256:" + strings.Repeat("a", 64), Seq: 2, Size: 10}
	line, err := r.encode()
	if err != nil {
		t.Fatal(err)
	}
	r.checked = true
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

// TestLogDamage: a record of a session's log that fails its checksum is
// reported, and the session is not opened as if the record were not
// there: the log is the session's only record.
func TestLogDamage(t *testing.T) {
	for _, which := range []int{4, 2} { // the last append's record, then the third's
		t.Run("", func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			st, _ := Open(root)
			fill(t, st, "a", 5)
			st.Close()
			lines, appends := journalLines(t, root, "a")
			l := []byte(lines[appends[which]])
			l[0] = 'z'
			lines[appends[which]] = string(l)
			log := filepath.Join(root, "sessions", "a", logName)
			os.WriteFile(log, []byte(strings.Join(lines, "")), 0o600)
			before, _ := os.ReadFile(log)

			st2, err := Open(root)
			if err != nil {
				t.Fatal(err)
			}
			defer st2.Close()
			if _, err := st2.Open(ctx, "a"); !errors.As(err, new(LogDamage)) {
				t.Errorf("open of a damaged log: %v", err)
			}
			if after, _ := os.ReadFile(log); !bytes.Equal(before, after) {
				t.Error("recovery rewrote a damaged log")
			}
			rep, err := st2.Verify(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rep.Problems) != 1 || rep.Problems[0].Kind != "log" {
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
		t.Errorf("log:\n%s", strings.Join(lines, ""))
	}
	die(st)
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
	if len(st.open["n"].pend.files) == 0 || !st.open["n"].lazy {
		t.Fatal("nothing pending after a lazy append")
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if h := st.open["n"]; len(h.pend.files) != 0 || len(h.pend.dirs) != 0 || h.lazy {
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
// header nor a session a delete renamed away and a crash left in the
// trash.
func TestListAfterCrash(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	fill(t, st, "live", 1)
	os.MkdirAll(filepath.Join(root, "sessions", "half"), 0o755)
	fill(t, st, "del", 1)
	st.Release("del")
	os.MkdirAll(filepath.Join(root, "trash"), 0o755)
	if err := os.Rename(filepath.Join(root, "sessions", "del"), filepath.Join(root, "trash", "del-1")); err != nil {
		t.Fatal(err)
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

// TestLostAppendStaysLost: recovery that finds a lazy append lost
// logs the loss before the sync record it writes, so a later open
// neither brings the lost appends back, which would name objects the
// store does not hold, nor cuts the durable appends made after it.
func TestLostAppendStaysLost(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "l"})
	kept, _ := st.Write(ctx, "l", agentsession.NewItemEntry(openresponses.UserText("kept")))
	lost, _ := st.Write(ctx, "l", agentsession.NewItemEntry(openresponses.UserText("lost")))
	after, _ := st.Write(ctx, "l", agentsession.NewItemEntry(openresponses.UserText("after")))
	die(st)
	// A crash took the lost entry's envelope.
	p, _ := st.objs.loosePath(spaceEntries, lost.ID)
	os.Remove(p)

	st2, _ := Open(root)
	s, err := st2.Open(ctx, "l")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != kept.ID {
		t.Fatalf("recovered: len %d leaf %s, want 1 %s", s.Len(), s.Leaf(), kept.ID)
	}
	again := mustAppend(t, st2, "l", agentsession.NewItemEntry(openresponses.UserText("again")))
	st2.Close()

	lines, _ := journalLines(t, root, "l")
	var lostRecs int
	for _, l := range lines {
		if strings.Contains(l, `"op":"lost"`) {
			lostRecs++
		}
	}
	if lostRecs != 2 {
		t.Errorf("%d lost records, want 2:\n%s", lostRecs, strings.Join(lines, ""))
	}

	for i := range 2 {
		st3, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		s, err = st3.Open(ctx, "l")
		if err != nil {
			t.Fatalf("open %d after recovery: %v", i+1, err)
		}
		if s.Len() != 2 || s.Leaf() != again {
			t.Errorf("open %d after recovery: len %d leaf %s, want 2 %s", i+1, s.Len(), s.Leaf(), again)
		}
		for _, id := range []string{lost.ID, after.ID} {
			if _, ok := s.Entry(id); ok {
				t.Errorf("open %d after recovery: lost append %s came back", i+1, id)
			}
		}
		st3.Close()
	}
}

// TestSyncRecords: making a held session's lazy appends durable says so
// in its log, on Sync, on Close and with a durable append, so a
// later open does not read their objects again to find them present.
func TestSyncRecords(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncOnResponse))
	st.Create(ctx, agentsession.Header{ID: "s"})
	syncs := func() int {
		lines, _ := journalLines(t, root, "s")
		n := 0
		for _, l := range lines {
			if strings.Contains(l, `"op":"sync"`) && strings.Contains(l, `"session":"s"`) {
				n++
			}
		}
		return n
	}
	mustAppend(t, st, "s", item("lazy"))
	if _, err := st.Write(ctx, "s", &agentsession.ResponseEntry{ResponseID: "r", Status: openresponses.ResponseStatusCompleted}); err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 1 {
		t.Errorf("after a durable append: %d sync records, want 1", n)
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n := syncs(); n != 1 {
		t.Errorf("Sync with nothing lazy: %d sync records, want 1", n)
	}
	mustAppend(t, st, "s", item("lazy again"))
	st.Close()
	if n := syncs(); n != 2 {
		t.Errorf("after Close: %d sync records, want 2", n)
	}
	// Every lazy append is covered, so an open reads no object to check.
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	v, err := ro.reconcile("s", filepath.Join(root, "sessions", "s"))
	if err != nil || len(v.adopt) != 0 {
		t.Errorf("reconcile after Close: adopt %v, %v", v.adopt, err)
	}
}

// TestParseRecord: a record the fast path reads reads as encoding/json
// reads it, and one it declines is left to encoding/json.
func TestParseRecord(t *testing.T) {
	h := "sha256:" + strings.Repeat("ab", 32)
	for _, r := range []logRecord{
		{Op: "append", Session: "s-1_x.y", Entry: h, Head: h, Seq: 3, Size: 1234, Lazy: true},
		{Op: "create", Session: "s", Base: h},
		{Op: "mark", Session: "s", Mark: "mirror"},
		{Op: "sync", Session: "s"},
		{Op: "head", Session: "s", Head: h, Seq: 0},
	} {
		line, err := r.encode()
		if err != nil {
			t.Fatal(err)
		}
		seg := bytes.TrimRight(line, "\n")
		fast, ok := parseRecord(seg)
		if !ok {
			t.Errorf("fast path declined %s", seg)
			continue
		}
		var slow logRecord
		if err := json.Unmarshal(seg, &slow); err != nil {
			t.Fatal(err)
		}
		if fast != slow {
			t.Errorf("%s: fast %+v, encoding/json %+v", seg, fast, slow)
		}
	}
	for _, seg := range []string{
		`{"op":"append","session":"s","seq":1.5}`,
		`{"op":"append","session":"s","seq":01}`,
		`{"op":"append","session":"s","Seq":1}`,
		`{"op":"append","session":"s\u0041"}`,
		`{"op":"append", "session":"s"}`,
		`{"op":"append","session":"s","lazy":false}`,
		`{"op":"append","session":"s","extra":1}`,
		`{"op":"append","session":"s","seq":-}`,
		`{"op":"append","session":"s","seq":12345678901234567890}`,
		`{"op":"append","session":"s"}x`,
		`{"op":"append","session":null}`,
	} {
		if r, ok := parseRecord([]byte(seg)); ok {
			t.Errorf("fast path read %s as %+v", seg, r)
		}
	}
}

func FuzzParseRecord(f *testing.F) {
	f.Add([]byte(`{"op":"append","session":"s","entry":"e","seq":1,"size":2,"lazy":true,"crc":"ff"}`))
	f.Add([]byte(`{"op":"sync","session":"s"}`))
	f.Fuzz(func(t *testing.T, seg []byte) {
		fast, ok := parseRecord(seg)
		if !ok {
			return
		}
		var slow logRecord
		if err := json.Unmarshal(seg, &slow); err != nil {
			t.Fatalf("fast path read %q, encoding/json refused it: %v", seg, err)
		}
		if fast != slow {
			t.Fatalf("%q: fast %+v, encoding/json %+v", seg, fast, slow)
		}
	})
}

// TestForkOfWorkingState: a fork of a base another process holds as
// working state makes the base's objects durable before it exists, so
// a crash cannot take the base from under it.
func TestForkOfWorkingState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, _ := Open(root, WithSync(SyncNever))
	defer a.Close()
	a.Create(ctx, agentsession.Header{ID: "o"})
	x := mustAppend(t, a, "o", item("x"))
	b, _ := Open(root)
	defer b.Close()
	if _, err := b.Create(ctx, agentsession.Header{ID: "f", Base: x, ParentSession: "o"}); err != nil {
		t.Fatal(err)
	}
	ep, _ := b.objs.loosePath(spaceEntries, x)
	b.objs.mu.Lock()
	pending := b.objs.pendFiles[ep]
	b.objs.mu.Unlock()
	if pending {
		t.Error("the fork exists and its base's envelope waits on a flush")
	}
}

// TestNoForkFromLost: an append its session's log records as lost is no
// entry of the session's, and no fork hangs from it.
func TestNoForkFromLost(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "o"})
	x := mustAppend(t, st, "o", item("x"))
	st.Sync(ctx)
	a1 := mustAppend(t, st, "o", item("a1"))
	lost := item("b")
	lost.Base().Parent = x
	b := mustAppend(t, st, "o", lost)
	die(st)
	c, _ := st.contentOf(a1)
	cp, _ := st.objs.loosePath(spaceContents, c)
	os.Remove(cp)
	r, _ := Open(root)
	defer r.Close()
	if s, err := r.Open(ctx, "o"); err != nil || s.Len() != 1 {
		t.Fatalf("recovery: %v", err)
	}
	for _, parent := range []string{"o", ""} {
		if _, err := r.Create(ctx, agentsession.Header{Base: b, ParentSession: parent}); !errors.Is(err, agentsession.ErrNoEntry) {
			t.Errorf("a fork from a lost append, parent %q: %v", parent, err)
		}
	}
}

// TestDamagedFirstByte: a log whose first byte is damaged is reported as
// damage, not taken for a store from before per-session logs.
func TestDamagedFirstByte(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "s", 2)
	st.Close()
	p := filepath.Join(root, "sessions", "s", logName)
	data, _ := os.ReadFile(p)
	data[0] = 'z'
	os.WriteFile(p, data, 0o600)
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "s"); !errors.As(err, new(LogDamage)) {
		t.Errorf("open: %v", err)
	}
}

// TestSweepRefusesDamagedLog: a sweep will not run while a session's log
// has a record it cannot read, since what that record named cannot be
// known, and removing it would make the damage a loss.
func TestSweepRefusesDamagedLog(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	ids := fill(t, st, "a", 3)
	st.Close()
	lines, appends := journalLines(t, root, "a")
	l := []byte(lines[appends[1]])
	l[len(l)/2] ^= 1
	lines[appends[1]] = string(l)
	os.WriteFile(filepath.Join(root, "sessions", "a", logName), []byte(strings.Join(lines, "")), 0o600)
	sw, _ := Open(root)
	defer sw.Close()
	if _, err := sw.Sweep(ctx, 0); !errors.As(err, new(LogDamage)) {
		t.Errorf("a sweep over a damaged log: %v", err)
	}
	if _, err := sw.objs.read(spaceEntries, ids[1]); err != nil {
		t.Errorf("the damaged record's entry after the sweep: %v", err)
	}
}

// TestFailedCommitLeavesNothing: an append whose commit fails is taken
// back out of the log, so a later open does not find, as a branch, an
// append the writer was told failed.
func TestFailedCommitLeavesNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	first := mustAppend(t, st, "s", item("one"))
	old := syncLog
	failed := false
	syncLog = func(f *os.File) error {
		if failed {
			return f.Sync() // the cut back
		}
		failed = true
		return errors.New("injected fsync failure")
	}
	_, err := st.Append(ctx, "s", item("failed"))
	syncLog = old
	if err == nil {
		t.Fatal("the append reported success")
	}
	second := mustAppend(t, st, "s", item("two"))
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != second || s.Entries()[0].Base().ID != first {
		t.Errorf("after a failed commit: %d entries at %s, want 2 at %s", s.Len(), s.Leaf(), second)
	}
}

// TestUncertainLogLetsGo: a commit that fails and cannot be taken back
// out of the log lets the session go, whatever wrote it, so a later
// operation recovers it from the disk rather than trusting state the
// log no longer follows from.
func TestUncertainLogLetsGo(t *testing.T) {
	ctx := context.Background()
	for name, commit := range map[string]func(st *Store, first string) error{
		"head":    func(st *Store, first string) error { return st.SetHead(ctx, "s", st.open["s"].head, first) },
		"release": func(st *Store, _ string) error { return st.Release("s") },
		"sync":    func(st *Store, _ string) error { return st.Sync(ctx) },
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			st, _ := Open(root, WithSync(SyncNever))
			defer st.Close()
			st.Create(ctx, agentsession.Header{ID: "s"})
			first := mustAppend(t, st, "s", item("one"))
			mustAppend(t, st, "s", item("two"))
			oldSync, oldWrite := syncLog, writeLogFile
			syncLog = func(*os.File) error { return errors.New("injected fsync failure") }
			writeLogFile = func(string, []byte) error { return errors.New("injected write failure") }
			err := commit(st, first)
			syncLog, writeLogFile = oldSync, oldWrite
			if !errors.Is(err, errLogUncertain) {
				t.Fatalf("the commit: %v", err)
			}
			if _, ok := st.open["s"]; ok {
				t.Error("the store still holds a session its log is uncertain of")
			}
			other, _ := Open(root)
			defer other.Close()
			if _, err := other.Open(ctx, "s"); err != nil {
				t.Errorf("another store, after the session was let go: %v", err)
			}
		})
	}
}

// linuxFsync fails an object's first fsync and reports every later one
// on that file a success without syncing anything, as Linux does once
// writeback fails; lose, when set, also takes the page cache's bytes,
// so the file reads back as the disk often has it: zeros, of the
// file's length. The function it returns stops failing files that have
// not failed yet; those that have go on reporting success.
func linuxFsync(t *testing.T, lose bool) (stop func()) {
	t.Helper()
	old := syncObject
	t.Cleanup(func() { syncObject = old })
	var mu sync.Mutex
	failed := map[string]bool{}
	stopped := false
	syncObject = func(f *os.File) error {
		mu.Lock()
		defer mu.Unlock()
		if failed[f.Name()] {
			return nil
		}
		if stopped {
			return f.Sync()
		}
		failed[f.Name()] = true
		if lose {
			info, _ := os.Stat(f.Name())
			os.WriteFile(f.Name(), make([]byte, info.Size()), 0o600)
		}
		return errors.New("injected writeback failure")
	}
	return func() {
		mu.Lock()
		defer mu.Unlock()
		stopped = true
	}
}

// pendingObjects returns the objects a held session's next commit owes.
func pendingObjects(st *Store, id string) []string {
	var out []string
	for f := range st.open[id].pend.files {
		out = append(out, f)
	}
	return out
}

// TestFailedObjectSyncRewrites: an object whose fsync failed is written
// again, not fsynced again, and the commit then holds.
func TestFailedObjectSyncRewrites(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("one"))
	before := map[string]os.FileInfo{}
	for _, f := range pendingObjects(st, "s") {
		before[f], _ = os.Stat(f)
	}
	if len(before) == 0 {
		t.Fatal("nothing pending")
	}
	linuxFsync(t, false)
	if err := st.Sync(ctx); err != nil {
		t.Fatalf("a commit whose objects could be written again: %v", err)
	}
	for f, info := range before {
		if now, err := os.Stat(f); err != nil || os.SameFile(info, now) {
			t.Errorf("%s was fsynced again rather than written again: %v", f, err)
		}
	}
}

// TestLostObjectFailsCommits: an object whose bytes were gone by the
// time its fsync failed fails every later commit that owes it, rather
// than one of them taking an fsync's word that it is durable.
func TestLostObjectFailsCommits(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("one"))
	linuxFsync(t, true)
	if err := st.Sync(ctx); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("the commit: %v", err)
	}
	if err := st.Sync(ctx); !errors.Is(err, ErrCorrupt) {
		t.Errorf("the next commit: %v", err)
	}
}

// TestLostObjectRepairedByWriter: a lost object is put back by the next
// write that holds its bytes, whose commit then holds, rather than that
// commit failing on the loss too.
func TestLostObjectRepairedByWriter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "a"})
	mustAppend(t, st, "a", item("same"))
	stop := linuxFsync(t, true)
	if err := st.Sync(ctx); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("the commit of a lost object: %v", err)
	}
	stop()
	st.Release("a")
	st.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, st, "b", item("same"))
	if err := st.Sync(ctx); err != nil {
		t.Fatalf("a commit holding the lost object's bytes: %v", err)
	}
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "b"); err != nil {
		t.Errorf("the session that put the object back: %v", err)
	}
}

// TestZeroedObjectNotReused: a loose object a crash left the right
// length and the wrong bytes is written again by the next write of it,
// not taken as a copy and committed.
func TestZeroedObjectNotReused(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "a"})
	e := mustAppend(t, st, "a", item("same"))
	c, _ := st.contentOf(e)
	st.Close()
	for _, obj := range []struct {
		sp   space
		hash string
	}{{spaceEntries, e}, {spaceContents, c}} {
		p, _ := st.objs.loosePath(obj.sp, obj.hash)
		info, _ := os.Stat(p)
		os.WriteFile(p, make([]byte, info.Size()), 0o600)
	}
	w, _ := Open(root)
	w.Create(ctx, agentsession.Header{ID: "c"})
	mustAppend(t, w, "c", item("same"))
	w.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "c"); err != nil {
		t.Errorf("a session committed over a zeroed copy: %v", err)
	}
}

// crash abandons a store as a process that died would: its sessions'
// locks go, and nothing it holds is committed.
func crash(st *Store) {
	for id, h := range st.open {
		st.dropHandle(id, h)
	}
}

// TestSweepPastTornLazyAppend: an object a crash left torn, named only
// by a lazy append no commit covered, does not stop sweeps, before
// recovery cuts the append or after.
func TestSweepPastTornLazyAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	torn := mustAppend(t, st, "s", item("torn"))
	c, _ := st.contentOf(torn)
	crash(st)
	p, _ := st.objs.loosePath(spaceContents, c)
	os.WriteFile(p, nil, 0o600)
	w, _ := Open(root)
	defer w.Close()
	if _, err := w.Sweep(ctx, 0); err != nil {
		t.Errorf("a sweep before recovery: %v", err)
	}
	if s, err := w.Open(ctx, "s"); err != nil || s.Len() != 1 {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := w.Sweep(ctx, 0); err != nil {
		t.Errorf("a sweep after recovery: %v", err)
	}
}

// TestZeroedLogLineReported: a committed record whose line, newline and
// all, a failed writeback left zeros runs into the next line, and is
// reported as damage rather than passed over.
func TestZeroedLogLineReported(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	a := mustAppend(t, st, "s", item("a"))
	b := mustAppend(t, st, "s", item("b"))
	if err := st.SetHead(ctx, "s", b, a); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("c"))
	st.Close()
	p := filepath.Join(root, "sessions", "s", logName)
	data, _ := os.ReadFile(p)
	var out []byte
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if bytes.Contains(line, []byte(`"op":"append"`)) && bytes.Contains(line, []byte(b)) {
			line = make([]byte, len(line))
		}
		out = append(out, line...)
	}
	os.WriteFile(p, out, 0o600)
	r, _ := Open(root)
	defer r.Close()
	if s, err := r.Open(ctx, "s"); !errors.As(err, new(LogDamage)) {
		n := 0
		if s != nil {
			n = s.Len()
		}
		t.Errorf("a zeroed record opened with %v and %d entries, want its damage", err, n)
	}
}

// TestUnwrittenTailCut: blocks a crash left unwritten in a log's
// uncommitted tail, with lazy appends after them, are the loss of that
// tail, cut as such, and not damage that closes the session.
func TestUnwrittenTailCut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	kept := mustAppend(t, st, "s", item("kept"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hole := mustAppend(t, st, "s", item("hole"))
	mustAppend(t, st, "s", item("after"))
	crash(st)
	p := filepath.Join(root, "sessions", "s", logName)
	data, _ := os.ReadFile(p)
	var out []byte
	for _, line := range bytes.SplitAfter(data, []byte("\n")) {
		if bytes.Contains(line, []byte(hole)) {
			line = make([]byte, len(line))
		}
		out = append(out, line...)
	}
	os.WriteFile(p, out, 0o600)
	w, _ := Open(root)
	defer w.Close()
	s, err := w.Open(ctx, "s")
	if err != nil {
		t.Fatalf("a crash's unwritten tail: %v", err)
	}
	if s.Len() != 1 || s.Leaf() != kept {
		t.Errorf("after the cut: %d entries at %s, want 1 at %s", s.Len(), s.Leaf(), kept)
	}
	if rep, err := w.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify after the cut: %v %v", err, rep.Problems)
	}
}

// TestReadOnlyServesCommitted: a read-only store serving a session
// another process writes serves what it committed, and none of its
// working state, which a crash could still take.
func TestReadOnlyServesCommitted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	kept := mustAppend(t, st, "s", item("kept"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("working"))
	ro, _ := Open(root, WithReadOnly())
	defer ro.Close()
	mir, _ := Open(t.TempDir())
	defer mir.Close()
	if _, err := mir.Fetch(ctx, ro, "s"); err != nil {
		t.Fatal(err)
	}
	s, err := mir.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != kept {
		t.Errorf("the mirror holds %d entries at %s, want only the committed %s", s.Len(), s.Leaf(), kept)
	}
}

// TestFailedLogSyncRewrites: a log whose fsync failed is written again
// as a new file, not fsynced again, and the session goes on.
func TestFailedLogSyncRewrites(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("one"))
	p := filepath.Join(root, "sessions", "s", logName)
	before, _ := os.Stat(p)
	old := syncLog
	failed := false
	syncLog = func(f *os.File) error {
		if failed {
			return nil // as Linux does, after it reported the failure
		}
		failed = true
		return errors.New("injected writeback failure")
	}
	err := st.Sync(ctx)
	syncLog = old
	if err == nil {
		t.Fatal("the commit reported success")
	}
	if now, _ := os.Stat(p); os.SameFile(before, now) {
		t.Error("the log was fsynced again rather than written again")
	}
	two := mustAppend(t, st, "s", item("two"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	if s, err := r.Open(ctx, "s"); err != nil || s.Len() != 2 || s.Leaf() != two {
		t.Errorf("after the log was written again: %v", err)
	}
}

// TestForkRefusesDoomedBase: a fork of a base another process holds as
// working state, after an earlier uncommitted append of that session
// whose objects are gone, is refused: recovery would cut the origin's
// log there and take the base with it.
func TestForkRefusesDoomedBase(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, _ := Open(root, WithSync(SyncNever))
	defer a.Close()
	a.Create(ctx, agentsession.Header{ID: "o"})
	x := mustAppend(t, a, "o", item("x"))
	gone := mustAppend(t, a, "o", item("gone"))
	sibling := item("base")
	sibling.Base().Parent = x // a sibling of gone, not its child
	base := mustAppend(t, a, "o", sibling)
	c, _ := a.contentOf(gone)
	cp, _ := a.objs.loosePath(spaceContents, c)
	os.Remove(cp)
	ep, _ := a.objs.loosePath(spaceEntries, gone)
	os.Remove(ep)
	b, _ := Open(root)
	defer b.Close()
	if _, err := b.Create(ctx, agentsession.Header{ID: "f", Base: base, ParentSession: "o"}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("a fork from a base its origin's recovery will cut: %v", err)
	}
}

// TestListAfterLostWorkingState: a listing taken while a writer held
// working state does not keep a summary a crash then makes wrong.
func TestListAfterLostWorkingState(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, _ := Open(root, WithSync(SyncNever))
	w.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, w, "s", item("kept"))
	w.Sync(ctx)
	lost := mustAppend(t, w, "s", item("lost"))
	size := func(st *Store) int64 {
		for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				t.Fatal(err)
			}
			return sum.Size
		}
		return -1
	}
	l, _ := Open(root)
	before := size(l)
	l.Close()
	die(w)
	p, _ := w.objs.loosePath(spaceEntries, lost)
	os.Remove(p)
	r, _ := Open(root, WithReadOnly())
	defer r.Close()
	if after := size(r); after >= before {
		t.Errorf("after the crash took an append, the listing still says %d bytes (was %d)", after, before)
	}
}

// TestCommitIsOneSession: a session's commit flushes the objects its own
// appends wrote, and leaves another session's working state to it.
func TestCommitIsOneSession(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "a"})
	st.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, st, "a", item("a's"))
	mustAppend(t, st, "b", item("b's"))
	if err := st.commitHandle("a", st.open["a"]); err != nil {
		t.Fatal(err)
	}
	if len(st.open["a"].pend.files) != 0 {
		t.Error("a's commit left a's objects unflushed")
	}
	if len(st.open["b"].pend.files) == 0 {
		t.Error("a's commit flushed b's working state")
	}
}
