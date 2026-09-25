package agentsession

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

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
// other malformed line, a missing parent or a repeated ID is an error.
// The leaf is the last entry in the file unless a [LeafLabel] is in
// force, in which case it is the entry that label names.
func Read(r io.Reader) (*Session, error) {
	br := bufio.NewReader(r)
	var (
		s    *Session
		line int
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
		if s == nil {
			var h Header
			if err := json.Unmarshal(data, &h); err != nil {
				return nil, fmt.Errorf("agentsession: header: %w", err)
			}
			if err := h.Validate(); err != nil {
				return nil, err
			}
			if err := migrate(&h); err != nil {
				return nil, err
			}
			s = New(h)
		} else {
			e, err := UnmarshalEntry(data)
			if err != nil {
				if !atEOF {
					// Only the last line may be broken; see whether more
					// non-blank lines follow.
					rest, _ := br.Peek(1)
					if len(rest) > 0 {
						return nil, fmt.Errorf("agentsession: line %d: %w", line, err)
					}
				}
				s.truncated = &TruncatedLine{Line: line, Data: append([]byte(nil), data...), Err: err}
				break
			}
			if err := s.link(e); err != nil {
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
	s.leaf = s.resolveLeaf()
	return s, nil
}

// link adds a decoded entry, checking the tree invariants.
func (s *Session) link(e Entry) error {
	b := e.Base()
	if _, taken := s.byID[b.ID]; taken {
		return fmt.Errorf("%w: %s", ErrDuplicateEntry, b.ID)
	}
	if b.Parent != "" {
		if _, ok := s.byID[b.Parent]; !ok {
			return fmt.Errorf("entry %s: %w: parent %s", b.ID, ErrNoEntry, b.Parent)
		}
	}
	s.add(e)
	return nil
}

// Write encodes the session as JSONL: the header, then every entry in
// file order, one per line.
func Write(w io.Writer, s *Session) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
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

// writeLine writes v as one JSON line.
func writeLine(w io.Writer, v any) error {
	data, err := jsonx.MarshalNoEscape(v)
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
	env := envelope{Type: u.Type, ID: u.ID, TS: u.Timestamp}
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
	return jsonx.JoinObjects(head, []byte("{}"), all), nil
}

// setClock replaces the time source, for tests.
func (s *Session) setClock(now func() time.Time) { s.now = now }
