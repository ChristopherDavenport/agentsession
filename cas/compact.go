package cas

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Compaction replaces the journal with one that holds only what the
// sessions' own files cannot yet stand for, as git replaces loose refs
// and old packs once what they said is written down elsewhere.
//
// A session's records can go once its files say what they say: its log
// holds the entries the journal appended, in order, its HEAD and record
// mark match, and no lazy append in it waits on a sync. Recovery of a
// session the journal says nothing about reads its files as they stand,
// so once those files are fsynced the records are redundant. A session
// that does not yet agree, one a live writer has lazy appends in or has
// committed to and not yet indexed, keeps its records; a delete keeps
// its record until the directory is gone. The new journal begins with a
// record naming a new generation at random, so no checkpoint of the old
// journal can pass for one of the new.
//
// Every commit holds the journal lock shared from the open of the
// journal to its close, and compaction holds it exclusive from the
// replay it reads the sessions from through the rename of the new
// journal into place, so no record lands in the journal being replaced
// and none is committed that the replay did not see. It holds the gc
// lock throughout, so no sweep or pack reads journal offsets across it.
// A crash before the rename leaves the old journal, which still says
// everything; after it, the new one, whose dropped sessions' files were
// fsynced first. A process that read the old journal notices the new
// file at its next replay and reads that from the start.

// damagedPrefix names a journal a compaction replaced while it held
// damaged lines, kept for Verify to report and a person to read.
const damagedPrefix = "journal.damaged-"

// opJournal is the op of the record that opens a compacted journal.
const opJournal = "journal"

// opSettled is the op of the record a writer commits as it takes up a
// session the journal holds nothing of: its Seq is how many entries of
// the session's log stood before, which recovery trusts as they stand
// rather than taking them for entries a damaged journal lost, and its
// Head the head then, which recovery falls back to as it would to an
// earlier head record.
const opSettled = "settled"

// journalSession is the session a journal record is filed under: not a
// valid session ID, so no store takes it for a session's.
const journalSession = "*"

// compactAt is the journal size past which a writing store compacts on
// its own, after an append or on Close. A variable so tests can lower
// it.
var compactAt int64 = 16 << 20

// compactWait bounds how long an automatic compaction waits for other
// processes' commits to finish before it gives up until later.
const compactWait = 2 * time.Second

// ErrCompactUnsupported is returned by Compact on a platform whose locks
// cannot keep other processes' commits out of a journal being replaced.
var ErrCompactUnsupported = errors.New("cas: compaction needs flock")

// Compact replaces the journal with one holding only the records of
// sessions whose files do not yet stand for them, and returns how many
// sessions' records it dropped. A store compacts on its own once the
// journal passes 16 MiB; Compact does it now. It returns ErrSweepRunning
// when a sweep, pack or compaction holds the gc lock.
func (s *Store) Compact(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.readOnly {
		return 0, errReadOnly()
	}
	if !sharedLocks {
		return 0, ErrCompactUnsupported
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactLocked(ctx, 0)
}

// maybeCompact compacts when this store's last commit left the journal
// compactAt past what the last compaction carried into it, which its
// opening record says: a compaction that had to carry much, for sessions
// still being written, is not followed by another until as much again
// has been committed, so each pays for compactAt of new records however
// much it carries. A compaction that cannot proceed, because the gc lock
// is taken or other processes' commits held the journal lock too long,
// is tried again once the journal has grown another quarter of
// compactAt. It runs under mu, and its failure is no failure of the
// caller's.
func (s *Store) maybeCompact() {
	if s.readOnly || !sharedLocks || s.journalSize < max(compactAt, s.compactRetryAt) {
		return
	}
	if s.journalSize < s.journalBase()+compactAt {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), compactWait)
	defer cancel()
	if _, err := s.compactLocked(ctx, compactAt); err != nil {
		s.compactRetryAt = s.journalSize + compactAt/4
	}
}

