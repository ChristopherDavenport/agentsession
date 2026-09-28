package agentsession

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// NormaliseJSON rewrites a JSON document so that every value passes the
// I-JSON test the format hashes under, and reports each change the way
// the format has a writer record it. A whole number outside binary64
// is rounded to binary64 and written canonically, so 9007199254740993
// becomes 9007199254740992 however it was spelled; a lone surrogate
// escape becomes U+FFFD; a string that is not valid UTF-8 has each
// invalid byte replaced with U+FFFD, as a decoder would do silently.
// Each change carries the RFC 6901 pointer of the value relative to
// data and the value's original JSON source text, escapes included, or
// its original bytes in base64 when there was no valid text to record.
// The changes are sorted by pointer as UTF-16 code units, the order the
// format fixes so two writers normalising one output agree.
//
// [Session.Append] runs this over an entry's body, so a harness that
// hands the library a body straight from a provider need do nothing.
// It is exported for the case Go's decoder hides: a lone surrogate in
// provider bytes is replaced on the way into a struct, and the record
// that it happened is lost unless the harness normalises the bytes
// first and keeps the changes, prefixing each pointer with that of the
// member the bytes become (for an item, "/item") before setting them
// on [EntryBase.Normalised].
//
// What cannot be normalised is an error: a number that is not finite
// in binary64, a repeated member name, a member name that itself fails
// the test.
func NormaliseJSON(data []byte) ([]byte, []Normalisation, error) {
	if !json.Valid(data) {
		return nil, nil, fmt.Errorf("%w: not JSON", ijson.ErrNotIJSON)
	}
	type frame struct {
		object    bool
		key       string
		expectKey bool
		index     int
	}
	type edit struct {
		start, end int
		repl       []byte
	}
	var (
		stack   []*frame
		edits   []edit
		changes []Normalisation
	)
	pointer := func() string {
		var b strings.Builder
		for _, f := range stack {
			b.WriteByte('/')
			if f.object {
				b.WriteString(strings.ReplaceAll(strings.ReplaceAll(f.key, "~", "~0"), "/", "~1"))
			} else {
				fmt.Fprintf(&b, "%d", f.index)
			}
		}
		return b.String()
	}
	valueDone := func() {
		if len(stack) == 0 {
			return
		}
		t := stack[len(stack)-1]
		if t.object {
			t.expectKey = true
		} else {
			t.index++
		}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	for {
		start := int(dec.InputOffset())
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", ijson.ErrNotIJSON, err)
		}
		end := int(dec.InputOffset())
		// The slice from the previous token runs over the separator
		// and any whitespace before this one; none of those bytes can
		// start a token.
		for start < end && (data[start] == ' ' || data[start] == '\t' || data[start] == '\n' || data[start] == '\r' || data[start] == ':' || data[start] == ',') {
			start++
		}
		src := data[start:end]
		switch t := tok.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, &frame{object: true, expectKey: true})
			case '[':
				stack = append(stack, &frame{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
			continue
		case string:
			if f := len(stack); f > 0 && stack[f-1].object && stack[f-1].expectKey {
				if !utf8.Valid(src) || hasLoneSurrogate(src) {
					return nil, nil, fmt.Errorf("%w: member name %s is not I-JSON", ijson.ErrNotIJSON, src)
				}
				stack[f-1].key = t
				stack[f-1].expectKey = false
				continue
			}
			switch {
			case !utf8.Valid(src):
				changes = append(changes, Normalisation{At: pointer(), Raw: base64.StdEncoding.EncodeToString(src)})
				edits = append(edits, edit{start, end, fixSurrogates(replaceInvalidUTF8(src))})
			case hasLoneSurrogate(src):
				changes = append(changes, Normalisation{At: pointer(), Was: string(src)})
				edits = append(edits, edit{start, end, fixSurrogates(src)})
			}
		case json.Number:
			if ijson.CheckNumber(string(t)) != nil {
				r, ok := new(big.Rat).SetString(string(t))
				if !ok {
					return nil, nil, fmt.Errorf("%w: number %q", ijson.ErrNotIJSON, t)
				}
				f, _ := r.Float64()
				if math.IsInf(f, 0) {
					return nil, nil, fmt.Errorf("%w: number %q is not finite in binary64", ijson.ErrNotIJSON, t)
				}
				repl, err := jcs.FormatNumber(f)
				if err != nil {
					return nil, nil, err
				}
				// From 1e21 the canonical form is an exponent whose
				// exact value is not the double's, so the rounding
				// fails the test it was meant to pass and a reader
				// would refuse the line (#76).
				if err := ijson.CheckNumber(repl); err != nil {
					return nil, nil, fmt.Errorf("%w: whole number %q rounds to %s, which canonical form cannot write exactly", ijson.ErrNotIJSON, t, repl)
				}
				changes = append(changes, Normalisation{At: pointer(), Was: string(src)})
				edits = append(edits, edit{start, end, []byte(repl)})
			}
		}
		valueDone()
	}
	out := data
	if len(edits) > 0 {
		var buf bytes.Buffer
		buf.Grow(len(data))
		at := 0
		for _, e := range edits {
			buf.Write(data[at:e.start])
			buf.Write(e.repl)
			at = e.end
		}
		buf.Write(data[at:])
		out = buf.Bytes()
	}
	// What the rewrite cannot reach, a repeated member above all, is
	// still refused.
	if err := ijson.Check(out); err != nil {
		return nil, nil, err
	}
	sortNormalisations(changes)
	return out, changes, nil
}

