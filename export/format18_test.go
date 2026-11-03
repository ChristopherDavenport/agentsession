package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// TestInstructionsPartListStep: the instruction-layers-list fixture
// exports the message the harness composed from two layers as a user
// step carrying instructions_part under extra.agentsession as written, a
// list of the two in order (RFC 0001, Projections/ATIF, 0.18).
func TestInstructionsPartListStep(t *testing.T) {
	s := loadFixture(t, "instruction-layers-list")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) < 2 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	composed, input := doc.Steps[0], doc.Steps[1]
	as, _ := composed.Extra[ExtraAgentSession].(map[string]any)
	if composed.Source != atif.SourceUser || !sameValue(t, as["instructions_part"], `[{"id":"/repo/CLAUDE.md","source":"agentsmd"},{"id":"~/.claude/memory/MEMORY.md","source":"agentmemory"}]`) {
		t.Errorf("the composed step is %+v", composed)
	}
	if as, _ := input.Extra[ExtraAgentSession].(map[string]any); input.Source != atif.SourceUser || as["instructions_part"] != nil {
		t.Errorf("the input step is %+v", input)
	}
}

// TestCostLeavesOutAPartlyPricedChild: a child with a model call the
// price source cannot price is left out of total_cost_usd whole, and the
// notes say so, rather than counted at the calls that were priced, which
// would understate it (RFC 0001, link, Cost across sessions: a reader
// leaves out a child it could not price rather than guess).
func TestCostLeavesOutAPartlyPricedChild(t *testing.T) {
	add := func(s *agentsession.Session, e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	usage := func(n int) *openresponses.Usage { return &openresponses.Usage{InputTokens: n, TotalTokens: n} }
	child := agentsession.New(agentsession.Header{ID: "child", ParentSession: "parent"})
	add(child, &agentsession.ConfigEntry{Model: "priced"})
	add(child, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("one"), ResponseID: "r1"})
	add(child, &agentsession.ResponseEntry{ResponseID: "r1", Usage: usage(10)})
	add(child, &agentsession.ConfigEntry{Model: "unpriced"})
	add(child, agentsession.NewItemEntry(openresponses.UserText("again")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("two"), ResponseID: "r2"})
	add(child, &agentsession.ResponseEntry{ResponseID: "r2", Usage: usage(20)})

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "priced"})
	add(parent, agentsession.NewItemEntry(openresponses.UserText("delegate")))
	add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{}`}, ResponseID: "p1"})
	add(parent, &agentsession.ResponseEntry{ResponseID: "p1", Usage: usage(1)})
	add(parent, agentsession.NewSubsessionLink("child", "call_1"))
	add(parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "done")))
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{
		Subsessions: func(id string) (*agentsession.Session, error) {
			if id == "child" {
				return child, nil
			}
			return nil, nil
		},
		Cost: func(model string, u openresponses.Usage) (float64, bool) {
			return float64(u.InputTokens), model == "priced"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 1 {
		got := -1.0
		if fm.TotalCostUSD != nil {
			got = *fm.TotalCostUSD
		}
		t.Errorf("total_cost_usd = %g, want 1: the path's alone, the partly priced child left out", got)
	}
	if !strings.Contains(doc.Notes, "excludes 1 delegated session(s) not read or not priced") || strings.Contains(doc.Notes, "includes the cost of") {
		t.Errorf("notes = %q", doc.Notes)
	}
}

// TestCostCountsEveryPricedChild: a child is counted when every call of
// it that records usage is priced, so a child with no calls counts as 0
// and one whose other call records no usage counts at its priced calls;
// and a call of the path itself the price source cannot price is named
// in the notes rather than skipped without a word (RFC 0001, link, Cost
// across sessions, 0.18).
func TestCostCountsEveryPricedChild(t *testing.T) {
	add := func(s *agentsession.Session, e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	usage := func(n int) *openresponses.Usage { return &openresponses.Usage{InputTokens: n, TotalTokens: n} }
	empty := agentsession.New(agentsession.Header{ID: "empty", ParentSession: "parent"})
	add(empty, &agentsession.ConfigEntry{Model: "priced"})
	add(empty, agentsession.NewItemEntry(openresponses.UserText("task")))
	partial := agentsession.New(agentsession.Header{ID: "partial", ParentSession: "parent"})
	add(partial, &agentsession.ConfigEntry{Model: "priced"})
	add(partial, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(partial, &agentsession.ItemEntry{Item: openresponses.AssistantText("one"), ResponseID: "r1"})
	add(partial, &agentsession.ResponseEntry{ResponseID: "r1"})
	add(partial, agentsession.NewItemEntry(openresponses.UserText("again")))
	add(partial, &agentsession.ItemEntry{Item: openresponses.AssistantText("two"), ResponseID: "r2"})
	add(partial, &agentsession.ResponseEntry{ResponseID: "r2", Usage: usage(20)})

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "priced"})
	add(parent, agentsession.NewItemEntry(openresponses.UserText("delegate")))
	for i, child := range []string{"empty", "partial"} {
		callID := fmt.Sprintf("call_%d", i)
		add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "spawn", Arguments: `{}`}, ResponseID: "p" + callID})
		add(parent, &agentsession.ResponseEntry{ResponseID: "p" + callID, Usage: usage(1)})
		add(parent, agentsession.NewSubsessionLink(child, callID))
		add(parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput(callID, "done")))
	}
	add(parent, &agentsession.ConfigEntry{Model: "unpriced"})
	add(parent, &agentsession.ItemEntry{Item: openresponses.AssistantText("all done"), ResponseID: "p_last"})
	add(parent, &agentsession.ResponseEntry{ResponseID: "p_last", Usage: usage(5)})
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	children := map[string]*agentsession.Session{"empty": empty, "partial": partial}
	doc, err := ToATIF(tr, Options{
		Subsessions: func(id string) (*agentsession.Session, error) { return children[id], nil },
		Cost: func(model string, u openresponses.Usage) (float64, bool) {
			return float64(u.InputTokens), model == "priced"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 22 {
		t.Errorf("total_cost_usd = %v, want 22: the path's 2 priced calls and the partial child's 20", fm.TotalCostUSD)
	}
	for _, want := range []string{"includes the cost of 2 delegated session(s)", "excludes 1 model call(s) of this path"} {
		if !strings.Contains(doc.Notes, want) {
			t.Errorf("notes %q say nothing of %q", doc.Notes, want)
		}
	}
	if strings.Contains(doc.Notes, "delegated session(s) not read or not priced") {
		t.Errorf("notes %q leave a child out", doc.Notes)
	}
}

// TestRebuildEmbeddedChild: an embedded trajectory's trajectory_id is
// session/leaf, so it names its leaf under extra.agentsession.leaf, and
// a converter rebuilding a redacted child from the parent's document
// finds the whole path to that leaf and restores the child's header
// (RFC 0001, Projections/ATIF, 0.18).
func TestRebuildEmbeddedChild(t *testing.T) {
	child := redaction(t, "basic", "test/v1", nil, func(v string) string { return v })
	if child.Leaf() == "" {
		t.Fatal("the redacted child has no leaf")
	}
	doc := embeddedChildDoc(t, child)
	if len(doc.SubagentTrajectories) != 1 {
		t.Fatalf("embedded %d trajectories", len(doc.SubagentTrajectories))
	}
	sub := doc.SubagentTrajectories[0]
	if leaf, _ := sub.Extra[ExtraAgentSession].(map[string]any)[ExtraLeaf].(string); leaf != child.Leaf() {
		t.Errorf("the embedded trajectory names leaf %q, want %s", leaf, child.Leaf())
	}
	if r := rebuildDocument(t, encode(t, sub)); !r.whole || !r.restoresHeader() {
		t.Errorf("the embedded child: whole %v, verified %v", r.whole, r.verified)
	}
}

// TestCostPricedDocument reads the cost-priced fixtures (RFC 0001, link,
// Cost across sessions, 0.18): the document's cost is the path's 3 priced
// calls, the empty child's 0 and the partial child's 20, the child with
// a call the source cannot price left out whole, and the notes name it
// and the path call that could not be priced.
func TestCostPricedDocument(t *testing.T) {
	parent := loadFixture(t, "cost-priced")
	children := map[string]*agentsession.Session{}
	for _, name := range []string{"cost-priced-empty", "cost-priced-partial", "cost-priced-unpriced"} {
		s := loadFixture(t, name)
		children[s.Header().ID] = s
	}
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{
		Subsessions: func(id string) (*agentsession.Session, error) { return children[id], nil },
		Cost: func(model string, u openresponses.Usage) (float64, bool) {
			return float64(u.InputTokens), model == "priced"
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 23 {
		t.Errorf("total_cost_usd = %v, want 23", fm.TotalCostUSD)
	}
	for _, want := range []string{"includes the cost of 2 delegated session(s)", "excludes 1 delegated session(s) not read or not priced", "excludes 1 model call(s) of this path"} {
		if !strings.Contains(doc.Notes, want) {
			t.Errorf("notes %q say nothing of %q", doc.Notes, want)
		}
	}
}

// rebuildFixtures are the ATIF documents under testdata/rebuild, each
// with whether a converter restores its header (RFC 0001,
// Projections/ATIF, 0.18): a redacted session's document as exported; it
// changed in a step's message alone; with a step appended; without its
// leaf's entry; and a parent's document embedding a redacted child,
// whose own header is restored from the embedded trajectory.
var rebuildFixtures = []struct {
	name     string
	restores bool
}{{"verified", true}, {"visible-edit", false}, {"appended-step", false}, {"removed-leaf", false}, {"embedded-child", true}}

// TestRebuildFixtures writes the rebuild fixtures with -update and holds
// the reference rebuild to each.
func TestRebuildFixtures(t *testing.T) {
	red := redaction(t, "basic", "test/v1", nil, func(v string) string {
		if v == "4" {
			return "[n]"
		}
		return v
	})
	tr, err := At(red, red.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	raw := encode(t, mustDoc(t, tr))
	first := lid(t, red, "i0000001")
	docs := map[string][]byte{
		"verified": raw,
		"visible-edit": edit(t, raw, func(doc map[string]any) {
			for _, sv := range doc["steps"].([]any) {
				step := obj(t, sv)
				if obj(t, obj(t, step["extra"])[ExtraAgentSession])["entry_id"] == first {
					step["message"] = "[redacted]"
				}
			}
		}),
		"appended-step": edit(t, raw, func(doc map[string]any) {
			doc["steps"] = append(doc["steps"].([]any), map[string]any{"step_id": len(doc["steps"].([]any)) + 1, "source": "user", "message": "appended"})
		}),
		"removed-leaf": edit(t, raw, func(doc map[string]any) {
			delete(obj(t, obj(t, obj(t, doc["extra"])[ExtraAgentSession])[ExtraEntries]), red.Leaf())
		}),
		"embedded-child": encode(t, embeddedChildDoc(t, red)),
	}
	for _, f := range rebuildFixtures {
		path := filepath.Join("..", "testdata", "rebuild", f.name+".json")
		var buf bytes.Buffer
		if err := json.Indent(&buf, docs[f.name], "", "  "); err != nil {
			t.Fatal(err)
		}
		checkGolden(t, path, append(bytes.TrimRight(buf.Bytes(), "\n"), '\n'))
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if f.name == "embedded-child" {
			var parent map[string]any
			if err := json.Unmarshal(data, &parent); err != nil {
				t.Fatal(err)
			}
			subs, _ := parent["subagent_trajectories"].([]any)
			if len(subs) != 1 {
				t.Fatalf("embedded-child embeds %d trajectories", len(subs))
			}
			if data, err = json.Marshal(subs[0]); err != nil {
				t.Fatal(err)
			}
		}
		if r := rebuildDocument(t, data); r.restoresHeader() != f.restores {
			t.Errorf("%s: restores the header %v, want %v: whole %v, verified %v, strays %v", f.name, r.restoresHeader(), f.restores, r.whole, r.verified, r.strays)
		}
	}
}

// embeddedChildDoc is the document of a parent that delegated a call to
// child, whose output names child's leaf, with child embedded.
func embeddedChildDoc(t *testing.T, child *agentsession.Session) *atif.Trajectory {
	t.Helper()
	parent := agentsession.New(agentsession.Header{ID: "parent", CreatedAt: child.Header().CreatedAt})
	at := child.Header().CreatedAt
	add := func(e agentsession.Entry) {
		t.Helper()
		at = at.Add(time.Second)
		e.Base().Timestamp = at
		mustAppend(t, parent, e)
	}
	add(&agentsession.ConfigEntry{Model: "gpt-5"})
	add(agentsession.NewItemEntry(openresponses.UserText("delegate")))
	add(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{}`}, ResponseID: "p1"})
	add(&agentsession.ResponseEntry{ResponseID: "p1"})
	add(agentsession.NewSubsessionLink(child.Header().ID, "call_1"))
	out := agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "done"))
	out.Parents = []agentsession.EntryRef{{Session: child.Header().ID, Entry: child.Leaf()}}
	add(out)
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{Subsessions: func(id string) (*agentsession.Session, error) {
		if id == child.Header().ID {
			return child, nil
		}
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestRedactorAtExportIsSeen: the visible hashes are of the document as
// the export built it, before its redactors run, so a custom redactor
// that edits a step's message alone leaves that step failing its hash:
// the entry is not rebuilt to its id and the header is not restored,
// since the session no longer holds what the document shows (RFC 0001,
// Projections/ATIF, 0.18).
func TestRedactorAtExportIsSeen(t *testing.T) {
	red := redaction(t, "basic", "test/v1", nil, func(v string) string { return v })
	tr, err := At(red, red.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	first := lid(t, red, "i0000001")
	doc, err := ToATIF(tr, Options{Redactors: []Redactor{RedactorFunc(func(d *atif.Trajectory) error {
		for i := range d.Steps {
			if as, _ := d.Steps[i].Extra[ExtraAgentSession].(map[string]any); as["entry_id"] == first {
				d.Steps[i].Message = atif.Text("[redacted]")
			}
		}
		return nil
	})}})
	if err != nil {
		t.Fatal(err)
	}
	if r := rebuildDocument(t, encode(t, doc)); r.restoresHeader() || r.verified[first] {
		t.Errorf("a redactor at export editing a message alone: verified %v", r.verified)
	}
}

// TestStepExtraIsVisible: a step's extra holds copies of entry text, a
// queued input's item among them, which a reader of the document sees,
// so the step's hash covers it, all but the hash itself: the queued
// copy edited alone leaves the step failing its hash (RFC 0001,
// Projections/ATIF, 0.18).
func TestStepExtraIsVisible(t *testing.T) {
	s := loadFixture(t, "queued")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	raw := encode(t, mustDoc(t, tr))
	if r := rebuildDocument(t, raw); !r.restoresHeader() {
		t.Fatalf("the queued document as exported: whole %v, verified %v", r.whole, r.verified)
	}
	var edited string
	changed := edit(t, raw, func(doc map[string]any) {
		for _, sv := range doc["steps"].([]any) {
			step := obj(t, sv)
			queued, _ := obj(t, step["extra"])["queued"].([]any)
			if len(queued) == 0 {
				continue
			}
			content := obj(t, obj(t, queued[0])["item"])["content"].([]any)
			obj(t, content[0])["text"] = "[redacted]"
			edited, _ = obj(t, obj(t, step["extra"])[ExtraAgentSession])["entry_id"].(string)
			return
		}
		t.Fatal("no step carries a queued input")
	})
	if r := rebuildDocument(t, changed); r.restoresHeader() || r.verified[edited] {
		t.Errorf("a queued copy edited in a step's extra: verified %v", r.verified)
	}
}

// mediaDocument is the document of a session whose person sends an
// image inline, a base64 data URL that WriteDocument moves to a file.
func mediaDocument(t *testing.T) *atif.Trajectory {
	t.Helper()
	at := time.Date(2026, 11, 3, 15, 0, 0, 0, time.UTC)
	s := agentsession.New(agentsession.Header{ID: "01995b2a-0000-7000-8000-000000000055", CreatedAt: at})
	add := func(e agentsession.Entry) {
		t.Helper()
		at = at.Add(time.Second)
		e.Base().Timestamp = at
		mustAppend(t, s, e)
	}
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	add(&agentsession.ConfigEntry{Model: "gpt-5"})
	add(agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputText{Text: "What is this?"}, &openresponses.InputImage{ImageURL: png})))
	add(&agentsession.ItemEntry{Item: openresponses.AssistantText("A pixel."), ResponseID: "resp_1"})
	add(&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted})
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	return mustDoc(t, tr)
}

// TestRebuildSpilledMedia: WriteDocument moves inline media to files
// after the hashes are computed, so a converter puts it back inline from
// the document's own files before it checks them, and the media fixture,
// untouched, then rebuilds every entry to its id (RFC 0001,
// Projections/ATIF, 0.18).
func TestRebuildSpilledMedia(t *testing.T) {
	dir := t.TempDir()
	if err := WriteDocument(filepath.Join(dir, "media.json"), mediaDocument(t)); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(filepath.Join(dir, "media.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixture := filepath.Join("..", "testdata", "rebuild")
	if *update {
		if err := WriteDocument(filepath.Join(fixture, "media.json"), mediaDocument(t)); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(filepath.Join(fixture, "media.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, written) {
		t.Error("testdata/rebuild/media.json is not what WriteDocument writes; run go test -update")
	}
	if !bytes.Contains(data, []byte(`"path": "images/`)) {
		t.Fatal("the media fixture holds no spilled image")
	}
	r := rebuildDocumentIn(t, data, fixture)
	if !r.whole || !r.restoresHeader() {
		t.Errorf("the media fixture: whole %v, verified %v", r.whole, r.verified)
	}
}
