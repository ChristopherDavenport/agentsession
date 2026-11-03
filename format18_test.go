package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// fixtures018 are the 0.18 conformance fixtures, each built natively by
// its function, as for fixtures017.
var fixtures018 = map[string]func(t *testing.T) *Session{
	"instruction-layers-list": func(t *testing.T) *Session { return instructionLayersListFixture(t, true) },
}

// badFixtures018 are the 0.18 negative fixtures: each a valid file whose
// lines a reader keeps a member of as one it does not know.
var badFixtures018 = map[string]func(t *testing.T) []byte{
	"bad-instructions-part-list": badInstructionsPartListLines,
}

// writeFixtures018 writes the 0.18 fixtures, positive and negative.
func writeFixtures018(t *testing.T) {
	t.Helper()
	for name, build := range fixtures018 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, build := range badFixtures018 {
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), build(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkBad018 fails the test when testdata/sessions/<name>.jsonl is not
// what its 0.18 builder writes, and returns the session it holds.
func checkBad018(t *testing.T, name string) *Session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, badFixtures018[name](t)) {
		t.Errorf("%s.jsonl is not what its builder writes; run go test -update", name)
	}
	return loadFixture(t, name)
}

// claudeMDAndMemory is the message the instruction-layers-list fixture
// sends: an instruction file and a block of memory composed into one
// reminder, as Claude Code sends CLAUDE.md and MEMORY.md.
const claudeMDAndMemory = "<system-reminder>Contents of /repo/CLAUDE.md:\n\nUse tabs.\n\nContents of ~/.claude/memory/MEMORY.md:\n\n- The user prefers short answers.</system-reminder>"

// twoLayers are the layers claudeMDAndMemory was composed from, in the
// order their texts appear in it.
var twoLayers = []LayerRef{{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}, {ID: "~/.claude/memory/MEMORY.md", Source: LayerAgentMemory}}

// instructionLayersListFixture builds the session
// instruction-layers-list.jsonl holds: a config composing a product
// prompt and an instruction file; a message the harness composed from a
// second instruction file and a block of memory, carrying
// instructions_part as the list of the two in order; then the person's
// input and the model's answer, whose request_hash verifies. Without
// marked the message carries no instructions_part, which is the same
// session as the model saw it.
func instructionLayersListFixture(t *testing.T, marked bool) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000049", Records: AllRecords}, "2026-11-03T12:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", InstructionsParts: []InstructionPart{
		{ID: "product", Source: LayerProduct, Text: "You are a coding agent."},
		{ID: "/repo/AGENTS.md", Source: LayerAgentsMD, Text: "Run make check before you commit."},
	}})
	must(NewRunStart("run-1", SourceInput, ""))
	reminder := &ItemEntry{Item: openresponses.UserText(claudeMDAndMemory)}
	if marked {
		reminder.InstructionsLayers = slices.Clone(twoLayers)
	}
	must(reminder)
	must(NewItemEntry(openresponses.UserText("Fix the build.")))
	say("resp_1", "Fixed.")
	e, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(e)
	return s
}

