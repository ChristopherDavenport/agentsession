package jsonx

import (
	"encoding/json"
	"reflect"
	"testing"
)

var memberSeeds = []string{
	`{}`,
	` { "a" : 1 , "b" : [1, {"c": "}"}], "d":"\"}" } `,
	`{"a":1,"a":2}`,
	`{"a":1}`,
	`{"a":"` + "\xff" + `"}`,
	`{"` + "\xff" + `":1}`,
	`null`,
	`[1]`,
	`{"a":1}x`,
	`{"a":true,"b":false,"c":null,"d":-1.5e3,"e":{}}`,
}

// TestMembers: wherever Members answers, it answers as the decoder does.
func TestMembers(t *testing.T) {
	for _, s := range memberSeeds {
		membersAgree(t, []byte(s))
	}
	if _, ok := Members([]byte(`{"type":"item","item":{"text":"a\nb"},"n":1}`)); !ok {
		t.Error("Members declined a plain object")
	}
}

func membersAgree(t *testing.T, data []byte) {
	t.Helper()
	fast, ok := Members(data)
	if !ok {
		return
	}
	var slow map[string]json.RawMessage
	if err := json.Unmarshal(data, &slow); err != nil {
		t.Fatalf("%q: Members split it, the decoder refused: %v", data, err)
	}
	if slow == nil || !reflect.DeepEqual(fast, slow) {
		t.Fatalf("%q: Members %q, decoder %q", data, fast, slow)
	}
}

func FuzzMembers(f *testing.F) {
	for _, s := range memberSeeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		membersAgree(t, data)
	})
}