// journalBase is how much of the journal the compaction that wrote it
// carried, from its opening record, or 0 for a journal no compaction
// wrote.
func (s *Store) journalBase() int64 {
	f, err := os.Open(filepath.Join(s.root, "journal"))
	if err != nil {
		return 0
	}
	defer f.Close()
	head := make([]byte, checkpointTail)
	n, _ := f.ReadAt(head, 0)
	line, _, ok := bytes.Cut(head[:n], []byte("\n"))
	if !ok {
		return 0
	}
	rec, _, err := decodeRecord(line)
	if err != nil || rec.Op != opJournal {
		return 0
	}
	return int64(len(line)+1) + rec.Size
}

// journalLock takes the journal lock shared, as every commit does. It
// passes through the gate first: a compaction waiting for the journal
// lock holds the gate exclusive, so commits that arrive then wait
// behind it rather than keeping the journal lock shared without a gap,
// which a compaction polling for it would never find. The gate is held
// for no longer than it takes to pass, so a compaction finds it free.
func (s *Store) journalLock() (*dirLock, error) {
	gate, err := lockShared(context.Background(), filepath.Join(s.root, "journal.gate"))
	if err != nil {
		return nil, err
	}
	gate.release()
	return lockShared(context.Background(), filepath.Join(s.root, "journal.lock"))
}

// compactLocked compacts under mu, unless the journal has grown less
// than atLeast past what the last compaction carried by the time the
// locks are held, as when another process compacted first.
func (s *Store) compactLocked(ctx context.Context, atLeast int64) (int, error) {
	gc, err := s.gcLock()
	if err != nil {
		return 0, err
	}
	defer gc.release()
	// This store's own lazy appends become durable and say so first,
	// so the sessions it holds can be dropped with the rest. That
	// commits, taking the journal lock shared, so it comes before the
	// lock is taken exclusive.
	if err := s.syncJournal(); err != nil {
		return 0, err
	}
	gate, err := lockExclusive(ctx, filepath.Join(s.root, "journal.gate"))
	if err != nil {
		return 0, err
	}
	defer gate.release()
	jl, err := lockExclusive(ctx, filepath.Join(s.root, "journal.lock"))
	if err != nil {
		return 0, err
	}
	defer jl.release()
	path := filepath.Join(s.root, "journal")
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("cas: journal: %w", err)
	}
	s.journalSize = info.Size()
	if atLeast > 0 && info.Size() < s.journalBase()+atLeast {
		return 0, nil // another process compacted first
	}
	scan, err := s.replay()
	if err != nil {
		return 0, err
	}
	if len(scan.damage) > 0 {
		// The damaged lines are what Verify reports, and the new journal
		// cannot hold them; the old journal is kept beside it, by a link,
		// until someone looks.
		aside := filepath.Join(s.root, fmt.Sprintf("%s%d", damagedPrefix, time.Now().UnixNano()))
		if err := os.Link(path, aside); err != nil {
			return 0, fmt.Errorf("cas: compact: keep the damaged journal: %w", err)
		}
	}
	ids := make([]string, 0, len(scan.states))
	for id := range scan.states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var carry []journalRecord
	var locks []*dirLock
	defer func() {
		for _, lk := range locks {
			lk.release()
		}
	}()
	toSync := map[string]bool{}
	dropped := 0
	for _, id := range ids {
		st := scan.states[id]
		keep, files, lk := s.settle(id, st, scan, len(locks))
		if lk != nil {
			locks = append(locks, lk)
		}
		for _, f := range files {
			toSync[f] = true
		}
		if keep == nil {
			dropped++
			continue
		}
		carry = append(carry, keep...)
	}
	// The files the dropped records stood for reach the disk before the
	// journal that no longer holds those records does.
	if err := syncAll(toSync, fsyncPath); err != nil {
		return 0, fmt.Errorf("cas: compact: %w", err)
	}
	gen := make([]byte, 16)
	if _, err := rand.Read(gen); err != nil {
		return 0, err
	}
	var body bytes.Buffer
	for _, r := range carry {
		line, err := r.encode()
		if err != nil {
			return 0, err
		}
		body.Write(line)
	}
	// The opening record names the generation and how much was carried.
	opening, err := journalRecord{Op: opJournal, Session: journalSession, Mark: hex.EncodeToString(gen), Size: int64(body.Len())}.encode()
	if err != nil {
		return 0, err
	}
	var buf bytes.Buffer
	buf.Write(opening)
	buf.Write(body.Bytes())
	tmp, err := os.CreateTemp(s.root, "journal.tmp-*")
	if err != nil {
		return 0, fmt.Errorf("cas: compact: %w", err)
	}
	_, werr := tmp.Write(buf.Bytes())
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), path)
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return 0, fmt.Errorf("cas: compact: %w", werr)
	}
	if err := syncDir(s.root); err != nil {
		return dropped, fmt.Errorf("cas: compact: %w", err)
	}
	s.journalSize, s.journalDirty = int64(buf.Len()), false
	s.compactions++
	// The checkpoint was of the old journal and no longer matches.
	os.Remove(filepath.Join(s.root, checkpointName))
	return dropped, nil
}

