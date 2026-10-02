package jsonl

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
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

// WithFollowInterval sets how often a follower looks at a session file
// no writer in this process rang for: the shortest wait, which it keeps
// while the file keeps growing and doubles while it does not, up to a
// second or the interval itself when that is longer. The default is
// 100 ms. A follower of a session this store appends to is woken by the
// append and does not wait for the interval.
func WithFollowInterval(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.followEvery = d
		}
	}
}

// Follow implements [agentsession.Follower] by tailing the session's
// file: it reads the file up to its last newline for the snapshot, then
// reads from that offset each time the file grows, which a stat finds
// and an append through this store announces. It takes no lock, writes
// nothing and trims nothing, so a read-only store follows too.
//
// A line still being written is not consumed until its newline lands.
// The file is replaced, and the follower yields a Reset, when the
// writer's first append to an older minor raises the header's format
// (the file is rewritten and renamed over), when the file shrinks, and
// when its inode changes. A file gone is [agentsession.ErrNoSession],
// and so is one under the same name that is another session. The cursor
// names the byte offset after a line, the file's inode and its header.
func (s *Store) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return func(yield func(agentsession.Change, error) bool) {
		path, err := s.find(id)
		if err != nil {
			yield(agentsession.Change{}, err)
			return
		}
		src := &source{store: s, id: id, path: path}
		for c, err := range follow.Run(ctx, src, from) {
			if !yield(c, err) {
				return
			}
		}
	}
}

// source is the jsonl side of a follow: one file.
type source struct {
	store *Store
	id    string
	path  string
}

// pos is what a cursor holds of a session file.
type pos struct {
	off   int64  // the byte after the last line consumed
	ino   uint64 // the file's inode, 0 when unknown
	raw   string // hash of the header line as it stands
	ident string // hash of the header without its format
}

func (p pos) cursor() agentsession.Cursor {
	return agentsession.Cursor(strconv.FormatInt(p.off, 10) + ":" + strconv.FormatUint(p.ino, 10) + ":" + p.raw + ":" + p.ident)
}

func parsePos(c agentsession.Cursor) (pos, bool) {
	f := strings.Split(string(c), ":")
	if len(f) != 4 {
		return pos{}, false
	}
	off, err1 := strconv.ParseInt(f[0], 10, 64)
	ino, err2 := strconv.ParseUint(f[1], 10, 64)
	if err1 != nil || err2 != nil || off <= 0 {
		return pos{}, false
	}
	return pos{off, ino, f[2], f[3]}, true
}

// headerIdent hashes a header line: raw as written, and ident without
// the format, which is all the writer's rewrite of a file changes.
func headerIdent(line []byte) (raw, ident string, err error) {
	line = bytes.TrimRight(line, "\r")
	var h agentsession.Header
	if err := h.UnmarshalJSON(line); err != nil {
		return "", "", err
	}
	h.Format = ""
	b, err := h.MarshalJSON()
	if err != nil {
		return "", "", err
	}
	r := sha256.Sum256(line)
	i := sha256.Sum256(b)
	return hex.EncodeToString(r[:8]), hex.EncodeToString(i[:8]), nil
}

func (src *source) gone() error {
	return fmt.Errorf("%w: %s", agentsession.ErrNoSession, src.id)
}

// Load reads the file up to the cursor, or up to its last newline.
func (src *source) Load(ctx context.Context, upTo agentsession.Cursor) (*agentsession.Session, agentsession.Cursor, error) {
	var want pos
	if upTo != "" {
		var ok bool
		if want, ok = parsePos(upTo); !ok {
			return nil, "", follow.ErrStale
		}
	}
	f, err := os.Open(src.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", src.gone()
	}
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	end := bytes.LastIndexByte(data, '\n') + 1
	if upTo != "" {
		if want.off > int64(end) || data[want.off-1] != '\n' {
			return nil, "", follow.ErrStale
		}
		end = int(want.off)
	}
	data = data[:end]
	line, _, _ := bytes.Cut(data, []byte{'\n'})
	raw, ident, err := headerIdent(line)
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: %s: header: %w", src.path, err)
	}
	p := pos{off: int64(end), ino: inode(fi), raw: raw, ident: ident}
	if upTo != "" && (raw != want.raw || ident != want.ident || (want.ino != 0 && p.ino != 0 && want.ino != p.ino)) {
		return nil, "", follow.ErrStale
	}
	sess, err := agentsession.Read(bytes.NewReader(data))
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: %s: %w", src.path, err)
	}
	if sess.ID() != src.id {
		return nil, "", src.gone()
	}
	return sess, p.cursor(), nil
}

// Tail reads what the file gained since the cursor.
func (src *source) Tail(ctx context.Context, cur agentsession.Cursor) ([]follow.Item, error) {
	p, ok := parsePos(cur)
	if !ok {
		return nil, follow.ErrStale
	}
	f, err := os.Open(src.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, src.gone()
	}
	if err != nil {
		return nil, fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	size, ino := fi.Size(), inode(fi)
	if size == p.off && (p.ino == 0 || ino == 0 || ino == p.ino) {
		return nil, nil
	}
	// The file changed. Which file it is decides what that means: the
	// same session rewritten is a reset, another session is gone.
	raw, ident, err := src.header(f)
	if err != nil {
		return nil, err
	}
	if ident != p.ident {
		return nil, src.gone()
	}
	if raw != p.raw || (p.ino != 0 && ino != 0 && ino != p.ino) || size < p.off {
		return nil, follow.ErrStale
	}
	buf := make([]byte, size-p.off)
	n, err := f.ReadAt(buf, p.off)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("jsonl: follow %s: %w", src.path, err)
	}
	buf = buf[:n]
	end := bytes.LastIndexByte(buf, '\n') + 1 // a line without its newline waits
	var items []follow.Item
	off := p.off
	for _, line := range bytes.SplitAfter(buf[:end], []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		off += int64(len(line))
		line = bytes.TrimRight(line, "\r\n")
		if len(line) == 0 {
			items = append(items, follow.Item{Skip: true, Cursor: pos{off, ino, p.raw, p.ident}.cursor()})
			continue
		}
		e, err := agentsession.UnmarshalEntry(line)
		if err != nil {
			// A line the follower cannot decode is for Read to judge.
			return nil, follow.ErrStale
		}
		items = append(items, follow.Item{Entry: e, Cursor: pos{off, ino, p.raw, p.ident}.cursor()})
	}
	return items, nil
}

// header reads the file's first line and hashes it.
func (src *source) header(f *os.File) (raw, ident string, err error) {
	var line []byte
	buf := make([]byte, 4096)
	for off := int64(0); ; off += int64(len(buf)) {
		n, rerr := f.ReadAt(buf, off)
		if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
			line = append(line, buf[:i]...)
			break
		}
		line = append(line, buf[:n]...)
		if rerr != nil {
			// No newline yet: the header is still being written.
			if errors.Is(rerr, io.EOF) {
				return "", "", follow.ErrStale
			}
			return "", "", fmt.Errorf("jsonl: follow %s: %w", src.path, rerr)
		}
	}
	raw, ident, err = headerIdent(line)
	if err != nil {
		return "", "", follow.ErrStale
	}
	return raw, ident, nil
}

func (src *source) Watch() (func() <-chan struct{}, func()) { return src.store.hub.Watch(src.id) }

func (src *source) Interval() (time.Duration, time.Duration) {
	lo := src.store.followEvery
	if lo <= 0 {
		lo = defaultFollowInterval
	}
	return lo, max(lo, maxFollowInterval)
}
