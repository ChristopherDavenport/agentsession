package jsonx

import (
	"encoding/json"
	"reflect"
	"testing"
)

type sample struct {
	A     string `json:"a"`
	B     int    `json:"b,omitempty"`
	Skip  string `json:"-"`
	Plain string
	embedded
}

type embedded struct {
	C bool `json:"c"`
}

func TestKeys(t *testing.T) {
	got := Keys[sample]()
	want := map[string]bool{"a": true, "b": true, "Plain": true, "c": true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Keys = %v, want %v", got, want)
	}
	if got := Keys[*sample](); !reflect.DeepEqual(got, want) {
		t.Errorf("Keys[*T] = %v", got)
	}
	if got := Keys[int](); len(got) != 0 {
		t.Errorf("Keys[int] = %v", got)
	}
}

func TestJoinObjects(t *testing.T) {
	extra := map[string]json.RawMessage{"z": json.RawMessage(`1`), "a": json.RawMessage(`"<x>"`)}
	tests := []struct {
		head, body string
		extra      map[string]json.RawMessage
		want       string
	}{
		{`{"t":1}`, `{"u":2}`, nil, `{"t":1,"u":2}`},
		{`{"t":1}`, `{}`, nil, `{"t":1}`},
		{`{}`, `{"u":2}`, nil, `{"u":2}`},
		{`{}`, `{}`, nil, `{}`},
		{`{"t":1}`, `{"u":2}`, extra, `{"t":1,"u":2,"a":"<x>","z":1}`},
		{`{}`, `{}`, extra, `{"a":"<x>","z":1}`},
	}
	for _, tt := range tests {
		got := JoinObjects([]byte(tt.head), []byte(tt.body), tt.extra)
		if string(got) != tt.want {
			t.Errorf("JoinObjects(%s, %s) = %s, want %s", tt.head, tt.body, got, tt.want)
		}
		if !json.Valid(got) {
			t.Errorf("JoinObjects produced invalid JSON %s", got)
		}
	}
}

func TestMarshalNoEscape(t *testing.T) {
	got, err := MarshalNoEscape(map[string]string{"h": "<a>&"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"h":"<a>&"}` {
		t.Errorf("MarshalNoEscape = %s", got)
	}
	if _, err := MarshalNoEscape(make(chan int)); err == nil {
		t.Error("MarshalNoEscape accepted a channel")
	}
}

func TestUnknownRoundTrip(t *testing.T) {
	in := `{"a":"x","c":true,"future":{"k":[1]},"Plain":"p"}`
	var v sample
	unknown, err := UnmarshalWithUnknown([]byte(in), &v, Keys[sample]())
	if err != nil {
		t.Fatal(err)
	}
	if string(unknown["future"]) != `{"k":[1]}` || len(unknown) != 1 {
		t.Errorf("unknown = %v", unknown)
	}
	out, err := MarshalWithUnknown(v, unknown)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":"x","Plain":"p","c":true,"future":{"k":[1]}}` {
		t.Errorf("MarshalWithUnknown = %s", out)
	}
	if got := ExtraKeys(map[string]json.RawMessage{"a": nil, "b": nil}, map[string]bool{"a": true}, "b"); got != nil {
		t.Errorf("ExtraKeys = %v", got)
	}
	if PeekString(json.RawMessage(`"s"`)) != "s" || PeekString(json.RawMessage(`1`)) != "" {
		t.Error("PeekString")
	}
	if _, err := UnmarshalWithUnknown([]byte(`[]`), &v, nil); err == nil {
		t.Error("array accepted")
	}
}
