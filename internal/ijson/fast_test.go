package ijson

import (
	"strings"
	"testing"
)

var fastSeeds = []string{
	`{"b":1,"a":[true,false,null,"x"]}`,
	`{"a":1.5,"b":-0.25,"c":0,"d":-0,"e":123456789012345}`,
	`{"a":1.0}`,
	`9007199254740993`,
	`9007199254740992`,
	`1152921504606847000`,
	`1e400`,
	`1e20`,
	`0.1`,
	`{"a":1,"a":2}`,
	`{"a":1,"a":2}`,
	`{"k0":0,"k1":1,"k2":2,"k3":3,"k4":4,"k5":5,"k6":6,"k7":7,"k8":8,"k9":9,"k10":10,"k11":11,"k12":12,"k13":13,"k14":14,"k15":15,"k16":16,"k3":3}`,
	`"\ud800"`,
	`"😀"`,
	`"\udc00"`,
	`"` + "\xff" + `"`,
	`[1,]`,
	`1 2`,
	``,
	`   `,
	`{"a":1}x`,
	strings.Repeat("[", 1200) + strings.Repeat("]", 1200),
	`" \n "`,
}

// TestCheckFast: whatever the byte-level path passes, the tokenizing
// check passes.
func TestCheckFast(t *testing.T) {
	for _, s := range fastSeeds {
		checkAgrees(t, []byte(s))
	}
	for _, s := range []string{
		`{"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it\n"}]},"n":1.25}`,
	} {
		if !checkFast([]byte(s)) {
			t.Errorf("fast path declined %s", s)
		}
	}
}

func checkAgrees(t *testing.T, data []byte) {
	t.Helper()
	if !checkFast(data) {
		return
	}
	if err := checkTokens(data); err != nil {
		t.Fatalf("%q: fast path passed it, the full check: %v", data, err)
	}
}

func FuzzCheckFast(f *testing.F) {
	for _, s := range fastSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		checkAgrees(t, data)
	})
}
