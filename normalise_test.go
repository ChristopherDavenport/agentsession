package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
)

// TestNormaliseJSON covers the rewrite the format has a writer make
// and the record it keeps of it.
func TestNormaliseJSON(t *testing.T) {
	tests := []struct {
		name string
		in   string
		out  string
		want []Normalisation
	}{
		{"digits", `{"n":9007199254740993}`, `{"n":9007199254740992}`, []Normalisation{{At: "/n", Was: "9007199254740993"}}},
		{"fraction spelling", `{"n":9007199254740993.0}`, `{"n":9007199254740992}`, []Normalisation{{At: "/n", Was: "9007199254740993.0"}}},
		{"exponent spelling", `{"n":9.007199254740993e15}`, `{"n":9007199254740992}`, []Normalisation{{At: "/n", Was: "9.007199254740993e15"}}},
		{"representable", `{"a":9007199254740992,"b":1e20,"c":0.1,"d":-0}`, `{"a":9007199254740992,"b":1e20,"c":0.1,"d":-0}`, nil},
		{"lone high surrogate", `{"s":"a\ud83db"}`, `{"s":"a�b"}`, []Normalisation{{At: "/s", Was: `"a\ud83db"`}}},
		{"lone low surrogate", `{"s":"\ude00"}`, `{"s":"�"}`, []Normalisation{{At: "/s", Was: `"\ude00"`}}},
		{"pair kept", `{"s":"😀"}`, `{"s":"😀"}`, nil},
		{"escaped backslash before u", `{"s":"\\ud83d"}`, `{"s":"\\ud83d"}`, nil},
		{"invalid utf-8", "{\"s\":\"a\xffb\"}", `{"s":"a` + "�" + `b"}`, []Normalisation{{At: "/s", Raw: "ImH/YiI="}}},
		{"invalid utf-8 and a surrogate", "{\"s\":\"\xff\\ud83d\"}", `{"s":"` + "�" + `�"}`, []Normalisation{{At: "/s", Raw: "Iv9cdWQ4M2Qi"}}},
		{"nested with escaped pointer", `{"a/b":[1,{"~":9007199254740993}]}`, `{"a/b":[1,{"~":9007199254740992}]}`, []Normalisation{{At: "/a~1b/1/~0", Was: "9007199254740993"}}},
		{"array indexes", `[9007199254740993,"x",9007199254740993]`, `[9007199254740992,"x",9007199254740992]`, []Normalisation{{At: "/0", Was: "9007199254740993"}, {At: "/2", Was: "9007199254740993"}}},
		{"whitespace kept", `{ "n" : 9007199254740993 , "m" : 1 }`, `{ "n" : 9007199254740992 , "m" : 1 }`, []Normalisation{{At: "/n", Was: "9007199254740993"}}},
		// Sorted by pointer as UTF-16 code units: U+1D11E is a
		// surrogate pair starting D834, which sorts before U+FF5E.
		{"utf-16 order", `{"～":9007199254740993,"𝄞":9007199254740993,"b":9007199254740993}`,
			`{"～":9007199254740992,"𝄞":9007199254740992,"b":9007199254740992}`,
			[]Normalisation{{At: "/b", Was: "9007199254740993"}, {At: "/\U0001D11E", Was: "9007199254740993"}, {At: "/～", Was: "9007199254740993"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, changes, err := NormaliseJSON([]byte(tt.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != tt.out {
				t.Errorf("out = %s, want %s", out, tt.out)
			}
			if !reflect.DeepEqual(changes, tt.want) {
				t.Errorf("changes = %+v, want %+v", changes, tt.want)
			}
			if err := ijson.Check(out); err != nil {
				t.Errorf("output is not I-JSON: %v", err)
			}
		})
	}
	for name, in := range map[string]string{
		"not finite":      `{"n":1e400}`,
		"repeated member": `{"a":1,"a":2}`,
		"bad member name": `{"\ud83d":1}`,
		"not json":        `{"a":`,
		// From 1e21 the rounding's canonical form is an exponent whose
		// exact value fails the test, so there is nothing to rewrite
		// to; see TestAppendRefusesWhatCanonicalFormCannotWrite.
		"beyond 1e21": `{"n":123456789012345678901234567890}`,
	} {
		if _, _, err := NormaliseJSON([]byte(in)); !errors.Is(err, ijson.ErrNotIJSON) {
			t.Errorf("%s: err = %v, want ErrNotIJSON", name, err)
		}
	}
}

// TestAppendNormalises: Append rewrites a body the way the format has a
// writer do, the caller's entry holds the rewrite, the record survives
// a write and a read, and the line verifies.
func TestAppendNormalises(t *testing.T) {
	s := New(Header{})
	e := &InfoEntry{Name: "n"}
	e.Unknown = map[string]json.RawMessage{"acme:seed": json.RawMessage(`9007199254740993`), "acme:text": json.RawMessage(`"\ud83d"`)}
	id, err := s.Append(e)
	if err != nil {
		t.Fatal(err)
	}
	want := []Normalisation{{At: "/acme:seed", Was: "9007199254740993"}, {At: "/acme:text", Was: `"\ud83d"`}}
	if !reflect.DeepEqual(e.Normalised, want) {
		t.Errorf("Normalised = %+v, want %+v", e.Normalised, want)
	}
	if got := string(e.Unknown["acme:seed"]); got != "9007199254740992" {
		t.Errorf("acme:seed = %s after normalisation, want 9007199254740992", got)
	}
	if got := string(e.Unknown["acme:text"]); got != `"�"` && got != `"`+"�"+`"` {
		t.Errorf("acme:text = %s after normalisation, want U+FFFD", got)
	}
	if e.ID != id || e.ContentHash() == "" {
		t.Errorf("the entry holds id %q and content %q after append %s", e.ID, e.ContentHash(), id)
	}

	// A number in a member the profile types as a number is rounded in
	// place, and a caller's own record is kept beside the library's.
	r := &ResponseEntry{ResponseID: "r", LatencyMS: 9007199254740993}
	r.Normalised = []Normalisation{{At: "/usage/input_tokens", Was: "9007199254740993"}}
	if _, err := s.Append(r); err != nil {
		t.Fatal(err)
	}
	if r.LatencyMS != 9007199254740992 {
		t.Errorf("latency_ms = %d, want the rounding", r.LatencyMS)
	}
	want = []Normalisation{{At: "/latency_ms", Was: "9007199254740993"}, {At: "/usage/input_tokens", Was: "9007199254740993"}}
	if !reflect.DeepEqual(r.Normalised, want) {
		t.Errorf("Normalised = %+v, want %+v", r.Normalised, want)
	}

	// A custom entry's data is a passthrough, and the pointer reaches
	// into it.
	c := &CustomEntry{NS: "acme", Data: json.RawMessage(`{"big":[9007199254740993]}`)}
	if _, err := s.Append(c); err != nil {
		t.Fatal(err)
	}
	if want := []Normalisation{{At: "/data/big/0", Was: "9007199254740993"}}; !reflect.DeepEqual(c.Normalised, want) {
		t.Errorf("Normalised = %+v, want %+v", c.Normalised, want)
	}

	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "9007199254740993}") || strings.Contains(buf.String(), `\ud83d"`) {
		t.Errorf("the file carries an unnormalised value:\n%s", buf.String())
	}
	back, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	got := back.Entries()
	if len(got) != 3 {
		t.Fatalf("%d entries read back", len(got))
	}
	if !reflect.DeepEqual(got[0].Base().Normalised, e.Normalised) || got[0].Base().ID != id {
		t.Errorf("read back %+v as %s, want %+v as %s", got[0].Base().Normalised, got[0].Base().ID, e.Normalised, id)
	}
	// The record is in the content hash and not in the context hash,
	// which strips it: two writers that normalised differently agree
	// on the cache key.
	if h, err := back.ContextHash(id); err != nil || h == "" {
		t.Errorf("ContextHash = %q, %v", h, err)
	}
}

// TestNormalisedFixture reads the conformance fixture: 9007199254740993
// in three spellings and once in a member the profile types as a
// number, and a lone surrogate with its was. Every line verifies, since
// Read checks each id.
func TestNormalisedFixture(t *testing.T) {
	s := loadFixture(t, "normalised")
	want := map[string][]Normalisation{
		"spellings": {{At: "/acme:digits", Was: "9007199254740993"}, {At: "/acme:exponent", Was: "9.007199254740993e15"}, {At: "/acme:fraction", Was: "9007199254740993.0"}},
		"typed":     {{At: "/latency_ms", Was: "9007199254740993"}},
		"surrogate": {{At: "/data/text", Was: `"cut \ud83d"`}},
	}
	seen := 0
	for _, e := range s.Entries() {
		var name string
		switch v := e.(type) {
		case *InfoEntry:
			name = v.Name
		case *ResponseEntry:
			name = "typed"
		case *CustomEntry:
			name = "surrogate"
		}
		w, ok := want[name]
		if !ok {
			continue
		}
		seen++
		if !reflect.DeepEqual(e.Base().Normalised, w) {
			t.Errorf("%s: normalised = %+v, want %+v", name, e.Base().Normalised, w)
		}
	}
	if seen != len(want) {
		t.Errorf("found %d of %d normalised entries", seen, len(want))
	}
}

// TestAppendRefusesWhatCanonicalFormCannotWrite: a whole number from
// 1e21 that binary64 holds exactly passes the I-JSON test as written
// and fails it in the canonical form Write emits, since ES6 renders it
// as an exponent whose exact value is not the double's. Append refuses
// it rather than write a line Read would refuse.
func TestAppendRefusesWhatCanonicalFormCannotWrite(t *testing.T) {
	s := New(Header{})
	e := &InfoEntry{Name: "n"}
	e.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(`1180591620717411303424`)} // 2^70
	if _, err := s.Append(e); !errors.Is(err, ijson.ErrNotIJSON) || !strings.Contains(err.Error(), "canonical form") {
		t.Errorf("Append of 2^70 = %v, want a refusal naming the canonical form", err)
	}
	ok := &InfoEntry{Name: "n"}
	ok.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(`1000000000000000000000`)} // 1e21, canonical "1e+21", exact
	if _, err := s.Append(ok); err != nil {
		t.Errorf("Append of 1e21 = %v", err)
	}
}
