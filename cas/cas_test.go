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
	swept, err := st.Sweep(ctx)
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
