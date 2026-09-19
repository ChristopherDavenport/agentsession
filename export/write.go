package export

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"

	"github.com/ChristopherDavenport/agentsession/atif"
)

// WriteATIF writes one JSON file per document under dir. A session's
// main trajectory (see [Trajectory.Main]) is named "<session_id>.json",
// which is what an unresolved subsession reference points at; every
// other document is "<session_id>_<trajectory_id>.json", or
// "<trajectory_id>.json" without a session ID. Media carried inline as
// data URLs
// is written beside the documents under images/ and audio/, named by
// content hash, and the parts are rewritten to point at those files.
// Documents are validated before they are written.
func WriteATIF(dir string, docs iter.Seq[*atif.Trajectory]) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("export: create %s: %w", dir, err)
	}
	for doc := range docs {
		if doc == nil {
			continue
		}
		spilled := map[string]string{}
		if err := spillMedia(dir, doc, spilled); err != nil {
			return err
		}
		if len(spilled) > 0 {
			// The raw items in the extras carry the same data URLs; point
			// them at the files too so the document stays small and the
			// bundle stays lossless.
			err := rewriteStrings(doc, func(s string) string {
				if path, ok := spilled[s]; ok {
					return path
				}
				return s
			})
			if err != nil {
				return fmt.Errorf("export: %s: %w", DocumentName(doc), err)
			}
		}
		if err := doc.Validate(); err != nil {
			return fmt.Errorf("export: %s: %w", DocumentName(doc), err)
		}
		data, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return fmt.Errorf("export: encode %s: %w", DocumentName(doc), err)
		}
		path := filepath.Join(dir, DocumentName(doc))
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			return fmt.Errorf("export: write %s: %w", path, err)
		}
	}
	return nil
}

// DocumentName returns the file name WriteATIF uses for a document.
func DocumentName(doc *atif.Trajectory) string {
	if doc.SessionID != "" && isMain(doc) {
		return MainDocumentName(doc.SessionID)
	}
	id := safeName(doc.TrajectoryID)
	if id == "" {
		id = "trajectory"
	}
	if doc.SessionID != "" {
		return safeName(doc.SessionID) + "_" + id + ".json"
	}
	return id + ".json"
}

// MainDocumentName returns the file name WriteATIF uses for a session's
// main trajectory, and the path an unresolved subsession reference
// carries.
func MainDocumentName(sessionID string) string {
	return safeName(sessionID) + ".json"
}

// isMain reports whether the exporter marked the document as its
// session's main trajectory.
func isMain(doc *atif.Trajectory) bool {
	as, ok := doc.Extra[ExtraAgentSession].(map[string]any)
	if !ok {
		return false
	}
	main, _ := as["main"].(bool)
	return main
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '-'
	}, s)
}

// spillMedia writes data-URL media to files and rewrites the parts,
// recording each data URL's new relative path in spilled.
func spillMedia(dir string, doc *atif.Trajectory, spilled map[string]string) error {
	for i := range doc.Steps {
		s := &doc.Steps[i]
		if err := spillParts(dir, s.Message.Parts, spilled); err != nil {
			return err
		}
		if s.Observation != nil {
			for j := range s.Observation.Results {
				if err := spillParts(dir, s.Observation.Results[j].Content.Parts, spilled); err != nil {
					return err
				}
			}
		}
	}
	for _, sub := range doc.SubagentTrajectories {
		if sub != nil {
			if err := spillMedia(dir, sub, spilled); err != nil {
				return err
			}
		}
	}
	return nil
}

func spillParts(dir string, parts []atif.ContentPart, spilled map[string]string) error {
	for i := range parts {
		p := &parts[i]
		if p.Source == nil || !strings.HasPrefix(p.Source.Path, "data:") {
			continue
		}
		mediaType, data, err := decodeDataURL(p.Source.Path)
		if err != nil {
			return fmt.Errorf("export: %s part: %w", p.Type, err)
		}
		if mediaType == "" {
			mediaType = p.Source.MediaType
		}
		sub := "images"
		if p.Type == atif.PartAudio {
			sub = "audio"
		}
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return fmt.Errorf("export: create %s: %w", sub, err)
		}
		sum := sha256.Sum256(data)
		name := hex.EncodeToString(sum[:]) + extensionFor(mediaType)
		path := filepath.Join(dir, sub, name)
		if _, err := os.Stat(path); err != nil {
			if err := os.WriteFile(path, data, 0o644); err != nil {
				return fmt.Errorf("export: write media %s: %w", path, err)
			}
		}
		spilled[p.Source.Path] = sub + "/" + name
		p.Source.Path = sub + "/" + name
		if mediaType != "" {
			p.Source.MediaType = mediaType
		}
	}
	return nil
}

// decodeDataURL splits a data URL into its media type and bytes.
func decodeDataURL(url string) (mediaType string, data []byte, err error) {
	rest := strings.TrimPrefix(url, "data:")
	meta, payload, ok := strings.Cut(rest, ",")
	if !ok {
		return "", nil, fmt.Errorf("malformed data URL")
	}
	params := strings.Split(meta, ";")
	mediaType = params[0]
	base64Encoded := false
	for _, p := range params[1:] {
		if p == "base64" {
			base64Encoded = true
		}
	}
	if base64Encoded {
		data, err = base64.StdEncoding.DecodeString(payload)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(payload)
		}
		if err != nil {
			return "", nil, fmt.Errorf("decode base64: %w", err)
		}
		return mediaType, data, nil
	}
	return mediaType, []byte(payload), nil
}

func extensionFor(mediaType string) string {
	switch mediaType {
	case "image/jpeg", "image/jpg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "audio/wav":
		return ".wav"
	case "audio/mpeg":
		return ".mp3"
	case "audio/mp4":
		return ".m4a"
	case "audio/aac":
		return ".aac"
	case "audio/ogg":
		return ".ogg"
	case "audio/flac":
		return ".flac"
	case "audio/webm":
		return ".webm"
	case "audio/aiff":
		return ".aiff"
	}
	if _, sub, ok := strings.Cut(mediaType, "/"); ok && sub != "" {
		return "." + safeName(sub)
	}
	return ".bin"
}
