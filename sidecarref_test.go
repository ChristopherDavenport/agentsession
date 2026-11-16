package agentsession

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/openresponses"
)

// TestSidecarBlobsWholeStrings: a sidecar reference is a JSON string
// whose whole value is sidecar:sha256:<hex>, in any entry type; the
// same text inside a longer string, as a message quoting one, names no
// blob (#300).
func TestSidecarBlobsWholeStrings(t *testing.T) {
	ref := func(b byte) string { return "sha256:" + strings.Repeat(string(b), 64) }
	quoted, image, custom, note := ref('a'), ref('b'), ref('c'), ref('d')
	cases := []struct {
		name string
		e    Entry
		want []string
	}{
		{"a message quoting a reference", NewItemEntry(openresponses.UserText("see sidecar:" + quoted)), nil},
		{"a message quoting one exactly, in a text part", NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: "sidecar:" + quoted + " "})), nil},
		{"an image URL beside a quoting text", NewItemEntry(openresponses.UserMessage(
			&openresponses.InputText{Text: "see sidecar:" + quoted},
			&openresponses.InputImage{ImageURL: "sidecar:" + image},
		)), []string{image}},
		{"a custom entry member", &CustomEntry{NS: "acme", Data: json.RawMessage(`{"shot":{"url":"sidecar:` + custom + `"},"note":"see sidecar:` + note + `"}`)}, []string{custom}},
		{"a reference as an object key", &CustomEntry{NS: "acme", Data: json.RawMessage(`{"sidecar:` + note + `":1}`)}, nil},
		{"arrays inside objects", &CustomEntry{NS: "acme", Data: json.RawMessage(`{"a":["sidecar:` + quoted + `",{"sidecar:` + note + `":"sidecar:` + image + `"},[]],"b":"sidecar:` + custom + `","c":{}}`)}, []string{quoted, image, custom}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := SidecarBlobs(c.e)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("SidecarBlobs = %q, want %q", got, c.want)
			}
		})
	}
}

// TestSidecarBlobsInInvalid: a value that is not JSON is an error,
// with the references named before the error.
func TestSidecarBlobsInInvalid(t *testing.T) {
	h := "sha256:" + strings.Repeat("a", 64)
	got, err := SidecarBlobsIn([]byte(`{"a":"sidecar:` + h + `","b":[1,`))
	if err == nil {
		t.Error("a truncated value: no error")
	}
	if !slices.Equal(got, []string{h}) {
		t.Errorf("a truncated value names %q, want %q", got, []string{h})
	}
	if _, err := SidecarBlobsIn([]byte(`{"a":"sidecar:` + h + `"}}`)); err == nil {
		t.Error("a value followed by a stray brace: no error")
	}
}

// TestImportMediaQuotedReference: a sidecar session whose message
// quotes a reference imports without the blob, and the import reports
// nothing unresolved (#300).
func TestImportMediaQuotedReference(t *testing.T) {
	ctx := context.Background()
	held := []byte("a picture")
	h := HashBytes(held)
	s := New(Header{ID: "q", Media: MediaSidecar})
	if _, err := s.Append(NewItemEntry(openresponses.UserMessage(
		&openresponses.InputText{Text: "the blob was sidecar:sha256:" + strings.Repeat("e", 64)},
		&openresponses.InputImage{ImageURL: "sidecar:" + h},
	))); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	media := fstest.MapFS{strings.TrimPrefix(h, HashPrefix): {Data: held}}
	st := NewMemoryStore()
	_, unresolved, err := ImportMedia(ctx, st, &buf, media, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Errorf("unresolved = %q, want none: a quoted reference names no blob", unresolved)
	}
	if got, err := st.Blob(ctx, h); err != nil || !bytes.Equal(got, held) {
		t.Errorf("the image's blob: %q, %v", got, err)
	}
}
