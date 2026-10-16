package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// Span is one element of an entry's redacted member: the entries of the
// original from From to To, inclusive, by position in the original's
// file order, prefix included and counting from 1. A single position
// has From == To and is written as the number n; a range has From < To
// and is written as [from, to].
type Span struct {
	From, To int
}

// MarshalJSON writes a single position as n and a range as [from, to].
func (s Span) MarshalJSON() ([]byte, error) {
	if s.From == s.To {
		return []byte(strconv.Itoa(s.From)), nil
	}
	return []byte("[" + strconv.Itoa(s.From) + "," + strconv.Itoa(s.To) + "]"), nil
}

// ErrBadRedacted is returned by [SetRedacted] for spans not in the form
// the format gives an entry's redacted member: each position at least
// 1, each range's From below its To, in ascending order and not
// overlapping.
var ErrBadRedacted = errors.New("agentsession: malformed redacted")

// checkSpans reports whether spans are in the form the format gives an
// entry's redacted member, as [ErrBadRedacted] says.
func checkSpans(spans []Span) error {
	last := 0
	for i, s := range spans {
		switch {
		case s.From < 1:
			return fmt.Errorf("%w: element %d starts at %d", ErrBadRedacted, i, s.From)
		case s.To < s.From:
			return fmt.Errorf("%w: element %d ends at %d before it starts at %d", ErrBadRedacted, i, s.To, s.From)
		case s.From <= last:
			return fmt.Errorf("%w: element %d starts at %d, not after %d", ErrBadRedacted, i, s.From, last)
		}
		last = s.To
	}
	return nil
}

// parseRedacted decodes a redacted member as the format defines it, and
// reports false for a value in any other form: a list whose elements are
// a position n, an integer of at least 1, or a range [from, to] of two
// integers with 1 <= from < to, in ascending order and not overlapping.
// Numbers are read by their value, as the canonical form the hash is
// over writes them, so a line spelling a position 3.0 reads as one
// spelling it 3, as the two share an id.
func parseRedacted(raw json.RawMessage) ([]Span, bool) {
	c, err := jcs.Transform(raw)
	if err != nil {
		return nil, false
	}
	var elems []json.RawMessage
	if json.Unmarshal(c, &elems) != nil || elems == nil {
		return nil, false // not a list, or null
	}
	spans := make([]Span, 0, len(elems))
	for _, el := range elems {
		if n, ok := position(el); ok {
			spans = append(spans, Span{n, n})
			continue
		}
		var pair []json.RawMessage
		if json.Unmarshal(el, &pair) != nil || len(pair) != 2 {
			return nil, false
		}
		from, ok1 := position(pair[0])
		to, ok2 := position(pair[1])
		if !ok1 || !ok2 || from >= to {
			return nil, false
		}
		spans = append(spans, Span{from, to})
	}
	if checkSpans(spans) != nil {
		return nil, false
	}
	return spans, true
}

// position decodes a canonical JSON number that is an integer of at
// least 1: its canonical form is then digits alone.
func position(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return 0, false
	}
	for _, b := range raw {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(string(raw))
	return n, err == nil
}

// takeRedacted takes a redacted member the line holds in the form the
// format defines into the base's typed field, out of Unknown; one in
// another form stays in Unknown as written. An extension entry keeps
// its members in its raw line and has no Unknown, so there it only sets
// the field.
func (b *EntryBase) takeRedacted(all map[string]json.RawMessage) {
	b.redacted, b.isRedacted = nil, false
	raw, ok := all["redacted"]
	if !ok {
		return
	}
	spans, ok := parseRedacted(raw)
	if !ok {
		return
	}
	b.redacted, b.isRedacted = spans, true
	if _, held := b.Unknown["redacted"]; held {
		delete(b.Unknown, "redacted")
		if len(b.Unknown) == 0 {
			b.Unknown = nil
		}
	}
}

// redactedJSON is the typed redacted member as written.
func (b *EntryBase) redactedJSON() json.RawMessage {
	spans := b.redacted
	if spans == nil {
		spans = []Span{}
	}
	raw, _ := json.Marshal(spans) // a Span always encodes
	return raw
}

// RedactedOf reports whether e is a redacted entry, one that carries
// the redacted member in the form the format defines, and returns its
// spans: the positions, in the original's file order, of the entries e
// was made from. An entry the redaction added, made from none, is
// redacted with no spans. A member in another form, or one read from a
// file whose header names a minor before 0.13, where the member is not
// defined, is one this package does not know: it stays in Unknown (or,
// on an extension entry, in its raw line) as written, and RedactedOf
// reports false. [Read], [RawEntry.Decode] and [Session.Extend] apply
// the header's minor; [UnmarshalEntry], given a line alone, decodes it
// as this package writes it. The spans are the entry's own, so a caller
// must not modify them.
func RedactedOf(e Entry) ([]Span, bool) {
	if e == nil {
		return nil, false
	}
	b := e.Base()
	if !b.isRedacted {
		return nil, false
	}
	if b.redacted == nil {
		return []Span{}, true
	}
	return b.redacted, true
}

