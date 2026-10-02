package cas

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
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
//
// The journal is retired by putting a tombstone in its place: a
// symbolic link, journal -> layout/journal, through the layout file,
// which is a regular file, so no write to the journal can be made
// through it (on unix, opening, creating or stat'ing it fails with
// ENOTDIR; elsewhere an open may succeed, but every write fails). A
// writer of the earlier library appends every commit to the journal,
// creating it when it is missing; through the tombstone each commit
// fails, so a writer the migration did not find, one holding no session
// while it ran, refuses every later write rather than write sessions in
// the old layout into a store that no longer reads them. Releases from
// v0.0.16 to v0.0.18 test for the journal with os.Stat, which fails the
// same way, and take it as gone, apart from a writing open of theirs
// that found the journal and waited on the gc lock while this release
// migrated: it reads through the tombstone, fails once, and the next
// open of that release succeeds. On a filesystem that makes no symbolic link, as Windows without
// the right to make one, the journal is only removed, and a writer of
// the earlier library still running there makes it again at its next
// commit.

// ErrLegacyStore is returned for a store, or a session, in the layout of
// a store from before logs were per session, where the operation cannot
// migrate it: a read-only store, which writes nothing. Its text says
// what the migration asks, since a product that reads such a store
// passes the text on: a writing open migrates the store, but one made
// beside a writer of the earlier release migrates it under that
// writer, so the open is the last step and not the first.
var ErrLegacyStore = errors.New("cas: the session predates per-session logs and has not been migrated; stop every writer, take a copy, and run agentsession migrate <root>")

// ErrMigrationBusy is returned by the writing open that would migrate a
// store while a process of an earlier version still holds a session.
var ErrMigrationBusy = errors.New("cas: a process holds a session of a store that needs migrating; stop every writer of the earlier version first")

// Legacy journal ops, which a migration reads and nothing writes.
const (
	opDelete    = "delete"
	opJournal   = "journal"
	opSettled   = "settled"
	journalFile = "journal"
)

// tombstone is where the journal's tombstone points: a path through the
// layout file, which no open can follow.
var tombstone = filepath.Join(layoutFile, journalFile)

// legacyJournal reports whether root holds a journal still to migrate: a
// regular file. The tombstone a migration leaves is a symbolic link, and
// is not one.
func legacyJournal(root string) bool {
	info, err := os.Lstat(filepath.Join(root, journalFile))
	return err == nil && info.Mode().IsRegular()
}

// NeedsMigration reports whether the store at root holds a journal of a
// store from before logs were per session, which its next writing open
// migrates: a store no writing open has migrated yet, or one whose
// migration left a session that failed, which [Store.Verify] names.
func NeedsMigration(root string) bool {
	return legacyJournal(root)
}

