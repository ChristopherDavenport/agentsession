package agentsession

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

func TestNewFillsHeader(t *testing.T) {
	s := New(Header{CWD: "/p"})
	h := s.Header()
	if h.Format != Format || h.Payload != Payload || h.ID == "" || h.CreatedAt.IsZero() || h.CWD != "/p" {
		t.Errorf("header = %+v", h)
	}
	if err := h.Validate(); err != nil {
		t.Error(err)
	}
	given := Header{ID: "fixed", CreatedAt: fixedTime, Format: "agentsession/0.0", Payload: "openresponses/2026-01-01"}
	if got := New(given).Header(); !reflect.DeepEqual(got, given) {
		t.Errorf("New changed a complete header: %+v", got)
	}
	if s.ID() != h.ID || s.Len() != 0 || s.Leaf() != "" || len(s.Roots()) != 0 {
		t.Error("empty session state")
	}
}

func TestAppend(t *testing.T) {
	s := New(Header{})
	clock := fixedTime
	s.setClock(func() time.Time { clock = clock.Add(time.Second); return clock })

	// First append: root, ID assigned, timestamp from the clock.
	cfg := &ConfigEntry{Model: "m"}
	id, err := s.Append(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" || id != cfg.ID || cfg.Parent != "" || !cfg.Timestamp.Equal(fixedTime.Add(time.Second)) {
		t.Errorf("first append: id %q entry %+v", id, cfg.EntryBase)
	}
	if s.Leaf() != id || !reflect.DeepEqual(s.Roots(), []string{id}) {
		t.Errorf("leaf %s roots %v", s.Leaf(), s.Roots())
	}

	// Second append hangs off the leaf.
	u := NewItemEntry(openresponses.UserText("hi"))
	uid, err := s.Append(u)
	if err != nil {
		t.Fatal(err)
	}
	if u.Parent != id || s.Leaf() != uid {
		t.Errorf("second append parent %q leaf %s", u.Parent, s.Leaf())
	}

	// An explicit parent branches in place and becomes the leaf.
	alt := &ItemEntry{EntryBase: EntryBase{Parent: id, ID: "alt", Timestamp: fixedTime}, Item: openresponses.UserText("other")}
	if _, err := s.Append(alt); err != nil {
		t.Fatal(err)
	}
	if s.Leaf() != "alt" || !reflect.DeepEqual(s.Children(id), []string{uid, "alt"}) {
		t.Errorf("branch: leaf %s children %v", s.Leaf(), s.Children(id))
	}
	if !alt.Timestamp.Equal(fixedTime) {
		t.Error("explicit timestamp was replaced")
	}

	// Branch moves the leaf; ResetLeaf starts a new root.
	if err := s.Branch(uid); err != nil || s.Leaf() != uid {
		t.Errorf("Branch: %v leaf %s", err, s.Leaf())
	}
	if err := s.Branch("nope"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("Branch(nope) = %v", err)
	}
	s.ResetLeaf()
	root2 := &InfoEntry{Name: "second root"}
	if _, err := s.Append(root2); err != nil {
		t.Fatal(err)
	}
	if root2.Parent != "" || len(s.Roots()) != 2 {
		t.Errorf("second root parent %q roots %v", root2.Parent, s.Roots())
	}

	// Errors.
	if _, err := s.Append(&InfoEntry{EntryBase: EntryBase{ID: "alt"}}); !errors.Is(err, ErrDuplicateEntry) {
		t.Errorf("duplicate = %v", err)
	}
	if _, err := s.Append(&InfoEntry{EntryBase: EntryBase{Parent: "ghost"}}); !errors.Is(err, ErrNoEntry) {
		t.Errorf("missing parent = %v", err)
	}
	if _, err := s.Append(nil); err == nil {
		t.Error("nil entry accepted")
	}
	if _, err := s.Append(&UnknownEntry{Type: "x:y"}); err == nil {
		t.Error("unknown entry without raw accepted")
	}
	if s.Len() != 4 {
		t.Errorf("len = %d", s.Len())
	}
	if got := s.Path("alt"); len(got) != 2 || got[0].Base().ID != id || got[1].Base().ID != "alt" {
		t.Errorf("Path(alt) = %v", got)
	}
	if s.Path("nope") != nil {
		t.Error("Path(nope) != nil")
	}
	if got := s.Entries(); len(got) != 4 || got[3] != Entry(root2) {
		t.Errorf("Entries = %v", got)
	}
}

