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

	"github.com/ChristopherDavenport/openresponses"
)

// fixtures017 are the 0.17 conformance fixtures, each built natively by
// its function, as for fixtures016.
var fixtures017 = map[string]func(t *testing.T) *Session{
	"instruction-layers":       func(t *testing.T) *Session { return instructionLayersFixture(t, true) },
	"inputs":                   inputsFixture,
	"cost-children":            func(t *testing.T) *Session { p, _, _ := costChildrenFixture(t); return p },
	"cost-children-child":      func(t *testing.T) *Session { _, c, _ := costChildrenFixture(t); return c },
	"cost-children-grandchild": func(t *testing.T) *Session { _, _, g := costChildrenFixture(t); return g },
	"env-snapshot":             envSnapshotFixture,
	"instruction-trust":        func(t *testing.T) *Session { return instructionTrustFixture(t, true) },
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

// inputsFixture builds the session inputs.jsonl holds, one run per
// shape of input (RFC 0001, run, since 0.17):
//
//   - run-1, an input, opens with two user messages and a composed
//     memory layer, its model asks to deploy, a steer drains mid-run and
//     the deploy is held;
//   - run-2 resumes the held call with the note a person typed;
//   - run-3 starts from a followup queued after run-2 ended;
//   - run-4 starts from a delivery, which is its input;
//   - run-5 asks a peer, which acknowledges;
//   - run-6 starts with an item copied from the parent session and the
//     peer's later report.
func inputsFixture(t *testing.T) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000044", Records: AllRecords}, "2026-11-02T14:00:00Z")
	endAs := func(reason string) {
		t.Helper()
		e, err := s.EndRun(reason, "")
		if err != nil {
			t.Fatal(err)
		}
		must(e)
	}
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Ship releases.")})

	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Plan the release.")))
	must(NewItemEntry(openresponses.UserText("Use the checklist.")))
	must(&ItemEntry{Item: openresponses.UserText("<memory>Releases ship on Tuesday.</memory>"), InstructionsPart: &LayerRef{Source: LayerAgentMemory}})
	sent := fixtureHash(t, s)
	deploy := must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_deploy", Name: "deploy", Arguments: `{}`}, ResponseID: "resp_1"})
	must(&ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	steer := NewQueued(openresponses.UserText("Also tag it."), ModeSteer)
	must(steer)
	must(steer.Drain())
	must(NewDecision("call_deploy", deploy, VerdictHold, ByPolicy).WithReason("asks a person"))
	endAs(ReasonInputRequired)

	must(NewRunStart("run-2", SourceResume, ""))
	must(NewItemEntry(openresponses.UserText("Approved, go ahead.")))
	must(NewDispatch("call_deploy", deploy))
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_deploy", "deployed v2")))
	say("resp_2", "Deployed and tagged.")
	endAs(ReasonDone)

	followup := NewQueued(openresponses.UserText("Now write the notes."), ModeFollowUp)
	must(followup)
	must(NewRunStart("run-3", SourceInput, ""))
	must(followup.Drain())
	say("resp_3", "Notes written.")
	endAs(ReasonDone)

	delivery := NewDelivery(openresponses.UserText("Background task smoke-1 finished: green.")).WithTrigger("task", "smoke-1", "background")
	must(delivery)
	must(NewRunStart("run-4", SourceInput, ""))
	must(delivery.Drain())
	say("resp_4", "Smoke tests are green.")
	endAs(ReasonDone)

	must(NewRunStart("run-5", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Ask the reviewer.")))
	sent = fixtureHash(t, s)
	review := must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_2", CallID: "call_review", Name: "review", Arguments: `{}`}, ResponseID: "resp_5"})
	must(&ResponseEntry{ResponseID: "resp_5", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	must(NewDispatch("call_review", review))
	link := must(NewPeerLink("call_review", Callee{URL: "https://review.example/a2a", Task: "task-1"}))
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_review", `{"status":"queued"}`)))
	say("resp_6", "The reviewer has it.")
	endAs(ReasonDone)

	must(NewRunStart("run-6", SourceInput, ""))
	must(&ItemEntry{Item: openresponses.UserText("From the lead: keep the notes short."), CopiedFrom: &CopiedFrom{Session: "01995b2a-0000-7000-8000-000000000045"}})
	must(&ItemEntry{Item: openresponses.UserText("Review: approved."), FromLink: link, Source: &Trigger{Kind: "delivery", Ref: "task-1", Source: "a2a"}})
	say("resp_7", "Approved; notes kept short.")
	endAs(ReasonDone)
	return s
}

// TestInputsFixture reads the inputs fixture: each run's inputs, and the
// user messages each leaves out with why, and on the same path without
// run entries the turns a reader takes up to each response. A delivery
// drained before its run's first response is that run's input; without
// run entries a reader cannot tell it from a steer that joined a turn,
// and leaves it out.
func TestInputsFixture(t *testing.T) {
	checkBuilt(t, "inputs")
	s := loadFixture(t, "inputs")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	text := func(e *ItemEntry) string { return e.Item.(*openresponses.Message).Content.Text() }
	describe := func(turns []TurnInputs) []string {
		var out []string
		for _, turn := range turns {
			var in, left []string
			for _, e := range turn.Inputs {
				in = append(in, text(e))
			}
			for _, l := range turn.LeftOut {
				left = append(left, l.Kind+": "+text(l.Entry))
			}
			out = append(out, strings.Join(in, " | ")+" / "+strings.Join(left, " | "))
		}
		return out
	}
	path := s.Path(s.Leaf())
	turns, byRuns := PathInputs(path)
	want := []string{
		"Plan the release. | Use the checklist. / instructions_part: <memory>Releases ship on Tuesday.</memory> | steer: Also tag it.",
		" / resume: Approved, go ahead.",
		"Now write the notes. / ",
		"Background task smoke-1 finished: green. / ",
		"Ask the reviewer. / ",
		" / copied: From the lead: keep the notes short. | from_link: Review: approved.",
	}
	if got := describe(turns); !byRuns || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("by runs %v, the runs' inputs are\n%s\nwant\n%s", byRuns, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	var bare []Entry
	for _, e := range path {
		if _, ok := e.(*RunEntry); !ok {
			bare = append(bare, e)
		}
	}
	turns, byRuns = PathInputs(bare)
	want = []string{
		"Plan the release. | Use the checklist. / instructions_part: <memory>Releases ship on Tuesday.</memory>",
		"Approved, go ahead. / steer: Also tag it.",
		"Now write the notes. / ",
		" / steer: Background task smoke-1 finished: green.",
		"Ask the reviewer. / ",
		" / copied: From the lead: keep the notes short. | from_link: Review: approved.",
	}
	if got := describe(turns); byRuns || strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("by runs %v, the turns' inputs are\n%s\nwant\n%s", byRuns, strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// costParent is the session ID of the cost-children fixture's parent.
const costParent = "01995b2a-0000-7000-8000-000000000046"

// costChildrenFixture builds the sessions cost-children.jsonl and its
// children hold (RFC 0001, link, Cost across sessions, since 0.17): a
// parent at gpt-5 delegates two calls by subsession links. Child A, at
// gpt-5-mini, delegates a call of its own to a grandchild at gpt-5-nano,
// answers, and is then continued past the answer the parent took, whose
// output names that answer in parents; a cancel notice names child A's
// link, and counts it no second time. Child B was never written, so a
// reader reports it unread.
func costChildrenFixture(t *testing.T) (parent, child, grandchild *Session) {
	t.Helper()
	childA, childB := SubsessionID(costParent, "call_a"), SubsessionID(costParent, "call_b")
	grand := SubsessionID(childA, "call_g")
	answer := func(s *Session, must func(Entry) string, resp, model, text string, in, out int) string {
		t.Helper()
		sent := fixtureHash(t, s)
		must(&ItemEntry{Item: &openresponses.Message{ID: "msg_" + resp, Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: text, Annotations: []openresponses.Annotation{}}}}, ResponseID: resp})
		return must(&ResponseEntry{ResponseID: resp, Model: model, Status: openresponses.ResponseStatusCompleted, RequestHash: sent, Usage: &openresponses.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}})
	}
	calls := func(s *Session, must func(Entry) string, resp, model string, in, out int, ids ...string) []string {
		t.Helper()
		sent := fixtureHash(t, s)
		var entries []string
		for _, id := range ids {
			entries = append(entries, must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_" + id, CallID: id, Name: "research", Arguments: `{}`}, ResponseID: resp}))
		}
		must(&ResponseEntry{ResponseID: resp, Model: model, Status: openresponses.ResponseStatusCompleted, RequestHash: sent, Usage: &openresponses.Usage{InputTokens: in, OutputTokens: out, TotalTokens: in + out}})
		return entries
	}

	grandchild, must, _ := fixtureSession(t, Header{ID: grand, ParentSession: childA, SpawnedBy: "call_g", Records: []string{TypeDispatch}}, "2026-11-02T15:00:10Z")
	must(&ConfigEntry{Model: "gpt-5-nano", Instructions: ptr("Look prices up.")})
	must(NewItemEntry(openresponses.UserText("Look up vendor A's prices.")))
	grandLeaf := answer(grandchild, must, "resp_g1", "gpt-5-nano", "A costs 10 a seat.", 10, 1)

	child, must, _ = fixtureSession(t, Header{ID: childA, ParentSession: costParent, SpawnedBy: "call_a", Records: []string{TypeDispatch}}, "2026-11-02T15:00:05Z")
	must(&ConfigEntry{Model: "gpt-5-mini", Instructions: ptr("Research a vendor.")})
	must(NewItemEntry(openresponses.UserText("Research vendor A.")))
	g := calls(child, must, "resp_c1", "gpt-5-mini", 50, 5, "call_g")
	must(NewSubsessionLink(grand, "call_g"))
	must(NewDispatch("call_g", g[0]))
	out := NewItemEntry(openresponses.NewFunctionCallOutput("call_g", "A costs 10 a seat."))
	out.Parents = []EntryRef{{Session: grand, Entry: grandLeaf}}
	must(out)
	childLeaf := answer(child, must, "resp_c2", "gpt-5-mini", "Vendor A is cheaper.", 60, 6)
	must(NewItemEntry(openresponses.UserText("And their support?")))
	answer(child, must, "resp_c3", "gpt-5-mini", "Support is email only.", 70, 7)

	parent, must, _ = fixtureSession(t, Header{ID: costParent, Records: []string{TypeDispatch}}, "2026-11-02T15:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Delegate research.")})
	must(NewItemEntry(openresponses.UserText("Compare vendors A and B.")))
	c := calls(parent, must, "resp_1", "gpt-5", 100, 20, "call_a", "call_b")
	linkA := NewSubsessionLink(childA, "call_a")
	must(linkA)
	must(NewDispatch("call_a", c[0]))
	must(NewSubsessionLink(childB, "call_b"))
	must(NewDispatch("call_b", c[1]))
	must(NewCancelSentLink(linkA))
	outA := NewItemEntry(openresponses.NewFunctionCallOutput("call_a", "Vendor A is cheaper."))
	outA.Parents = []EntryRef{{Session: childA, Entry: childLeaf}}
	must(outA)
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_b", "Vendor B did not answer.")))
	answer(parent, must, "resp_2", "gpt-5", "A is cheaper; B is unknown.", 200, 30)
	return parent, child, grandchild
}

// TestCostChildrenFixture reads the cost-children fixtures as a reader
// that reports cost counts them: child A once, at the leaf the parent's
// output names, its calls priced at gpt-5-mini, with the grandchild at
// gpt-5-nano under it; child B unread; the parent's own calls its own.
func TestCostChildrenFixture(t *testing.T) {
	for _, name := range []string{"cost-children", "cost-children-child", "cost-children-grandchild"} {
		checkBuilt(t, name)
	}
	held := map[string]*Session{}
	for _, name := range []string{"cost-children-child", "cost-children-grandchild"} {
		s := loadFixture(t, name)
		held[s.ID()] = s
	}
	resolve := func(id string) (*Session, error) { return held[id], nil }
	parent := loadFixture(t, "cost-children")
	path := parent.Path(parent.Leaf())
	own := 0
	for _, c := range ModelCalls(path) {
		if c.Model != "gpt-5" {
			t.Errorf("the parent's call %s is priced at %s", c.Entry.Base().ID, c.Model)
		}
		own += c.Usage.InputTokens
	}
	if own != 300 {
		t.Errorf("the parent's own input tokens are %d", own)
	}
	children, unread := DelegatedWork(path, resolve)
	describe := func(d Delegated) string {
		var parts []string
		for _, c := range d.Calls {
			parts = append(parts, fmt.Sprintf("%s:%d", c.Model, c.Usage.InputTokens))
		}
		return strings.Join(parts, ",")
	}
	if len(children) != 1 {
		t.Fatalf("%d children counted, want 1", len(children))
	}
	a := children[0]
	childA := held[SubsessionID(costParent, "call_a")]
	if a.Session != childA.ID() || len(a.Leaves) != 1 || !a.Leaves[0].Named || a.Leaves[0].Entry == childA.Leaf() || describe(a) != "gpt-5-mini:50,gpt-5-mini:60" {
		t.Errorf("child A is counted as %+v: %s", a, describe(a))
	}
	if len(a.Children) != 1 || len(a.Children[0].Leaves) != 1 || !a.Children[0].Leaves[0].Named || describe(a.Children[0]) != "gpt-5-nano:10" {
		t.Errorf("the grandchild is counted as %+v", a.Children)
	}
	if len(unread) != 1 || unread[0].Session != SubsessionID(costParent, "call_b") || !errors.Is(unread[0].Err, ErrChildNotFound) {
		t.Errorf("unread = %+v", unread)
	}
	// Without the parents member a reader takes child A's resume leaf,
	// and says it did.
	var bare []Entry
	for _, e := range path {
		if it, ok := e.(*ItemEntry); ok && len(it.Parents) > 0 {
			c := *it
			c.Parents = nil
			e = &c
		}
		bare = append(bare, e)
	}
	children, _ = DelegatedWork(bare, resolve)
	if len(children) != 1 || len(children[0].Leaves) != 1 || children[0].Leaves[0].Named || children[0].Leaves[0].Entry != childA.Leaf() || describe(children[0]) != "gpt-5-mini:50,gpt-5-mini:60,gpt-5-mini:70" {
		t.Errorf("without parents child A is counted as %+v", children)
	}
}

// envSnapshotFixture builds the session env-snapshot.jsonl holds (RFC
// 0001, env, since 0.17): an env with cwd, an abbreviated git revision
// and a local workspace on a host, then a turn; after it two branches,
// one whose env names a new cwd alone, which leaves the workspace absent
// and is a substitution, the vcs unsaid, and one whose env repeats every
// member it still knows beside the new cwd, which is none.
func envSnapshotFixture(t *testing.T) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000047"}, "2026-11-02T16:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Work in the repo.")})
	full := func(cwd string) *EnvEntry {
		e := NewEnvEntry(cwd)
		e.VCS = &VCS{System: "git", Revision: "a1b2c3d"}
		e.SetWorkspace("local", "").Host = "laptop"
		return e
	}
	must(full("/home/u/proj"))
	must(NewItemEntry(openresponses.UserText("Build it.")))
	say("resp_1", "Built.")
	turn := s.Leaf()
	must(NewEnvEntry("/home/u/proj/service"))
	must(NewItemEntry(openresponses.UserText("Now the service.")))
	say("resp_2", "Built the service.")
	if err := s.Branch(turn); err != nil {
		t.Fatal(err)
	}
	must(full("/home/u/proj/service"))
	must(NewItemEntry(openresponses.UserText("Now the service.")))
	say("resp_3", "Built the service.")
	return s
}

// TestEnvSnapshotFixture reads the env-snapshot fixture: on the branch
// whose env names a cwd alone that env is a substitution to the absent
// workspace and says nothing of the vcs; on the branch that repeats every
// member it is none; the abbreviated revision is kept as written; and the
// header's cwd is the working directory only before the first env.
func TestEnvSnapshotFixture(t *testing.T) {
	checkBuilt(t, "env-snapshot")
	s := loadFixture(t, "env-snapshot")
	leaves := s.Leaves()
	if len(leaves) != 2 {
		t.Fatalf("%d leaves", len(leaves))
	}
	bare, whole := s.Path(leaves[0]), s.Path(leaves[1])
	subs := Substitutions(bare)
	if len(subs) != 1 || subs[0].Workspace != nil || subs[0].CWD != "/home/u/proj/service" {
		t.Errorf("on the cwd-only branch the substitutions are %+v", subs)
	}
	if env := EnvInForce(bare); env == nil || env.VCS != nil || env.Workspace != nil {
		t.Errorf("on the cwd-only branch the env in force is %+v", env)
	}
	if subs := Substitutions(whole); len(subs) != 0 {
		t.Errorf("on the whole branch the substitutions are %+v", subs)
	}
	env := EnvInForce(whole)
	if env == nil || env.VCS == nil || env.VCS.Revision != "a1b2c3d" || env.Workspace == nil || env.Workspace.Host != "laptop" {
		t.Errorf("on the whole branch the env in force is %+v", env)
	}
	h := s.Header()
	h.CWD = "/srv/elsewhere"
	if got := WorkingDir(h, whole[:1]); got != "/srv/elsewhere" {
		t.Errorf("before the first env the working directory is %q", got)
	}
	if got := WorkingDir(h, whole); got != "/home/u/proj/service" {
		t.Errorf("after it the working directory is %q", got)
	}
	noCWD := append(append([]Entry(nil), whole...), &EnvEntry{VCS: &VCS{System: "git"}})
	if got := WorkingDir(h, noCWD); got != "" {
		t.Errorf("an env naming no cwd leaves the working directory %q, want unsaid", got)
	}
}

// TestDelegatedElsewhere: a subsession callee in another system whose
// session the reader does not hold is not counted, and is reported as
// such, apart from a child that could not be read (RFC 0001, link, Cost
// across sessions).
func TestDelegatedElsewhere(t *testing.T) {
	s := New(Header{ID: "parent"})
	must := func(e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	must(&ConfigEntry{Model: "gpt-5"})
	call := must(&ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_x", Name: "delegate", Arguments: `{}`}, ResponseID: "resp_1"})
	must(&ResponseEntry{ResponseID: "resp_1"})
	must(NewDispatch("call_x", call))
	must(NewCalleeSubsessionLink("call_x", Callee{URL: "https://agents.example/a2a", Task: "t1", Session: "remote-1"}))
	must(NewItemEntry(openresponses.NewFunctionCallOutput("call_x", "done")))
	children, unread := DelegatedWork(s.Path(s.Leaf()), func(string) (*Session, error) { return nil, nil })
	if len(children) != 0 || len(unread) != 1 || unread[0].Session != "remote-1" || !errors.Is(unread[0].Err, ErrChildElsewhere) || errors.Is(unread[0].Err, ErrChildNotFound) {
		t.Errorf("children %+v, unread %+v", children, unread)
	}
}

// TestDelegatedUnion: one session named by two links that count it at
// different leaves is counted once, over the union of its two paths, so
// the prefix the two share counts once (RFC 0001, link, Cost across
// sessions).
func TestDelegatedUnion(t *testing.T) {
	child := New(Header{ID: "child"})
	add := func(s *Session, e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	usage := func(n int) *openresponses.Usage { return &openresponses.Usage{InputTokens: n, TotalTokens: n} }
	add(child, &ConfigEntry{Model: "m"})
	add(child, NewItemEntry(openresponses.UserText("task")))
	fork := add(child, &ResponseEntry{ResponseID: "r1", Usage: usage(10)})
	add(child, NewItemEntry(openresponses.UserText("one way")))
	one := add(child, &ResponseEntry{ResponseID: "r2", Usage: usage(20)})
	if err := child.Branch(fork); err != nil {
		t.Fatal(err)
	}
	add(child, NewItemEntry(openresponses.UserText("the other")))
	other := add(child, &ResponseEntry{ResponseID: "r3", Usage: usage(30)})

	parent := New(Header{ID: "parent"})
	add(parent, &ConfigEntry{Model: "m"})
	for i, leaf := range []string{one, other} {
		callID := fmt.Sprintf("call_%d", i)
		call := add(parent, &ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "spawn", Arguments: `{}`}, ResponseID: "p" + callID})
		add(parent, &ResponseEntry{ResponseID: "p" + callID})
		add(parent, NewSubsessionLink("child", callID))
		add(parent, NewDispatch(callID, call))
		out := NewItemEntry(openresponses.NewFunctionCallOutput(callID, "done"))
		out.Parents = []EntryRef{{Session: "child", Entry: leaf}}
		add(parent, out)
	}
	children, unread := DelegatedWork(parent.Path(parent.Leaf()), func(id string) (*Session, error) {
		if id == "child" {
			return child, nil
		}
		return nil, nil
	})
	if len(unread) != 0 || len(children) != 1 {
		t.Fatalf("children %+v, unread %+v", children, unread)
	}
	total := 0
	for _, c := range children[0].Calls {
		total += c.Usage.InputTokens
	}
	if d := children[0]; len(d.Leaves) != 2 || d.Leaves[0].Entry != one || d.Leaves[1].Entry != other || total != 60 {
		t.Errorf("the child is counted at %+v for %d tokens, want both leaves and 60", d.Leaves, total)
	}
}

// TestDelegatedReportsOnce: a child read again over the union of its
// paths, when a later link names it at another leaf, reports a leaf its
// own link names and its child does not hold once, not once per read.
func TestDelegatedReportsOnce(t *testing.T) {
	add := func(s *Session, e Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	spawn := func(s *Session, callID, child, leaf string) {
		t.Helper()
		call := add(s, &ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "spawn", Arguments: `{}`}, ResponseID: "r_" + callID})
		add(s, &ResponseEntry{ResponseID: "r_" + callID})
		add(s, NewSubsessionLink(child, callID))
		add(s, NewDispatch(callID, call))
		out := NewItemEntry(openresponses.NewFunctionCallOutput(callID, "done"))
		if leaf != "" {
			out.Parents = []EntryRef{{Session: child, Entry: leaf}}
		}
		add(s, out)
	}
	d := New(Header{ID: "d"})
	add(d, &ConfigEntry{Model: "m"})
	a := New(Header{ID: "a"})
	add(a, &ConfigEntry{Model: "m"})
	spawn(a, "call_d", "d", "sha256:none")
	spawn(a, "call_c", "c", "")
	a1 := add(a, NewItemEntry(openresponses.UserText("first")))
	a2 := add(a, NewItemEntry(openresponses.UserText("second")))
	c := New(Header{ID: "c"})
	add(c, &ConfigEntry{Model: "m"})
	spawn(c, "call_back", "a", a2)
	parent := New(Header{ID: "p"})
	add(parent, &ConfigEntry{Model: "m"})
	spawn(parent, "call_a", "a", a1)
	held := map[string]*Session{"a": a, "c": c, "d": d}
	_, unread := DelegatedWork(parent.Path(parent.Leaf()), func(id string) (*Session, error) { return held[id], nil })
	if len(unread) != 1 || unread[0].Session != "d" {
		t.Errorf("unread = %+v, want d once", unread)
	}
}

// instructionTrustFixture builds the session instruction-trust.jsonl
// holds (RFC 0001, Instructions as parts, since 0.17): a memory part
// carrying untrusted beside a product part carrying none; a delta that
// keeps both, the memory part's trust with it, and adds an instruction
// file read from a private store; a compaction whose
// checkpoint carries the parts with their trust; every response hashed
// and verifying. Without marked no part carries trust, which is the same
// session as the model saw it.
func instructionTrustFixture(t *testing.T, marked bool) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000048"}, "2026-11-02T17:00:00Z")
	trust := func(labels ...string) *Trust {
		if !marked {
			return nil
		}
		return &Trust{Reads: labels}
	}
	memory := InstructionPart{ID: "memory", Source: LayerAgentMemory, Text: "The release page said v2 ships Friday.", Trust: trust(TrustUntrusted)}
	must(&ConfigEntry{Model: "gpt-5", InstructionsParts: []InstructionPart{
		{ID: "product", Source: LayerProduct, Text: "You are a release agent."},
		memory,
	}})
	must(NewItemEntry(openresponses.UserText("When does v2 ship?")))
	say("resp_1", "Friday.")
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	delta := ctx.Settings.InstructionsDelta([]InstructionPart{
		{ID: "product", Source: LayerProduct, Text: "You are a release agent."},
		memory,
		{ID: "/vault/AGENTS.md", Source: LayerAgentsMD, Text: "Ask before tagging.", Trust: trust(TrustPrivate)},
	})
	if delta == nil {
		t.Fatal("no delta for a new part")
	}
	must(delta)
	kept := must(NewItemEntry(openresponses.UserText("Tag it.")))
	say("resp_2", "Asking first: tag v2?")
	if ctx, err = s.Context(); err != nil {
		t.Fatal(err)
	}
	must(&CompactionEntry{FirstKept: kept, Summary: openresponses.SystemText("Summary: v2 ships Friday."), Config: ctx.Settings, Trust: PathTrust(s.Path(s.Leaf()))})
	say("resp_3", "Still waiting on the tag.")
	return s
}

// TestInstructionTrustFixture reads the instruction-trust fixture: the
// part a delta keeps keeps its trust in force, the checkpoint carries
// the parts' trust, the path's trust takes in the parts', the responses
// verify, and the context hash at the leaf is the same session's without
// any part's trust.
func TestInstructionTrustFixture(t *testing.T) {
	raw := checkBuilt(t, "instruction-trust")
	if !bytes.Contains(raw, []byte(`"trust":{"reads":["untrusted"]}`)) {
		t.Error("the memory part does not carry its trust")
	}
	s := loadFixture(t, "instruction-trust")
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("response %s: %v", r.ID, err)
			}
		}
	}
	path := s.Path(s.Leaf())
	cfgs := 0
	for _, e := range path {
		if c, ok := e.(*ConfigEntry); ok {
			cfgs++
			if cfgs == 2 {
				for _, p := range c.InstructionsParts {
					if p.ID == "memory" || p.Keep == 0 && p.Text == "" {
						t.Errorf("the delta repeats the memory part, or names one without text: %+v", p)
					}
				}
			}
		}
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Trust{}
	for _, p := range ctx.Settings.InstructionsParts {
		byID[p.ID] = p.Trust
	}
	if m := byID["memory"]; m == nil || !slices.Equal(m.Reads, []string{TrustUntrusted}) || byID["product"] != nil {
		t.Errorf("the parts in force carry trust %v", byID)
	}
	want := []string{TrustPrivate, TrustUntrusted}
	if ctx.Trust == nil || !slices.Equal(ctx.Trust.Reads, want) {
		t.Errorf("the context's trust is %+v", ctx.Trust)
	}
	if got := PathTrust(path[:1]); got == nil || !slices.Equal(got.Reads, []string{TrustUntrusted}) {
		t.Errorf("the first config's trust is %+v", got)
	}
	plain := instructionTrustFixture(t, false)
	hm, err := s.ContextHash(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	hp, err := plain.ContextHash(plain.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if hm != hp {
		t.Errorf("the context hash is %s, and %s without the parts' trust", hm, hp)
	}
}

// TestPartTrustForms: a part's trust is taken in the form 0.15 defines
// for an item's, and a rewrite leaves its labels as they are.
func TestPartTrustForms(t *testing.T) {
	for _, tt := range []struct {
		member string
		typed  bool
	}{
		{`{"reads":["untrusted"]}`, true},
		{`{"reads":[]}`, true},
		{`{"reads":["private","private"]}`, false},
		{`["untrusted"]`, false},
	} {
		line := `{"type":"config","id":"x","parent":null,"ts":"2026-11-02T10:00:00Z","instructions_parts":[{"id":"memory","source":"agentmemory","text":"m","trust":` + tt.member + `}]}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s: %v", tt.member, err)
		}
		if p := e.(*ConfigEntry).InstructionsParts[0]; (p.Trust != nil) != tt.typed {
			t.Errorf("%s: typed %+v", tt.member, p.Trust)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(back, []byte(`"trust":`+tt.member)) {
			t.Errorf("%s: written back as %s", tt.member, back)
		}
	}
	e := &ConfigEntry{Model: "m", InstructionsParts: []InstructionPart{{ID: "memory", Source: LayerAgentMemory, Text: "untrusted private", Trust: &Trust{Reads: []string{TrustPrivate, TrustUntrusted}}}}}
	e.ID, e.Timestamp = "x", time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	out, err := RewriteStrings(e, func(s string) string { return strings.NewReplacer("untrusted", "U", "private", "P").Replace(s) })
	if err != nil {
		t.Fatal(err)
	}
	if p := out.(*ConfigEntry).InstructionsParts[0]; p.Trust == nil || !slices.Equal(p.Trust.Reads, []string{TrustPrivate, TrustUntrusted}) || p.Text != "U P" {
		t.Errorf("rewritten: %+v", p)
	}
}
