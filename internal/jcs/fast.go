package jcs

import (
	"bytes"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// transformFast canonicalises data by walking its bytes, without
// decoding it into Go values: a string without escapes and a short
// integer are already in canonical form and are copied, and members are
// reordered as spans. It reports false for what it leaves to the
// decoder — invalid UTF-8 and lone surrogates, which the decoder
// replaces, repeated member names, which it resolves, nesting deeper
// than maxDepth, and anything that is not one JSON document — so that
// whatever it returns is what the decoder's path returns.
func transformFast(data []byte) ([]byte, bool) {
	if !utf8.Valid(data) {
		return nil, false
	}
	t := fastT{in: data, out: make([]byte, 0, len(data))}
	i := skipWS(data, 0)
	i, ok := t.value(i, 0)
	if !ok || skipWS(data, i) != len(data) {
		return nil, false
	}
	return t.out, true
}

const maxDepth = 1000

type fastT struct {
	in, out []byte
	scratch []byte
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

func (t *fastT) value(i, depth int) (int, bool) {
	if i >= len(t.in) {
		return i, false
	}
	switch c := t.in[i]; {
	case c == '{':
		if depth >= maxDepth {
			return i, false
		}
		return t.object(i, depth+1)
	case c == '[':
		if depth >= maxDepth {
			return i, false
		}
		return t.array(i, depth+1)
	case c == '"':
		s, end, ok := t.str(i)
		if !ok {
			return i, false
		}
		t.out = appendString(t.out, s)
		return end, true
	case c == '-' || c >= '0' && c <= '9':
		return t.number(i)
	case c == 't':
		return t.lit(i, "true")
	case c == 'f':
		return t.lit(i, "false")
	case c == 'n':
		return t.lit(i, "null")
	}
	return i, false
}

func (t *fastT) lit(i int, word string) (int, bool) {
	if !bytes.HasPrefix(t.in[i:], []byte(word)) {
		return i, false
	}
	t.out = append(t.out, word...)
	return i + len(word), true
}

// numberEnd returns the end of the number at i and whether it is a
// plain integer: an optional minus and digits, with no fraction or
// exponent.
func numberEnd(b []byte, i int) (end int, integer, ok bool) {
	start := i
	if i < len(b) && b[i] == '-' {
		i++
	}
	switch {
	case i < len(b) && b[i] == '0':
		i++
	case i < len(b) && b[i] >= '1' && b[i] <= '9':
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
	default:
		return start, false, false
	}
	integer = true
	if i < len(b) && b[i] == '.' {
		integer = false
		i++
		d := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		if i == d {
			return start, false, false
		}
	}
	if i < len(b) && (b[i] == 'e' || b[i] == 'E') {
		integer = false
		i++
		if i < len(b) && (b[i] == '+' || b[i] == '-') {
			i++
		}
		d := i
		for i < len(b) && b[i] >= '0' && b[i] <= '9' {
			i++
		}
		if i == d {
			return start, false, false
		}
	}
	return i, integer, true
}

func (t *fastT) number(i int) (int, bool) {
	end, integer, ok := numberEnd(t.in, i)
	if !ok {
		return i, false
	}
	lit := t.in[i:end]
	digits := len(lit)
	if lit[0] == '-' {
		digits--
	}
	// An integer of at most 15 digits is exact in binary64 and written
	// as it stands, apart from negative zero, which is written 0.
	if integer && digits <= 15 && !(lit[0] == '-' && lit[1] == '0') {
		t.out = append(t.out, lit...)
		return end, true
	}
	f, err := strconv.ParseFloat(string(lit), 64)
	if err != nil {
		return i, false
	}
	s, err := FormatNumber(f)
	if err != nil {
		return i, false
	}
	t.out = append(t.out, s...)
	return end, true
}

// str reads the string at i and returns its value: the bytes between
// the quotes when it has no escapes, the decoded value otherwise.
func (t *fastT) str(i int) ([]byte, int, bool) {
	start := i + 1
	j := start
	for j < len(t.in) {
		c := t.in[j]
		switch {
		case c == '"':
			return t.in[start:j], j + 1, true
		case c == '\\':
			return t.strEscaped(start)
		case c < 0x20:
			return nil, i, false
		}
		j++
	}
	return nil, i, false
}

func (t *fastT) strEscaped(start int) ([]byte, int, bool) {
	out := t.scratch[:0]
	j := start
	for j < len(t.in) {
		c := t.in[j]
		switch {
		case c == '"':
			t.scratch = out
			return out, j + 1, true
		case c < 0x20:
			return nil, start, false
		case c != '\\':
			out = append(out, c)
			j++
			continue
		}
		if j+1 >= len(t.in) {
			return nil, start, false
		}
		switch e := t.in[j+1]; e {
		case '"', '\\', '/':
			out = append(out, e)
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, ok := hex4(t.in, j+2)
			if !ok {
				return nil, start, false
			}
			if utf16.IsSurrogate(rune(r)) {
				// Only a well-formed pair; the decoder turns a lone
				// half into U+FFFD, which is left to it.
				if r >= 0xDC00 || j+11 >= len(t.in) || t.in[j+6] != '\\' || t.in[j+7] != 'u' {
					return nil, start, false
				}
				lo, ok := hex4(t.in, j+8)
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return nil, start, false
				}
				out = utf8.AppendRune(out, utf16.DecodeRune(rune(r), rune(lo)))
				j += 12
				continue
			}
			out = utf8.AppendRune(out, rune(r))
			j += 6
			continue
		default:
			return nil, start, false
		}
		j += 2
	}
	return nil, start, false
}

func hex4(b []byte, at int) (uint32, bool) {
	if at+4 > len(b) {
		return 0, false
	}
	var v uint32
	for _, c := range b[at : at+4] {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			return 0, false
		}
	}
	return v, true
}

