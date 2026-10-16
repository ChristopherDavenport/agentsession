package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/openresponses"
)

// redactedHeader is a canonical header line of format with the given
// redacted members, spelled as Write spells them, so a rewrite can be
// compared byte for byte.
func redactedHeader(format, redacted string) string {
	return `{"created_at":"2026-09-17T16:00:00Z","format":"` + format + `","id":"01995b2a-0000-7000-8000-0000000000aa","payload":"openresponses/2026-04-24",` + redacted + `,"type":"session"}` + "\n"
}

// TestRedactedFromRoundTrip: a 0.13 header's redacted_from decodes into
// its typed members, keeps a namespaced member, a profile in a form it
// does not take and the first draft's entries and aligned in Extra, and
// writes back byte for byte.
func TestRedactedFromRoundTrip(t *testing.T) {
	for _, tt := range []struct {
		name, member string
		want         RedactedFrom
	}{
		{
			"every member",
			`"redacted":true,"redacted_from":{"profile":"example.redact-home@1","session":"01995b2a-0000-7000-8000-000000000001","x.mapping":{"kept":[0,2],"of":3}}`,
			RedactedFrom{Session: "01995b2a-0000-7000-8000-000000000001", Profile: "example.redact-home@1",
				Extra: map[string]json.RawMessage{"x.mapping": json.RawMessage(`{"kept":[0,2],"of":3}`)}},
		},
		{
			"only what is required",
			`"redacted":true,"redacted_from":{"session":"o"}`,
			RedactedFrom{Session: "o"},
		},
		{
			"profile in another form",
			`"redacted":true,"redacted_from":{"profile":"","session":"o"}`,
			RedactedFrom{Session: "o", Extra: map[string]json.RawMessage{"profile": json.RawMessage(`""`)}},
		},
		{
			"the first draft's entries and aligned",
			`"redacted":true,"redacted_from":{"aligned":true,"entries":12,"profile":"p","session":"o"}`,
			RedactedFrom{Session: "o", Profile: "p",
				Extra: map[string]json.RawMessage{"aligned": json.RawMessage(`true`), "entries": json.RawMessage(`12`)}},
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
			if got.Session != tt.want.Session || got.Profile != tt.want.Profile || len(got.Extra) != len(tt.want.Extra) {
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
// original, is malformed, wherever it is read. The refusal binds the
// object whose session is a non-empty string, whatever else it holds.
func TestRedactedFromRefused(t *testing.T) {
	for name, redacted := range map[string]string{
		"absent":                `"redacted_from":{"session":"o"}`,
		"false":                 `"redacted":false,"redacted_from":{"session":"o"}`,
		"itself":                `"redacted":true,"redacted_from":{"session":"01995b2a-0000-7000-8000-0000000000aa"}`,
		"absent, other members": `"redacted_from":{"entries":"1","profile":7,"session":"o"}`,
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
		"no session": {Profile: "p"},
		"itself":     {Session: "x"},
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
		{"0.13, no session", Format, `"redacted_from":{"profile":"p"}`},
		{"0.13, an empty session", Format, `"redacted_from":{"session":""}`},
		{"0.13, a session that is a number", Format, `"redacted_from":{"session":1}`},
		{"0.13, a session in another case", Format, `"redacted_from":{"Session":"o"}`},
		{"0.12, the object, not redacted", "agentsession/0.12", `"redacted_from":{"session":"o"}`},
		{"0.12, the object, redacted", "agentsession/0.12", `"redacted":true,"redacted_from":{"profile":"p","session":"o"}`},
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
// an Extra map would appear twice, so it is refused; the first draft's
// entries and aligned are untyped and may be in Extra.
func TestRedactedFromMarshalConflict(t *testing.T) {
	r := &RedactedFrom{Session: "o"}
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
		{"profile", RedactedFrom{Session: "o", Profile: "p"}, true},
		{"profile", RedactedFrom{Session: "o"}, false},
		{"entries", RedactedFrom{Session: "o"}, false},
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
	file = strings.Replace(file, `"records":`, `"redacted_from":{"session":"o"},"records":`, 1)
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

// canonicalFile is in with each line in canonical form, as Write writes
// a file whose lines it reproduces.
func canonicalFile(t *testing.T, in string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSuffix(in, "\n"), "\n") {
		c, err := jcs.Transform([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		out.Write(c)
		out.WriteByte('\n')
	}
	return out.String()
}

// writeFile is s as Write writes it.
func writeFile(t *testing.T, s *Session) string {
	t.Helper()
	var out bytes.Buffer
	if err := Write(&out, s); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestRedactedEntryRoundTrip: an entry's redacted member in each form
// the format defines, on a core entry and an extension entry, reads as
// its spans, by Read and by Scan's Decode, writes back byte for byte,
// and is hashed: the same entry without it has another id.
func TestRedactedEntryRoundTrip(t *testing.T) {
	msg := `"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	for _, tt := range []struct {
		name, body, member string
		want               []Span
	}{
		{"one position", `"type":"item",` + msg, `[3]`, []Span{{3, 3}}},
		{"a range", `"type":"info","name":"[redacted]"`, `[[2,5]]`, []Span{{2, 5}}},
		{"a merge", `"type":"item",` + msg, `[1,[3,4],7]`, []Span{{1, 1}, {3, 4}, {7, 7}}},
		{"adjacent", `"type":"item",` + msg, `[1,2,[3,9]]`, []Span{{1, 1}, {2, 2}, {3, 9}}},
		{"added", `"type":"item",` + msg, `[]`, []Span{}},
		{"an extension entry", `"type":"x:note","text":"t"`, `[[1,2],4]`, []Span{{1, 2}, {4, 4}}},
		{"a number spelled another way", `"type":"item",` + msg, `[3.0,[1e1,2E1]]`, []Span{{3, 3}, {10, 20}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in, ids := hashedLines(t, Format, tt.body+`,"redacted":`+tt.member)
			_, plain := hashedLines(t, Format, tt.body)
			if ids[0] == plain[0] {
				t.Error("redacted is not hashed")
			}
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			e := s.Entries()[0]
			got, ok := RedactedOf(e)
			if !ok || !slices.Equal(got, tt.want) || got == nil {
				t.Errorf("RedactedOf = %v %v, want %v", got, ok, tt.want)
			}
			if _, held := e.Base().Unknown["redacted"]; held {
				t.Error("redacted both typed and in Unknown")
			}
			if out, want := writeFile(t, s), canonicalFile(t, in); out != want {
				t.Errorf("rewrite\n got %s want %s", out, want)
			}
			_, seq, err := Scan(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			for r, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				d, err := r.Decode()
				if err != nil {
					t.Fatal(err)
				}
				if got, ok := RedactedOf(d); !ok || !slices.Equal(got, tt.want) {
					t.Errorf("Decode: RedactedOf = %v %v", got, ok)
				}
			}
		})
	}
}

// TestRedactedEntryOtherForms: a redacted member not in the form the
// format defines is one this package does not know, in a 0.13 file: it
// stays in Unknown as written, the entry is not redacted, and the file
// writes back byte for byte.
func TestRedactedEntryOtherForms(t *testing.T) {
	for name, member := range map[string]string{
		"null":               `null`,
		"a number":           `3`,
		"a string":           `"3"`,
		"an object":          `{"from":1}`,
		"zero":               `[0]`,
		"negative":           `[-1]`,
		"a fraction":         `[1.5]`,
		"a string element":   `["1"]`,
		"a boolean element":  `[true]`,
		"a range of one":     `[[5,5]]`,
		"a range backwards":  `[[5,4]]`,
		"a range of three":   `[[1,2,3]]`,
		"a range from zero":  `[[0,2]]`,
		"a nested range":     `[[1,[2,3]]]`,
		"unsorted":           `[3,2]`,
		"repeated":           `[2,2]`,
		"overlapping":        `[[1,3],2]`,
		"overlapping ranges": `[[1,3],[3,4]]`,
		"too large":          `[1e21]`,
		"more than an int":   `[100000000000000000000]`,
	} {
		t.Run(name, func(t *testing.T) {
			in, _ := hashedLines(t, Format, `"type":"info","name":"n","redacted":`+member)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			e := s.Entries()[0]
			if got, ok := RedactedOf(e); ok {
				t.Errorf("RedactedOf = %v, want not redacted", got)
			}
			if raw := e.Base().Unknown["redacted"]; string(raw) != member {
				t.Errorf("kept %s, want %s", raw, member)
			}
			if out, want := writeFile(t, s), canonicalFile(t, in); out != want {
				t.Errorf("rewrite\n got %s want %s", out, want)
			}
		})
	}
}

// TestRedactedEntryGate: in a file whose header names a minor before
// 0.13 an entry's redacted is unknown whatever its form, by Read, by
// Scan's Decode and by Extend: kept as written, the entry not redacted.
// Such a file is not raised: Read holds the header at its own format,
// Write keeps it, and an append is refused with ErrNoRaise.
func TestRedactedEntryGate(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	more := func() Entry { return NewItemEntry(openresponses.UserText("more")) }
	refused := func(t *testing.T, s *Session) {
		t.Helper()
		for name, add := range map[string]func(Entry) error{
			"Append":  func(e Entry) error { _, err := s.Append(e); return err },
			"Prepare": func(e Entry) error { _, err := s.Prepare(e); return err },
		} {
			if err := add(more()); !errors.Is(err, ErrNoRaise) || !errors.Is(err, ErrUnsupportedFormat) {
				t.Errorf("%s = %v, want ErrNoRaise", name, err)
			}
		}
	}
	for _, format := range []string{"agentsession/0.12", "agentsession/0.5"} {
		t.Run(format, func(t *testing.T) {
			in, _ := hashedLines(t, format, msg, msg+`,"redacted":[1]`, `"type":"x:note","redacted":[]`)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			for i, e := range s.Entries()[1:] {
				if got, ok := RedactedOf(e); ok {
					t.Errorf("entry %d: RedactedOf = %v under %s", i+1, got, format)
				}
			}
			if raw := s.Entries()[1].Base().Unknown["redacted"]; string(raw) != `[1]` {
				t.Errorf("kept %s", raw)
			}
			if f := s.Header().Format; f != format {
				t.Errorf("held at %s, want %s", f, format)
			}
			if out, want := writeFile(t, s), canonicalFile(t, in); out != want {
				t.Errorf("rewrite\n got %s want %s", out, want)
			}
			refused(t, s)
			if s.Len() != 3 {
				t.Errorf("the session holds %d entries", s.Len())
			}
			_, seq, err := Scan(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			for r, err := range seq {
				if err != nil {
					t.Fatal(err)
				}
				d, err := r.Decode()
				if err != nil {
					t.Fatal(err)
				}
				if got, ok := RedactedOf(d); ok {
					t.Errorf("Decode: RedactedOf = %v under %s", got, format)
				}
			}
		})
	}

	// A file before 0.5 is rehashed as 0.5's, so it takes the last minor
	// that does not define the member.
	old := `{"type":"session","format":"agentsession/0.4","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}` + "\n" +
		`{"type":"item","id":"a","parent":null,"ts":"2026-09-17T16:00:01Z","item":{"type":"message","role":"user","content":[]},"redacted":[1]}` + "\n"
	if s, err := Read(strings.NewReader(old)); err != nil {
		t.Fatal(err)
	} else if f := s.Header().Format; f != "agentsession/0.12" {
		t.Errorf("a 0.4 file an entry of which holds redacted read as %s", f)
	} else if _, ok := RedactedOf(s.Entries()[0]); ok {
		t.Error("a 0.4 entry reads as redacted")
	} else {
		refused(t, s)
	}

	// Extend reads under the format the file declared: a 0.12 file
	// without the member is raised in memory, and an entry that arrives
	// holding redacted puts the header back and stops appends.
	in, ids := hashedLines(t, "agentsession/0.12", msg)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if f := s.Header().Format; f != Format {
		t.Fatalf("a 0.12 file without the member read as %s", f)
	}
	line := `{"type":"item","parent":"` + ids[0] + `","ts":"2026-09-17T16:00:02Z","item":{"type":"message","role":"user","content":[]},"redacted":[1]}`
	id, _, err := EntryHashes([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	e, err := UnmarshalEntry([]byte(`{"id":"` + id + `",` + line[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := RedactedOf(e); !ok {
		t.Fatal("UnmarshalEntry reads a line alone as this package writes it")
	}
	if _, err := s.Extend(e); err != nil {
		t.Fatal(err)
	}
	if got, ok := RedactedOf(e); ok {
		t.Errorf("Extend kept redacted %v under 0.12", got)
	}
	if f := s.Header().Format; f != "agentsession/0.12" {
		t.Errorf("after Extend the header is %s", f)
	}
	refused(t, s)

	// A 0.13 file whose entry holds redacted in another form is raised
	// already, and takes appends.
	in, _ = hashedLines(t, Format, msg+`,"redacted":[0]`)
	if s, err := Read(strings.NewReader(in)); err != nil {
		t.Fatal(err)
	} else if _, err := s.Append(more()); err != nil {
		t.Errorf("Append to a 0.13 file: %v", err)
	}
}

// TestSetRedacted: SetRedacted marks an entry with spans in the form,
// refusing any other and leaving the entry as it was; the mark is
// written as positions and ranges, replaces a member in another form,
// tells an added entry ([]) from one that is not redacted, and survives
// a commit and a read. ClearRedacted takes it off again, on a core and
// an extension entry alike.
func TestSetRedacted(t *testing.T) {
	for _, bad := range [][]Span{
		{{0, 0}},
		{{2, 1}},
		{{3, 3}, {2, 2}},
		{{1, 3}, {3, 4}},
		{{1, 1}, {1, 1}},
	} {
		e := NewItemEntry(openresponses.UserText("x"))
		if err := SetRedacted(e, bad); !errors.Is(err, ErrBadRedacted) {
			t.Errorf("SetRedacted(%v) = %v, want ErrBadRedacted", bad, err)
		}
		if _, ok := RedactedOf(e); ok {
			t.Errorf("SetRedacted(%v) marked the entry", bad)
		}
	}

	s := New(Header{Redacted: true, RedactedFrom: &RedactedFrom{Session: "o", Profile: "p"}})
	marks := []struct {
		spans []Span
		wire  string
	}{
		{[]Span{{1, 1}}, `"redacted":[1]`},
		{[]Span{{2, 4}}, `"redacted":[[2,4]]`},
		{[]Span{{5, 5}, {7, 9}, {10, 10}}, `"redacted":[5,[7,9],10]`},
		{nil, `"redacted":[]`},
		{[]Span{}, `"redacted":[]`},
	}
	for _, m := range marks {
		e := NewItemEntry(openresponses.UserText("x"))
		e.Unknown = map[string]json.RawMessage{"redacted": json.RawMessage(`"another form"`)}
		if err := SetRedacted(e, m.spans); err != nil {
			t.Fatal(err)
		}
		data, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), m.wire) || strings.Count(string(data), `"redacted"`) != 1 {
			t.Errorf("SetRedacted(%v) writes %s, want %s", m.spans, data, m.wire)
		}
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	native := NewItemEntry(openresponses.UserText("native"))
	if _, err := s.Append(native); err != nil {
		t.Fatal(err)
	}
	back, err := Read(strings.NewReader(writeFile(t, s)))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range back.Entries() {
		got, ok := RedactedOf(e)
		if i == len(marks) {
			if ok {
				t.Errorf("the native entry reads as redacted %v", got)
			}
			continue
		}
		want := marks[i].spans
		if want == nil {
			want = []Span{}
		}
		if !ok || got == nil || !slices.Equal(got, want) {
			t.Errorf("entry %d reads back %v %v, want %v", i, got, ok, want)
		}
	}

	for _, line := range []string{
		`{"type":"info","id":"x","parent":null,` + tsLine + `,"name":"n","redacted":[0]}`,
		`{"type":"info","id":"x","parent":null,` + tsLine + `,"name":"n","redacted":[1]}`,
		`{"type":"x:note","id":"x","parent":null,` + tsLine + `,"text":"t","redacted":[1]}`,
	} {
		e := entryOf(t, line)
		if err := ClearRedacted(e); err != nil {
			t.Fatal(err)
		}
		if out := canonicalEntry(t, e); strings.Contains(out, "redacted") {
			t.Errorf("ClearRedacted left %s", out)
		}
		if err := SetRedacted(e, []Span{{2, 3}}); err != nil {
			t.Fatal(err)
		}
		if out := canonicalEntry(t, e); !strings.Contains(out, `"redacted":[[2,3]]`) {
			t.Errorf("SetRedacted wrote %s", out)
		}
	}
}

// TestRedactedKeptThroughHelpers: an entry read under a minor before
// 0.13 keeps its redacted unknown through RewriteStrings and RemapRefs,
// which build their result from the entry's JSON, so a helper cannot
// give the member a meaning the file did not.
func TestRedactedKeptThroughHelpers(t *testing.T) {
	in, _ := hashedLines(t, "agentsession/0.12",
		`"type":"info","name":"n","redacted":[1]`,
		`"type":"x:note","text":"t","redacted":[2]`)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range s.Entries() {
		rw, err := RewriteStrings(e, strings.ToUpper)
		if err != nil {
			t.Fatal(err)
		}
		rm, err := RemapRefs(e, Remap{})
		if err != nil {
			t.Fatal(err)
		}
		for name, out := range map[string]Entry{"RewriteStrings": rw, "RemapRefs": rm.Entry} {
			if got, ok := RedactedOf(out); ok {
				t.Errorf("entry %d: %s made redacted %v", i, name, got)
			}
			if c := canonicalEntry(t, out); !strings.Contains(c, `"redacted":[`) {
				t.Errorf("entry %d: %s dropped the member: %s", i, name, c)
			}
		}
	}
}

// TestContextHashLeavesRedacted: redacted is provenance, removed from
// the context hash in whatever form it is held, as legacy_id and
// normalised are, so entries with it and the same bodies without it
// share every context hash.
func TestContextHashLeavesRedacted(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	cfg := `"type":"config","model":"m"`
	plainIn, plain := hashedLines(t, Format, msg, cfg, msg)
	for _, tt := range []struct{ name, format, a, b, c string }{
		{"typed", Format, `,"redacted":[1]`, `,"redacted":[[2,3]]`, `,"redacted":[]`},
		{"another form", Format, `,"redacted":["x"]`, `,"redacted":0`, `,"redacted":null`},
		{"under 0.12", "agentsession/0.12", `,"redacted":[1]`, ``, `,"redacted":[4]`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in, ids := hashedLines(t, tt.format, msg+tt.a, cfg+tt.b, msg+tt.c)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			p, err := Read(strings.NewReader(plainIn))
			if err != nil {
				t.Fatal(err)
			}
			for i := range ids {
				got, err1 := s.ContextHash(ids[i])
				want, err2 := p.ContextHash(plain[i])
				if err1 != nil || err2 != nil || got != want {
					t.Errorf("entry %d: context hash %s (%v), without redacted %s (%v)", i, got, err1, want, err2)
				}
			}
		})
	}
}

// TestRaiseRefused: a session may not be written at 0.13 or later when
// its header holds redacted_from from before 0.13 or an entry holds a
// redacted member read under such a header; Fork refuses a fork at 0.13
// whose prefix holds such an entry, and takes one whose prefix does not
// or that is written under the earlier minor.
func TestRaiseRefused(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	in, ids := hashedLines(t, "agentsession/0.12", msg, msg+`,"redacted":[1]`, msg)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
		t.Errorf("RaiseRefused(%s) = %v, want ErrNoRaise", Format, err)
	}
	if err := s.RaiseRefused("agentsession/0.12"); err != nil {
		t.Errorf("RaiseRefused(0.12) = %v", err)
	}
	if err := New(Header{}).RaiseRefused(Format); err != nil {
		t.Errorf("a new session: %v", err)
	}
	h := Header{Format: "agentsession/0.12", Extra: map[string]json.RawMessage{"redacted_from": json.RawMessage(`{"session":"o"}`)}}
	if err := New(h).RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
		t.Errorf("a header holding redacted_from from 0.12: %v", err)
	}

	if _, err := Fork(s, ids[2], Header{}); !errors.Is(err, ErrNoRaise) {
		t.Errorf("Fork over the old redacted at %s = %v, want ErrNoRaise", Format, err)
	}
	if f, err := Fork(s, ids[0], Header{}); err != nil {
		t.Errorf("Fork before the old redacted: %v", err)
	} else if _, err := f.Append(NewItemEntry(openresponses.UserText("more"))); err != nil {
		t.Errorf("Append to that fork: %v", err)
	}
	if f, err := Fork(s, ids[2], Header{Format: "agentsession/0.12"}); err != nil {
		t.Errorf("Fork under 0.12: %v", err)
	} else if _, err := f.Append(NewItemEntry(openresponses.UserText("more"))); !errors.Is(err, ErrNoRaise) {
		t.Errorf("Append to a 0.12 fork over the old redacted = %v, want ErrNoRaise", err)
	}
}

// TestGatedThroughHelpersAndSessions: the mark a gated entry carries
// survives RewriteStrings and RemapRefs, so a session that takes the
// result by Commit may not be written at 0.13, and Append refuses it
// until the writer clears or marks it, the review's copy of a 0.12
// file into a new session among them.
func TestGatedThroughHelpersAndSessions(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	in, _ := hashedLines(t, "agentsession/0.12", msg, msg+`,"redacted":[1]`, `"type":"x:note","redacted":[2]`, msg)
	orig, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range orig.Entries()[1:3] {
		rw, err := RewriteStrings(e, strings.ToUpper)
		if err != nil {
			t.Fatal(err)
		}
		rm, err := RemapRefs(e, Remap{})
		if err != nil {
			t.Fatal(err)
		}
		for name, out := range map[string]Entry{"RewriteStrings": rw, "RemapRefs": rm.Entry} {
			out.Base().ID, out.Base().Parent = "", ""
			s := New(Header{Format: "agentsession/0.12"})
			if _, err := s.Commit(out); err != nil {
				t.Fatalf("entry %d, %s: Commit: %v", i+1, name, err)
			}
			if err := s.RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
				t.Errorf("entry %d, %s: RaiseRefused = %v, want ErrNoRaise", i+1, name, err)
			}
		}
	}

	// Copying the file's entries into a new session with Append stops
	// at the gated entry, which a 0.13 file would give a meaning, and
	// takes it once the writer has said what it means.
	cp := New(Header{})
	ids := map[string]string{}
	for i, e := range orig.Entries() {
		data, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		c, err := UnmarshalEntry(data)
		if err != nil {
			t.Fatal(err)
		}
		GateEntry(orig.DeclaredFormat(), c)
		c.Base().ID = ""
		c.Base().Parent = ids[e.Base().Parent]
		_, err = cp.Append(c)
		switch i {
		case 1, 2:
			if !errors.Is(err, ErrNoRaise) || !strings.Contains(err.Error(), "ClearRedacted") {
				t.Fatalf("Append of gated entry %d = %v, want ErrNoRaise naming ClearRedacted", i, err)
			}
			fix := ClearRedacted
			if i == 2 {
				fix = func(e Entry) error { return SetRedacted(e, []Span{{2, 2}}) }
			}
			if err := fix(c); err != nil {
				t.Fatal(err)
			}
			if _, err = cp.Append(c); err != nil {
				t.Fatalf("Append once marked: %v", err)
			}
		default:
			if err != nil {
				t.Fatal(err)
			}
		}
		ids[e.Base().ID] = c.Base().ID
	}
	if err := cp.RaiseRefused(Format); err != nil {
		t.Errorf("the copy: RaiseRefused = %v", err)
	}
	back, err := Read(strings.NewReader(writeFile(t, cp)))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := RedactedOf(back.Entries()[1]); ok {
		t.Error("the cleared entry reads as redacted")
	}
	if got, ok := RedactedOf(back.Entries()[2]); !ok || !slices.Equal(got, []Span{{2, 2}}) {
		t.Errorf("the marked entry reads back %v %v", got, ok)
	}
}

// TestForkFormats: a fork over a 0.12 origin whose gated entry is on a
// sibling branch, not on the fork's prefix, is taken at 0.13; a fork
// written under 0.12 over a 0.13 origin's redacted prefix entry is
// refused, since 0.12 would read the member as one it does not know.
func TestForkFormats(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	root := `{"id":"ROOT",` + `"type":"item","parent":null,"ts":"2026-09-17T16:00:01Z","item":{"type":"message","role":"user","content":[]}}`
	rid, _, err := EntryHashes([]byte(`{` + root[len(`{"id":"ROOT",`):]))
	if err != nil {
		t.Fatal(err)
	}
	line := func(body string) (string, string) {
		l := `{` + body + `,"parent":"` + rid + `","ts":"2026-09-17T16:00:02Z"}`
		id, _, err := EntryHashes([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		return `{"id":"` + id + `",` + l[1:], id
	}
	gated, _ := line(msg + `,"redacted":[1]`)
	sibling, sid := line(`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"other"}]}`)
	head := `{"type":"session","format":"agentsession/0.12","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
	file := head + "\n" + strings.Replace(root, "ROOT", rid, 1) + "\n" + gated + "\n" + sibling + "\n"
	origin, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	if err := origin.RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
		t.Fatalf("the origin: RaiseRefused = %v", err)
	}
	if f, err := Fork(origin, sid, Header{}); err != nil {
		t.Errorf("Fork at the sibling: %v", err)
	} else if err := f.RaiseRefused(Format); err != nil {
		t.Errorf("the sibling's fork: RaiseRefused = %v", err)
	}

	in, ids := hashedLines(t, Format, msg+`,"redacted":[1]`, msg)
	s13, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Fork(s13, ids[1], Header{Format: "agentsession/0.12"}); !errors.Is(err, ErrUnsupportedFormat) {
		t.Errorf("Fork under 0.12 over a redacted prefix entry = %v, want ErrUnsupportedFormat", err)
	}
	if _, err := Fork(s13, ids[1], Header{}); err != nil {
		t.Errorf("Fork at 0.13: %v", err)
	}
}

// TestGateOtherForms: under 0.12 a member in another form is unknown
// too, and forbids the raise: on a core entry, on an extension entry,
// spelled with an escape in the key, and through Extend. A valid member
// spelled in a non-canonical form is kept as written.
func TestGateOtherForms(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	for name, body := range map[string]string{
		"malformed, core":             msg + `,"redacted":["x"]`,
		"malformed, extension":        `"type":"x:note","redacted":0`,
		"escaped key, extension":      `"type":"x:note","redacted":"x"`,
		"non-canonical, core":         msg + `,"redacted":[3.0]`,
		"non-canonical, extension":    `"type":"x:note","redacted":[1e0]`,
		"malformed escaped key, core": msg + `,"redacted":{}`,
	} {
		t.Run(name, func(t *testing.T) {
			in, _ := hashedLines(t, "agentsession/0.12", body)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			e := s.Entries()[0]
			if _, ok := RedactedOf(e); ok {
				t.Error("reads as redacted")
			}
			if err := s.RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
				t.Errorf("RaiseRefused = %v, want ErrNoRaise", err)
			}
			if f := s.Header().Format; f != "agentsession/0.12" {
				t.Errorf("held at %s", f)
			}
			if out, want := writeFile(t, s), canonicalFile(t, in); out != want {
				t.Errorf("rewrite\n got %s want %s", out, want)
			}
		})
	}
	// Kept as written: [3.0] stays [3.0] in Unknown.
	in, _ := hashedLines(t, "agentsession/0.12", msg+`,"redacted":[3.0]`)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if raw := s.Entries()[0].Base().Unknown["redacted"]; string(raw) != `[3.0]` {
		t.Errorf("kept %s, want [3.0]", raw)
	}

	// Through Extend: a malformed member arriving at a 0.12 session.
	in, ids := hashedLines(t, "agentsession/0.12", msg)
	s, err = Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"x:note","parent":"` + ids[0] + `","ts":"2026-09-17T16:00:02Z","redacted":"x"}`
	id, _, err := EntryHashes([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	e, err := UnmarshalEntry([]byte(`{"id":"` + id + `",` + line[1:]))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Extend(e); err != nil {
		t.Fatal(err)
	}
	if err := s.RaiseRefused(Format); !errors.Is(err, ErrNoRaise) {
		t.Errorf("after Extend: RaiseRefused = %v, want ErrNoRaise", err)
	}
	if f := s.Header().Format; f != "agentsession/0.12" {
		t.Errorf("after Extend the header is %s", f)
	}
}
