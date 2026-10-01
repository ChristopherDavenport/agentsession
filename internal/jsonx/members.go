package jsonx

import (
	"encoding/json"
	"unicode/utf8"
)

// Members splits a JSON object into its members as unmarshalling it
// into a map of raw messages does: each value's bytes copied as they
// stand, a repeated name taking the last value. It slices the members
// out rather than running the decoder over each, and reports false for
// what it leaves to the decoder: a document that is not valid JSON or
// not an object, and a member name with an escape or invalid UTF-8,
// which the decoder rewrites.
func Members(data []byte) (map[string]json.RawMessage, bool) {
	if !json.Valid(data) {
		return nil, false
	}
	i := skipWS(data, 0)
	if i >= len(data) || data[i] != '{' {
		return nil, false
	}
	all := map[string]json.RawMessage{}
	i = skipWS(data, i+1)
	if data[i] == '}' {
		return all, true
	}
	for {
		// The document is valid, so a name follows.
		start := i + 1
		end := start
		for data[end] != '"' {
			if data[end] == '\\' {
				return nil, false
			}
			end++
		}
		key := data[start:end]
		if !utf8.Valid(key) {
			return nil, false
		}
		i = skipWS(data, end+1) + 1 // past the colon
		i = skipWS(data, i)
		v := skipValue(data, i)
		all[string(key)] = append(json.RawMessage(nil), data[i:v]...)
		i = skipWS(data, v)
		if data[i] == '}' {
			return all, true
		}
		i = skipWS(data, i+1) // past the comma
	}
}

func skipWS(b []byte, i int) int {
	for i < len(b) {
		switch b[i] {
		case ' ', '\t', '\n', '\r':
			i++
		default:
			return i
		}
	}
	return i
}

// skipValue returns the end of the valid JSON value at i.
func skipValue(b []byte, i int) int {
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for i < len(b) {
			switch b[i] {
			case '"':
				i = skipString(b, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for i < len(b) {
		switch b[i] {
		case ',', '}', ']', ' ', '\t', '\n', '\r':
			return i
		}
		i++
	}
	return i
}

// skipString returns the end of the string at i, past its closing quote.
func skipString(b []byte, i int) int {
	for i++; i < len(b); i++ {
		switch b[i] {
		case '\\':
			i++
		case '"':
			return i + 1
		}
	}
	return i
}