// appendString appends s, valid UTF-8, as writeString writes it.
func appendString(out, s []byte) []byte {
	out = append(out, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '\b':
			out = append(out, `\b`...)
		case '\t':
			out = append(out, `\t`...)
		case '\n':
			out = append(out, `\n`...)
		case '\f':
			out = append(out, `\f`...)
		case '\r':
			out = append(out, `\r`...)
		case '"':
			out = append(out, `\"`...)
		case '\\':
			out = append(out, `\\`...)
		default:
			if c < 0x20 {
				const hexDigits = "0123456789abcdef"
				out = append(out, '\\', 'u', '0', '0', hexDigits[c>>4], hexDigits[c&0xf])
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}

type member struct {
	key        []byte
	start, end int // the member's "key":value in out
}

func (t *fastT) object(i, depth int) (int, bool) {
	base := len(t.out)
	t.out = append(t.out, '{')
	i = skipWS(t.in, i+1)
	if i < len(t.in) && t.in[i] == '}' {
		t.out = append(t.out, '}')
		return i + 1, true
	}
	var members []member
	sorted := true
	for {
		if i >= len(t.in) || t.in[i] != '"' {
			return i, false
		}
		k, end, ok := t.str(i)
		if !ok {
			return i, false
		}
		// The scratch buffer is reused; the key must outlive it.
		key := append([]byte(nil), k...)
		i = skipWS(t.in, end)
		if i >= len(t.in) || t.in[i] != ':' {
			return i, false
		}
		i = skipWS(t.in, i+1)
		if len(members) > 0 {
			t.out = append(t.out, ',')
		}
		start := len(t.out)
		t.out = appendString(t.out, key)
		t.out = append(t.out, ':')
		if i, ok = t.value(i, depth); !ok {
			return i, false
		}
		m := member{key: key, start: start, end: len(t.out)}
		if len(members) > 0 {
			switch prev := members[len(members)-1].key; {
			case bytes.Equal(prev, key):
				return i, false
			case !lessKey(prev, key):
				sorted = false
			}
		}
		members = append(members, m)
		i = skipWS(t.in, i)
		if i >= len(t.in) {
			return i, false
		}
		if t.in[i] == ',' {
			i = skipWS(t.in, i+1)
			continue
		}
		if t.in[i] != '}' {
			return i, false
		}
		i++
		break
	}
	if !sorted {
		sortMembers(members)
		for k := 1; k < len(members); k++ {
			if bytes.Equal(members[k-1].key, members[k].key) {
				return i, false
			}
		}
		body := append([]byte(nil), t.out[base+1:]...)
		t.out = t.out[:base+1]
		for k, m := range members {
			if k > 0 {
				t.out = append(t.out, ',')
			}
			t.out = append(t.out, body[m.start-base-1:m.end-base-1]...)
		}
	}
	t.out = append(t.out, '}')
	return i, true
}

func (t *fastT) array(i, depth int) (int, bool) {
	t.out = append(t.out, '[')
	i = skipWS(t.in, i+1)
	if i < len(t.in) && t.in[i] == ']' {
		t.out = append(t.out, ']')
		return i + 1, true
	}
	for n := 0; ; n++ {
		if n > 0 {
			t.out = append(t.out, ',')
		}
		var ok bool
		if i, ok = t.value(i, depth); !ok {
			return i, false
		}
		i = skipWS(t.in, i)
		if i >= len(t.in) {
			return i, false
		}
		if t.in[i] == ',' {
			i = skipWS(t.in, i+1)
			continue
		}
		if t.in[i] != ']' {
			return i, false
		}
		t.out = append(t.out, ']')
		return i + 1, true
	}
}

// lessKey orders member names by UTF-16 code units. UTF-8 byte order
// agrees with it apart from characters above the basic multilingual
// plane, which UTF-16 writes as surrogates below U+E000; only a name
// holding one, whose UTF-8 has a four-byte sequence, needs the
// conversion.
func lessKey(a, b []byte) bool {
	if !hasSupplementary(a) && !hasSupplementary(b) {
		return bytes.Compare(a, b) < 0
	}
	return LessUTF16(string(a), string(b))
}

func hasSupplementary(s []byte) bool {
	for _, c := range s {
		if c >= 0xF0 {
			return true
		}
	}
	return false
}

// sortMembers is an insertion sort for the few members an object has,
// falling back to a merge for many.
func sortMembers(m []member) {
	if len(m) > 32 {
		mergeSort(m, make([]member, len(m)))
		return
	}
	for i := 1; i < len(m); i++ {
		for j := i; j > 0 && lessKey(m[j].key, m[j-1].key); j-- {
			m[j], m[j-1] = m[j-1], m[j]
		}
	}
}

func mergeSort(m, tmp []member) {
	if len(m) <= 32 {
		sortMembers(m)
		return
	}
	mid := len(m) / 2
	mergeSort(m[:mid], tmp[:mid])
	mergeSort(m[mid:], tmp[mid:])
	copy(tmp, m)
	a, b, k := 0, mid, 0
	for a < mid && b < len(m) {
		if lessKey(tmp[b].key, tmp[a].key) {
			m[k] = tmp[b]
			b++
		} else {
			m[k] = tmp[a]
			a++
		}
		k++
	}
	for ; a < mid; a++ {
		m[k] = tmp[a]
		k++
	}
	for ; b < len(m); b++ {
		m[k] = tmp[b]
		k++
	}
}
