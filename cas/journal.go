package cas

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
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
	Op      string `json:"op"` // create, append, head, mark, delete, sync, lost
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
	if r, ok := parseRecord(seg); ok {
		rec = r
	} else if err := json.Unmarshal(seg, &rec); err != nil {
		return rec, false, err
	}
	rec.checked = bytes.Contains(seg, []byte(crcMember))
	if rec.Op == "" || rec.Session == "" {
		return rec, false, errors.New("record names no operation or session")
	}
	return rec, false, nil
}

// parseRecord reads a record in the shape encode writes it: one flat
// object of known lower-case members, strings without escapes, whole
// numbers and true. It reports false for anything else, which the
// caller hands to encoding/json, so a record read here reads as that
// would read it; replay is mostly this, and the decoder's reflection
// was most of what opening a store cost.
func parseRecord(seg []byte) (journalRecord, bool) {
	var r journalRecord
	if len(seg) < 2 || seg[0] != '{' || seg[len(seg)-1] != '}' {
		return r, false
	}
	i := 1
	str := func() (string, bool) {
		if i >= len(seg) || seg[i] != '"' {
			return "", false
		}
		start := i + 1
		for j := start; j < len(seg); j++ {
			switch c := seg[j]; {
			case c == '"':
				i = j + 1
				return string(seg[start:j]), true
			case c == '\\' || c < 0x20 || c >= 0x80:
				return "", false
			}
		}
		return "", false
	}
	num := func() (int64, bool) {
		start, neg := i, false
		if i < len(seg) && seg[i] == '-' {
			neg = true
			i++
		}
		digits := i
		var n int64
		for i < len(seg) && seg[i] >= '0' && seg[i] <= '9' {
			if i-digits >= 18 {
				return 0, false
			}
			n = n*10 + int64(seg[i]-'0')
			i++
		}
		if i == digits || (seg[digits] == '0' && i-digits > 1) || i-start == 0 {
			return 0, false
		}
		if neg {
			n = -n
		}
		return n, true
	}
	if seg[i] == '}' {
		return r, i == len(seg)-1
	}
	for {
		key, ok := str()
		if !ok || i >= len(seg) || seg[i] != ':' {
			return r, false
		}
		i++
		switch key {
		case "op", "session", "entry", "head", "base", "mark", "crc":
			v, ok := str()
			if !ok {
				return r, false
			}
			switch key {
			case "op":
				r.Op = v
			case "session":
				r.Session = v
			case "entry":
				r.Entry = v
			case "head":
				r.Head = v
			case "base":
				r.Base = v
			case "mark":
				r.Mark = v
			}
		case "seq", "size":
			n, ok := num()
			if !ok {
				return r, false
			}
			if key == "seq" {
				r.Seq = int(n)
			} else {
				r.Size = n
			}
		case "lazy":
			if !bytes.HasPrefix(seg[i:], []byte("true")) {
				return r, false
			}
			r.Lazy = true
			i += len("true")
		default:
			return r, false
		}
		if i >= len(seg) {
			return r, false
		}
		switch seg[i] {
		case ',':
			i++
		case '}':
			return r, i == len(seg)-1
		default:
			return r, false
		}
	}
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
	lines  int   // the whole lines read, for damage line numbers
}

// replay reads the journal from the start and returns each session's
// state as the records say, in order, with a create or delete record
// clearing what came before it. A record a crashed process cut short
// is skipped, and if another process's record landed on the same line
// after the torn bytes, that record is found and counted. A damaged
// record is reported and skipped. Nothing is ever truncated: the
// journal is shared, and a record another process fsynced is not this
// process's to remove.
//
// The journal only grows, so the store keeps what it has read and each
// replay reads only the records written since, by this process or any
// other. The first starts from the store's checkpoint when it matches
// the journal, and a writing store saves another once the journal has
// grown checkpointEvery past the last. What replay returns is the
// caller's own, unchanged by later replays.
//
// The journal file is opened once per replay, and everything the replay
// reads, checkpoint checks included, comes through that one handle: a
// compaction may rename a new journal into place at any moment, and a
// replay must not read part of one file and part of the other. A
// journal that is not the file the scan was read from, or is shorter
// than where it stopped, is read afresh.
func (s *Store) replay() (*journalScan, error) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	f, err := os.Open(filepath.Join(s.root, "journal"))
	if errors.Is(err, os.ErrNotExist) {
		s.scan, s.scanFile, s.checkpointed = &journalScan{states: map[string]*sessionState{}}, nil, 0
		return s.scan.snapshot(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	// A filesystem may give a compacted journal the inode of the one it
	// replaced, so the file's identity is checked by its first line too,
	// which a compaction makes unique.
	gen, err := journalGen(f, info.Size())
	if err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	if s.scan == nil || s.scanFile == nil || !os.SameFile(s.scanFile, info) || gen != s.scanGen || info.Size() < s.scan.end {
		s.scan, s.checkpointBytes = s.loadCheckpoint(f, info.Size())
		if s.scan == nil {
			s.scan = &journalScan{states: map[string]*sessionState{}}
		}
		s.checkpointed, s.scanFile, s.scanGen = s.scan.end, info, gen
	}
	tail, err := s.scan.read(f)
	if err != nil {
		return nil, err
	}
	s.saveCheckpoint(f)
	snap := s.scan.snapshot()
	for _, rec := range tail {
		snap.apply(rec)
	}
	return snap, nil
}

// journalGen hashes the journal's first line, up to checkpointTail
// bytes of it: the same until a compaction replaces the journal, and
// different after, since a compacted journal opens with a record naming
// a new generation at random. A journal whose first line is not yet
// whole has none.
func journalGen(f *os.File, size int64) ([32]byte, error) {
	head := make([]byte, min(size, checkpointTail))
	if _, err := f.ReadAt(head, 0); err != nil {
		return [32]byte{}, err
	}
	k := bytes.IndexByte(head, '\n')
	if k < 0 {
		return [32]byte{}, nil
	}
	return sha256.Sum256(head[:k+1]), nil
}

// saveCheckpoint writes the scan, read from the journal f, as the
// store's checkpoint once the journal has grown past the last one by
// checkpointEvery or by the last one's size, whichever is more, so a
// checkpoint is rewritten at most once per its own size of journal. It
// runs under scanMu.
func (s *Store) saveCheckpoint(f *os.File) {
	if s.readOnly || s.scan == nil || s.scan.end-s.checkpointed < max(checkpointEvery, s.checkpointBytes) {
		return
	}
	if n, err := s.writeCheckpoint(s.scan, f); err == nil {
		s.checkpointed, s.checkpointBytes = s.scan.end, n
	}
}

// replayFrom reads the journal from an offset on its own, for a caller
// that wants only the records written after it. The caller holds the gc
// lock, which a compaction takes too, so the offset is into this file.
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
	tail, err := scan.read(f)
	if err != nil {
		return nil, err
	}
	for _, rec := range tail {
		scan.apply(rec)
	}
	if from > 0 {
		// Line numbers from an offset are relative; say so in the offset.
		for i := range scan.damage {
			scan.damage[i].Line = 0
		}
	}
	return scan, nil
}

