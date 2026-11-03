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

// fixtures017 are the 0.17 conformance fixtures, each built natively by
// its function, as for fixtures016.
var fixtures017 = map[string]func(t *testing.T) *Session{
	"instruction-layers": func(t *testing.T) *Session { return instructionLayersFixture(t, true) },
}

// badFixtures017 are the 0.17 negative fixtures: each a valid file whose
// lines a reader keeps a member of as one it does not know.
var badFixtures017 = map[string]func(t *testing.T) []byte{
	"bad-instructions-part": badInstructionsPartLines,
}

// writeFixtures017 writes the 0.17 fixtures, positive and negative.
func writeFixtures017(t *testing.T) {
	t.Helper()
	for name, build := range fixtures017 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, build := range badFixtures017 {
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), build(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkBad017 fails the test when testdata/sessions/<name>.jsonl is not
// what its 0.17 builder writes, and returns the session it holds.
func checkBad017(t *testing.T, name string) *Session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, badFixtures017[name](t)) {
		t.Errorf("%s.jsonl is not what its builder writes; run go test -update", name)
	}
	return loadFixture(t, name)
}

// claudeMD is the instruction file the instruction-layers fixture sends
// as a message, wrapped as the harness wraps it.
const claudeMD = "<system-reminder>Contents of /repo/CLAUDE.md:\n\nUse tabs.</system-reminder>"

// instructionLayersFixture builds the session instruction-layers.jsonl
// holds: a config composing a product prompt, two instruction files by
// path under agentsmd, a skill catalogue and a memory part, with a third
// instruction file omitted for its size; a message the harness composed
// from a fourth instruction file, carrying instructions_part, then the
// person's input and the model's answer, whose request_hash verifies.
// Without marked the message carries no instructions_part, which is the
// same session as the model saw it.
func instructionLayersFixture(t *testing.T, marked bool) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000042", Records: AllRecords}, "2026-11-02T12:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", InstructionsParts: []InstructionPart{
		{ID: "product", Source: LayerProduct, Text: "You are a coding agent."},
		{ID: "/repo/AGENTS.md", Source: LayerAgentsMD, Text: "Run make check before you commit."},
		{ID: "/repo/service/AGENTS.md", Source: LayerAgentsMD, Text: "The service speaks gRPC."},
		{ID: "skills", Source: LayerAgentSkill, Text: "review: review a diff"},
		{ID: "memory", Source: LayerAgentMemory, Text: "The user prefers short answers."},
	}, InstructionsOmitted: []OmittedPart{{ID: "/repo/docs/AGENTS.md", Reason: "budget", Size: 9000, Source: LayerAgentsMD}}})
	must(NewRunStart("run-1", SourceInput, ""))
	reminder := &ItemEntry{Item: openresponses.UserText(claudeMD)}
	if marked {
		reminder.InstructionsPart = &LayerRef{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}
	}
	must(reminder)
	must(NewItemEntry(openresponses.UserText("Fix the build.")))
	say("resp_1", "Fixed: the Makefile named a missing target.")
	e, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(e)
	return s
}

