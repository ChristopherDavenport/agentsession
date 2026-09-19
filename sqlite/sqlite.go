// Package sqlite is a session store over one SQLite database file. It
// is a nested module so the driver (modernc.org/sqlite, pure Go) stays
// out of the library's dependency graph. Sessions and entries are rows;
// an entry row holds the same JSON line the jsonl store would write, so
// the two stores are interchangeable and a database can be dumped to
// session files without conversion.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id             TEXT PRIMARY KEY,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	cwd            TEXT NOT NULL DEFAULT '',
	parent_session TEXT NOT NULL DEFAULT '',
	header         TEXT NOT NULL
) STRICT;
CREATE INDEX IF NOT EXISTS sessions_created_at ON sessions(created_at);
CREATE INDEX IF NOT EXISTS sessions_cwd ON sessions(cwd, created_at);
CREATE TABLE IF NOT EXISTS entries (
	session_id TEXT    NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
	seq        INTEGER NOT NULL,
	id         TEXT    NOT NULL,
	parent     TEXT,
	type       TEXT    NOT NULL,
	line       TEXT    NOT NULL,
	UNIQUE (session_id, seq),
	UNIQUE (session_id, id)
) STRICT;
`

// Store is a SQLite-backed [agentsession.Store]. It is safe for
// concurrent use within one process; several processes may share the
// file, since every write is one immediate transaction, but a session
// open in two processes at once will diverge.
type Store struct {
	w, r *sql.DB

	mu   sync.Mutex
	open map[string]*agentsession.Session
}

// Open opens or creates the database at path and applies the schema.
// Every connection runs in WAL mode with foreign keys on and a busy
// timeout, and writes go through a single-connection pool so writers
// queue instead of failing.
func Open(path string) (*Store, error) {
	pragmas := url.Values{}
	for _, p := range []string{"journal_mode(WAL)", "synchronous(NORMAL)", "foreign_keys(ON)", "busy_timeout(5000)"} {
		pragmas.Add("_pragma", p)
	}
	dsn := "file:" + path + "?" + pragmas.Encode()
	w, err := sql.Open("sqlite", dsn+"&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("sqlite: open writer: %w", err)
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn)
	if err != nil {
		w.Close()
		return nil, fmt.Errorf("sqlite: open reader: %w", err)
	}
	r.SetMaxOpenConns(4 * runtime.NumCPU())
	if _, err := w.Exec(schema); err != nil {
		w.Close()
		r.Close()
		return nil, fmt.Errorf("sqlite: apply schema: %w", err)
	}
	return &Store{w: w, r: r, open: map[string]*agentsession.Session{}}, nil
}

// Close runs PRAGMA optimize and closes both pools.
func (s *Store) Close() error {
	_, _ = s.w.Exec("PRAGMA optimize")
	err := s.w.Close()
	if rerr := s.r.Close(); err == nil {
		err = rerr
	}
	s.mu.Lock()
	s.open = map[string]*agentsession.Session{}
	s.mu.Unlock()
	return err
}

// Create implements agentsession.Store.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	sess := agentsession.New(h)
	h = sess.Header()
	line, err := h.MarshalJSON()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.open[h.ID]; ok {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
	}
	now := stamp(time.Now())
	_, err = s.w.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at, cwd, parent_session, header) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, stamp(h.CreatedAt), now, h.CWD, h.ParentSession, string(line))
	if err != nil {
		if isConstraint(err) {
			return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
		}
		return nil, fmt.Errorf("sqlite: insert session: %w", err)
	}
	s.open[h.ID] = sess
	return sess, nil
}

// Open implements agentsession.Store. A session already open in this
// store is returned as is.
func (s *Store) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openLocked(ctx, id)
}

func (s *Store) openLocked(ctx context.Context, id string) (*agentsession.Session, error) {
	if sess, ok := s.open[id]; ok {
		return sess, nil
	}
	var header string
	err := s.r.QueryRowContext(ctx, `SELECT header FROM sessions WHERE id = ?`, id).Scan(&header)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: load session: %w", err)
	}
	// Rebuild the JSONL form and let the library validate the tree.
	var buf bytes.Buffer
	buf.WriteString(header)
	buf.WriteByte('\n')
	rows, err := s.r.QueryContext(ctx, `SELECT line FROM entries WHERE session_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, fmt.Errorf("sqlite: load entries: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, fmt.Errorf("sqlite: load entries: %w", err)
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: load entries: %w", err)
	}
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return nil, fmt.Errorf("sqlite: session %s: %w", id, err)
	}
	if sess.ID() != id {
		return nil, fmt.Errorf("sqlite: row %s holds session %s", id, sess.ID())
	}
	s.open[id] = sess
	return sess, nil
}

