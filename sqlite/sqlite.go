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
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/procs"
	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

const schema = `
CREATE TABLE IF NOT EXISTS sessions (
	id             TEXT PRIMARY KEY,
	created_at     TEXT NOT NULL,
	updated_at     TEXT NOT NULL,
	cwd            TEXT NOT NULL DEFAULT '',
	parent_session TEXT NOT NULL DEFAULT '',
	name           TEXT NOT NULL DEFAULT '',
	superseded_by  TEXT NOT NULL DEFAULT '',
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
CREATE TABLE IF NOT EXISTS holders (
	session_id TEXT    PRIMARY KEY REFERENCES sessions(id) ON DELETE CASCADE,
	pid        INTEGER NOT NULL,
	host       TEXT    NOT NULL,
	token      TEXT    NOT NULL,
	since      TEXT    NOT NULL,
	heartbeat  TEXT    NOT NULL
) STRICT;
`

// ErrSessionLocked is returned by Create, Open and Append when another
// process holds the session. The message names the holder, with the
// times it has held it since and last appended; BreakLock removes a
// lock the caller has decided is dead.
//
// It is [agentsession.ErrSessionLocked], so a host that reads the
// store through the interface matches the same sentinel whichever
// store it was given.
var ErrSessionLocked = agentsession.ErrSessionLocked

// LockInfo describes the holder of a session.
type LockInfo struct {
	PID   int
	Host  string
	Since time.Time
	// Heartbeat is when the holder last appended.
	Heartbeat time.Time
}

// Option configures a Store.
type Option func(*Store)

// WithReadOnly opens the store for reading: Open takes no hold on a
// session, so a session another process holds can be read while it is
// held, and Create, Append and Delete return
// [agentsession.ErrReadOnly]. Opening the database still creates or
// migrates its tables, which is what makes a file an earlier release
// wrote readable; nothing in a session is written.
func WithReadOnly() Option {
	return func(s *Store) { s.readOnly = true }
}

// WithStaleLockReport sets a function the store calls when Create or
// Open takes over the hold of a process on this host that no longer
// runs, with the dead holder's details, as the jsonl store's option
// of the same name does. The takeover itself is not changed.
func WithStaleLockReport(report func(LockInfo)) Option {
	return func(s *Store) { s.staleReport = report }
}

// ErrConcurrentWriter is returned by Append when another process
// appended to the session since this store loaded it. The store has no
// cross-process lock, so the conflict is found at the write: the
// entries table's (session_id, seq) key refuses the line. The session
// then stays refused in this store until Release reloads it, so a
// recorder that continued from a transcript the other writer never
// saw fails loudly rather than re-parenting its entries under a leaf
// its agent never received.
var ErrConcurrentWriter = errors.New("sqlite: another process appended to the session")

// Store is a SQLite-backed [agentsession.Store]. It is safe for
// concurrent use within one process. A store opened with
// [WithReadOnly] takes no hold and refuses every write, so a session
// another process is writing can still be read. Several processes may
// share the file: every write is one immediate transaction, and each
// open session is held by one process at a time through the holders
// table, so a second process that opens the same session gets
// ErrSessionLocked instead of interleaving entries with the first.
// The hold is released by Release, Delete and Close; a hold left by a
// process on this host that no longer runs is taken over on the next
// open, and BreakLock clears one from any other holder. Every append
// refreshes the holder's heartbeat and fails with ErrSessionLocked if
// the hold was taken from under it.
type Store struct {
	w, r        *sql.DB
	staleReport func(LockInfo)
	readOnly    bool
	pid         int
	host        string
	// token identifies this Store among holders, since two stores in
	// one process share a pid.
	token string
	// refused holds the sessions whose last append found another
	// writer, so later appends fail until the caller reloads.
	refused map[string]error

	mu   sync.Mutex
	open map[string]*agentsession.Session
}

