package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// redactedHeader is a canonical header line of format with the given
// redacted members, spelled as Write spells them, so a rewrite can be
// compared byte for byte.
func redactedHeader(format, redacted string) string {
	return `{"created_at":"2026-09-17T16:00:00Z","format":"` + format + `","id":"01995b2a-0000-7000-8000-0000000000aa","payload":"openresponses/2026-04-24",` + redacted + `,"type":"session"}` + "\n"
}

// TestRedactedFromRoundTrip: a 0.13 header's redacted_from decodes into
// its typed members, keeps a namespaced member and a profile and an
// aligned in forms it does not take in Extra, and writes back byte for
// byte.
func TestRedactedFromRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name, member string
		want         RedactedFrom
	}{
		{
			"every member",
			`"redacted":true,"redacted_from":{"aligned":true,"entries":12,"profile":"example.redact-home@1","session":"01995b2a-0000-7000-8000-000000000001","x.mapping":{"kept":[0,2],"of":3}}`,
			RedactedFrom{Session: "01995b2a-0000-7000-8000-000000000001", Entries: 12, Profile: "example.redact-home@1", Aligned: true,
				Extra: map[string]json.RawMessage{"x.mapping": json.RawMessage(`{"kept":[0,2],"of":3}`)}},
		},
		{
			"only what is required",
			`"redacted":true,"redacted_from":{"entries":0,"session":"o"}`,
			RedactedFrom{Session: "o"},
		},
		{
			"profile and aligned in other forms",
			`"redacted":true,"redacted_from":{"aligned":false,"entries":3,"profile":"","session":"o"}`,
			RedactedFrom{Session: "o", Entries: 3,
				Extra: map[string]json.RawMessage{"aligned": json.RawMessage(`false`), "profile": json.RawMessage(`""`)}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := redactedHeader(Format, tt.member)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			h := s.Header()
			if h.RedactedFrom == nil {
				t.Fatalf("redacted_from not decoded; extra %v", h.Extra)
			}
			if _, kept := h.Extra["redacted_from"]; kept {
				t.Error("redacted_from both typed and in Extra")
			}
			got := *h.RedactedFrom
			if got.Session != tt.want.Session || got.Entries != tt.want.Entries || got.Profile != tt.want.Profile || got.Aligned != tt.want.Aligned || len(got.Extra) != len(tt.want.Extra) {
				t.Errorf("redacted_from = %+v, want %+v", got, tt.want)
			}
			for k, v := range tt.want.Extra {
				if !bytes.Equal(got.Extra[k], v) {
					t.Errorf("extra %s = %s, want %s", k, got.Extra[k], v)
				}
			}
			var out bytes.Buffer
			if err := Write(&out, s); err != nil {
				t.Fatal(err)
			}
			if out.String() != in {
				t.Errorf("rewrite\n got %s want %s", out.String(), in)
			}
		})
	}
}

// TestRedactedFromRefused: a 0.13 header carrying redacted_from on a
// session that is not redacted, or naming the session itself as its
// original, is malformed, wherever it is read.
func TestRedactedFromRefused(t *testing.T) {
	for name, redacted := range map[string]string{
		"absent": `"redacted_from":{"entries":1,"session":"o"}`,
		"false":  `"redacted":false,"redacted_from":{"entries":1,"session":"o"}`,
		"itself": `"redacted":true,"redacted_from":{"entries":1,"session":"01995b2a-0000-7000-8000-0000000000aa"}`,
	} {
		t.Run(name, func(t *testing.T) {
			in := redactedHeader(Format, redacted)
			if _, err := Read(strings.NewReader(in)); !errors.Is(err, ErrRedactedFrom) {
				t.Errorf("Read = %v, want ErrRedactedFrom", err)
			}
			if _, _, err := Scan(strings.NewReader(in)); !errors.Is(err, ErrRedactedFrom) {
				t.Errorf("Scan = %v, want ErrRedactedFrom", err)
			}
		})
	}
	for name, r := range map[string]*RedactedFrom{
		"no session":     {Entries: 1},
		"negative count": {Session: "o", Entries: -1},
		"itself":         {Session: "x", Entries: 1},
	} {
		h := Header{Format: Format, ID: "x", Payload: Payload, Redacted: true, RedactedFrom: r}
		h.fill(utcNow())
		if err := h.Validate(); !errors.Is(err, ErrRedactedFrom) {
			t.Errorf("%s: Validate = %v, want ErrRedactedFrom", name, err)
		}
	}
}

