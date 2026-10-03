package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"iter"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/follow"
)

const (
	defaultFollowInterval = 100 * time.Millisecond
	maxFollowInterval     = time.Second
)

// Follow implements [agentsession.Follower] by tailing the session's
// log. The snapshot is what Read builds, from the same read of the log,
// and the tail reads the log from the offset the follower stopped at
// each time its size shows it grown, which a stat finds and an append
// through this store announces. It takes no lock, opens no log, writes
// nothing and recovers nothing, so a read-only store follows too.
//
// A follower yields the records the log holds as Read would read them:
// an append whose objects the store holds, a head record, and not a
// record still being written, nor an append whose objects are not there
// yet, which it waits for. Objects a pack or a sweep moves are looked
// for again, as Read does; neither moves a log, so neither disturbs a
// follower.
//
// The log is replaced, and the follower yields a Reset, when a writer's
// open recovers a crash, when Repair or a migration writes the session
// a new log, and when a failed append is cut back out of it, which the
// follower may have seen. A session deleted ends the follow with
// [agentsession.ErrNoSession], and so does one created again under the
// same ID, which is another session. The cursor names the log's offset,
// the log's and the session directory's identity, the header's, and a
// checksum of the last line consumed, by which a log cut back and grown
// again is told from the one the cursor was taken in.
func (s *Store) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return func(yield func(agentsession.Change, error) bool) {
		dir, err := s.sessionDir(id)
		if err != nil {
			yield(agentsession.Change{}, err)
			return
		}
		src := &source{store: s, id: id, dir: dir}
		for c, err := range follow.Run(ctx, src, from) {
			if !yield(c, err) {
				return
			}
		}
	}
}

// source is the cas side of a follow: one session's log.
type source struct {
	store *Store
	id    string
	dir   string
}

// pos is what a cursor holds of a session's log.
type pos struct {
	off   int64
	log   string // the log file's device and inode
	dir   string // the session directory's identity, or "-"
	ident string // hash of the header without its format
	tail  string // crc and length of the last line consumed, "0.0" for none
}

func (p pos) cursor() agentsession.Cursor {
	return agentsession.Cursor(strconv.FormatInt(p.off, 10) + ":" + p.log + ":" + p.dir + ":" + p.ident + ":" + p.tail)
}

func parsePos(c agentsession.Cursor) (pos, bool) {
	f := strings.Split(string(c), ":")
	if len(f) != 5 {
		return pos{}, false
	}
	off, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || off < 0 {
		return pos{}, false
	}
	return pos{off, f[1], f[2], f[3], f[4]}, true
}

func (src *source) gone() error {
	return fmt.Errorf("%w: %s", agentsession.ErrNoSession, src.id)
}

// identity reads what tells this log of this session from another.
func (src *source) identity() (pos, os.FileInfo, error) {
	var p pos
	if _, err := os.Stat(src.dir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return p, nil, src.gone()
		}
		return p, nil, err
	}
	if did, ok := dirIdentity(src.dir); ok {
		p.dir = fmt.Sprintf("%x.%x.%x.%t", did.dev, did.ino, did.gen, did.hasGen)
	} else {
		p.dir = "-"
	}
	hdr, err := readHeader(src.dir)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, src.gone()
	}
	if err != nil {
		return p, nil, err
	}
	if p.ident, err = agentsession.HeaderIdent(hdr); err != nil {
		return p, nil, err
	}
	fi, err := os.Stat(filepath.Join(src.dir, logName))
	if errors.Is(err, os.ErrNotExist) {
		p.log = "-"
		return p, nil, nil
	}
	if err != nil {
		return p, nil, err
	}
	p.log = "-"
	if v, ok := versionOf(fi); ok {
		p.log = fmt.Sprintf("%x.%x", v.dev, v.ino)
	}
	return p, fi, nil
}

// sameLogSession reports whether two identities are one session's.
func sameLogSession(a, b pos) bool {
	if a.dir != "-" && b.dir != "-" && a.dir != b.dir {
		return false
	}
	return a.ident == b.ident
}

// lastLine returns the checksum and length of the line of log that ends
// at off, as the cursor holds them; "0.0" when there is none or it is
// too long to be worth checking.
func lastLine(f *os.File, off int64) (string, error) {
	const limit = 1 << 20
	if off == 0 {
		return "0.0", nil
	}
	buf := make([]byte, min(off, limit))
	n, err := f.ReadAt(buf, off-int64(len(buf)))
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	buf = buf[:n]
	if len(buf) == 0 || buf[len(buf)-1] != '\n' {
		return "", follow.ErrStale
	}
	i := bytes.LastIndexByte(buf[:len(buf)-1], '\n')
	if i < 0 && int64(len(buf)) < off {
		return "0.0", nil // a line longer than the look
	}
	line := buf[i+1:]
	return fmt.Sprintf("%x.%x", crc32.Checksum(line, crcTable), len(line)), nil
}

// checkTail reports whether the log still ends, at the cursor's offset,
// with the line the cursor consumed last.
func checkTail(f *os.File, p pos) (bool, error) {
	if p.tail == "0.0" || p.off == 0 {
		return true, nil
	}
	got, err := lastLine(f, p.off)
	if errors.Is(err, follow.ErrStale) {
		return false, nil
	}
	return got == p.tail, err
}

