package export

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// sidecarRef matches a sidecar reference, the URL RFC 0001 gives an item
// that names a blob beside the file.
var sidecarRef = regexp.MustCompile(`sidecar:sha256:[0-9a-f]{64}`)

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

// resolveSidecar resolves each sidecar reference in the visible content
// of doc's own steps, a message's or an observation result's, through
// blobs: an image part names its blob as a data URL of the blob's type,
// or, for a type ATIF does not take, becomes the text a data URL of
// that type is described by, as any image of such a type does; a
// reference in text is replaced by that description. An embedded
// trajectory was resolved by its own export.
func resolveSidecar(doc *atif.Trajectory, session string, blobs Blobs) error {
	resolve := func(c *atif.Content) error {
		if c.Text != "" && sidecarRef.MatchString(c.Text) {
			text, err := describeRefs(c.Text, session, blobs)
			if err != nil {
				return err
			}
			c.Text = text
		}
		for i := range c.Parts {
			p := &c.Parts[i]
			if p.Text != "" && sidecarRef.MatchString(p.Text) {
				text, err := describeRefs(p.Text, session, blobs)
				if err != nil {
					return err
				}
				p.Text = text
			}
			if p.Source == nil || !strings.HasPrefix(p.Source.Path, "sidecar:") {
				continue
			}
			hash := strings.TrimPrefix(p.Source.Path, "sidecar:")
			data, mediaType, err := readBlob(blobs, session, hash)
			if err != nil {
				return err
			}
			url := "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
			mt := imageMediaType(url)
			if p.Type != atif.PartImage || mt == "" {
				*p = atif.ContentPart{Type: atif.PartText, Text: "[" + string(p.Type) + ": " + describeRef(url) + "]"}
				continue
			}
			p.Source.MediaType = mt
			p.Source.Path = "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
		return nil
	}
	for i := range doc.Steps {
		st := &doc.Steps[i]
		if err := resolve(&st.Message); err != nil {
			return err
		}
		if st.Observation != nil {
			for j := range st.Observation.Results {
				if err := resolve(&st.Observation.Results[j].Content); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// describeRefs replaces each sidecar reference in text with the
// description of a data URL of its blob, once the blob is read.
func describeRefs(text, session string, blobs Blobs) (string, error) {
	var err error
	out := sidecarRef.ReplaceAllStringFunc(text, func(ref string) string {
		if err != nil {
			return ref
		}
		var mediaType string
		if _, mediaType, err = readBlob(blobs, session, strings.TrimPrefix(ref, "sidecar:")); err != nil {
			return ref
		}
		return "data URL " + mediaType
	})
	return out, err
}

// copyBlobs writes each blob the raw items of doc reference, and of each
// trajectory it embeds, under SidecarDir in dir, in the layout of the
// session each belongs to, so a converter rebuilds the session from the
// document alone. A blob it cannot read refuses the write.
func copyBlobs(dir string, doc *atif.Trajectory, blobs Blobs) error {
	own := *doc
	own.SubagentTrajectories = nil
	data, err := json.Marshal(&own)
	if err != nil {
		return fmt.Errorf("export: encode: %w", err)
	}
	seen := map[string]bool{}
	for _, m := range sidecarRef.FindAll(data, -1) {
		hash := strings.TrimPrefix(string(m), "sidecar:")
		if seen[hash] {
			continue
		}
		seen[hash] = true
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
	for _, sub := range doc.SubagentTrajectories {
		if sub != nil {
			if err := copyBlobs(dir, sub, blobs); err != nil {
				return err
			}
		}
	}
	return nil
}
