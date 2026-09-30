package cas

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// A store written before logs were per session committed every append
// through one store-wide journal, and kept each session's log as a list
// of entry hashes that the journal could run ahead of. The first
// writing open of such a store migrates it: under the gc lock, the
// journal is read once, each session is recovered as that store would
// have recovered it, and its log is rewritten as the records its
// recovered state comes to, committed; then the journal goes. A crash
// part way leaves the journal, and the next open finishes, passing over
// the sessions already rewritten. A store is migrated with no writer of
// the earlier library running on it.

// ErrLegacyStore is returned for a store, or a session, in the layout of
// a store from before logs were per session, where the operation cannot
// migrate it: a read-only store, which writes nothing.
var ErrLegacyStore = errors.New("cas: the store predates per-session logs; open it for writing once to migrate it")

// Legacy journal ops, which a migration reads and nothing writes.
const (
	opDelete    = "delete"
	opJournal   = "journal"
	opSettled   = "settled"
	journalFile = "journal"
)

// readLegacyLog reads a session's log as a store kept it before logs
// were per session: each line an entry hash, followed by the entry's
// size in bytes when the line was written with one.
func readLegacyLog(dir string) ([]string, map[string]int64, error) {
	sizes := map[string]int64{}
	data, err := os.ReadFile(filepath.Join(dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, sizes, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cas: log: %w", err)
	}
	var out []string
	for _, l := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		hash, size, _ := strings.Cut(l, " ")
		if hash == "" {
			continue
		}
		out = append(out, hash)
		if n, err := strconv.ParseInt(size, 10, 64); err == nil {
			sizes[hash] = n
		}
	}
	return out, sizes, nil
}

// sessionState is what the journal says about one session since its
// last create or delete.
type sessionState struct {
	recs    []logRecord
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
	damage []LogDamage
	end    int64 // where the read stopped: the next record's offset
	lines  int   // the whole lines read, for damage line numbers
}

// read takes into the scan every whole line from its end on, and
// returns the checked records of a last line without its newline
// unapplied: that line may be a write in flight, which a later read
// reads again once it is whole.
func (scan *journalScan) read(f *os.File) ([]logRecord, error) {
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
			var tail []logRecord
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
			scan.damage = append(scan.damage, LogDamage{Line: scan.lines, Offset: start, Err: derr})
		}
		for _, rec := range recs {
			scan.apply(rec)
		}
	}
}

