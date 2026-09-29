package agentsession

import (
	"bytes"
	"encoding/json"
	"fmt"
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
		"acme:pow60":    json.RawMessage(`1152921504606846976`),
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

	// parts: the 0.7 conformance vectors, appended natively since no
	// earlier source can hold them. A memory of many short facts whose
	// deltas keep runs of parts, a response that took retries, and a
	// workspace holding its host and instance.
	var pbuf bytes.Buffer
	if err := Write(&pbuf, partsFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "parts.jsonl"), pbuf.Bytes(), 0o644); err != nil {
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

// partsFixture builds the session parts.jsonl holds: three turns over a
// memory of twelve facts, the second patching one and the third
// removing one and adding another, each response carrying the hash of
// the request its context rebuilds.
func partsFixture(t *testing.T) *Session {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, "2026-09-29T10:00:00Z")
	s := New(Header{ID: "01995b2a-0000-7000-8000-000000000012", CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	env := &EnvEntry{CWD: "/home/u/proj"}
	w := env.SetWorkspace(WorkspaceContainer, "sha256:0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0")
	if err := w.SetMember("host", "build-7"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMember("instance", "ctr-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(env); err != nil {
		t.Fatal(err)
	}
	parts := memoryParts(12)
	turn := func(parts []InstructionPart, user string, attempts int) {
		t.Helper()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		cfg := ctx.Settings.InstructionsDelta(parts)
		if len(ctx.Settings.InstructionsParts) == 0 {
			if cfg, err = ConfigFromRequestParts(openresponses.Request{Model: "gpt-5", Instructions: JoinInstructions(parts)}, parts...); err != nil {
				t.Fatal(err)
			}
		}
		if cfg != nil {
			if _, err := s.Append(cfg); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Append(NewItemEntry(openresponses.UserText(user))); err != nil {
			t.Fatal(err)
		}
		if ctx, err = s.Context(); err != nil {
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
		id := fmt.Sprintf("resp_%d", s.Len())
		answer := NewItemEntry(&openresponses.Message{ID: "msg_" + id, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Noted.", Annotations: []openresponses.Annotation{}}}})
		answer.ResponseID = id
		if _, err := s.Append(answer); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(&ResponseEntry{ResponseID: id, Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: hash, Attempts: attempts}); err != nil {
			t.Fatal(err)
		}
	}
	turn(parts, "Remember the deploy window.", 0)
	parts = edit(parts, "m6", parts[6].Text+" (corrected)")
	turn(parts, "Correct fact six.", 2)
	// The container restarted from the same image: another instance,
	// so a substitution.
	restarted := &EnvEntry{CWD: "/home/u/proj"}
	w = restarted.SetWorkspace(WorkspaceContainer, env.Workspace.Ref)
	if err := w.SetMember("host", "build-7"); err != nil {
		t.Fatal(err)
	}
	if err := w.SetMember("instance", "ctr-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(restarted); err != nil {
		t.Fatal(err)
	}
	parts = append(append(append([]InstructionPart(nil), parts[:3]...), InstructionPart{ID: "m12", Source: "agentmemory", Text: "fact 012: the deploy window is Tuesday"}), parts[4:]...)
	turn(parts, "Forget fact three.", 0)
	return s
}

// TestPartsFixture reads the 0.7 conformance fixture: every request
// hash verifies through deltas that keep runs, the response that took
// retries says how many calls it took, and the workspace holds its host
// and instance, so the restart onto another instance is a substitution.
func TestPartsFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "parts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`{"keep":6}`, `"attempts":2`, `"host":"build-7"`, `"instance":"ctr-1"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("the fixture lacks %s", want)
		}
	}
	s := loadFixture(t, "parts")
	verified, calls := 0, 0
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("%s: %v", r.ResponseID, err)
			}
			verified++
			calls += r.Calls()
		}
	}
	if verified != 3 || calls != 4 {
		t.Errorf("%d responses verified over %d calls, want 3 over 4", verified, calls)
	}
	var envs []*EnvEntry
	for _, e := range s.Entries() {
		if v, ok := e.(*EnvEntry); ok {
			envs = append(envs, v)
		}
	}
	if len(envs) != 2 || SameWorkspace(envs[0].Workspace, envs[1].Workspace) {
		t.Errorf("%d env entries; the restart is not a substitution", len(envs))
	}
	var buf bytes.Buffer
	if err := Write(&buf, partsFixture(t)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), raw) {
		t.Error("parts.jsonl is not what partsFixture builds; run go test -update")
	}
}
