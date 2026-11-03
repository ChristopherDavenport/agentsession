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
// sidecar file, and its media type beside it in the file
// [agentsession.SidecarTypeName] names, as RFC 0001 names that. A blob
// is held once for every session that names it, as RFC 0002's Media
// section has a store hold one. A project directory of the same name is
// harmless: a blob's name has no ".jsonl".
const mediaDir = "media"

func (s *Store) blobPath(hash string) string {
	return filepath.Join(s.root, mediaDir, strings.TrimPrefix(hash, agentsession.HashPrefix))
}

func (s *Store) typePath(hash string) string {
	return filepath.Join(s.root, mediaDir, agentsession.SidecarTypeName(hash))
}

// typeSuffix ends the name of a type file, after the blob's hex digest.
const typeSuffix = ".type"

// PutBlob implements [agentsession.BlobWriter]: it is PutTypedBlob with
// no media type.
func (s *Store) PutBlob(ctx context.Context, data []byte) (string, error) {
	return s.PutTypedBlob(ctx, data, "")
}

// PutTypedBlob implements [agentsession.TypedBlobWriter]: it writes the
// blob to the store's media directory under its hash, durably, through
// a temporary file linked into place, so a crash leaves the blob whole
// or not there, and then its media type beside it the same way. A blob
// the store holds is left as it is, its grace for [Store.SweepBlobs]
// started again, and a type it holds is kept: the link that places a
// type never replaces one. A crash between the two leaves the blob with
// no type, which storing it again with its type gives it.
func (s *Store) PutTypedBlob(ctx context.Context, data []byte, mediaType string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if s.readOnly {
		return "", agentsession.ErrReadOnly
	}
	mt, err := agentsession.BlobMediaType(mediaType)
	if err != nil {
		return "", err
	}
	hash, err := s.putBlob(data)
	if err != nil || mt == "" {
		return hash, err
	}
	if err := s.putType(hash, mt); err != nil {
		return "", err
	}
	return hash, nil
}

// putType gives a held blob its media type, unless it has one, under
// the media guard a sweep removes a blob and its type by, so a sweep
// never takes the blob from under its new type nor the type from under
// a blob it keeps.
func (s *Store) putType(hash, mediaType string) error {
	path := s.typePath(hash)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return guarded(s.mediaGuard(), func() error {
		if _, err := os.Stat(s.blobPath(hash)); err != nil {
			return fmt.Errorf("jsonl: blob %s: %w", hash, err)
		}
		if err := linkNew(path, []byte(mediaType)); err != nil {
			return fmt.Errorf("jsonl: blob %s: type: %w", hash, err)
		}
		return nil
	})
}

// linkNew writes data to path durably, through a temporary file linked
// into place, and leaves a file already at path as it is.
func linkNew(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, blobTmp+"*")
	if err != nil {
		return err
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
		return err
	}
	syncDir(dir)
	return nil
}

// putBlob is PutBlob's write of the bytes.
func (s *Store) putBlob(data []byte) (string, error) {
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("jsonl: blob: %w", err)
	}
	if err := linkNew(path, data); err != nil {
		return "", fmt.Errorf("jsonl: blob %s: %w", hash, err)
	}
	return hash, nil
}

