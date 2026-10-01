package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// A checkpoint is the journal scan saved at an offset, so opening a
// store reads it and the records after it rather than the whole
// journal, as git reads packed-refs and a commit-graph rather than
// walking every object. It is derived data: the journal stays the
// commit point and the record, and a checkpoint that does not match it
// is ignored, never trusted.
//
// It matches when the journal is at least as long as the offset, its
// first line hashes to what the checkpoint says, and its last
// checkpointTail bytes before the offset do too. The journal only grows
// until a compaction replaces it, and a compacted journal begins with a
// record naming a new generation at random, so a journal whose first
// line matches is the one the checkpoint was read from, and bytes that
// match before the offset are the bytes it read; a journal compacted
// since, or restored from before the offset, fails. The file ends
// in the SHA-256 of what precedes it, so one a crash cut short or a
// flipped bit damaged fails too. It is written to a temporary name and
// renamed without an fsync: a crash may leave it empty or stale, which
// the checks turn into a full replay.
//
// Layout, integers as varints:
//
//	magic
//	end, lines, SHA-256 of the journal's first line, of its tail
//	damage count, then each: line, offset, message
//	session count, then each: id, flags, base, record count, records
//	SHA-256 of everything above
const (
	checkpointName  = "checkpoint"
	checkpointMagic = "agentsession-cas-checkpoint 2\n"
	checkpointTail  = 4096
)

// checkpointEvery is how far the journal grows past the last checkpoint
// before a writing store saves another: what an open replays beyond a
// checkpoint stays under it. A variable so tests can lower it.
var checkpointEvery int64 = 256 << 10

// String kinds in a checkpoint. A hash is held as its 32 bytes, and a
// string that repeats the one beside it — a head that is the record's
// own entry, as an append's usually is, or the session a record is
// filed under — as a back reference.
const (
	strEmpty byte = iota
	strRaw
	strHash
	strEntry
)

const (
	recLazy byte = 1 << iota
	recChecked
)

const (
	stateDeleted byte = 1 << iota
	stateCreated
)

type ckptWriter struct{ buf []byte }

func (w *ckptWriter) uvarint(n uint64) { w.buf = binary.AppendUvarint(w.buf, n) }
func (w *ckptWriter) varint(n int64)   { w.buf = binary.AppendVarint(w.buf, n) }
func (w *ckptWriter) raw(s string) {
	w.uvarint(uint64(len(s)))
	w.buf = append(w.buf, s...)
}

func (w *ckptWriter) str(s, entry string) {
	switch {
	case s == "":
		w.buf = append(w.buf, strEmpty)
	case s == entry:
		w.buf = append(w.buf, strEntry)
	default:
		if d, ok := hashDigest(s); ok {
			w.buf = append(w.buf, strHash)
			w.buf = append(w.buf, d...)
			return
		}
		w.buf = append(w.buf, strRaw)
		w.raw(s)
	}
}

// hashDigest returns the bytes of a hash spelled as the format spells
// one, lower-case hex after the prefix, so that it reads back the same.
func hashDigest(s string) ([]byte, bool) {
	hexPart, ok := strings.CutPrefix(s, agentsession.HashPrefix)
	if !ok || len(hexPart) != 2*sha256.Size {
		return nil, false
	}
	for i := 0; i < len(hexPart); i++ {
		if c := hexPart[i]; !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return nil, false
		}
	}
	d, err := hex.DecodeString(hexPart)
	return d, err == nil
}

// journalMarks are the hashes a checkpoint is checked against: of the
// journal's first line and of its tail before the checkpoint's offset.
type journalMarks struct{ head, tail [sha256.Size]byte }

