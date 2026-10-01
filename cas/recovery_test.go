package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
// append the writer was told failed; and the store, whose fsync failed,
// writes nothing more until it is opened again.
func TestFailedCommitLeavesNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	first := mustAppend(t, st, "s", item("one"))
	old := syncLog
	syncLog = func(*os.File) error { return errors.New("injected fsync failure") }
	_, err := st.Append(ctx, "s", item("failed"))
	syncLog = old
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("the append: %v", err)
	}
	for name, write := range map[string]func() error{
		"append": func() error { _, err := st.Append(ctx, "s", item("two")); return err },
		"create": func() error { _, err := st.Create(ctx, agentsession.Header{ID: "n"}); return err },
		"delete": func() error { return st.Delete(ctx, "s") },
		"blob":   func() error { _, err := st.PutBlob(ctx, []byte("b")); return err },
		"sync":   func() error { return st.Sync(ctx) },
	} {
		if err := write(); !errors.Is(err, ErrStopped) {
			t.Errorf("%s on a stopped store: %v", name, err)
		}
	}
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	s, err := r.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != first {
		t.Errorf("reopened: %d entries at %s, want 1 at %s", s.Len(), s.Leaf(), first)
	}
	second := mustAppend(t, r, "s", item("two"))
	if s, _ := r.Open(ctx, "s"); s.Leaf() != second {
		t.Error("the reopened store does not write")
	}
}

// TestSharedFsyncFailureStops: two sessions commit an object they
// share; the first's fsync of it fails. The second's fsync of the same
// file, which the kernel would tell it succeeded, waits for the first
// and finds the store stopped, and its commit fails rather than claim
// bytes the disk may not hold.
func TestSharedFsyncFailureStops(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	for _, id := range []string{"a", "b"} {
		st.Create(ctx, agentsession.Header{ID: id})
		mustAppend(t, st, id, item("shared"))
	}
	old := syncObject
	defer func() { syncObject = old }()
	var mu sync.Mutex
	failed := false
	entered := make(chan struct{})
	syncObject = func(f *os.File) error {
		if !strings.Contains(f.Name(), string(filepath.Separator)+"contents"+string(filepath.Separator)) {
			return f.Sync() // each session's own entry
		}
		mu.Lock()
		first := !failed
		failed = true
		mu.Unlock()
		if first {
			close(entered)
			time.Sleep(100 * time.Millisecond)
			return errors.New("injected writeback failure")
		}
		return nil // as Linux says, once the failure was reported
	}
	errs := make(chan error, 2)
	go func() { errs <- st.Release("a") }()
	<-entered
	go func() { errs <- st.Release("b") }()
	for range 2 {
		if err := <-errs; !errors.Is(err, ErrStopped) {
			t.Errorf("a commit of the object whose fsync failed: %v", err)
		}
	}
}

// TestDirFsyncFailureStops: a failed fsync of a directory stops the
// store, at a session's create and at an object directory alike, so no
// append is committed into what a crash could still take.
func TestDirFsyncFailureStops(t *testing.T) {
	ctx := context.Background()
	for name, match := range map[string]string{"sessions": string(filepath.Separator) + "sessions", "objects": string(filepath.Separator) + "objects" + string(filepath.Separator)} {
		t.Run(name, func(t *testing.T) {
			st, _ := Open(t.TempDir())
			defer st.Close()
			if name == "objects" {
				st.Create(ctx, agentsession.Header{ID: "s"})
			}
			old := syncDirFile
			syncDirFile = func(d *os.File) error {
				if strings.Contains(d.Name(), match) {
					return errors.New("injected directory fsync failure")
				}
				return d.Sync()
			}
			var err error
			if name == "sessions" {
				_, err = st.Create(ctx, agentsession.Header{ID: "s"})
			} else {
				_, err = st.Append(ctx, "s", item("one"))
			}
			syncDirFile = old
			if !errors.Is(err, ErrStopped) {
				t.Fatalf("the write: %v", err)
			}
			if _, err := st.Append(ctx, "s", item("two")); !errors.Is(err, ErrStopped) {
				t.Errorf("an append after: %v", err)
			}
		})
	}
}

