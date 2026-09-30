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
	// between two records that both read.
	lost bool
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
		var first [1]byte
		if _, err := f.ReadAt(first[:], 0); err == nil && first[0] != '{' {
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
		if derr != nil {
			l.damage = append(l.damage, LogDamage{Line: lineNo, Offset: start, Err: derr})
			if !errors.Is(derr, errNewline) {
				l.lost = true
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
// object written lazily before, this session's appends' included, is
// fsynced first, then the log.
func (s *Store) appendRecords(h *handle, dir string, durable bool, recs ...logRecord) error {
	if s.readOnly {
		return errReadOnly()
	}
	data, err := encodeRecords(durable, recs)
	if err != nil {
		return err
	}
	if durable {
		if err := s.objs.flush(); err != nil {
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
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("cas: log: %w", err)
	}
	if durable {
		if err := f.Sync(); err != nil {
			return fmt.Errorf("cas: log: %w", err)
		}
	}
	return nil
}

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

// ownEntries returns the entries the records append, each once, in
// order: what a session's log names, lost appends included, which is
// what the index and a sweep keep.
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
