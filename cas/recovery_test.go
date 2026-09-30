package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if len(st.objs.pendFiles) == 0 || !st.open["n"].lazy {
		t.Fatal("nothing pending after a lazy append")
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if len(st.objs.pendFiles) != 0 || len(st.objs.pendDirs) != 0 || st.open["n"].lazy {
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
