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
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// ErrBadNormalisation is returned for a Normalisation a caller set that
// the format cannot carry: no pointer, or not exactly one of Was and
// Raw.
var ErrBadNormalisation = errors.New("agentsession: normalised entry is malformed")

// NormaliseJSON rewrites a JSON document so that every value passes the
// I-JSON test the format hashes under, and reports each change the way
// the format has a writer record it. A whole number outside binary64
// is rounded to binary64 and written canonically, so 9007199254740993
// becomes 9007199254740992 however it was spelled and a 64-bit id such
// as 1234567890123456789 becomes 1234567890123456800; a lone surrogate
// escape becomes U+FFFD; in a string that is not valid UTF-8 each
// maximal subpart of an ill-formed sequence (Unicode §3.9) becomes one
// U+FFFD, as the WHATWG decoder does. Each change carries the RFC 6901
// pointer of the value relative to data and the value's original JSON
// source text, a string's quotes included, or its original bytes in
// base64 when there was no valid text to record. The changes are sorted
// by pointer as UTF-16 code units, the order the format fixes so two
// writers normalising one output agree.
//
// [Session.Append] runs this over an entry's body, and repairs a Go
// string that is not valid UTF-8 the same way, so a harness that hands
// the library a body straight from a provider need do nothing. It is
// exported for the case Go's decoder hides: a lone surrogate in
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
	out, changes, _, err := normaliseJSON(data, nil)
	return out, changes, err
}

// stringRepair is a Go string that was not valid UTF-8, replaced in the
// entry by a sentinel so its place in the marshalled body can be found.
type stringRepair struct {
	original []byte
	repaired string
}

// normaliseJSON is NormaliseJSON with the sentinels repairStrings put
// in the entry: each one found is replaced with its repaired string and
// recorded, and located reports which were found, since a string a
// custom marshaller leaves out is never written and has nothing to
// record.
func normaliseJSON(data []byte, sentinels map[string]stringRepair) (out []byte, changes []Normalisation, located map[string]bool, err error) {
	if !json.Valid(data) {
		return nil, nil, nil, fmt.Errorf("%w: not JSON", ijson.ErrNotIJSON)
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
		stack []*frame
		edits []edit
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
			return nil, nil, nil, fmt.Errorf("%w: %v", ijson.ErrNotIJSON, err)
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
					return nil, nil, nil, fmt.Errorf("%w: member name %s is not I-JSON", ijson.ErrNotIJSON, src)
				}
				stack[f-1].key = t
				stack[f-1].expectKey = false
				continue
			}
			if r, ok := sentinels[t]; ok {
				// A Go string the marshaller would have repaired
				// silently: the record is its bytes as the JSON text
				// a writer would have emitted for them.
				if located == nil {
					located = map[string]bool{}
				}
				located[t] = true
				repl, _ := jsonx.MarshalNoEscape(r.repaired)
				changes = append(changes, Normalisation{At: pointer(), Raw: base64.StdEncoding.EncodeToString(quoteBytes(r.original))})
				edits = append(edits, edit{start, end, repl})
				break
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
					return nil, nil, nil, fmt.Errorf("%w: number %q", ijson.ErrNotIJSON, t)
				}
				f, _ := r.Float64()
				if math.IsInf(f, 0) {
					return nil, nil, nil, fmt.Errorf("%w: number %q is not finite in binary64", ijson.ErrNotIJSON, t)
				}
				// The canonical rendering of the rounding, which the
				// test admits by construction.
				repl, err := jcs.FormatNumber(f)
				if err != nil {
					return nil, nil, nil, err
				}
				changes = append(changes, Normalisation{At: pointer(), Was: string(src)})
				edits = append(edits, edit{start, end, []byte(repl)})
			}
		}
		valueDone()
	}
	out = data
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
		return nil, nil, nil, err
	}
	sortNormalisations(changes)
	return out, changes, located, nil
}

// sortNormalisations orders changes by pointer as UTF-16 code units.
func sortNormalisations(n []Normalisation) {
	sort.SliceStable(n, func(i, j int) bool { return jcs.LessUTF16(n[i].At, n[j].At) })
}

