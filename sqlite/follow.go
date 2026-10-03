package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
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

// WithFollowInterval sets how often a follower looks at the database
// for a change no writer in this process rang for: the shortest wait,
// which it keeps while rows keep arriving and doubles while they do
// not, up to a second or the interval itself when that is longer. The
// default is 100 ms. A follower of a session this store appends to is
// woken by the append and does not wait for the interval.
func WithFollowInterval(d time.Duration) Option {
	return func(s *Store) {
		if d > 0 {
			s.followEvery = d
		}
	}
}

// Follow implements [agentsession.Follower]. The snapshot is what Read
// returns, from one read transaction, and the tail selects the rows
// whose seq is above the last the follower has, in order, each poll: a
// lookup of the session's header and newest seq, then the new rows when
// there are any. It takes no hold and writes nothing, so a read-only
// store follows too. An append is visible at its commit, and the
// database runs with synchronous=NORMAL, so a transaction a power loss
// takes back may have been followed.
//
// The poll is a query on the pool and not a PRAGMA data_version on a
// connection of the follower's own, which would keep one connection of
// the pool per follower and leave none for the store's reads.
//
// A seq below the cursor's, which only a rebuild of the rows makes, is
// a [agentsession.Reset]. A session deleted ends the follow with
// [agentsession.ErrNoSession], and so does one created again under its
// ID, which the stored header tells: its ID, creation stamp and members,
// but not its format, which the first append of a newer writer raises
// and which changes nothing a follower holds. The cursor names the last
// seq and that header's hash.
func (s *Store) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return follow.Run(ctx, &source{store: s, id: id}, from)
}

type source struct {
	store *Store
	id    string
}

type pos struct {
	seq   int64
	ident string
}

func (p pos) cursor() agentsession.Cursor {
	return agentsession.Cursor(strconv.FormatInt(p.seq, 10) + ":" + p.ident)
}

func parsePos(c agentsession.Cursor) (pos, bool) {
	a, b, ok := strings.Cut(string(c), ":")
	seq, err := strconv.ParseInt(a, 10, 64)
	if !ok || err != nil || seq < 0 {
		return pos{}, false
	}
	return pos{seq, b}, true
}

// identOf hashes a stored header without its format.
func identOf(header string) (string, error) {
	var h agentsession.Header
	if err := h.UnmarshalJSON([]byte(header)); err != nil {
		return "", err
	}
	h.Format = ""
	b, err := h.MarshalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), nil
}

func (src *source) gone() error {
	return fmt.Errorf("%w: %s", agentsession.ErrNoSession, src.id)
}

// Load reads the session as its rows up to the cursor's seq say.
func (src *source) Load(ctx context.Context, upTo agentsession.Cursor) (*agentsession.Session, agentsession.Cursor, error) {
	limit := int64(-1)
	var want pos
	if upTo != "" {
		var ok bool
		if want, ok = parsePos(upTo); !ok {
			return nil, "", follow.ErrStale
		}
		limit = want.seq
	}
	sess, header, last, err := src.store.loadTo(ctx, src.id, limit)
	if err != nil {
		return nil, "", err
	}
	ident, err := identOf(header)
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: session %s: header: %w", src.id, err)
	}
	if upTo != "" && (ident != want.ident || last != want.seq) {
		return nil, "", follow.ErrStale
	}
	return sess, pos{last, ident}.cursor(), nil
}

// Tail selects the rows beyond the cursor's seq.
func (src *source) Tail(ctx context.Context, cur agentsession.Cursor) ([]follow.Item, error) {
	p, ok := parsePos(cur)
	if !ok {
		return nil, follow.ErrStale
	}
	tx, err := src.store.r.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	var header string
	err = tx.QueryRowContext(ctx, `SELECT header FROM sessions WHERE id = ?`, src.id).Scan(&header)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, src.gone()
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: follow %s: %w", src.id, err)
	}
	ident, err := identOf(header)
	if err != nil {
		return nil, fmt.Errorf("sqlite: session %s: header: %w", src.id, err)
	}
	if ident != p.ident {
		return nil, src.gone() // another session under the ID
	}
	var newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM entries WHERE session_id = ?`, src.id).Scan(&newest); err != nil {
		return nil, fmt.Errorf("sqlite: follow %s: %w", src.id, err)
	}
	if newest.Int64 < p.seq {
		return nil, follow.ErrStale
	}
	if newest.Int64 == p.seq {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, line FROM entries WHERE session_id = ? AND seq > ? ORDER BY seq`, src.id, p.seq)
	if err != nil {
		return nil, fmt.Errorf("sqlite: follow %s: %w", src.id, err)
	}
	defer rows.Close()
	var items []follow.Item
	for rows.Next() {
		var seq int64
		var line string
		if err := rows.Scan(&seq, &line); err != nil {
			return nil, fmt.Errorf("sqlite: follow %s: %w", src.id, err)
		}
		e, err := agentsession.UnmarshalEntry([]byte(line))
		if err != nil {
			return nil, follow.ErrStale // for Read to judge
		}
		items = append(items, follow.Item{Entry: e, Cursor: pos{seq, ident}.cursor()})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: follow %s: %w", src.id, err)
	}
	return items, nil
}

func (src *source) Watch() (func() <-chan struct{}, func()) { return src.store.hub.Watch(src.id) }

// FileLeaf marks the source as one whose sessions agentsession.Read
// builds, so the follower places the leaf as Read does.
func (src *source) FileLeaf() {}

func (src *source) Interval() (time.Duration, time.Duration) {
	lo := src.store.followEvery
	if lo <= 0 {
		lo = defaultFollowInterval
	}
	return lo, max(lo, maxFollowInterval)
}