// Load builds the session the log's first bytes say, as Read does.
func (src *source) Load(ctx context.Context, upTo agentsession.Cursor) (*agentsession.Session, agentsession.Cursor, error) {
	s := src.store
	s.mu.Lock()
	ferr, faulty := s.faulty[src.id]
	s.mu.Unlock()
	if faulty {
		return nil, "", fmt.Errorf("cas: session %s could not be indexed: %w", src.id, ferr)
	}
	var want pos
	limit := int64(-1)
	if upTo != "" {
		var ok bool
		if want, ok = parsePos(upTo); !ok {
			return nil, "", follow.ErrStale
		}
		limit = want.off
	}
	for attempt := 0; attempt < 3; attempt++ {
		before, fi, err := src.identity()
		if err != nil {
			return nil, "", err
		}
		if upTo != "" {
			if !sameLogSession(before, want) {
				return nil, "", follow.ErrStale
			}
			if before.log != want.log || fi == nil || fi.Size() < want.off {
				return nil, "", follow.ErrStale
			}
		}
		v, err := s.reconcileTo(src.id, src.dir, false, limit)
		if err != nil {
			return nil, "", err
		}
		if !v.exists {
			return nil, "", src.gone()
		}
		sess, _, err := s.build(src.id, src.dir, v)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // moved or removed as it was read: look again
			}
			return nil, "", err
		}
		end := v.whole
		if upTo != "" {
			end = want.off
		}
		after, _, err := src.identity()
		if err != nil {
			return nil, "", err
		}
		if after.log != before.log || !sameLogSession(before, after) {
			if upTo != "" {
				return nil, "", follow.ErrStale
			}
			continue
		}
		p := before
		p.off, p.tail = end, "0.0"
		if end > 0 {
			f, err := os.Open(filepath.Join(src.dir, logName))
			if err != nil {
				return nil, "", err
			}
			p.tail, err = lastLine(f, end)
			f.Close()
			if err != nil {
				if errors.Is(err, follow.ErrStale) {
					return nil, "", follow.ErrStale
				}
				return nil, "", err
			}
			if upTo != "" && p.tail != want.tail && want.tail != "0.0" {
				return nil, "", follow.ErrStale
			}
		}
		return sess, p.cursor(), nil
	}
	return nil, "", fmt.Errorf("cas: session %s was replaced each time it was read", src.id)
}

// Tail reads the records the log gained since the cursor.
func (src *source) Tail(ctx context.Context, cur agentsession.Cursor) ([]follow.Item, error) {
	p, ok := parsePos(cur)
	if !ok {
		return nil, follow.ErrStale
	}
	now, fi, err := src.identity()
	if err != nil {
		return nil, err
	}
	if !sameLogSession(now, p) {
		return nil, src.gone() // another session under the ID, or none
	}
	if fi == nil {
		if p.off == 0 {
			return nil, nil
		}
		return nil, follow.ErrStale
	}
	if now.log != p.log || fi.Size() < p.off {
		return nil, follow.ErrStale
	}
	f, err := os.Open(filepath.Join(src.dir, logName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, follow.ErrStale
		}
		return nil, err
	}
	defer f.Close()
	if same, err := checkTail(f, p); err != nil {
		return nil, err
	} else if !same {
		return nil, follow.ErrStale
	}
	size := fi.Size()
	if size == p.off {
		return nil, nil
	}
	buf := make([]byte, size-p.off)
	n, err := f.ReadAt(buf, p.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n') + 1 // a record without its newline waits
	var items []follow.Item
	off := p.off
	for _, line := range bytes.SplitAfter(buf[:end], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		off += int64(len(line))
		recs, derr := decodeLine(line)
		if derr == nil && !wholeRecords(line, recs) {
			derr = errSkipped
		}
		if derr != nil {
			return items, fmt.Errorf("cas: session %s: log line ending at byte %d: %w", src.id, off, derr)
		}
		q := p
		q.off = off
		q.tail = fmt.Sprintf("%x.%x", crc32.Checksum(line, crcTable), len(line))
		for _, r := range recs {
			it := follow.Item{Skip: true, Cursor: q.cursor()}
			switch r.Op {
			case opAppend:
				if r.Entry == "" {
					break
				}
				e, ok, err := src.entry(r.Entry)
				if err != nil {
					return items, err
				}
				if !ok {
					return items, nil // its objects are not there yet
				}
				it = follow.Item{Entry: e, Cursor: q.cursor()}
			case opHead:
				it = follow.Item{Leaf: r.Head, Cursor: q.cursor()}
			}
			items = append(items, it)
		}
	}
	return items, nil
}

// entry loads and decodes an entry the log appends. It reports false
// when the store does not hold its objects, which a follower waits for.
func (src *source) entry(id string) (agentsession.Entry, bool, error) {
	s := src.store
	ok, err := s.present(id)
	if err != nil && !errors.Is(err, ErrCorrupt) {
		return nil, false, err
	}
	if !ok {
		return nil, false, nil
	}
	line, err := s.loadLine(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cas: session %s: %w", src.id, err)
	}
	e, err := agentsession.UnmarshalEntry(line)
	if err != nil {
		return nil, false, fmt.Errorf("cas: session %s: entry %s: %w", src.id, id, err)
	}
	return e, true, nil
}

func (src *source) Watch() (func() <-chan struct{}, func()) { return src.store.hub.Watch(src.id) }

func (src *source) Interval() (time.Duration, time.Duration) {
	lo := src.store.followEvery
	if lo <= 0 {
		lo = defaultFollowInterval
	}
	return lo, max(lo, maxFollowInterval)
}
