package agentsession

import (
	"encoding/json"
	"errors"
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
