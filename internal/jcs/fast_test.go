package jcs

import (
	"bytes"
	"strings"
	"testing"
)

var fastSeeds = []string{
	`{"b":1,"a":[true,false,null,"x"]}`,
	`{"z":{"y":{"x":1.5e3}},"a":-0,"m":0.000001,"n":1e21,"o":123456789012345678}`,
	` { "a" : "é\n\t\"\\\/\b\f\r\u0001" } `,
	`{"€":1,"€":2}`,
	`{"😀":1,"￿":2,"😀x":3}`,
	`"\ud800"`,
	`"\udc00\ud800"`,
	`[1,2,3,[4,[5,{}]],[]]`,
	`{"a":1,"a":2}`,
	`{"b":1,"a":2,"b":3}`,
	`01`,
	`1.`,
	`-`,
	`1e999`,
	`"` + "\xff" + `"`,
	`{"a":1}x`,
	`[1,]`,
	strings.Repeat("[", 1200) + strings.Repeat("]", 1200),
	`{"k":"` + strings.Repeat("\\n", 100) + `"}`,
	`9007199254740993`,
	`100000000000000`,
	`1000000000000000`,
	`-0.0`,
	`1E+2`,
}

// TestTransformFast: the byte-level path agrees with the decoder's
// wherever it answers.
func TestTransformFast(t *testing.T) {
	for _, s := range fastSeeds {
		checkFast(t, []byte(s))
	}
	// The common shapes take the fast path.
	for _, s := range []string{
		`{"b":{"d":[1,"x\n"],"c":null},"a":"é"}`,
		`{"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it\n"}]}}`,
	} {
		if _, ok := transformFast([]byte(s)); !ok {
			t.Errorf("fast path declined %s", s)
		}
	}
}

func checkFast(t *testing.T, data []byte) {
	t.Helper()
	fast, ok := transformFast(data)
	if !ok {
		return
	}
	slow, err := transformDecoded(data)
	if err != nil {
		t.Fatalf("%q: fast path gave %q, decoder refused: %v", data, fast, err)
	}
	if !bytes.Equal(fast, slow) {
		t.Fatalf("%q: fast %q, decoder %q", data, fast, slow)
	}
}

func FuzzTransformFast(f *testing.F) {
	for _, s := range fastSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkFast(t, data)
	})
}
