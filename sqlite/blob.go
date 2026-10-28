package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agentsession"
)

// PutBlob implements [agentsession.BlobWriter]: it stores the blob in
// the blobs table under its hash, held once for every session that
// names it, as RFC 0002's Media section has a store hold one. A blob the
// store holds is left as it is. The write is a transaction, durable as
// the database's other commits are.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	hash := agentsession.HashBytes(data)
	if _, err := s.w.ExecContext(ctx, `INSERT INTO blobs (hash, data) VALUES (?, ?) ON CONFLICT (hash) DO NOTHING`, hash, data); err != nil {
		return "", fmt.Errorf("sqlite: blob %s: %w", hash, err)
	}
	return hash, nil
}

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
	_ agentsession.BlobWriter = (*Store)(nil)
	_ agentsession.BlobReader = (*Store)(nil)
)
