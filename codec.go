package agentsession

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// TruncatedLine describes a final line that did not parse.
type TruncatedLine struct {
	// Line is the 1-based line number in the file.
	Line int
	// Data is the line as read, without its terminator.
	Data []byte
	// Err is the decode error.
	Err error
}

// Error implements error.
func (t *TruncatedLine) Error() string {
	return fmt.Sprintf("agentsession: line %d truncated: %v", t.Line, t.Err)
}

// Unwrap returns the decode error.
func (t *TruncatedLine) Unwrap() error { return t.Err }

// Read decodes a session from its JSONL form. A final line that does not
// parse is tolerated and reported through [Session.Truncated]; any
// other malformed line, a missing parent, a convergence reference that
// names no entry yet in this file, a line that breaks the base rule
// ([ErrBaseRule]) in a file whose header names one, or a repeated ID is
// an error.
// The leaf is the last entry in the file unless a [LeafLabel] is in
// force, in which case it is the entry that label names.
//
// Each line is read under the format the header declares, and the
// header is brought up to this package's format in memory unless the
// file may not be raised ([ErrNoRaise]): then it keeps its own, or for
// a file before 0.5, whose entries are rehashed as 0.5's, 0.12. Before
// 0.13 an entry's redacted member is unknown whatever its form
// ([RedactedOf]).
func Read(r io.Reader) (*Session, error) {
	br := bufio.NewReader(r)
	var (
		s    *Session
		line int
		m    *migration
		rule *baseRule
	)
	for {
		data, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("agentsession: read line %d: %w", line+1, err)
		}
		atEOF := errors.Is(err, io.EOF)
		data = bytes.TrimRight(data, "\r\n")
		if len(data) == 0 {
			if atEOF {
				break
			}
			line++
			continue // blank lines are not part of the format but cost nothing to skip
		}
		line++
		// A line that is JSON but not I-JSON is refused as written,
		// before the decoder can repair it. A line that is not JSON at
		// all is left to the decoder below, which knows a truncated last
		// line from a broken middle one. The check passes almost every
		// line, and one it passes is JSON, so validity is asked only of
		// a line it fails.
		if err := ijson.Check(data); err != nil && json.Valid(data) {
			return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
		}
		if s == nil {
			var h Header
			if err := json.Unmarshal(data, &h); err != nil {
				return nil, fmt.Errorf("agentsession: header: %w", err)
			}
			if err := h.Validate(); err != nil {
				return nil, err
			}
			_, minor, _ := ParseFormat(h.Format)
			if minor < hashedMinor {
				m = &migration{ids: map[string]string{}}
			}
			declared := h.Format
			if err := migrate(&h); err != nil {
				return nil, err
			}
			s = New(h)
			s.migrated = m != nil
			s.declared = declared
			rule = newBaseRule(h.Base)
		} else {
			e, c, err := decodeLine(data)
			if err != nil {
				// Only a last line that is not JSON is a truncated line,
				// which a crash mid-append leaves behind. A last line
				// that parses and is refused for what it says is an
				// error like any other line's.
				if !atEOF {
					rest, _ := br.Peek(1)
					if len(rest) > 0 {
						return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
					}
				}
				if json.Valid(data) {
					return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
				}
				s.truncated = &TruncatedLine{Line: line, Data: append([]byte(nil), data...), Err: err}
				break
			}
			if err := s.link(e, m, data, c, rule); err != nil {
				return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
			}
		}
		if atEOF {
			break
		}
	}
	if s == nil {
		return nil, errors.New("agentsession: empty input")
	}
	if s.header.Base != "" {
		if _, ok := s.byID[s.header.Base]; !ok {
			return nil, fmt.Errorf("%w: base %s is not in the file", ErrNoEntry, s.header.Base)
		}
		s.prefix = map[string]bool{}
		for _, e := range s.path(s.header.Base) {
			s.prefix[e.Base().ID] = true
		}
	}
	s.leaf = s.resolveLeaf()
	return s, nil
}

// migration carries the state of an in-memory migration from an earlier
// minor version: the map from each entry's old id to its new hash.
type migration struct {
	ids map[string]string
}

// link adds a decoded entry, checking the tree invariants. For a 0.5
// line the envelope and the id are verified from the line's canonical
// bytes, c when decoding came by them and data canonicalised otherwise,
// as Scan verifies them: the id is the hash of the line, not of what the
// entry's fields encode. For a line of an earlier minor the entry is
// rewritten — references to the hashes assigned earlier in the file, the
// old id to legacy_id, ts to its one spelling — and hashed. A repeated
// id is the same entry, kept once and reported. rule, for a file with a
// base, holds the line to the base rule, as Scan holds it.
func (s *Session) link(e Entry, m *migration, data, c []byte, rule *baseRule) error {
	// The line is read under the format the header declared: before
	// 0.13 a redacted member is unknown whatever its form.
	gateRedacted(e, s.declared)
	if m != nil {
		var err error
		if e, err = m.rewrite(e, s); err != nil {
			return err
		}
	}
	b := e.Base()
	if m == nil {
		if c == nil {
			var err error
			if c, err = jcs.Transform(data); err != nil {
				return err
			}
		}
		v, err := verifyLine(c)
		if err != nil {
			return err
		}
		b.ID, b.content = v.id, v.content
	}
	if _, taken := s.byID[b.ID]; taken {
		s.repeated = append(s.repeated, b.ID)
		return nil
	}
	if b.Parent != "" {
		if _, ok := s.byID[b.Parent]; !ok {
			return fmt.Errorf("entry %s: %w: parent %s", b.ID, ErrNoEntry, b.Parent)
		}
	}
	if err := rule.line(b.ID, b.Parent); err != nil {
		return err
	}
	if err := s.checkParents(b); err != nil {
		return err
	}
	s.add(e)
	return nil
}