// TestMendFsyncFailureStops: recovery whose fsync of the mended log
// fails stops the store, rather than the next open taking the log as
// whole and committing on it.
func TestMendFsyncFailureStops(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("one"))
	st.Sync(ctx)
	crash(st)
	f, _ := os.OpenFile(filepath.Join(root, "sessions", "s", logName), os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"op":"app`) // a torn tail
	f.Close()
	w, _ := Open(root)
	defer w.Close()
	old := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	_, err := w.Open(ctx, "s")
	syncFile = old
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("recovery: %v", err)
	}
	if _, err := w.Append(ctx, "s", item("two")); !errors.Is(err, ErrStopped) {
		t.Errorf("an append after: %v", err)
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

// unwrite leaves unwritten, as a crash can on a filesystem that writes
// a file out of order, the log's blocks from the first block boundary
// at or after the start of the line holding marker: one block, when one
// is set, or to the log's end. It returns the entries of the appends
// whose lines end before the zeros, which recovery keeps.
func unwrite(t *testing.T, p, marker string, one bool) []string {
	t.Helper()
	data, _ := os.ReadFile(p)
	line := bytes.Index(data, []byte(marker))
	if line < 0 {
		t.Fatal("no such record")
	}
	line = bytes.LastIndexByte(data[:line], '\n') + 1
	from := (line + blockSize - 1) / blockSize * blockSize
	to := len(data)
	if one {
		to = min(from+blockSize, len(data))
	}
	if from >= to || (one && to == len(data)) {
		t.Fatalf("the log is too short: %d bytes, zeros from %d", len(data), from)
	}
	var kept []string
	off := 0
	for _, l := range bytes.SplitAfter(data, []byte("\n")) {
		if off+len(l) > from {
			break
		}
		recs, _ := decodeLine(l)
		for _, r := range recs {
			if r.Op == opAppend {
				kept = append(kept, r.Entry)
			}
		}
		off += len(l)
	}
	clear(data[from:to])
	os.WriteFile(p, data, 0o600)
	return kept
}

// TestZeroedLineCutsTail: a block a crash left unwritten, with
// committed records after it, is the cut of an uncommitted tail: no
// fsync of the log finished after it, so what follows, a commit that
// was in flight included, was never committed. Damage that changed a
// line's bytes is reported instead.
func TestZeroedLineCutsTail(t *testing.T) {
	ctx := context.Background()
	for _, zero := range []bool{true, false} {
		root := t.TempDir()
		st, _ := Open(root)
		st.Create(ctx, agentsession.Header{ID: "s"})
		a := mustAppend(t, st, "s", item("a"))
		b := mustAppend(t, st, "s", item("b"))
		if err := st.SetHead(ctx, "s", b, a); err != nil {
			t.Fatal(err)
		}
		for i := range 8 {
			mustAppend(t, st, "s", item(fmt.Sprint("c", i)))
		}
		st.Close()
		p := filepath.Join(root, "sessions", "s", logName)
		var kept []string
		if zero {
			kept = unwrite(t, p, b, true)
		} else {
			data, _ := os.ReadFile(p)
			i := bytes.Index(data, []byte(b))
			os.WriteFile(p, append(data[:i:i], bytes.Replace(data[i:], []byte(`"append"`), []byte(`"appenx"`), 1)...), 0o600)
		}
		r, _ := Open(root)
		s, err := r.Open(ctx, "s")
		switch {
		case zero && err != nil:
			t.Errorf("an unwritten block: %v", err)
		case zero && s.Len() != len(kept):
			t.Errorf("after the cut: %d entries, want the %d before the zeros", s.Len(), len(kept))
		case !zero && !errors.As(err, new(LogDamage)):
			t.Errorf("a changed line opened with %v, want its damage", err)
		}
		if zero {
			if rep, err := r.Verify(ctx); err != nil || !rep.OK() {
				t.Errorf("verify after the cut: %v %v", err, rep.Problems)
			}
		}
		r.Close()
	}
}

// TestUnterminatedLineChecked: a last line without its newline is read
// as any other line is. A record a crash cut short is a torn write, cut
// by the holder; a whole record whose newline became another byte, or
// one that fails its checksum, cannot come of a crash, and is damage.
func TestUnterminatedLineChecked(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		mangle func([]byte) []byte
		damage bool
	}{
		"cut short": {func(d []byte) []byte { return d[:len(d)-20] }, false},
		"newline":   {func(d []byte) []byte { d[len(d)-1] = 'x'; return d }, true},
		"checksum": {func(d []byte) []byte {
			d = d[:len(d)-1]
			i := bytes.LastIndex(d, []byte(`"seq":`)) + len(`"seq":`)
			d[i]++
			return d
		}, true},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			st, _ := Open(root)
			st.Create(ctx, agentsession.Header{ID: "s"})
			kept := mustAppend(t, st, "s", item("kept"))
			mustAppend(t, st, "s", item("last"))
			st.Close()
			p := filepath.Join(root, "sessions", "s", logName)
			data, _ := os.ReadFile(p)
			os.WriteFile(p, tc.mangle(data), 0o600)
			w, _ := Open(root)
			defer w.Close()
			s, err := w.Open(ctx, "s")
			switch {
			case tc.damage && !errors.As(err, new(LogDamage)):
				t.Errorf("opened with %v, want its damage", err)
			case !tc.damage && (err != nil || s.Leaf() != kept):
				t.Errorf("a torn write: %v", err)
			}
		})
	}
}