func encodeCheckpoint(scan *journalScan, m journalMarks) []byte {
	w := &ckptWriter{buf: make([]byte, 0, 64+len(scan.states)*64)}
	w.buf = append(w.buf, checkpointMagic...)
	w.varint(scan.end)
	w.uvarint(uint64(scan.lines))
	w.buf = append(w.buf, m.head[:]...)
	w.buf = append(w.buf, m.tail[:]...)
	w.uvarint(uint64(len(scan.damage)))
	for _, d := range scan.damage {
		w.uvarint(uint64(d.Line))
		w.varint(d.Offset)
		w.raw(d.Err.Error())
	}
	ids := make([]string, 0, len(scan.states))
	for id := range scan.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	w.uvarint(uint64(len(ids)))
	for _, id := range ids {
		st := scan.states[id]
		w.raw(id)
		var flags byte
		if st.deleted {
			flags |= stateDeleted
		}
		if st.created {
			flags |= stateCreated
		}
		w.buf = append(w.buf, flags)
		w.str(st.base, "")
		w.uvarint(uint64(len(st.recs)))
		for _, r := range st.recs {
			w.str(r.Op, "")
			w.str(r.Session, id)
			w.str(r.Entry, "")
			w.str(r.Head, r.Entry)
			w.str(r.Base, "")
			w.str(r.Mark, "")
			w.varint(int64(r.Seq))
			w.varint(r.Size)
			var rf byte
			if r.Lazy {
				rf |= recLazy
			}
			if r.checked {
				rf |= recChecked
			}
			w.buf = append(w.buf, rf)
		}
	}
	sum := sha256.Sum256(w.buf)
	return append(w.buf, sum[:]...)
}

var errCheckpoint = errors.New("cas: checkpoint does not read")

type ckptReader struct {
	buf []byte
	err error
}

func (r *ckptReader) fail() { r.err = errCheckpoint }

func (r *ckptReader) uvarint() uint64 {
	if r.err != nil {
		return 0
	}
	n, k := binary.Uvarint(r.buf)
	if k <= 0 {
		r.fail()
		return 0
	}
	r.buf = r.buf[k:]
	return n
}

func (r *ckptReader) varint() int64 {
	if r.err != nil {
		return 0
	}
	n, k := binary.Varint(r.buf)
	if k <= 0 {
		r.fail()
		return 0
	}
	r.buf = r.buf[k:]
	return n
}

func (r *ckptReader) bytes(n uint64) []byte {
	if r.err != nil {
		return nil
	}
	if n > uint64(len(r.buf)) {
		r.fail()
		return nil
	}
	b := r.buf[:n]
	r.buf = r.buf[n:]
	return b
}

