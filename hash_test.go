package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

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
