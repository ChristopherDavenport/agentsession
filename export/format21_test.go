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

// TestSidecarQuotedText: only a session whose header says sidecar has
// sidecar references, and only a part's URL is one. Text that quotes a
// reference, as a session about this format or a tool output showing a
// fixture does, is text: an inline session holding it exports and
// writes with no blob reader, and a sidecar session shows it as written
// and needs no blob for it, even where the whole text is a reference
// (RFC 0001, Projections, ATIF, since 0.21).
func TestSidecarQuotedText(t *testing.T) {
	quoted := "sidecar:sha256:" + strings.Repeat("ab", 32)
	at := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)

	inline := agentsession.New(agentsession.Header{ID: "inline", CreatedAt: at})
	mustAppend(t, inline, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: "the fixture names " + quoted})))
	mustAppend(t, inline, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: quoted})))
	tr, err := At(inline, inline.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{})
	if err != nil {
		t.Fatalf("ToATIF of an inline session quoting a reference: %v", err)
	}
	if got := doc.Steps[0].Message.Text; got != "the fixture names "+quoted {
		t.Errorf("the quoted text reads %q", got)
	}
	if err := WriteATIF(t.TempDir(), one(doc)); err != nil {
		t.Errorf("WriteATIF of an inline session quoting a reference: %v", err)
	}

	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	hash := agentsession.HashBytes(data)
	blobs := func(session, h string) ([]byte, string, error) {
		if h == hash {
			return data, "image/png", nil
		}
		return nil, "", agentsession.ErrNoBlob
	}
	side := agentsession.New(agentsession.Header{ID: "side", CreatedAt: at, Media: agentsession.MediaSidecar})
	mustAppend(t, side, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: "see " + quoted}, &openresponses.InputImage{ImageURL: "sidecar:" + hash})))
	// A message whose whole text is a reference is text still: the
	// document holds it as a whole string, and no blob is asked for it.
	mustAppend(t, side, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: quoted})))
	tr, err = At(side, side.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if doc, err = ToATIF(tr, Options{Blobs: blobs}); err != nil {
		t.Fatalf("ToATIF of a sidecar session quoting a reference: %v", err)
	}
	if parts := doc.Steps[0].Message.Parts; len(parts) != 2 || parts[0].Text != "see "+quoted || parts[1].Source == nil || !strings.HasPrefix(parts[1].Source.Path, "data:image/png;base64,") {
		t.Errorf("the parts: %+v", parts)
	}
	dir := t.TempDir()
	if err := WriteATIF(dir, one(doc), WithBlobs(blobs)); err != nil {
		t.Fatalf("WriteATIF of a sidecar session quoting a reference: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sidecar", "side", strings.TrimPrefix(hash, agentsession.HashPrefix))); err != nil {
		t.Errorf("the image's blob beside the document: %v", err)
	}
}

