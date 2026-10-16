package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestReadForImportRefusesAMarker: a projection's synthetic leaf marker
// names a head the resume rule over the lines before it would not, so
// a store keeping its head only in its entries refuses the file.
func TestReadForImportRefusesAMarker(t *testing.T) {
	s := New(Header{ID: "m"})
	root, err := s.Append(NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	var plain bytes.Buffer
	if err := Write(&plain, s); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadForImport(bytes.NewReader(plain.Bytes())); err != nil {
		t.Fatalf("a file without a marker: %v", err)
	}
	marker := NewLabelEntry(root, LeafLabel)
	marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage(`true`)}
	if _, err := s.Commit(marker); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadForImport(&buf); !errors.Is(err, ErrSyntheticMarker) {
		t.Errorf("a file with a marker: %v, want ErrSyntheticMarker", err)
	}
}