// TestAppendUnknownEntry checks that an extension entry built in memory
// gets its envelope filled in and written from the raw line.
func TestAppendUnknownEntry(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&InfoEntry{Name: "root"}); err != nil {
		t.Fatal(err)
	}
	u := &UnknownEntry{Type: "acme:note", Raw: json.RawMessage(`{"type":"acme:note","text":"<x>"}`)}
	id, err := s.Append(u)
	if err != nil {
		t.Fatal(err)
	}
	out, err := MarshalEntry(u)
	if err != nil {
		t.Fatal(err)
	}
	back, err := UnmarshalEntry(out)
	if err != nil {
		t.Fatalf("re-decode %s: %v", out, err)
	}
	if back.Base().ID != id || back.Base().Parent != s.Roots()[0] || back.Base().Timestamp.IsZero() {
		t.Errorf("envelope = %+v from %s", back.Base(), out)
	}
	if got := string(back.(*UnknownEntry).Raw); got != string(out) || !regexp.MustCompile(`"text":"<x>"`).MatchString(got) {
		t.Errorf("raw = %s", got)
	}
	// A raw line that already carries its own type is accepted without
	// Type set on the struct.
	u2 := &UnknownEntry{Raw: json.RawMessage(`{"type":"acme:other","k":1}`)}
	if _, err := s.Append(u2); err != nil || u2.Type != "acme:other" {
		t.Errorf("Append: %v type %q", err, u2.Type)
	}
	if _, err := s.Append(&UnknownEntry{Type: "acme:bad", Raw: json.RawMessage(`[`)}); err == nil {
		t.Error("malformed raw accepted")
	}
}

func TestIDs(t *testing.T) {
	uuid := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := NewSessionID()
		if !uuid.MatchString(id) {
			t.Fatalf("NewSessionID = %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %s", id)
		}
		seen[id] = true
		eid := NewEntryID()
		if len(eid) != 8 {
			t.Fatalf("NewEntryID = %q", eid)
		}
	}
	// UUIDv7 sorts by time.
	a := uuidv7(fixedTime)
	b := uuidv7(fixedTime.Add(time.Millisecond))
	if a >= b {
		t.Errorf("%s should sort before %s", a, b)
	}
	// Collision handling widens the ID rather than looping forever.
	taken := map[string]bool{}
	id := newUniqueEntryID(func(id string) bool { return len(id) == 8 })
	if len(id) != 32 {
		t.Errorf("fallback id %q", id)
	}
	_ = taken
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		in           string
		major, minor int
		ok           bool
	}{
		{"agentsession/0.1", 0, 1, true},
		{"agentsession/0.12", 0, 12, true},
		{"agentsession/1.0", 1, 0, true},
		{"agentsession/1", 0, 0, false},
		{"agentsession/a.b", 0, 0, false},
		{"pi/0.1", 0, 0, false},
		{"", 0, 0, false},
	}
	for _, tt := range tests {
		major, minor, err := ParseFormat(tt.in)
		if (err == nil) != tt.ok || major != tt.major || minor != tt.minor {
			t.Errorf("ParseFormat(%q) = %d, %d, %v", tt.in, major, minor, err)
		}
		if err != nil && !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("ParseFormat(%q) error %v is not ErrUnsupportedFormat", tt.in, err)
		}
	}
}

func TestHeaderValidate(t *testing.T) {
	good := Header{Format: Format, ID: "x", CreatedAt: fixedTime, Payload: Payload}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Header){
		"major":   func(h *Header) { h.Format = "agentsession/1.0" },
		"id":      func(h *Header) { h.ID = "" },
		"created": func(h *Header) { h.CreatedAt = time.Time{} },
		"payload": func(h *Header) { h.Payload = "" },
		"profile": func(h *Header) { h.Payload = "anthropic/1" },
	} {
		h := good
		mutate(&h)
		if err := h.Validate(); err == nil {
			t.Errorf("%s: Validate accepted %+v", name, h)
		}
	}
}