// maxCompactLocks bounds the session locks one compaction holds; a
// session it would drop past that is carried to the next.
const maxCompactLocks = 512

// settle decides what the new journal keeps of one session: nil when
// its files stand for its records, with the files to fsync before they
// go and the session's lock, which the compaction holds until the new
// journal is in place, so no writer takes the session up from the old
// journal's view and commits to the new one as if its records were
// still there; or the records to carry.
//
// A session another holder has open keeps one settled record in place
// of its records, naming the log's length and the head its files show,
// so what the holder commits next follows the record a writer taking
// it up would have written. A session whose directory has no header
// yet is being created or imported, and keeps its records, as does a
// delete whose directory is still there or whose deleter still holds
// it.
func (s *Store) settle(id string, st *sessionState, scan *journalScan, locks int) (keep []journalRecord, files []string, lk *dirLock) {
	carryAll := func() []journalRecord {
		var out []journalRecord
		if st.deleted {
			return []journalRecord{{Op: "delete", Session: id}}
		}
		if st.created {
			out = append(out, journalRecord{Op: "create", Session: id, Base: st.base})
		}
		out = append(out, st.recs...)
		if out == nil {
			out = []journalRecord{} // nothing to carry, yet kept
		}
		return out
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return carryAll(), nil, nil // not a session's; kept as found
	}
	var held bool
	if locks < maxCompactLocks {
		lk, err = s.lockSession(id)
		switch {
		case errors.Is(err, agentsession.ErrSessionLocked):
			held = true
		case err != nil:
			return carryAll(), nil, nil
		}
	} else {
		held = true // no lock to spare: treated as held
	}
	carry := func(recs []journalRecord) ([]journalRecord, []string, *dirLock) {
		lk.release()
		return recs, nil, nil
	}
	if st.deleted {
		if _, err := os.Stat(dir); held || !errors.Is(err, os.ErrNotExist) {
			return carry(carryAll())
		}
		// The directory's removal reaches the disk before the delete
		// record stops saying it happened.
		return nil, []string{filepath.Join(s.root, "sessions")}, lk
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); err != nil {
		if _, derr := os.Stat(dir); held || derr == nil {
			return carry(carryAll()) // being created or imported
		}
		return nil, nil, lk // nothing on disk for recovery to read
	}
	v, err := s.reconcile(id, dir, scan)
	if err != nil || !v.exists {
		return carry(carryAll())
	}
	if v.logChanged || v.headChanged || v.markChanged || v.damaged || len(v.adopt) > 0 || len(v.dropped) > 0 || lazyUnsynced(st) {
		return carry(carryAll())
	}
	for _, name := range []string{"log", "HEAD", "record", "header"} {
		files = append(files, filepath.Join(dir, name))
	}
	files = append(files, dir)
	if held {
		// Its files are fsynced like any settled session's, and one
		// record stands for all it had.
		return []journalRecord{{Op: opSettled, Session: id, Seq: len(v.log), Head: v.head}}, files, nil
	}
	return nil, files, lk
}

// lazyUnsynced reports whether a lazy append follows the session's last
// sync record.
func lazyUnsynced(st *sessionState) bool {
	synced := -1
	for i, r := range st.recs {
		if r.Op == "sync" {
			synced = i
		}
	}
	for i := synced + 1; i < len(st.recs); i++ {
		if r := st.recs[i]; r.Op == "append" && r.Lazy {
			return true
		}
	}
	return false
}

// fsyncPath fsyncs a file or directory; one that is not there needs
// nothing.
func fsyncPath(p string) error {
	f, err := os.Open(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	err = f.Sync()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
