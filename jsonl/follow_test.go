package jsonl_test

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/followtest"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/openresponses"
)

func userEntry(text string) agentsession.Entry {
	return agentsession.NewItemEntry(openresponses.UserText(text))
}

// followed makes a session of two entries in a store over a new root.
func followed(t *testing.T, opts ...jsonl.Option) (*jsonl.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := jsonl.Open(root, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"a", "b"} {
		if _, err := st.Append(ctx, "s", userEntry(text)); err != nil {
			t.Fatal(err)
		}
	}
	path, err := st.Path("s")
	if err != nil {
		t.Fatal(err)
	}
	return st, root, path
}

func readOnly(t *testing.T, root string) *jsonl.Store {
	t.Helper()
	st, err := jsonl.Open(root, jsonl.WithReadOnly(), jsonl.WithFollowInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestFollowPartialLine: a line whose newline has not landed is not
// consumed, and is once it does.
func TestFollowPartialLine(t *testing.T) {
	_, root, path := followed(t)
	ro := readOnly(t, root)
	w := followtest.Start(t, ro, "s", "")
	snap := w.NextKind(agentsession.Snapshot)

	// The next entry, built on a copy of the session as it stands.
	sess, err := ro.Read(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	e := userEntry("torn")
	id, err := sess.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	line, err := agentsession.MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	half := len(line) / 2
	if _, err := f.Write(line[:half]); err != nil {
		t.Fatal(err)
	}
	w.Quiet() // half a line: wait, no error
	if _, err := f.Write(line[half:]); err != nil {
		t.Fatal(err)
	}
	w.Quiet() // all of it, no newline yet
	if _, err := f.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	c := w.NextKind(agentsession.Appended)
	if c.ID != id || c.Session.Len() != snap.Session.Len() {
		t.Errorf("appended %s into %d entries, want %s", c.ID, c.Session.Len(), id)
	}
}

// raisedFile makes a session whose file names 0.10, as an older writer
// left it.
func raisedFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	current := `"format":"` + agentsession.Format + `"`
	if !strings.Contains(string(data), current) {
		t.Fatal("the file does not name the current format")
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), current, `"format":"agentsession/0.10"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestFollowResetOnRaiseFormat: the first append of this writer to a
// 0.10 file rewrites the file and renames it over, which is a reset to a
// follower in another store and in the writer's own.
func TestFollowResetOnRaiseFormat(t *testing.T) {
	for _, name := range []string{"another store", "the writer's store"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st, root, path := followed(t)
			st.Close()
			raisedFile(t, path)
			writer, err := jsonl.Open(root, jsonl.WithFollowInterval(5*time.Millisecond))
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			var f agentsession.Follower = readOnly(t, root)
			if name == "the writer's store" {
				f = writer
			}
			w := followtest.Start(t, f, "s", "")
			snap := w.NextKind(agentsession.Snapshot)
			if snap.Session.DeclaredFormat() != "agentsession/0.10" {
				t.Fatalf("snapshot declares %s", snap.Session.DeclaredFormat())
			}
			if _, err := writer.Open(ctx, "s"); err != nil {
				t.Fatal(err)
			}
			id, err := writer.Append(ctx, "s", userEntry("c"))
			if err != nil {
				t.Fatal(err)
			}
			r := w.NextKind(agentsession.Reset)
			if r.Session.Len() != 3 || r.Session.DeclaredFormat() != agentsession.Format {
				t.Errorf("reset holds %d entries declaring %s, want 3 declaring %s", r.Session.Len(), r.Session.DeclaredFormat(), agentsession.Format)
			}
			if _, ok := r.Session.Entry(id); !ok {
				t.Errorf("the reset lacks the entry that caused it")
			}
			w.Quiet()
			id2, err := writer.Append(ctx, "s", userEntry("d"))
			if err != nil {
				t.Fatal(err)
			}
			if c := w.NextKind(agentsession.Appended); c.ID != id2 || c.Session.Len() != 4 {
				t.Errorf("appended %s into %d entries after the reset, want %s into 4", c.ID, c.Session.Len(), id2)
			}
		})
	}
}

// TestFollowResetOnShrink: a file cut back is a reset, and an inode that
// changes under the same bytes is another.
func TestFollowResetOnShrink(t *testing.T) {
	_, root, path := followed(t)
	ro := readOnly(t, root)
	w := followtest.Start(t, ro, "s", "")
	w.NextKind(agentsession.Snapshot)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cut := bytes.LastIndexByte(data[:len(data)-1], '\n') + 1
	if err := os.Truncate(path, int64(cut)); err != nil {
		t.Fatal(err)
	}
	r := w.NextKind(agentsession.Reset)
	if r.Session.Len() != 1 {
		t.Errorf("reset after a shrink holds %d entries, want 1", r.Session.Len())
	}
	w.Quiet()
}

func TestFollowResetOnNewInode(t *testing.T) {
	_, root, path := followed(t)
	ro := readOnly(t, root)
	w := followtest.Start(t, ro, "s", "")
	w.NextKind(agentsession.Snapshot)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".new", data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
	r := w.NextKind(agentsession.Reset)
	if r.Session.Len() != 2 {
		t.Errorf("reset after a new inode holds %d entries, want 2", r.Session.Len())
	}
	w.Quiet()
}

// TestFollowResumeAfterRaise: a cursor from before the rewrite is not a
// place in the file any more, and the answer is a reset.
func TestFollowResumeAfterRaise(t *testing.T) {
	ctx := context.Background()
	st, root, path := followed(t)
	w := followtest.Start(t, st, "s", "")
	snap := w.NextKind(agentsession.Snapshot)
	w.Stop()
	st.Close()
	raisedFile(t, path)
	writer, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Append(ctx, "s", userEntry("c")); err != nil {
		t.Fatal(err)
	}
	r := followtest.Start(t, readOnly(t, root), "s", snap.Cursor)
	if c := r.NextKind(agentsession.Reset); c.Session.Len() != 3 {
		t.Errorf("reset holds %d entries, want 3", c.Session.Len())
	}
}

// TestFollowWokenByAppend: a follower of a session its own store writes
// does not wait out the poll interval.
func TestFollowWokenByAppend(t *testing.T) {
	st, _, _ := followed(t, jsonl.WithFollowInterval(time.Hour))
	w := followtest.Start(t, st, "s", "")
	w.NextKind(agentsession.Snapshot)
	start := time.Now()
	if _, err := st.Append(context.Background(), "s", userEntry("c")); err != nil {
		t.Fatal(err)
	}
	w.NextKind(agentsession.Appended)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("the append took %v to reach the follower", d)
	}
}
