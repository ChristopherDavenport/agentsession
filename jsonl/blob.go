package jsonl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
)

// mediaDir is where the store keeps the sidecar blobs of its sessions,
// under the root, each named by its hex digest as RFC 0001 names a
// sidecar file. A blob is held once for every session that names it, as
// RFC 0002's Media section has a store hold one. A project directory of
// the same name is harmless: a blob's name has no ".jsonl".
const mediaDir = "media"

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.root, mediaDir, strings.TrimPrefix(hash, agentsession.HashPrefix))
}

// PutBlob implements [agentsession.BlobWriter]: it writes the blob to
// the store's media directory under its hash, durably, through a
// temporary file linked into place, so a crash leaves the blob whole or
// not there. A blob the store holds is left as it is.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	hash := agentsession.HashBytes(data)
	path := s.blobPath(hash)
	if _, err := os.Stat(path); err == nil {
		return hash, nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("jsonl: blob: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", fmt.Errorf("jsonl: blob: %w", err)
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(data)
	if err == nil {
		err = syncEntry(tmp)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if err = os.Link(tmp.Name(), path); errors.Is(err, os.ErrExist) {
			err = nil
		}
	}
	if err != nil {
		return "", fmt.Errorf("jsonl: blob %s: %w", hash, err)
	}
	syncDir(dir)
	return hash, nil
}

// Blob implements [agentsession.BlobReader].
func (s *Store) Blob(ctx context.Context, hash string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !agentsession.ValidHash(hash) {
		return nil, fmt.Errorf("%w: %q", agentsession.ErrNoBlob, hash)
	}
	data, err := os.ReadFile(s.blobPath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", agentsession.ErrNoBlob, hash)
	}
	if err != nil {
		return nil, fmt.Errorf("jsonl: blob %s: %w", hash, err)
	}
	if agentsession.HashBytes(data) != hash {
		return nil, fmt.Errorf("jsonl: blob %s: its bytes do not hash to its name", hash)
	}
	return data, nil
}

var (
	_ agentsession.BlobWriter = (*Store)(nil)
	_ agentsession.BlobReader = (*Store)(nil)
)