// Open opens or creates the database at path and applies the schema.
// Every connection runs in WAL mode with foreign keys on and a busy
// timeout, and writes go through a single-connection pool so writers
// queue instead of failing.
func Open(path string, opts ...Option) (*Store, error) {
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
	if err := migrate(w); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	host, _ := os.Hostname()
	s := &Store{w: w, r: r, open: map[string]*agentsession.Session{}, refused: map[string]error{}, pid: os.Getpid(), host: host, token: newToken()}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// migrate brings a database created by an earlier release up to the
// current schema. CREATE TABLE IF NOT EXISTS leaves an existing table
// alone, so columns added later are added here and filled from the
// stored entries.
func migrate(w *sql.DB) error {
	cols, err := columns(w, "sessions")
	if err != nil {
		return err
	}
	if cols["name"] && cols["superseded_by"] {
		return nil
	}
	tx, err := w.Begin()
	if err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	defer tx.Rollback()
	if !cols["name"] {
		if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN name TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("sqlite: migrate: add name: %w", err)
		}
	}
	if !cols["superseded_by"] {
		if _, err := tx.Exec(`ALTER TABLE sessions ADD COLUMN superseded_by TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("sqlite: migrate: add superseded_by: %w", err)
		}
		links, err := tx.Query(`SELECT session_id, line FROM entries WHERE type = ? ORDER BY session_id, seq`, agentsession.TypeLink)
		if err != nil {
			return fmt.Errorf("sqlite: migrate: read link entries: %w", err)
		}
		next := map[string]string{}
		for links.Next() {
			var id, line string
			if err := links.Scan(&id, &line); err != nil {
				links.Close()
				return fmt.Errorf("sqlite: migrate: %w", err)
			}
			if s := continuedIn(line); s != "" {
				next[id] = s
			}
		}
		links.Close()
		if err := links.Err(); err != nil {
			return fmt.Errorf("sqlite: migrate: %w", err)
		}
		for id, s := range next {
			if _, err := tx.Exec(`UPDATE sessions SET superseded_by = ? WHERE id = ?`, s, id); err != nil {
				return fmt.Errorf("sqlite: migrate: set superseded_by: %w", err)
			}
		}
	}
	if cols["name"] {
		return tx.Commit()
	}
	rows, err := tx.Query(`SELECT session_id, line FROM entries WHERE type = ? ORDER BY session_id, seq`, agentsession.TypeInfo)
	if err != nil {
		return fmt.Errorf("sqlite: migrate: read info entries: %w", err)
	}
	names := map[string]string{}
	for rows.Next() {
		var id, line string
		if err := rows.Scan(&id, &line); err != nil {
			rows.Close()
			return fmt.Errorf("sqlite: migrate: %w", err)
		}
		if name := infoName(line); name != "" {
			names[id] = name
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	for id, name := range names {
		if _, err := tx.Exec(`UPDATE sessions SET name = ? WHERE id = ?`, name, id); err != nil {
			return fmt.Errorf("sqlite: migrate: set name: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: migrate: %w", err)
	}
	return nil
}

// continuedIn returns the successor a stored link line names, or "".
func continuedIn(line string) string {
	var probe struct {
		Rel     string `json:"rel"`
		Session string `json:"session"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil || probe.Rel != agentsession.RelContinuedIn {
		return ""
	}
	return probe.Session
}

// columns returns the column names of a table.
func columns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("sqlite: columns of %s: %w", table, err)
		}
		out[name] = true
	}
	return out, rows.Err()
}

// infoName returns the name an info entry line sets, or "".
func infoName(line string) string {
	var probe struct {
		Name string `json:"name"`
	}
	if json.Unmarshal([]byte(line), &probe) != nil {
		return ""
	}
	return probe.Name
}

