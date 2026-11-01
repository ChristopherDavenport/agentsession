package agentsession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"
	"time"
)

// BlobWriter is implemented by a store that holds sidecar media, as RFC
// 0002's Media section requires of a store that holds a sidecar session:
// PutBlob stores a blob by its bytes under its hash, the SHA-256 of the
// bytes in the format's notation, and returns the hash. An item names
// the blob with a sidecar: URL carrying the hash. Storing a blob the
// store holds already changes nothing.
//
// A store that cannot hold blobs refuses to create or import a session
// whose media is sidecar, with [ErrNoBlobWriter].
type BlobWriter interface {
	PutBlob(ctx context.Context, data []byte) (string, error)
}

// BlobReader is implemented by a store that serves the sidecar blobs it
// holds: Blob returns a blob's bytes by its hash, and an error wrapping
// [ErrNoBlob] for one it does not hold.
type BlobReader interface {
	Blob(ctx context.Context, hash string) ([]byte, error)
}

// BlobSweeper is implemented by a store whose blobs RFC 0002's sweep
// collects (Deletion and retention): SweepBlobs removes each blob that
// no entry of any session the store holds names, prefixes included, and
// that was stored, or stored again, longer than grace ago, and returns
// how many it removed. A blob is retained while a session names it, and
// grace spares one a write is about to name, as an upload awaits the
// append, import or push that names it. A sweep never removes a blob
// that a write it races names: [MemoryStore] and the sqlite store sweep
// in one step no write interleaves with, and the jsonl store's append
// and import restart the grace of each blob they name before they land,
// and its sweep looks at a blob's age again under a guard the restart
// takes. The library sweeps only when called; RFC 0003's server calls
// it for the store it serves. A store whose blobs are objects among
// others, as cas's are, collects them in its own sweep instead.
type BlobSweeper interface {
	SweepBlobs(ctx context.Context, grace time.Duration) (int, error)
}

// ErrNoBlobWriter is returned for a sidecar session given to a store
// that cannot hold its blobs, which RFC 0002 has it refuse.
var ErrNoBlobWriter = errors.New("agentsession: the store cannot hold sidecar media, so it takes no sidecar session")

// ErrNoBlob is returned by [BlobReader.Blob] for a blob the store does
// not hold.
var ErrNoBlob = errors.New("agentsession: no such blob")

// ErrBlobMismatch is returned by an import whose sidecar directory
// holds a blob whose bytes do not hash to its name: the blob is not the
// one the items name, and the import is refused, as one whose line
// fails its hash is.
var ErrBlobMismatch = errors.New("agentsession: a sidecar blob does not hash to its name")

// MediaImporter is implemented by a store that imports a sidecar session
// with its blobs in one step, as RFC 0002's import of a projection
// takes the session's directory with the file. See [ImportMedia].
type MediaImporter interface {
	ImportMedia(ctx context.Context, r io.Reader, media fs.FS, asRecord bool) (*Session, []string, error)
}

// sidecarRefs matches a sidecar reference, the URL RFC 0001 gives an
// item that names a blob beside the file.
var sidecarRefs = regexp.MustCompile(`sidecar:sha256:[0-9a-f]{64}`)

// SidecarBlobs returns the hashes of the sidecar blobs an entry's items
// name, each once, in the order they first appear.
func SidecarBlobs(e Entry) ([]string, error) {
	line, err := MarshalEntry(e)
	if err != nil {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	for _, m := range sidecarRefs.FindAll(line, -1) {
		h := strings.TrimPrefix(string(m), "sidecar:")
		if !seen[h] {
			seen[h] = true
			out = append(out, h)
		}
	}
	return out, nil
}

// SessionBlobs returns the hashes of the sidecar blobs a session's
// entries name, its prefix included, each once.
func SessionBlobs(s *Session) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	for _, e := range s.Entries() {
		bs, err := SidecarBlobs(e)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			if !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out, nil
}

// ReadSidecarBlob reads the blob named hash from a session's sidecar
// directory, where RFC 0001 names it by its hex digest alone, and
// verifies it against the hash. A blob the directory lacks is
// fs.ErrNotExist; one whose bytes do not hash to its name is
// [ErrBlobMismatch].
func ReadSidecarBlob(media fs.FS, hash string) ([]byte, error) {
	if !ValidHash(hash) {
		return nil, fmt.Errorf("agentsession: blob %q: not a hash", hash)
	}
	if media == nil {
		return nil, fs.ErrNotExist
	}
	data, err := fs.ReadFile(media, strings.TrimPrefix(hash, HashPrefix))
	if err != nil {
		return nil, err
	}
	if got := HashBytes(data); got != hash {
		return nil, fmt.Errorf("%w: %s holds bytes that hash to %s", ErrBlobMismatch, hash, got)
	}
	return data, nil
}

// ImportMedia imports an RFC 0001 file into st with the session's
// sidecar directory, media, as RFC 0002's import of a projection does:
// it verifies each blob the session's items name against its name and
// stores it, and returns the hashes of those the directory lacks and the
// store does not hold, unresolved, as an append reports one. The session
// is imported either way; one with an unresolved blob cannot be pushed
// or projected until the blob arrives. A blob that fails its name refuses
// the import with [ErrBlobMismatch]. A session whose media is not
// sidecar is imported as [Importer.Import] does, and media is not read.
//
// A store that implements [MediaImporter] does the whole of it. For one
// that does not, a sidecar session needs a [BlobWriter], or it is
// refused with [ErrNoBlobWriter]; the file is imported first and the
// blobs stored after, so a refused import stores nothing, and a failure
// between the two leaves the blobs unresolved, which another call to
// PutBlob resolves. Where the store is no [BlobReader] either, a blob
// the directory lacks is reported unresolved though the store may hold
// it.
func ImportMedia(ctx context.Context, st Importer, r io.Reader, media fs.FS, asRecord bool) (*Session, []string, error) {
	if mi, ok := st.(MediaImporter); ok {
		return mi.ImportMedia(ctx, r, media, asRecord)
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, nil, err
	}
	file, err := ReadForImport(bytes.NewReader(data))
	if err != nil {
		return nil, nil, err
	}
	if mediaForm(file.Header().Media) != MediaSidecar {
		sess, err := st.Import(ctx, bytes.NewReader(data), asRecord)
		return sess, nil, err
	}
	bw, ok := st.(BlobWriter)
	if !ok {
		return nil, nil, ErrNoBlobWriter
	}
	hashes, err := SessionBlobs(file)
	if err != nil {
		return nil, nil, err
	}
	blobs := make(map[string][]byte, len(hashes))
	var unresolved []string
	for _, h := range hashes {
		b, err := ReadSidecarBlob(media, h)
		switch {
		case err == nil:
			blobs[h] = b
		case errors.Is(err, fs.ErrNotExist):
			if br, ok := st.(BlobReader); ok {
				if _, err := br.Blob(ctx, h); err == nil {
					continue
				}
			}
			unresolved = append(unresolved, h)
		default:
			return nil, nil, err
		}
	}
	sess, err := st.Import(ctx, bytes.NewReader(data), asRecord)
	if err != nil {
		return nil, nil, err
	}
	for _, h := range hashes {
		b, ok := blobs[h]
		if !ok {
			continue
		}
		if _, err := bw.PutBlob(ctx, b); err != nil {
			return sess, unresolved, fmt.Errorf("agentsession: import: the session is imported, and blob %s could not be stored: %w", h, err)
		}
	}
	return sess, unresolved, nil
}