// apply takes one record into the scan.
func (scan *journalScan) apply(rec logRecord) {
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

// readJournal reads a legacy journal whole.
func readJournal(root string) (*journalScan, error) {
	scan := &journalScan{states: map[string]*sessionState{}}
	f, err := os.Open(filepath.Join(root, journalFile))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tail, err := scan.read(f)
	if err != nil {
		return nil, err
	}
	for _, rec := range tail {
		scan.apply(rec)
	}
	return scan, nil
}

// legacyView is one session as a legacy journal and its files say.
type legacyView struct {
	deleted, exists, damaged, fromFiles bool
	log                                 []string
	sizes                               map[string]int64
	head, mark                          string
	adopt, dropped                      []string

	logChanged, headChanged, markChanged bool
}

func (s *Store) legacyReconcile(id, dir string, scan *journalScan) (legacyView, error) {
	var v legacyView
	st := scan.states[id]
	if st != nil && st.deleted {
		v.deleted = true
		return v, nil
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
		return v, nil // never finished creating
	}
	v.exists = true
	have, sizes, err := readLegacyLog(dir)
	if err != nil {
		return v, err
	}
	fileHead, err := readHead(dir)
	if err != nil {
		return v, err
	}
	fileMark := readMark(dir)
	v.sizes = sizes
	if st == nil {
		v.log, v.head, v.mark, v.fromFiles = have, fileHead, fileMark, true
		return v, nil
	}
	// A session a compaction dropped, and a writer took up again, begins
	// its records here with a settled record: that many entries of its
	// log were there, fsynced, before the journal held anything of it,
	// and stand as they are.
	settled := 0
	if !st.created {
		for _, r := range st.recs {
			if r.Op == opSettled {
				settled = r.Seq
			}
		}
	}
	// The journal's own view, cut at the first lazy append that was lost.
	var jEntries []string
	seen := map[string]bool{}
	lost := map[string]bool{}
	jHead, hasJHead := "", false
	cut := false
	// A sync record says every lazy record before it is durable, apart
	// from the ones a lost record already said were lost.
	synced := -1
	// lostAt is where the last lost record naming each entry is: an
	// append of it before that is gone, one after it is kept.
	lostAt := map[string]int{}
	for i, r := range st.recs {
		switch r.Op {
		case "sync":
			synced = i
		case "lost":
			lostAt[r.Entry] = i
		}
	}
	gone := func(entry string, i int) bool {
		at, ok := lostAt[entry]
		return ok && at > i
	}
	var dropped []string
	for i, r := range st.recs {
		if cut {
			if r.Op == "append" && r.Entry != "" {
				lost[r.Entry] = true
				if !gone(r.Entry, i) {
					dropped = append(dropped, r.Entry)
				}
			}
			continue
		}
		switch r.Op {
		case "append":
			if r.Entry != "" && gone(r.Entry, i) {
				lost[r.Entry] = true // lost, and recorded so, earlier
				continue
			}
			if r.Lazy && r.Entry != "" && i > synced {
				ok, err := s.present(r.Entry)
				if errors.Is(err, ErrCorrupt) {
					ok, err = false, nil // a lazy object a crash left torn
				}
				if err != nil {
					return v, fmt.Errorf("cas: session %s: %w", id, err)
				}
				if !ok {
					cut = true
					lost[r.Entry] = true
					dropped = append(dropped, r.Entry)
					continue
				}
				v.adopt = append(v.adopt, r.Entry)
			}
			if r.Entry != "" && !seen[r.Entry] {
				seen[r.Entry] = true
				jEntries = append(jEntries, r.Entry)
				if r.Size > 0 {
					v.sizes[r.Entry] = r.Size
				}
			}
			if r.Head != "" {
				jHead, hasJHead = r.Head, true
			}
		case "head":
			if gone(r.Head, i) {
				continue
			}
			jHead, hasJHead = r.Head, true
		case opSettled:
			// The head as the session's files had it when taken up.
			jHead, hasJHead = r.Head, true
		}
	}
	for _, e := range jEntries {
		delete(lost, e) // appended again after the loss, and kept
	}
	for _, e := range dropped {
		if lost[e] {
			v.dropped = append(v.dropped, e)
		}
	}
	inLog := map[string]bool{}
	for i, e := range have {
		if lost[e] {
			v.logChanged = true
			continue
		}
		if i < settled && !seen[e] {
			inLog[e] = true
			v.log = append(v.log, e)
			continue
		}
		if !seen[e] {
			ok, err := s.present(e)
			if errors.Is(err, ErrCorrupt) && s.emptyLoose(e) {
				// Written with a lazy record the crash took, and left empty
				// by it, as a file renamed before it was synced can be. A
				// corrupt object with bytes in it is damage, and fails.
				ok, err = false, nil
			}
			if err != nil {
				return v, fmt.Errorf("cas: session %s: %w", id, err)
			}
			if !ok {
				// A log line that reached the disk ahead of a lazy record
				// and objects the crash took: an append that was lost.
				lost[e] = true
				v.logChanged = true
				continue
			}
			v.damaged = true
		}
		inLog[e] = true
		v.log = append(v.log, e)
	}
	for _, e := range jEntries {
		if !inLog[e] {
			v.log = append(v.log, e)
			v.logChanged = true
		}
	}
	base := st.base
	if base == "" {
		if hdr, err := readHeader(dir); err == nil {
			base = hdr.Base
		}
	}
	v.head = fileHead
	if hasJHead && !(v.damaged && headIn(fileHead, v.log, base)) {
		v.head = jHead
	}
	if lost[v.head] {
		v.head = jHead
	}
	if v.head == "" && !hasJHead {
		v.head = base // a fork's first head, whose HEAD a crash took
	}
	v.headChanged = v.head != fileHead
	v.mark = fileMark
	if m := st.mark(); m != "" {
		v.mark = m
	}
	v.markChanged = v.mark != fileMark
	return v, nil
}

func headIn(head string, log []string, base string) bool {
	if head == "" {
		return false
	}
	if head == base {
		return true
	}
	for _, e := range log {
		if e == head {
			return true
		}
	}
	return false
}

// migrate rewrites a legacy store's sessions as per-session logs and
// retires its journal. It runs at the first writing open, under the gc
// lock.
func (s *Store) migrate() error {
	if _, err := os.Stat(filepath.Join(s.root, journalFile)); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	var gc *dirLock
	for wait := 0; ; wait++ {
		lk, err := s.gcLock()
		if err == nil {
			gc = lk
			break
		}
		if !errors.Is(err, ErrSweepRunning) || wait > 600 {
			return fmt.Errorf("cas: migrate: %w", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer gc.release()
	if _, err := os.Stat(filepath.Join(s.root, journalFile)); errors.Is(err, os.ErrNotExist) {
		return nil // another process migrated it first
	}
	scan, err := readJournal(s.root)
	if err != nil {
		return fmt.Errorf("cas: migrate: %w", err)
	}
	ids := map[string]bool{}
	for id := range scan.states {
		if validSessionID(id) {
			ids[id] = true
		}
	}
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return fmt.Errorf("cas: migrate: %w", err)
	}
	for _, d := range dirs {
		if d.IsDir() && validSessionID(d.Name()) {
			ids[d.Name()] = true
		}
	}
	for id := range ids {
		if err := s.migrateSession(id, scan); err != nil {
			return fmt.Errorf("cas: migrate session %s: %w", id, err)
		}
	}
	if err := syncDir(filepath.Join(s.root, "sessions")); err != nil {
		return err
	}
	journal := filepath.Join(s.root, journalFile)
	if len(scan.damage) > 0 {
		// The damage is what Verify reports; the journal is kept for it.
		aside := filepath.Join(s.root, fmt.Sprintf("%s%d", damagedPrefix, time.Now().UnixNano()))
		if err := os.Rename(journal, aside); err != nil {
			return err
		}
	} else if err := os.Remove(journal); err != nil {
		return err
	}
	for _, name := range []string{"checkpoint", "journal.lock", "journal.gate"} {
		os.Remove(filepath.Join(s.root, name))
	}
	for _, pat := range []string{"checkpoint.tmp-*", "journal.tmp-*"} {
		left, _ := filepath.Glob(filepath.Join(s.root, pat))
		for _, p := range left {
			os.Remove(p)
		}
	}
	return syncDir(s.root)
}

// damagedPrefix names a legacy journal with damaged lines, kept after a
// migration for Verify to report and a person to read.
const damagedPrefix = "journal.damaged-"

// migrateSession rewrites one session's log from its legacy state. A
// session whose log is already per session was migrated before a crash
// and is passed over.
func (s *Store) migrateSession(id string, scan *journalScan) error {
	dir, err := s.sessionDir(id)
	if err != nil {
		return err
	}
	if l, err := readSessionLog(dir, 0); err == nil && !l.legacy && l.size > 0 {
		return nil
	}
	v, err := s.legacyReconcile(id, dir, scan)
	if err != nil {
		return err
	}
	if v.deleted || !v.exists {
		// Deleted, or never finished creating: nothing of it is read.
		return os.RemoveAll(dir)
	}
	hdr, err := readHeader(dir)
	if err != nil {
		return err
	}
	// Lazy appends that survived are made durable, as recovery would
	// adopt them, since the rewritten log commits them.
	for _, e := range v.adopt {
		if err := s.objs.freshen(spaceEntries, e); err != nil {
			return err
		}
		c, err := s.contentOf(e)
		if err != nil {
			return err
		}
		if err := s.objs.freshen(spaceContents, c); err != nil {
			return err
		}
	}
	if err := s.objs.flush(); err != nil {
		return err
	}
	recs := []logRecord{{Op: opCreate, Session: id, Base: hdr.Base}, {Op: opMark, Session: id, Mark: v.mark}}
	for i, e := range v.log {
		recs = append(recs, logRecord{Op: opAppend, Session: id, Entry: e, Seq: i + 1, Size: v.sizes[e]})
	}
	recs = append(recs, logRecord{Op: opHead, Session: id, Head: v.head}, logRecord{Op: opSync, Session: id})
	data, err := encodeRecords(true, recs)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, logName), data, true); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "HEAD"), []byte(v.head+"\n"), true); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, "record"), []byte(v.mark+"\n"), true); err != nil {
		return err
	}
	return syncDir(dir)
}
