// Package ijson checks that a JSON document is I-JSON (RFC 7493) by the
// test the session format states: every number is finite once rounded
// to binary64, a number whose exact value is a whole number is exactly
// representable in binary64 or is the exact value of the canonical
// rendering of the nearest double, no object repeats a member name, and
// no string holds a lone surrogate. The JSON Canonicalization Scheme is
// defined over I-JSON, so this is what makes two implementations hash
// one document the same way; the format's writer normalises what a
// model or a provider emits so that every hashed member passes.
package ijson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"io"
	"math"
	"math/big"
	"strings"
	"unicode/utf8"
)

// ErrNotIJSON is wrapped by every error Check returns.
var ErrNotIJSON = errors.New("ijson: not I-JSON")

// Check reports the first way data fails the test, or nil.
func Check(data []byte) error {
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: invalid UTF-8", ErrNotIJSON)
	}
	if err := checkSurrogates(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	type frame struct {
		object    bool
		keys      map[string]bool
		expectKey bool
	}
	var stack []*frame
	top := func() *frame {
		if len(stack) == 0 {
			return nil
		}
		return stack[len(stack)-1]
	}
	// valueDone tells the enclosing object that a value finished, so the
	// next string is a key again.
	valueDone := func() {
		if t := top(); t != nil && t.object {
			t.expectKey = true
		}
	}
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrNotIJSON, err)
		}
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, keys: map[string]bool{}, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			case '}', ']':
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		case string:
			if f := top(); f != nil && f.object && f.expectKey {
				if f.keys[t] {
					return fmt.Errorf("%w: member %q repeated", ErrNotIJSON, t)
				}
				f.keys[t] = true
				f.expectKey = false
				continue
			}
		case json.Number:
			if err := CheckNumber(string(t)); err != nil {
				return err
			}
		}
		valueDone()
	}
	return nil
}

// CheckNumber applies the number rule to one JSON number literal: finite
// once rounded to binary64, and, when its exact value is a whole
// number, either exactly representable or the exact value of the
// canonical rendering of the nearest double. The rounding of a fraction
// is expected and is not an error, so 0.1 passes; 9007199254740993
// fails however it is spelled, and 9007199254740992, 1e20, 2^60 and
// 1152921504606847000, what canonical form writes for 2^60, pass.
func CheckNumber(lit string) error {
	r, ok := new(big.Rat).SetString(lit)
	if !ok {
		return fmt.Errorf("%w: number %q", ErrNotIJSON, lit)
	}
	f, exact := r.Float64()
	if math.IsInf(f, 0) || math.IsNaN(f) {
		return fmt.Errorf("%w: number %q is not finite in binary64", ErrNotIJSON, lit)
	}
	if r.IsInt() && !exact {
		// The canonical form is ECMAScript's, which below 1e21 pads the
		// shortest digits that round-trip a double with zeros, so from
		// 2^53 it writes many representable whole numbers as a whole
		// number no double holds: 2^60 as 1152921504606847000. That
		// rendering is admissible, or a canonical rewrite could fail
		// the test the original passed.
		if c, err := jcs.FormatNumber(f); err == nil {
			if cr, ok := new(big.Rat).SetString(c); ok && cr.Cmp(r) == 0 {
				return nil
			}
		}
		return fmt.Errorf("%w: whole number %q is not exactly representable in binary64, nor the canonical rendering of a double", ErrNotIJSON, lit)
	}
	return nil
}

// checkSurrogates scans string literals for a \u escape in the surrogate
// range that is not half of a well-formed pair. The decoder would
// replace one with U+FFFD silently, which is exactly the change the
// test exists to make visible.
func checkSurrogates(data []byte) error {
	inString := false
	for i := 0; i < len(data); i++ {
		c := data[i]
		if !inString {
			if c == '"' {
				inString = true
			}
			continue
		}
		switch c {
		case '"':
			inString = false
		case '\\':
			if i+1 >= len(data) {
				return nil
			}
			i++
			if data[i] != 'u' {
				continue
			}
			hi, ok := hex4(data, i+1)
			if !ok {
				continue
			}
			i += 4
			switch {
			case hi >= 0xDC00 && hi <= 0xDFFF:
				return fmt.Errorf("%w: lone low surrogate \\u%04x", ErrNotIJSON, hi)
			case hi >= 0xD800 && hi <= 0xDBFF:
				if i+2 < len(data) && data[i+1] == '\\' && data[i+2] == 'u' {
					if lo, ok := hex4(data, i+3); ok && lo >= 0xDC00 && lo <= 0xDFFF {
						i += 6
						continue
					}
				}
				return fmt.Errorf("%w: lone high surrogate \\u%04x", ErrNotIJSON, hi)
			}
		}
	}
	return nil
}

func hex4(data []byte, at int) (uint32, bool) {
	if at+4 > len(data) {
		return 0, false
	}
	var v uint32
	for _, c := range data[at : at+4] {
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

// IsWhole reports whether a number literal's exact value is a whole
// number, which is the case the exactness rule is about.
func IsWhole(lit string) bool {
	r, ok := new(big.Rat).SetString(strings.TrimSpace(lit))
	return ok && r.IsInt()
}
