package cas

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// injected is the panic the tests below raise inside the store.
type injected struct{}

// mustPanic runs f and checks that the injected panic reached it as
// raised: the store lets go of what it took and panics on, swallowing
// nothing.
func mustPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != (injected{}) {
			t.Errorf("%s: recovered %v, want the injected panic", what, r)
		}
	}()
	f()
}

// within runs f and fails the test if it has not returned in time, as
// a call waiting on a handle a panic left claimed never does.
func within(t *testing.T, what string, f func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s after a panic: %v", what, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("%s after a panic did not return: the panic left the session claimed", what)
	}
}

// panicSyncUnder makes the store's fsyncs of files, objects and
// directories under root panic until the returned func restores them.
func panicSyncUnder(root string) func() {
	oldFile, oldObject, oldDir := syncFile, syncObject, syncDirFile
	under := root + string(os.PathSeparator)
	hook := func(old func(*os.File) error) func(*os.File) error {
		return func(f *os.File) error {
			if strings.HasPrefix(f.Name(), under) {
				panic(injected{})
			}
			return old(f)
		}
	}
	syncFile, syncObject, syncDirFile = hook(oldFile), hook(oldObject), hook(oldDir)
	return func() { syncFile, syncObject, syncDirFile = oldFile, oldObject, oldDir }
}

// TestPanicWhileClaimedFreesSession panics inside the store while it
// has a session claimed, at each place it claims one: opening a session
// it does not hold, rebuilding one it holds after an exchange, creating,
// importing and receiving one by push. A caller that recovers, as
// net/http does a handler's panic, finds the session free: the next
// call for it and Close return, rather than wait forever on a handle
// left locked, and the session's lock is not kept from the next open.
func TestPanicWhileClaimedFreesSession(t *testing.T) {
	ctx := context.Background()
	t.Run("Open", func(t *testing.T) {
		st, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fill(t, st, "s", 1)
		if err := st.Release("s"); err != nil {
			t.Fatal(err)
		}
		loading = func() { panic(injected{}) }
		mustPanic(t, "Open", func() { st.Open(ctx, "s") })
		loading = nil
		within(t, "Open", func() error { _, err := st.Open(ctx, "s"); return err })
		within(t, "Append", func() error { _, err := st.Append(ctx, "s", item("after")); return err })
		within(t, "Close", st.Close)
	})
	t.Run("reopen", func(t *testing.T) {
		rec, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		mir, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer rec.Close()
		fill(t, rec, "s", 1)
		if _, err := rec.Push(ctx, mir, "s", PushOptions{}); err != nil {
			t.Fatal(err)
		}
		mustAppend(t, rec, "s", item("more"))
		if _, err := mir.Open(ctx, "s"); err != nil {
			t.Fatal(err)
		}
		loading = func() { panic(injected{}) }
		mustPanic(t, "Fetch", func() { mir.Fetch(ctx, rec, "s") })
		loading = nil
		within(t, "Open", func() error {
			sess, err := mir.Open(ctx, "s")
			if err == nil && sess.Len() != 2 {
				t.Errorf("the mirror holds %d entries after the fetch, want 2", sess.Len())
			}
			return err
		})
		within(t, "Close", mir.Close)
	})
	t.Run("Create", func(t *testing.T) {
		root := t.TempDir()
		st, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		restore := panicSyncUnder(root)
		mustPanic(t, "Create", func() { st.Create(ctx, agentsession.Header{ID: "c"}) })
		restore()
		within(t, "Create", func() error { _, err := st.Create(ctx, agentsession.Header{ID: "c"}); return err })
		within(t, "Close", st.Close)
	})
	t.Run("Import", func(t *testing.T) {
		src, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer src.Close()
		fill(t, src, "i", 2)
		var file bytes.Buffer
		if err := src.Project(ctx, &file, "i"); err != nil {
			t.Fatal(err)
		}
		root := t.TempDir()
		st, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		restore := panicSyncUnder(root)
		mustPanic(t, "Import", func() { st.Import(ctx, bytes.NewReader(file.Bytes()), false) })
		restore()
		within(t, "Import", func() error { _, err := st.Import(ctx, bytes.NewReader(file.Bytes()), false); return err })
		within(t, "Close", st.Close)
	})
	t.Run("Push", func(t *testing.T) {
		rec, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer rec.Close()
		fill(t, rec, "p", 2)
		root := t.TempDir()
		mir, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		restore := panicSyncUnder(root)
		mustPanic(t, "Push", func() { rec.Push(ctx, mir, "p", PushOptions{}) })
		restore()
		within(t, "Push", func() error { _, err := rec.Push(ctx, mir, "p", PushOptions{}); return err })
		within(t, "Close", mir.Close)
	})
	t.Run("Import openHeld", func(t *testing.T) {
		src, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fill(t, src, "i", 2)
		var file bytes.Buffer
		if err := src.Project(ctx, &file, "i"); err != nil {
			t.Fatal(err)
		}
		src.Close()
		st, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		loading = func() { panic(injected{}) }
		mustPanic(t, "Import", func() { st.Import(ctx, bytes.NewReader(file.Bytes()), false) })
		loading = nil
		within(t, "Open", func() error { _, err := st.Open(ctx, "i"); return err })
		within(t, "Close", st.Close)
	})
	t.Run("Push openHeld", func(t *testing.T) {
		rec, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		defer rec.Close()
		fill(t, rec, "p", 2)
		mir, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		loading = func() { panic(injected{}) }
		mustPanic(t, "Push", func() { rec.Push(ctx, mir, "p", PushOptions{}) })
		loading = nil
		within(t, "Open", func() error { _, err := mir.Open(ctx, "p"); return err })
		within(t, "Close", mir.Close)
	})
}

// TestPanicWhileHeldFreesSession panics inside the store while it holds
// a session's handle to write to it or commit it: an append, Sync, a
// durable ReadLog and Close. The handle is dropped, so the next call
// for the session opens it afresh from what the disk holds, and returns,
// as Close does, rather than wait forever on a handle left locked.
func TestPanicWhileHeldFreesSession(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		policy SyncPolicy
		call   func(st *Store)
	}{
		{"Append", SyncEveryAppend, func(st *Store) { st.Append(ctx, "s", item("panics")) }},
		{"Sync", SyncNever, func(st *Store) { st.Sync(ctx) }},
		{"ReadLog", SyncNever, func(st *Store) { st.ReadLog(ctx, "s", "", true) }},
		{"Close", SyncNever, func(st *Store) { st.Close() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			st, err := Open(root, WithSync(tt.policy))
			if err != nil {
				t.Fatal(err)
			}
			fill(t, st, "s", 2)
			restore := panicSyncUnder(root)
			mustPanic(t, tt.name, func() { tt.call(st) })
			restore()
			within(t, "Append", func() error { _, err := st.Append(ctx, "s", item("after")); return err })
			within(t, "Open", func() error { _, err := st.Open(ctx, "s"); return err })
			within(t, "Close", st.Close)
		})
	}
}
