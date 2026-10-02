package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
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
	"otel": {"runs", "branch", "replay"},
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
	// The base rule's negative fixtures, each the fork's file with one
	// line re-parented and re-hashed, so every id verifies and the base
	// rule alone is what refuses them (#161). The fork's lines are the
	// header, the six-line prefix ending at the base, and two own lines.
	forkLines := strings.Split(strings.TrimSuffix(fbuf.String(), "\n"), "\n")
	root, own := lineID(t, forkLines[1]), forkLines[7]
	writeLines := func(name string, lines []string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// bad-base-root: an own entry that is a second root.
	writeLines("bad-base-root", append(append([]string{}, forkLines...), reparent(t, own, "")))
	// bad-base-parent: an own entry hung from the prefix above the base.
	writeLines("bad-base-parent", append(append([]string{}, forkLines...), reparent(t, own, root)))
	// bad-base-prefix: a line before the base that is not on the path to
	// it, a child of the root beside the one the prefix continues with.
	beside := append([]string{}, forkLines[:2]...)
	beside = append(beside, reparent(t, own, root))
	writeLines("bad-base-prefix", append(beside, forkLines[2:]...))

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

	// replay: the 0.8 conformance vectors, appended natively. A crash
	// with two calls in flight, one run again under its key and one
	// answered, omitted parts in force across a delta, carried by a
	// compaction and cleared, and a queued firing's own facts.
	var rbuf bytes.Buffer
	if err := Write(&rbuf, replayFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "replay.jsonl"), rbuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	gen["replay"] = strings.Split(strings.TrimSuffix(rbuf.String(), "\n"), "\n")

	// omitted: the 0.9 conformance vectors, appended natively. A memory
	// at its budget whose omitted list names runs of the list in force
	// by keep as facts are saved and forgotten, beside a replace and a
	// compaction's checkpoint that write it whole.
	var obuf bytes.Buffer
	if err := Write(&obuf, omittedFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "omitted.jsonl"), obuf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}

	// empty-resume and bad-resume: the 0.11 conformance vectors for a
	// resume that took up nothing, appended natively. The first holds a
	// refused resume and one cut and closed on restart, each followed by
	// the resume that took the call up; the second a resume that adds a
	// message and takes nothing up, which stays reported.
	for name, message := range map[string]bool{"empty-resume": false, "bad-resume": true} {
		var buf bytes.Buffer
		if err := Write(&buf, emptyResumeFixture(t, message)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// handback and bad-handback: the 0.11 conformance vectors for an
	// agent handed a session again, appended natively. The first names
	// the parts that left force by hash and the omitted list by of; the
	// second writes an of that names no entry on the path.
	for name, bad := range map[string]bool{"handback": false, "bad-handback": true} {
		var buf bytes.Buffer
		if err := Write(&buf, handbackFixture(t, bad)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// omit, omit-absent and bad-omit: the 0.11 conformance vectors for
	// the omit setting, appended natively. A session that switches
	// model and back under reasoning: the first writes omit and hashes
	// every response; the second is the same session as 0.10 writes
	// it, with no omit and no hash after the first switch; the third
	// records a hash over the request the omit in force says was not
	// sent.
	for name, variant := range map[string]switchVariant{"omit": switchOmit, "omit-absent": switchAbsent, "bad-omit": switchBad, "omit-folded": switchFolded} {
		var buf bytes.Buffer
		if err := Write(&buf, switchFixture(t, variant)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// judged: the 0.11 conformance vector for a judge's link, appended
	// natively: a session that answered a task and records the session
	// that judged it, and the entry the judgement is about.
	var jbuf bytes.Buffer
	if err := Write(&jbuf, judgedFixture(t)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", "sessions", "judged.jsonl"), jbuf.Bytes(), 0o644); err != nil {
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

// lineID returns the id an entry line carries.
func lineID(t *testing.T, line string) string {
	t.Helper()
	var env struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(line), &env); err != nil || env.ID == "" {
		t.Fatalf("line has no id: %v", err)
	}
	return env.ID
}

// reparent rewrites an entry line's parent, null for "", and its id to
// the hash the line then has, so a negative fixture breaks a link rule
// and nothing else.
func reparent(t *testing.T, line, parent string) string {
	t.Helper()
	var all map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &all); err != nil {
		t.Fatal(err)
	}
	all["parent"] = json.RawMessage("null")
	if parent != "" {
		all["parent"] = json.RawMessage(`"` + parent + `"`)
	}
	delete(all, "id")
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := EntryHashes(data)
	if err != nil {
		t.Fatal(err)
	}
	all["id"] = json.RawMessage(`"` + id + `"`)
	if data, err = json.Marshal(all); err != nil {
		t.Fatal(err)
	}
	if data, err = jcs.Transform(data); err != nil {
		t.Fatal(err)
	}
	return string(data)
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

// replayFixture builds the session replay.jsonl holds. A run hands two
// calls to their tools and is cut off; the next run closes it, runs
// deploy again under the key its first dispatch carried and answers
// notify, whose outcome is unknown, without running it; a nightly
// firing queued behind it, late and on its second attempt, drains into
// the run after. The omitted parts written with the first config stay
// in force across a delta that does not name them and a compaction,
// and a config carrying [] alone clears them.
func replayFixture(t *testing.T) *Session {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, "2026-09-29T11:00:00Z")
	s := New(Header{ID: "01995b2a-0000-7000-8000-000000000013", CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj", Records: []string{TypeRun, TypeDispatch, TypeDecision, TypeQueued}})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return id
	}
	hash := func() string {
		t.Helper()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		req, err := ctx.Request()
		if err != nil {
			t.Fatal(err)
		}
		h, err := RequestHash(req)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	reply := func(id, text string) {
		t.Helper()
		h := hash()
		answer := NewItemEntry(&openresponses.Message{ID: "msg_" + id, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: text, Annotations: []openresponses.Annotation{}}}})
		answer.ResponseID = id
		must(answer)
		must(&ResponseEntry{ResponseID: id, Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: h})
	}
	output := func(callID, text string) {
		t.Helper()
		must(NewItemEntry(&openresponses.FunctionCallOutput{CallID: callID, Output: openresponses.FunctionCallOutputData{Text: text}}))
	}

	parts := []InstructionPart{
		{ID: "product", Source: "product", Text: "You deploy services."},
		{ID: "memory", Source: "agentmemory", Text: "The deploy window is Tuesday."},
	}
	cfg, err := ConfigFromRequestParts(openresponses.Request{Model: "gpt-5", Instructions: JoinInstructions(parts)}, parts...)
	if err != nil {
		t.Fatal(err)
	}
	cfg.InstructionsOmitted = []OmittedPart{{ID: "memory/2026-08", Reason: "budget", Size: 4096, Source: "agentmemory"}}
	must(cfg)

	// Run 1 hands both calls over and is cut off.
	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Deploy billing and tell the channel.")))
	h := hash()
	deploy := &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_deploy", Name: "deploy", Arguments: `{"service":"billing"}`}, ResponseID: "resp_1"}
	deployID := must(deploy)
	notify := &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_2", CallID: "call_notify", Name: "notify", Arguments: `{"channel":"#ops"}`}, ResponseID: "resp_1"}
	notifyID := must(notify)
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: h})
	must(NewDispatch("call_deploy", deployID).WithIdempotencyKey("idem-deploy-1"))
	must(NewDispatch("call_notify", notifyID).WithIdempotencyKey("idem-notify-1"))

	// The next process closes the run the crash cut off, then resumes.
	end, err := s.EndRun(ReasonError, "cut: the process exited with two calls in flight")
	if err != nil {
		t.Fatal(err)
	}
	must(end)
	must(NewRunStart("run-2", SourceResume, ""))
	must(NewDecision("call_deploy", deployID, VerdictProceed, ByPolicy).WithReason("run again: keyed"))
	must(NewDispatch("call_deploy", deployID).WithIdempotencyKey("idem-deploy-1"))
	output("call_deploy", "deployed billing@4f2a")
	must(NewDecision("call_notify", notifyID, VerdictAnswer, ByPolicy).WithReason("replay unknown: notify cannot say a second run is safe"))
	output("call_notify", "outcome unknown: the call may have run before the restart")
	// The nightly firing arrives behind the busy run: due at 03:00,
	// taken at 09:00, its second attempt.
	firing := &Trigger{Kind: "schedule", Ref: "nightly", Source: "cron"}
	if err := firing.SetMember("due", "2026-09-29T03:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if err := firing.SetMember("attempt", 2); err != nil {
		t.Fatal(err)
	}
	q := NewQueued(openresponses.UserText("Run the nightly check."), ModeFollowUp)
	q.Trigger = firing
	must(q)
	// The memory moved; the delta does not name the omitted parts, so
	// they stay in force.
	parts[1].Text = "The deploy window is Tuesday. Billing is at 4f2a."
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	must(ctx.Settings.InstructionsDelta(parts))
	reply("resp_2", "Billing is deployed; whether #ops was told is unknown.")
	end, err = s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(end)

	// Run 3 drains the firing with its own facts, folds, and clears the
	// omitted parts.
	start := NewRunStart("run-3", SourceInput, "nightly")
	start.Trigger = firing.Clone()
	must(start)
	must(q.Drain())
	comp, err := s.CompactKeeping(1, openresponses.UserText("Billing was deployed at 4f2a; #ops may not have been told."))
	if err != nil {
		t.Fatal(err)
	}
	must(comp)
	must(&ConfigEntry{InstructionsOmitted: []OmittedPart{}})
	reply("resp_3", "The nightly check passed.")
	end, err = s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(end)
	return s
}

// TestReplayFixture reads the 0.8 conformance fixture: deploy was
// handed to its tool twice under one key, notify once and answered,
// every run end and request hash verifies, the omitted parts are in
// force until the config that clears them, the compaction carries them,
// and the queued firing's due time and attempt reach the item that
// drained it.
func TestReplayFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "replay.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"idempotency_key":"idem-deploy-1"`, `"verdict":"answer"`, `"instructions_omitted":[]`, `"attempt":2`, `"due":"2026-09-29T03:00:00Z"`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("the fixture lacks %s", want)
		}
	}
	s := loadFixture(t, "replay")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Error(err)
	}
	verified := 0
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("%s: %v", r.ResponseID, err)
			}
			verified++
		}
	}
	if verified != 3 {
		t.Errorf("%d responses verified, want 3", verified)
	}
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	deploy, notify := calls[0], calls[1]
	if len(deploy.Dispatches) != 2 || deploy.Dispatch != deploy.Dispatches[0] || deploy.IdempotencyKey() != "idem-deploy-1" || deploy.Answered() {
		t.Errorf("deploy: %d dispatches, key %q, answered %v", len(deploy.Dispatches), deploy.IdempotencyKey(), deploy.Answered())
	}
	if len(notify.Dispatches) != 1 || !notify.Answered() || notify.Rejected() || notify.IdempotencyKey() != "idem-notify-1" {
		t.Errorf("notify: %d dispatches, answered %v, key %q", len(notify.Dispatches), notify.Answered(), notify.IdempotencyKey())
	}
	var comp *CompactionEntry
	var cleared *ConfigEntry
	var drained *ItemEntry
	var delta *ConfigEntry
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *CompactionEntry:
			comp = v
		case *ConfigEntry:
			if v.InstructionsOmitted != nil && len(v.InstructionsOmitted) == 0 {
				cleared = v
			} else if v.Model == "" && len(v.InstructionsParts) > 0 {
				delta = v
			}
		case *ItemEntry:
			if v.QueuedFrom != "" {
				drained = v
			}
		}
	}
	if comp == nil || cleared == nil || drained == nil || delta == nil {
		t.Fatalf("compaction %v, clearing config %v, drained item %v, delta %v", comp != nil, cleared != nil, drained != nil, delta != nil)
	}
	omittedAt := func(id string) []OmittedPart {
		t.Helper()
		ctx, err := s.ContextAt(id)
		if err != nil {
			t.Fatal(err)
		}
		return ctx.InstructionsOmitted()
	}
	if got := omittedAt(delta.ID); len(got) != 1 || got[0].ID != "memory/2026-08" {
		t.Errorf("after a delta that does not name them, omitted = %v", got)
	}
	if len(comp.Config.InstructionsOmitted) != 1 {
		t.Errorf("the checkpoint carries %v", comp.Config.InstructionsOmitted)
	}
	if got := omittedAt(comp.ID); len(got) != 1 {
		t.Errorf("after the compaction, omitted = %v", got)
	}
	if got := omittedAt(cleared.ID); got != nil {
		t.Errorf("after [], omitted = %v", got)
	}
	var due string
	if drained.Source == nil || drained.Source.Kind != "schedule" || json.Unmarshal(drained.Source.Unknown["due"], &due) != nil || due != "2026-09-29T03:00:00Z" || string(drained.Source.Unknown["attempt"]) != "2" {
		t.Errorf("drained source = %+v", drained.Source)
	}
	runs, err := s.Runs(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || !runs[2].Start.Trigger.Equal(drained.Source) {
		t.Errorf("%d runs; run-3 trigger %+v", len(runs), runs[len(runs)-1].Start.Trigger)
	}
	var buf bytes.Buffer
	if err := Write(&buf, replayFixture(t)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), raw) {
		t.Error("replay.jsonl is not what replayFixture builds; run go test -update")
	}
}

