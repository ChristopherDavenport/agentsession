package ijson

import "testing"

func TestCheckNumber(t *testing.T) {
	for _, tc := range []struct {
		lit string
		ok  bool
	}{
		{"0", true}, {"0.1", true}, {"-1.5e300", false}, {"-1.5e3", true}, {"1e20", true},
		{"9007199254740992", true}, {"9007199254740992.0", true}, {"9.007199254740992e15", true},
		{"9007199254740993", false}, {"9007199254740993.0", false}, {"9.007199254740993e15", false},
		{"1e400", false}, {"1e21", true}, {"123456789012345678901234567890", false},
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
