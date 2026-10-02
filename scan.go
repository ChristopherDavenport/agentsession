package agentsession

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// RawEntry is one entry line as [Scan] met it: its envelope, read from
// the line's own bytes, and the line, which Decode turns into an entry
// when asked.
type RawEntry struct {
	ID, Parent, Type string
	// Line is the line as read, without its terminator.
	Line []byte
	// Repeat is set for a line whose id an earlier line had, which a
	// reader treats as that same entry.
	Repeat bool
}

// Decode decodes the entry, as [Read] decodes each line. Its id is the
// one Scan verified.
func (r RawEntry) Decode() (Entry, error) {
	return UnmarshalEntry(r.Line)
}

// ErrScanMigrated is returned by [Scan] for a file of a minor before
// 0.5, whose entries carry no hashes to verify: [Read] migrates one.
var ErrScanMigrated = errors.New("agentsession: a file before 0.5 has no hashed ids to scan; read it")

// Scan reads a session file and verifies it as [Read] does, decoding no
// entry: every line passes the I-JSON test, every entry's id is the hash
// of its line's canonical bytes, its ts has the one spelling the format
// admits, its parent is null or an entry earlier in the file, its
// parents keep the convergence rules, in a file whose header names a
// base it keeps the base rule ([ErrBaseRule]), and the legacy_id and
// normalised any entry may carry have their types. The header is read and checked
// first and returned as the file declares it. A line met a second time
// is yielded with Repeat set. A last line cut short, as a crash
// mid-append leaves, ends the sequence with a [*TruncatedLine] error;
// any other failure ends it with its error, and the header's base not
// being in the file is reported once the file ends. Lines are numbered,
// and empty ones skipped, as Read numbers and skips them.
//
// Scan checks no more of an entry than that: what a core entry type
// requires of its own members is Decode's to check, as it is Read's.
// The sequence reads r as it goes, so it ranges once; a second range
// yields an error.
func Scan(r io.Reader) (Header, iter.Seq2[RawEntry, error], error) {
	br := bufio.NewReader(r)
	line := 0
	next := func() ([]byte, bool, error) {
		for {
			data, err := br.ReadBytes('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, false, fmt.Errorf("agentsession: read line %d: %w", line+1, err)
			}
			atEOF := err != nil
			data = bytes.TrimRight(data, "\r\n")
			if len(data) == 0 {
				if atEOF {
					return nil, true, nil
				}
				line++
				continue
			}
			line++
			return data, atEOF, nil
		}
	}
	first, atEOF, err := next()
	if err != nil {
		return Header{}, nil, err
	}
	if first == nil {
		return Header{}, nil, errors.New("agentsession: empty input")
	}
	if err := ijson.Check(first); err != nil && json.Valid(first) {
		return Header{}, nil, fmt.Errorf("agentsession: line %d: %w", line, err)
	}
	var h Header
	if err := json.Unmarshal(first, &h); err != nil {
		return Header{}, nil, fmt.Errorf("agentsession: header: %w", err)
	}
	if err := h.Validate(); err != nil {
		return Header{}, nil, err
	}
	if _, minor, _ := ParseFormat(h.Format); minor < hashedMinor {
		return Header{}, nil, fmt.Errorf("%w: %s", ErrScanMigrated, h.Format)
	}
	used := false
	seq := func(yield func(RawEntry, error) bool) {
		if used {
			yield(RawEntry{}, errors.New("agentsession: a Scan sequence ranges once"))
			return
		}
		used = true
		seen := map[string]bool{}
		rule := newBaseRule(h.Base)
		for !atEOF {
			var data []byte
			if data, atEOF, err = next(); err != nil {
				yield(RawEntry{}, err)
				return
			}
			if data == nil {
				break
			}
			e, err := scanLine(data, h.ID, seen, rule)
			if err != nil {
				// Only a last line that is not JSON is cut short, and a
				// line is the last only when nothing at all follows it.
				// The file ends there, so a base not yet met is not in it.
				if (atEOF || nothingLeft(br)) && !json.Valid(data) {
					if h.Base != "" && !seen[h.Base] {
						yield(RawEntry{}, fmt.Errorf("%w: base %s is not in the file", ErrNoEntry, h.Base))
						return
					}
					yield(RawEntry{}, &TruncatedLine{Line: line, Data: append([]byte(nil), data...), Err: err})
					return
				}
				yield(RawEntry{}, fmt.Errorf("agentsession: line %d: %w", line, err))
				return
			}
			if !yield(e, nil) {
				return
			}
		}
		if h.Base != "" && !seen[h.Base] {
			yield(RawEntry{}, fmt.Errorf("%w: base %s is not in the file", ErrNoEntry, h.Base))
		}
	}
	return h, seq, nil
}

