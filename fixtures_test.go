package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// The session fixtures under testdata/sessions are 0.5 files generated
// from the hand-written 0.4 sources under testdata/sessions/v0.4 by the
// library's own migration: each entry's id is its envelope hash and its
// readable old id is kept in legacy_id, which is how the tests name
// entries. `go test -update` regenerates them; the negative fixtures are
// built from the generated lines by breaking them in the one way each
// test expects.

// fixtureNames are the sources that read cleanly and regenerate as
// valid 0.5 files.
var fixtureNames = []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved", "instructions", "queued", "resume", "pinned", "converge", "bad-first-kept", "bad-records"}

// nestedFixtures are the fixtures a nested module's tests read, copied
// under that module's own testdata so its tests run from the published
// module, where the parent directory is not part of the artifact. The
// copies are written by TestRegenerateFixtures and checked against the
// originals by TestNestedModuleFixtures.
var nestedFixtures = map[string][]string{
	"otel": {"runs", "branch"},
}

// TestNestedModuleFixtures checks that every fixture copy a nested
// module carries is byte for byte the root's generated fixture. It
// skips outside the repository, where the nested modules are not
// present.
func TestNestedModuleFixtures(t *testing.T) {
	for module, names := range nestedFixtures {
		dir := filepath.Join(module, "testdata", "sessions")
		if _, err := os.Stat(filepath.Join(module, "go.mod")); err != nil {
			t.Skipf("%s is not beside this module: %v", module, err)
		}
		for _, name := range names {
			want, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(filepath.Join(dir, name+".jsonl"))
			if err != nil {
				t.Fatalf("%s: %v; run go test -update", module, err)
			}
			if !bytes.Equal(want, got) {
				t.Errorf("%s/testdata/sessions/%s.jsonl differs from the root fixture; run go test -update", module, name)
			}
		}
	}
}