// rewrite migrates one entry of an earlier minor version: the old id
// moves to legacy_id, ts takes its one spelling with the instant
// unchanged, every member that names an entry in this file takes the
// hash assigned to that entry earlier in the file, through
// [RemapRefs] with the order of parents kept, and the entry is hashed.
// It returns the migrated entry, which replaces e. A reference left as
// written, but parent, which link refuses when it names no entry, and
// an extension entry, whose members are opaque and which is rewritten
// only in its envelope, are recorded as unresolved.
func (m *migration) rewrite(e Entry, s *Session) (Entry, error) {
	b := e.Base()
	b.line = nil // the line is rewritten, so the fields write it
	old := b.ID
	if old == "" {
		return nil, fmt.Errorf("%w: missing id", ErrBadID)
	}
	b.LegacyID = old
	b.Timestamp = b.Timestamp.UTC().Truncate(time.Nanosecond)
	r, err := remapRefs(e, Remap{Entry: func(session, entry string) (string, bool) {
		// A reference into another session names an entry by an id
		// this reader cannot rewrite; it keeps its string and the file
		// is not re-emitted as 0.5.
		if session != "" && session != s.header.ID {
			return "", false
		}
		id, ok := m.ids[entry]
		return id, ok
	}}, false)
	if err != nil {
		return nil, err
	}
	unresolved := r.Opaque
	for _, ref := range r.Unmapped {
		if ref.At != "/parent" {
			unresolved = true
		}
	}
	if unresolved {
		s.unresolved = append(s.unresolved, old)
	}
	e = r.Entry
	b = e.Base()
	b.ID = ""
	if u, ok := e.(*UnknownEntry); ok {
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return nil, err
		}
		u.Raw = raw
	}
	if err := s.hashEntry(e, nil); err != nil {
		return nil, err
	}
	if u, ok := e.(*UnknownEntry); ok {
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return nil, err
		}
		u.Raw = raw
	}
	m.ids[old] = b.ID
	return e, nil
}

// Write encodes the session as JSONL: the header, then every entry in
// file order, one per line, each in its canonical form. Preservation is
// of members and not bytes, and the hashes are over canonical forms, so
// a canonical line is what a reader verifies most directly. A session
// migrated from an earlier minor version that holds an entry the
// migration could not rewrite is refused, since the format forbids
// re-emitting such a file as 0.5.
func Write(w io.Writer, s *Session) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.migrated && len(s.unresolved) > 0 {
		return fmt.Errorf("%w: %d entries the migration could not rewrite", ErrUnresolvedMigration, len(s.unresolved))
	}
	bw := bufio.NewWriter(w)
	if err := writeLine(bw, s.header); err != nil {
		return err
	}
	for _, e := range s.entries {
		if err := writeLine(bw, e); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// ErrUnresolvedMigration is returned by Write for a session migrated
// from an earlier minor version that holds an entry the migration could
// not rewrite.
var ErrUnresolvedMigration = errors.New("agentsession: migrated session holds entries that could not be rewritten")

// writeLine writes v as one canonical JSON line.
func writeLine(w io.Writer, v any) error {
	data, err := jsonx.MarshalNoEscape(v)
	if err != nil {
		return err
	}
	data, err = jcs.Transform(data)
	if err != nil {
		return err
	}
	if _, err := w.Write(data); err != nil {
		return err
	}
	_, err = w.Write([]byte{'\n'})
	return err
}

// rewriteEnvelope re-emits an unknown entry's raw line with the
// envelope values from its base, for an entry that was constructed in
// memory and had its ID, parent or timestamp assigned on append.
func rewriteEnvelope(u *UnknownEntry) ([]byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(u.Raw, &all); err != nil {
		return nil, fmt.Errorf("agentsession: unknown entry raw: %w", err)
	}
	env := envelope{Type: u.Type, ID: u.ID, Parents: u.Parents, TS: u.Timestamp}
	if env.Type == "" {
		env.Type = jsonx.PeekString(all["type"])
		u.Type = env.Type
	}
	if u.Parent != "" {
		env.Parent = &u.Parent
	}
	head, err := jsonx.MarshalNoEscape(env)
	if err != nil {
		return nil, err
	}
	for _, k := range envelopeKeys {
		delete(all, k)
	}
	extra := u.extraMembers()
	for _, k := range commonBodyKeys {
		if v, ok := extra[k]; ok {
			all[k] = v
			continue
		}
		// Unset on the base: a member the line holds as null or empty
		// says the same and stays as written, since it is hashed.
		if raw, ok := all[k]; ok {
			if v, err := parseValue(raw); err != nil || !isZeroValue(v) {
				delete(all, k)
			}
		}
	}
	return jsonx.JoinObjects(head, []byte("{}"), all), nil
}

// setClock replaces the time source, for tests.
func (s *Session) setClock(now func() time.Time) { s.now = now }
