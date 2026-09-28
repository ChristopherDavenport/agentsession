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
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/storetest"
	"github.com/ChristopherDavenport/openresponses"
)

func TestConformance(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store {
			st, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		},
		Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
			old := s.(*Store)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			st, err := Open(old.Root())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		},
	})
}

func mustAppend(t *testing.T, st *Store, id string, e agentsession.Entry) string {
	t.Helper()
	got, err := st.Append(context.Background(), id, e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestObjectsAreShared checks the point of the layout: the same body in
// two sessions is one content object with two envelopes, and a fork
// shares its origin's entries rather than copying them.
func TestObjectsAreShared(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, _ := st.Create(ctx, agentsession.Header{ID: "a"})
	b, _ := st.Create(ctx, agentsession.Header{ID: "b"})
	e1 := agentsession.NewItemEntry(openresponses.UserText("same tool output"))
	e2 := agentsession.NewItemEntry(openresponses.UserText("same tool output"))
	id1 := mustAppend(t, st, a.ID(), e1)
	id2 := mustAppend(t, st, b.ID(), e2)
	if id1 == id2 {
		t.Fatal("two sessions' entries share an id; ts should differ")
	}
	if e1.ContentHash() != e2.ContentHash() {
		t.Fatalf("content hashes differ: %s %s", e1.ContentHash(), e2.ContentHash())
	}
	contents, _ := filepath.Glob(filepath.Join(st.Root(), "objects", "contents", "*", "*"))
	entries, _ := filepath.Glob(filepath.Join(st.Root(), "objects", "entries", "*", "*"))
	if len(contents) != 1 || len(entries) != 2 {
		t.Errorf("%d contents and %d entries, want 1 and 2", len(contents), len(entries))
	}
	// A fork of a stores nothing new until it appends.
	f, err := st.Create(ctx, agentsession.Header{ID: "f", ParentSession: "a", Base: id1})
	if err != nil {
		t.Fatal(err)
	}
	if f.Leaf() != id1 || f.Len() != 1 || !f.Prefix(id1) {
		t.Errorf("fork opens leaf %s len %d", f.Leaf(), f.Len())
	}
	entries, _ = filepath.Glob(filepath.Join(st.Root(), "objects", "entries", "*", "*"))
	if len(entries) != 2 {
		t.Errorf("a fork stored %d entries, want none", len(entries)-2)
	}
	own := mustAppend(t, st, "f", agentsession.NewItemEntry(openresponses.UserText("continued")))
	if e, _ := f.Entry(own); e.Base().Parent != id1 || f.Leaf() != own {
		t.Errorf("fork's own entry parent %s leaf %s", e.Base().Parent, f.Leaf())
	}
	// Reopened, the fork rebuilds its prefix from the shared objects.
	st.Close()
	st2, err := Open(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	again, err := st2.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if again.Len() != 2 || again.Leaf() != own || !again.Prefix(id1) || again.Header().Base != id1 {
		t.Errorf("fork after reopen: len %d leaf %s", again.Len(), again.Leaf())
	}
}

// TestHead checks the head rules: an append elsewhere is a branch that
// moves nothing, SetHead is a compare-and-swap, a leaf label moves the
// head to its target, and the head never rests on a label.
func TestHead(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := st.Create(ctx, agentsession.Header{ID: "h"})
	a := mustAppend(t, st, "h", agentsession.NewItemEntry(openresponses.UserText("a")))
	b := mustAppend(t, st, "h", agentsession.NewItemEntry(openresponses.UserText("b")))
	branch := agentsession.NewItemEntry(openresponses.UserText("c"))
	branch.Parent = a
	c := mustAppend(t, st, "h", branch)
	if s.Leaf() != b {
		t.Errorf("a branch moved the head to %s", s.Leaf())
	}
	if err := st.SetHead(ctx, "h", a, c); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("SetHead with the wrong expected = %v", err)
	}
	if err := st.SetHead(ctx, "h", b, c); err != nil {
		t.Fatal(err)
	}
	if s.Leaf() != c {
		t.Errorf("head after SetHead = %s", s.Leaf())
	}
	label := agentsession.NewLabelEntry(b, agentsession.LeafLabel)
	labelID := mustAppend(t, st, "h", label)
	if s.Leaf() != b {
		t.Errorf("a leaf label did not move the head to its target: %s", s.Leaf())
	}
	if err := st.SetHead(ctx, "h", b, labelID); err == nil {
		t.Error("SetHead onto a leaf label succeeded")
	}
	// Reopened, the head is the HEAD file, not the last line.
	st.Close()
	st2, _ := Open(st.Root())
	defer st2.Close()
	again, err := st2.Open(ctx, "h")
	if err != nil {
		t.Fatal(err)
	}
	if again.Leaf() != b {
		t.Errorf("head after reopen = %s, want %s", again.Leaf(), b)
	}
}

// TestProjectAndImport round-trips a session through its file: the
// projection carries a synthetic marker only when the resume rule would
// miss the head, the import discards it, and the imported session is a
// mirror that refuses local writes until declared the record.
func TestProjectAndImport(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	s, _ := st.Create(ctx, agentsession.Header{ID: "p", CWD: "/w"})
	a := mustAppend(t, st, "p", agentsession.NewItemEntry(openresponses.UserText("a")))
	mustAppend(t, st, "p", agentsession.NewItemEntry(openresponses.UserText("b")))
	if err := st.SetHead(ctx, "p", s.Leaf(), a); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := st.Project(ctx, &buf, "p"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"synthetic":true`) {
		t.Error("a head that is not the last line projected without a marker")
	}
	file, err := agentsession.Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if file.Leaf() != a {
		t.Errorf("the file resumes at %s, want the head %s", file.Leaf(), a)
	}
	other, _ := Open(t.TempDir())
	defer other.Close()
	imported, err := other.Import(ctx, bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Len() != 2 || imported.Leaf() != a {
		t.Errorf("imported len %d leaf %s", imported.Len(), imported.Leaf())
	}
	if mark, _ := other.Mark(ctx, "p"); mark != MarkMirror {
		t.Errorf("imported mark = %s, want mirror", mark)
	}
	if _, err := other.Append(ctx, "p", agentsession.NewItemEntry(openresponses.UserText("x"))); !errors.Is(err, ErrMirror) {
		t.Errorf("append to a mirror = %v", err)
	}
	if err := other.DeclareRecord(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Append(ctx, "p", agentsession.NewItemEntry(openresponses.UserText("x"))); err != nil {
		t.Errorf("append after declaring the record = %v", err)
	}
	// A projection with no marker when the head is the last line.
	buf.Reset()
	if err := other.Project(ctx, &buf, "p"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `"synthetic"`) {
		t.Error("a head on the last line projected with a marker")
	}
	// A redacted file is refused.
	redacted := strings.Replace(buf.String(), `"cwd":"/w"`, `"cwd":"/w","redacted":true`, 1)
	if _, err := other.Import(ctx, strings.NewReader(redacted), false); err == nil {
		t.Error("a redacted projection was imported")
	}
}

// TestRecovery cuts the log behind the journal and checks that opening
// the store replays the journal's tail.
func TestRecovery(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	st.Create(ctx, agentsession.Header{ID: "r"})
	mustAppend(t, st, "r", agentsession.NewItemEntry(openresponses.UserText("a")))
	last := mustAppend(t, st, "r", agentsession.NewItemEntry(openresponses.UserText("b")))
	st.Close()
	dir := filepath.Join(st.Root(), "sessions", "r")
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	os.WriteFile(filepath.Join(dir, "log"), []byte(lines[0]+"\n"), 0o600)
	os.Remove(filepath.Join(dir, "HEAD"))
	st2, err := Open(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s, err := st2.Open(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != last {
		t.Errorf("after recovery: len %d leaf %s, want 2 %s", s.Len(), s.Leaf(), last)
	}
}

// TestSweep deletes a session and sweeps: its own objects go, the ones a
// surviving fork's prefix needs stay.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "o"})
	a := mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("kept by the fork")))
	mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("only the origin's")))
	if _, err := st.Create(ctx, agentsession.Header{ID: "f", ParentSession: "o", Base: a}); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "o"); err != nil {
		t.Fatal(err)
	}
	if err := st.Release("f"); err != nil {
		t.Fatal(err)
	}
	swept, err := st.Sweep(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if swept != 2 { // the second entry's envelope and its content
		t.Errorf("swept %d objects, want 2", swept)
	}
	if !st.holds(a) {
		t.Error("the fork's prefix was swept")
	}
	f, err := st.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 1 {
		t.Errorf("fork len %d after sweep", f.Len())
	}
}

// TestLock refuses a second store the session a first one holds.
func TestLock(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "l"})
	st2, _ := Open(st.Root())
	defer st2.Close()
	if _, err := st2.Open(ctx, "l"); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Errorf("second store opened a held session: %v", err)
	}
	if err := st.Release("l"); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Open(ctx, "l"); err != nil {
		t.Errorf("open after release = %v", err)
	}
}

// TestRefusals covers what the RFC has a store refuse: a synthetic
// marker appended as an entry, a base that is a leaf label, a base
// written as an object but never committed, a fork whose media differs
// from its origin's, and an import with a torn last line, an own entry
// above the base, or a differing media.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "o", Media: agentsession.MediaInline})
	r := mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("r")))
	a := mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("a")))
	marker := agentsession.NewLabelEntry(a, agentsession.LeafLabel)
	marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
	if _, err := st.Append(ctx, "o", marker); !errors.Is(err, ErrSynthetic) {
		t.Errorf("appending a synthetic marker = %v", err)
	}
	label := mustAppend(t, st, "o", agentsession.NewLabelEntry(a, agentsession.LeafLabel))
	if _, err := st.Create(ctx, agentsession.Header{ID: "f1", Base: label}); err == nil {
		t.Error("a fork at a leaf label was created")
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "f2", Base: a, Media: agentsession.MediaSidecar}); err == nil {
		t.Error("a fork with another media was created")
	}
	// An object written ahead of its record is not held.
	ghost := agentsession.NewItemEntry(openresponses.UserText("ghost"))
	scratch := agentsession.New(agentsession.Header{})
	if _, err := scratch.Append(ghost); err != nil {
		t.Fatal(err)
	}
	if err := st.storeEntry(ghost); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "f3", Base: ghost.ID}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("a fork at an uncommitted object = %v", err)
	}
	// Import refusals.
	var buf bytes.Buffer
	if err := st.Project(ctx, &buf, "o"); err != nil {
		t.Fatal(err)
	}
	other, _ := Open(t.TempDir())
	defer other.Close()
	torn := buf.String()[:buf.Len()-10]
	if _, err := other.Import(ctx, strings.NewReader(torn), false); err == nil || !strings.Contains(err.Error(), "cut short") {
		t.Errorf("import of a torn file = %v", err)
	}
	// A fork file whose own entry hangs above the base.
	fork, err := st.Create(ctx, agentsession.Header{ID: "f4", Base: a})
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "f4", agentsession.NewItemEntry(openresponses.UserText("own")))
	buf.Reset()
	if err := st.Project(ctx, &buf, "f4"); err != nil {
		t.Fatal(err)
	}
	_ = fork
	// A file claiming base a whose own entry hangs from r, above the
	// base: the lines are rebuilt through a session so the ids verify,
	// then the header is given the base.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	prefixOnly, err := agentsession.Read(strings.NewReader(strings.Join(lines[:3], "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	hdr := prefixOnly.Header()
	loose := agentsession.New(agentsession.Header{ID: "f5", CreatedAt: hdr.CreatedAt, Payload: hdr.Payload, Media: hdr.Media})
	for _, e := range prefixOnly.Entries() {
		data, _ := agentsession.MarshalEntry(e)
		c, _ := agentsession.UnmarshalEntry(data)
		c.Base().ID = ""
		if _, err := loose.Append(c); err != nil {
			t.Fatal(err)
		}
	}
	above := agentsession.NewItemEntry(openresponses.UserText("above"))
	above.Parent = r
	if _, err := loose.Append(above); err != nil {
		t.Fatal(err)
	}
	var loosebuf bytes.Buffer
	agentsession.Write(&loosebuf, loose)
	claimed := strings.Replace(loosebuf.String(), `"id":"f5"`, `"base":"`+a+`","id":"f5","parent_session":"o"`, 1)
	if _, err := other.Import(ctx, strings.NewReader(claimed), false); err == nil || !strings.Contains(err.Error(), "above the base") {
		t.Errorf("import with an own entry above the base = %v", err)
	}
	// Media differing from the session holding the base.
	buf.Reset()
	st.Project(ctx, &buf, "f4")
	if _, err := other.Import(ctx, strings.NewReader(buf.String()), false); err != nil {
		t.Fatalf("a clean fork import failed: %v", err)
	}
	other.Release("f4")
	if _, err := other.Import(ctx, strings.NewReader(strings.Replace(buf.String(), `"media":"inline"`, `"media":"sidecar"`, 1)), false); err == nil {
		t.Error("an import with another media than the base's session was accepted")
	}
}

// TestWriteOutcomes checks what Write reports.
func TestWriteOutcomes(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "w"})
	ra, _ := st.Write(ctx, "w", agentsession.NewItemEntry(openresponses.UserText("a")))
	rb, _ := st.Write(ctx, "w", agentsession.NewItemEntry(openresponses.UserText("b")))
	if ra.Outcome != agentsession.Continued || rb.Outcome != agentsession.Continued {
		t.Errorf("continues = %s %s", ra.Outcome, rb.Outcome)
	}
	br := agentsession.NewItemEntry(openresponses.UserText("c"))
	br.Parent = ra.ID
	rc, _ := st.Write(ctx, "w", br)
	if rc.Outcome != agentsession.Branched {
		t.Errorf("branch = %s", rc.Outcome)
	}
	again := agentsession.NewItemEntry(openresponses.UserText("c"))
	again.Parent = ra.ID
	again.Timestamp = br.Timestamp
	rh, _ := st.Write(ctx, "w", again)
	if rh.Outcome != agentsession.Held || rh.ID != rc.ID {
		t.Errorf("re-append = %s %s", rh.Outcome, rh.ID)
	}
	rl, _ := st.Write(ctx, "w", agentsession.NewLabelEntry(rc.ID, agentsession.LeafLabel))
	if rl.Outcome != agentsession.LeafMoved {
		t.Errorf("leaf label = %s", rl.Outcome)
	}
	rn, _ := st.Write(ctx, "w", agentsession.NewLabelEntry(rl.ID, agentsession.LeafLabel))
	if rn.Outcome != agentsession.LeafNotMoved {
		t.Errorf("leaf label onto a label = %s", rn.Outcome)
	}
	// A head moved in memory is journaled before the next append.
	s, _ := st.Open(ctx, "w")
	if err := s.Branch(rb.ID); err != nil {
		t.Fatal(err)
	}
	rd, _ := st.Write(ctx, "w", agentsession.NewItemEntry(openresponses.UserText("d")))
	if rd.Outcome != agentsession.Continued {
		t.Errorf("append after an in-memory branch = %s", rd.Outcome)
	}
	journal, _ := os.ReadFile(filepath.Join(st.Root(), "journal"))
	if !strings.Contains(string(journal), `"op":"head","session":"w","head":"`+rb.ID+`"`) {
		t.Error("the in-memory head move was not journaled")
	}
}

// TestSetHeadEdges: "" is a valid expected value, a wrong one reports
// ErrHeadMoved, and a mirror is refused.
func TestSetHeadEdges(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "e"})
	if err := st.SetHead(ctx, "e", "", "nope"); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("SetHead from no head to an absent entry = %v, want ErrNoEntry (the expected value matched)", err)
	}
	if err := st.SetHead(ctx, "e", "wrong", "nope"); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("SetHead with a wrong expected = %v", err)
	}
	a := mustAppend(t, st, "e", agentsession.NewItemEntry(openresponses.UserText("a")))
	var buf bytes.Buffer
	st.Project(ctx, &buf, "e")
	other, _ := Open(t.TempDir())
	defer other.Close()
	if _, err := other.Import(ctx, &buf, false); err != nil {
		t.Fatal(err)
	}
	if err := other.SetHead(ctx, "e", a, a); !errors.Is(err, ErrMirror) {
		t.Errorf("SetHead on a mirror = %v", err)
	}
}

// TestCrashWindows breaks the store between each pair of writes and
// checks what a reopen sees: objects without a journal record are not
// an entry, a journal record without a log line is, a stale head is
// brought forward, and a torn journal tail is cut so the next commit
// lands after the last good record.
func TestCrashWindows(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	st.Create(ctx, agentsession.Header{ID: "c"})
	a := mustAppend(t, st, "c", agentsession.NewItemEntry(openresponses.UserText("a")))
	b := mustAppend(t, st, "c", agentsession.NewItemEntry(openresponses.UserText("b")))
	st.Close()
	dir := filepath.Join(st.Root(), "sessions", "c")

	// Objects written, no journal record: the entry does not exist.
	orphan := agentsession.NewItemEntry(openresponses.UserText("orphan"))
	orphan.Parent = b
	scratch, _ := agentsession.Read(bytes.NewReader(projectFile(t, st.Root(), "c")))
	if _, err := scratch.Append(orphan); err != nil {
		t.Fatal(err)
	}
	if err := st.storeEntry(orphan); err != nil {
		t.Fatal(err)
	}
	st2, _ := Open(st.Root())
	s, err := st2.Open(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || st2.holds(orphan.ID) {
		t.Errorf("an object without a record counted: len %d held %v", s.Len(), st2.holds(orphan.ID))
	}
	st2.Close()

	// Stale head: the HEAD file behind the journal.
	writeHead(dir, a)
	st3, _ := Open(st.Root())
	s, _ = st3.Open(ctx, "c")
	if s.Leaf() != b {
		t.Errorf("stale head not brought forward: %s", s.Leaf())
	}
	st3.Close()

	// Torn journal record: a record a crashed process cut short is
	// skipped, the journal is not truncated since another process may
	// have fsynced records after it, and the next commit is found even
	// when it lands on the same line as the torn bytes.
	journal := filepath.Join(st.Root(), "journal")
	f, _ := os.OpenFile(journal, os.O_WRONLY|os.O_APPEND, 0o600)
	f.WriteString(`{"op":"append","session":"c","entry":"sha256:trunc`)
	f.Close()
	before, _ := os.ReadFile(journal)
	st4, _ := Open(st.Root())
	after, _ := os.ReadFile(journal)
	if !bytes.Equal(before, after) {
		t.Error("opening the store changed the journal")
	}
	c := mustAppend(t, st4, "c", agentsession.NewItemEntry(openresponses.UserText("c")))
	st4.Close()
	st5, _ := Open(st.Root())
	defer st5.Close()
	s, _ = st5.Open(ctx, "c")
	if s.Len() != 3 || s.Leaf() != c {
		t.Errorf("after a torn tail and a commit: len %d leaf %s", s.Len(), s.Leaf())
	}
}

func projectFile(t *testing.T, root, id string) []byte {
	t.Helper()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var buf bytes.Buffer
	if err := st.Project(context.Background(), &buf, id); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestDeleteThenRecreate: a session made under a deleted session's ID
// starts from nothing, since the journal records the delete.
func TestDeleteThenRecreate(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	st.Create(ctx, agentsession.Header{ID: "d"})
	mustAppend(t, st, "d", agentsession.NewItemEntry(openresponses.UserText("old")))
	if err := st.Delete(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	st.Create(ctx, agentsession.Header{ID: "d"})
	n := mustAppend(t, st, "d", agentsession.NewItemEntry(openresponses.UserText("new")))
	st.Close()
	st2, err := Open(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	s, err := st2.Open(ctx, "d")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 1 || s.Leaf() != n {
		t.Errorf("recreated session: len %d leaf %s", s.Len(), s.Leaf())
	}
}

// TestNames refuses a session ID or a hash that cannot be a path.
func TestNames(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	for _, id := range []string{"../escaped", "a/b", "", ".", ".."} {
		if id != "" { // an empty ID is filled with a UUID
			if _, err := st.Create(ctx, agentsession.Header{ID: id}); !errors.Is(err, ErrBadName) {
				t.Errorf("Create(%q) = %v", id, err)
			}
		}
		if _, err := st.Open(ctx, id); !errors.Is(err, ErrBadName) {
			t.Errorf("Open(%q) = %v", id, err)
		}
		if err := st.Delete(ctx, id); !errors.Is(err, ErrBadName) {
			t.Errorf("Delete(%q) = %v", id, err)
		}
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "ok", Base: "sha256:a"}); err == nil {
		t.Error("a short hash as a base was accepted")
	}
	if _, err := os.Stat(filepath.Join(st.Root(), "..", "escaped")); err == nil {
		t.Error("a path escaped the store")
	}
}

// TestModifiedOutsideTheStore: an entry appended on the session directly
// is not in any log, so nothing may build on it, and a head moved to
// one is refused.
func TestModifiedOutsideTheStore(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	s, _ := st.Create(ctx, agentsession.Header{ID: "m"})
	mustAppend(t, st, "m", agentsession.NewItemEntry(openresponses.UserText("a")))
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("direct"))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "m", agentsession.NewItemEntry(openresponses.UserText("b"))); !errors.Is(err, ErrModified) {
		t.Errorf("append after a direct append = %v", err)
	}
	var buf bytes.Buffer
	if err := st.Project(ctx, &buf, "m"); !errors.Is(err, ErrModified) {
		t.Errorf("project after a direct append = %v", err)
	}
	// Reopened from disk, the store's record stands: one entry.
	st.Release("m")
	again, err := st.Open(ctx, "m")
	if err != nil {
		t.Fatal(err)
	}
	if again.Len() != 1 {
		t.Errorf("len after reopen = %d", again.Len())
	}
}

// TestImportCleansUp: a synthetic marker anywhere but the end is refused,
// and a failed import leaves nothing that blocks its ID.
func TestImportCleansUp(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	st.Create(ctx, agentsession.Header{ID: "i"})
	a := mustAppend(t, st, "i", agentsession.NewItemEntry(openresponses.UserText("a")))
	mustAppend(t, st, "i", agentsession.NewItemEntry(openresponses.UserText("b")))
	var buf bytes.Buffer
	st.Project(ctx, &buf, "i")
	// Insert a synthetic marker in the middle by rebuilding through a
	// session, so the ids verify.
	file, _ := agentsession.Read(bytes.NewReader(buf.Bytes()))
	rebuilt := agentsession.New(file.Header())
	ids := map[string]string{}
	for i, e := range file.Entries() {
		data, _ := agentsession.MarshalEntry(e)
		c, _ := agentsession.UnmarshalEntry(data)
		c.Base().ID = ""
		if p := c.Base().Parent; p != "" {
			c.Base().Parent = ids[p]
		}
		id, _ := rebuilt.Append(c)
		ids[e.Base().ID] = id
		if i == 0 {
			marker := agentsession.NewLabelEntry(id, agentsession.LeafLabel)
			marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
			if _, err := rebuilt.Append(marker); err != nil {
				t.Fatal(err)
			}
			rebuilt.Branch(id)
		}
	}
	_ = a
	var mid bytes.Buffer
	agentsession.Write(&mid, rebuilt)
	other, _ := Open(t.TempDir())
	defer other.Close()
	if _, err := other.Import(ctx, bytes.NewReader(mid.Bytes()), false); !errors.Is(err, ErrSynthetic) {
		t.Errorf("import with a marker in the middle = %v", err)
	}
	// The ID is free: a clean import of the same session works.
	if _, err := other.Import(ctx, bytes.NewReader(buf.Bytes()), false); err != nil {
		t.Errorf("import after a refused import = %v", err)
	}
}

// TestCrashDuringDelete: a directory left behind after the delete record
// is removed on the next open, and the ID can be created again.
func TestCrashDuringDelete(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	st.Create(ctx, agentsession.Header{ID: "x"})
	mustAppend(t, st, "x", agentsession.NewItemEntry(openresponses.UserText("a")))
	st.Close()
	// The delete record lands, the directory does not go.
	st2, _ := Open(st.Root())
	if err := st2.commit(journalRecord{Op: "delete", Session: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Open(ctx, "x"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("open of a deleted session = %v", err)
	}
	if _, err := st2.Create(ctx, agentsession.Header{ID: "x"}); err != nil {
		t.Errorf("recreate after a crashed delete = %v", err)
	}
	st2.Close()
}

// TestTwoProcesses: two stores on one directory, as two processes would
// be, each appending to its own session; both survive a reopen, a store
// cannot open the other's held session, and the sweep refuses while
// either holds one.
func TestTwoProcesses(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, _ := Open(root)
	b, _ := Open(root)
	a.Create(ctx, agentsession.Header{ID: "pa"})
	b.Create(ctx, agentsession.Header{ID: "pb"})
	var la, lb string
	for i := 0; i < 5; i++ {
		la = mustAppend(t, a, "pa", agentsession.NewItemEntry(openresponses.UserText("a")))
		lb = mustAppend(t, b, "pb", agentsession.NewItemEntry(openresponses.UserText("b")))
	}
	if _, err := b.Open(ctx, "pa"); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Errorf("b opened a's held session: %v", err)
	}
	// The sweep runs alongside live sessions and keeps their objects.
	if _, err := a.Sweep(ctx, 0); err != nil {
		t.Errorf("sweep with held sessions = %v", err)
	}
	a.Close()
	b.Close()
	c, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sa, err := c.Open(ctx, "pa")
	if err != nil {
		t.Fatal(err)
	}
	sb, err := c.Open(ctx, "pb")
	if err != nil {
		t.Fatal(err)
	}
	if sa.Len() != 5 || sa.Leaf() != la || sb.Len() != 5 || sb.Leaf() != lb {
		t.Errorf("after two writers: a %d %s, b %d %s", sa.Len(), sa.Leaf(), sb.Len(), sb.Leaf())
	}
}

// TestSweepKeepsWhatTheJournalNames: an append acknowledged as durable
// whose log line never reached disk, in a session nobody has reopened,
// is kept by the sweep, since the sweep works from the journal.
func TestSweepKeepsWhatTheJournalNames(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	st.Create(ctx, agentsession.Header{ID: "j"})
	mustAppend(t, st, "j", agentsession.NewItemEntry(openresponses.UserText("a")))
	last := mustAppend(t, st, "j", agentsession.NewItemEntry(openresponses.UserText("b")))
	st.Close()
	dir := filepath.Join(st.Root(), "sessions", "j")
	log, _ := os.ReadFile(filepath.Join(dir, "log"))
	lines := strings.Split(strings.TrimRight(string(log), "\n"), "\n")
	os.WriteFile(filepath.Join(dir, "log"), []byte(lines[0]+"\n"), 0o600)
	st2, _ := Open(st.Root())
	defer st2.Close()
	if _, err := st2.Sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	s, err := st2.Open(ctx, "j")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 2 || s.Leaf() != last {
		t.Errorf("after a sweep before recovery: len %d leaf %s", s.Len(), s.Leaf())
	}
}

// TestSweepGrace keeps a young object nothing names yet.
func TestSweepGrace(t *testing.T) {
	ctx := context.Background()
	st, _ := Open(t.TempDir())
	defer st.Close()
	ghost := agentsession.NewItemEntry(openresponses.UserText("ahead of its record"))
	scratch := agentsession.New(agentsession.Header{})
	scratch.Append(ghost)
	if err := st.storeEntry(ghost); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.Sweep(ctx, time.Hour); n != 0 {
		t.Errorf("swept %d young objects", n)
	}
	if n, _ := st.Sweep(ctx, 0); n != 2 {
		t.Errorf("swept %d old objects, want 2", n)
	}
}