// badInstructionsPartListLines builds bad-instructions-part-list.jsonl:
// two messages, each carrying an instructions_part list a writer of 0.18
// does not write: an empty list, and a list naming one id twice. A
// reader keeps each as a member it does not know, so each moves the
// context hash and neither is told from a person's input.
func badInstructionsPartListLines(t *testing.T) []byte {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000050"}, "2026-11-03T13:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	unknown := func(item openresponses.Item, member string) *ItemEntry {
		e := NewItemEntry(item)
		e.Unknown = map[string]json.RawMessage{"instructions_part": json.RawMessage(member)}
		return e
	}
	must(unknown(openresponses.UserText("<memory>none</memory>"), `[]`))
	must(unknown(openresponses.UserText(claudeMDAndMemory), `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"id":"/repo/CLAUDE.md","source":"agentmemory"}]`))
	must(NewItemEntry(openresponses.UserText("Fix the build.")))
	say("resp_1", "Done.")
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInstructionsPartListForms: an instructions_part list is taken in
// the form 0.18 defines on a message's entry, in a file of any minor:
// not empty, each element in the single form, each layer named once. A
// list in any other form is kept whole as written, and either is
// written back as read.
func TestInstructionsPartListForms(t *testing.T) {
	message := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	output := `{"type":"function_call_output","call_id":"call_1","output":"hi"}`
	for _, tt := range []struct {
		item, member string
		typed        int
	}{
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"source":"agentmemory"}]`, 2},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"}]`, 1},
		{message, `[{"id":"a","source":"agentsmd"},{"id":"b","source":"agentsmd"},{"id":"notes","source":"x:notes","x:wrapped":"reminder"}]`, 3},
		{message, `[{"id":"m1","source":"agentmemory"},{"id":"m2","source":"agentmemory"}]`, 2},
		{message, `[]`, 0},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"id":"/repo/CLAUDE.md","source":"agentmemory"}]`, 0},
		{message, `[{"source":"agentmemory"},{"source":"agentmemory"}]`, 0},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"source":"agentsmd"}]`, 0},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"id":"memory"}]`, 0},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},"agentmemory"]`, 0},
		{message, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},null]`, 0},
		{message, `[[{"source":"agentmemory"}]]`, 0},
		{output, `[{"id":"/repo/CLAUDE.md","source":"agentsmd"}]`, 0},
	} {
		line := `{"type":"item","id":"x","parent":null,"ts":"2026-11-03T10:00:00Z","item":` + tt.item + `,"instructions_part":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s: %v", tt.member, err)
		}
		it := e.(*ItemEntry)
		_, kept := it.Unknown["instructions_part"]
		if len(it.InstructionsLayers) != tt.typed || it.InstructionsPart != nil || kept == (tt.typed > 0) {
			t.Errorf("%s on %s: typed %+v, single %+v, kept %v", tt.member, tt.item, it.InstructionsLayers, it.InstructionsPart, kept)
		}
		if tt.typed > 0 && len(it.Layers()) != tt.typed {
			t.Errorf("%s: Layers() = %+v", tt.member, it.Layers())
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

// TestLayers: a reader reads a single instructions_part as a list of
// one, and an item carrying neither form names no layer.
func TestLayers(t *testing.T) {
	single := &ItemEntry{Item: openresponses.UserText(claudeMD), InstructionsPart: &LayerRef{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}}
	if got := single.Layers(); !slices.Equal(got, []LayerRef{{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}}) {
		t.Errorf("single: %+v", got)
	}
	list := &ItemEntry{Item: openresponses.UserText(claudeMDAndMemory), InstructionsLayers: twoLayers}
	if got := list.Layers(); !slices.Equal(got, twoLayers) {
		t.Errorf("list: %+v", got)
	}
	if got := NewItemEntry(openresponses.UserText("x")).Layers(); got != nil {
		t.Errorf("none: %+v", got)
	}
}

// TestInstructionsPartListWriter: a writer refuses a list on any item
// but a message, beside the single form, with an element the single
// form refuses, or naming a layer twice.
func TestInstructionsPartListWriter(t *testing.T) {
	msg := func(layers ...LayerRef) *ItemEntry {
		return &ItemEntry{Item: openresponses.UserText(claudeMDAndMemory), InstructionsLayers: layers}
	}
	both := msg(twoLayers...)
	both.InstructionsPart = &LayerRef{Source: LayerAgentMemory}
	unknown := msg(twoLayers...)
	unknown.Unknown = map[string]json.RawMessage{"instructions_part": json.RawMessage(`[]`)}
	for _, tt := range []struct {
		name string
		e    *ItemEntry
		ok   bool
	}{
		{"two layers", msg(twoLayers...), true},
		{"one layer", msg(twoLayers[0]), true},
		{"output", &ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "x"), InstructionsLayers: twoLayers}, false},
		{"both forms", both, false},
		{"typed and unknown", unknown, false},
		{"no source", msg(twoLayers[0], LayerRef{ID: "memory"}), false},
		{"agentsmd without id", msg(twoLayers[1], LayerRef{Source: LayerAgentsMD}), false},
		{"id twice", msg(twoLayers[0], LayerRef{ID: twoLayers[0].ID, Source: LayerAgentMemory}), false},
		{"source twice without id", msg(LayerRef{Source: LayerAgentMemory}, LayerRef{Source: LayerAgentMemory}), false},
	} {
		_, err := New(Header{ID: "a"}).Append(tt.e)
		if (err == nil) != tt.ok {
			t.Errorf("%s: Append = %v", tt.name, err)
		}
	}
}