func TestRegenerateFixtures(t *testing.T) {
	if !*update {
		t.Skip("run with -update to regenerate the 0.5 fixtures from the 0.4 sources")
	}
	gen := map[string][]string{}
	for _, name := range fixtureNames {
		src, err := os.Open(filepath.Join("testdata", "sessions", "v0.4", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		migrated, err := Read(src)
		src.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		// Replay into a fresh session so extension entries, which a
		// migration records as unresolved, are appended natively; the
		// hashes are the same, since the content and ts are.
		native := New(migrated.Header())
		for _, e := range migrated.Entries() {
			e.Base().ID = ""
			if _, err := native.Append(e); err != nil {
				t.Fatalf("%s: replay %s: %v", name, e.Base().LegacyID, err)
			}
		}
		var buf bytes.Buffer
		if err := Write(&buf, native); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		gen[name] = strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	}
	// fork: basic continued from its tool output in a session of its own,
	// with a different answer. The one native fixture, since a fork has
	// no 0.4 source.
	origin, err := Read(strings.NewReader(strings.Join(gen["basic"], "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	var forkAt string
	for _, e := range origin.Entries() {
		if e.Base().LegacyID == "i0000004" {
			forkAt = e.Base().ID
		}
	}
	created, _ := time.Parse(time.RFC3339, "2026-09-17T12:01:00Z")
	fork, err := Fork(origin, forkAt, Header{ID: "01995b2a-0000-7000-8000-000000000010", CreatedAt: created, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	if err != nil {
		t.Fatal(err)
	}
	answer := NewItemEntry(&openresponses.Message{ID: "msg_9", Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Four. The tool agreed, for what that was worth.", Annotations: []openresponses.Annotation{}}}})
	answer.ResponseID = "resp_9"
	answer.Timestamp = created.Add(4 * time.Second)
	if _, err := fork.Append(answer); err != nil {
		t.Fatal(err)
	}
	ctx, err := fork.ContextAt(forkAt)
	if err != nil {
		t.Fatal(err)
	}
	req, err := ctx.Request()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := RequestHash(req)
	if err != nil {
		t.Fatal(err)
	}
	resp := &ResponseEntry{ResponseID: "resp_9", Model: "gpt-5", Status: "completed", RequestHash: hash, LatencyMS: 350}
	resp.Timestamp = created.Add(4 * time.Second)
	if _, err := fork.Append(resp); err != nil {
		t.Fatal(err)
	}
	var fbuf bytes.Buffer
	if err := Write(&fbuf, fork); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "fork.jsonl"), fbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// normalised: the conformance vectors for writer-side normalisation,
	// appended natively since a source file carrying them could not be
	// read. 9007199254740993 in three spellings, once in a member the
	// profile types as a number, and a lone surrogate with its was.
	nat, _ := time.Parse(time.RFC3339, "2026-09-28T10:00:00Z")
	norm := New(Header{ID: "01995b2a-0000-7000-8000-000000000011", CreatedAt: nat, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	norm.setClock(func() time.Time { nat = nat.Add(time.Second); return nat })
	spellings := &InfoEntry{Name: "spellings"}
	spellings.Unknown = map[string]json.RawMessage{
		"acme:digits":   json.RawMessage(`9007199254740993`),
		"acme:fraction": json.RawMessage(`9007199254740993.0`),
		"acme:exponent": json.RawMessage(`9.007199254740993e15`),
	}
	if _, err := norm.Append(spellings); err != nil {
		t.Fatal(err)
	}
	if _, err := norm.Append(NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	if _, err := norm.Append(&ResponseEntry{ResponseID: "resp_1", Status: openresponses.ResponseStatusCompleted, LatencyMS: 9007199254740993}); err != nil {
		t.Fatal(err)
	}
	if _, err := norm.Append(&CustomEntry{NS: "acme", Data: json.RawMessage(`{"text":"cut \ud83d"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := norm.Append(NewItemEntry(openresponses.UserText("bytes \xe2\x82 end"))); err != nil {
		t.Fatal(err)
	}
	var nbuf bytes.Buffer
	if err := Write(&nbuf, norm); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "normalised.jsonl"), nbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	for module, names := range nestedFixtures {
		dir := filepath.Join(module, "testdata", "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range names {
			if err := os.WriteFile(filepath.Join(dir, name+".jsonl"), []byte(strings.Join(gen[name], "\n")+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	basic := gen["basic"]
	write := func(name string, lines []string) {
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// bad-parent: the third entry's parent is gone from the file.
	write("bad-parent", append(append([]string{}, basic[:3]...), basic[4:]...))
	// duplicate-id: one line twice; the reader keeps it once and reports.
	write("duplicate-id", append(append([]string{}, basic...), basic[2]))
	// corrupt-middle: line 3 does not parse and more lines follow.
	corrupt := append([]string{}, basic...)
	corrupt[2] = corrupt[2][:len(corrupt[2])/2]
	write("corrupt-middle", corrupt)
	// missing-id: an entry without its id.
	missing := append([]string{}, basic...)
	i := strings.Index(missing[2], `"id":"`)
	j := strings.Index(missing[2][i+6:], `"`) + i + 6 + 2 // past the closing quote and comma
	missing[2] = missing[2][:i] + missing[2][j:]
	write("missing-id", missing)
	// truncated: the last line cut short.
	trunc := append([]string{}, basic...)
	trunc[len(trunc)-1] = trunc[len(trunc)-1][:20]
	write("truncated", trunc)
	// unsupported-format: a header from another major version.
	write("unsupported-format", []string{`{"type":"session","format":"agentsession/1.0","id":"01995b2a-0000-7000-8000-0000000000aa","created_at":"2026-09-17T12:00:00Z","payload":"openresponses/2026-04-24"}`})
	// bad-parents: an entry converges one written later in the file.
	at, _ := time.Parse(time.RFC3339, "2026-09-25T09:00:00Z")
	s := New(Header{ID: "01995b2a-0000-7000-8000-00000000000d", CreatedAt: at})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	a := appendText(t, s, "a")
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	late := appendText(t, s, "written later")
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	early := NewItemEntry(openresponses.UserText("converges an entry not yet written"))
	early.Parents = []EntryRef{{Entry: late}}
	if _, err := s.Append(early); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	lines[2], lines[3] = lines[3], lines[2]
	write("bad-parents", lines)
}
