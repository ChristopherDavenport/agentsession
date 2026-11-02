package agentsession

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
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

// TypedBlobWriter is implemented by a store that keeps a blob's media
// type beside it, as RFC 0002's Media section has every store that
// holds sidecar media do: PutTypedBlob stores the blob as PutBlob does,
// with mediaType, the type of the data URL the blob replaced. The type
// is outside every hash: the hash returned, and the sidecar: URL that
// names the blob, are of the bytes alone.
//
// A type once held never changes. Storing bytes the store holds with
// another type, or with none, keeps the type held; storing them with a
// type where the store holds none gives them that one. An empty
// mediaType, or [DefaultMediaType] with no parameters, is none, and
// PutBlob is PutTypedBlob with none. A type that is not a media type is
// refused with [ErrBadMediaType]. A sweep that removes the blob removes
// its type.
//
// It is an interface of its own, beside [BlobWriter], so a writer
// declared against BlobWriter, or an interface of the same shape
// declared elsewhere, is satisfied as it was.
type TypedBlobWriter interface {
	BlobWriter
	PutTypedBlob(ctx context.Context, data []byte, mediaType string) (string, error)
}

// TypedBlobReader is implemented by a store that returns a blob with its
// media type: TypedBlob returns the bytes and the type they were stored
// with, as given, and "" for a blob held with none, which a reader that
// needs a type reads as [DefaultMediaType]. A blob the store does not
// hold is an error wrapping [ErrNoBlob].
type TypedBlobReader interface {
	BlobReader
	TypedBlob(ctx context.Context, hash string) (data []byte, mediaType string, err error)
}

// DefaultMediaType is the type a reader gives a blob held with none, as
// RFC 0002's Media section says: a writer resolving a sidecar: URL to a
// data URL for a provider uses it for such a blob. Given to a store as
// a blob's type with no parameters, it says no more than none, and the
// store holds the blob with none.
const DefaultMediaType = "application/octet-stream"

// MaxMediaTypeLen is the longest media type a store of this library
// keeps; it refuses a longer one with [ErrBadMediaType], as RFC 0002
// lets a store, rather than shorten it.
const MaxMediaTypeLen = 1024

// ErrBadMediaType is returned for a blob's media type that is not one:
// a store refuses to keep it, and an import refuses a sidecar directory
// whose type file holds one.
var ErrBadMediaType = errors.New("agentsession: not a media type")

// BlobMediaType checks a blob's media type as a store keeps it, RFC
// 6838's type/subtype with any parameters, and returns what the store
// holds for it: the type as given, or "" for none, which an empty type
// and [DefaultMediaType] with no parameters both are. A type that is
// not printable US-ASCII, has no subtype, does not parse, or is longer
// than [MaxMediaTypeLen] is [ErrBadMediaType].
func BlobMediaType(t string) (string, error) {
	if t == "" || strings.EqualFold(t, DefaultMediaType) {
		return "", nil
	}
	if len(t) > MaxMediaTypeLen {
		return "", fmt.Errorf("%w: %d bytes, longer than %d", ErrBadMediaType, len(t), MaxMediaTypeLen)
	}
	for i := 0; i < len(t); i++ {
		if t[i] < 0x20 || t[i] > 0x7e {
			return "", fmt.Errorf("%w: %q", ErrBadMediaType, t)
		}
	}
	base, _, err := mime.ParseMediaType(t)
	if err != nil || strings.TrimSpace(t) != t {
		return "", fmt.Errorf("%w: %q", ErrBadMediaType, t)
	}
	if typ, sub, ok := strings.Cut(base, "/"); !ok || typ == "" || sub == "" {
		return "", fmt.Errorf("%w: %q has no subtype", ErrBadMediaType, t)
	}
	return t, nil
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

// SidecarTypeName is the name RFC 0001 gives, since format 0.16, the
// file in a session's sidecar directory that holds the media type of
// the blob named hash: its hex digest and ".type".
func SidecarTypeName(hash string) string {
	return strings.TrimPrefix(hash, HashPrefix) + ".type"
}

// ReadSidecarType reads the media type of the blob named hash from a
// session's sidecar directory, the file [SidecarTypeName] names, one
// trailing LF ignored, and returns it as [BlobMediaType] does: "" for a
// blob whose directory holds no type file, as one a 0.15 writer
// projected. A file that holds no media type is [ErrBadMediaType].
func ReadSidecarType(media fs.FS, hash string) (string, error) {
	if !ValidHash(hash) {
		return "", fmt.Errorf("agentsession: blob %q: not a hash", hash)
	}
	if media == nil {
		return "", nil
	}
	data, err := fs.ReadFile(media, SidecarTypeName(hash))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	t, err := BlobMediaType(strings.TrimSuffix(string(data), "\n"))
	if err != nil {
		return "", fmt.Errorf("agentsession: blob %s: its type file: %w", hash, err)
	}
	return t, nil
}

// putBlobTyped stores a blob through bw with mediaType: by
// [TypedBlobWriter] when bw is one, and by PutBlob, the type left
// behind, when it is not.
func putBlobTyped(ctx context.Context, bw BlobWriter, data []byte, mediaType string) (string, error) {
	if tw, ok := bw.(TypedBlobWriter); ok {
		return tw.PutTypedBlob(ctx, data, mediaType)
	}
	return bw.PutBlob(ctx, data)
}

// ImportMedia imports an RFC 0001 file into st with the session's
// sidecar directory, media, as RFC 0002's import of a projection does:
// it verifies each blob the session's items name against its name and
// stores it, with the media type the directory gives it beside it
// ([ReadSidecarType]), and returns the hashes of those the directory
// lacks and the store does not hold, unresolved, as an append reports
// one. The session
// is imported either way; one with an unresolved blob cannot be pushed
// or projected until the blob arrives. A blob that fails its name refuses
// the import with [ErrBlobMismatch], and a type file that holds no media
// type with [ErrBadMediaType]. A session whose media is not
// sidecar is imported as [Importer.Import] does, and media is not read.
//
// A store that implements [MediaImporter] does the whole of it. For one
// that does not, a sidecar session needs a [BlobWriter], or it is
// refused with [ErrNoBlobWriter], and its types are kept when it is a
// [TypedBlobWriter]; the file is imported first and the blobs stored
// after, so a refused import stores nothing, and a failure
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
	types := map[string]string{}
	var unresolved []string
	for _, h := range hashes {
		b, err := ReadSidecarBlob(media, h)
		switch {
		case err == nil:
			blobs[h] = b
			if types[h], err = ReadSidecarType(media, h); err != nil {
				return nil, nil, err
			}
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
		if _, err := putBlobTyped(ctx, bw, b, types[h]); err != nil {
			return sess, unresolved, fmt.Errorf("agentsession: import: the session is imported, and blob %s could not be stored: %w", h, err)
		}
	}
	return sess, unresolved, nil
}
