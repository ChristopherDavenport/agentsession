// Package follow is the loop the file and database stores share to
// implement [agentsession.Follower]: a store says how to read a session
// up to a cursor and what has been written beyond one, and Run turns
// that into the iterator, keeping the follower's own session, yielding
// the changes in order and waiting between them.
package follow

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/wake"
)

// ErrStale is what a Source reports when a cursor names no place in
// the session's current log, because the log was replaced: the
// follower reads the session again and yields a Reset. A session that
// is gone, or that is another session now under the same ID, is
// [agentsession.ErrNoSession] instead.
var ErrStale = errors.New("follow: the log was replaced")

// Item is one record a Source found beyond a cursor: an entry the
// store accepted, a head record naming Leaf when Entry is nil, or, when
// Skip is set, a record that changes nothing a follower sees, which
// only moves the cursor past it.
type Item struct {
	Entry  agentsession.Entry
	Leaf   string
	Skip   bool
	Cursor agentsession.Cursor
}

// Source is a store's side of a follow on one session.
type Source interface {
	// Load reads the session as the log held it at the cursor upTo, or
	// as it holds it now when upTo is empty, and returns the session
	// with the cursor at its end. It reports ErrStale for a cursor the
	// log does not hold.
	Load(ctx context.Context, upTo agentsession.Cursor) (*agentsession.Session, agentsession.Cursor, error)
	// Tail returns the complete records the log holds beyond cur, in
	// order, each with the cursor after it. It returns none, and no
	// error, when there is nothing new or the end is incomplete.
	Tail(ctx context.Context, cur agentsession.Cursor) ([]Item, error)
	// Watch registers for same-process wakeups: c returns the channel
	// the next change closes, and done releases the registration.
	Watch() (c func() <-chan struct{}, done func())
	// Interval is the poll interval: the shortest wait, and the longest
	// the backoff reaches while nothing changes.
	Interval() (min, max time.Duration)
}

// Run follows the session src reads, from the cursor from.
func Run(ctx context.Context, src Source, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return func(yield func(agentsession.Change, error) bool) {
		wakeCh, done := src.Watch()
		defer done()
		_, fileLeaf := src.(FileLeaf)
		fail := func(err error) {
			if ctx.Err() == nil {
				yield(agentsession.Change{}, err)
			}
		}
		var (
			sess *agentsession.Session
			cur  agentsession.Cursor
			err  error
		)
		// reload reads the session afresh and yields it as kind.
		reload := func(kind agentsession.ChangeKind) bool {
			if sess, cur, err = src.Load(ctx, ""); err != nil {
				fail(err)
				return false
			}
			return yield(agentsession.Change{Kind: kind, Session: sess, Cursor: cur}, nil)
		}
		if from == "" {
			if !reload(agentsession.Snapshot) {
				return
			}
		} else {
			switch sess, cur, err = src.Load(ctx, from); {
			case err == nil:
			case errors.Is(err, ErrStale):
				if !reload(agentsession.Reset) {
					return
				}
			default:
				fail(err)
				return
			}
		}
		lo, hi := src.Interval()
		poll := wake.Poll{Min: lo, Max: hi}
	next:
		for {
			if ctx.Err() != nil {
				return
			}
			ch := wakeCh()
			items, err := src.Tail(ctx, cur)
			switch {
			case err == nil:
			case errors.Is(err, ErrStale):
				if !reload(agentsession.Reset) {
					return
				}
				continue
			default:
				fail(err)
				return
			}
			for _, it := range items {
				if ctx.Err() != nil {
					return
				}
				c, err := apply(sess, it, fileLeaf)
				if err != nil {
					// What the log holds does not extend the session
					// this follower has: read it again.
					if !reload(agentsession.Reset) {
						return
					}
					continue next
				}
				cur = it.Cursor
				for _, c := range c {
					if !yield(c, nil) {
						return
					}
				}
			}
			if len(items) > 0 {
				poll.Reset()
				continue
			}
			if !wake.Wait(ctx.Done(), ch, poll.Next()) {
				return
			}
		}
	}
}

// apply extends sess with an item and returns the changes it makes: an
// Appended, and a Head after it when the entry moved the leaf, or a
// Head alone for a head record. A head that names the leaf the session
// already has is no change: cas logs the head a writer moved before the
// leaf label that records it, and the follower reports the move once.
func apply(sess *agentsession.Session, it Item, fileLeaf bool) ([]agentsession.Change, error) {
	if it.Skip {
		return nil, nil
	}
	before := sess.Leaf()
	if it.Entry == nil {
		if it.Leaf != "" {
			if err := sess.Branch(it.Leaf); err != nil {
				return nil, err
			}
		} else {
			sess.ResetLeaf()
		}
		if it.Leaf == before {
			return nil, nil
		}
		return []agentsession.Change{{Kind: agentsession.Head, Session: sess, Leaf: it.Leaf, Cursor: it.Cursor}}, nil
	}
	commit := sess.Commit
	if fileLeaf {
		commit = sess.Extend
	}
	r, err := commit(it.Entry)
	if err != nil {
		return nil, fmt.Errorf("follow: %w", err)
	}
	if r.Outcome == agentsession.Held {
		return nil, nil
	}
	out := []agentsession.Change{{Kind: agentsession.Appended, Session: sess, ID: r.ID, Entry: it.Entry, Cursor: it.Cursor}}
	moved := r.Outcome == agentsession.LeafMoved
	if fileLeaf {
		// Under Read's rule an append that becomes the leaf says so by
		// being appended; a move elsewhere is a leaf label's.
		moved = sess.Leaf() != r.ID
	}
	if moved && sess.Leaf() != before {
		out = append(out, agentsession.Change{Kind: agentsession.Head, Session: sess, Leaf: sess.Leaf(), Cursor: it.Cursor})
	}
	return out, nil
}

// FileLeaf is implemented by a Source whose store reads a session with
// [agentsession.Read], which places the leaf by the file's order and its
// leaf labels. The follower then adds each entry with
// [agentsession.Session.Extend], so its leaf is the one a Read would
// give, and not the live rule's, which a writer's Session.Branch leaves
// behind. A store that records its head, as cas does, gives the leaf in
// its Items instead.
type FileLeaf interface {
	FileLeaf()
}