// SetRedacted marks e as a redacted entry made from the entries of the
// original that spans name, as a redactor marks every entry it writes:
// spans in the form [ErrBadRedacted] says, each a position (From ==
// To) or a range, by position in the original's file order counting
// from 1. Nil or empty spans mark an entry the redaction added. It
// replaces any redacted member e held, in whatever form, and refuses
// spans not in the form with ErrBadRedacted, leaving e as it was. It
// changes e, so it is for an entry not yet appended, whose hashes the
// session computes when it commits it.
func SetRedacted(e Entry, spans []Span) error {
	if e == nil {
		return errors.New("agentsession: nil entry")
	}
	if err := checkSpans(spans); err != nil {
		return err
	}
	b := e.Base()
	b.redacted, b.isRedacted = append([]Span{}, spans...), true
	return setRawRedacted(e, b.redactedJSON())
}

// ClearRedacted removes e's redacted member, in whatever form it held
// it, as a writer does to an entry it appends natively from one it
// took from a redacted session. Like SetRedacted, it is for an entry
// not yet appended.
func ClearRedacted(e Entry) error {
	if e == nil {
		return errors.New("agentsession: nil entry")
	}
	b := e.Base()
	b.redacted, b.isRedacted = nil, false
	return setRawRedacted(e, nil)
}

// setRawRedacted makes the members e keeps as written agree with its
// typed redacted member: an Unknown one is dropped, as the typed field
// writes it or e is to hold none, and an extension entry's raw line
// holds raw, or no redacted member when raw is nil. The line the entry
// was read with no longer applies.
func setRawRedacted(e Entry, raw json.RawMessage) error {
	b := e.Base()
	b.line = nil
	delete(b.kept, "redacted")
	if _, held := b.Unknown["redacted"]; held {
		delete(b.Unknown, "redacted")
		if len(b.Unknown) == 0 {
			b.Unknown = nil
		}
	}
	u, ok := e.(*UnknownEntry)
	if !ok || len(u.Raw) == 0 {
		return nil
	}
	all, err := splitMembers(u.Raw)
	if err != nil {
		return err
	}
	if raw == nil {
		delete(all, "redacted")
	} else {
		all["redacted"] = raw
	}
	u.Raw = jsonx.JoinObjects([]byte("{}"), []byte("{}"), all)
	return nil
}

// gateRedacted applies the minor gate to a decoded entry read from a
// file whose header named format: before 0.13 the redacted member is
// not defined, so a redacted member in any form stays a member this
// package does not know, kept as written, and the entry is not
// redacted. It reports whether the entry holds a redacted member the
// gate leaves unknown, which forbids raising the file.
func gateRedacted(e Entry, format string) bool {
	if definesRedactedFrom(format) {
		return false
	}
	return demoteRedacted(e)
}

// demoteRedacted moves a typed redacted member into the members e keeps
// as written, as the typed field wrote it, so e writes the same bytes
// and is not redacted. It reports whether e holds a redacted member in
// any form afterwards.
func demoteRedacted(e Entry) bool {
	b := e.Base()
	if b.isRedacted {
		raw := b.redactedJSON()
		b.redacted, b.isRedacted = nil, false
		if _, ok := e.(*UnknownEntry); !ok {
			if b.Unknown == nil {
				b.Unknown = map[string]json.RawMessage{}
			}
			b.Unknown["redacted"] = raw
		}
		return true
	}
	return holdsRedactedMember(e)
}

// holdsRedactedMember reports whether e holds a redacted member that is
// not typed: in Unknown, or in an extension entry's raw line.
func holdsRedactedMember(e Entry) bool {
	if _, ok := e.Base().Unknown["redacted"]; ok {
		return true
	}
	u, ok := e.(*UnknownEntry)
	if !ok || len(u.Raw) == 0 || !bytes.Contains(u.Raw, []byte(`"redacted"`)) {
		return false
	}
	all, err := splitMembers(u.Raw)
	if err != nil {
		return false
	}
	_, held := all["redacted"]
	return held
}

// keepRedactedForm carries in's redacted member's standing onto out, an
// entry a helper made from in by way of its JSON: a member in did not
// hold typed, unknown or read under a minor before 0.13, is not typed in
// out either, which decoding the JSON alone would make it.
func keepRedactedForm(in, out Entry) {
	if _, ok := RedactedOf(in); !ok {
		demoteRedacted(out)
	}
}
