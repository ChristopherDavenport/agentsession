package cas

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

// journalRecord is one line of the journal: one commit.
//
// A record ends in a CRC-32C of the bytes before it, so a record
// damaged after it was written is found rather than read as something
// else. A crash cuts a record short only at the end of the journal,
// where it is not yet a line, and the next record appended lands after
// the torn bytes on the same line; so a whole line holding no record
// that reads is damage, never a torn write. A record written before the
// checksum existed has none and is taken as it reads.
type journalRecord struct {
	Op      string `json:"op"` // create, append, head, mark, delete
	Session string `json:"session"`
	Entry   string `json:"entry,omitempty"`
	Head    string `json:"head,omitempty"`
	Seq     int    `json:"seq,omitempty"`
	// Base is a created session's base, so the sweep can keep the
	// prefix of a session created after it looked at the headers.
	Base string `json:"base,omitempty"`
	// Mark is the record mark a mark record sets.
	Mark string `json:"mark,omitempty"`
	// Size is the bytes of the entry's envelope and content.
	Size int64 `json:"size,omitempty"`
	// Lazy is set on an append acknowledged before it was durable. Its
	// objects were not fsynced ahead of the record, so after a crash a
	// lazy record whose objects are missing is an append that was lost,
	// not damage.
	Lazy bool `json:"lazy,omitempty"`

	checked bool // it carried a checksum that matched
}

var crcTable = crc32.MakeTable(crc32.Castagnoli)

const crcMember = `,"crc":"`

// encode renders the record as one journal line with its checksum.
func (r journalRecord) encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	body := data[:len(data)-1] // without the closing brace
	sum := crc32.Checksum(body, crcTable)
	line := make([]byte, 0, len(data)+20)
	line = append(line, body...)
	line = append(line, crcMember...)
	line = strconv.AppendUint(line, uint64(sum), 16)
	line = append(line, '"', '}', '\n')
	return line, nil
}

// decodeRecord parses one record. torn is set for bytes a crashed write
// cut short, which recovery skips; any other failure is damage.
func decodeRecord(seg []byte) (rec journalRecord, torn bool, err error) {
	seg = bytes.TrimRight(seg, "\n")
	if len(seg) == 0 || seg[len(seg)-1] != '}' {
		return rec, true, errors.New("cut short")
	}
	if k := bytes.LastIndex(seg, []byte(crcMember)); k >= 0 {
		tail := seg[k+len(crcMember):]
		if len(tail) < 3 || tail[len(tail)-2] != '"' {
			return rec, false, errors.New("malformed checksum")
		}
		want, perr := strconv.ParseUint(string(tail[:len(tail)-2]), 16, 32)
		if perr != nil {
			return rec, false, errors.New("malformed checksum")
		}
		if crc32.Checksum(seg[:k], crcTable) != uint32(want) {
			return rec, false, errors.New("checksum mismatch")
		}
	}
	if err := json.Unmarshal(seg, &rec); err != nil {
		return rec, false, err
	}
	rec.checked = bytes.Contains(seg, []byte(crcMember))
	if rec.Op == "" || rec.Session == "" {
		return rec, false, errors.New("record names no operation or session")
	}
	return rec, false, nil
}

// JournalDamage is a journal record that was written whole and no
// longer reads: a flipped bit, or a restore that put back part of a
// file. Recovery never takes it for a torn write and never removes what
// the logs hold because of it.
type JournalDamage struct {
	Line   int   // 1-based
	Offset int64 // the byte offset of the line
	Err    error
}

func (d JournalDamage) Error() string {
	return fmt.Sprintf("cas: journal line %d (offset %d) is damaged: %v", d.Line, d.Offset, d.Err)
}

// sessionState is what the journal says about one session since its
// last create or delete.
type sessionState struct {
	recs    []journalRecord
	deleted bool
	created bool   // a create record opens what recs holds
	base    string // the create record's base, when it carries one
}

// entries returns the own entries the records append, each once, in
// journal order.
func (st *sessionState) entries() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range st.recs {
		if r.Op == "append" && r.Entry != "" && !seen[r.Entry] {
			seen[r.Entry] = true
			out = append(out, r.Entry)
		}
	}
	return out
}

// mark returns the last mark the records set.
func (st *sessionState) mark() string {
	m := ""
	for _, r := range st.recs {
		if r.Op == "mark" {
			m = r.Mark
		}
	}
	return m
}

// journalScan is the journal read from a byte offset.
type journalScan struct {
	states map[string]*sessionState
	damage []JournalDamage
	end    int64 // where the read stopped: the next record's offset
}

// replay reads the journal from the start and returns each session's
// state as the records say, in order, with a create or delete record
// clearing what came before it. A record a crashed process cut short
// is skipped, and if another process's record landed on the same line
// after the torn bytes, that record is found and counted. A damaged
// record is reported and skipped. Nothing is ever truncated: the
// journal is shared, and a record another process fsynced is not this
// process's to remove.
func (s *Store) replay() (*journalScan, error) {
	return s.replayFrom(0)
}

