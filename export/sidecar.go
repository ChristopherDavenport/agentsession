package export

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
)

// SidecarDir is the directory, beside a document [WriteDocument]
// writes, that holds the sidecar blobs its raw items reference, in the
// session's own layout: under a directory named after the session, the
// blob named by its hex digest alone and its media type in the file
// [agentsession.SidecarTypeName] names (RFC 0001, File; Projections,
// ATIF, since 0.21).
const SidecarDir = "sidecar"

// StoreBlobs reads a session's blobs from a store, as
// [agentsession.ReadSessionBlob] does: the type the session holds for
// the blob, the store's where the session holds none.
func StoreBlobs(ctx context.Context, br agentsession.BlobReader) Blobs {
	return func(session, hash string) ([]byte, string, error) {
		return agentsession.ReadSessionBlob(ctx, br, session, hash)
	}
}

// DirBlobs reads a session's blobs from the sidecar directory beside
// its file in dir, the directory named after the session, as RFC 0001's
// File section lays it out: each verified against its hash, with the
// type its type file holds.
func DirBlobs(dir string) Blobs {
	return func(session, hash string) ([]byte, string, error) {
		media := os.DirFS(filepath.Join(dir, session))
		data, err := agentsession.ReadSidecarBlob(media, hash)
		if err != nil {
			return nil, "", err
		}
		t, err := agentsession.ReadSidecarType(media, hash)
		return data, t, err
	}
}

// readBlob reads one blob of session through blobs and checks it hashes
// to its name, giving it the type a reader gives one held with none.
func readBlob(blobs Blobs, session, hash string) ([]byte, string, error) {
	if blobs == nil {
		return nil, "", fmt.Errorf("%w: %s of session %s: no blob reader", ErrBlobUnread, hash, session)
	}
	data, mediaType, err := blobs(session, hash)
	if err != nil {
		return nil, "", fmt.Errorf("%w: %s of session %s: %w", ErrBlobUnread, hash, session, err)
	}
	if got := agentsession.HashBytes(data); got != hash {
		return nil, "", fmt.Errorf("%w: %s of session %s holds bytes that hash to %s", ErrBlobUnread, hash, session, got)
	}
	if mediaType, err = agentsession.BlobMediaType(mediaType); err != nil {
		return nil, "", fmt.Errorf("%w: %s of session %s: %w", ErrBlobUnread, hash, session, err)
	}
	return data, mediaType, nil
}

// sidecars resolves the sidecar references of one session's parts as
// the projection renders them: nil for a session whose media is inline,
// which has none. The first blob it cannot read is kept in err, which
// [ToATIF] returns once the document is built.
type sidecars struct {
	session string
	blobs   Blobs
	read    map[string]resolved // by hash
	err     error
}

// resolved is a blob a sidecar reference names, as the projection
// renders it.
type resolved struct {
	// url is the data URL of the blob under the type the session holds
	// for it, [agentsession.DefaultMediaType] where it holds none.
	url string
	// image is, for a blob held with no type whose bytes sniff as an
	// image type ATIF takes, the data URL under that type; "" otherwise.
	image string
}

// newSidecars returns the resolver of a session with header h: nil
// unless its media is sidecar (RFC 0001, File).
func newSidecars(h agentsession.Header, blobs Blobs) *sidecars {
	if h.Media != agentsession.MediaSidecar {
		return nil
	}
	return &sidecars{session: h.ID, blobs: blobs, read: map[string]resolved{}}
}

// url returns the data URL of the blob ref names, its blob's media type
// and true, when ref, a URL a part holds, is a sidecar reference; for any
// other it returns ref and false. A blob it cannot read is kept in s.err
// and ref returned as it is.
func (s *sidecars) url(ref string) (string, bool) {
	r, ok := s.resolve(ref)
	return r.url, ok
}

// imageURL is url for an image part: a blob held with no type is
// rendered under the image type its bytes sniff as, where that is one
// ATIF takes, as RFC 0001's Projections, ATIF, asks. The sniffed type
// is the projection's alone; the blob beside the document keeps none.
func (s *sidecars) imageURL(ref string) (string, bool) {
	r, ok := s.resolve(ref)
	if r.image != "" {
		return r.image, ok
	}
	return r.url, ok
}