// badInstructionsPartLines builds bad-instructions-part.jsonl: three
// messages and an output, each carrying an instructions_part a writer of
// 0.17 does not write: on an output, with no source, naming agentsmd
// with no id. A reader keeps each as a member it does not know, so each
// moves the context hash and none is told from a person's input.
func badInstructionsPartLines(t *testing.T) []byte {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000043"}, "2026-11-02T13:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	unknown := func(item openresponses.Item, member string) *ItemEntry {
		e := NewItemEntry(item)
		e.Unknown = map[string]json.RawMessage{"instructions_part": json.RawMessage(member)}
		return e
	}
	must(unknown(openresponses.UserText("<memory>none</memory>"), `{"id":"memory"}`))
	must(unknown(openresponses.UserText(claudeMD), `{"source":"agentsmd"}`))
	must(NewItemEntry(openresponses.UserText("Read the file.")))
	sent := fixtureHash(t, s)
	must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "read", Arguments: `{}`}, ResponseID: "resp_1"})
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	must(unknown(openresponses.NewFunctionCallOutput("call_1", "Use tabs."), `{"id":"/repo/AGENTS.md","source":"agentsmd"}`))
	say("resp_2", "Done.")
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInstructionsPartForms: instructions_part is taken in the form 0.17
// defines on a message's entry, in a file of any minor, and kept as
// written otherwise, and written back as read either way.
func TestInstructionsPartForms(t *testing.T) {
	message := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	output := `{"type":"function_call_output","call_id":"call_1","output":"hi"}`
	for _, tt := range []struct {
		item, member string
		typed        bool
	}{
		{message, `{"id":"/repo/AGENTS.md","source":"agentsmd"}`, true},
		{message, `{"source":"agentmemory"}`, true},
		{message, `{"id":"notes","source":"x:notes"}`, true},
		{message, `{"id":"/repo/AGENTS.md","source":"agentsmd","x:wrapped":"reminder"}`, true},
		{message, `{"source":"agentsmd"}`, false},
		{message, `{"id":"memory"}`, false},
		{message, `{"id":"","source":"agentmemory"}`, false},
		{message, `{"source":""}`, false},
		{message, `{"source":7}`, false},
		{message, `"agentsmd"`, false},
		{output, `{"id":"/repo/AGENTS.md","source":"agentsmd"}`, false},
	} {
		line := `{"type":"item","id":"x","parent":null,"ts":"2026-11-02T10:00:00Z","item":` + tt.item + `,"instructions_part":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s: %v", tt.member, err)
		}
		it := e.(*ItemEntry)
		if _, kept := it.Unknown["instructions_part"]; (it.InstructionsPart != nil) != tt.typed || kept == tt.typed {
			t.Errorf("%s on %s: typed %+v, kept %v", tt.member, tt.item, it.InstructionsPart, kept)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, members(t, back)["instructions_part"], []byte(tt.member)) {
			t.Errorf("%s: written back as %s", tt.member, back)
		}
	}
}

// TestInstructionsPartWriter: a writer refuses an instructions_part on
// any item but a message, with no source, or naming agentsmd with no id.
func TestInstructionsPartWriter(t *testing.T) {
	for _, tt := range []struct {
		name string
		e    *ItemEntry
		ok   bool
	}{
		{"file", &ItemEntry{Item: openresponses.UserText(claudeMD), InstructionsPart: &LayerRef{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}}, true},
		{"memory", &ItemEntry{Item: openresponses.UserText("<memory/>"), InstructionsPart: &LayerRef{Source: LayerAgentMemory}}, true},
		{"output", &ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "x"), InstructionsPart: &LayerRef{Source: LayerAgentMemory}}, false},
		{"no source", &ItemEntry{Item: openresponses.UserText("x"), InstructionsPart: &LayerRef{ID: "memory"}}, false},
		{"agentsmd without id", &ItemEntry{Item: openresponses.UserText("x"), InstructionsPart: &LayerRef{Source: LayerAgentsMD}}, false},
	} {
		_, err := New(Header{ID: "a"}).Append(tt.e)
		if (err == nil) != tt.ok {
			t.Errorf("%s: Append = %v", tt.name, err)
		}
	}
}

// TestInstructionsPartContextHash: RFC 0002's context hash leaves
// instructions_part out, so a message carrying it keys as the same
// message without it.
func TestInstructionsPartContextHash(t *testing.T) {
	plain, marked := New(Header{ID: "a"}), New(Header{ID: "b"})
	ida, _ := plain.Append(NewItemEntry(openresponses.UserText(claudeMD)))
	idb, err := marked.Append(&ItemEntry{Item: openresponses.UserText(claudeMD), InstructionsPart: &LayerRef{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := plain.ContextHash(ida)
	hb, _ := marked.ContextHash(idb)
	if ha == "" || ha != hb {
		t.Errorf("context hashes %s and %s differ", ha, hb)
	}
}

// TestRewriteKeepsKnownSources: a rewrite keeps a source the format
// defines as written, on an instructions part, an omitted part and an
// item's instructions_part, and rewrites any other source and every id,
// which are text.
func TestRewriteKeepsKnownSources(t *testing.T) {
	r := strings.NewReplacer("agentsmd", "A", "agentmemory", "M", "x:notes", "N", "/repo", "~")
	f := func(s string) string { return r.Replace(s) }
	at := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

	item := &ItemEntry{Item: openresponses.UserText("agentsmd at /repo"), InstructionsPart: &LayerRef{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}}
	item.ID, item.Timestamp = "x", at
	out, err := RewriteStrings(item, f)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*ItemEntry)
	if got.InstructionsPart == nil || *got.InstructionsPart != (LayerRef{ID: "~/CLAUDE.md", Source: LayerAgentsMD}) || got.Item.(*openresponses.Message).Content.Text() != "A at ~" {
		t.Errorf("rewritten item: %+v, %+v", got.InstructionsPart, got.Item)
	}

	other := &ItemEntry{Item: openresponses.UserText("x"), InstructionsPart: &LayerRef{ID: "notes", Source: "x:notes"}}
	other.ID, other.Timestamp = "y", at
	if out, err = RewriteStrings(other, f); err != nil {
		t.Fatal(err)
	}
	if p := out.(*ItemEntry).InstructionsPart; p == nil || p.Source != "N" {
		t.Errorf("a source the format does not define was kept: %+v", p)
	}

	cfg := &ConfigEntry{Model: "gpt-5", InstructionsParts: []InstructionPart{
		{ID: "/repo/AGENTS.md", Source: LayerAgentsMD, Text: "agentsmd rules"},
		{ID: "notes", Source: "x:notes", Text: "notes"},
	}, InstructionsOmitted: []OmittedPart{{ID: "/repo/docs/AGENTS.md", Reason: "budget", Size: 10, Source: LayerAgentsMD}, {ID: "m", Reason: "budget", Size: 3, Source: "agentmemory"}}}
	cfg.ID, cfg.Timestamp = "z", at
	if out, err = RewriteStrings(cfg, f); err != nil {
		t.Fatal(err)
	}
	c := out.(*ConfigEntry)
	if p := c.InstructionsParts; len(p) != 2 || p[0].Source != LayerAgentsMD || p[0].ID != "~/AGENTS.md" || p[0].Text != "A rules" || p[1].Source != "N" {
		t.Errorf("rewritten parts: %+v", p)
	}
	if o := c.InstructionsOmitted; len(o) != 2 || o[0].Source != LayerAgentsMD || o[0].ID != "~/docs/AGENTS.md" || o[1].Source != LayerAgentMemory {
		t.Errorf("rewritten omitted parts: %+v", o)
	}
}

// TestInstructionLayersFixture reads the instruction-layers fixture: the
// parts carry the layers by their defined sources, the composed message
// carries instructions_part, the response verifies, and the context hash
// at the leaf is the same session's without the member.
func TestInstructionLayersFixture(t *testing.T) {
	raw := checkBuilt(t, "instruction-layers")
	if !bytes.Contains(raw, []byte(`"instructions_part":{"id":"/repo/CLAUDE.md","source":"agentsmd"}`)) {
		t.Error("the composed message does not carry its instructions_part")
	}
	s := loadFixture(t, "instruction-layers")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	marked := 0
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *ResponseEntry:
			if err := s.Verify(v.ID); err != nil {
				t.Errorf("response %s: %v", v.ID, err)
			}
		case *ItemEntry:
			if v.InstructionsPart != nil {
				marked++
			}
		}
	}
	if marked != 1 {
		t.Errorf("%d items carry instructions_part, want 1", marked)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, p := range ctx.Settings.InstructionsParts {
		if p.Source == LayerAgentsMD {
			files = append(files, p.ID)
		}
	}
	if strings.Join(files, ",") != "/repo/AGENTS.md,/repo/service/AGENTS.md" {
		t.Errorf("the instruction files are %v", files)
	}
	plain := instructionLayersFixture(t, false)
	hm, err := s.ContextHash(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	hp, err := plain.ContextHash(plain.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if hm != hp {
		t.Errorf("the context hash is %s, and %s without instructions_part", hm, hp)
	}
}

// TestBadInstructionsPartFixture reads bad-instructions-part: each
// instructions_part in a form 0.17 does not define is kept as written,
// and moves the context hash as any member the reader does not know.
func TestBadInstructionsPartFixture(t *testing.T) {
	s := checkBad017(t, "bad-instructions-part")
	held := 0
	for _, e := range s.Entries() {
		it, ok := e.(*ItemEntry)
		if !ok {
			continue
		}
		if it.InstructionsPart != nil {
			t.Errorf("%s: instructions_part taken as %+v", it.ID, it.InstructionsPart)
		}
		if _, ok := it.Unknown["instructions_part"]; ok {
			held++
			plain := New(Header{ID: "p"})
			bare := NewItemEntry(it.Item)
			id, _ := plain.Append(bare)
			with := New(Header{ID: "q"})
			kept := NewItemEntry(it.Item)
			kept.Unknown = map[string]json.RawMessage{"instructions_part": it.Unknown["instructions_part"]}
			id2, _ := with.Append(kept)
			ha, _ := plain.ContextHash(id)
			hb, _ := with.ContextHash(id2)
			if ha == hb {
				t.Errorf("%s: a kept instructions_part leaves the context hash as it is", it.ID)
			}
		}
	}
	if held != 3 {
		t.Errorf("%d items keep instructions_part as written, want 3", held)
	}
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("response %s: %v", r.ID, err)
			}
		}
	}
}

// TestHistoryMembers: legacy_id, normalised and redacted are an entry's
// history members, redacted and redacted_from a header's, and no member
// a later minor may define is one; the context hash leaves out exactly
// an entry's (RFC 0001, Entry envelope; RFC 0002).
func TestHistoryMembers(t *testing.T) {
	for _, name := range []string{"legacy_id", "normalised", "redacted"} {
		if !IsHistoryMember(name) {
			t.Errorf("%s is not a history member", name)
		}
	}
	for _, name := range []string{"failed", "trust", "copied_from", "from_link", "instructions_part", "x:legacy_id", "item", "id", "redacted_from"} {
		if IsHistoryMember(name) {
			t.Errorf("%s is a history member", name)
		}
	}
	for _, name := range []string{"redacted", "redacted_from"} {
		if !IsHeaderHistoryMember(name) {
			t.Errorf("%s is not a header history member", name)
		}
	}
	for _, name := range []string{"cwd", "parent_session", "legacy_id"} {
		if IsHeaderHistoryMember(name) {
			t.Errorf("%s is a header history member", name)
		}
	}
	plain := New(Header{ID: "a"})
	ida, _ := plain.Append(NewItemEntry(openresponses.UserText("hello")))
	ha, _ := plain.ContextHash(ida)
	marked := New(Header{ID: "b"})
	e := NewItemEntry(openresponses.UserText("hello"))
	e.LegacyID = "i0000001"
	idb, _ := marked.Append(e)
	if hb, _ := marked.ContextHash(idb); ha == "" || ha != hb {
		t.Errorf("a history member moves the context hash: %s, %s", ha, hb)
	}
}