// TestSweepAfterTailCut: a sweep after recovery cut an unwritten tail,
// and recorded as lost an append whose envelope a crash left torn, is
// not stopped by that envelope.
func TestSweepAfterTailCut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	st.Sync(ctx)
	hole := mustAppend(t, st, "s", item("hole"))
	for i := range 6 {
		mustAppend(t, st, "s", item(fmt.Sprint("filler", i)))
	}
	torn := mustAppend(t, st, "s", item("torn"))
	crash(st)
	ep, _ := st.objs.loosePath(spaceEntries, torn)
	os.WriteFile(ep, []byte("x"), 0o600)
	p := filepath.Join(root, "sessions", "s", logName)
	unwrite(t, p, hole, true)
	if data, _ := os.ReadFile(p); !bytes.Contains(data, []byte(torn)) {
		t.Fatal("the torn append's record is not after the zeros")
	}
	w, _ := Open(root)
	defer w.Close()
	if _, err := w.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := w.Sweep(ctx, 0); err != nil {
			t.Errorf("a sweep after the cut: %v", err)
		}
	}
}

// TestUnwrittenTailCut: blocks a crash left unwritten in a log's
// uncommitted tail are the loss of that tail, cut as such, and not
// damage that closes the session.
func TestUnwrittenTailCut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hole := mustAppend(t, st, "s", item("hole"))
	for i := range 6 {
		mustAppend(t, st, "s", item(fmt.Sprint("after", i)))
	}
	crash(st)
	kept := unwrite(t, filepath.Join(root, "sessions", "s", logName), hole, false)
	w, _ := Open(root)
	defer w.Close()
	s, err := w.Open(ctx, "s")
	if err != nil {
		t.Fatalf("a crash's unwritten tail: %v", err)
	}
	if s.Len() != len(kept) || s.Leaf() != kept[len(kept)-1] {
		t.Errorf("after the cut: %d entries at %s, want %d at %s", s.Len(), s.Leaf(), len(kept), kept[len(kept)-1])
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

// TestSyncNeverCommitsRecords: under SyncNever an entry whose type the
// header names in records is committed before it is acknowledged, as
// RFC 0001 requires, and the rest stay lazy.
func TestSyncNeverCommitsRecords(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s", Records: agentsession.AllRecords})
	r, err := st.Write(ctx, "s", item("lazy"))
	if err != nil || r.Durable {
		t.Errorf("an item: durable %v, %v", r.Durable, err)
	}
	r, err = st.Write(ctx, "s", agentsession.NewRunStart("run-1", agentsession.SourceInput, ""))
	if err != nil || !r.Durable {
		t.Errorf("a run the header records: durable %v, %v", r.Durable, err)
	}
}

// TestSessionsIndependent: a session's commit, however slow its fsync,
// does not hold up another session's append or open in the same store.
func TestSessionsIndependent(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	for _, id := range []string{"slow", "fast", "other"} {
		st.Create(ctx, agentsession.Header{ID: id})
		mustAppend(t, st, id, item("one"))
	}
	st.Release("other")
	old := syncLog
	defer func() { syncLog = old }()
	inSlow := make(chan struct{})
	syncLog = func(f *os.File) error {
		if strings.Contains(f.Name(), string(filepath.Separator)+"slow"+string(filepath.Separator)) {
			close(inSlow)
			time.Sleep(300 * time.Millisecond)
		}
		return f.Sync()
	}
	done := make(chan error)
	go func() { done <- st.Release("slow") }()
	<-inSlow
	start := time.Now()
	if _, err := st.Append(ctx, "fast", item("two")); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Open(ctx, "other"); err != nil {
		t.Fatal(err)
	}
	if waited := time.Since(start); waited > 150*time.Millisecond {
		t.Errorf("another session waited %v behind a commit", waited)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentWriters: goroutines writing their own sessions and one
// they share, with commits, releases and reopens among them, leave
// every append in its session.
func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "shared"})
	const writers, appends = 8, 20
	var wg sync.WaitGroup
	for w := range writers {
		id := fmt.Sprintf("w%d", w)
		st.Create(ctx, agentsession.Header{ID: id})
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range appends {
				if _, err := st.Append(ctx, id, item(fmt.Sprintf("%s-%d", id, i))); err != nil {
					t.Error(err)
					return
				}
				if _, err := st.Append(ctx, "shared", item(fmt.Sprintf("shared-%s-%d", id, i))); err != nil {
					t.Error(err)
					return
				}
				switch i % 5 {
				case 1:
					st.Sync(ctx)
				case 3:
					st.Release(id)
				}
			}
		}()
	}
	wg.Wait()
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	r, _ := Open(root)
	defer r.Close()
	for w := range writers {
		if s, err := r.Open(ctx, fmt.Sprintf("w%d", w)); err != nil || s.Len() != appends {
			t.Errorf("w%d: %v", w, err)
		}
	}
	if s, err := r.Open(ctx, "shared"); err != nil || s.Len() != writers*appends {
		t.Errorf("shared: %v", err)
	}
}

