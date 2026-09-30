package cas

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// A session's log is its write-ahead log and its record, as RFC 0002
// says: one line per record, each carrying a checksum, appended under
// the session's lock by the one process holding it. An append is
// accepted when its record is written and committed when the log is
// fsynced after its objects. HEAD and record are indexes of the log,
// rebuilt from it; nothing outside the session's directory says
// anything about the session.
//
// The records:
//
//	create  the session began, with its base
//	mark    the record mark: "record" or "mirror"
//	append  an entry, its sequence, its size and where the head went;
//	        lazy when it was acknowledged before it was committed
//	head    a head move
//	sync    a commit: every lazy append before it is durable
//	lost    an append recovery found a crash took
const (
	opCreate = "create"
	opAppend = "append"
	opHead   = "head"
	opMark   = "mark"
	opSync   = "sync"
	opLost   = "lost"
)

// logName is the session's log in its directory.
const logName = "log"

// sessionLog is a session's log as read: its records in order, the
// damage found, where its whole lines end, and whether it is in the
// format of a store from before logs were per session.
type sessionLog struct {
	recs   []logRecord
	damage []LogDamage
	whole  int64 // bytes of whole lines; anything after is a torn tail
	size   int64
	legacy bool
	// lost is set when damage cost a record, rather than a newline
	// between two records that both read; lossRec is where in recs the
	// first such line's records begin, and lossOff where the line does.
	lost    bool
	lossRec int
	lossOff int64
	// unwritten is set while every line whose damage cost a record
	// holds zero bytes: a block a crash left unwritten, not bytes
	// changed after they were written.
	unwritten bool
	// unterminated is set when the last line is a whole record, its
	// checksum good, that a crash left without its newline: it counts,
	// and the holder ends the line rather than cutting it.
	unterminated bool
}

// readSessionLog reads the log in dir from the byte offset from. A log
// that is not there is empty. A line that fails its checksum is damage,
// reported and skipped; a last line without its newline is a record a
// crash cut short, which the log's holder truncates.
func readSessionLog(dir string, from int64) (sessionLog, error) {
	var l sessionLog
	f, err := os.Open(filepath.Join(dir, logName))
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return l, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return l, err
	}
	l.size, l.whole = info.Size(), from
	if from == 0 && info.Size() > 0 {
		// A log of bare hashes is one from before logs were per session;
		// any other first line is read, and damage in it reported.
		var first [7]byte
		if n, _ := f.ReadAt(first[:], 0); string(first[:n]) == "sha256:" {
			l.legacy = true
			return l, nil
		}
	}
	if _, err := f.Seek(from, io.SeekStart); err != nil {
		return l, err
	}
	br := bufio.NewReaderSize(f, 256<<10)
	lineNo := 0
	for {
		line, err := br.ReadBytes('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return l, err
		}
		if len(line) == 0 {
			return l, nil
		}
		if line[len(line)-1] != '\n' {
			// A last line without its newline: a torn write, unless it
			// holds whole records that pass their checksums.
			recs, _ := decodeLine(line)
			for _, r := range recs {
				if r.checked {
					l.recs = append(l.recs, r)
					l.unterminated = true
				}
			}
			if l.unterminated {
				l.whole += int64(len(line))
			}
			return l, nil
		}
		lineNo++
		start := l.whole
		l.whole += int64(len(line))
		recs, derr := decodeLine(line)
		if derr == nil && (!bytes.HasPrefix(line, recordOpening) || len(recs) != bytes.Count(line, recordOpening)) {
			// The journal's reading passes over torn bytes a later record
			// landed after. A session's holder cuts a torn tail before it
			// writes again, so here such bytes are a record that was
			// whole and is not: a block of zeros where a line was, run
			// into the next.
			derr = errSkipped
		}
		if derr != nil {
			l.damage = append(l.damage, LogDamage{Line: lineNo, Offset: start, Err: derr})
			if !errors.Is(derr, errNewline) {
				if !l.lost {
					l.lost, l.lossRec, l.lossOff, l.unwritten = true, len(l.recs), start, true
				}
				if bytes.IndexByte(line, 0) < 0 {
					l.unwritten = false
				}
			}
		}
		l.recs = append(l.recs, recs...)
	}
}

// encodeRecords renders records as log lines, marking an append lazy
// when it is not durable.
func encodeRecords(durable bool, recs []logRecord) ([]byte, error) {
	var buf bytes.Buffer
	for _, r := range recs {
		if !durable && r.Op == opAppend {
			r.Lazy = true
		}
		line, err := r.encode()
		if err != nil {
			return nil, err
		}
		buf.Write(line)
	}
	return buf.Bytes(), nil
}

