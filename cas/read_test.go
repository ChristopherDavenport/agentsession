package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestReadHeldLazy reads a session this store holds with lazy appends
// no commit covers yet: their records are in the log and their objects
// written, so Read sees them, and the store's hold and handle are as
// they were.
func TestReadHeldLazy(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"a", "b"} {
		r, err := st.Write(ctx, "s", agentsession.NewItemEntry(openresponses.UserText(text)))
		if err != nil {
			t.Fatal(err)
		}
		if r.Durable {
			t.Fatal("an append under SyncNever was durable")
		}
	}
	h := st.handles()["s"]
	got, err := st.Read(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got == s || got.Len() != 2 || got.Leaf() != s.Leaf() {
		t.Errorf("Read: %d entries at %s, want a copy of 2 at %s", got.Len(), got.Leaf(), s.Leaf())
	}
	if st.handles()["s"] != h {
		t.Error("Read replaced the store's handle on the session")
	}
	other, err := Open(st.Root())
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err := other.Open(ctx, "s"); !errors.Is(err, ErrSessionLocked) {
		t.Errorf("another store's Open after Read: %v, want ErrSessionLocked", err)
	}
}

// TestReadWritesNothing reads a session nobody holds whose log a crash
// left with a torn tail. Read cuts it in memory, as a read-only store
// does, and leaves the session's files as they were, keeps no handle,
// and takes no lock: a writing open after it recovers the session.
func TestReadWritesNothing(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", agentsession.NewItemEntry(openresponses.UserText("a")))
	mustAppend(t, st, "s", agentsession.NewItemEntry(openresponses.UserText("b")))
	if err := st.Release("s"); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "sessions", "s")
	f, err := os.OpenFile(filepath.Join(dir, logName), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"op":"app`) // a torn tail
	f.Close()
	before := snapshotDir(t, dir)

	got, err := st.Read(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() != 2 {
		t.Errorf("Read past a torn tail: %d entries, want 2", got.Len())
	}
	if after := snapshotDir(t, dir); !equalDirs(before, after) {
		t.Error("Read changed the session's files")
	}
	if _, ok := st.handles()["s"]; ok {
		t.Error("Read left the session open in the store")
	}
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if s, err := other.Open(ctx, "s"); err != nil || s.Len() != 2 {
		t.Fatalf("another store's Open after Read: %v", err)
	}
}

// TestReadDeletedPartWay deletes a session and sweeps its objects after
// a Read, or a read-only Open, has read its log and before it loads
// them: neither holds a lock a delete waits on, so the session is
// reported missing rather than as an object that is not there.
func TestReadDeletedPartWay(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	defer func() { loading = nil }()
	for _, tt := range []struct {
		name string
		read func(id string) error
	}{
		{"Read", func(id string) error { _, err := st.Read(ctx, id); return err }},
		{"read-only Open", func(id string) error { _, err := ro.Open(ctx, id); return err }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
				t.Fatal(err)
			}
			mustAppend(t, st, "s", agentsession.NewItemEntry(openresponses.UserText("a "+tt.name)))
			if err := st.Release("s"); err != nil {
				t.Fatal(err)
			}
			swept := 0
			loading = func() {
				loading = nil
				if err := st.Delete(ctx, "s"); err != nil {
					t.Error(err)
				}
				n, err := st.Sweep(ctx, 0)
				if err != nil {
					t.Error(err)
				}
				swept = n
			}
			if err := tt.read("s"); !errors.Is(err, agentsession.ErrNoSession) {
				t.Errorf("%s of a session deleted part way: %v, want ErrNoSession", tt.name, err)
			}
			if swept == 0 {
				t.Error("the sweep removed nothing, so the read was not tested")
			}
		})
	}
}

// TestReadBesideWriter reads a session again and again while this store
// appends to it, lazily, and packs and sweeps the store: each Read is
// whole, and holds no fewer entries than the one before.
func TestReadBesideWriter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	const n = 60
	done := make(chan error, 1)
	go func() {
		for i := 0; i < n; i++ {
			if _, err := st.Append(ctx, "s", agentsession.NewItemEntry(openresponses.UserText(fmt.Sprint("entry ", i)))); err != nil {
				done <- err
				return
			}
			switch i % 20 {
			case 9:
				if _, err := st.Pack(ctx); err != nil {
					done <- err
					return
				}
			case 19:
				if _, err := st.Sweep(ctx, 0); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	last := 0
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			got, err := st.Read(ctx, "s")
			if err != nil || got.Len() != n {
				t.Fatalf("Read after the writer: %v", err)
			}
			return
		default:
		}
		got, err := st.Read(ctx, "s")
		if err != nil {
			t.Fatalf("Read beside the writer: %v", err)
		}
		if got.Len() < last {
			t.Fatalf("Read holds %d entries after one held %d", got.Len(), last)
		}
		last = got.Len()
	}
}

type fileState struct {
	data []byte
	mod  int64
}

func snapshotDir(t *testing.T, dir string) map[string]fileState {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fileState{}
	for _, e := range ents {
		p := filepath.Join(dir, e.Name())
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(p)
		out[e.Name()] = fileState{data: data, mod: info.ModTime().UnixNano()}
	}
	return out
}

func equalDirs(a, b map[string]fileState) bool {
	if len(a) != len(b) {
		return false
	}
	for name, fa := range a {
		fb, ok := b[name]
		if !ok || fa.mod != fb.mod || !bytes.Equal(fa.data, fb.data) {
			return false
		}
	}
	return true
}