// nothingLeft reports whether nothing at all is left to read, not even
// a blank line, which is how Read decides a line is the last.
func nothingLeft(br *bufio.Reader) bool {
	b, _ := br.Peek(1)
	return len(b) == 0
}

// scanLine verifies one entry line against the entries seen before it
// and adds it to them; rule holds it to the base rule in a file with
// one.
func scanLine(data []byte, session string, seen map[string]bool, rule *baseRule) (RawEntry, error) {
	if err := ijson.Check(data); err != nil {
		return RawEntry{}, err
	}
	c, err := jcs.Transform(data)
	if err != nil {
		return RawEntry{}, err
	}
	v, err := verifyLine(c)
	if err != nil {
		return RawEntry{}, err
	}
	e := RawEntry{ID: v.id, Parent: v.parent, Type: v.typ, Line: data}
	if seen[e.ID] {
		e.Repeat = true
		return e, nil
	}
	if e.Parent != "" && !seen[e.Parent] {
		return RawEntry{}, fmt.Errorf("entry %s: %w: parent %s", e.ID, ErrNoEntry, e.Parent)
	}
	if err := rule.line(e.ID, e.Parent); err != nil {
		return RawEntry{}, err
	}
	if p := v.parents; p != nil && string(p) != "null" {
		var refs []EntryRef
		if err := json.Unmarshal(p, &refs); err != nil {
			return RawEntry{}, fmt.Errorf("agentsession: entry %s: parents: %w", e.ID, err)
		}
		if err := checkRefs(e.ID, e.Parent, session, refs, func(id string) bool { return seen[id] }); err != nil {
			return RawEntry{}, err
		}
	}
	seen[e.ID] = true
	return e, nil
}

// lineEnvelope is what verifyLine reads of a line: parents is the
// member as the line holds it, which [EntryRef] decodes.
type lineEnvelope struct {
	id, parent, typ, content string
	parents                  []byte
}

// verifyLine checks a 0.5 entry line in its canonical form c, as both
// readers check it before they look at any other entry: the envelope's
// members have their types and ts its one spelling, the body carries
// none of the envelope's names, legacy_id and normalised have their
// types, and the id is the hash of the line. A root's parent is null;
// the empty string names no entry, so it is refused, not read as a root.
func verifyLine(c []byte) (lineEnvelope, error) {
	members, ok := canonicalMembers(c)
	if !ok {
		return lineEnvelope{}, errors.New("agentsession: entry is not an object")
	}
	var (
		e                                         lineEnvelope
		id, parent, ts, typ, legacyID, normalised []byte
	)
	for _, m := range members {
		switch m.key {
		case "id":
			id = m.val
		case "parent":
			parent = m.val
		case "parents":
			e.parents = m.val
		case "ts":
			ts = m.val
		case "type":
			typ = m.val
		case "content":
			return lineEnvelope{}, fmt.Errorf("%w: content", ErrReservedMember)
		case "legacy_id":
			legacyID = m.val
		case "normalised":
			normalised = m.val
		}
	}
	str := func(k string, v []byte) (string, error) {
		if v == nil || string(v) == "null" {
			return "", nil
		}
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' && bytes.IndexByte(v, '\\') < 0 {
			return string(v[1 : len(v)-1]), nil
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", fmt.Errorf("agentsession: %s is not a string", k)
		}
		return s, nil
	}
	var err error
	if e.typ, err = str("type", typ); err != nil {
		return lineEnvelope{}, err
	}
	if e.id, err = str("id", id); err != nil {
		return lineEnvelope{}, err
	}
	if e.parent, err = str("parent", parent); err != nil {
		return lineEnvelope{}, err
	}
	tsStr, err := str("ts", ts)
	if err != nil {
		return lineEnvelope{}, err
	}
	switch {
	case e.typ == "":
		return lineEnvelope{}, errors.New("agentsession: entry has no type")
	case e.id == "":
		return lineEnvelope{}, fmt.Errorf("%w: missing id", ErrBadID)
	case e.parent == "" && parent != nil && string(parent) != "null":
		return lineEnvelope{}, fmt.Errorf("%w: entry %s: parent is the empty string; a root's parent is null", ErrBadID, e.id)
	}
	if _, ok := ParseCanonicalTime(tsStr); !ok {
		return lineEnvelope{}, fmt.Errorf("%w: ts %q is not in the one form the format admits", ErrBadID, tsStr)
	}
	if _, _, err := commonBody(legacyID, normalised); err != nil {
		return lineEnvelope{}, fmt.Errorf("agentsession: entry %s: %w", e.id, err)
	}
	var hashed string
	if hashed, e.content = memberHashes(c, members); hashed != e.id {
		return lineEnvelope{}, fmt.Errorf("%w: line says %s, hashes to %s", ErrBadID, e.id, hashed)
	}
	return e, nil
}