func (s *Store) replayFrom(from int64) (*journalScan, error) {
	scan := &journalScan{states: map[string]*sessionState{}, end: from}
	f, err := os.Open(filepath.Join(s.root, "journal"))
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	br := bufio.NewReaderSize(f, 1<<20)
	off := from
	lineNo := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cas: journal: %w", err)
		}
		if len(line) == 0 {
			break
		}
		if line[len(line)-1] != '\n' {
			// The journal's end without a newline: a record still being
			// written, one a crash cut short, or a whole record whose
			// newline was damaged. Only whole records that pass their
			// checksum count, and nothing is reported: the rest may be a
			// write in flight.
			if recs, _ := decodeLine(line); len(recs) > 0 {
				for _, rec := range recs {
					if rec.checked {
						scan.apply(rec)
					}
				}
			}
			break
		}
		lineNo++
		start := off
		off += int64(len(line))
		scan.end = off
		recs, derr := decodeLine(line)
		if derr != nil {
			scan.damage = append(scan.damage, JournalDamage{Line: lineNo, Offset: start, Err: derr})
		}
		for _, rec := range recs {
			scan.apply(rec)
		}
	}
	if from > 0 {
		// Line numbers from an offset are relative; say so in the offset.
		for i := range scan.damage {
			scan.damage[i].Line = 0
		}
	}
	return scan, nil
}

// decodeLine reads the records of one journal line. A line normally
// holds one. A crash cuts a record short only at the journal's end, and
// the next record appended lands on the same line after the torn bytes,
// so bytes ahead of the first whole record are skipped as torn. Every
// other piece that does not read is damage: a record whose newline was
// damaged into another byte is read with the record it ran into, and a
// whole line holding no record that reads is reported.
func decodeLine(line []byte) ([]journalRecord, error) {
	var starts []int
	for i := 0; ; {
		k := bytes.Index(line[i:], []byte(`{"op":"`))
		if k < 0 {
			break
		}
		starts = append(starts, i+k)
		i += k + 1
	}
	if len(starts) == 0 {
		return nil, errors.New("no record")
	}
	var recs []journalRecord
	var damage error
	for n, st := range starts {
		end := len(line)
		if n+1 < len(starts) {
			end = starts[n+1]
		}
		seg := line[st:end]
		rec, _, err := decodeRecord(seg)
		if err != nil && len(seg) > 1 {
			// A record followed by one damaged byte where its newline was:
			// read, and reported.
			if r2, _, err2 := decodeRecord(seg[:len(seg)-1]); err2 == nil {
				recs = append(recs, r2)
				if damage == nil {
					damage = errors.New("a record's newline is damaged")
				}
				continue
			}
		}
		if err != nil {
			if n+1 < len(starts) && len(recs) == 0 && !bytes.Contains(seg, []byte(crcMember)) {
				continue // torn bytes a later record landed after
			}
			if damage == nil {
				damage = err
			}
			continue
		}
		recs = append(recs, rec)
	}
	if starts[0] > 0 && len(recs) == 0 && damage == nil {
		damage = errors.New("no record")
	}
	return recs, damage
}

// apply takes one record into the scan.
func (scan *journalScan) apply(rec journalRecord) {
	st := scan.states[rec.Session]
	if st == nil {
		st = &sessionState{}
		scan.states[rec.Session] = st
	}
	switch rec.Op {
	case "delete":
		scan.states[rec.Session] = &sessionState{deleted: true}
	case "create":
		// The boundary a session starts from: whatever the journal
		// said about this ID before belongs to a session that is gone.
		scan.states[rec.Session] = &sessionState{created: true, base: rec.Base}
	default:
		st.recs = append(st.recs, rec)
	}
}

// commit appends one or more journal records in one write. With durable
// set it fsyncs them, first flushing every object and journal record
// written lazily before, so the records never reach the disk ahead of
// what they name: that fsync is the commit point. Without it the records
// are written and the fsync is left to the next durable commit or Sync.
func (s *Store) commit(durable bool, recs ...journalRecord) error {
	if s.readOnly {
		return errReadOnly()
	}
	var buf bytes.Buffer
	for _, r := range recs {
		if !durable && r.Op == "append" {
			r.Lazy = true
		}
		line, err := r.encode()
		if err != nil {
			return err
		}
		buf.Write(line)
	}
	if durable {
		if err := s.objs.flush(); err != nil {
			return fmt.Errorf("cas: flush: %w", err)
		}
	}
	path := filepath.Join(s.root, "journal")
	_, existed := os.Stat(path)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return fmt.Errorf("cas: journal: %w", err)
	}
	if durable {
		if err := f.Sync(); err != nil {
			f.Close()
			return fmt.Errorf("cas: journal: %w", err)
		}
		s.journalDirty = false
	} else {
		s.journalDirty = true
	}
	if err := f.Close(); err != nil {
		return err
	}
	if existed != nil {
		return s.objs.syncOrDefer(s.root, true, durable)
	}
	return nil
}

// syncJournal fsyncs what lazy commits left: the objects, then the
// journal.
func (s *Store) syncJournal() error {
	if err := s.objs.flush(); err != nil {
		return err
	}
	if !s.journalDirty {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(s.root, "journal"), os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		return fmt.Errorf("cas: journal: %w", err)
	}
	s.journalDirty = false
	return nil
}