// retireJournal puts the tombstone in the journal's place in one rename,
// so a crash leaves the journal or the tombstone and never neither. It
// runs with the sweep's lock held exclusive, which a writer of the
// earlier library takes shared through each commit, so no commit lands
// in the journal it replaces. Where no symbolic link can be made the
// journal is removed instead.
func (s *Store) retireJournal() error {
	journal := filepath.Join(s.root, journalFile)
	tmp := journal + ".tmp-tombstone"
	os.Remove(tmp)
	if err := os.Symlink(tombstone, tmp); err != nil {
		if err := os.Remove(journal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.Rename(tmp, journal); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// markTombstone puts the tombstone in place in a store with its layout
// file and no journal: one a release before the tombstone migrated, or
// whose migration a crash stopped between setting a damaged journal
// aside and retiring it, or a new store, which a writer of the earlier
// library would otherwise give a journal. It never replaces what is
// there, so a journal a writer of the earlier library made a moment ago
// is left for the next writing open to migrate.
func (s *Store) markTombstone() error {
	journal := filepath.Join(s.root, journalFile)
	if _, err := os.Lstat(journal); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.root, layoutFile)); err != nil {
		return nil
	}
	if err := os.Symlink(tombstone, journal); err != nil {
		return nil // no symbolic links here, or something made the journal first
	}
	return s.objs.fsyncDir(s.root)
}

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
		if !agentsession.ValidHash(hash) {
			continue // a line a crash cut short; the journal has it
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

// migrating, when set, is called once a migration has read the journal
// and locked the sessions it migrates, so a test can act part way.
var migrating func()

// retiring, when set, is called just before the tombstone replaces the
// journal, and an error it returns stops the migration there, as a
// crash would.
var retiring func() error

// migrate rewrites a legacy store's sessions as per-session logs and
// retires its journal. It runs at the first writing open, under the gc
// lock, and holds the sweep's lock exclusive from before it reads the
// journal until the tombstone is in place: a writer of the earlier
// library holds that lock shared from its object writes through its
// commit, so none commits to the journal between the read and the
// retirement, where the commit would be acknowledged and then removed
// with the journal. On a platform without flock the lock is nothing,
// and that window is open.
func (s *Store) migrate() error {
	if !legacyJournal(s.root) {
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sweep, err := s.sweepLock(ctx)
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: a writer held sweep.lock for a minute", ErrMigrationBusy)
	}
	if err != nil {
		return fmt.Errorf("cas: migrate: %w", err)
	}
	defer sweep.release()
	if !legacyJournal(s.root) {
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
	// Only a session still in the earlier format is migrated. One
	// already rewritten, by this migration before a crash or at an open
	// that kept the journal, or created since, is this version's to
	// recover, damage and all; migrating it again would write its log
	// over from the journal's older view.
	failed := false
	var todo []string
	for id := range ids {
		dir, err := s.sessionDir(id)
		if err == nil {
			var old bool
			if old, err = unmigrated(dir); err == nil && !old {
				continue
			}
		}
		if err != nil {
			s.faulty[id] = fmt.Errorf("cas: migrate session %s: %w", id, err)
			failed = true
			continue
		}
		todo = append(todo, id)
	}
	// A writer of the earlier version holds its session's lock, the
	// same lock this version takes; migration holds the lock of every
	// session it migrates until it is done, and refuses while any is
	// held.
	var locks []*dirLock
	defer func() {
		for _, lk := range locks {
			lk.release()
		}
	}()
	for _, id := range todo {
		lk, err := s.lockSession(id)
		if errors.Is(err, ErrSessionLocked) {
			return fmt.Errorf("%w: session %s", ErrMigrationBusy, id)
		}
		if err != nil {
			return fmt.Errorf("cas: migrate: %w", err)
		}
		locks = append(locks, lk)
	}
	if migrating != nil {
		migrating()
	}
	// A session that fails to migrate is left as it was and reported
	// when it is opened; the others go on, and the journal is kept for
	// the next writing open to try that session again.
	for _, id := range todo {
		if err := s.migrateSession(id, scan); err != nil {
			s.faulty[id] = fmt.Errorf("cas: migrate session %s: %w", id, err)
			failed = true
		}
	}
	if err := s.objs.fsyncDir(filepath.Join(s.root, "sessions")); err != nil {
		return err
	}
	if err := s.objs.stopped(); err != nil {
		// A session's rewrite whose fsync failed may not survive a
		// crash; the journal stays for the next open to migrate from.
		return err
	}
	if failed {
		return nil
	}
	// The layout goes first, so the tombstone never points through a
	// path that is not there yet; a store with its layout and a journal
	// is migrated again by the next writing open.
	if err := s.writeLayout(); err != nil {
		return err
	}
	if len(scan.damage) > 0 {
		// The damage is what Verify reports; the journal is kept for it,
		// as a hard link, so the journal stays until the tombstone
		// replaces it and a crash between leaves the journal for the
		// next open to migrate again. Where the filesystem makes no hard
		// link it is renamed aside, and a crash before the tombstone
		// leaves neither, until the next writing open of this release
		// puts the tombstone in place.
		journal := filepath.Join(s.root, journalFile)
		aside := filepath.Join(s.root, fmt.Sprintf("%s%d", damagedPrefix, time.Now().UnixNano()))
		if err := linkFile(journal, aside); err != nil {
			if err := os.Rename(journal, aside); err != nil {
				return err
			}
		}
	}
	if retiring != nil {
		if err := retiring(); err != nil {
			return err
		}
	}
	if err := s.retireJournal(); err != nil {
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
	return s.objs.fsyncDir(s.root)
}

// unmigrated reports whether a session's directory is in the format of
// a store from before logs were per session: it has a directory, and
// its log is missing, empty, or holds no line with a record's opening.
// A legacy line is a hash and a size, torn or whole, and cannot hold
// one, so a log that does is this version's, however damaged; its
// damage is for recovery to report, not for a migration to write over.
// A session with no directory has nothing to migrate.
func unmigrated(dir string) (bool, error) {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	f, err := os.Open(filepath.Join(dir, logName))
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := br.ReadSlice('\n')
		if bytes.Contains(line, recordOpening) {
			return false, nil
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			// A line longer than the buffer is no legacy line.
			return false, nil
		case errors.Is(err, io.EOF):
			return true, nil
		case err != nil:
			return false, err
		}
	}
}

// discard renames a session's directory into the trash, where a sweep
// removes it.
func (s *Store) discard(id, dir string) error {
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	trash := filepath.Join(s.root, "trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return err
	}
	return os.Rename(dir, filepath.Join(trash, fmt.Sprintf("%s-%d", id, time.Now().UnixNano())))
}

// damagedPrefix names a legacy journal with damaged lines, kept after a
// migration for Verify to report and a person to read.
const damagedPrefix = "journal.damaged-"

// migrateSession rewrites one session's log from its legacy state. A
// session whose log is already per session is passed over; migrate
// checked before taking its lock, and this checks again under it.
func (s *Store) migrateSession(id string, scan *journalScan) error {
	dir, err := s.sessionDir(id)
	if err != nil {
		return err
	}
	if old, err := unmigrated(dir); err != nil || !old {
		return err
	}
	v, err := s.legacyReconcile(id, dir, scan)
	if err != nil {
		return err
	}
	if v.deleted {
		// A delete the journal cannot vouch for, because the journal
		// holds damage or records follow the delete, leaves the session
		// to be migrated from its own files rather than removed.
		if st := scan.states[id]; len(scan.damage) > 0 || (st != nil && len(st.recs) > 0) {
			if _, err := os.Stat(filepath.Join(dir, "header")); err == nil {
				if v, err = s.legacyReconcile(id, dir, &journalScan{states: map[string]*sessionState{}}); err != nil {
					return err
				}
			}
		}
	}
	if v.deleted || !v.exists {
		// Deleted, or never finished creating: nothing of it is read,
		// and it goes to the trash rather than straight away.
		return s.discard(id, dir)
	}
	hdr, err := readHeader(dir)
	if err != nil {
		return err
	}
	// Lazy appends that survived are made durable, as recovery would
	// adopt them, since the rewritten log commits them.
	pend := newPendSet()
	for _, e := range v.adopt {
		if err := s.freshenEntry(e, pend); err != nil {
			return err
		}
	}
	if err := s.objs.flushSet(pend); err != nil {
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
	for _, f := range []struct {
		name string
		data []byte
	}{{logName, data}, {"HEAD", []byte(v.head + "\n")}, {"record", []byte(v.mark + "\n")}} {
		if _, err := s.objs.writeFile(filepath.Join(dir, f.name), f.data, true); err != nil {
			return err
		}
	}
	return s.objs.fsyncDir(dir)
}