// Close runs PRAGMA optimize and closes both pools.
func (s *Store) Close() error {
	s.mu.Lock()
	for id := range s.open {
		s.releaseLocked(id)
	}
	s.mu.Unlock()
	if !s.readOnly {
		_, _ = s.w.Exec("PRAGMA optimize")
	}
	err := s.w.Close()
	if rerr := s.r.Close(); err == nil {
		err = rerr
	}
	s.mu.Lock()
	s.open = map[string]*agentsession.Session{}
	s.refused = map[string]error{}
	s.mu.Unlock()
	return err
}

// Create implements agentsession.Store.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if s.readOnly {
		return nil, agentsession.ErrReadOnly
	}
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
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO sessions (id, created_at, updated_at, cwd, parent_session, header) VALUES (?, ?, ?, ?, ?, ?)`,
		h.ID, stamp(h.CreatedAt), now, h.CWD, h.ParentSession, string(line))
	if err != nil {
		if isConstraint(err) {
			return nil, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, h.ID)
		}
		return nil, fmt.Errorf("sqlite: insert session: %w", err)
	}
	if err := s.claim(ctx, tx, h.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("sqlite: create session: %w", err)
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
	if err, ok := s.refused[id]; ok {
		return nil, err
	}
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
	if !s.readOnly {
		tx, err := s.w.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("sqlite: begin: %w", err)
		}
		if err := s.claim(ctx, tx, id); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("sqlite: hold session: %w", err)
		}
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
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
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
	res, err := tx.ExecContext(ctx, `UPDATE holders SET heartbeat = ? WHERE session_id = ? AND token = ?`,
		stamp(time.Now()), sessionID, s.token)
	if err == nil {
		if n, _ := res.RowsAffected(); n == 0 {
			delete(s.open, sessionID)
			holder, _ := s.holder(ctx, sessionID)
			s.refused[sessionID] = fmt.Errorf("%w: hold on %s lost to %s", ErrSessionLocked, sessionID, describeHolder(holder))
			return "", s.refused[sessionID]
		}
	}
	if err == nil {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO entries (session_id, seq, id, parent, type, line) VALUES (?, ?, ?, ?, ?, ?)`,
			sessionID, sess.Len(), id, parent, e.EntryType(), string(line))
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET updated_at = ? WHERE id = ?`, stamp(time.Now()), sessionID)
	}
	if info, ok := e.(*agentsession.InfoEntry); ok && info.Name != "" && err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET name = ? WHERE id = ?`, info.Name, sessionID)
	}
	if l, ok := e.(*agentsession.LinkEntry); ok && l.Rel == agentsession.RelContinuedIn && l.Session != "" && err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE sessions SET superseded_by = ? WHERE id = ?`, l.Session, sessionID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		delete(s.open, sessionID)
		if isSeqConflict(err) {
			s.refused[sessionID] = fmt.Errorf("%w: entry %s at seq %d", ErrConcurrentWriter, id, sess.Len())
			return "", s.refused[sessionID]
		}
		return "", fmt.Errorf("sqlite: insert entry %s: %w", id, err)
	}
	return id, nil
}

// isSeqConflict reports whether an insert failed on the entries
// table's (session_id, seq) key, which is what another writer's
// append looks like.
func isSeqConflict(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") && strings.Contains(msg, "entries.seq")
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
		if f.Current {
			where = append(where, "superseded_by = ''")
		}
		q := `SELECT s.header, s.updated_at, s.name, s.superseded_by,
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
			var header, updated, name, supersededBy string
			var size int64
			if err := rows.Scan(&header, &updated, &name, &supersededBy, &size); err != nil {
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
			if !yield(agentsession.Summary{Header: h, Name: name, SupersededBy: supersededBy, Size: size, Modified: mod}, nil) {
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
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
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
	s.releaseLocked(id)
}

func (s *Store) releaseLocked(id string) {
	if _, ok := s.open[id]; ok && !s.readOnly {
		_, _ = s.w.Exec(`DELETE FROM holders WHERE session_id = ? AND token = ?`, id, s.token)
	}
	delete(s.open, id)
	delete(s.refused, id)
}

// LockHolder reports who holds a session, or nil when it is free. A
// session this store holds is reported as held by this process.
func (s *Store) LockHolder(ctx context.Context, id string) (*LockInfo, error) {
	return s.holder(ctx, id)
}

// BreakLock removes a session's hold whoever has it. Use it when Open
// reports ErrSessionLocked and the caller has confirmed the holder is
// gone, for example a process on another host that crashed. Breaking
// the hold of a live writer makes that writer's next append fail with
// ErrSessionLocked rather than interleave with the new holder's.
func (s *Store) BreakLock(ctx context.Context, id string) error {
	if _, err := s.w.ExecContext(ctx, `DELETE FROM holders WHERE session_id = ?`, id); err != nil {
		return fmt.Errorf("sqlite: break lock: %w", err)
	}
	return nil
}

// holder reads the holders row for a session.
func (s *Store) holder(ctx context.Context, id string) (*LockInfo, error) {
	var info LockInfo
	var since, beat string
	err := s.r.QueryRowContext(ctx, `SELECT pid, host, since, heartbeat FROM holders WHERE session_id = ?`, id).Scan(&info.PID, &info.Host, &since, &beat)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: read holder: %w", err)
	}
	info.Since, _ = time.Parse(time.RFC3339Nano, since)
	info.Heartbeat, _ = time.Parse(time.RFC3339Nano, beat)
	return &info, nil
}