// TestInstructionsPartListContextHash: RFC 0002's context hash leaves an
// instructions_part list out, as it does the single form.
func TestInstructionsPartListContextHash(t *testing.T) {
	plain, marked := New(Header{ID: "a"}), New(Header{ID: "b"})
	ida, _ := plain.Append(NewItemEntry(openresponses.UserText(claudeMDAndMemory)))
	idb, err := marked.Append(&ItemEntry{Item: openresponses.UserText(claudeMDAndMemory), InstructionsLayers: twoLayers})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := plain.ContextHash(ida)
	hb, _ := marked.ContextHash(idb)
	if ha == "" || ha != hb {
		t.Errorf("context hashes %s and %s differ", ha, hb)
	}
}

// TestRewriteKeepsListSources: a rewrite keeps a source the format
// defines as written on each element of an instructions_part list, and
// rewrites any other source and every id, which are text.
func TestRewriteKeepsListSources(t *testing.T) {
	r := strings.NewReplacer("agentsmd", "A", "agentmemory", "M", "x:notes", "N", "/repo", "~")
	item := &ItemEntry{Item: openresponses.UserText("agentmemory at /repo"), InstructionsLayers: []LayerRef{
		{ID: "/repo/CLAUDE.md", Source: LayerAgentsMD}, {Source: LayerAgentMemory}, {ID: "notes", Source: "x:notes"},
	}}
	item.ID, item.Timestamp = "x", time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	out, err := RewriteStrings(item, r.Replace)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*ItemEntry)
	want := []LayerRef{{ID: "~/CLAUDE.md", Source: LayerAgentsMD}, {Source: LayerAgentMemory}, {ID: "notes", Source: "N"}}
	if !slices.Equal(got.InstructionsLayers, want) || got.Item.(*openresponses.Message).Content.Text() != "M at ~" {
		t.Errorf("rewritten item: %+v, %+v", got.InstructionsLayers, got.Item)
	}
}

// TestInstructionLayersListFixture reads the instruction-layers-list
// fixture: the composed message names its two layers in order, the
// response verifies, the message is no input of its run, and the context
// hash at the leaf is the same session's without the member.
func TestInstructionLayersListFixture(t *testing.T) {
	raw := checkBuilt(t, "instruction-layers-list")
	if !bytes.Contains(raw, []byte(`"instructions_part":[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"id":"~/.claude/memory/MEMORY.md","source":"agentmemory"}]`)) {
		t.Error("the composed message does not carry its instructions_part list")
	}
	s := loadFixture(t, "instruction-layers-list")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	var marked []*ItemEntry
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *ResponseEntry:
			if err := s.Verify(v.ID); err != nil {
				t.Errorf("response %s: %v", v.ID, err)
			}
		case *ItemEntry:
			if v.Layers() != nil {
				marked = append(marked, v)
			}
		}
	}
	if len(marked) != 1 || !slices.Equal(marked[0].Layers(), twoLayers) {
		t.Fatalf("items carrying layers: %v", marked)
	}
	turns, _ := PathInputs(s.Path(s.Leaf()))
	if len(turns) != 1 || len(turns[0].Inputs) != 1 || len(turns[0].LeftOut) != 1 || turns[0].LeftOut[0].Kind != NotInputComposed {
		t.Errorf("the run's inputs are %+v", turns)
	}
	plain := instructionLayersListFixture(t, false)
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

// TestBadInstructionsPartListFixture reads bad-instructions-part-list:
// each list in a form 0.18 does not define is kept whole as written,
// names no layer, and moves the context hash as any member the reader
// does not know.
func TestBadInstructionsPartListFixture(t *testing.T) {
	s := checkBad018(t, "bad-instructions-part-list")
	held := 0
	for _, e := range s.Entries() {
		it, ok := e.(*ItemEntry)
		if !ok {
			continue
		}
		if it.Layers() != nil {
			t.Errorf("%s: instructions_part taken as %+v", it.ID, it.Layers())
		}
		if _, ok := it.Unknown["instructions_part"]; ok {
			held++
			plain := New(Header{ID: "p"})
			id, _ := plain.Append(NewItemEntry(it.Item))
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
	if held != 2 {
		t.Errorf("%d items keep instructions_part as written, want 2", held)
	}
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("response %s: %v", r.ID, err)
			}
		}
	}
}
