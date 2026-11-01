package jsonl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// not there. A blob the store holds is left as it is, its grace for
// [Store.SweepBlobs] started again.
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
		// Stored again: its grace for a sweep starts again, under the
		// guard a sweep removes by, which may have taken it since.
		if err := s.freshenBlobs([]string{hash}); err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err == nil {
			return hash, nil
		}
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("jsonl: blob: %w", err)
	}
	tmp, err := os.CreateTemp(dir, blobTmp+"*")
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

// blobTmp prefixes the temporary file a blob is written through before
// it is linked into place.
const blobTmp = ".tmp-"

// mediaGuard is the path whose directory's guard serializes freshening
// a blob with a sweep's removing it, across the processes that share
// the root.
func (s *Store) mediaGuard() string { return filepath.Join(s.root, mediaDir, "guard") }

// freshenBlobs restarts each held blob's grace for a sweep, under the
// media guard: a sweep that found it old looks again under the guard
// before it removes it, so a blob freshened before that look stays, and
// one removed before the freshening is simply not held. A blob the
// store does not hold is passed over.
func (s *Store) freshenBlobs(hashes []string) error {
	if len(hashes) == 0 {
		return nil
	}
	if _, err := os.Stat(filepath.Join(s.root, mediaDir)); err != nil {
		return nil // no blobs held
	}
	return guarded(s.mediaGuard(), func() error {
		now := time.Now()
		for _, h := range hashes {
			if !agentsession.ValidHash(h) {
				continue
			}
			if err := os.Chtimes(s.blobPath(h), now, now); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("jsonl: blob %s: %w", h, err)
			}
		}
		return nil
	})
}

// SweepBlobs implements [agentsession.BlobSweeper]: it removes each blob
// in the media directory older than grace, by the time it was stored,
// stored again or named by an append, that no session names, and the
// temporary files of blob writes a crash left, once older than grace.
// It reads every session the store holds, and removes nothing when one
// cannot be read, since that one might name a blob. A blob it found old
// is looked at again under the guard an append freshens by before it is
// removed, so an append naming it meanwhile keeps it.
func (s *Store) SweepBlobs(ctx context.Context, grace time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.readOnly {
		return 0, agentsession.ErrReadOnly
	}
	dir := filepath.Join(s.root, mediaDir)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("jsonl: media: %w", err)
	}
	cutoff := time.Now().Add(-grace)
	old := func(name string) bool {
		info, err := os.Stat(filepath.Join(dir, name))
		return err == nil && info.ModTime().Before(cutoff)
	}
	var candidates []string
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, blobTmp):
			if old(name) {
				os.Remove(filepath.Join(dir, name))
			}
		case agentsession.ValidHash(agentsession.HashPrefix+name) && old(name):
			candidates = append(candidates, name)
		}
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	named := map[string]bool{}
	for sum, err := range s.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			return 0, fmt.Errorf("jsonl: sweep blobs: %w", err)
		}
		sess, err := s.Read(ctx, sum.Header.ID)
		if errors.Is(err, agentsession.ErrNoSession) {
			continue // deleted since the listing
		}
		if err != nil {
			return 0, fmt.Errorf("jsonl: sweep blobs: session %s: %w", sum.Header.ID, err)
		}
		bs, err := agentsession.SessionBlobs(sess)
		if err != nil {
			return 0, err
		}
		for _, b := range bs {
			named[strings.TrimPrefix(b, agentsession.HashPrefix)] = true
		}
	}
	n := 0
	err = guarded(s.mediaGuard(), func() error {
		for _, name := range candidates {
			if named[name] || !old(name) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("jsonl: blob %s: %w", name, err)
			}
			n++
		}
		return nil
	})
	if n > 0 {
		syncDir(dir)
	}
	return n, err
}

var (
	_ agentsession.BlobWriter  = (*Store)(nil)
	_ agentsession.BlobReader  = (*Store)(nil)
	_ agentsession.BlobSweeper = (*Store)(nil)
)
