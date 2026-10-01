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
// admits, and its parent, and every predecessor it converges in this
// session, is an entry earlier in the file. The header is read and
// checked first and returned as the file declares it. A line met a
// second time is yielded with Repeat set. A last line cut short, as a
// crash mid-append leaves, ends the sequence with a [*TruncatedLine]
// error; any other failure ends it with its error, and the header's
// base not being in the file is reported once the file ends.
//
// Scan checks no more of an entry than its envelope: what a core entry
// type requires of its members is Decode's to check, as it is Read's.
func Scan(r io.Reader) (Header, iter.Seq2[RawEntry, error], error) {
	br := bufio.NewReader(r)
	line := 0
	next := func() ([]byte, bool, error) {
		for {
			data, err := br.ReadBytes('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return nil, false, err
			}
			atEOF := err != nil
			data = bytes.TrimRight(data, "\r\n")
			if len(bytes.TrimSpace(data)) == 0 {
				if atEOF {
					return nil, true, nil
				}
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
	if err := ijson.Check(first); err != nil {
		return Header{}, nil, fmt.Errorf("agentsession: line 1: %w", err)
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
	seq := func(yield func(RawEntry, error) bool) {
		seen := map[string]bool{}
		for !atEOF {
			var data []byte
			if data, atEOF, err = next(); err != nil {
				yield(RawEntry{}, err)
				return
			}
			if data == nil {
				break
			}
			e, err := scanLine(data, h.ID, seen)
			if err != nil {
				// Only a last line that is not JSON is cut short.
				if atEOF || isLast(br) {
					if !json.Valid(data) {
						yield(RawEntry{}, &TruncatedLine{Line: line, Data: append([]byte(nil), data...), Err: err})
						return
					}
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

// isLast reports whether nothing but blank space is left to read.
func isLast(br *bufio.Reader) bool {
	for {
		b, err := br.Peek(1)
		if err != nil || len(b) == 0 {
			return true
		}
		if b[0] != '\n' && b[0] != '\r' && b[0] != ' ' && b[0] != '\t' {
			return false
		}
		br.ReadByte()
	}
}

// scanLine verifies one entry line against the entries seen before it
// and adds it to them.
func scanLine(data []byte, session string, seen map[string]bool) (RawEntry, error) {
	if err := ijson.Check(data); err != nil {
		return RawEntry{}, err
	}
	c, err := jcs.Transform(data)
	if err != nil {
		return RawEntry{}, err
	}
	members, ok := canonicalMembers(c)
	if !ok {
		return RawEntry{}, errors.New("agentsession: entry is not an object")
	}
	var env struct{ id, parent, parents, ts, typ, content []byte }
	for _, m := range members {
		switch m.key {
		case "id":
			env.id = m.val
		case "parent":
			env.parent = m.val
		case "parents":
			env.parents = m.val
		case "ts":
			env.ts = m.val
		case "type":
			env.typ = m.val
		case "content":
			env.content = m.val
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
	var e RawEntry
	if e.Type, err = str("type", env.typ); err != nil {
		return RawEntry{}, err
	}
	if e.ID, err = str("id", env.id); err != nil {
		return RawEntry{}, err
	}
	if e.Parent, err = str("parent", env.parent); err != nil {
		return RawEntry{}, err
	}
	ts, err := str("ts", env.ts)
	if err != nil {
		return RawEntry{}, err
	}
	switch {
	case e.Type == "":
		return RawEntry{}, errors.New("agentsession: entry has no type")
	case e.ID == "":
		return RawEntry{}, fmt.Errorf("%w: missing id", ErrBadID)
	case env.content != nil:
		return RawEntry{}, fmt.Errorf("%w: entry %s carries the reserved member content", ErrBadID, e.ID)
	}
	if _, ok := ParseCanonicalTime(ts); !ok {
		return RawEntry{}, fmt.Errorf("%w: ts %q is not in the one form the format admits", ErrBadID, ts)
	}
	id, _, err := entryHashesCanonical(c)
	if err != nil {
		return RawEntry{}, err
	}
	if id != e.ID {
		return RawEntry{}, fmt.Errorf("%w: line says %s, hashes to %s", ErrBadID, e.ID, id)
	}
	e.Line = data
	if seen[e.ID] {
		e.Repeat = true
		return e, nil
	}
	if e.Parent != "" && !seen[e.Parent] {
		return RawEntry{}, fmt.Errorf("entry %s: %w: parent %s", e.ID, ErrNoEntry, e.Parent)
	}
	if p := env.parents; p != nil {
		var refs []EntryRef
		if err := json.Unmarshal(p, &refs); err != nil {
			return RawEntry{}, fmt.Errorf("%w: entry %s: parents: %v", ErrBadConvergence, e.ID, err)
		}
		for _, r := range refs {
			if (r.Session == "" || r.Session == session) && !seen[r.Entry] {
				return RawEntry{}, fmt.Errorf("%w: entry %s converges %s, which is not in this session yet", ErrBadConvergence, e.ID, r.Entry)
			}
		}
	}
	seen[e.ID] = true
	return e, nil
}