// TestLazyLargeObjectNotTrusted: a large object written lazily, whose
// writeback failed in the background and whose pages were evicted
// before any fsync, is read and compared at its next reuse, not taken
// on the trust its write earned, and written again.
func TestLazyLargeObjectNotTrusted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	big := strings.Repeat("large content ", (64<<10)/14+1)
	st.Create(ctx, agentsession.Header{ID: "a"})
	e := mustAppend(t, st, "a", item(big))
	c, _ := st.contentOf(e)
	p, _ := st.objs.loosePath(spaceContents, c)
	info, _ := os.Stat(p)
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt(make([]byte, info.Size()), 0) // the same file, zeros
	f.Close()
	st.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, st, "b", item(big))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "b"); err != nil {
		t.Errorf("a session committed over a large object gone to zeros: %v", err)
	}
}

// TestNoSharedPending: what a fork, an import, an exchange, a blob and
// a recovery write, each flushes itself; nothing waits in the store's
// own set, where a flush of one caller could return before another's
// fsyncs had.
func TestNoSharedPending(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	defer st.Close()
	ids := fill(t, st, "a", 3)
	if _, err := st.Create(ctx, agentsession.Header{ID: "f", ParentSession: "a", Base: ids[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutBlob(ctx, []byte("blob")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := st.Project(ctx, &buf, "a"); err != nil {
		t.Fatal(err)
	}
	other, _ := Open(t.TempDir())
	defer other.Close()
	if _, err := other.Import(ctx, bytes.NewReader(buf.Bytes()), true); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Fetch(ctx, other, "a"); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*objects{st.objs, other.objs} {
		if n := len(o.pendFiles) + len(o.pendDirs); n != 0 {
			t.Errorf("%d paths left in a store's own pending set", n)
		}
	}
}

// TestSweepPastUnwrittenTail: a crash's unwritten tail in one session's
// log does not stop sweeps of the store before the session is opened.
func TestSweepPastUnwrittenTail(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	hole := mustAppend(t, st, "s", item("hole"))
	for i := range 6 {
		mustAppend(t, st, "s", item(fmt.Sprint("after", i)))
	}
	crash(st)
	unwrite(t, filepath.Join(root, "sessions", "s", logName), hole, false)
	w, _ := Open(root)
	defer w.Close()
	if _, err := w.Sweep(ctx, 0); err != nil {
		t.Errorf("a sweep past an unwritten tail: %v", err)
	}
}

// TestIndexBuildBlocksNothing: while a fork builds the index, reading
// every session, appends and opens of other sessions go on.
func TestIndexBuildBlocksNothing(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	ids := fill(t, st, "a", 2)
	st.Create(ctx, agentsession.Header{ID: "b"})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	scanning = func() {
		once.Do(func() {
			close(entered)
			<-release
		})
	}
	defer func() { scanning = nil }()
	done := make(chan error)
	go func() {
		// No parent named: the base is looked up in the index.
		_, err := st.Create(ctx, agentsession.Header{ID: "f", Base: ids[1]})
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("the fork ended before building the index: %v", err)
	}
	appended := make(chan error)
	go func() {
		_, err := st.Append(ctx, "b", item("during"))
		appended <- err
	}()
	waited := false
	select {
	case err := <-appended:
		if err != nil {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("an append waited on an index build")
		waited = true
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if waited {
		<-appended
	}
	if owner, held, err := st.holderOf(ids[0], ""); err != nil || !held || owner != "a" {
		t.Errorf("the index after the build: %s %v %v", owner, held, err)
	}
}

// TestExchangeWithSelf: a store does not push to or fetch from itself,
// nor from another Store open on its directory; a handover to itself
// would leave no store the record.
func TestExchangeWithSelf(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	fill(t, st, "s", 1)
	if _, err := st.Push(ctx, st, "s", PushOptions{Handover: true}); !errors.Is(err, ErrSameStore) {
		t.Errorf("a push to itself: %v", err)
	}
	twin, _ := Open(root, WithReadOnly())
	defer twin.Close()
	if _, err := st.Fetch(ctx, twin, "s"); !errors.Is(err, ErrSameStore) {
		t.Errorf("a fetch from its own directory: %v", err)
	}
	if m, _ := st.Mark(ctx, "s"); m != MarkRecord {
		t.Errorf("the mark after: %s", m)
	}
}

// TestCheckedFileChangedInPlace: a large object this store fsynced and
// remembers, changed in place since, is read and compared at its next
// reuse, not taken for the version it fsynced.
func TestCheckedFileChangedInPlace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	big := strings.Repeat("large content ", (64<<10)/14+1)
	st.Create(ctx, agentsession.Header{ID: "a"})
	e := mustAppend(t, st, "a", item(big))
	c, _ := st.contentOf(e)
	p, _ := st.objs.loosePath(spaceContents, c)
	if _, ok := st.objs.checked[p]; !ok && runtime.GOOS == "linux" {
		t.Fatal("a durable write was not remembered")
	}
	info, _ := os.Stat(p)
	f, _ := os.OpenFile(p, os.O_WRONLY, 0)
	f.WriteAt(make([]byte, info.Size()), 0)
	f.Close()
	st.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, st, "b", item(big))
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "b"); err != nil {
		t.Errorf("a session committed over a file changed in place: %v", err)
	}
}

// TestUncheckedRecordIsDamage: a record of a session's log whose
// checksum member damage renamed reads without its checksum, and is
// damage, not a record from before checksums.
func TestUncheckedRecordIsDamage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	fill(t, st, "s", 2)
	st.Close()
	p := filepath.Join(root, "sessions", "s", logName)
	data, _ := os.ReadFile(p)
	lines := bytes.SplitAfter(data, []byte("\n"))
	lines[2] = bytes.Replace(lines[2], []byte(`"crc":"`), []byte(`"crx":"`), 1)
	os.WriteFile(p, bytes.Join(lines, nil), 0o600)
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "s"); !errors.As(err, new(LogDamage)) {
		t.Errorf("a record without its checksum: %v", err)
	}
}

// TestPushKeepsMedia: a push that creates a fork at the receiver is
// held to the media of the session there holding the base, as a create
// or an import is.
func TestPushKeepsMedia(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	ids := fill(t, mir, "o", 2) // inline, at the receiver
	var buf bytes.Buffer
	if err := mir.Project(ctx, &buf, "o"); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Import(ctx, bytes.NewReader(buf.Bytes()), true); err != nil {
		t.Fatal(err)
	}
	if _, err := rec.Create(ctx, agentsession.Header{ID: "f", Base: ids[1], ParentSession: "o", Media: agentsession.MediaSidecar}); err == nil {
		// The sender's own check refuses it; build the fork's header by
		// hand, as a sender of another implementation might.
		t.Skip("the sender took a fork of other media")
	}
	hdr := agentsession.New(agentsession.Header{ID: "f", Base: ids[1], ParentSession: "o", Media: agentsession.MediaSidecar}).Header()
	b := &bundle{header: hdr, mark: MarkRecord, head: ids[1], blobs: map[string][]byte{}}
	if _, err := mir.receive(ctx, b, receiveOptions{push: true, expected: ids[1]}); err == nil || !strings.Contains(err.Error(), "media") {
		t.Errorf("a push of a fork whose media differs from its origin's: %v", err)
	}
}

// TestSingleZeroByteIsDamage: one byte of a committed record changed to
// zero is damage, not a block a crash left unwritten, and the log is
// left as it is.
func TestSingleZeroByteIsDamage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("a"))
	b := mustAppend(t, st, "s", item("b"))
	mustAppend(t, st, "s", item("c"))
	st.Close()
	p := filepath.Join(root, "sessions", "s", logName)
	data, _ := os.ReadFile(p)
	i := bytes.Index(data, []byte(b)) + 10
	for i%blockSize == 0 || (i+1)%blockSize == 0 {
		i++
	}
	data[i] = 0
	os.WriteFile(p, data, 0o600)
	r, _ := Open(root)
	defer r.Close()
	if _, err := r.Open(ctx, "s"); !errors.As(err, new(LogDamage)) {
		t.Errorf("a committed record with one byte zeroed: %v", err)
	}
	if after, _ := os.ReadFile(p); !bytes.Equal(after, data) {
		t.Error("the open changed the damaged log")
	}
}