// Append implements agentsession.Store: the entry joins the in-memory
// tree, then its line is inserted in one immediate transaction. If the
// insert fails the cached session is dropped so the next Open reloads
// the database's view.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, err := s.openLocked(ctx, sessionID)
	if err != nil {
		return "", err
	}
	id, err := sess.Append(e)
	if err != nil {
		return "", err
	}
	line, err := agentsession.MarshalEntry(e)
	if err != nil {
		delete(s.open, sessionID)
		return "", err
	}
	b := e.Base()
	var parent any
	if b.Parent != "" {
		parent = b.Parent
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		delete(s.open, sessionID)
		return "", fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO entries (session_id, seq, id, parent, type, line) VALUES (?, ?, ?, ?, ?, ?)`,
		sessionID, sess.Len(), id, parent, e.EntryType(), string(line))
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, stamp(time.Now()), sessionID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		delete(s.open, sessionID)
		return "", fmt.Errorf("sqlite: insert entry %s: %w", id, err)
	}
	return id, nil
}

// List implements agentsession.Store.
func (s *Store) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		var (
			where []string
			args  []any
		)
		if f.CWD != "" {
			where = append(where, "cwd = ?")
			args = append(args, f.CWD)
		}
		if f.ParentSession != "" {
			where = append(where, "parent_session = ?")
			args = append(args, f.ParentSession)
		}
		if !f.After.IsZero() {
			where = append(where, "created_at > ?")
			args = append(args, stamp(f.After))
		}
		if !f.Before.IsZero() {
			where = append(where, "created_at < ?")
			args = append(args, stamp(f.Before))
		}
		q := `SELECT s.header, s.updated_at,
			length(s.header) + COALESCE((SELECT SUM(length(e.line) + 1) FROM entries e WHERE e.session_id = s.id), 0) + 1
			FROM sessions s`
		if len(where) > 0 {
			q += " WHERE " + strings.Join(where, " AND ")
		}
		q += " ORDER BY created_at DESC"
		if f.Limit > 0 {
			q += " LIMIT ?"
			args = append(args, f.Limit)
		}
		rows, err := s.r.QueryContext(ctx, q, args...)
		if err != nil {
			yield(agentsession.Summary{}, fmt.Errorf("sqlite: list: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var header, updated string
			var size int64
			if err := rows.Scan(&header, &updated, &size); err != nil {
				yield(agentsession.Summary{}, fmt.Errorf("sqlite: list: %w", err))
				return
			}
			var h agentsession.Header
			if err := h.UnmarshalJSON([]byte(header)); err != nil {
				if !yield(agentsession.Summary{}, fmt.Errorf("sqlite: list: header: %w", err)) {
					return
				}
				continue
			}
			// The stored columns filter coarsely (string comparison of
			// stamps); the header decides.
			if !f.Matches(h) {
				continue
			}
			mod, _ := time.Parse(time.RFC3339Nano, updated)
			if !yield(agentsession.Summary{Header: h, Size: size, Modified: mod}, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield(agentsession.Summary{}, fmt.Errorf("sqlite: list: %w", err))
		}
	}
}

// Delete implements agentsession.Store. Entries go with the session.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.w.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	delete(s.open, id)
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: delete session: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	return nil
}

// Release forgets an open session so the next Open reloads it from the
// database.
func (s *Store) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.open, id)
}

// stamp renders a time for lexicographic ordering in the database.
func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint")
}

var _ agentsession.Store = (*Store)(nil)