// TypedBlob implements [agentsession.TypedBlobReader]: the blob, as
// Blob reads it, and the type beside it, "" when it has none.
func (s *Store) TypedBlob(ctx context.Context, hash string) ([]byte, string, error) {
	data, err := s.Blob(ctx, hash)
	if err != nil {
		return nil, "", err
	}
	raw, err := os.ReadFile(s.typePath(hash))
	if errors.Is(err, os.ErrNotExist) {
		return data, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: blob %s: type: %w", hash, err)
	}
	mt, err := agentsession.BlobMediaType(string(raw))
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: blob %s: type: %w", hash, err)
	}
	return data, mt, nil
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
// stored again or named by an append, that no session names, with its
// type, the type first, so a crash between the two leaves no type
// without its blob; the type of a blob not held, once older than grace;
// and the temporary files of blob writes a crash left, once older than
// grace.
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
	var candidates, types []string
	for _, e := range ents {
		name := e.Name()
		switch {
		case strings.HasPrefix(name, blobTmp):
			if old(name) {
				os.Remove(filepath.Join(dir, name))
			}
		case agentsession.ValidHash(agentsession.HashPrefix+name) && old(name):
			candidates = append(candidates, name)
		case strings.HasSuffix(name, typeSuffix) && agentsession.ValidHash(agentsession.HashPrefix+strings.TrimSuffix(name, typeSuffix)):
			types = append(types, name)
		}
	}
	if err := s.sweepTypes(dir, types, old); err != nil {
		return 0, err
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
			if err := os.Remove(filepath.Join(dir, name+typeSuffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("jsonl: blob %s: type: %w", name, err)
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

// sessionTypesDir is the directory holding a session's types for its
// blobs, each in the file [agentsession.SidecarTypeName] names, as RFC
// 0001 lays out a session's directory: under the media directory, apart
// from the store's own type for a blob, and removed with the session.
func (s *Store) sessionTypesDir(id string) string {
	return filepath.Join(s.root, mediaDir, "sessions", id)
}

// SetSessionBlobTypes implements [agentsession.SessionBlobTyper]: each
// type is a file of the session's types directory, placed by a link that
// never replaces one, so the session's first type for a blob is kept.
func (s *Store) SetSessionBlobTypes(ctx context.Context, session string, types map[string]string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	set, err := agentsession.CheckBlobTypes(types)
	if err != nil {
		return err
	}
	if !validSessionID(session) {
		return fmt.Errorf("%w: session %q", ErrBadName, session)
	}
	if _, err := s.Path(session); err != nil {
		return err
	}
	if len(set) == 0 {
		return nil
	}
	dir := s.sessionTypesDir(session)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("jsonl: session %s: types: %w", session, err)
	}
	for h, t := range set {
		if err := linkNew(filepath.Join(dir, agentsession.SidecarTypeName(h)), []byte(t)); err != nil {
			return fmt.Errorf("jsonl: session %s: blob %s: type: %w", session, h, err)
		}
	}
	syncDir(filepath.Dir(dir))
	return nil
}

// SessionBlobTypes implements [agentsession.SessionBlobTyper].
func (s *Store) SessionBlobTypes(ctx context.Context, session string) (map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := s.Path(session); err != nil {
		return nil, err
	}
	out := map[string]string{}
	if !validSessionID(session) {
		return out, nil
	}
	dir := s.sessionTypesDir(session)
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jsonl: session %s: types: %w", session, err)
	}
	for _, e := range ents {
		hex, ok := strings.CutSuffix(e.Name(), typeSuffix)
		if !ok || !agentsession.ValidHash(agentsession.HashPrefix+hex) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("jsonl: session %s: types: %w", session, err)
		}
		mt, err := agentsession.BlobMediaType(string(raw))
		if err != nil {
			return nil, fmt.Errorf("jsonl: session %s: blob %s: type: %w", session, hex, err)
		}
		if mt != "" {
			out[agentsession.HashPrefix+hex] = mt
		}
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
	if !validSessionID(session) {
		return data, mt, nil
	}
	raw, err := os.ReadFile(filepath.Join(s.sessionTypesDir(session), agentsession.SidecarTypeName(hash)))
	if errors.Is(err, os.ErrNotExist) {
		return data, mt, nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("jsonl: session %s: blob %s: type: %w", session, hash, err)
	}
	if mt, err = agentsession.BlobMediaType(string(raw)); err != nil {
		return nil, "", fmt.Errorf("jsonl: session %s: blob %s: type: %w", session, hash, err)
	}
	return data, mt, nil
}

// sweepTypes removes, under the media guard, each type file among names
// whose blob the directory does not hold and that is older than grace,
// as old says: one a crash left between a sweep's removing a type and
// its blob, or between a put's placing a type and a sweep's look.
func (s *Store) sweepTypes(dir string, names []string, old func(string) bool) error {
	var stray []string
	for _, name := range names {
		if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, typeSuffix))); errors.Is(err, os.ErrNotExist) && old(name) {
			stray = append(stray, name)
		}
	}
	if len(stray) == 0 {
		return nil
	}
	return guarded(s.mediaGuard(), func() error {
		for _, name := range stray {
			if _, err := os.Stat(filepath.Join(dir, strings.TrimSuffix(name, typeSuffix))); !errors.Is(err, os.ErrNotExist) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("jsonl: media: %w", err)
			}
		}
		return nil
	})
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