// TestRecoveryWritesAfresh: recovery that keeps working state writes
// the log, and the objects of that state, as new files, rather than
// fsync what it found: after a failed fsync, a process's pages it never
// wrote stay in memory marked written, and an fsync of them now would
// succeed. So does recovery after the store stopped and was reopened.
func TestRecoveryWritesAfresh(t *testing.T) {
	ctx := context.Background()
	for _, stopped := range []bool{false, true} {
		root := t.TempDir()
		st, _ := Open(root, WithSync(SyncNever))
		st.Create(ctx, agentsession.Header{ID: "s"})
		mustAppend(t, st, "s", item("kept"))
		st.Sync(ctx)
		x := mustAppend(t, st, "s", item("working"))
		p := filepath.Join(root, "sessions", "s", logName)
		ep, _ := st.objs.loosePath(spaceEntries, x)
		if stopped {
			old := syncLog
			syncLog = func(*os.File) error { return errors.New("injected fsync failure") }
			// The commit fails, and its sync record is cut back out.
			if err := st.Sync(ctx); !errors.Is(err, ErrStopped) {
				t.Fatalf("the commit: %v", err)
			}
			syncLog = old
			st.Close()
		} else {
			crash(st)
		}
		logBefore, _ := os.Stat(p)
		objBefore, _ := os.Stat(ep)
		w, _ := Open(root)
		s, err := w.Open(ctx, "s")
		if err != nil || s.Leaf() != x {
			t.Fatalf("recovery: %v", err)
		}
		if now, _ := os.Stat(p); os.SameFile(logBefore, now) {
			t.Errorf("stopped %v: recovery fsynced the log it found", stopped)
		}
		if now, _ := os.Stat(ep); os.SameFile(objBefore, now) {
			t.Errorf("stopped %v: recovery fsynced an object it found", stopped)
		}
		w.Close()
	}
}