// TestReplayFixtureNegative breaks the replay fixture in the ways the
// format forbids: a dispatch after the answer, refused on append and
// reported by VerifyRecords when a file carries one.
func TestReplayFixtureNegative(t *testing.T) {
	s := loadFixture(t, "replay")
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	notify := calls[1]
	if _, err := s.Append(NewDispatch(notify.ID(), notify.Entry.ID)); !errors.Is(err, ErrCallAnswered) {
		t.Errorf("dispatch after an answer: Append = %v, want ErrCallAnswered", err)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "replay.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	// Another dispatch of notify, hashed as a writer would, after the
	// last line.
	body := fmt.Sprintf(`{"type":"dispatch","call_id":%q,"target":%q,"parent":%q,"ts":"2026-09-29T12:00:00Z"}`, notify.ID(), notify.Entry.ID, s.Leaf())
	id, _, err := EntryHashes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	line := `{"id":"` + id + `",` + body[1:]
	broken, err := Read(strings.NewReader(strings.Join(append(lines, line), "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.VerifyRecords(broken.Leaf()); !errors.Is(err, ErrCallAnswered) {
		t.Errorf("VerifyRecords = %v, want ErrCallAnswered", err)
	}
}

// TestFrozen07Fixture reads parts.jsonl as v0.0.10 generated it, a
// file labelled agentsession/0.7, kept as released rather than
// regenerated: a 0.7 file reads as it stands, every entry keeps its id,
// the ids are those the 0.8 fixture holds, and every request hash
// verifies.
func TestFrozen07Fixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "v0.7", "parts.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"format":"agentsession/0.7"`)) {
		t.Fatal("the frozen fixture is not a 0.7 file")
	}
	old, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if migrated, _ := old.Migrated(); migrated {
		t.Error("a 0.7 file was migrated")
	}
	current := loadFixture(t, "parts")
	oe, ce := old.Entries(), current.Entries()
	if len(oe) != len(ce) {
		t.Fatalf("%d entries, the 0.8 fixture has %d", len(oe), len(ce))
	}
	for i := range oe {
		if oe[i].Base().ID != ce[i].Base().ID {
			t.Errorf("entry %d: id %s, the 0.8 fixture has %s", i, oe[i].Base().ID, ce[i].Base().ID)
		}
		if r, ok := oe[i].(*ResponseEntry); ok {
			if err := old.Verify(r.ID); err != nil {
				t.Errorf("%s: %v", r.ResponseID, err)
			}
		}
	}
}

// memoryFact is the nth fact of the omitted fixture's memory: shown as
// an instructions part, or left out as an omitted part.
func memoryFact(n int) (InstructionPart, OmittedPart) {
	id := fmt.Sprintf("memory/user/n-%04d", n)
	text := fmt.Sprintf("fact %04d: the user said something worth keeping", n)
	return InstructionPart{ID: id, Source: "agentmemory", Text: text}, OmittedPart{ID: id, Reason: "budget", Size: len(text), Source: "agentmemory"}
}

// omittedFixture builds the session omitted.jsonl holds: a memory of
// sixteen facts under a budget that shows four. A save that sorts into
// the shown facts pushes one out, written as that part and a keep; a
// forget of an omitted fact is a keep, the part after it, and a keep,
// in a config carrying no setting; a model switch replaces the
// settings and writes the list whole; a compaction's checkpoint
// carries it whole; and a save after the fold keeps a run of the
// checkpoint's list. Every response carries the hash of the request
// its context rebuilds, which the omitted list does not reach.
func omittedFixture(t *testing.T) *Session {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, "2026-09-29T13:00:00Z")
	s := New(Header{ID: "01995b2a-0000-7000-8000-000000000014", CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return id
	}
	product := InstructionPart{ID: "product", Source: "product", Text: "You are a helpful assistant with a memory."}
	var shownFacts, omittedFacts []int
	for n := 0; n < 16; n++ {
		if n < 4 {
			shownFacts = append(shownFacts, n)
		} else {
			omittedFacts = append(omittedFacts, n)
		}
	}
	render := func() ([]InstructionPart, []OmittedPart) {
		parts := []InstructionPart{product}
		for _, n := range shownFacts {
			p, _ := memoryFact(n)
			parts = append(parts, p)
		}
		var omitted []OmittedPart
		for _, n := range omittedFacts {
			_, o := memoryFact(n)
			omitted = append(omitted, o)
		}
		return parts, omitted
	}
	turn := func(model string, replace bool, user string) {
		t.Helper()
		parts, omitted := render()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		var cfg *ConfigEntry
		if replace {
			// A replace discards the list in force, so it carries the
			// list whole.
			if cfg, err = ConfigFromRequestParts(openresponses.Request{Model: model, Instructions: JoinInstructions(parts)}, parts...); err != nil {
				t.Fatal(err)
			}
			cfg.InstructionsOmitted = omitted
		} else {
			cfg = ctx.Settings.InstructionsDelta(parts)
			if d := ctx.Settings.OmittedDelta(omitted); d != nil {
				if cfg == nil {
					cfg = &ConfigEntry{}
				}
				cfg.InstructionsOmitted = d
			}
		}
		if cfg != nil {
			must(cfg)
		}
		must(NewItemEntry(openresponses.UserText(user)))
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
		must(answer)
		must(&ResponseEntry{ResponseID: id, Model: model, Status: openresponses.ResponseStatusCompleted, RequestHash: hash})
	}
	turn("gpt-5", true, "Hello again.")
	// A save that sorts into the shown facts pushes fact 3 out, to the
	// head of the omitted list: that part and a keep of twelve.
	shownFacts = []int{0, 1, 2, 16}
	omittedFacts = append([]int{3}, omittedFacts...)
	turn("gpt-5", false, "I moved to Bristol.")
	// A forget of omitted fact 9: nothing shown moves, so the config
	// carries the omitted list alone.
	omittedFacts = []int{3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15}
	turn("gpt-5", false, "Forget fact nine.")
	// A model switch replaces the settings, and writes the list whole.
	turn("gpt-5-mini", true, "Switch to the smaller model.")
	comp, err := s.CompactKeeping(1, openresponses.UserText("The user moved to Bristol and switched models."))
	if err != nil {
		t.Fatal(err)
	}
	must(comp)
	// After the fold a save pushes fact 2 out: a keep of the
	// checkpoint's list.
	shownFacts = []int{0, 1, 16, 17}
	omittedFacts = append([]int{2}, omittedFacts...)
	turn("gpt-5-mini", false, "My sister is called Ada.")
	return s
}

// TestOmittedFixture reads the 0.9 conformance fixture: every request
// hash verifies, the omitted list in force after each config is the
// one the writer rendered, a delta writes runs of the list in force as
// keeps, and a replace and the checkpoint write it whole.
func TestOmittedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "omitted.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"format":"` + Format + `"`, `{"keep":12}`, `[{"keep":6},{"id":"memory/user/n-0010"`, `{"keep":5}]`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("the fixture lacks %s", want)
		}
	}
	s := loadFixture(t, "omitted")
	verified := 0
	var configs []*ConfigEntry
	var comp *CompactionEntry
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *ResponseEntry:
			if err := s.Verify(v.ID); err != nil {
				t.Errorf("%s: %v", v.ResponseID, err)
			}
			verified++
		case *ConfigEntry:
			configs = append(configs, v)
		case *CompactionEntry:
			comp = v
		}
	}
	if verified != 5 || len(configs) != 5 || comp == nil {
		t.Fatalf("%d responses verified, %d configs, compaction %v", verified, len(configs), comp != nil)
	}
	facts := func(ns ...int) string {
		ids := make([]string, 0, len(ns))
		for _, n := range ns {
			_, o := memoryFact(n)
			ids = append(ids, o.ID)
		}
		return strings.Join(ids, ",")
	}
	seq := func(from, to int) []int {
		var out []int
		for n := from; n <= to; n++ {
			out = append(out, n)
		}
		return out
	}
	after9 := []int{3, 4, 5, 6, 7, 8, 10, 11, 12, 13, 14, 15}
	for i, want := range []string{
		facts(seq(4, 15)...),
		facts(seq(3, 15)...),
		facts(after9...),
		facts(after9...),
		facts(append([]int{2}, after9...)...),
	} {
		ctx, err := s.ContextAt(configs[i].ID)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, o := range ctx.InstructionsOmitted() {
			if o.Unresolved() {
				t.Errorf("config %d: an unresolved element in force", i)
			}
			if _, full := memoryFact(0); o.Reason != full.Reason || o.Source != full.Source || o.Size == 0 {
				t.Errorf("config %d: %s lost a member: %+v", i, o.ID, o)
			}
			got = append(got, o.ID)
		}
		if strings.Join(got, ",") != want {
			t.Errorf("config %d: omitted in force %v, want %s", i, got, want)
		}
	}
	whole := func(what string, list []OmittedPart) {
		t.Helper()
		for _, o := range list {
			if o.Keep != 0 || o.ID == "" {
				t.Errorf("%s carries %+v, want the list whole", what, o)
			}
		}
	}
	if !configs[3].Replace {
		t.Error("the fourth config is not a replace")
	}
	whole("the replace", configs[3].InstructionsOmitted)
	whole("the checkpoint", comp.Config.InstructionsOmitted)
	if len(configs[2].InstructionsParts) != 0 || configs[2].Model != "" {
		t.Errorf("the forget's config carries settings: %+v", configs[2])
	}
	var buf bytes.Buffer
	if err := Write(&buf, omittedFixture(t)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), raw) {
		t.Error("omitted.jsonl is not what omittedFixture builds; run go test -update")
	}
}