// sortNormalisations orders changes by pointer as UTF-16 code units.
func sortNormalisations(n []Normalisation) {
	sort.SliceStable(n, func(i, j int) bool { return jcs.LessUTF16(n[i].At, n[j].At) })
}

// replaceInvalidUTF8 replaces each byte of s that is not part of a
// valid UTF-8 sequence with U+FFFD, one replacement per byte, which is
// what a decoder that repairs does.
func replaceInvalidUTF8(s []byte) []byte {
	out := make([]byte, 0, len(s))
	for len(s) > 0 {
		r, size := utf8.DecodeRune(s)
		if r == utf8.RuneError && size == 1 {
			out = utf8.AppendRune(out, utf8.RuneError)
		} else {
			out = append(out, s[:size]...)
		}
		s = s[size:]
	}
	return out
}

// surrogateAt reads a \uXXXX escape at s[i:] and reports the code unit,
// whether it was one, and whether it is a surrogate.
func surrogateAt(s []byte, i int) (unit uint32, ok bool) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	for _, c := range s[i+2 : i+6] {
		unit <<= 4
		switch {
		case c >= '0' && c <= '9':
			unit |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			unit |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			unit |= uint32(c-'A') + 10
		default:
			return 0, false
		}
	}
	return unit, true
}

// hasLoneSurrogate reports whether the string literal s carries a
// \uXXXX escape for a surrogate without its pair.
func hasLoneSurrogate(s []byte) bool {
	return !bytes.Equal(fixSurrogates(s), s)
}

// fixSurrogates rewrites each lone surrogate escape in the string
// literal s as �, leaving pairs and everything else as written.
func fixSurrogates(s []byte) []byte {
	var out []byte
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			if out != nil {
				out = append(out, s[i])
			}
			continue
		}
		hi, ok := surrogateAt(s, i)
		if !ok {
			// Another escape: copy it and the byte it escapes.
			if out != nil {
				out = append(out, s[i])
				if i+1 < len(s) {
					out = append(out, s[i+1])
				}
			}
			i++
			continue
		}
		switch {
		case hi >= 0xD800 && hi <= 0xDBFF:
			if lo, ok := surrogateAt(s, i+6); ok && lo >= 0xDC00 && lo <= 0xDFFF {
				if out != nil {
					out = append(out, s[i:i+12]...)
				}
				i += 11
				continue
			}
			fallthrough
		case hi >= 0xDC00 && hi <= 0xDFFF:
			if out == nil {
				out = append(make([]byte, 0, len(s)), s[:i]...)
			}
			out = append(out, `�`...)
			i += 5
		default:
			if out != nil {
				out = append(out, s[i:i+6]...)
			}
			i += 5
		}
	}
	if out == nil {
		return s
	}
	return out
}

// normaliseEntry rewrites e's body as the format has a writer do
// before writing, when the body does not pass the I-JSON test as
// marshalled, and records the changes on the entry beside any the
// caller recorded. The rewritten line is decoded back into e, so what
// the caller holds is what is hashed and written.
func normaliseEntry(e Entry) error {
	data, err := MarshalEntry(e)
	if err != nil {
		return err
	}
	if ijson.Check(data) == nil {
		return nil
	}
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	env := make(map[string]json.RawMessage, len(envelopeKeys))
	body := make(map[string]json.RawMessage, len(all))
	for k, v := range all {
		if isEnvelopeKey(k) {
			env[k] = v
		} else {
			body[k] = v
		}
	}
	prior := e.Base().Normalised
	delete(body, "normalised")
	bodyJSON, err := jsonx.MarshalNoEscape(body)
	if err != nil {
		return err
	}
	out, changes, err := NormaliseJSON(bodyJSON)
	if err != nil {
		return fmt.Errorf("agentsession: entry body: %w", err)
	}
	if len(changes) == 0 {
		return errors.New("agentsession: entry body: not I-JSON and nothing to normalise")
	}
	norm := append(append([]Normalisation(nil), prior...), changes...)
	sortNormalisations(norm)
	head, err := jsonx.MarshalNoEscape(env)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(norm)
	if err != nil {
		return err
	}
	line := jsonx.JoinObjects(head, out, map[string]json.RawMessage{"normalised": raw})
	return json.Unmarshal(line, e)
}