// TestStoppedStoreReads: a stopped store still reads, a session whose
// commit failed included, and an open of one with a torn tail recovers
// it in memory and writes nothing.
func TestStoppedStoreReads(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, st, "b", item("b"))
	st.Release("b")
	bp := filepath.Join(root, "sessions", "b", logName)
	f, _ := os.OpenFile(bp, os.O_WRONLY|os.O_APPEND, 0)
	f.WriteString(`{"op":"app`)
	f.Close()
	torn, _ := os.ReadFile(bp)
	st.Create(ctx, agentsession.Header{ID: "a"})
	a := mustAppend(t, st, "a", item("a"))
	old := syncLog
	syncLog = func(*os.File) error { return errors.New("injected fsync failure") }
	err := st.Sync(ctx)
	syncLog = old
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("the commit: %v", err)
	}
	defer st.Close()
	if s, err := st.Open(ctx, "a"); err != nil || s.Leaf() != a {
		t.Errorf("reading the session whose commit failed: %v", err)
	}
	if _, err := st.Open(ctx, "b"); err != nil {
		t.Errorf("reading a session with a torn tail: %v", err)
	}
	if after, _ := os.ReadFile(bp); !bytes.Equal(after, torn) {
		t.Error("a stopped store changed a log")
	}
}

// TestSweepStops: a sweep whose last fsync fails reports the stop, and
// removes nothing more.
func TestSweepStops(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	defer st.Close()
	fill(t, st, "s", 3)
	old := syncDirFile
	defer func() { syncDirFile = old }()
	pack := string(filepath.Separator) + "pack"
	calls := 0
	syncDirFile = func(d *os.File) error {
		if strings.HasSuffix(d.Name(), pack) {
			if calls++; calls > 1 {
				return errors.New("injected directory fsync failure")
			}
		}
		return d.Sync()
	}
	loose := func() int {
		n := 0
		for _, sp := range []space{spaceEntries, spaceContents} {
			st.objs.eachLoose(sp, func(string, string, os.FileInfo, bool) error { n++; return nil })
		}
		return n
	}
	before := loose()
	if _, err := st.Sweep(ctx, 0); !errors.Is(err, ErrStopped) {
		t.Errorf("the sweep: %v", err)
	}
	if loose() != before {
		t.Error("a sweep went on removing after the store stopped")
	}
}

