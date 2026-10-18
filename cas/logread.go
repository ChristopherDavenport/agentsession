package cas

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/follow"
)

// LogRecord is one record of a session's log as ReadLog reads it: an
// append, with the entry it appended, or a head move, with Entry nil.
// The records a follower skips (a commit, a mark, the session's
// creation) are not listed; the cursor moves past them.
type LogRecord struct {
	// Entry is the entry the record appended, nil for a head move.
	Entry agentsession.Entry
	// Seq is the entry's place among the session's entries, from 1, as a
	// follower counts them: a fork's prefix first. Zero for a head move.
	Seq int
	// Head is the session's head after the record, "" for none.
	Head string
	// Durable is set when the record was committed when it was read.
	Durable bool
	// Cursor is the position after the record: a follower's cursor, to
	// read or follow from.
	Cursor agentsession.Cursor
}

// Log is a session's log as one ReadLog call read it.
type Log struct {
	Header agentsession.Header
	// Mark is the session's record mark as the log read says it.
	Mark string
	// Session is the session as of Cursor, built as a follower builds it.
	Session *agentsession.Session
	// Records are the log's records after the cursor asked for, in log
	// order.
	Records []LogRecord
	// Cursor is where the read ended, past the records it skipped too.
	Cursor agentsession.Cursor
	// Reset is set when the cursor asked for named no place in the log,
	// because the log was replaced since: Records are then the whole log,
	// and Session is built from its start.
	Reset bool
}

// ReadLog reads a session's log after the cursor after, from its start
// when after is empty: RFC 0003's log read. Each record says whether it
// was committed when it was read, and its cursor is the one the store's
// follower gives, so a read and a follow resume from each other.
//
// With durable, it reads only what is committed, RFC 0002's reading of
// a sender: a session this store holds is committed first, so what its
// writer accepted is served, and the read stops before the first append
// no commit covers, which is another process's working state when
// another process holds the session. The head and mark are then the
// committed view's. Without durable it reads what a follower reads.
//
// It takes no hold, as Follow does: a session nobody holds is not
// opened. A cursor from before the log was replaced is answered with
// the whole log and Reset, never with a silent skip.
func (s *Store) ReadLog(ctx context.Context, id string, after agentsession.Cursor, durable bool) (Log, error) {
	if err := ctx.Err(); err != nil {
		return Log{}, err
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return Log{}, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if durable && !s.readOnly {
		// A sender commits the session before it serves it. Only a
		// session this store holds has working state of this store's.
		if h := s.held(id); h != nil {
			err := s.commitHandle(id, h)
			h.mu.Unlock()
			if err != nil {
				return Log{}, err
			}
		}
	}
	src := &source{store: s, id: id, dir: dir}
	var out Log
	start := after
	if start != "" {
		sess, cur, err := src.Load(ctx, start)
		switch {
		case err == nil:
			out.Session, start = sess, cur
		case errors.Is(err, follow.ErrStale):
			out.Reset, start = true, ""
		default:
			return Log{}, err
		}
	}
	// The whole log is read for what decides each record's durability:
	// a commit after it, and an append no commit covers before it.
	first, _, err := src.identity()
	if err != nil {
		return Log{}, err
	}
	first.off, first.tail = 0, "0.0"
	all, err := src.lines(first.cursor(), false)
	if err != nil {
		if errors.Is(err, follow.ErrStale) {
			return Log{}, fmt.Errorf("cas: session %s: its log was replaced as it was read; read it again", id)
		}
		return Log{}, err
	}
	// committedEnd is the first line holding an append that no commit
	// covers: a lazy append after the last sync record. Everything before
	// it is durable.
	lastSync := -1
	for i, l := range all {
		for _, r := range l.recs {
			if r.rec.Op == opSync {
				lastSync = i
			}
		}
	}
	committedEnd := len(all)
	for i, l := range all {
		if i <= lastSync {
			continue
		}
		for _, r := range l.recs {
			if r.rec.Op == opAppend && r.rec.Lazy {
				committedEnd = min(committedEnd, i)
			}
		}
	}
	if out.Session == nil {
		// From the start: the session as the log's first byte has it, its
		// header and any prefix, which each record then extends.
		sess, cur, err := src.Load(ctx, first.cursor())
		if err != nil {
			if !errors.Is(err, follow.ErrStale) {
				return Log{}, err
			}
			// No log yet: the session as it is, with nothing to read.
			if sess, cur, err = src.Load(ctx, ""); err != nil {
				return Log{}, err
			}
		}
		out.Session, start = sess, cur
	}
	out.Cursor = start
	hdr, err := readHeader(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Log{}, src.gone()
		}
		return Log{}, err
	}
	out.Header = hdr
	out.Mark = readMark(dir)
	startOff := int64(0)
	if p, ok := parsePos(start); ok {
		startOff = p.off
	}
	sess := out.Session
	for i, l := range all {
		p, _ := parsePos(l.cursor)
		if durable && i >= committedEnd {
			break
		}
		for _, r := range l.recs {
			if r.rec.Op == opMark && r.rec.Mark != "" {
				out.Mark = r.rec.Mark
			}
		}
		if p.off <= startOff {
			continue
		}
		for _, r := range l.recs {
			switch r.rec.Op {
			case opAppend:
				if r.rec.Entry == "" {
					continue
				}
				e, ok, err := src.entry(r.rec.Entry)
				if err != nil {
					return Log{}, err
				}
				if !ok {
					// Its objects are not there yet: a follower waits for
					// them, and the read ends before the line.
					return out, nil
				}
				// Read under the format the header names, as Read
				// reads a file: before 0.13 a redacted member is
				// unknown.
				agentsession.GateEntry(hdr.Format, e)
				res, err := sess.Commit(e)
				if err != nil {
					return Log{}, fmt.Errorf("cas: session %s: %w", id, err)
				}
				if res.Outcome == agentsession.Held {
					continue
				}
				out.Records = append(out.Records, LogRecord{Entry: e, Seq: len(sess.Entries()), Head: sess.Leaf(), Durable: i < committedEnd, Cursor: l.cursor})
			case opHead:
				before := sess.Leaf()
				if r.rec.Head != "" {
					if err := sess.Branch(r.rec.Head); err != nil {
						return Log{}, fmt.Errorf("cas: session %s: %w", id, err)
					}
				} else {
					sess.ResetLeaf()
				}
				if sess.Leaf() == before {
					continue // the head a writer moved before the label that records it
				}
				out.Records = append(out.Records, LogRecord{Head: sess.Leaf(), Durable: i < committedEnd, Cursor: l.cursor})
			}
		}
		out.Cursor = l.cursor
	}
	return out, nil
}

// LogCursor is the cursor at the end of the session's log as it stands:
// where a follow or a log read from now on starts. It reads the log's
// last line and nothing else.
func (s *Store) LogCursor(ctx context.Context, id string) (agentsession.Cursor, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return "", fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	src := &source{store: s, id: id, dir: dir}
	p, _, err := src.identity()
	if err != nil {
		return "", err
	}
	p.off, p.tail = 0, "0.0"
	lines, err := src.lines(p.cursor(), false)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return p.cursor(), nil
	}
	return lines[len(lines)-1].cursor, nil
}
