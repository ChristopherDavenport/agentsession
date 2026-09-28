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
// names no entry yet in this file, or a repeated ID is an error.
// The leaf is the last entry in the file unless a [LeafLabel] is in
// force, in which case it is the entry that label names.
func Read(r io.Reader) (*Session, error) {
	br := bufio.NewReader(r)
	var (
		s    *Session
		line int
		m    *migration
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
		if json.Valid(data) {
			// A line that is JSON but not I-JSON is refused as written,
			// before the decoder can repair it. A line that is not JSON
			// at all is left to the decoder below, which knows a
			// truncated last line from a broken middle one.
			if err := ijson.Check(data); err != nil {
				return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
			}
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
			if err := migrate(&h); err != nil {
				return nil, err
			}
			s = New(h)
			s.migrated = m != nil
		} else {
			e, form, err := decodeLine(data)
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
			if err := s.link(e, m, form); err != nil {
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
// line the id is verified against the hashes the format defines; for a
// line of an earlier minor the entry is rewritten — references to the
// hashes assigned earlier in the file, the old id to legacy_id, ts to
// its one spelling — and hashed. A repeated id is the same entry, kept
// once and reported.
//
// form is the entry's typed encoding as read, which a 0.5 line is
// hashed from rather than encoded again; nil encodes it.
func (s *Session) link(e Entry, m *migration, form []byte) error {
	b := e.Base()
	if m != nil {
		if err := m.rewrite(e, s); err != nil {
			return err
		}
	} else {
		if b.ID == "" {
			return fmt.Errorf("%w: missing id", ErrBadID)
		}
		if _, ok := ParseCanonicalTime(b.tsRaw); !ok {
			return fmt.Errorf("%w: ts %q is not in the one form the format admits", ErrBadID, b.tsRaw)
		}
		want := b.ID
		if form != nil && len(b.kept) > 0 {
			var err error
			if form, err = restoreKept(form, b.kept); err != nil {
				return err
			}
		}
		if err := s.hashEntry(e, form); err != nil {
			return err
		}
		if b.ID != want {
			return fmt.Errorf("%w: line says %s, hashes to %s", ErrBadID, want, b.ID)
		}
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
	if err := s.checkParents(b); err != nil {
		return err
	}
	s.add(e)
	return nil
}

// rewrite migrates one entry of an earlier minor version in place: every
// member that names an entry in this file takes the hash assigned to
// that entry earlier in the file, the old id moves to legacy_id, ts
// takes its one spelling with the instant unchanged, and the entry is
// hashed. An extension entry's members are opaque, so it is rewritten
// only in its envelope and recorded as unresolved.
func (m *migration) rewrite(e Entry, s *Session) error {
	b := e.Base()
	old := b.ID
	if old == "" {
		return fmt.Errorf("%w: missing id", ErrBadID)
	}
	b.LegacyID = old
	b.Timestamp = b.Timestamp.UTC().Truncate(time.Nanosecond)
	if b.Parent != "" {
		if id, ok := m.ids[b.Parent]; ok {
			b.Parent = id
		}
	}
	unresolved := false
	for i, r := range b.Parents {
		if r.Session == "" || r.Session == s.header.ID {
			if id, ok := m.ids[r.Entry]; ok {
				b.Parents[i].Entry = id
			} else {
				unresolved = true
			}
		} else {
			// A reference into another session names an entry by an id
			// this reader cannot rewrite; it keeps its string and the
			// file is not re-emitted as 0.5.
			unresolved = true
		}
	}
	ref := func(p *string) {
		if *p == "" {
			return
		}
		if id, ok := m.ids[*p]; ok {
			*p = id
		} else {
			unresolved = true
		}
	}
	switch v := e.(type) {
	case *ItemEntry:
		ref(&v.QueuedFrom)
	case *CompactionEntry:
		ref(&v.FirstKept)
	case *BranchSummaryEntry:
		ref(&v.From)
	case *LabelEntry:
		ref(&v.Target)
	case *OutcomeEntry:
		ref(&v.Target)
	case *DispatchEntry:
		ref(&v.Target)
	case *DecisionEntry:
		ref(&v.Target)
	case *UnknownEntry:
		unresolved = true
	}
	if unresolved {
		s.unresolved = append(s.unresolved, old)
	}
	b.ID = ""
	if u, ok := e.(*UnknownEntry); ok {
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return err
		}
		u.Raw = raw
	}
	if err := s.hashEntry(e, nil); err != nil {
		return err
	}
	if u, ok := e.(*UnknownEntry); ok {
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return err
		}
		u.Raw = raw
	}
	m.ids[old] = b.ID
	return nil
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
	for _, k := range commonBodyKeys {
		delete(all, k)
	}
	for k, v := range u.extraMembers() {
		if k == "legacy_id" || k == "normalised" {
			all[k] = v
		}
	}
	return jsonx.JoinObjects(head, []byte("{}"), all), nil
}

// setClock replaces the time source, for tests.
func (s *Session) setClock(now func() time.Time) { s.now = now }