// TestLazyCopyNeverReplaces: a lazy append of content another store
// committed puts its own copy in place durably, never an unsynced file
// over the committed one, whose bytes a crash could otherwise leave
// zeros; and a lazy write of a new object does not take the place of
// one written there meanwhile.
func TestLazyCopyNeverReplaces(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root)
	st.Create(ctx, agentsession.Header{ID: "a"})
	e := mustAppend(t, st, "a", item("shared tool output"))
	c, _ := st.contentOf(e)
	st.Close()
	p, _ := st.objs.loosePath(spaceContents, c)
	before, _ := os.Stat(p)
	lazy, _ := Open(root, WithSync(SyncNever))
	defer lazy.Close()
	lazy.Create(ctx, agentsession.Header{ID: "b"})
	mustAppend(t, lazy, "b", item("shared tool output"))
	after, _ := os.Stat(p)
	if !os.SameFile(before, after) && lazy.open["b"].pend.files[p] {
		t.Error("an unsynced copy took the place of a committed object")
	}
	other := filepath.Join(t.TempDir(), "o")
	os.WriteFile(other, []byte("there first"), 0o600)
	if _, err := writeFileWith(other, []byte("late"), nil, false); !errors.Is(err, os.ErrExist) {
		t.Errorf("a write that must not replace, over a file: %v", err)
	}
	if got, _ := os.ReadFile(other); string(got) != "there first" {
		t.Errorf("the file there first is now %q", got)
	}
}

