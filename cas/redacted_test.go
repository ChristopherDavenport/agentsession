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
	"github.com/ChristopherDavenport/openresponses"
)

// redactedFile is a redacted session file of n entries, made from an
// original by changing its header alone: the header is not hashed, so
// the entries verify as they are.
func redactedFile(t *testing.T, id string, n int) []byte {
	t.Helper()
	s := agentsession.New(agentsession.Header{ID: id, Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "orig", Entries: n}})
	for i := range n {
		if _, err := s.Commit(agentsession.NewItemEntry(openresponses.UserText(strings.Repeat("x", i+1)))); err != nil {
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
	setter, err := Open(root, WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	setter.Close()
	if before.redactedOnly.Load() {
		t.Fatal("the store opened before the setting already knows it")
	}
	if err := before.placeSession(filepath.Join(root, "sessions", "o"), agentsession.Header{ID: "o"}, nil, "", MarkRecord); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("placeSession of an original: %v, want ErrNotRedacted", err)
	}
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
	if _, err := part.Fetch(ctx, full, "r"); !errors.Is(err, agentsession.ErrRedacted) {
		t.Errorf("a fetch adding to a held redacted session: %v, want ErrRedacted", err)
	}
	// Nothing of the refused entry landed, its object included.
	fs, err := full.Read(ctx, "r")
	if err != nil {
		t.Fatal(err)
	}
	last := fs.Entries()[fs.Len()-1].Base().ID
	if held, err := part.HasObject(ctx, SpaceEntries, last); err != nil || held {
		t.Errorf("the refused entry's object is in the store: %v, %v", held, err)
	}
	if s, err := part.Read(ctx, "r"); err != nil || s.Len() != 2 {
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
			h.RedactedFrom = &agentsession.RedactedFrom{Session: "o", Entries: 1}
			return h
		}(),
		"redacted_from naming itself": func() agentsession.Header {
			h := s.Header()
			h.Redacted = true
			h.RedactedFrom = &agentsession.RedactedFrom{Session: "x", Entries: 1}
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