// checkNormalisations refuses an element the format cannot carry: no
// pointer, a pointer recorded twice, not exactly one of was and raw, a
// was that is not JSON text, a raw that is not padded standard base64.
func checkNormalisations(n []Normalisation) error {
	seen := make(map[string]bool, len(n))
	for _, x := range n {
		if !strings.HasPrefix(x.At, "/") {
			return fmt.Errorf("%w: at %q is not a pointer into the body", ErrBadNormalisation, x.At)
		}
		if seen[x.At] {
			return fmt.Errorf("%w: %s is recorded twice", ErrBadNormalisation, x.At)
		}
		seen[x.At] = true
		if (x.Was == "") == (x.Raw == "") {
			return fmt.Errorf("%w: %s must carry exactly one of was and raw", ErrBadNormalisation, x.At)
		}
		if x.Was != "" && (!utf8.ValidString(x.Was) || !json.Valid([]byte(x.Was))) {
			return fmt.Errorf("%w: %s: was is not JSON source text", ErrBadNormalisation, x.At)
		}
		if x.Raw != "" {
			if _, err := base64.StdEncoding.Strict().DecodeString(x.Raw); err != nil {
				return fmt.Errorf("%w: %s: raw is not padded standard base64", ErrBadNormalisation, x.At)
			}
		}
	}
	return nil
}

// quoteBytes renders s as the JSON string literal a writer would have
// emitted for it, byte for byte: the two-character escapes for the
// characters that have them, \u00xx for the other control characters,
// and every other byte as it is, valid or not.
func quoteBytes(s []byte) []byte {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for _, c := range s {
		switch c {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\b':
			out = append(out, '\\', 'b')
		case '\f':
			out = append(out, '\\', 'f')
		case '\n':
			out = append(out, '\\', 'n')
		case '\r':
			out = append(out, '\\', 'r')
		case '\t':
			out = append(out, '\\', 't')
		default:
			if c < 0x20 {
				out = append(out, fmt.Sprintf(`\u%04x`, c)...)
			} else {
				out = append(out, c)
			}
		}
	}
	return append(out, '"')
}

// replaceInvalidUTF8 replaces each maximal subpart of an ill-formed
// UTF-8 sequence in s with one U+FFFD, the substitution Unicode §3.9
// recommends and the WHATWG decoder performs, so a writer in another
// language produces the same bytes.
func replaceInvalidUTF8(s []byte) []byte {
	out := make([]byte, 0, len(s))
	for len(s) > 0 {
		r, size := utf8.DecodeRune(s)
		if r != utf8.RuneError || size > 1 {
			out = append(out, s[:size]...)
			s = s[size:]
			continue
		}
		out = utf8.AppendRune(out, utf8.RuneError)
		s = s[maximalSubpart(s):]
	}
	return out
}