// TestRedactedFromOtherForms: a redacted_from that does not decode as
// the object, in a 0.13 file, and one in any form in a file declaring
// an earlier minor, which was free to hold one, is a member this
// package does not know: kept in Extra as written, never refused.
func TestRedactedFromOtherForms(t *testing.T) {
	for _, tt := range []struct{ name, format, member string }{
		{"0.13, a string", Format, `"redacted":true,"redacted_from":"o"`},
		{"0.13, no entries", Format, `"redacted":true,"redacted_from":{"session":"o"}`},
		{"0.13, entries as a string", Format, `"redacted_from":{"entries":"1","session":"o"}`},
		{"0.13, an empty session", Format, `"redacted_from":{"entries":1,"session":""}`},
		{"0.13, a fraction", Format, `"redacted_from":{"entries":1.5,"session":"o"}`},
		{"0.13, a session in another case", Format, `"redacted_from":{"Session":"o","entries":1}`},
		{"0.12, the object, not redacted", "agentsession/0.12", `"redacted_from":{"entries":1,"session":"o"}`},
		{"0.12, the object, redacted", "agentsession/0.12", `"redacted":true,"redacted_from":{"aligned":true,"entries":1,"session":"o"}`},
		{"0.5, another form", "agentsession/0.5", `"redacted_from":["o",1]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in := redactedHeader(tt.format, tt.member)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			h := s.Header()
			if h.RedactedFrom != nil {
				t.Errorf("redacted_from decoded as %+v", *h.RedactedFrom)
			}
			raw, ok := h.Extra["redacted_from"]
			if !ok {
				t.Fatalf("redacted_from not in Extra: %v", h.Extra)
			}
			if want := tt.member[strings.Index(tt.member, `"redacted_from":`)+len(`"redacted_from":`):]; string(raw) != want {
				t.Errorf("kept %s, want %s", raw, want)
			}
			var out bytes.Buffer
			if err := Write(&out, s); err != nil {
				t.Fatal(err)
			}
			// A 0.13 header writes back as read. An earlier one keeps
			// its own format, since raising it would give the member a
			// meaning it was not written with.
			if want := in; out.String() != want {
				t.Errorf("rewrite\n got %s want %s", out.String(), want)
			}
		})
	}
}

// TestRedactedFromMarshalConflict: a member written both typed and in
// an Extra map would appear twice, so it is refused.
func TestRedactedFromMarshalConflict(t *testing.T) {
	r := &RedactedFrom{Session: "o", Entries: 1}
	h := Header{Format: Format, ID: "x", Payload: Payload, Redacted: true, RedactedFrom: r,
		Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`{}`)}}
	if _, err := json.Marshal(h); !errors.Is(err, ErrRedactedFrom) {
		t.Errorf("header with redacted_from typed and in Extra: %v", err)
	}
	for _, tt := range []struct {
		key string
		r   RedactedFrom
		bad bool
	}{
		{"session", RedactedFrom{Session: "o"}, true},
		{"entries", RedactedFrom{Session: "o"}, true},
		{"profile", RedactedFrom{Session: "o", Profile: "p"}, true},
		{"profile", RedactedFrom{Session: "o"}, false},
		{"aligned", RedactedFrom{Session: "o", Aligned: true}, true},
		{"aligned", RedactedFrom{Session: "o"}, false},
	} {
		tt.r.Extra = map[string]json.RawMessage{tt.key: json.RawMessage(`null`)}
		_, err := json.Marshal(tt.r)
		if got := errors.Is(err, ErrRedactedFrom); got != tt.bad {
			t.Errorf("%+v: Marshal = %v", tt.r, err)
		}
	}
}

// TestNoRaise: a header that names a minor before 0.13 and holds
// redacted_from, in any form, is not raised: not in memory by Read, so
// it writes back under its own format, not by [Header.Raise], and not
// by an append, which is refused.
func TestNoRaise(t *testing.T) {
	file, _ := hashedLines(t, "agentsession/0.12",
		`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`)
	file = strings.Replace(file, `"records":`, `"redacted_from":{"entries":1,"session":"o"},"records":`, 1)
	s, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if f := s.Header().Format; f != "agentsession/0.12" {
		t.Errorf("held at %s, want agentsession/0.12", f)
	}
	var out bytes.Buffer
	if err := Write(&out, s); err != nil {
		t.Fatal(err)
	}
	back, err := Read(&out)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if f := back.Header().Format; f != "agentsession/0.12" {
		t.Errorf("written as %s", f)
	}
	for name, add := range map[string]func(Entry) error{
		"Append":  func(e Entry) error { _, err := s.Append(e); return err },
		"Prepare": func(e Entry) error { _, err := s.Prepare(e); return err },
	} {
		err := add(NewItemEntry(openresponses.UserText("more")))
		if !errors.Is(err, ErrNoRaise) || !errors.Is(err, ErrUnsupportedFormat) {
			t.Errorf("%s = %v, want ErrNoRaise", name, err)
		}
	}
	if s.Len() != 1 {
		t.Errorf("the session holds %d entries", s.Len())
	}

	// A file before 0.5 is rehashed as 0.5's, so it takes the last
	// minor that does not define the member.
	old := `{"type":"session","format":"agentsession/0.4","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24","redacted_from":"o"}` + "\n"
	if s, err := Read(strings.NewReader(old)); err != nil {
		t.Fatal(err)
	} else if f := s.Header().Format; f != "agentsession/0.12" {
		t.Errorf("a 0.4 header holding redacted_from read as %s", f)
	}

	for _, tt := range []struct {
		name string
		h    Header
		to   string
		ok   bool
	}{
		{"0.12, in Extra", Header{Format: "agentsession/0.12", Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`"o"`)}}, Format, false},
		{"0.12, typed", Header{Format: "agentsession/0.12", Redacted: true, RedactedFrom: &RedactedFrom{Session: "o"}}, Format, false},
		{"0.10, in Extra, to 0.12", Header{Format: "agentsession/0.10", Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`{}`)}}, "agentsession/0.12", true},
		{"0.12, without", Header{Format: "agentsession/0.12", Extra: map[string]json.RawMessage{"x.y": json.RawMessage(`1`)}}, Format, true},
		{"0.13, typed, to 0.14", Header{Format: Format, Redacted: true, RedactedFrom: &RedactedFrom{Session: "o"}}, "agentsession/0.14", true},
	} {
		h := tt.h
		err := h.Raise(tt.to)
		if tt.ok && (err != nil || h.Format != tt.to) || !tt.ok && (!errors.Is(err, ErrNoRaise) || h.Format != tt.h.Format) {
			t.Errorf("%s: Raise(%s) = %v, format %s", tt.name, tt.to, err, h.Format)
		}
	}
}