// TestFrozen08Fixture reads replay.jsonl as v0.0.11 generated it, a
// file labelled agentsession/0.8, kept as released rather than
// regenerated: a 0.8 file reads as it stands, every entry keeps its id,
// the ids are those the 0.9 fixture holds, every request hash verifies
// and the omitted list in force is the one the 0.9 fixture reads.
func TestFrozen08Fixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "v0.8", "replay.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"format":"agentsession/0.8"`)) {
		t.Fatal("the frozen fixture is not a 0.8 file")
	}
	old, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if migrated, _ := old.Migrated(); migrated {
		t.Error("a 0.8 file was migrated")
	}
	current := loadFixture(t, "replay")
	oe, ce := old.Entries(), current.Entries()
	if len(oe) != len(ce) {
		t.Fatalf("%d entries, the 0.9 fixture has %d", len(oe), len(ce))
	}
	for i := range oe {
		id := oe[i].Base().ID
		if id != ce[i].Base().ID {
			t.Errorf("entry %d: id %s, the 0.9 fixture has %s", i, id, ce[i].Base().ID)
			continue
		}
		if r, ok := oe[i].(*ResponseEntry); ok {
			if err := old.Verify(r.ID); err != nil {
				t.Errorf("%s: %v", r.ResponseID, err)
			}
		}
		octx, err1 := old.ContextAt(id)
		cctx, err2 := current.ContextAt(id)
		if err1 != nil || err2 != nil {
			t.Fatalf("entry %d: %v, %v", i, err1, err2)
		}
		if !slices.Equal(octx.InstructionsOmitted(), cctx.InstructionsOmitted()) {
			t.Errorf("entry %d: omitted %v, the 0.9 fixture reads %v", i, octx.InstructionsOmitted(), cctx.InstructionsOmitted())
		}
	}
}