// claim takes the hold on a session inside tx: a holder on this host
// whose process no longer runs is taken over and reported; any other
// holder produces ErrSessionLocked.
func (s *Store) claim(ctx context.Context, tx *sql.Tx, id string) error {
	var cur LockInfo
	var token, since, beat string
	err := tx.QueryRowContext(ctx, `SELECT pid, host, token, since, heartbeat FROM holders WHERE session_id = ?`, id).Scan(&cur.PID, &cur.Host, &token, &since, &beat)
	// Both times are read before the branches: the live-holder message
	// is the one an operator sees, and a zero timestamp in it reads
	// like a broken lock rather than a lock doing its job.
	cur.Since, _ = time.Parse(time.RFC3339Nano, since)
	cur.Heartbeat, _ = time.Parse(time.RFC3339Nano, beat)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return fmt.Errorf("sqlite: read holder: %w", err)
	case token == s.token:
		return nil // ours already, from an earlier open in this store
	case cur.Host == s.host && !procs.Alive(cur.PID):
		if _, err := tx.ExecContext(ctx, `DELETE FROM holders WHERE session_id = ?`, id); err != nil {
			return fmt.Errorf("sqlite: remove stale holder: %w", err)
		}
		if s.staleReport != nil {
			s.staleReport(cur)
		}
	default:
		return fmt.Errorf("%w: %s held by %s", ErrSessionLocked, id, describeHolder(&cur))
	}
	now := stamp(time.Now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO holders (session_id, pid, host, token, since, heartbeat) VALUES (?, ?, ?, ?, ?, ?)`, id, s.pid, s.host, s.token, now, now); err != nil {
		return fmt.Errorf("sqlite: hold session: %w", err)
	}
	return nil
}

// newToken returns a random identity for one Store.
func newToken() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func describeHolder(l *LockInfo) string {
	if l == nil {
		return "no holder"
	}
	out := fmt.Sprintf("pid %d on %s", l.PID, l.Host)
	if !l.Since.IsZero() {
		out += " since " + l.Since.Format(time.RFC3339)
	}
	if !l.Heartbeat.IsZero() && !l.Heartbeat.Equal(l.Since) {
		out += ", last append " + l.Heartbeat.Format(time.RFC3339)
	}
	return out
}

// stamp renders a time for lexicographic ordering in the database.
func stamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func isConstraint(err error) bool {
	return err != nil && strings.Contains(err.Error(), "constraint")
}

var _ agentsession.Store = (*Store)(nil)
