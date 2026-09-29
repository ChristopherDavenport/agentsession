package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/openresponses"
)

// hashVector is one entry of testdata/hash/vectors.json: a request as it
// was sent and the hash a conforming writer records for it. agentturn
// tests its own copy of the hash against the same file.
type hashVector struct {
	Name    string          `json:"name"`
	Request json.RawMessage `json:"request"`
	Hash    string          `json:"hash"`
}

func TestRequestHashVectors(t *testing.T) {
	path := filepath.Join("testdata", "hash", "vectors.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var vectors []hashVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	for i, v := range vectors {
		t.Run(v.Name, func(t *testing.T) {
			// Through the typed request, as a writer would hash it.
			var req openresponses.Request
			if err := json.Unmarshal(v.Request, &req); err != nil {
				t.Fatal(err)
			}
			got, err := RequestHash(req)
			if err != nil {
				t.Fatal(err)
			}
			// And straight from the bytes, as a reader might.
			raw, err := HashRequestJSON(v.Request)
			if err != nil {
				t.Fatal(err)
			}
			if got != raw {
				t.Errorf("typed hash %s != raw hash %s", got, raw)
			}
			if *update {
				vectors[i].Hash = got
				return
			}
			if got != v.Hash {
				t.Errorf("hash = %s, want %s", got, v.Hash)
			}
		})
	}
	if *update {
		// Keep the requests as written; only the hashes change.
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(vectors); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHashRequestJSONIsCanonical(t *testing.T) {
	a := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"store":false,"temperature":1.0}`)
	b := []byte(" {\n \"temperature\": 1, \"store\": false,\n \"input\": [ {\"content\": [ {\"text\": \"hi\", \"type\": \"input_text\"} ], \"role\": \"user\", \"type\": \"message\"} ],\n \"model\": \"m\" }\n")
	ha, err := HashRequestJSON(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := HashRequestJSON(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("member order and whitespace changed the hash: %s vs %s", ha, hb)
	}
	if len(ha) != len(HashPrefix)+64 {
		t.Errorf("hash %q has the wrong length", ha)
	}
	if _, err := HashRequestJSON([]byte(`{"model":`)); err == nil {
		t.Error("malformed request hashed")
	}
}

// TestEntryHashesCanonical: the fast path Read takes for a canonical
// line gives the hashes EntryHashes gives, for every entry line of every
// fixture and for lines built to trip a slicer: brackets and quotes
// inside strings, escaped keys, and parents spelled empty or null.
func TestEntryHashesCanonical(t *testing.T) {
	var lines [][]byte
	paths, err := filepath.Glob(filepath.Join("testdata", "sessions", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if i > 0 && json.Valid(line) {
				lines = append(lines, line)
			}
		}
	}
	lines = append(lines,
		[]byte(`{"type":"custom","ns":"a","data":{"s":"}]\"{[","k\"ey":[1,{"x":"]"}]},"parent":null,"ts":"2026-09-17T16:00:01Z"}`),
		[]byte(`{"type":"info","name":"n","parents":[],"parent":"sha256:00","ts":"2026-09-17T16:00:01Z"}`),
		[]byte(`{"type":"info","name":"n","parents":null,"parent":"sha256:00","ts":"2026-09-17T16:00:01Z"}`),
		[]byte(`{"type":"info","name":"é😀","parents":[{"entry":"sha256:01"}],"parent":"sha256:00","ts":"2026-09-17T16:00:01Z"}`),
		[]byte(`{"type":"info","parent":null,"ts":"2026-09-17T16:00:01Z"}`),
	)
	if len(lines) < 50 {
		t.Fatalf("only %d lines; the test proves little", len(lines))
	}
	for _, line := range lines {
		wantID, wantContent, err := EntryHashes(line)
		if err != nil {
			continue // a negative fixture's line
		}
		c, err := jcs.Transform(line)
		if err != nil {
			t.Fatal(err)
		}
		id, content, err := entryHashesCanonical(c)
		if err != nil || id != wantID || content != wantContent {
			t.Errorf("canonical hashes %s %s %v, want %s %s\nline %s", id, content, err, wantID, wantContent, line)
		}
	}
}