// read takes into the scan every whole line from its end on, and
// returns the checked records of a last line without its newline
// unapplied: that line may be a write in flight, which a later read
// reads again once it is whole.
func (scan *journalScan) read(f *os.File) ([]journalRecord, error) {
	if _, err := f.Seek(scan.end, io.SeekStart); err != nil {
		return nil, fmt.Errorf("cas: journal: %w", err)
	}
	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("cas: journal: %w", err)
		}
		if len(line) == 0 {
			return nil, nil
		}
		if line[len(line)-1] != '\n' {
			// The journal's end without a newline: a record still being
			// written, one a crash cut short, or a whole record whose
			// newline was damaged. Only whole records that pass their
			// checksum count, and nothing is reported: the rest may be a
			// write in flight.
			var tail []journalRecord
			recs, _ := decodeLine(line)
			for _, rec := range recs {
				if rec.checked {
					tail = append(tail, rec)
				}
			}
			return tail, nil
		}
		scan.lines++
		start := scan.end
		scan.end += int64(len(line))
		recs, derr := decodeLine(line)
		if derr != nil {
			scan.damage = append(scan.damage, JournalDamage{Line: scan.lines, Offset: start, Err: derr})
		}
		for _, rec := range recs {
			scan.apply(rec)
		}
	}
}

// snapshot returns a copy of the scan that later reads into the scan do
// not change: each state is copied, and each slice is capped at its
// length, so an append on either side reallocates rather than writing
// where the other reads.
func (scan *journalScan) snapshot() *journalScan {
	snap := &journalScan{
		states: make(map[string]*sessionState, len(scan.states)),
		damage: scan.damage[:len(scan.damage):len(scan.damage)],
		end:    scan.end,
		lines:  scan.lines,
	}
	for id, st := range scan.states {
		c := *st
		c.recs = st.recs[:len(st.recs):len(st.recs)]
		snap.states[id] = &c
	}
	return snap
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
		rec, torn, err := decodeRecord(seg)
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
			if n+1 < len(starts) && len(recs) == 0 && torn {
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
	if rec.Op == opJournal {
		return // names the journal, not a session
	}
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
	// A compaction renames a new journal into place while it holds this
	// lock exclusive; held shared from the open through the close, it
	// keeps a record from landing in a journal that is being replaced.
	jl, err := s.journalLock()
	if err != nil {
		return err
	}
	defer jl.release()
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
	if end, err := f.Seek(0, io.SeekCurrent); err == nil {
		s.journalSize = end
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
// journal, with a sync record for each held session that has lazy
// appends, so a later open need not check their objects.
func (s *Store) syncJournal() error {
	if err := s.objs.flush(); err != nil {
		return err
	}
	var syncs []journalRecord
	for id, h := range s.open {
		if h.lazy {
			syncs = append(syncs, journalRecord{Op: "sync", Session: id})
		}
	}
	if len(syncs) > 0 {
		sort.Slice(syncs, func(i, j int) bool { return syncs[i].Session < syncs[j].Session })
		if err := s.commit(true, syncs...); err != nil {
			return err
		}
		for _, r := range syncs {
			s.open[r.Session].lazy = false
		}
		return nil
	}
	if !s.journalDirty {
		return nil
	}
	jl, err := s.journalLock()
	if err != nil {
		return err
	}
	defer jl.release()
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
