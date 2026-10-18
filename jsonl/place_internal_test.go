package jsonl

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestRefusedPlaceLeavesNoDirectory: a create or an import refused after
// the project directory was made removes the directory it made, so a
// refused import leaves nothing under the root.
func TestRefusedPlaceLeavesNoDirectory(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := agentsession.New(agentsession.Header{ID: "fresh", CWD: "/fresh-project"})
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	refused := errors.New("refused after the directory was made")
	placeFault = func() error { return refused }
	defer func() { placeFault = nil }()
	if _, err := st.Import(ctx, &buf, true); !errors.Is(err, refused) {
		t.Fatalf("import: %v, want the injected refusal", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), ProjectKey("/fresh-project"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused import left its project directory: %v", err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "made", CWD: "/another-project"}); !errors.Is(err, refused) {
		t.Fatalf("create: %v, want the injected refusal", err)
	}
	if _, err := os.Stat(filepath.Join(st.Root(), ProjectKey("/another-project"))); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the refused create left its project directory: %v", err)
	}
}
