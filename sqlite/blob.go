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

// PutBlob implements [agentsession.BlobWriter]: it stores the blob in
// the blobs table under its hash, held once for every session that
// names it, as RFC 0002's Media section has a store hold one. A blob the
// store holds keeps its bytes, and its grace for [Store.SweepBlobs]
// starts again. The write is a transaction, durable as the database's
// other commits are.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	hash := agentsession.HashBytes(data)
	if _, err := s.w.ExecContext(ctx, `INSERT INTO blobs (hash, data, stored) VALUES (?, ?, ?) ON CONFLICT (hash) DO UPDATE SET stored = excluded.stored`, hash, data, time.Now().UnixNano()); err != nil {
		return "", fmt.Errorf("sqlite: blob %s: %w", hash, err)
	}
	return hash, nil
}

// SweepBlobs implements [agentsession.BlobSweeper]: it removes each blob
// stored longer than grace ago that no session's entries name, in one
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
	var data []byte
	err := s.r.QueryRowContext(ctx, `SELECT data FROM blobs WHERE hash = ?`, hash).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoBlob, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: blob %s: %w", hash, err)
	}
	return data, nil
}

var (
	_ agentsession.BlobWriter  = (*Store)(nil)
	_ agentsession.BlobReader  = (*Store)(nil)
	_ agentsession.BlobSweeper = (*Store)(nil)
)