// TestNoHardLinks: on a filesystem that makes no hard links, a lazy
// write of a new object is written durably instead, and the store goes
// on writing.
func TestNoHardLinks(t *testing.T) {
	ctx := context.Background()
	old := linkFile
	defer func() { linkFile = old }()
	links := 0
	linkFile = func(string, string) error {
		links++
		return &os.LinkError{Op: "link", Err: errors.ErrUnsupported}
	}
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	for i := range 3 {
		mustAppend(t, st, "s", item(fmt.Sprint("on vfat ", i)))
	}
	if links != 1 {
		t.Errorf("tried %d links, want 1 and then none", links)
	}
	st.Close()
	r, _ := Open(root)
	defer r.Close()
	if s, err := r.Open(ctx, "s"); err != nil || s.Len() != 3 {
		t.Errorf("reopened: %v", err)
	}
}

// TestUnsyncedStaysOwed: an object a commit could not open to fsync,
// for a reason that is no failed fsync, stays owed: the commit fails,
// the store goes on, and the next commit fsyncs it before its sync
// record says it is durable.
func TestUnsyncedStaysOwed(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir(), WithSync(SyncNever))
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "s"})
	e := mustAppend(t, st, "s", item("one"))
	p, _ := st.objs.loosePath(spaceEntries, e)
	os.Chmod(p, 0)
	err := st.Sync(ctx)
	os.Chmod(p, 0o644)
	if err == nil {
		t.Skip("running as a user that can read a mode-0 file")
	}
	if errors.Is(err, ErrStopped) {
		t.Fatalf("a file that would not open stopped the store: %v", err)
	}
	old := syncObject
	defer func() { syncObject = old }()
	synced := false
	syncObject = func(f *os.File) error {
		if f.Name() == p {
			synced = true
		}
		return f.Sync()
	}
	if err := st.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !synced {
		t.Error("the next commit's sync record claims an object it never fsynced")
	}
}

// TestStoppedStoreServesCommitted: a stopped store recovering a session
// in memory keeps its working state uncommitted, so it serves none of
// it, and writes no index of it when it lets the session go.
func TestStoppedStoreServesCommitted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	st.Sync(ctx)
	mustAppend(t, st, "s", item("working"))
	crash(st)
	head := filepath.Join(root, "sessions", "s", "HEAD")
	before, _ := os.ReadFile(head)
	w, _ := Open(root)
	old := syncFile
	syncFile = func(*os.File) error { return errors.New("injected fsync failure") }
	_, err := w.PutBlob(ctx, []byte("blob"))
	syncFile = old
	if !errors.Is(err, ErrStopped) {
		t.Fatalf("the blob: %v", err)
	}
	if _, err := w.Open(ctx, "s"); err != nil {
		t.Fatalf("reading the session: %v", err)
	}
	r, _ := Open(t.TempDir())
	defer r.Close()
	if _, err := r.Fetch(ctx, w, "s"); !errors.Is(err, ErrStopped) {
		t.Errorf("a fetch of uncommitted state from a stopped store: %v", err)
	}
	w.Close()
	if after, _ := os.ReadFile(head); !bytes.Equal(before, after) {
		t.Errorf("a stopped store rewrote HEAD: %q to %q", before, after)
	}
}

// TestDamageAfterCut: bytes changed after a block a crash left
// unwritten are in the uncommitted tail the block cuts, and keep the
// session closed no more than the block does.
func TestDamageAfterCut(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, _ := Open(root, WithSync(SyncNever))
	st.Create(ctx, agentsession.Header{ID: "s"})
	mustAppend(t, st, "s", item("kept"))
	st.Sync(ctx)
	hole := mustAppend(t, st, "s", item("hole"))
	for i := range 6 {
		mustAppend(t, st, "s", item(fmt.Sprint("after", i)))
	}
	crash(st)
	p := filepath.Join(root, "sessions", "s", logName)
	kept := unwrite(t, p, hole, true)
	data, _ := os.ReadFile(p)
	i := bytes.LastIndex(data, []byte(`"seq":`)) + len(`"seq":`)
	data[i]++
	os.WriteFile(p, data, 0o600)
	w, _ := Open(root)
	defer w.Close()
	s, err := w.Open(ctx, "s")
	if err != nil {
		t.Fatalf("damage after the cut: %v", err)
	}
	if s.Len() != len(kept) {
		t.Errorf("after the cut: %d entries, want %d", s.Len(), len(kept))
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