// openLog opens a session's log for appending, creating it.
func openLog(dir string) (*os.File, error) {
	return os.OpenFile(filepath.Join(dir, logName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
}

// appendRecords accepts records into a session's log, through the
// handle's open log when it has one. Durable, it is a commit: every
// object the session's appends wrote or found lazily is fsynced first,
// then the log.
func (s *Store) appendRecords(h *handle, dir string, durable bool, recs ...logRecord) error {
	if s.readOnly {
		return errReadOnly()
	}
	data, err := encodeRecords(durable, recs)
	if err != nil {
		return err
	}
	if durable {
		// A session's commit flushes what its own appends wrote or found
		// lazily; recovery, which has no handle, flushes the store's own.
		var pend *pendSet
		if h != nil {
			pend = h.pend
		}
		if err := s.objs.flushSet(pend); err != nil {
			return fmt.Errorf("cas: flush: %w", err)
		}
	}
	f := (*os.File)(nil)
	if h != nil {
		f = h.logf
	}
	if f == nil {
		if f, err = openLog(dir); err != nil {
			return fmt.Errorf("cas: log: %w", err)
		}
		defer f.Close()
	}
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("cas: log: %w", err)
	}
	before := info.Size()
	_, err = f.Write(data)
	fsyncFailed := false
	if err == nil && durable {
		err = syncLog(f)
		fsyncFailed = err != nil
	}
	if err != nil {
		// A record that failed is not left for a later open to find: the
		// log is cut back to where it was. If even that fails, what the
		// log holds is unknown, and the session is dropped so its next
		// open reads the disk.
		uncertain := func(err error) error {
			// A held session's state no longer follows from its log;
			// it is let go, and its next open recovers from the disk.
			// Every record names the session it belongs to.
			if h != nil {
				s.dropHandle(recs[0].Session, h)
			}
			return err
		}
		if terr := f.Truncate(before); terr != nil {
			return uncertain(fmt.Errorf("%w: %v; and cutting it back: %v", errLogUncertain, err, terr))
		}
		// A failed fsync may have marked the pages of earlier records
		// clean without writing them, and a later fsync of the file
		// would say they were written; so the log is written again, to
		// a new file, from the bytes the page cache still holds.
		var serr error
		if !fsyncFailed {
			serr = syncLog(f)
		}
		if fsyncFailed || serr != nil {
			if rerr := s.rewriteLog(h, dir, before); rerr != nil {
				return uncertain(fmt.Errorf("%w: %v; and writing it again: %v", errLogUncertain, err, rerr))
			}
		}
		return fmt.Errorf("cas: log: %w", err)
	}
	return nil
}

// rewriteLog writes a session's log again, durably, as a new file of
// the size it had, and points the handle's open log at it.
func (s *Store) rewriteLog(h *handle, dir string, size int64) error {
	path := filepath.Join(dir, logName)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("the log is %d bytes, not %d", len(data), size)
	}
	if err := writeLogFile(path, data); err != nil {
		return err
	}
	if h != nil && h.logf != nil {
		h.logf.Close()
		h.logf, err = openLog(dir)
	}
	return err
}

// writeLogFile writes a log again; a variable so a test can fail it.
var writeLogFile = writeAtomic

// errSkipped is a line of a session's log holding bytes that are no
// record ahead of or between its records.
var errSkipped = errors.New("bytes that are no record run into a record")

// errLogUncertain is an append that failed and could not be taken back
// out of the log. appendRecords has let the session's handle go.
var errLogUncertain = errors.New("cas: a failed append could not be taken out of the log")

// syncLog fsyncs a session's log; a variable so a test can fail it.
var syncLog = func(f *os.File) error { return f.Sync() }

// mendTail cuts a torn last line from a session's log, and ends one a
// crash left whole but without its newline, which only the session's
// holder does, so the next record starts a line of its own.
func mendTail(dir string, whole int64, unterminated bool) error {
	f, err := os.OpenFile(filepath.Join(dir, logName), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Truncate(whole); err != nil {
		return err
	}
	if unterminated {
		if _, err := f.WriteAt([]byte{'\n'}, whole); err != nil {
			return err
		}
	}
	return f.Sync()
}

// tailLoss reports whether a log's lossy damage is what a crash leaves
// in an uncommitted tail: blocks left unwritten, holding zeros, with
// nothing after the first of them but lazy appends, none of which a
// commit covers. A crash damages only what was written after the last
// fsync, and on a filesystem that writes a file out of order it can do
// so anywhere in that tail; what it takes there is working state, cut
// as any crash's. Damage that a committed record follows, or that
// changed bytes rather than leaving them unwritten, is damage.
func tailLoss(l sessionLog) bool {
	if !l.lost || !l.unwritten {
		return false
	}
	for _, r := range l.recs[l.lossRec:] {
		if r.Op != opAppend || !r.Lazy {
			return false
		}
	}
	return true
}

// ownEntries returns the entries the records append, each once, in
// order: what a session's log names, lost appends included, which is
// what a sweep keeps.
func ownEntries(recs []logRecord) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range recs {
		if r.Op == opAppend && r.Entry != "" && !seen[r.Entry] {
			seen[r.Entry] = true
			out = append(out, r.Entry)
		}
	}
	return out
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

// heldEntries returns the entries a session holds: those its records
// append, less the ones a later lost record says a crash took. A lost
// append is no entry of the session's, and no fork may hang from it.
func heldEntries(recs []logRecord) []string {
	lostAt := map[string]int{}
	for i, r := range recs {
		if r.Op == opLost {
			lostAt[r.Entry] = i
		}
	}
	var out []string
	seen := map[string]bool{}
	for i, r := range recs {
		if r.Op != opAppend || r.Entry == "" || seen[r.Entry] {
			continue
		}
		if at, ok := lostAt[r.Entry]; ok && at > i {
			continue
		}
		seen[r.Entry] = true
		out = append(out, r.Entry)
	}
	return out
}

// firstLoss returns the first damage in a log that cost a record.
func firstLoss(l sessionLog) error {
	for _, d := range l.damage {
		if !errors.Is(d.Err, errNewline) {
			return d
		}
	}
	return nil
}