// resolve reads the blob ref names, once, when ref is a sidecar
// reference; for any other ref it returns ref as the url, and false. A
// blob it cannot read is kept in s.err, and ref returned as it is.
func (s *sidecars) resolve(ref string) (resolved, bool) {
	if s == nil {
		return resolved{url: ref}, false
	}
	hash, ok := strings.CutPrefix(ref, "sidecar:")
	if !ok || !agentsession.ValidHash(hash) {
		return resolved{url: ref}, false
	}
	if r, ok := s.read[hash]; ok {
		return r, true
	}
	if s.err != nil {
		return resolved{url: ref}, true
	}
	data, mediaType, err := readBlob(s.blobs, s.session, hash)
	if err != nil {
		s.err = err
		return resolved{url: ref}, true
	}
	r := resolved{url: dataURL(mediaType, data)}
	if mediaType == "" {
		if t := sniffImage(data); t != "" {
			r.image = dataURL(t, data)
		}
	}
	s.read[hash] = r
	return r, true
}

// dataURL is the data URL of data under mediaType, in the one form a
// written document moves to a file: data:<type>;base64, and the bytes
// in standard base64 with padding. An empty mediaType, a blob or part
// with none, is [agentsession.DefaultMediaType], since RFC 2397 reads
// data:; as text/plain.
func dataURL(mediaType string, data []byte) string {
	if mediaType == "" {
		mediaType = agentsession.DefaultMediaType
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// sidecarMedia is the media the root extra names, under
// [ExtraAgentSession], for a document of a sidecar session.
func sidecarMedia(doc *atif.Trajectory) bool {
	as, _ := doc.Extra[ExtraAgentSession].(map[string]any)
	m, _ := as["media"].(string)
	return m == agentsession.MediaSidecar
}

// partURLs are the members of a part that hold its URL, the only
// members a sidecar reference is written in (RFC 0001, File).
var partURLs = map[string]bool{"image_url": true, "file_url": true, "video_url": true}

// sidecarURLs adds to seen each part URL of v, at any depth, that is a
// sidecar reference as a whole, appending each new one's hash to out. A
// string in any other member, such as a message's text, is text.
func sidecarURLs(v any, seen map[string]bool, out []string) []string {
	switch t := v.(type) {
	case []any:
		for _, el := range t {
			out = sidecarURLs(el, seen, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if u, ok := t[k].(string); ok {
				if h, ok := strings.CutPrefix(u, "sidecar:"); ok && partURLs[k] && agentsession.ValidHash(h) && !seen[h] {
					seen[h] = true
					out = append(out, h)
				}
				continue
			}
			out = sidecarURLs(t[k], seen, out)
		}
	}
	return out
}

// copyBlobs writes each blob the raw items of doc reference, and of each
// trajectory it embeds, under SidecarDir in dir, in the layout of the
// session each belongs to, so a converter rebuilds the session from the
// document alone. Only a document of a sidecar session, which names its
// media under [ExtraAgentSession], references a blob, and only by a
// part's URL that is a sidecar: URL as a whole; text quoting one, or
// that is one, is text. A blob it cannot read refuses the write.
func copyBlobs(dir string, doc *atif.Trajectory, blobs Blobs) error {
	if sidecarMedia(doc) {
		own := *doc
		own.SubagentTrajectories = nil
		data, err := json.Marshal(&own)
		if err != nil {
			return fmt.Errorf("export: encode: %w", err)
		}
		var tree any
		if err := json.Unmarshal(data, &tree); err != nil {
			return fmt.Errorf("export: decode: %w", err)
		}
		for _, hash := range sidecarURLs(tree, map[string]bool{}, nil) {
			blob, mediaType, err := readBlob(blobs, doc.SessionID, hash)
			if err != nil {
				return err
			}
			at := filepath.Join(dir, SidecarDir, safeName(doc.SessionID))
			if err := os.MkdirAll(at, 0o755); err != nil {
				return fmt.Errorf("export: create %s: %w", at, err)
			}
			if err := os.WriteFile(filepath.Join(at, strings.TrimPrefix(hash, agentsession.HashPrefix)), blob, 0o644); err != nil {
				return fmt.Errorf("export: write blob %s: %w", hash, err)
			}
			if mediaType != "" {
				if err := os.WriteFile(filepath.Join(at, agentsession.SidecarTypeName(hash)), []byte(mediaType), 0o644); err != nil {
					return fmt.Errorf("export: write the type of blob %s: %w", hash, err)
				}
			}
		}
	}
	for _, sub := range doc.SubagentTrajectories {
		if sub != nil {
			if err := copyBlobs(dir, sub, blobs); err != nil {
				return err
			}
		}
	}
	return nil
}