func (r *ckptReader) byte() byte {
	if b := r.bytes(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *ckptReader) raw() string { return string(r.bytes(r.uvarint())) }

// count reads a count of items each at least min bytes long, refusing
// one the rest of the file could not hold.
func (r *ckptReader) count(min int) int {
	n := r.uvarint()
	if r.err == nil && n > uint64(len(r.buf)/min) {
		r.fail()
		return 0
	}
	return int(n)
}

func (r *ckptReader) str(entry string) string {
	switch r.byte() {
	case strEmpty:
		return ""
	case strEntry:
		return entry
	case strHash:
		return agentsession.HashPrefix + hex.EncodeToString(r.bytes(sha256.Size))
	case strRaw:
		return r.raw()
	}
	r.fail()
	return ""
}

// decodeCheckpoint reads a checkpoint's scan and the marks it claims,
// or fails for a file that is not one whole checkpoint.
func decodeCheckpoint(data []byte) (*journalScan, journalMarks, error) {
	var m journalMarks
	if len(data) < len(checkpointMagic)+sha256.Size || !bytes.HasPrefix(data, []byte(checkpointMagic)) {
		return nil, m, errCheckpoint
	}
	body := data[:len(data)-sha256.Size]
	if sum := sha256.Sum256(body); !bytes.Equal(sum[:], data[len(body):]) {
		return nil, m, errCheckpoint
	}
	r := &ckptReader{buf: body[len(checkpointMagic):]}
	scan := &journalScan{states: map[string]*sessionState{}}
	scan.end = r.varint()
	scan.lines = int(r.uvarint())
	copy(m.head[:], r.bytes(sha256.Size))
	copy(m.tail[:], r.bytes(sha256.Size))
	if n := r.count(3); n > 0 {
		scan.damage = make([]JournalDamage, n)
		for i := range scan.damage {
			scan.damage[i] = JournalDamage{Line: int(r.uvarint()), Offset: r.varint(), Err: errors.New(r.raw())}
		}
	}
	for n := r.count(4); n > 0 && r.err == nil; n-- {
		id := r.raw()
		flags := r.byte()
		st := &sessionState{deleted: flags&stateDeleted != 0, created: flags&stateCreated != 0}
		st.base = r.str("")
		if m := r.count(9); m > 0 {
			st.recs = make([]journalRecord, m)
			for i := range st.recs {
				rec := &st.recs[i]
				rec.Op = r.str("")
				rec.Session = r.str(id)
				rec.Entry = r.str("")
				rec.Head = r.str(rec.Entry)
				rec.Base = r.str("")
				rec.Mark = r.str("")
				rec.Seq = int(r.varint())
				rec.Size = r.varint()
				rf := r.byte()
				rec.Lazy, rec.checked = rf&recLazy != 0, rf&recChecked != 0
			}
		}
		scan.states[id] = st
	}
	if r.err != nil || len(r.buf) != 0 || scan.end < 0 {
		return nil, m, errCheckpoint
	}
	return scan, m, nil
}

// marksOf hashes the journal f's first line, up to checkpointTail
// bytes of it, and its last checkpointTail bytes before end.
func marksOf(f *os.File, end int64) (journalMarks, error) {
	var m journalMarks
	head := make([]byte, min(end, checkpointTail))
	if _, err := f.ReadAt(head, 0); err != nil {
		return m, err
	}
	if k := bytes.IndexByte(head, '\n'); k >= 0 {
		head = head[:k+1]
	}
	m.head = sha256.Sum256(head)
	start := max(end-checkpointTail, 0)
	tail := make([]byte, end-start)
	if _, err := f.ReadAt(tail, start); err != nil {
		return m, err
	}
	m.tail = sha256.Sum256(tail)
	return m, nil
}

// loadCheckpoint returns the scan the store's checkpoint holds, with
// the checkpoint's size, when it matches the journal f of the given
// size, and nil otherwise.
func (s *Store) loadCheckpoint(f *os.File, size int64) (*journalScan, int64) {
	data, err := os.ReadFile(filepath.Join(s.root, checkpointName))
	if err != nil {
		return nil, 0
	}
	scan, want, err := decodeCheckpoint(data)
	if err != nil || scan.end == 0 || size < scan.end {
		return nil, 0
	}
	if got, err := marksOf(f, scan.end); err != nil || got != want {
		return nil, 0
	}
	return scan, int64(len(data))
}

// writeCheckpoint saves the scan, read from the journal f, as the
// store's checkpoint and returns its size. A failure costs the next
// open a longer replay and nothing else, so the caller may ignore it.
func (s *Store) writeCheckpoint(scan *journalScan, f *os.File) (int64, error) {
	if s.readOnly || scan.end == 0 {
		return 0, nil
	}
	m, err := marksOf(f, scan.end)
	if err != nil {
		return 0, err
	}
	data := encodeCheckpoint(scan, m)
	tmp, err := os.CreateTemp(s.root, checkpointName+".tmp-*")
	if err != nil {
		return 0, err
	}
	_, werr := tmp.Write(data)
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), filepath.Join(s.root, checkpointName))
	}
	if werr != nil {
		os.Remove(tmp.Name())
	}
	s.cleanCheckpointTemps()
	return int64(len(data)), werr
}

// cleanCheckpointTemps removes temporary checkpoints and journals a
// crash left, once they are old enough that no writer can still be
// renaming them.
func (s *Store) cleanCheckpointTemps() {
	old, _ := filepath.Glob(filepath.Join(s.root, checkpointName+".tmp-*"))
	journals, _ := filepath.Glob(filepath.Join(s.root, "journal.tmp-*"))
	for _, p := range append(old, journals...) {
		if info, err := os.Stat(p); err == nil && time.Since(info.ModTime()) > time.Hour {
			os.Remove(p)
		}
	}
}
