package cas

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

func twoStores(t *testing.T) (*Store, *Store) {
	t.Helper()
	a, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close(); b.Close() })
	return a, b
}

func item(text string) agentsession.Entry {
	return agentsession.NewItemEntry(openresponses.UserText(text))
}

// TestPushThenFetch pushes a session to a store that lacks it, which
// holds it as a mirror and refuses local writes; the record goes on,
// and the mirror follows by fetch.
func TestPushThenFetch(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	ids := fill(t, rec, "s", 2)
	x, err := rec.Push(ctx, mir, "s", PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !x.Created || x.Admitted != 2 || !x.HeadMoved || x.Head != ids[1] {
		t.Errorf("push: %+v", x)
	}
	if m, _ := mir.Mark(ctx, "s"); m != MarkMirror {
		t.Errorf("the receiver's mark is %s", m)
	}
	if _, err := mir.Append(ctx, "s", item("local")); !errors.Is(err, ErrMirror) {
		t.Errorf("a local append to a mirror: %v", err)
	}
	third := mustAppend(t, rec, "s", item("more"))
	x, err = mir.Fetch(ctx, rec, "s")
	if err != nil {
		t.Fatal(err)
	}
	if x.Created || x.Admitted != 1 || !x.HeadMoved || x.Head != third {
		t.Errorf("fetch: %+v", x)
	}
	s, _ := mir.Open(ctx, "s")
	if s.Len() != 3 || s.Leaf() != third {
		t.Errorf("mirror after fetch: len %d leaf %s", s.Len(), s.Leaf())
	}
	// Nothing new: nothing admitted, nothing moved.
	if x, err := mir.Fetch(ctx, rec, "s"); err != nil || x.Admitted != 0 || x.HeadMoved {
		t.Errorf("an idle fetch: %+v, %v", x, err)
	}
	// The mirror projects the same file as the record.
	rec.Release("s")
	mir.Release("s")
	if a, b := projectFile(t, rec.Root(), "s"), projectFile(t, mir.Root(), "s"); string(a) != string(b) {
		t.Error("the mirror's projection differs from the record's")
	}
}

// TestFetchFromStaleMirror moves nothing: the fetched head does not
// descend from the fetcher's.
func TestFetchFromStaleMirror(t *testing.T) {
	ctx := context.Background()
	rec, stale := twoStores(t)
	fill(t, rec, "s", 1)
	rec.Push(ctx, stale, "s", PushOptions{})
	fresh, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	mustAppend(t, rec, "s", item("later"))
	fresh.Fetch(ctx, rec, "s")
	x, err := fresh.Fetch(ctx, stale, "s")
	if err != nil {
		t.Fatal(err)
	}
	if x.HeadMoved || x.Why == "" {
		t.Errorf("a fetch from a stale mirror: %+v", x)
	}
	// A record never takes a head by fetch.
	x, err = rec.Fetch(ctx, fresh, "s")
	if err != nil || x.HeadMoved {
		t.Errorf("a record fetching: %+v, %v", x, err)
	}
}

// TestPushCompareAndSwap lands the entries whatever the head, moves it
// only when the expected head is right or the push is forced, and says
// which.
func TestPushCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	ids := fill(t, rec, "s", 1)
	rec.Push(ctx, mir, "s", PushOptions{})
	next := mustAppend(t, rec, "s", item("two"))
	x, err := rec.Push(ctx, mir, "s", PushOptions{Expected: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000"})
	if err != nil {
		t.Fatal(err)
	}
	if x.Admitted != 1 || x.HeadMoved || x.Head != ids[0] || x.Why == "" {
		t.Errorf("a wrong expected head: %+v", x)
	}
	x, err = rec.Push(ctx, mir, "s", PushOptions{Expected: "sha256:" + "0000000000000000000000000000000000000000000000000000000000000000", Force: true})
	if err != nil || !x.HeadMoved || !x.Forced || x.Head != next {
		t.Errorf("a forced push: %+v, %v", x, err)
	}
	last := mustAppend(t, rec, "s", item("three"))
	x, err = rec.Push(ctx, mir, "s", PushOptions{Expected: next})
	if err != nil || !x.HeadMoved || x.Forced || x.Head != last {
		t.Errorf("a fast-forward: %+v, %v", x, err)
	}
}

// TestHandover makes the receiver the record and the sender a mirror.
func TestHandover(t *testing.T) {
	ctx := context.Background()
	a, b := twoStores(t)
	ids := fill(t, a, "s", 1)
	x, err := a.Push(ctx, b, "s", PushOptions{Handover: true})
	if err != nil || !x.Handover {
		t.Fatalf("%+v, %v", x, err)
	}
	if m, _ := b.Mark(ctx, "s"); m != MarkRecord {
		t.Errorf("receiver mark %s", m)
	}
	if m, _ := a.Mark(ctx, "s"); m != MarkMirror {
		t.Errorf("sender mark %s", m)
	}
	if _, err := a.Append(ctx, "s", item("no")); !errors.Is(err, ErrMirror) {
		t.Errorf("the old record appended: %v", err)
	}
	mustAppend(t, b, "s", item("the new record's"))
	if _, err := a.Push(ctx, b, "s", PushOptions{Expected: ids[0]}); !errors.Is(err, ErrNotRecord) {
		t.Errorf("a push from a mirror: %v", err)
	}
	// Handover to a store that already holds the session as a mirror,
	// and back: the marks survive a reopen.
	if _, err := b.Push(ctx, a, "s", PushOptions{Handover: true, Expected: ids[0]}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	a2, _ := Open(a.Root())
	defer a2.Close()
	if m, _ := a2.Mark(ctx, "s"); m != MarkRecord {
		t.Errorf("after reopening, the mark is %s", m)
	}
}

// TestExchangeRefusals: a different session under the same ID, and an
// own entry that does not hang from the union.
func TestExchangeRefusals(t *testing.T) {
	ctx := context.Background()
	a, b := twoStores(t)
	fill(t, a, "s", 1)
	fill(t, b, "s", 1) // another session under the same ID
	if _, err := a.Push(ctx, b, "s", PushOptions{}); !errors.Is(err, ErrHeaderDiffers) {
		t.Errorf("a push onto a different session: %v", err)
	}
	if _, err := b.Fetch(ctx, a, "s"); !errors.Is(err, ErrHeaderDiffers) {
		t.Errorf("a fetch of a different session: %v", err)
	}
	ro, _ := Open(b.Root(), WithReadOnly())
	defer ro.Close()
	if _, err := ro.Fetch(ctx, a, "s"); !errors.Is(err, agentsession.ErrReadOnly) {
		t.Errorf("a read-only receiver: %v", err)
	}
}

// TestPushFork carries a fork's prefix to a store that holds no part of
// its origin, and the log merges as a set when both stores wrote.
func TestPushFork(t *testing.T) {
	ctx := context.Background()
	a, b := twoStores(t)
	ids := fill(t, a, "o", 3)
	if _, err := a.Create(ctx, agentsession.Header{ID: "f", Base: ids[1]}); err != nil {
		t.Fatal(err)
	}
	own := mustAppend(t, a, "f", item("fork"))
	x, err := a.Push(ctx, b, "f", PushOptions{Expected: ids[1]})
	if err != nil {
		t.Fatal(err)
	}
	if !x.Created || x.Admitted != 1 || x.Head != own {
		t.Errorf("fork push: %+v", x)
	}
	f, err := b.Open(ctx, "f")
	if err != nil {
		t.Fatal(err)
	}
	if f.Len() != 3 || f.Header().Base != ids[1] {
		t.Errorf("fork at the receiver: len %d base %s", f.Len(), f.Header().Base)
	}
	// Both believe they are the record (a declared mirror): each appends,
	// and a push lands the sender's branch beside the receiver's.
	if err := b.DeclareRecord(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	theirs := mustAppend(t, b, "f", item("receiver's"))
	ours := mustAppend(t, a, "f", item("sender's"))
	x, err = a.Push(ctx, b, "f", PushOptions{Expected: own})
	if err != nil {
		t.Fatal(err)
	}
	if x.HeadMoved || x.Head != theirs || x.Admitted != 1 {
		t.Errorf("two records: %+v", x)
	}
	f, _ = b.Open(ctx, "f")
	if _, ok := f.Entry(ours); !ok || f.Len() != 5 {
		t.Errorf("the union: len %d, sender's entry held %v", f.Len(), ok)
	}
}

// TestPushCarriesBlobs sends the media a sidecar session's entries name.
func TestPushCarriesBlobs(t *testing.T) {
	ctx := context.Background()
	a, b := twoStores(t)
	blob, err := a.PutBlob(ctx, []byte("image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	a.Create(ctx, agentsession.Header{ID: "m", Media: agentsession.MediaSidecar})
	mustAppend(t, a, "m", item("see sidecar:"+blob))
	if _, err := a.Push(ctx, b, "m", PushOptions{}); err != nil {
		t.Fatal(err)
	}
	if data, err := b.Blob(ctx, blob); err != nil || string(data) != "image bytes" {
		t.Errorf("blob at the receiver: %q, %v", data, err)
	}
}

// TestLaterFormatRefused: a fork of an origin, and an exchange of a
// session, whose header names a minor this package does not read.
func TestLaterFormatRefused(t *testing.T) {
	ctx := context.Background()
	a, b := twoStores(t)
	ids := fill(t, a, "s", 1)
	bun, err := a.bundleOf(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	bun.header.Format = "agentsession/0.99"
	if _, err := b.receive(ctx, bun, receiveOptions{}); !errors.Is(err, agentsession.ErrUnsupportedFormat) {
		t.Errorf("a later format received: %v", err)
	}
	a.Release("s")
	h, _ := readHeader(a.Root() + "/sessions/s")
	h.Format = "agentsession/0.99"
	if err := writeHeader(a.Root()+"/sessions/s", h); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Create(ctx, agentsession.Header{ID: "f", Base: ids[0]}); !errors.Is(err, agentsession.ErrUnsupportedFormat) {
		t.Errorf("a fork of a later-format origin: %v", err)
	}
}
