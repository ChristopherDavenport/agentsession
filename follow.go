package agentsession

import (
	"bytes"
	"context"
	"fmt"
	"iter"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/wake"
)

// Follower is implemented by a store that can follow a session: read
// it, then receive each change the store accepts, without holding it.
//
// Follow takes no hold and writes nothing, recovery included, as
// [Reader] does, so a store opened read-only can follow, and so can one
// that is not while another process holds the session. The iterator
// yields a [Snapshot] first, or with a cursor the changes after it, and
// then each change as the store accepts it. It ends without an error
// when ctx is done, and with [ErrNoSession] when the session is deleted;
// a session created again under the same ID is another session, and the
// follower never continues into it. Breaking out of the range stops the
// follower and releases what it held.
//
// Everything a follower yields is visible, never promised durable, as
// [Reader] says of a read: a crash, or an append that failed after
// another process saw it, can take back an entry a follower yielded,
// and the follower then yields [Reset]. Appends arrive in the order the
// store accepted them, on every branch, never in the order of a path.
type Follower interface {
	Follow(ctx context.Context, id string, from Cursor) iter.Seq2[Change, error]
}

// Cursor is a position in one session's log, opaque and the store's
// own. The zero Cursor is the start: Follow begins with a snapshot. A
// cursor from one store is meaningless to another, and a cursor the
// current log no longer holds, because the log was replaced since, is
// answered with a [Reset], never with a silent skip.
type Cursor string

// ChangeKind says what a [Change] is.
type ChangeKind int

const (
	// Snapshot: Session is the session as it was read.
	Snapshot ChangeKind = iota
	// Appended: Entry was appended and Session includes it.
	Appended
	// Head: the store recorded Leaf as the session's head. A leaf a
	// writer moved with Session.Branch and did not record is not seen.
	Head
	// Reset: the log was replaced. Session is the session read again,
	// and a consumer drops what it derived from before. It may lack an
	// entry that was yielded earlier.
	Reset
)

// String names the kind.
func (k ChangeKind) String() string {
	switch k {
	case Snapshot:
		return "snapshot"
	case Appended:
		return "appended"
	case Head:
		return "head"
	case Reset:
		return "reset"
	}
	return fmt.Sprintf("changekind(%d)", int(k))
}

// Change is one step of a follow.
type Change struct {
	Kind ChangeKind
	// Session is the follower's own session after this change, which
	// the next step extends in place: Path, ContextAt and Verify work on
	// it as on any read session. It is never the writer's, and a caller
	// never appends to it.
	Session *Session
	// ID and Entry are the entry an Appended change adds.
	ID    string
	Entry Entry
	// Leaf is the leaf a Head change records.
	Leaf string
	// Cursor is the position after this change, to resume from.
	Cursor Cursor
}

// memoryPoll bounds how long a memory follower goes without looking: a
// session appended to directly, not through the store, rings nothing.
const (
	memoryPollMin = 100 * time.Millisecond
	memoryPollMax = time.Second
)

var memoryGen atomic.Uint64

// Follow implements [Follower]. A cursor is good for the session as it
// was created, so a session deleted and created again under its ID
// answers a cursor from before with a [Reset]. Appends through the
// store wake the follower directly; one made through the session
// returned by Open is found by a slow look, since nothing rings.
func (m *MemoryStore) Follow(ctx context.Context, id string, from Cursor) iter.Seq2[Change, error] {
	return func(yield func(Change, error) bool) {
		cur, done := m.hub.Watch(id)
		defer done()
		m.mu.RLock()
		live, gen := m.sessions[id], m.gens[id]
		m.mu.RUnlock()
		if live == nil {
			yield(Change{}, fmt.Errorf("%w: %s", ErrNoSession, id))
			return
		}
		sess, n, resumed, kind, err := memoryStart(live, gen, from)
		if err != nil {
			yield(Change{}, err)
			return
		}
		if !resumed && !yield(Change{Kind: kind, Session: sess, Cursor: memoryCursor(gen, n)}, nil) {
			return
		}
		poll := wake.Poll{Min: memoryPollMin, Max: memoryPollMax}
		for {
			ch := cur()
			m.mu.RLock()
			now, g := m.sessions[id], m.gens[id]
			m.mu.RUnlock()
			if now != live || g != gen {
				yield(Change{}, fmt.Errorf("%w: %s", ErrNoSession, id))
				return
			}
			entries := live.Entries()
			for ; n < len(entries); n++ {
				if ctx.Err() != nil {
					return
				}
				// A copy, so the follower's session shares no entry
				// with the writer's.
				b, err := MarshalEntry(entries[n])
				var e Entry
				if err == nil {
					e, err = UnmarshalEntry(b)
				}
				var r Result
				if err == nil {
					r, err = sess.Commit(e)
				}
				if err != nil {
					yield(Change{}, fmt.Errorf("agentsession: follow %s: %w", id, err))
					return
				}
				c := Change{Kind: Appended, Session: sess, ID: r.ID, Entry: e, Cursor: memoryCursor(gen, n+1)}
				if !yield(c, nil) {
					return
				}
				poll.Reset()
				if r.Outcome == LeafMoved {
					c = Change{Kind: Head, Session: sess, Leaf: sess.Leaf(), Cursor: c.Cursor}
					if !yield(c, nil) {
						return
					}
				}
			}
			if !wake.Wait(ctx.Done(), ch, poll.Next()) {
				return
			}
		}
	}
}

func memoryCursor(gen uint64, n int) Cursor {
	return Cursor(strconv.FormatUint(gen, 10) + ":" + strconv.Itoa(n))
}

// memoryStart reads the session a follow begins with: the whole of it
// for the zero cursor or one that names no place in this session, and
// its first n entries for one that does, which resumed reports. It
// returns how many entries the session holds.
func memoryStart(live *Session, gen uint64, from Cursor) (sess *Session, n int, resumed bool, kind ChangeKind, err error) {
	var buf bytes.Buffer
	if err := Write(&buf, live); err != nil {
		return nil, 0, false, 0, err
	}
	lines := bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte{'\n'}) // header, then the entries
	kind, take := Snapshot, len(lines)
	if from != "" {
		g, c, ok := strings.Cut(string(from), ":")
		gn, err1 := strconv.ParseUint(g, 10, 64)
		nn, err2 := strconv.Atoi(c)
		if ok && err1 == nil && err2 == nil && gn == gen && nn >= 0 && nn < len(lines) {
			take, resumed = nn+1, true
		} else {
			kind = Reset
		}
	}
	sess, err = Read(bytes.NewReader(bytes.Join(lines[:take], []byte{'\n'})))
	if err != nil {
		return nil, 0, false, 0, err
	}
	return sess, take - 1, resumed, kind, nil
}