// TestSidecarUntypedImage: an image part whose blob the session holds
// with no type takes its type from the blob's bytes by WHATWG MIME
// Sniffing's image type pattern matching. Bytes that match an image
// type ATIF takes export as that image; any others render as text,
// described as an application/octet-stream data URL, never as data:;
// which RFC 2397 reads as text/plain. The sniffed type is the
// projection's alone: no type file is written beside either blob (RFC
// 0001, File; Projections, ATIF; agentsession#292).
func TestSidecarUntypedImage(t *testing.T) {
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	if err != nil {
		t.Fatal(err)
	}
	other := []byte("plain bytes, no image")
	held := map[string][]byte{agentsession.HashBytes(png): png, agentsession.HashBytes(other): other}
	blobs := func(session, h string) ([]byte, string, error) {
		if b, ok := held[h]; ok && session == "side" {
			return b, "", nil
		}
		return nil, "", agentsession.ErrNoBlob
	}
	s := agentsession.New(agentsession.Header{ID: "side", CreatedAt: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), Media: agentsession.MediaSidecar})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserMessage(
		&openresponses.InputText{Text: "see"},
		&openresponses.InputImage{ImageURL: "sidecar:" + agentsession.HashBytes(png)},
		&openresponses.InputImage{ImageURL: "sidecar:" + agentsession.HashBytes(other)},
	)))
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	export := func() *atif.Trajectory {
		t.Helper()
		doc, err := ToATIF(tr, Options{Blobs: blobs})
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	parts := export().Steps[0].Message.Parts
	if len(parts) != 3 {
		t.Fatalf("parts: %+v", parts)
	}
	if want := "data:image/png;base64," + base64.StdEncoding.EncodeToString(png); parts[1].Type != atif.PartImage || parts[1].Source == nil || parts[1].Source.MediaType != "image/png" || parts[1].Source.Path != want {
		t.Errorf("the untyped PNG: %+v, want an image/png part", parts[1])
	}
	if parts[2].Type != atif.PartText || parts[2].Text != "[image: data URL application/octet-stream]" {
		t.Errorf("the untyped other bytes: %+v, want text naming application/octet-stream", parts[2])
	}

	dir := t.TempDir()
	if err := WriteATIF(dir, one(export()), WithBlobs(blobs)); err != nil {
		t.Fatalf("WriteATIF: %v", err)
	}
	for h, b := range held {
		hex := strings.TrimPrefix(h, agentsession.HashPrefix)
		if got, err := os.ReadFile(filepath.Join(dir, "sidecar", "side", hex)); err != nil || string(got) != string(b) {
			t.Errorf("the blob %s beside the document: %v", hex, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "sidecar", "side", hex+".type")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a type file beside the untyped blob %s: %v", hex, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "images", strings.TrimPrefix(agentsession.HashBytes(png), agentsession.HashPrefix)+".png")); err != nil {
		t.Errorf("the visible image beside the document: %v", err)
	}
}

// TestInlineMediaUntyped: a moved media part with no media_type comes
// back inline as an application/octet-stream data URL, the type a
// reader gives bytes with none, never as data:; which RFC 2397 reads
// as text/plain (agentsession#292).
func TestInlineMediaUntyped(t *testing.T) {
	data := []byte("bytes with no type")
	hex := strings.TrimPrefix(agentsession.HashBytes(data), agentsession.HashPrefix)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "images"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "images", hex), data, 0o644); err != nil {
		t.Fatal(err)
	}
	src := map[string]any{"path": "images/" + hex}
	doc := map[string]any{"steps": []any{map[string]any{"message": []any{map[string]any{"type": "image", "source": src}}}}}
	if err := InlineMedia(doc, dir); err != nil {
		t.Fatal(err)
	}
	if want := "data:application/octet-stream;base64," + base64.StdEncoding.EncodeToString(data); src["path"] != want {
		t.Errorf("inlined %v, want %s", src["path"], want)
	}
}

// TestSniffImage: the image patterns of WHATWG MIME Sniffing that name
// a type ATIF takes, and nothing else.
func TestSniffImage(t *testing.T) {
	for _, tt := range []struct {
		data string
		want string
	}{
		{"\x89PNG\r\n\x1a\nrest", "image/png"},
		{"\xff\xd8\xffrest", "image/jpeg"},
		{"GIF87arest", "image/gif"},
		{"GIF89arest", "image/gif"},
		{"RIFF\x01\x02\x03\x04WEBPVP8 ", "image/webp"},
		{"RIFF\x01\x02\x03\x04WAVEfmt ", ""},
		{"BMrest", ""},           // image/bmp, a type ATIF does not take
		{"\x00\x00\x01\x00", ""}, // image/x-icon, likewise
		{"\x89PNG", ""},
		{"", ""},
		{"<svg xmlns=", ""},
	} {
		if got := sniffImage([]byte(tt.data)); got != tt.want {
			t.Errorf("sniffImage(%q) = %q, want %q", tt.data, got, tt.want)
		}
	}
}