// emptyResumeFixture builds the session empty-resume.jsonl holds, or
// with message set bad-resume.jsonl. A charge call is held at the end of
// the first run. The second run is written resume, and a subscriber
// refuses it before it takes the call up; the third is written resume
// too and is cut after its start, and the restart closes it error; the
// fourth takes the call up. With message set the second run instead
// adds a user message, is answered, and takes nothing up: the shape of
// an input written resume, which VerifyRecords reports.
func emptyResumeFixture(t *testing.T, message bool) *Session {
	t.Helper()
	id := "01995b2a-0000-7000-8000-000000000016"
	if message {
		id = "01995b2a-0000-7000-8000-000000000017"
	}
	at, _ := time.Parse(time.RFC3339, "2026-10-01T09:00:00Z")
	s := New(Header{ID: id, CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj", Records: AllRecords})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		got, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return got
	}
	end := func(reason, ref string) {
		t.Helper()
		e, err := s.EndRun(reason, ref)
		if err != nil {
			t.Fatal(err)
		}
		must(e)
	}
	// hash is the request the context at the leaf rebuilds, which the
	// next response's request_hash must be.
	hash := func() string {
		t.Helper()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		req, err := ctx.Request()
		if err != nil {
			t.Fatal(err)
		}
		h, err := RequestHash(req)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	say := func(responseID, text string) {
		t.Helper()
		sent := hash()
		answer := NewItemEntry(&openresponses.Message{ID: "msg_" + responseID, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: text, Annotations: []openresponses.Annotation{}}}})
		answer.ResponseID = responseID
		must(answer)
		must(&ResponseEntry{ResponseID: responseID, Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	}
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Charge the card.")))
	sent := hash()
	call := must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_charge", Name: "charge", Arguments: "{}"}, ResponseID: "resp_1"})
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	must(NewDecision("call_charge", call, VerdictHold, ByPolicy))
	end(ReasonInputRequired, "")
	must(NewRunStart("run-2", SourceResume, ""))
	if message {
		must(NewItemEntry(openresponses.UserText("Also, which card is it?")))
		say("resp_2", "The one ending 4242.")
		end(ReasonDone, "")
		return s
	}
	end(ReasonError, "a run_start subscriber refused the run")
	must(NewRunStart("run-3", SourceResume, ""))
	end(ReasonError, "the harness stopped before the run took anything up")
	must(NewRunStart("run-4", SourceResume, ""))
	must(NewDecision("call_charge", call, VerdictProceed, ByPolicy))
	must(NewDispatch("call_charge", call))
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_charge", "charged")))
	say("resp_2", "Charged.")
	end(ReasonDone, "")
	return s
}

// TestEmptyResumeFixture reads the 0.11 conformance fixtures: a file
// holding a refused resume and a cut one, each written resume over a
// segment that holds nothing, verifies, in a file of 0.9 and 0.10 too;
// one holding a resume that adds a message and takes nothing up is still
// reported (#172).
func TestEmptyResumeFixture(t *testing.T) {
	for name, tt := range map[string]struct {
		message bool
		want    error
	}{
		"empty-resume": {false, nil},
		"bad-resume":   {true, ErrSourceMismatch},
	} {
		raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := Write(&buf, emptyResumeFixture(t, tt.message)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), raw) {
			t.Errorf("%s.jsonl is not what emptyResumeFixture builds; run go test -update", name)
		}
		// The relaxation is the reader's, so the file reads the same
		// whatever minor its header declares.
		for _, minor := range []string{Format, "agentsession/0.10", "agentsession/0.9"} {
			s, err := Read(bytes.NewReader(bytes.Replace(raw, []byte(`"format":"`+Format+`"`), []byte(`"format":"`+minor+`"`), 1)))
			if err != nil {
				t.Fatalf("%s as %s: %v", name, minor, err)
			}
			if s.DeclaredFormat() != minor {
				t.Errorf("%s declares %s, want %s", name, s.DeclaredFormat(), minor)
			}
			for _, leaf := range s.Leaves() {
				if err := s.VerifyRecords(leaf); !errors.Is(err, tt.want) {
					t.Errorf("%s as %s: VerifyRecords = %v, want %v", name, minor, err, tt.want)
				}
			}
			for _, e := range s.Entries() {
				if r, ok := e.(*ResponseEntry); ok {
					if err := s.Verify(r.ID); err != nil {
						t.Errorf("%s as %s: %s: %v", name, minor, r.ResponseID, err)
					}
				}
			}
		}
	}
	s := loadFixture(t, "empty-resume")
	var empty int
	runs, err := s.Runs(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if r.Start.Source == SourceResume && r.Empty() {
			empty++
		}
		if err := r.Verify(); err != nil {
			t.Errorf("run %s: %v", r.RunID(), err)
		}
	}
	if empty != 2 || len(runs) != 4 {
		t.Errorf("%d empty resumes in %d runs, want 2 in 4", empty, len(runs))
	}
}

// judgedFixture builds the session judged.jsonl holds: one answered
// task, and two judged_by links written after it, the first naming the
// entry the judgement is about and the second a judge that named none.
func judgedFixture(t *testing.T) *Session {
	t.Helper()
	at, _ := time.Parse(time.RFC3339, "2026-10-01T11:00:00Z")
	s := New(Header{ID: "01995b2a-0000-7000-8000-000000000018", CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return id
	}
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	must(NewItemEntry(openresponses.UserText("What is 2+2?")))
	answer := NewItemEntry(&openresponses.Message{ID: "msg_1", Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Four.", Annotations: []openresponses.Annotation{}}}})
	answer.ResponseID = "resp_1"
	last := must(answer)
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted})
	must(NewJudgedByLink(SubsessionID("01995b2a-0000-7000-8000-000000000018", "judge-1"), last))
	must(NewJudgedByLink(SubsessionID("01995b2a-0000-7000-8000-000000000018", "judge-2"), ""))
	return s
}

// TestJudgedFixture reads the 0.11 conformance fixture: the judged_by
// links are read back with the target they name, which is an entry on
// the path, and the file is what the builder writes.
func TestJudgedFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "judged.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, judgedFixture(t)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), raw) {
		t.Error("judged.jsonl is not what judgedFixture builds; run go test -update")
	}
	if !bytes.Contains(raw, []byte(`"rel":"judged_by"`)) || !bytes.Contains(raw, []byte(`"target":"sha256:`)) {
		t.Error("the fixture lacks a judged_by link with its target")
	}
	s := loadFixture(t, "judged")
	judges, err := s.Judges(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(judges) != 2 {
		t.Fatalf("%d judges, want 2", len(judges))
	}
	if _, ok := s.Entry(judges[0].Target); !ok || len(s.Path(judges[0].Target)) == 0 {
		t.Errorf("the first judge's target %q is not an entry of the session", judges[0].Target)
	}
	if judges[1].Target != "" || judges[0].Session == judges[1].Session {
		t.Errorf("judges = %+v, %+v", judges[0], judges[1])
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
}

// handbackFixture builds the session handback.jsonl holds: a triage
// agent A with a memory of twenty facts, sixteen of them omitted by
// budget, handed to a billing agent B and back, five turns in all.
//
//  1. A runs, writing its parts and its omitted list whole.
//  2. B replaces A's parts and omitted list with its own, in a delta.
//  3. A is handed the session back, and has saved a fact: its parts
//     are named by hash, the one new fact by text, and its omitted
//     list is the new fact's element and a keep of sixteen over the
//     list entry 1 wrote.
//  4. B again.
//  5. A again, unchanged: its omitted list is one keep over the list
//     entry 3 resolved to, and its parts are named by hash.
//
// Every response carries the hash of the request its context rebuilds.
// With bad set, entry 5 names an entry that is not on the path.
func handbackFixture(t *testing.T, bad bool) *Session {
	t.Helper()
	id := "01995b2a-0000-7000-8000-000000000019"
	if bad {
		id = "01995b2a-0000-7000-8000-00000000001a"
	}
	at, _ := time.Parse(time.RFC3339, "2026-10-01T12:00:00Z")
	s := New(Header{ID: id, CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		got, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return got
	}
	factParts := func(shown []int) []InstructionPart {
		var parts []InstructionPart
		for _, n := range shown {
			p, _ := memoryFact(n)
			parts = append(parts, p)
		}
		return parts
	}
	factsOmitted := func(ns ...int) []OmittedPart {
		var list []OmittedPart
		for _, n := range ns {
			_, o := memoryFact(n)
			list = append(list, o)
		}
		return list
	}
	seq := func(from, to int) []int {
		var out []int
		for n := from; n <= to; n++ {
			out = append(out, n)
		}
		return out
	}
	agentA := func(shown []int) []InstructionPart {
		return append([]InstructionPart{{ID: "product", Source: "product", Text: "You are the triage agent."}, {ID: "agentsmd", Source: "agentsmd", Text: "Route billing questions to billing."}}, factParts(shown)...)
	}
	agentB := []InstructionPart{
		{ID: "product", Source: "product", Text: "You are the billing agent."},
		{ID: "skills", Source: "agentskill", Text: "refund: issue a refund\ninvoice: find an invoice"},
	}
	omittedB := []OmittedPart{
		{ID: "skills/dispute", Reason: "budget", Size: 120, Source: "agentskill"},
		{ID: "skills/audit", Reason: "budget", Size: 90, Source: "agentskill"},
	}
	turn := func(n int, model string, parts []InstructionPart, omitted []OmittedPart, user string) {
		t.Helper()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		var cfg *ConfigEntry
		if n == 1 {
			if cfg, err = ConfigFromRequestParts(openresponses.Request{Model: model, Instructions: JoinInstructions(parts)}, parts...); err != nil {
				t.Fatal(err)
			}
			cfg.InstructionsOmitted = omitted
		} else {
			cfg = ctx.Settings.InstructionsDelta(parts)
			if cfg == nil {
				cfg = &ConfigEntry{}
			}
			if model != ctx.Settings.Model {
				cfg.Model = model
			}
			if d := ctx.Settings.OmittedDelta(omitted); d != nil {
				cfg.InstructionsOmitted = d
			}
		}
		if n == 5 && bad {
			cfg.InstructionsOmitted = []OmittedPart{{Keep: len(omitted), Of: "sha256:" + strings.Repeat("0", 64)}}
		}
		must(cfg)
		must(NewItemEntry(openresponses.UserText(user)))
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
		respID := fmt.Sprintf("resp_%d", n)
		answer := NewItemEntry(&openresponses.Message{ID: "msg_" + respID, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Noted.", Annotations: []openresponses.Annotation{}}}})
		answer.ResponseID = respID
		must(answer)
		must(&ResponseEntry{ResponseID: respID, Model: model, Status: openresponses.ResponseStatusCompleted, RequestHash: hash})
	}
	turn(1, "gpt-5", agentA(seq(0, 3)), factsOmitted(seq(4, 19)...), "I was charged twice.")
	turn(2, "gpt-5-mini", agentB, omittedB, "Please refund the second charge.")
	// A saved fact 20 while B ran: it sorts into the shown facts and
	// pushes fact 3 out, to the head of the omitted list.
	turn(3, "gpt-5", agentA([]int{0, 1, 2, 20}), factsOmitted(append([]int{3}, seq(4, 19)...)...), "Thanks. What about my other card?")
	turn(4, "gpt-5-mini", agentB, omittedB, "Which invoice was it?")
	turn(5, "gpt-5", agentA([]int{0, 1, 2, 20}), factsOmitted(append([]int{3}, seq(4, 19)...)...), "Back to triage.")
	return s
}

// A switchVariant is the way switchFixture writes its session.
type switchVariant int

const (
	// switchOmit writes omit and hashes every response.
	switchOmit switchVariant = iota
	// switchAbsent writes as 0.10 does: no omit, and a response whose
	// request left items out carries no hash.
	switchAbsent
	// switchBad writes omit, and hashes the third response over the
	// request with the other model's reasoning in it.
	switchBad
	// switchFolded writes omit, folds the context, and switches model
	// again: the checkpoint carries the omit, and the reasoning the fold
	// kept is left out of the request to the other model.
	switchFolded
)

// switchFixture builds the session omit.jsonl holds, or with another
// variant omit-absent.jsonl or bad-omit.jsonl. One conversation, four
// requests, reasoning in each response:
//
//  1. gpt-5 answers, and reasons. Its response entry names the model
//     under a dated snapshot name, as a provider does.
//  2. The session switches to gpt-5-mini, and the config entry that
//     changes the model writes omit reasoning other_models: the request
//     leaves out gpt-5's reasoning, which gpt-5-mini would refuse.
//  3. The session switches back and the config writes the rule again,
//     which changes nothing: gpt-5's reasoning is in the request, and
//     gpt-5-mini's is out.
//  4. A host drops gpt-5-mini's message by listing its entry in omit
//     items, beside the rule still in force.
func switchFixture(t *testing.T, variant switchVariant) *Session {
	t.Helper()
	id := map[switchVariant]string{
		switchOmit:   "01995b2a-0000-7000-8000-00000000001b",
		switchAbsent: "01995b2a-0000-7000-8000-00000000001c",
		switchBad:    "01995b2a-0000-7000-8000-00000000001d",
		switchFolded: "01995b2a-0000-7000-8000-00000000001e",
	}[variant]
	at, _ := time.Parse(time.RFC3339, "2026-10-01T14:00:00Z")
	s := New(Header{ID: id, CreatedAt: at, Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	s.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must := func(e Entry) string {
		t.Helper()
		got, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %T: %v", e, err)
		}
		return got
	}
	// sent is the hash of the request the next call is sent: what the
	// context at the leaf rebuilds, or, with bare set, what it would
	// rebuild had nothing been left out.
	sent := func(bare bool) string {
		t.Helper()
		ctx, err := buildContext(s.Path(s.Leaf()), !bare)
		if err != nil {
			t.Fatal(err)
		}
		req, err := ctx.Request()
		if err != nil {
			t.Fatal(err)
		}
		h, err := RequestHash(req)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// call appends the user message, then the output of the call and
	// its response, which carries the hash unless the writer cannot
	// stand behind one. It returns the message's entry.
	call := func(n int, model, snapshot, user string, hash func() string) (message string) {
		t.Helper()
		must(NewItemEntry(openresponses.UserText(user)))
		h := hash()
		respID := fmt.Sprintf("resp_%d", n)
		reasoning := NewItemEntry(&openresponses.ReasoningItem{ID: fmt.Sprintf("rs_%d", n), Summary: openresponses.Contents{&openresponses.SummaryText{Text: "thinking about " + user}}, EncryptedContent: fmt.Sprintf("enc-%s-%d", model, n)})
		reasoning.ResponseID = respID
		must(reasoning)
		answer := NewItemEntry(&openresponses.Message{ID: "msg_" + respID, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Answer " + respID + ".", Annotations: []openresponses.Annotation{}}}})
		answer.ResponseID = respID
		message = must(answer)
		must(&ResponseEntry{ResponseID: respID, Model: snapshot, Status: openresponses.ResponseStatusCompleted, RequestHash: h})
		return message
	}
	hashed := func() string { return sent(false) }
	// afterSwitch is the hash a writer of 0.10 could record for a request
	// that left items out of what the path shows: none.
	afterSwitch := func() string {
		if variant == switchAbsent {
			return ""
		}
		return sent(false)
	}
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	call(1, "gpt-5", "gpt-5-2026-08-07", "Plan the migration.", hashed)

	switch1 := &ConfigEntry{Model: "gpt-5-mini"}
	if variant != switchAbsent {
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		d, ok := ctx.Settings.OmitDelta(Omit{Reasoning: OmitOtherModels})
		if !ok || d == nil {
			t.Fatalf("OmitDelta = %v, %v", d, ok)
		}
		switch1.Omit = d
	}
	must(switch1)
	miniMessage := call(2, "gpt-5-mini", "gpt-5-mini-2026-08-07", "Now apply step one.", afterSwitch)

	switch2 := &ConfigEntry{Model: "gpt-5"}
	if variant != switchAbsent {
		// A writer that states the rule at every switch writes it again;
		// it changes nothing, and so does not make the entry a delta.
		switch2.Omit = &Omit{Reasoning: OmitOtherModels}
	}
	must(switch2)
	third := afterSwitch
	if variant == switchBad {
		third = func() string { return sent(true) }
	}
	call(3, "gpt-5", "gpt-5-2026-08-07", "Back to the first model: summarise.", third)

	if variant != switchAbsent {
		must(&ConfigEntry{Omit: &Omit{Items: []string{miniMessage}}})
	}
	call(4, "gpt-5", "gpt-5-2026-08-07", "Anything else?", afterSwitch)
	if variant == switchFolded {
		// The request holds ten items, two of gpt-5's reasoning among
		// them in the last six, which the fold keeps. The checkpoint
		// carries the rule and the listed entry.
		comp, err := s.CompactKeeping(6, openresponses.UserText("Summary of the migration so far."))
		if err != nil {
			t.Fatal(err)
		}
		must(comp)
		must(&ConfigEntry{Model: "gpt-5-mini"})
		call(5, "gpt-5-mini", "gpt-5-mini-2026-08-07", "Finish the migration.", hashed)
	}
	return s
}

// TestOmitFixtures reads the 0.11 conformance fixtures for the omit
// setting: with the rule written at each switch every response's hash
// verifies, though the requests leave out another model's reasoning and
// a host's listed message; the same session as 0.10 writes it verifies
// the first response alone; and a hash over the request with the items
// in fails as a divergence from the rule, not as a response with no
// hash.
func TestOmitFixtures(t *testing.T) {
	for name, variant := range map[string]switchVariant{"omit": switchOmit, "omit-absent": switchAbsent, "bad-omit": switchBad, "omit-folded": switchFolded} {
		raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := Write(&buf, switchFixture(t, variant)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf.Bytes(), raw) {
			t.Errorf("%s.jsonl is not what switchFixture builds; run go test -update", name)
		}
		s := loadFixture(t, name)
		var results []string
		for _, e := range s.Entries() {
			r, ok := e.(*ResponseEntry)
			if !ok {
				continue
			}
			switch err := s.Verify(r.ID); {
			case err == nil:
				results = append(results, "ok")
			case errors.Is(err, ErrNoHash):
				results = append(results, "unhashed")
			case errors.Is(err, ErrOmitDivergence) && errors.Is(err, ErrHashMismatch):
				results = append(results, "divergence")
			case errors.Is(err, ErrHashMismatch):
				results = append(results, "mismatch")
			default:
				t.Fatalf("%s: %s: %v", name, r.ResponseID, err)
			}
		}
		want := map[string]string{
			"omit":        "ok ok ok ok",
			"omit-absent": "ok unhashed unhashed unhashed",
			"bad-omit":    "ok ok divergence ok",
			"omit-folded": "ok ok ok ok ok",
		}[name]
		if got := strings.Join(results, " "); got != want {
			t.Errorf("%s: responses %s, want %s", name, got, want)
		}
	}

	// What the first fixture's requests leave out, and why.
	s := loadFixture(t, "omit")
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, o := range ctx.OmittedItems {
		left = append(left, o.Reason+" "+o.Entry.Item.ItemType())
	}
	if got := strings.Join(left, ","); got != "other_models reasoning,items message" {
		t.Errorf("the leaf's request leaves out %s", got)
	}
	if ctx.Settings.Omit.Reasoning != OmitOtherModels || len(ctx.Settings.Omit.Items) != 1 {
		t.Errorf("omit in force: %+v", ctx.Settings.Omit)
	}
	// 4 users, 4 answers' worth less one listed message, 4 reasoning
	// items less gpt-5-mini's.
	if len(ctx.Items) != 4+3+3 || len(ctx.Items) != len(ctx.ItemEntries) {
		t.Errorf("the request holds %d items", len(ctx.Items))
	}
	// After a fold the checkpoint carries the omit, and the request to
	// the other model still leaves out gpt-5's reasoning the fold kept.
	folded := loadFixture(t, "omit-folded")
	var comp *CompactionEntry
	for _, e := range folded.Entries() {
		if c, ok := e.(*CompactionEntry); ok {
			comp = c
		}
	}
	if comp == nil || comp.Config.Omit.Reasoning != OmitOtherModels || len(comp.Config.Omit.Items) != 1 {
		t.Fatalf("the checkpoint carries %+v", comp)
	}
	fctx, err := folded.Context()
	if err != nil {
		t.Fatal(err)
	}
	var fleft []string
	for _, o := range fctx.OmittedItems {
		fleft = append(fleft, o.Reason+" "+o.Entry.Item.ItemType())
	}
	if got := strings.Join(fleft, ","); got != "other_models reasoning,other_models reasoning" {
		t.Errorf("after the fold the request leaves out %s", got)
	}

	// A path that ends before the switch never meets the member: the
	// first request is as written, and no later entry leaves anything
	// out of it.
	var firstResp string
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			firstResp = r.ID
			break
		}
	}
	before, err := s.ContextAt(firstResp)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.OmittedItems) != 0 || !before.Settings.Omit.IsZero() {
		t.Errorf("the path to the first response leaves out %d items under %+v", len(before.OmittedItems), before.Settings.Omit)
	}
}
