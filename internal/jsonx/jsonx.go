// Package jsonx holds the encoding/json helpers the agentsession
// packages share: escape-free marshalling, member joining for objects
// with unknown members, and struct key discovery.
package jsonx

import (
	"bytes"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
)

// MarshalNoEscape marshals v without HTML escaping, so that raw bytes
// re-emitted by extension types stay byte-identical to their source.
func MarshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// JoinObjects concatenates the members of two JSON objects and appends
// extra in key order. Both inputs must be objects; body may be "{}".
func JoinObjects(head, body []byte, extra map[string]json.RawMessage) []byte {
	out := make([]byte, 0, len(head)+len(body)+16*len(extra))
	out = append(out, head[:len(head)-1]...) // drop closing brace
	if len(body) > 2 {
		if len(head) > 2 {
			out = append(out, ',')
		}
		out = append(out, body[1:len(body)-1]...)
	}
	if len(extra) > 0 {
		keys := make([]string, 0, len(extra))
		for k := range extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if len(out) > 1 {
				out = append(out, ',')
			}
			kb, _ := json.Marshal(k)
			out = append(out, kb...)
			out = append(out, ':')
			out = append(out, extra[k]...)
		}
	}
	return append(out, '}')
}

// MarshalWithUnknown marshals v (which must produce an object) and
// appends the unknown members in key order.
func MarshalWithUnknown(v any, unknown map[string]json.RawMessage) ([]byte, error) {
	body, err := MarshalNoEscape(v)
	if err != nil {
		return nil, err
	}
	if len(unknown) == 0 {
		return body, nil
	}
	return JoinObjects(body, []byte("{}"), unknown), nil
}

// UnmarshalWithUnknown decodes data into v and returns the members not
// declared by v's struct tags, or nil when there are none.
func UnmarshalWithUnknown(data []byte, v any, known map[string]bool) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	return ExtraKeys(all, known), nil
}

// ExtraKeys returns the members of all whose keys are not in known or
// among skip, or nil when there are none.
func ExtraKeys(all map[string]json.RawMessage, known map[string]bool, skip ...string) map[string]json.RawMessage {
	var extra map[string]json.RawMessage
	for k, v := range all {
		if known[k] {
			continue
		}
		skipped := false
		for _, s := range skip {
			if k == s {
				skipped = true
				break
			}
		}
		if skipped {
			continue
		}
		if extra == nil {
			extra = make(map[string]json.RawMessage)
		}
		extra[k] = v
	}
	return extra
}

// Keys returns the JSON member names a struct type T produces, following
// embedded structs, so unknown members can be told apart from declared
// ones on decode.
func Keys[T any]() map[string]bool {
	keys := map[string]bool{}
	var zero T
	collectKeys(reflect.TypeOf(zero), keys)
	return keys
}

func collectKeys(t reflect.Type, keys map[string]bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			collectKeys(f.Type, keys)
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
}

// PeekString decodes raw as a JSON string, returning "" for anything
// else.
func PeekString(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}
