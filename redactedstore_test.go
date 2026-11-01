package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestCheckCreate: a redacted session is created as any other; a
// redacted_from without redacted, typed or decoded into Extra, is
// malformed; and a store that holds only redacted entries creates no
// session whose header lacks redacted.
func TestCheckCreate(t *testing.T) {
	for _, tt := range []struct {
		h    Header
		only bool
		want error
	}{
		{Header{}, false, nil},
		{Header{Redacted: true, RedactedFrom: &RedactedFrom{Session: "o"}}, false, nil},
		{Header{Redacted: true}, true, nil},
		{Header{}, true, ErrNotRedacted},
		{Header{Base: "x"}, true, ErrNotRedacted},
		{Header{RedactedFrom: &RedactedFrom{Session: "o"}}, false, ErrRedactedFrom},
		{Header{Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`{"session":"o"}`)}}, false, ErrRedactedFrom},
		{Header{Format: "agentsession/0.12", Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`"o"`)}}, false, nil},
		{Header{ID: "r", Redacted: true, RedactedFrom: &RedactedFrom{Session: "r"}}, false, ErrRedactedFrom},
		{Header{Redacted: true, RedactedFrom: &RedactedFrom{}}, false, ErrRedactedFrom},
		{Header{Redacted: true, RedactedFrom: &RedactedFrom{Session: "o"}}, true, nil},
	} {
		if err := CheckCreate(tt.h, tt.only); !errors.Is(err, tt.want) || (tt.want == nil) != (err == nil) {
			t.Errorf("CheckCreate(%+v, %v) = %v, want %v", tt.h, tt.only, err, tt.want)
		}
	}
}

// TestCheckRedacted: a store that holds only redacted entries refuses a
// header without redacted and any entry without the member, an added
// entry ([]) being redacted.
func TestCheckRedacted(t *testing.T) {
	marked, added, native := NewItemEntry(openresponses.UserText("a")), NewItemEntry(openresponses.UserText("b")), NewItemEntry(openresponses.UserText("c"))
	if err := SetRedacted(marked, []Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if err := SetRedacted(added, nil); err != nil {
		t.Fatal(err)
	}
	h := Header{ID: "r", Redacted: true}
	if err := CheckRedacted(h, []Entry{marked, added}); err != nil {
		t.Errorf("redacted entries: %v", err)
	}
	if err := CheckRedacted(h, []Entry{marked, native}); !errors.Is(err, ErrNotRedacted) {
		t.Errorf("a native entry: %v, want ErrNotRedacted", err)
	}
	if err := CheckRedacted(Header{ID: "o"}, nil); !errors.Is(err, ErrNotRedacted) {
		t.Errorf("a header without redacted: %v, want ErrNotRedacted", err)
	}
	// A redacted member read under a minor before 0.13, and ones in a
	// form no minor defines, are not markers.
	gated, err := UnmarshalEntry([]byte(lineWithRedacted(t, `[1]`)))
	if err != nil {
		t.Fatal(err)
	}
	GateEntry("agentsession/0.12", gated)
	if err := CheckRedacted(h, []Entry{gated}); !errors.Is(err, ErrNotRedacted) {
		t.Errorf("a redacted member from before 0.13: %v, want ErrNotRedacted", err)
	}
	for _, form := range []string{`"x"`, `[0]`, `[[2,1]]`, `[3,2]`} {
		e, err := UnmarshalEntry([]byte(lineWithRedacted(t, form)))
		if err != nil {
			t.Fatal(err)
		}
		if err := CheckRedacted(h, []Entry{e}); !errors.Is(err, ErrNotRedacted) {
			t.Errorf("redacted %s: %v, want ErrNotRedacted", form, err)
		}
	}
}

// lineWithRedacted is an entry line, its id its hash, whose redacted
// member is written as form.
func lineWithRedacted(t *testing.T, form string) string {
	t.Helper()
	s := New(Header{ID: "l"})
	e := NewItemEntry(openresponses.UserText("x"))
	e.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(form)}
	if _, err := s.Commit(e); err != nil {
		t.Fatal(err)
	}
	line, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// A store that holds only redacted entries takes a leaf label as the
// record of a head move only when it is that and nothing more: its
// target names an entry of the session and it carries no other member
// at any depth. A target of free text, or text inside a parents
// element, is content, and is refused as any unredacted entry is.
func TestRedactedOnlyTakesOnlyABareHeadMove(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore(MemoryRedactedOnly())
	s, err := st.Create(ctx, Header{Redacted: true, RedactedFrom: &RedactedFrom{Session: "orig", Profile: "p"}})
	if err != nil {
		t.Fatal(err)
	}
	first := NewItemEntry(openresponses.UserText("[redacted]"))
	if err := SetRedacted(first, nil); err != nil {
		t.Fatal(err)
	}
	firstID, err := st.Append(ctx, s.ID(), first)
	if err != nil {
		t.Fatal(err)
	}
	line := func(body string) Entry {
		t.Helper()
		id, _, err := EntryHashes([]byte(body))
		if err != nil {
			t.Fatal(err)
		}
		e, err := UnmarshalEntry([]byte(`{"id":"` + id + `",` + body[1:]))
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	for name, body := range map[string]string{
		"a target of free text": `{"type":"label","ts":"2026-01-01T00:00:00Z","parent":"FIRST","label":"leaf","target":"my password is hunter2"}`,
		"text in parents":       `{"type":"label","ts":"2026-01-01T00:00:00Z","parent":null,"label":"leaf","target":"FIRST","parents":[{"entry":"FIRST","note":"my password is hunter2"}]}`,
		"a member of its own":   `{"type":"label","ts":"2026-01-01T00:00:00Z","parent":"FIRST","label":"leaf","target":"FIRST","note":"my password is hunter2"}`,
	} {
		if _, err := st.Append(ctx, s.ID(), line(strings.ReplaceAll(body, "FIRST", firstID))); !errors.Is(err, ErrNotRedacted) {
			t.Errorf("%s: %v, want ErrNotRedacted", name, err)
		}
	}
	if _, err := st.Append(ctx, s.ID(), NewLabelEntry(firstID, LeafLabel)); err != nil {
		t.Errorf("a bare leaf label on an entry of the session: %v, want it taken", err)
	}
}
