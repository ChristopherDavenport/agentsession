package agentsession

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/openresponses"
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
		// One U+FFFD per maximal subpart, as the WHATWG decoder does: a
		// truncated two-byte and a truncated four-byte sequence are one
		// each, and a surrogate encoded in UTF-8 is three.
		{"maximal subpart", "{\"s\":\"x\xe2\x82y\"}", `{"s":"x` + "�" + `y"}`, []Normalisation{{At: "/s", Raw: "Injignki"}}},
		{"truncated four-byte", "{\"s\":\"\xf0\x9f\x98\"}", `{"s":"` + "�" + `"}`, []Normalisation{{At: "/s", Raw: "IvCfmCI="}}},
		{"utf-8 surrogate", "{\"s\":\"\xed\xa0\xbd\"}", `{"s":"` + "���" + `"}`, []Normalisation{{At: "/s", Raw: "Iu2gvSI="}}},
		// From 2^53 the canonical rendering of a double may not be exact
		// and is admissible anyway, so a 64-bit id rounds to it.
		{"64-bit id", `{"n":1234567890123456789}`, `{"n":1234567890123456800}`, []Normalisation{{At: "/n", Was: "1234567890123456789"}}},
		{"2^60 as written", `{"n":1152921504606846976}`, `{"n":1152921504606846976}`, nil},
		{"large whole number", `{"n":123456789012345678901234567890}`, `{"n":1.2345678901234568e+29}`, []Normalisation{{At: "/n", Was: "123456789012345678901234567890"}}},
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
		"bytes":     {{At: "/item/content/0/text", Raw: base64.StdEncoding.EncodeToString([]byte("\"bytes \xe2\x82 end\""))}},
	}
	seen := 0
	for _, e := range s.Entries() {
		var name string
		switch v := e.(type) {
		case *InfoEntry:
			name = v.Name
			// 2^60 is written as its canonical rendering, which is
			// what a reader decodes, with nothing to record.
			if got := string(v.Unknown["acme:pow60"]); got != "1152921504606847000" {
				t.Errorf("acme:pow60 read back as %s, want 1152921504606847000", got)
			}
		case *ResponseEntry:
			name = "typed"
		case *CustomEntry:
			name = "surrogate"
		case *ItemEntry:
			if len(v.Normalised) > 0 {
				name = "bytes"
			}
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

// TestLargeWholeNumbersRoundTrip: a whole number from 2^53 that
// binary64 holds exactly is written in a canonical form whose exact
// value may not be a double, 2^60 as 1152921504606847000, and the rule
// admits that rendering, so what Append accepts Read accepts back.
func TestLargeWholeNumbersRoundTrip(t *testing.T) {
	s := New(Header{})
	for _, lit := range []string{"1152921504606846976", "9223372036854775808", "1000000000000000000000", "1234567890123456789"} {
		e := &InfoEntry{Name: lit}
		e.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(lit)}
		if _, err := s.Append(e); err != nil {
			t.Fatalf("Append %s: %v", lit, err)
		}
	}
	// math.MaxInt64 rounds to 2^63, which int64 cannot hold, so the
	// entry cannot carry what its line would say and is refused; the
	// band is the last 512 values of the type.
	r := &ResponseEntry{ResponseID: "r", LatencyMS: 9223372036854775807}
	if _, err := s.Append(r); err == nil || !strings.Contains(err.Error(), "does not fit") {
		t.Errorf("Append max int64 = %v, want a refusal naming the type", err)
	}
	// 2^63-514 rounds to the double 2^63-1024, whose canonical
	// rendering is 9223372036854775000; the entry holds that value,
	// which is what any reader of the line decodes.
	r = &ResponseEntry{ResponseID: "r", LatencyMS: 9223372036854775807 - 513}
	if _, err := s.Append(r); err != nil || r.LatencyMS != 9223372036854775000 {
		t.Errorf("Append 2^63-514 = %v, latency %d; want 9223372036854775000", err, r.LatencyMS)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(strings.NewReader(buf.String())); err != nil {
		t.Errorf("Read of what Write emitted: %v\n%s", err, buf.String())
	}
}

// TestAppendRepairsGoStrings: a Go string that is not valid UTF-8, which
// the marshaller would repair silently, is repaired in place and the
// bytes recorded under the pointer of the member it became.
func TestAppendRepairsGoStrings(t *testing.T) {
	s := New(Header{})
	e := NewItemEntry(openresponses.UserText("cut \xe2\x82 and \xff"))
	if _, err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	want := []Normalisation{{At: "/item/content/0/text", Raw: base64.StdEncoding.EncodeToString([]byte("\"cut \xe2\x82 and \xff\""))}}
	if !reflect.DeepEqual(e.Normalised, want) {
		t.Errorf("Normalised = %+v, want %+v", e.Normalised, want)
	}
	if got := e.Item.(*openresponses.Message).Content[0].(*openresponses.InputText).Text; got != "cut � and �" {
		t.Errorf("text = %q after repair", got)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(strings.NewReader(buf.String())); err != nil {
		t.Errorf("Read: %v", err)
	}
}

// TestCallerNormalisedIsSortedAndChecked: a list the caller set is
// written in the order the format fixes even when nothing needed
// rewriting, and a malformed element is refused.
func TestCallerNormalisedIsSortedAndChecked(t *testing.T) {
	s := New(Header{})
	e := &InfoEntry{Name: "n"}
	e.Normalised = []Normalisation{{At: "/zzz", Was: "1"}, {At: "/aaa", Was: "2"}}
	if _, err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if e.Normalised[0].At != "/aaa" {
		t.Errorf("Normalised = %+v, want sorted by at", e.Normalised)
	}
	// The line hashed is the line written: the sort happens before
	// either, or Read would refuse the id.
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(strings.NewReader(buf.String())); err != nil {
		t.Errorf("Read of a caller's unsorted list written back: %v", err)
	}
	for name, bad := range map[string]Normalisation{
		"no pointer": {At: "zzz", Was: "1"},
		"neither":    {At: "/a"},
		"both":       {At: "/a", Was: "1", Raw: "MQ=="},
	} {
		e := &InfoEntry{Name: "n"}
		e.Normalised = []Normalisation{bad}
		if _, err := s.Append(e); !errors.Is(err, ErrBadNormalisation) {
			t.Errorf("%s: err = %v, want ErrBadNormalisation", name, err)
		}
	}
}

// TestFailedAppendLeavesTheEntryUntouched: when an append is refused,
// every string is back as the caller had it, neither repaired nor
// holding a sentinel, so a retry after fixing the refusal still
// records the repair.
func TestFailedAppendLeavesTheEntryUntouched(t *testing.T) {
	s := New(Header{})
	e := &InfoEntry{Name: "n\xff"}
	e.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(`1e400`)}
	if _, err := s.Append(e); err == nil {
		t.Fatal("Append of 1e400 succeeded")
	}
	if e.Name != "n\xff" {
		t.Errorf("name = %q after a refused append, want the original bytes", e.Name)
	}
	delete(e.Unknown, "acme:n")
	if _, err := s.Append(e); err != nil {
		t.Fatal(err)
	}
	if want := []Normalisation{{At: "/name", Raw: base64.StdEncoding.EncodeToString([]byte("\"n\xff\""))}}; !reflect.DeepEqual(e.Normalised, want) {
		t.Errorf("Normalised = %+v, want %+v", e.Normalised, want)
	}
	// A refusal from inside the walk, a member name that is not valid
	// UTF-8, restores what was already replaced.
	env := &EnvEntry{CWD: "/tmp/\xff", Tools: map[string]string{"bad\xff": "x"}}
	if _, err := s.Append(env); err == nil {
		t.Fatal("Append with a bad member name succeeded")
	}
	if env.CWD != "/tmp/\xff" {
		t.Errorf("cwd = %q after a refused append, want the original bytes", env.CWD)
	}
	// A rounding the type cannot hold beside a bad string: both back.
	r := &ResponseEntry{ResponseID: "r\xff", LatencyMS: 9223372036854775807}
	if _, err := s.Append(r); err == nil {
		t.Fatal("Append of max int64 succeeded")
	}
	if r.ResponseID != "r\xff" || r.LatencyMS != 9223372036854775807 {
		t.Errorf("entry = %q/%d after a refused append, want the originals", r.ResponseID, r.LatencyMS)
	}
	// A string a custom marshaller leaves out is never written: nothing
	// to record, the entry keeps it, and the append succeeds.
	out := NewItemEntry(&openresponses.FunctionCallOutput{CallID: "c", Output: openresponses.FunctionCallOutputData{Text: "t\xff", Parts: openresponses.Contents{&openresponses.InputText{Text: "ok"}}}})
	if _, err := s.Append(out); err != nil {
		t.Fatalf("Append with an unmarshalled bad string: %v", err)
	}
	if len(out.Normalised) != 0 {
		t.Errorf("Normalised = %+v for a string that was not written", out.Normalised)
	}
}

// TestCallerNormalisedIsCheckedDeeply: a was must be JSON text, a raw
// padded standard base64, and a pointer recorded once.
func TestCallerNormalisedIsCheckedDeeply(t *testing.T) {
	s := New(Header{})
	for name, bad := range map[string][]Normalisation{
		"raw not base64": {{At: "/x", Raw: "not base64!"}},
		"raw unpadded":   {{At: "/x", Raw: "MQ"}},
		"was not json":   {{At: "/x", Was: "not json"}},
		"was bad utf-8":  {{At: "/x", Was: "\"\xff\""}},
		"twice":          {{At: "/x", Was: "1"}, {At: "/x", Was: "2"}},
		"at bad utf-8":   {{At: "/\xff", Was: "1"}},
		"at bad escape":  {{At: "/~2", Was: "1"}},
		"raw newline":    {{At: "/x", Raw: "MQ==\n"}},
		"raw pad bits":   {{At: "/x", Raw: "MR=="}},
		"was whitespace": {{At: "/x", Was: " 1 "}},
	} {
		e := &InfoEntry{Name: "n"}
		e.Normalised = bad
		if _, err := s.Append(e); !errors.Is(err, ErrBadNormalisation) {
			t.Errorf("%s: err = %v, want ErrBadNormalisation", name, err)
		}
	}
}

// TestRefusedAppendKeepsTheEnvelope: a refusal leaves the parent, the
// timestamp and a caller-set id as they were, so a retry continues
// from the leaf at the time of the retry.
func TestRefusedAppendKeepsTheEnvelope(t *testing.T) {
	s := New(Header{})
	a := appendText(t, s, "a")
	e := &InfoEntry{Name: "n"}
	e.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(`1e400`)}
	if _, err := s.Append(e); err == nil {
		t.Fatal("Append of 1e400 succeeded")
	}
	if e.Parent != "" || !e.Timestamp.IsZero() {
		t.Errorf("refused entry has parent %q and ts %v, want neither", e.Parent, e.Timestamp)
	}
	b := appendText(t, s, "b")
	delete(e.Unknown, "acme:n")
	r, err := s.Commit(e)
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != Continued || e.Parent != b || a == b {
		t.Errorf("retry: outcome %v under %s, want Continued under %s", r.Outcome, e.Parent, b)
	}
	// The caller's own parents slice, which the check sorts in place,
	// is back in the caller's order and is still the caller's slice;
	// an unknown entry's raw line is back too.
	callers := []EntryRef{{Entry: b}, {Entry: a}}
	bad := &InfoEntry{Name: "n"}
	bad.ID, bad.Parents = "sha256:deadbeef", callers
	bad.Unknown = map[string]json.RawMessage{"acme:n": json.RawMessage(`9007199254740993`)}
	if _, err := s.Append(bad); !errors.Is(err, ErrBadID) {
		t.Fatalf("Append with a wrong id = %v", err)
	}
	if &bad.Parents[0] != &callers[0] || callers[0].Entry != b || bad.ID != "sha256:deadbeef" {
		t.Errorf("after ErrBadID: parents %v (same slice %v), id %q", callers, &bad.Parents[0] == &callers[0], bad.ID)
	}
	u := &UnknownEntry{Type: "acme:thing", Raw: json.RawMessage(`{"type":"acme:thing","x":1}`)}
	u.ID = "sha256:deadbeef"
	if _, err := s.Append(u); !errors.Is(err, ErrBadID) {
		t.Fatalf("Append of an unknown entry with a wrong id = %v", err)
	}
	if string(u.Raw) != `{"type":"acme:thing","x":1}` {
		t.Errorf("raw = %s after a refused append", u.Raw)
	}
}

// TestSentinelCannotCollide: a string that spells the sentinel's prefix
// is a string like any other, since each call draws a nonce.
func TestSentinelCannotCollide(t *testing.T) {
	s := New(Header{})
	r := &ResponseEntry{ResponseID: "\x00\x01agentsession:normalise:0\x01\x00", Model: "m\xff"}
	if _, err := s.Append(r); err != nil {
		t.Fatal(err)
	}
	if r.ResponseID != "\x00\x01agentsession:normalise:0\x01\x00" || r.Model != "m\ufffd" {
		t.Errorf("entry = %q/%q after append", r.ResponseID, r.Model)
	}
	if len(r.Normalised) != 1 || r.Normalised[0].At != "/model" {
		t.Errorf("Normalised = %+v, want the model alone", r.Normalised)
	}
}
