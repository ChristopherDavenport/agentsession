package export

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// TestSidecarExport: the projection of a sidecar session resolves each
// sidecar: reference in a step's visible content to a data URL of its
// blob before any hash, while the raw items keep the reference as
// recorded, since the entry IDs hash over it; a document written to
// files carries each blob beside it in the session's sidecar layout; and
// a blob that cannot be read is refused, never written as a dangling
// reference (RFC 0001, Projections, ATIF, since 0.21; agentsession#280).
func TestSidecarExport(t *testing.T) {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	hash := agentsession.HashBytes(data)
	ref := "sidecar:" + hash
	s := agentsession.New(agentsession.Header{ID: "side", CreatedAt: time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC), Media: agentsession.MediaSidecar})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: "see"}, &openresponses.InputImage{ImageURL: ref})))
	blobs := func(session, h string) ([]byte, string, error) {
		if session == "side" && h == hash {
			return data, "image/png", nil
		}
		return nil, "", agentsession.ErrNoBlob
	}
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]Blobs{
		"no reader":  nil,
		"unreadable": func(string, string) ([]byte, string, error) { return nil, "", agentsession.ErrNoBlob },
		"other bytes": func(string, string) ([]byte, string, error) {
			return []byte("not the image"), "image/png", nil
		},
	} {
		if _, err := ToATIF(tr, Options{Blobs: b}); !errors.Is(err, ErrBlobUnread) {
			t.Errorf("%s: ToATIF = %v, want ErrBlobUnread", name, err)
		}
	}
	export := func() *atif.Trajectory {
		t.Helper()
		doc, err := ToATIF(tr, Options{Blobs: blobs})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	doc := export()
	for _, p := range doc.Steps[0].Message.Parts {
		if p.Source != nil && strings.Contains(p.Source.Path, "sidecar:") || strings.Contains(p.Text, "sidecar:") {
			t.Errorf("a visible part names a sidecar: %+v", p)
		}
	}
	if parts := doc.Steps[0].Message.Parts; len(parts) != 2 || parts[1].Source == nil || !strings.HasPrefix(parts[1].Source.Path, "data:image/png;base64,") {
		t.Errorf("the image part: %+v", parts)
	}
	items, err := Items(doc)
	if err != nil {
		t.Fatal(err)
	}
	if img := items[0].(*openresponses.Message).Content[1].(*openresponses.InputImage); img.ImageURL != ref {
		t.Errorf("the raw item names %s, the session %s", img.ImageURL, ref)
	}

	dir := t.TempDir()
	if err := WriteATIF(filepath.Join(dir, "none"), one(export())); !errors.Is(err, ErrBlobUnread) {
		t.Errorf("WriteATIF with no blobs = %v, want ErrBlobUnread", err)
	}
	if err := WriteATIF(dir, one(export()), WithBlobs(blobs)); err != nil {
		t.Fatalf("WriteATIF: %v", err)
	}
	hex := strings.TrimPrefix(hash, agentsession.HashPrefix)
	if got, err := os.ReadFile(filepath.Join(dir, "sidecar", "side", hex)); err != nil || string(got) != string(data) {
		t.Errorf("the blob beside the document: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sidecar", "side", hex+".type")); err != nil || strings.TrimSuffix(string(got), "\n") != "image/png" {
		t.Errorf("the blob's type beside the document: %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "images", hex+".png")); err != nil {
		t.Errorf("the visible image beside the document: %v", err)
	}
}
