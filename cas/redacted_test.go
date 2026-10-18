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
	"github.com/ChristopherDavenport/openresponses"
)

// redactedFile is a redacted session file of n entries, each marked
// redacted from the original's entry in its place.
func redactedFile(t *testing.T, id string, n int) []byte {
	t.Helper()
	s := agentsession.New(agentsession.Header{ID: id, Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "orig"}})
	for i := range n {
		e := agentsession.NewItemEntry(openresponses.UserText(strings.Repeat("x", i+1)))
		if err := agentsession.SetRedacted(e, []agentsession.Span{{From: i + 1, To: i + 1}}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func importFile(t *testing.T, st *Store, file []byte) {
	t.Helper()
	if _, err := st.Import(context.Background(), bytes.NewReader(file), true); err != nil {
		t.Fatal(err)
	}
}

// TestRedactedOnlyIsTheStores: the setting is written into the layout
// file, which a release that reads the layout whole refuses; every later
// open obeys it without the option, a store opened before it was set
// obeys it from then on, and a read-only open of the store reads it.
func TestRedactedOnlyIsTheStores(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	before, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	st, err := Open(root, WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, layoutFile))
	if got := strings.TrimSpace(string(data)); got == layout || !strings.HasPrefix(got, layout+"\n") {
		t.Errorf("the layout file holds %q; a release reading it whole would take it for %q", got, layout)
	}
	if _, err := before.Create(ctx, agentsession.Header{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Create through a store opened before the setting: %v, want ErrNotRedacted", err)
	}
	if _, err := before.Import(ctx, bytes.NewReader([]byte(`{"type":"session","format":"`+agentsession.Format+`","id":"o","created_at":"2026-10-16T00:00:00Z","payload":"`+agentsession.Payload+`"}`+"\n")), true); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Import through a store opened before the setting: %v, want ErrNotRedacted", err)
	}
	importFile(t, st, redactedFile(t, "r", 2))
	st.Close()

	again, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if _, err := again.Create(ctx, agentsession.Header{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Create after a reopen without the option: %v, want ErrNotRedacted", err)
	}
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if only, err := ro.onlyRedacted(); err != nil || !only {
		t.Errorf("a read-only open: %v, %v", only, err)
	}

	// A setting this release does not know is refused, as a layout is.
	os.WriteFile(filepath.Join(root, layoutFile), []byte(layout+"\n"+layoutRedactedOnly+"\nsomething-later\n"), 0o600)
	if _, err := Open(root); !errors.Is(err, ErrLayout) {
		t.Errorf("a layout naming an unknown setting: %v, want ErrLayout", err)
	}
}

// TestRedactedOnlyRefusesAHoldingStore: a store that holds an original
// is not set to hold only redacted sessions, and its layout is left.
func TestRedactedOnlyRefusesAHoldingStore(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	importFile(t, st, redactedFile(t, "r", 1))
	if _, err := st.Create(context.Background(), agentsession.Header{ID: "o"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := Open(root, WithRedactedOnly()); !errors.Is(err, agentsession.ErrNotRedacted) || !strings.Contains(err.Error(), "holds o,") {
		t.Errorf("setting a store holding an original: %v, want ErrNotRedacted naming o", err)
	}
	// One whose redacted session was resumed natively holds an entry
	// that is not redacted.
	resumed := t.TempDir()
	st, err = Open(resumed)
	if err != nil {
		t.Fatal(err)
	}
	importFile(t, st, redactedFile(t, "r", 1))
	native, err := st.Append(context.Background(), "r", agentsession.NewItemEntry(openresponses.UserText("native")))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, err := Open(resumed, WithRedactedOnly()); !errors.Is(err, agentsession.ErrNotRedacted) || !strings.Contains(err.Error(), native) {
		t.Errorf("setting a store holding a native entry: %v, want ErrNotRedacted naming %s", err, native)
	}
	if data, _ := os.ReadFile(filepath.Join(root, layoutFile)); strings.TrimSpace(string(data)) != layout {
		t.Errorf("the refused setting left the layout %q", data)
	}
}

// TestRedactedOnlyPlacesNoOriginal: a store opened before another
// process set the store to hold only redacted sessions, its own flag not
// set, refuses an original where every session is placed, and, in
// admitNew, before any of its objects is written: the setting is read
// again under the sweep's lock.
func TestRedactedOnlyPlacesNoOriginal(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	before, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	second, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	setter, err := Open(root, WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	setter.Close()
	if before.redactedOnly.Load() || second.redactedOnly.Load() {
		t.Fatal("the store opened before the setting already knows it")
	}
	// admitNew first, so the setting it reads is read from the file and
	// not the flag a refusal caches; placeSession in a second store that
	// has not read it either.
	s := agentsession.New(agentsession.Header{ID: "o2"})
	id, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("an original's secret")))
	if err != nil {
		t.Fatal(err)
	}
	dir, _ := before.sessionDir("o2")
	if _, err := before.admitNew(ctx, dir, s.Header(), MarkRecord, s.Entries(), s.Entries(), nil, id); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("admitNew of an original: %v, want ErrNotRedacted", err)
	}
	if held, err := before.HasObject(ctx, SpaceEntries, id); err != nil || held {
		t.Errorf("the refused original's entry is in the store: %v, %v", held, err)
	}
	if before.redactedOnly.Load() != true {
		t.Error("the refusal did not come from the setting")
	}
	if err := second.placeSession(filepath.Join(root, "sessions", "o"), agentsession.Header{ID: "o"}, nil, "", MarkRecord); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("placeSession of an original: %v, want ErrNotRedacted", err)
	}
}

// TestLayoutNeverReplaced: an open that finds no layout file and writes
// one does not write over one another process wrote meanwhile, so the
// redacted-only setting that file holds stands, and the open obeys it.
func TestLayoutNeverReplaced(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	p := filepath.Join(root, layoutFile)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	set := []byte(layout + "\n" + layoutRedactedOnly + "\n")
	layoutWriting = func() { os.WriteFile(p, set, 0o600) }
	defer func() { layoutWriting = nil }()
	racer, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer racer.Close()
	if got, _ := os.ReadFile(p); !bytes.Equal(got, set) {
		t.Errorf("the layout file holds %q after the racing open, want %q", got, set)
	}
	if _, err := racer.Create(context.Background(), agentsession.Header{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Create through the racing open: %v, want ErrNotRedacted", err)
	}
}

// TestFetchRedacted: a fetch takes a redacted session whole, adds
// nothing to one held, and brings no original into a store that holds
// only redacted sessions.
func TestFetchRedacted(t *testing.T) {
	ctx := context.Background()
	open := func(opts ...Option) *Store {
		st, err := Open(t.TempDir(), opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	full, part := open(), open()
	file := redactedFile(t, "r", 3)
	importFile(t, full, file)
	importFile(t, part, file[:bytes.LastIndexByte(file[:len(file)-1], '\n')+1])

	pub := open(WithRedactedOnly())
	if x, err := pub.Fetch(ctx, full, "r"); err != nil || !x.Created || x.Admitted != 3 {
		t.Fatalf("fetch of a redacted session: %+v, %v", x, err)
	}
	if x, err := pub.Fetch(ctx, full, "r"); err != nil || x.Admitted != 0 {
		t.Errorf("a repeat of the fetch: %+v, %v", x, err)
	}
	// A redacted session grows by exchange, as any session does.
	if x, err := part.Fetch(ctx, full, "r"); err != nil || x.Admitted != 1 {
		t.Errorf("a fetch adding to a held redacted session: %+v, %v", x, err)
	}
	// Resumed natively, it brings the redacted-only store an entry that
	// is not redacted: refused, and nothing of it lands, its object
	// included.
	native, err := full.Append(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("native")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Fetch(ctx, full, "r"); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a fetch of a native entry into a redacted-only store: %v, want ErrNotRedacted", err)
	}
	if _, err := full.Push(ctx, pub, "r", PushOptions{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a push of a native entry into a redacted-only store: %v, want ErrNotRedacted", err)
	}
	if held, err := pub.HasObject(ctx, SpaceEntries, native); err != nil || held {
		t.Errorf("the refused entry's object is in the store: %v, %v", held, err)
	}
	if s, err := pub.Read(ctx, "r"); err != nil || s.Len() != 3 {
		t.Errorf("after the refused fetch: %v", err)
	}
	if _, err := full.Create(ctx, agentsession.Header{ID: "o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := pub.Fetch(ctx, full, "o"); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a fetch of an original into a redacted-only store: %v, want ErrNotRedacted", err)
	}
	if _, err := full.Push(ctx, pub, "o", PushOptions{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a push of an original into a redacted-only store: %v, want ErrNotRedacted", err)
	}
}

// TestExchangeValidatesHeader: a header that arrives by push or fetch
// is held to the rules import holds a file's to, so a redacted_from
// without redacted, or naming the session itself, is refused and lands
// nothing.
func TestExchangeValidatesHeader(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := agentsession.New(agentsession.Header{ID: "x"})
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	for name, h := range map[string]agentsession.Header{
		"redacted_from without redacted": func() agentsession.Header {
			h := s.Header()
			h.RedactedFrom = &agentsession.RedactedFrom{Session: "o"}
			return h
		}(),
		"redacted_from naming itself": func() agentsession.Header {
			h := s.Header()
			h.Redacted = true
			h.RedactedFrom = &agentsession.RedactedFrom{Session: "x"}
			return h
		}(),
	} {
		c := Closure{Header: h, Own: s.Entries(), Head: s.Leaf()}
		for _, o := range []ReceiveOptions{{}, {Fetch: true}} {
			if _, err := st.Receive(ctx, c, o); !errors.Is(err, agentsession.ErrRedactedFrom) {
				t.Errorf("%s (fetch %v): %v, want ErrRedactedFrom", name, o.Fetch, err)
			}
		}
	}
	if _, err := st.Read(ctx, "x"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a refused exchange left the session: %v", err)
	}
}

// TestRedactedOnlyAppend: a store set to hold only redacted entries
// takes an append of a redacted entry to a redacted session and refuses
// a native one, in a process that opened the store before the setting
// too, leaving no object of it.
func TestRedactedOnlyAppend(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	before, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Close()
	importFile(t, before, redactedFile(t, "r", 1))
	setter, err := Open(root, WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	setter.Close()
	marked := agentsession.NewItemEntry(openresponses.UserText("marked"))
	if err := agentsession.SetRedacted(marked, []agentsession.Span{{From: 2, To: 2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := before.Append(ctx, "r", marked); err != nil {
		t.Errorf("a redacted append: %v", err)
	}
	native := agentsession.NewItemEntry(openresponses.UserText("native"))
	// Its ID, as the session would give it, to look for its object.
	probe, err := before.Read(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := probe.Prepare(native); err != nil {
		t.Fatal(err)
	}
	if _, err := before.Append(ctx, "r", native); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a native append: %v, want ErrNotRedacted", err)
	}
	if held, err := before.HasObject(ctx, SpaceEntries, native.Base().ID); err != nil || held {
		t.Errorf("the refused entry's object is in the store: %v, %v", held, err)
	}
	if s, err := before.Read(ctx, "r"); err != nil || s.Len() != 2 {
		t.Errorf("after the refused append: %v", err)
	}
}

// TestExchangeRaiseRefused: an exchange carrying a later format than the
// held header raises it, unless the session holds a redacted member
// read under a minor before 0.13, which Header.Raise cannot see: the
// session's RaiseRefused refuses it, and nothing lands. The receiver
// holds a 0.12 session with entries a and g, g holding redacted; the
// sender holds a, appended natively under this package's format, and
// pushes.
func TestExchangeRaiseRefused(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := agentsession.New(agentsession.Header{ID: "old", Format: "agentsession/0.12"})
	a, err := old.Commit(agentsession.NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	g := agentsession.NewItemEntry(openresponses.UserText("an old redacted member"))
	g.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`[1]`)}
	if _, err := old.Commit(g); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, old); err != nil {
		t.Fatal(err)
	}
	held, err := st.Import(ctx, &buf, true)
	if err != nil {
		t.Fatal(err)
	}
	h := held.Header()
	h.Format = agentsession.Format
	sender := agentsession.New(h)
	ae, _ := held.Entry(a.ID)
	if _, err := sender.Commit(ae); err != nil {
		t.Fatal(err)
	}
	n := agentsession.NewItemEntry(openresponses.UserText("native"))
	n.Parent = a.ID
	if _, err := sender.Commit(n); err != nil {
		t.Fatal(err)
	}
	for _, o := range []ReceiveOptions{{}, {Fetch: true}} {
		if _, err := st.Receive(ctx, Closure{Header: h, Own: sender.Entries(), Head: sender.Leaf()}, o); !errors.Is(err, agentsession.ErrNoRaise) {
			t.Errorf("an exchange raising a session holding an old redacted member (fetch %v): %v, want ErrNoRaise", o.Fetch, err)
		}
	}
	dir, _ := st.sessionDir("old")
	if hdr, err := readHeader(dir); err != nil || hdr.Format != "agentsession/0.12" {
		t.Errorf("the header is %s after the refusal (%v)", hdr.Format, err)
	}
	if ok, err := st.HasObject(ctx, SpaceEntries, n.Base().ID); err != nil || ok {
		t.Errorf("the refused entry's object is in the store: %v, %v", ok, err)
	}
}

// TestReceiveGatesTheBundle: a closure its caller read under another
// header, as a line read alone or under this package's format reads,
// carries a redacted member as a marker where its own header names a
// minor before 0.13, which does not define it: Receive reads it under
// the closure's header, so a store holding only redacted entries
// refuses it as not redacted, and nothing lands.
func TestReceiveGatesTheBundle(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir(), WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := agentsession.Header{ID: "s", Redacted: true, CreatedAt: time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC), Payload: agentsession.Payload}
	marked := agentsession.New(h)
	e := agentsession.NewItemEntry(openresponses.UserText("[one]"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := marked.Commit(e); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, marked); err != nil {
		t.Fatal(err)
	}
	read, err := agentsession.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	own := read.Entries()
	if _, ok := agentsession.RedactedOf(own[0]); !ok {
		t.Fatal("the entry does not read as redacted under this package's format")
	}
	h.Format = "agentsession/0.12"
	for _, o := range []ReceiveOptions{{}, {Fetch: true}} {
		if _, err := st.Receive(ctx, Closure{Header: h, Own: own, Head: own[0].Base().ID}, o); !errors.Is(err, agentsession.ErrNotRedacted) {
			t.Errorf("a 0.12 closure with a redacted member (fetch %v): %v, want ErrNotRedacted", o.Fetch, err)
		}
	}
	if held, err := st.HasObject(ctx, SpaceEntries, own[0].Base().ID); err != nil || held {
		t.Errorf("the refused entry's object is in the store: %v, %v", held, err)
	}
}

// TestRedactedOnlyScanHoldsWriters: the setting is written under the
// sweep's lock held exclusive, so a create in another store that passed
// its first look at the setting waits for the scan and the write, and is
// then refused, rather than land between them.
func TestRedactedOnlyScanHoldsWriters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	other, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	done := make(chan error, 1)
	redactedOnlyScanned = func() {
		go func() {
			_, err := other.Create(ctx, agentsession.Header{ID: "racer"})
			done <- err
		}()
		select {
		case err := <-done:
			t.Errorf("a create finished while the setting was being written: %v", err)
			done <- err
		case <-time.After(200 * time.Millisecond):
		}
	}
	defer func() { redactedOnlyScanned = nil }()
	st, err := Open(root, WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := <-done; !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("the create that waited: %v, want ErrNotRedacted", err)
	}
	if _, err := st.Read(ctx, "racer"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("the racer is in the store: %v", err)
	}
}
