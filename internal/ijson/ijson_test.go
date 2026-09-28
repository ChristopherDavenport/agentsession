package ijson

import "testing"

func TestCheckNumber(t *testing.T) {
	for _, tc := range []struct {
		lit string
		ok  bool
	}{
		// -1.5e300 is a whole number no double holds, and the exact
		// value of the canonical rendering of the nearest double, so
		// the second arm of the rule admits it.
		{"0", true}, {"0.1", true}, {"-1.5e300", true}, {"-1.5e3", true}, {"1e20", true},
		{"9007199254740992", true}, {"9007199254740992.0", true}, {"9.007199254740992e15", true},
		{"9007199254740993", false}, {"9007199254740993.0", false}, {"9.007199254740993e15", false},
		{"1e400", false}, {"1e21", true}, {"123456789012345678901234567890", false},
		// The canonical rendering of a double is admissible: 2^60 and
		// what ES6 writes for it, a 64-bit id's rounding, an exponent.
		{"1152921504606846976", true}, {"1152921504606847000", true},
		{"1234567890123456789", false}, {"1234567890123456800", true},
		{"9223372036854775807", false}, {"9223372036854776000", true},
		{"1.2345678901234568e+29", true}, {"123456789012345680000", true},
	} {
		err := CheckNumber(tc.lit)
		if (err == nil) != tc.ok {
			t.Errorf("CheckNumber(%s) = %v, want ok=%v", tc.lit, err, tc.ok)
		}
	}
}

func TestCheck(t *testing.T) {
	for _, tc := range []struct {
		doc string
		ok  bool
	}{
		{`{"a":1,"b":[0.1,"x"]}`, true},
		{`{"a":1,"a":2}`, false},
		{`{"o":{"x":1},"p":{"x":1}}`, true},
		{`["😀"]`, true},
		{`["\ud83d"]`, false},
		{`["\ude00"]`, false},
		{`["\\ud83d"]`, true},
		{`{"n":9007199254740993}`, false},
		{`{"n":1e20}`, true},
		{"[\"\xff\"]", false},
	} {
		err := Check([]byte(tc.doc))
		if (err == nil) != tc.ok {
			t.Errorf("Check(%s) = %v, want ok=%v", tc.doc, err, tc.ok)
		}
	}
}

func TestCheckNested(t *testing.T) {
	for _, doc := range []string{
		`{"id":"x","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"parent":null,"ts":"t","type":"item"}`,
		`{"a":{"b":1},"c":{"b":2},"d":[{"e":1},{"e":2}],"f":"g"}`,
		`[{"a":1},{"a":1}]`,
	} {
		if err := Check([]byte(doc)); err != nil {
			t.Errorf("Check(%s) = %v", doc, err)
		}
	}
	for _, doc := range []string{
		`{"a":{"b":1,"b":2}}`,
		`{"a":[1],"a":[2]}`,
		`{"a":{"x":1},"a":2}`,
	} {
		if err := Check([]byte(doc)); err == nil {
			t.Errorf("Check(%s) accepted a repeated member", doc)
		}
	}
}
