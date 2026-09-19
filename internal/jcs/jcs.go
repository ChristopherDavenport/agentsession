// Package jcs implements the JSON Canonicalization Scheme (RFC 8785):
// members sorted by UTF-16 code units, no insignificant whitespace,
// strings with the minimal escapes the scheme prescribes and numbers in
// the ECMAScript Number.prototype.toString form. The request hash the
// session format records is the SHA-256 of this serialisation, so a
// writer and a reader built separately agree byte for byte.
package jcs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Transform returns the canonical form of the JSON document in data.
// Numbers are interpreted as IEEE 754 doubles, as the scheme requires,
// so integers beyond 2^53 lose precision. A number that overflows a
// double is an error, as is anything after the first document.
func Transform(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("jcs: decode: %w", err)
	}
	if dec.More() {
		return nil, errors.New("jcs: trailing data after the document")
	}
	var buf bytes.Buffer
	if err := write(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func write(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if x {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		if err != nil {
			return fmt.Errorf("jcs: number %q: %w", x, err)
		}
		s, err := FormatNumber(f)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case string:
		writeString(buf, x)
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := write(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := write(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unexpected value of type %T", v)
	}
	return nil
}

// lessUTF16 orders strings by their UTF-16 code units, which is the
// member ordering RFC 8785 section 3.2.3 requires. It differs from byte
// order only for code points above the basic multilingual plane.
func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// writeString serialises s as RFC 8785 section 3.2.2.2 prescribes: the
// two-character escapes for backspace, tab, newline, form feed, carriage
// return, quote and backslash, \u00xx for the other control characters
// and every other character literally.
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		default:
			if r < 0x20 {
				fmt.Fprintf(buf, `\u%04x`, r)
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// FormatNumber renders f as ECMAScript's Number.prototype.toString does
// (ECMA-262 section 7.1.12.1), which is the number form RFC 8785
// requires. Negative zero renders as "0". NaN and infinities are errors
// because JSON cannot carry them.
func FormatNumber(f float64) (string, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", errors.New("jcs: NaN and infinity are not representable")
	}
	if f == 0 {
		return "0", nil
	}
	var sb strings.Builder
	if f < 0 {
		sb.WriteByte('-')
		f = -f
	}
	// Shortest round-tripping digits and the decimal exponent.
	e := strconv.FormatFloat(f, 'e', -1, 64) // d.ddde±xx
	mant, expStr, _ := strings.Cut(e, "e")
	exp, err := strconv.Atoi(expStr)
	if err != nil {
		return "", fmt.Errorf("jcs: parse exponent of %q: %w", e, err)
	}
	digits := strings.Replace(mant, ".", "", 1)
	k := len(digits)
	n := exp + 1 // position of the decimal point relative to the digits
	switch {
	case k <= n && n <= 21:
		sb.WriteString(digits)
		sb.WriteString(strings.Repeat("0", n-k))
	case 0 < n && n <= 21:
		sb.WriteString(digits[:n])
		sb.WriteByte('.')
		sb.WriteString(digits[n:])
	case -6 < n && n <= 0:
		sb.WriteString("0.")
		sb.WriteString(strings.Repeat("0", -n))
		sb.WriteString(digits)
	default:
		sb.WriteByte(digits[0])
		if k > 1 {
			sb.WriteByte('.')
			sb.WriteString(digits[1:])
		}
		sb.WriteByte('e')
		if n-1 >= 0 {
			sb.WriteByte('+')
		}
		sb.WriteString(strconv.Itoa(n - 1))
	}
	return sb.String(), nil
}
