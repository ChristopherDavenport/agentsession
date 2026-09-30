package ijson

import (
	"bytes"
	"unicode/utf8"
)

// checkFast reports whether data is one JSON document that passes the
// test, by walking its bytes. It answers only yes: false means it left
// the document to the tokenizing check, which finds and names what is
// wrong, or finds nothing. It leaves to that check every document it
// does not pass outright: a surrogate escape, an escaped member name,
// nesting deeper than maxDepth, and anything that is not one JSON
// document. A number outside the plain forms goes to CheckNumber, the
// rule itself.
func checkFast(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	i := skipWS(data, 0)
	i, ok := fastValue(data, i, 0)
	return ok && skipWS(data, i) == len(data)
}

const maxDepth = 1000

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

func fastValue(b []byte, i, depth int) (int, bool) {
	if i >= len(b) {
		return i, false
	}
	switch c := b[i]; {
	case c == '{':
		if depth >= maxDepth {
			return i, false
		}
		return fastObject(b, i, depth+1)
	case c == '[':
		if depth >= maxDepth {
			return i, false
		}
		return fastArray(b, i, depth+1)
	case c == '"':
		end, _, ok := fastString(b, i)
		return end, ok
	case c == '-' || c >= '0' && c <= '9':
		return fastNumber(b, i)
	case c == 't':
		return fastLit(b, i, "true")
	case c == 'f':
		return fastLit(b, i, "false")
	case c == 'n':
		return fastLit(b, i, "null")
	}
	return i, false
}

func fastLit(b []byte, i int, word string) (int, bool) {
	if !bytes.HasPrefix(b[i:], []byte(word)) {
		return i, false
	}
	return i + len(word), true
}

// fastString returns the end of the string at i and whether it holds an
// escape. A \u escape in the surrogate range is left to the full check,
// which tells a pair from a lone half.
func fastString(b []byte, i int) (end int, escaped, ok bool) {
	j := i + 1
	for j < len(b) {
		c := b[j]
		switch {
		case c == '"':
			return j + 1, escaped, true
		case c < 0x20:
			return i, false, false
		case c == '\\':
			escaped = true
			if j+1 >= len(b) {
				return i, false, false
			}
			switch b[j+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j += 2
				continue
			case 'u':
				v, ok := hex4(b, j+2)
				if !ok || v >= 0xD800 && v <= 0xDFFF {
					return i, false, false
				}
				j += 6
				continue
			}
			return i, false, false
		}
		j++
	}
	return i, false, false
}

// fastNumber passes a number whose value the rule plainly admits: an
// integer of at most 15 digits, which binary64 holds exactly, or a
// fraction with a non-zero fractional digit, an integer part of at most
// 15 digits and no exponent, which is finite and not whole. Any other
// goes to CheckNumber.
func fastNumber(b []byte, i int) (int, bool) {
	start := i
	if b[i] == '-' {
		i++
	}
	intStart := i
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case i < len(b) && b[i] >= '1' && b[i] <= '9':
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
	default:
		return start, false
	}
	intDigits := i - intStart
	plain := intDigits <= 15
	if i < len(b) && b[i] == '.' {
		i++
		d, nonZero := i, false
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			nonZero = nonZero || b[i] != '0'
			i++
		}
		if i == d {
			return start, false
		}
		plain = plain && nonZero
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		plain = false
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		d := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		if i == d {
			return start, false
		}
	}
	if !plain && CheckNumber(string(b[start:i])) != nil {
		return start, false
	}
	return i, true
}

func fastObject(b []byte, i, depth int) (int, bool) {
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == '}' {
		return i + 1, true
	}
	var keys [][]byte
	var seen map[string]bool
	for {
		if i >= len(b) || b[i] != '"' {
			return i, false
		}
		end, escaped, ok := fastString(b, i)
		if !ok || escaped {
			// An escaped name may spell a name seen unescaped; the full
			// check compares decoded names.
			return i, false
		}
		key := b[i+1 : end-1]
		// A few names are compared in turn; many through a map.
		if seen == nil && len(keys) < 16 {
			for _, k := range keys {
				if bytes.Equal(k, key) {
					return i, false
				}
			}
			keys = append(keys, key)
		} else {
			if seen == nil {
				seen = make(map[string]bool, 32)
				for _, k := range keys {
					seen[string(k)] = true
				}
			}
			if seen[string(key)] {
				return i, false
			}
			seen[string(key)] = true
		}
		i = skipWS(b, end)
		if i >= len(b) || b[i] != ':' {
			return i, false
		}
		i = skipWS(b, i+1)
		if i, ok = fastValue(b, i, depth); !ok {
			return i, false
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return i, false
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case '}':
			return i + 1, true
		default:
			return i, false
		}
	}
}

func fastArray(b []byte, i, depth int) (int, bool) {
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == ']' {
		return i + 1, true
	}
	for {
		var ok bool
		if i, ok = fastValue(b, i, depth); !ok {
			return i, false
		}
		i = skipWS(b, i)
		if i >= len(b) {
			return i, false
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case ']':
			return i + 1, true
		default:
			return i, false
		}
	}
}
