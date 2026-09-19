package agentsession

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// update regenerates golden files under testdata from the current
// output. Review the diff before committing.
var update = flag.Bool("update", false, "rewrite golden files")

var fixedTime = time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)

func ptr[T any](v T) *T { return &v }

// loadFixture reads a session fixture from testdata/sessions.
func loadFixture(t *testing.T, name string) *Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := Read(f)
	if err != nil {
		t.Fatalf("Read %s: %v", name, err)
	}
	return s
}

// checkGolden compares got (pretty-printed JSON) against the golden
// file, rewriting it under -update.
func checkGolden(t *testing.T, path string, got any) {
	t.Helper()
	data, err := json.MarshalIndent(got, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, data) {
		t.Errorf("golden %s differs\nwant:\n%s\ngot:\n%s", path, want, data)
	}
}