// maximalSubpart returns the length of the maximal subpart of the
// ill-formed sequence at the start of s: the longest prefix that could
// begin a well-formed sequence, or one byte when nothing can.
func maximalSubpart(s []byte) int {
	c := s[0]
	var need int
	var lo, hi byte = 0x80, 0xBF
	switch {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
		need = 2
	case c == 0xED:
		need, hi = 2, 0x9F
	case c == 0xF0:
		need, lo = 3, 0x90
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	case c == 0xF4:
		need, hi = 3, 0x8F
	default:
		return 1
	}
	i := 1
	for ; i <= need && i < len(s); i++ {
		if s[i] < lo || s[i] > hi {
			return i
		}
		lo, hi = 0x80, 0xBF
	}
	return i
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

// repairStrings finds every Go string in e that is not valid UTF-8,
// which the marshaller would otherwise repair silently, and replaces
// each with a sentinel so its place in the marshalled body can be
// found and its bytes recorded. normaliseEntry sets each located one
// to its repaired form and puts every other back as it was, on error
// too, so a failed append leaves the entry untouched. A string it
// cannot set, inside an unexported field or a struct held by value in
// a map or an interface, is an error rather than a silent repair.
func repairStrings(e Entry) (map[string]stringRepair, error) {
	var found map[string]stringRepair
	var walk func(v reflect.Value) error
	walk = func(v reflect.Value) error {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if v.IsNil() {
				return nil
			}
			return walk(v.Elem())
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if !t.Field(i).IsExported() {
					continue
				}
				if err := walk(v.Field(i)); err != nil {
					return err
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return nil // raw bytes, which normaliseJSON handles
			}
			for i := 0; i < v.Len(); i++ {
				if err := walk(v.Index(i)); err != nil {
					return err
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				if k.Kind() == reflect.String && !utf8.ValidString(k.String()) {
					return errors.New("agentsession: a member name is not valid UTF-8")
				}
				mv := v.MapIndex(k)
				if mv.Kind() == reflect.String {
					if !utf8.ValidString(mv.String()) {
						s := newSentinel(&found, mv.String())
						v.SetMapIndex(k, reflect.ValueOf(s).Convert(mv.Type()))
					}
					continue
				}
				if err := walk(mv); err != nil {
					return err
				}
			}
		case reflect.String:
			if utf8.ValidString(v.String()) {
				return nil
			}
			if !v.CanSet() {
				return errors.New("agentsession: a string that is not valid UTF-8 cannot be repaired in place")
			}
			v.SetString(newSentinel(&found, v.String()))
		}
		return nil
	}
	if err := walk(reflect.ValueOf(e)); err != nil {
		restoreStrings(e, found, nil)
		return nil, err
	}
	return found, nil
}

func newSentinel(found *map[string]stringRepair, s string) string {
	if *found == nil {
		*found = map[string]stringRepair{}
	}
	sentinel := fmt.Sprintf("\x00\x01agentsession:normalise:%d\x01\x00", len(*found))
	(*found)[sentinel] = stringRepair{original: []byte(s), repaired: string(replaceInvalidUTF8([]byte(s)))}
	return sentinel
}

// restoreStrings replaces each sentinel left in e: with the repaired
// string when located holds it, since that is what was written, and
// with the original otherwise. With a nil located every string goes
// back as it was.
func restoreStrings(e Entry, sentinels map[string]stringRepair, located map[string]bool) {
	if len(sentinels) == 0 {
		return
	}
	pick := func(s string) (string, bool) {
		r, ok := sentinels[s]
		if !ok {
			return "", false
		}
		if located[s] {
			return r.repaired, true
		}
		return string(r.original), true
	}
	var walk func(v reflect.Value)
	walk = func(v reflect.Value) {
		switch v.Kind() {
		case reflect.Pointer, reflect.Interface:
			if !v.IsNil() {
				walk(v.Elem())
			}
		case reflect.Struct:
			t := v.Type()
			for i := 0; i < v.NumField(); i++ {
				if t.Field(i).IsExported() {
					walk(v.Field(i))
				}
			}
		case reflect.Slice, reflect.Array:
			if v.Type().Elem().Kind() != reflect.Uint8 {
				for i := 0; i < v.Len(); i++ {
					walk(v.Index(i))
				}
			}
		case reflect.Map:
			for _, k := range v.MapKeys() {
				mv := v.MapIndex(k)
				if mv.Kind() == reflect.String {
					if s, ok := pick(mv.String()); ok {
						v.SetMapIndex(k, reflect.ValueOf(s).Convert(mv.Type()))
					}
					continue
				}
				walk(mv)
			}
		case reflect.String:
			if s, ok := pick(v.String()); ok && v.CanSet() {
				v.SetString(s)
			}
		}
	}
	walk(reflect.ValueOf(e))
}

// normaliseEntry rewrites e's body as the format has a writer do
// before writing, when the body does not pass the I-JSON test as
// marshalled or holds a Go string the marshaller would repair, and
// records the changes on the entry beside any the caller recorded,
// sorted as the format requires. The rewritten line is decoded back
// into e, so what the caller holds is what is hashed and written. It
// returns the marshalled line when nothing needed rewriting, so the
// hash need not marshal again, and nil after a rewrite. On error the
// entry is left as the caller had it.
func normaliseEntry(e Entry) ([]byte, error) {
	prior := e.Base().Normalised
	if err := checkNormalisations(prior); err != nil {
		return nil, err
	}
	sentinels, err := repairStrings(e)
	if err != nil {
		return nil, err
	}
	data, err := MarshalEntry(e)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
	}
	if len(sentinels) == 0 && ijson.Check(data) == nil {
		sortNormalisations(prior)
		return data, nil
	}
	all, err := splitMembers(data)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
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
	delete(body, "normalised")
	bodyJSON, err := jsonx.MarshalNoEscape(body)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
	}
	out, changes, located, err := normaliseJSON(bodyJSON, sentinels)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, fmt.Errorf("agentsession: entry body: %w", err)
	}
	if len(changes) == 0 {
		// The strings that were not valid UTF-8 are ones the
		// marshaller leaves out, so the line is as it was.
		restoreStrings(e, sentinels, nil)
		sortNormalisations(prior)
		return nil, nil
	}
	norm := append(append([]Normalisation(nil), prior...), changes...)
	sortNormalisations(norm)
	head, err := jsonx.MarshalNoEscape(env)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
	}
	raw, err := json.Marshal(norm)
	if err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
	}
	line := jsonx.JoinObjects(head, out, map[string]json.RawMessage{"normalised": raw})
	// Decoded into a fresh entry first: a rounding the entry's own
	// type cannot hold, math.MaxInt64 to 2^63, fails partway, and a
	// decode into e would leave the members before it changed. The
	// line is right and the struct cannot carry it, so the entry is
	// refused as the caller had it.
	fresh := reflect.New(reflect.TypeOf(e).Elem()).Interface()
	if err := json.Unmarshal(line, fresh); err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, fmt.Errorf("agentsession: entry body: the normalised value does not fit the entry's type: %w", err)
	}
	if err := json.Unmarshal(line, e); err != nil {
		restoreStrings(e, sentinels, nil)
		return nil, err
	}
	// A string the marshaller left out is not in the line and so
	// still holds its sentinel; it goes back as it was.
	restoreStrings(e, sentinels, located)
	return nil, nil
}
