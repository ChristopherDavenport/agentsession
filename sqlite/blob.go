package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// PutBlob implements [agentsession.BlobWriter]: it is PutTypedBlob
// with no media type.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	return s.PutTypedBlob(ctx, data, "")
}

// PutTypedBlob implements [agentsession.TypedBlobWriter]: it stores the
// blob in the blobs table under its hash, held once for every session
// that names it, as RFC 0002's Media section has a store hold one, with
// its media type in the row's media_type, ” for none. A blob the store
// holds keeps its bytes and a type it has, takes the type given when it
// has none, and its grace for [Store.SweepBlobs] starts again. The
// write is a transaction, durable as the database's other commits are.
func (s *Store) PutTypedBlob(ctx context.Context, data []byte, mediaType string) (string, error) {
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	mt, err := agentsession.BlobMediaType(mediaType)
	if err != nil {
		return "", err
	}
	hash := agentsession.HashBytes(data)
	if _, err := s.w.ExecContext(ctx, `INSERT INTO blobs (hash, data, stored, media_type) VALUES (?, ?, ?, ?)
ON CONFLICT (hash) DO UPDATE SET stored = excluded.stored,
	media_type = CASE WHEN blobs.media_type = '' THEN excluded.media_type ELSE blobs.media_type END`, hash, data, time.Now().UnixNano(), mt); err != nil {
		return "", fmt.Errorf("sqlite: blob %s: %w", hash, err)
	}
	return hash, nil
}

// SweepBlobs implements [agentsession.BlobSweeper]: it removes each blob
// stored longer than grace ago that no session's entries name, its type
// with it, since the type is a column of its row, in one
// write transaction, so no write names a blob between the sweep's
// reading the entries and its removing one.
func (s *Store) SweepBlobs(ctx context.Context, grace time.Duration) (int, error) {
	if s.readOnly {
		return 0, agentsession.ErrReadOnly
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	var old []string
	rows, err := tx.QueryContext(ctx, `SELECT hash FROM blobs WHERE stored < ?`, time.Now().Add(-grace).UnixNano())
	if err != nil {
		return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
	}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
		}
		old = append(old, h)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
	}
	if len(old) == 0 {
		return 0, nil
	}
	named := map[string]bool{}
	rows, err = tx.QueryContext(ctx, `SELECT line FROM entries WHERE instr(line, 'sidecar:') > 0`)
	if err != nil {
		return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
	}
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
		}
		for _, m := range sidecarRef.FindAllString(line, -1) {
			named[strings.TrimPrefix(m, "sidecar:")] = true
		}
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
	}
	n := 0
	for _, h := range old {
		if named[h] {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE hash = ?`, h); err != nil {
			return 0, fmt.Errorf("sqlite: sweep blob %s: %w", h, err)
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("sqlite: sweep blobs: %w", err)
	}
	return n, nil
}

// sidecarRef matches a sidecar reference in an entry's line, the URL
// RFC 0001 gives an item that names a blob beside the file.
var sidecarRef = regexp.MustCompile(`sidecar:sha256:[0-9a-f]{64}`)

// Blob implements [agentsession.BlobReader].
func (s *Store) Blob(ctx context.Context, hash string) ([]byte, error) {
	data, _, err := s.TypedBlob(ctx, hash)
	return data, err
}

// TypedBlob implements [agentsession.TypedBlobReader].
func (s *Store) TypedBlob(ctx context.Context, hash string) ([]byte, string, error) {
	var data []byte
	var mt string
	err := s.r.QueryRowContext(ctx, `SELECT data, media_type FROM blobs WHERE hash = ?`, hash).Scan(&data, &mt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", fmt.Errorf("%w: %s", agentsession.ErrNoBlob, hash)
	}
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: blob %s: %w", hash, err)
	}
	if mt, err = agentsession.BlobMediaType(mt); err != nil {
		return nil, "", fmt.Errorf("sqlite: blob %s: type: %w", hash, err)
	}
	return data, mt, nil
}

// SetSessionBlobTypes implements [agentsession.SessionBlobTyper]: each
// type is a row of session_blob_types, inserted unless the session holds
// one for the blob, and deleted with the session.
func (s *Store) SetSessionBlobTypes(ctx context.Context, session string, types map[string]string) error {
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	set, err := agentsession.CheckBlobTypes(types)
	if err != nil {
		return err
	}
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: begin: %w", err)
	}
	defer tx.Rollback()
	var one int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, session).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", agentsession.ErrNoSession, session)
	} else if err != nil {
		return fmt.Errorf("sqlite: session %s: %w", session, err)
	}
	for h, t := range set {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_blob_types (session_id, hash, media_type) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, session, h, t); err != nil {
			return fmt.Errorf("sqlite: session %s: blob %s: type: %w", session, h, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: session %s: types: %w", session, err)
	}
	return nil
}

// SessionBlobTypes implements [agentsession.SessionBlobTyper].
func (s *Store) SessionBlobTypes(ctx context.Context, session string) (map[string]string, error) {
	var one int
	if err := s.r.QueryRowContext(ctx, `SELECT 1 FROM sessions WHERE id = ?`, session).Scan(&one); errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoSession, session)
	} else if err != nil {
		return nil, fmt.Errorf("sqlite: session %s: %w", session, err)
	}
	rows, err := s.r.QueryContext(ctx, `SELECT hash, media_type FROM session_blob_types WHERE session_id = ?`, session)
	if err != nil {
		return nil, fmt.Errorf("sqlite: session %s: types: %w", session, err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var h, t string
		if err := rows.Scan(&h, &t); err != nil {
			return nil, fmt.Errorf("sqlite: session %s: types: %w", session, err)
		}
		mt, err := agentsession.BlobMediaType(t)
		if err != nil {
			return nil, fmt.Errorf("sqlite: session %s: blob %s: type: %w", session, h, err)
		}
		out[h] = mt
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: session %s: types: %w", session, err)
	}
	return out, nil
}

// SessionBlob implements [agentsession.SessionBlobReader]: the blob,
// with the session's type for it, or the store's where the session
// holds none.
func (s *Store) SessionBlob(ctx context.Context, session, hash string) ([]byte, string, error) {
	data, mt, err := s.TypedBlob(ctx, hash)
	if err != nil {
		return nil, "", err
	}
	var t string
	err = s.r.QueryRowContext(ctx, `SELECT media_type FROM session_blob_types WHERE session_id = ? AND hash = ?`, session, hash).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return data, mt, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sqlite: session %s: blob %s: type: %w", session, hash, err)
	}
	if mt, err = agentsession.BlobMediaType(t); err != nil {
		return nil, "", fmt.Errorf("sqlite: session %s: blob %s: type: %w", session, hash, err)
	}
	return data, mt, nil
}

var (
	_ agentsession.BlobWriter      = (*Store)(nil)
	_ agentsession.BlobReader      = (*Store)(nil)
	_ agentsession.TypedBlobWriter = (*Store)(nil)
	_ agentsession.TypedBlobReader = (*Store)(nil)

	_ agentsession.SessionBlobTyper  = (*Store)(nil)
	_ agentsession.SessionBlobReader = (*Store)(nil)
	_ agentsession.BlobSweeper       = (*Store)(nil)
)
