package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/openresponses"
)

// syncBuffer is a buffer a test reads while the command writes it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// waitFor fails the test when out does not come to hold want.
func waitFor(t *testing.T, out *syncBuffer, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), want) {
		if time.Now().After(deadline) {
			t.Fatalf("output never held %q:\n%s", want, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// startShow runs show -f in the background under a context the test
// ends, and returns its output, its stderr and a function that stops
// it and returns its exit status.
func startShow(t *testing.T, args ...string) (*syncBuffer, *syncBuffer, func() int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	old := followContext
	followContext = func() (context.Context, context.CancelFunc) { return ctx, cancel }
	t.Cleanup(func() { followContext = old })
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(append([]string{"show", "-f"}, args...), &stdout, &stderr) }()
	return &stdout, &stderr, func() int {
		cancel()
		select {
		case code := <-done:
			return code
		case <-time.After(10 * time.Second):
			t.Fatal("show -f did not stop when interrupted")
			return -1
		}
	}
}

func say(text string) agentsession.Entry {
	return agentsession.NewItemEntry(openresponses.UserText(text))
}

// TestShowFollowFile: show -f prints the session, then each entry the
// writer appends, and ends cleanly when interrupted.
func TestShowFollowFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Create(ctx, agentsession.Header{ID: "live"}); err != nil {
		t.Fatal(err)
	}
	first, err := w.Append(ctx, "live", say("hello there"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := w.Path("live")
	if err != nil {
		t.Fatal(err)
	}
	out, errOut, stop := startShow(t, path)
	waitFor(t, out, "hello there")
	waitFor(t, errOut, "following live")
	if !strings.Contains(out.String(), "session  live") || !strings.Contains(out.String(), shortID(first)) {
		t.Errorf("the session was not printed first:\n%s", out)
	}
	second, err := w.Append(ctx, "live", say("and then this"))
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "and then this")
	if !strings.Contains(out.String(), shortID(second)+"  "+shortID(first)) {
		t.Errorf("the appended entry is not a row naming its parent:\n%s", out)
	}
	// A leaf label is an entry, and the head it records follows it.
	sess, _ := w.Open(ctx, "live")
	if err := sess.Branch(first); err != nil {
		t.Fatal(err)
	}
	mark, _ := sess.MarkLeaf()
	if _, err := w.Append(ctx, "live", mark); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "head -> "+shortID(first))
	if code := stop(); code != 0 {
		t.Errorf("exit status %d after an interrupt, want 0", code)
	}
}

// TestShowFollowCas: the same through a cas store, which a read-only
// open follows beside the writer holding the session.
func TestShowFollowCas(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Create(ctx, agentsession.Header{ID: "live"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, "live", say("hello there")); err != nil {
		t.Fatal(err)
	}
	out, _, stop := startShow(t, root, "live")
	waitFor(t, out, "hello there")
	if _, err := w.Append(ctx, "live", say("and then this")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "and then this")
	if code := stop(); code != 0 {
		t.Errorf("exit status %d after an interrupt, want 0", code)
	}
}

// TestShowFollowDeleted: a session deleted while it is followed ends the
// command with an error that says so.
func TestShowFollowDeleted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	w.Create(ctx, agentsession.Header{ID: "live"})
	w.Append(ctx, "live", say("hello"))
	out, errOut, stop := startShow(t, root, "live")
	waitFor(t, out, "hello")
	if err := w.Delete(ctx, "live"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, errOut, "session live was deleted")
	if code := stop(); code != 1 {
		t.Errorf("exit status %d after a delete, want 1", code)
	}
}

// TestShowFollowRefusals: a file outside a store's layout cannot be
// followed, and -leaf has no meaning for a session that is growing.
func TestShowFollowRefusals(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"show", "-f", "../../testdata/sessions/basic.jsonl"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "cannot be followed as a file") {
		t.Errorf("show -f of a loose file: status %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"show", "-f", "-leaf", "x", "../../testdata/sessions/basic.jsonl"}, &stdout, &stderr); code != 2 || !strings.Contains(stderr.String(), "-leaf") {
		t.Errorf("show -f -leaf: status %d, stderr %q", code, stderr.String())
	}
}
