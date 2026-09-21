package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

var update = flag.Bool("update", false, "rewrite golden files")

func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden %s differs\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}

func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v", err)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("documents differ\nwant: %s\ngot:  %s", want, got)
	}
}

func encode(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestATIFGolden exports every leaf of every fixture and compares the
// documents against testdata/export. Every document must validate,
// and the raw items it carries must rebuild the path's item list byte
// for byte.
func TestATIFGolden(t *testing.T) {
	for _, name := range []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved"} {
		t.Run(name, func(t *testing.T) {
			s := loadFixture(t, name)
			n := 0
			for tr, err := range Trajectories(s) {
				if err != nil {
					t.Fatal(err)
				}
				n++
				doc, err := ToATIF(tr, Options{})
				if err != nil {
					t.Fatalf("ToATIF(%s): %v", tr.LeafID, err)
				}
				if err := doc.Validate(); err != nil {
					t.Errorf("%s: invalid document: %v", tr.LeafID, err)
				}
				if doc.HasUnknown() {
					t.Errorf("%s: document has unknown members", tr.LeafID)
				}
				got := encode(t, doc)
				checkGolden(t, filepath.Join("..", "testdata", "export", name+"."+tr.LeafID+".json"), got)

				// Lossless: the items ride in the extras, byte for byte.
				items, err := Items(doc)
				if err != nil {
					t.Fatalf("Items: %v", err)
				}
				wantItems, _ := json.Marshal(tr.Context.Items)
				gotItems, _ := json.Marshal(items)
				if !bytes.Equal(wantItems, gotItems) {
					t.Errorf("%s: items changed\nwant %s\ngot  %s", tr.LeafID, wantItems, gotItems)
				}
				// And structurally after a trip through the file form.
				var back atif.Trajectory
				if err := json.Unmarshal(got, &back); err != nil {
					t.Fatal(err)
				}
				items, err = Items(&back)
				if err != nil {
					t.Fatalf("Items after decode: %v", err)
				}
				gotItems, _ = json.Marshal(items)
				assertSameJSON(t, wantItems, gotItems)
				if err := back.Validate(); err != nil {
					t.Errorf("decoded document invalid: %v", err)
				}
			}
			if n != len(s.Leaves()) {
				t.Errorf("%d trajectories for %d leaves", n, len(s.Leaves()))
			}
		})
	}
}

func TestBasicMapping(t *testing.T) {
	s := loadFixture(t, "basic")
	var tr Trajectory
	for x, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	doc, err := ToATIF(tr, Options{Cost: func(model string, u openresponses.Usage) (float64, bool) {
		return float64(u.InputTokens)*0.001 + float64(u.OutputTokens)*0.002, true
	}})
	if err != nil {
		t.Fatal(err)
	}
	if doc.SessionID != s.ID() || doc.TrajectoryID != "o0000001" || doc.SchemaVersion != atif.SchemaVersion {
		t.Errorf("root = %+v", doc)
	}
	if doc.Agent.Name != "fixture" || doc.Agent.Version != "1" || doc.Agent.ModelName != "gpt-5" || len(doc.Agent.ToolDefinitions) != 1 {
		t.Errorf("agent = %+v", doc.Agent)
	}
	if !strings.Contains(string(doc.Agent.ToolDefinitions[0]), `"function":{"description":"Look something up","name":"lookup","parameters":{`) {
		t.Errorf("tool definition = %s", doc.Agent.ToolDefinitions[0])
	}
	if len(doc.Steps) != 3 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	user, call, final := doc.Steps[0], doc.Steps[1], doc.Steps[2]
	if user.Source != atif.SourceUser || user.Message.String() != "What is 2+2?" || user.Timestamp != "2026-09-17T12:00:01Z" {
		t.Errorf("user step = %+v", user)
	}
	if call.Source != atif.SourceAgent || call.ModelName != "gpt-5" || call.ReasoningEffort != "low" || call.ReasoningContent != "Arithmetic, but the tool is there." {
		t.Errorf("call step = %+v", call)
	}
	if len(call.ToolCalls) != 1 || call.ToolCalls[0].ToolCallID != "call_1" || call.ToolCalls[0].FunctionName != "lookup" || call.ToolCalls[0].Arguments["q"] != "2+2" {
		t.Errorf("tool calls = %+v", call.ToolCalls)
	}
	if call.Observation == nil || len(call.Observation.Results) != 1 || call.Observation.Results[0].SourceCallID != "call_1" || call.Observation.Results[0].Content.String() != "4" {
		t.Errorf("observation = %+v", call.Observation)
	}
	if *call.Metrics.PromptTokens != 40 || *call.Metrics.CompletionTokens != 12 || *call.Metrics.CachedTokens != 0 || *call.Metrics.CostUSD != 40*0.001+12*0.002 {
		t.Errorf("metrics = %+v", call.Metrics)
	}
	if call.Metrics.Extra["reasoning_tokens"] != 5 || *call.LLMCallCount != 1 || call.Timestamp != "2026-09-17T12:00:02.5Z" {
		t.Errorf("call step = %+v", call)
	}
	if final.Message.String() != "2+2 is 4." || *final.Metrics.CachedTokens != 40 || !reflect.DeepEqual(final.Extra["phases"], []string{"final_answer"}) {
		t.Errorf("final step = %+v", final)
	}
	if final.Extra[ExtraOutcome] == nil {
		t.Error("outcome not attached to its target step")
	}
	fm := doc.FinalMetrics
	if *fm.TotalPromptTokens != 100 || *fm.TotalCompletionTokens != 20 || *fm.TotalCachedTokens != 40 || *fm.TotalSteps != 3 || fm.TotalCostUSD == nil {
		t.Errorf("final metrics = %+v", fm)
	}
	if fm.Extra[ExtraOutcome] == nil {
		t.Error("outcome missing from final metrics")
	}
	as := doc.Extra[ExtraAgentSession].(map[string]any)
	if as["cwd"] != "/home/u/proj" || as["leaf"] != "o0000001" || as["format"] != s.Header().Format {
		t.Errorf("root agentsession extra = %v", as)
	}
	// Without a price source there is no cost.
	doc, err = ToATIF(tr, Options{AgentName: "custom", AgentVersion: "9"})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Steps[1].Metrics.CostUSD != nil || doc.FinalMetrics.TotalCostUSD != nil || doc.Agent.Name != "custom" || doc.Agent.Version != "9" {
		t.Errorf("doc without cost = %+v", doc)
	}
}

func TestCompactionMapping(t *testing.T) {
	s := loadFixture(t, "compaction")
	var tr Trajectory
	for x, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	doc, err := ToATIF(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	boundary := doc.Steps[0]
	if boundary.Source != atif.SourceSystem || boundary.IsCopiedContext == nil || !*boundary.IsCopiedContext {
		t.Errorf("boundary = %+v", boundary)
	}
	cm, _ := boundary.Extra[ExtraContextMgmt].(map[string]any)
	if cm["type"] != "compaction" || cm["boundary"] != "replace" || boundary.Extra["tokens_before"] != 5000 {
		t.Errorf("context management = %v", boundary.Extra)
	}
	if boundary.Observation.Results[0].Content.String() != "Summary: the user said first and the assistant said one." {
		t.Errorf("boundary observation = %+v", boundary.Observation)
	}
	// Kept entries are copied context; entries after the compaction are
	// not.
	var copied, fresh int
	for _, st := range doc.Steps[1:] {
		if st.IsCopiedContext != nil && *st.IsCopiedContext {
			copied++
		} else {
			fresh++
		}
	}
	if copied != 3 || fresh != 2 {
		t.Errorf("copied %d fresh %d", copied, fresh)
	}
	// The agent block reflects the checkpoint; the config replay after
	// the compaction reaches the last step.
	if doc.Agent.ModelName != "gpt-5-mini" {
		t.Errorf("agent model = %s", doc.Agent.ModelName)
	}
	last := doc.Steps[len(doc.Steps)-1]
	if last.Source != atif.SourceUser || last.Extra["config_entries"] == nil {
		t.Errorf("last step = %+v", last)
	}
}

func TestPreferencePairs(t *testing.T) {
	s := loadFixture(t, "branch")
	got := map[string]Trajectory{}
	for tr, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		got[tr.LeafID] = tr
	}
	if len(got) != 2 {
		t.Fatalf("trajectories = %v", got)
	}
	abandoned, continued := got["r0000002"], got["n0000001"]
	if abandoned.AbandonedAt != "r0000001" || len(abandoned.PreferredOver) != 0 || abandoned.Main {
		t.Errorf("abandoned = %+v", abandoned)
	}
	if continued.AbandonedAt != "" || !reflect.DeepEqual(continued.PreferredOver, []string{"r0000002"}) || !continued.Main {
		t.Errorf("continued = %+v", continued)
	}
	if continued.Name != "Branching demo" || continued.Labels["r0000001"] != "fork" {
		t.Errorf("name %q labels %v", continued.Name, continued.Labels)
	}
	doc, err := ToATIF(continued, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(doc.Extra[ExtraPreferredOver], []string{"r0000002"}) {
		t.Errorf("preferred_over = %v", doc.Extra[ExtraPreferredOver])
	}
	var summary *atif.Step
	for i := range doc.Steps {
		if doc.Steps[i].Extra[ExtraBranchFrom] != nil {
			summary = &doc.Steps[i]
		}
	}
	if summary == nil || summary.Source != atif.SourceSystem || summary.Extra[ExtraBranchFrom] != "r0000002" || summary.IsCopiedContext == nil {
		t.Errorf("branch summary step = %+v", summary)
	}
	// Labels and info land on the next step or the root.
	last := doc.Steps[len(doc.Steps)-1]
	if last.Extra["labels"] != nil {
		t.Error("labels after the last item should be on the root")
	}
	if doc.Extra["labels"] == nil || doc.Extra["info"] == nil {
		t.Errorf("root extra = %v", doc.Extra)
	}
	doc, err = ToATIF(abandoned, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if doc.Extra[ExtraAbandonedAt] != "r0000001" {
		t.Errorf("abandoned_at = %v", doc.Extra[ExtraAbandonedAt])
	}
}

func TestExtensionsMapping(t *testing.T) {
	s := loadFixture(t, "extensions")
	var tr Trajectory
	for x, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	doc, err := ToATIF(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	// user, namespaced item, agent, failed agent
	if len(doc.Steps) != 4 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	ns := doc.Steps[1]
	if ns.Source != atif.SourceSystem || ns.Extra["item_type"] != "agentturn:note" || ns.Message.String() != "<hi> & bye" || ns.Extra["visible"] != false {
		t.Errorf("namespaced item step = %+v", ns)
	}
	if ns.Extra["extensions"] == nil {
		t.Error("extension entry not attached to the next step")
	}
	agent := doc.Steps[2]
	if agent.Extra["custom"] == nil || agent.Extra[ExtraEnvironment] != nil {
		t.Errorf("agent step extra = %v", agent.Extra)
	}
	env, _ := doc.Extra[ExtraEnvironment].(map[string]any)
	if env == nil || env["cwd"] != "/home/u/proj" {
		t.Errorf("root environment = %v", doc.Extra[ExtraEnvironment])
	}
	failed := doc.Steps[3]
	if failed.Source != atif.SourceAgent || failed.Extra["status"] != "failed" || failed.Extra["error"] == nil || failed.Metrics != nil {
		t.Errorf("failed step = %+v", failed)
	}
	// The unresolved subsession link becomes a file reference on the
	// root because its call is not in this path.
	links, _ := doc.Extra["links"].([]any)
	if len(links) != 1 {
		t.Fatalf("links = %v", doc.Extra["links"])
	}
	ref := links[0].(map[string]any)["subagent_trajectory_ref"].(atif.SubagentTrajectoryRef)
	if ref.SessionID != "01995b2a-0000-7000-8000-0000000000aa" || ref.TrajectoryPath != "01995b2a-0000-7000-8000-0000000000aa.json" {
		t.Errorf("ref = %+v", ref)
	}
}

// TestSubsessions builds a parent session whose tool call spawned a
// child session and checks the child is embedded and referenced from
// the call's observation.
func TestSubsessions(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	child := agentsession.New(agentsession.Header{ID: "child", CreatedAt: ts, ParentSession: "parent"})
	mustAppend(t, child, &agentsession.ConfigEntry{Model: "gpt-5-mini"})
	mustAppend(t, child, agentsession.NewItemEntry(openresponses.UserText("sub task")))
	mustAppend(t, child, &agentsession.ItemEntry{Item: openresponses.AssistantText("sub done"), ResponseID: "resp_c"})
	mustAppend(t, child, &agentsession.ResponseEntry{ResponseID: "resp_c", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}})

	parent := agentsession.New(agentsession.Header{ID: "parent", CreatedAt: ts})
	mustAppend(t, parent, &agentsession.ConfigEntry{Model: "gpt-5"})
	mustAppend(t, parent, agentsession.NewItemEntry(openresponses.UserText("delegate")))
	mustAppend(t, parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{"task":"sub"}`}, ResponseID: "resp_p"})
	mustAppend(t, parent, &agentsession.ResponseEntry{ResponseID: "resp_p", Status: openresponses.ResponseStatusCompleted})
	mustAppend(t, parent, agentsession.NewSubsessionLink("child", "call_1"))
	mustAppend(t, parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "sub done")))
	mustAppend(t, parent, agentsession.NewSubsessionLink("missing", "call_1"))
	mustAppend(t, parent, agentsession.NewLinkEntry(agentsession.RelContinuedIn, "later"))

	var tr Trajectory
	for x, err := range Trajectories(parent) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	resolver := func(id string) (*agentsession.Session, error) {
		if id == "child" {
			return child, nil
		}
		return nil, nil
	}
	doc, err := ToATIF(tr, Options{Subsessions: resolver, Notes: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if doc.Notes != "n" || len(doc.SubagentTrajectories) != 1 {
		t.Fatalf("doc = %+v", doc)
	}
	sub := doc.SubagentTrajectories[0]
	if sub.SessionID != "child" || !strings.HasPrefix(sub.TrajectoryID, "child/") || len(sub.Steps) != 2 || sub.Notes != "" {
		t.Errorf("embedded = %+v", sub)
	}
	call := doc.Steps[1]
	if len(call.Observation.Results) != 1 {
		t.Fatalf("observation = %+v", call.Observation)
	}
	refs := call.Observation.Results[0].SubagentTrajectoryRef
	if len(refs) != 2 || refs[0].TrajectoryID != sub.TrajectoryID || refs[0].SessionID != "child" || refs[1].TrajectoryPath != "missing.json" {
		t.Errorf("refs = %+v", refs)
	}
	links, _ := doc.Extra["links"].([]any)
	if len(links) != 1 || links[0].(map[string]any)["rel"] != agentsession.RelContinuedIn {
		t.Errorf("links = %v", links)
	}
	// A link whose call has no observation yet still attaches.
	if call.Observation.Results[0].Content.String() != "sub done" {
		t.Errorf("observation content = %q", call.Observation.Results[0].Content.String())
	}
	// Without a resolver the reference is a file path, and writing the
	// parent and the child with WriteATIF makes that path exist.
	doc, err = ToATIF(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref := doc.Steps[1].Observation.Results[0].SubagentTrajectoryRef[0]
	if len(doc.SubagentTrajectories) != 0 || ref.TrajectoryPath != "child.json" {
		t.Errorf("unresolved doc = %+v", doc.Steps[1].Observation.Results[0])
	}
	dir := t.TempDir()
	var childDocs []*atif.Trajectory
	for ct, err := range Trajectories(child) {
		if err != nil {
			t.Fatal(err)
		}
		cd, err := ToATIF(ct, Options{})
		if err != nil {
			t.Fatal(err)
		}
		childDocs = append(childDocs, cd)
	}
	if err := WriteATIF(dir, func(yield func(*atif.Trajectory) bool) {
		if !yield(doc) {
			return
		}
		for _, cd := range childDocs {
			if !yield(cd) {
				return
			}
		}
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, ref.TrajectoryPath))
	if err != nil {
		t.Fatalf("referenced child document: %v", err)
	}
	found, err := atif.Parse(data)
	if err != nil || found.SessionID != "child" || !isMain(found) {
		t.Errorf("referenced document = %+v, %v", found, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "parent.json")); err != nil {
		t.Errorf("parent main document: %v", err)
	}
}

func TestOrphanOutputAndMultimodal(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s := agentsession.New(agentsession.Header{ID: "mm", CreatedAt: ts})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_x", "orphan")))
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserMessage(
		&openresponses.InputText{Text: "look"},
		&openresponses.InputImage{ImageURL: "data:image/png;base64,iVBORw0KGgo="},
		&openresponses.InputFile{Filename: "a.pdf"},
	)))
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.DeveloperText("dev note")))
	mustAppend(t, s, &agentsession.ItemEntry{Item: &openresponses.Message{Role: openresponses.RoleAssistant,
		Content: openresponses.Contents{&openresponses.Refusal{Refusal: "no"}}}})
	mustAppend(t, s, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_y", Name: "f", Arguments: "not json"}})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_y", "")))
	mustAppend(t, s, agentsession.NewItemEntry(&openresponses.ItemReference{ID: "msg_9"}))
	var tr Trajectory
	for x, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	doc, err := ToATIF(tr, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(doc.Steps) != 5 {
		t.Fatalf("steps = %d: %+v", len(doc.Steps), doc.Steps)
	}
	orphan := doc.Steps[0]
	if orphan.Source != atif.SourceSystem || orphan.Extra["orphan_call_id"] != "call_x" || orphan.Observation.Results[0].Content.String() != "orphan" {
		t.Errorf("orphan = %+v", orphan)
	}
	user := doc.Steps[1]
	if user.Message.Parts == nil || len(user.Message.Parts) != 3 || user.Message.Parts[1].Type != atif.PartImage || user.Message.Parts[1].Source.MediaType != "image/png" || user.Message.Parts[2].Text != "[file: a.pdf]" {
		t.Errorf("multimodal user = %+v", user.Message)
	}
	if !doc.HasMultimodalContent() {
		t.Error("HasMultimodalContent = false")
	}
	dev := doc.Steps[2]
	if dev.Source != atif.SourceSystem || dev.Extra["role"] != "developer" {
		t.Errorf("developer step = %+v", dev)
	}
	agent := doc.Steps[3]
	if agent.Source != atif.SourceAgent || agent.Message.String() != "[refusal] no" || agent.Metrics != nil || agent.Timestamp == "" {
		t.Errorf("agent without response = %+v", agent)
	}
	if agent.ToolCalls[0].Arguments["_arguments"] != "not json" || agent.Observation.Results[0].SourceCallID != "call_y" {
		t.Errorf("agent call = %+v", agent)
	}
	ref := doc.Steps[4]
	if ref.Extra["item_type"] != "item_reference" {
		t.Errorf("reference step = %+v", ref)
	}
	if got, _ := Items(doc); len(got) != 7 {
		t.Errorf("Items = %d", len(got))
	}
}

func TestItemsErrors(t *testing.T) {
	if _, err := Items(&atif.Trajectory{Steps: []atif.Step{{StepID: 1}}}); !errors.Is(err, ErrNoRawItems) {
		t.Errorf("Items(no raw) = %v", err)
	}
	bad := &atif.Trajectory{Steps: []atif.Step{{StepID: 1, Extra: map[string]any{ExtraOpenResponses: map[string]any{"item": "nope"}}}}}
	if _, err := Items(bad); err == nil {
		t.Error("Items accepted a non-object item")
	}
}

func TestRedactors(t *testing.T) {
	s := loadFixture(t, "extensions")
	var tr Trajectory
	for x, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		tr = x
	}
	doc, err := ToATIF(tr, Options{Redactors: []Redactor{
		Secrets("hello", "", "hi"),
		HomePaths("/home/u"),
		Environment(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(doc)
	text := string(out)
	for _, forbidden := range []string{`"hello"`, `"hi"`, "/home/u", "sha256:aaaa", `"environment"`} {
		if strings.Contains(text, forbidden) {
			t.Errorf("document still contains %s:\n%s", forbidden, text)
		}
	}
	if !strings.Contains(text, `[REDACTED]`) {
		t.Errorf("replacements missing:\n%s", text)
	}
	// HomePaths alone rewrites the working directory and file paths.
	homeOnly, err := ToATIF(tr, Options{Redactors: []Redactor{HomePaths("/home/u")}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ = json.Marshal(homeOnly)
	if strings.Contains(string(out), "/home/u") || !strings.Contains(string(out), `"~/proj"`) {
		t.Errorf("HomePaths:\n%s", out)
	}
	if doc.Steps[0].Message.String() != Replacement {
		t.Errorf("user message = %q", doc.Steps[0].Message.String())
	}
	// The raw item in the extras is redacted too, so Items yields the
	// redacted text.
	items, err := Items(doc)
	if err != nil {
		t.Fatal(err)
	}
	if items[0].(*openresponses.Message).Text() != Replacement {
		t.Errorf("raw item text = %q", items[0].(*openresponses.Message).Text())
	}
	// Longer secrets win over their substrings.
	d := &atif.Trajectory{SchemaVersion: atif.SchemaVersion, Agent: atif.Agent{Name: "a", Version: "1"},
		Steps: []atif.Step{{StepID: 1, Source: atif.SourceUser, Message: atif.Text("key=abc123 and abc")}}}
	if err := Redact(d, Secrets("abc", "abc123")); err != nil {
		t.Fatal(err)
	}
	if d.Steps[0].Message.String() != "key=[REDACTED] and [REDACTED]" {
		t.Errorf("message = %q", d.Steps[0].Message.String())
	}
	if err := Redact(d, HomePaths(""), HomePaths("/"), Secrets()); err != nil {
		t.Errorf("no-op redactors: %v", err)
	}
	failing := RedactorFunc(func(*atif.Trajectory) error { return errors.New("boom") })
	if _, err := ToATIF(tr, Options{Redactors: []Redactor{failing}}); err == nil {
		t.Error("ToATIF ignored a redactor error")
	}
}

func TestWriteATIF(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	s := agentsession.New(agentsession.Header{ID: "media/sess", CreatedAt: ts})
	png := "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserMessage(
		&openresponses.InputText{Text: "see"},
		&openresponses.InputImage{ImageURL: png},
		&openresponses.InputImage{ImageURL: "https://example.com/pic.jpg"},
	)))
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserMessage(&openresponses.InputImage{ImageURL: png})))
	var docs []*atif.Trajectory
	for tr, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		doc, err := ToATIF(tr, Options{})
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, doc)
	}
	// An audio part written by hand, since Open Responses has no audio
	// input part.
	docs[0].Steps[0].Observation = &atif.Observation{Results: []atif.ObservationResult{{Content: atif.Content{Parts: []atif.ContentPart{
		{Type: atif.PartAudio, Source: &atif.MediaSource{MediaType: "audio/wav", Path: "data:audio/wav;base64,UklGRg=="}},
	}}}}}
	dir := t.TempDir()
	if err := WriteATIF(dir, func(yield func(*atif.Trajectory) bool) {
		for _, d := range docs {
			if !yield(d) {
				return
			}
		}
		yield(nil)
	}); err != nil {
		t.Fatalf("WriteATIF: %v", err)
	}
	name := DocumentName(docs[0])
	if name != "media-sess.json" {
		t.Errorf("main document name = %s", name)
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	back, err := atif.Parse(data)
	if err != nil {
		t.Fatalf("written document: %v", err)
	}
	parts := back.Steps[0].Message.Parts
	if !strings.HasPrefix(parts[1].Source.Path, "images/") || !strings.HasSuffix(parts[1].Source.Path, ".png") {
		t.Errorf("image path = %s", parts[1].Source.Path)
	}
	if parts[2].Source.Path != "https://example.com/pic.jpg" || parts[2].Source.MediaType != "image/jpeg" {
		t.Errorf("remote image = %+v", parts[2].Source)
	}
	if back.Steps[1].Message.Parts[0].Source.Path != parts[1].Source.Path {
		t.Error("identical media should share one file")
	}
	img, err := os.ReadFile(filepath.Join(dir, parts[1].Source.Path))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(img, []byte("\x89PNG")) {
		t.Errorf("image bytes = %q", img[:8])
	}
	audio := back.Steps[0].Observation.Results[0].Content.Parts[0].Source.Path
	if !strings.HasPrefix(audio, "audio/") || !strings.HasSuffix(audio, ".wav") {
		t.Errorf("audio path = %s", audio)
	}
	if strings.Contains(string(data), "base64,") {
		t.Error("data URL left in the document")
	}
	// The in-memory document was rewritten too.
	if docs[0].Steps[0].Message.Parts[1].Source.Path != parts[1].Source.Path {
		t.Error("in-memory document not rewritten")
	}

	// Errors: a malformed data URL and an invalid document.
	bad := &atif.Trajectory{SchemaVersion: atif.SchemaVersion, Agent: atif.Agent{Name: "a", Version: "1"},
		Steps: []atif.Step{{StepID: 1, Source: atif.SourceUser, Message: atif.Content{Parts: []atif.ContentPart{
			{Type: atif.PartImage, Source: &atif.MediaSource{MediaType: "image/png", Path: "data:image/png;base64,***"}}}}}}}
	if err := WriteATIF(dir, one(bad)); err == nil {
		t.Error("WriteATIF accepted a malformed data URL")
	}
	invalid := &atif.Trajectory{SchemaVersion: atif.SchemaVersion, Agent: atif.Agent{Name: "a"}}
	if err := WriteATIF(dir, one(invalid)); err == nil {
		t.Error("WriteATIF accepted an invalid document")
	}
	if DocumentName(&atif.Trajectory{}) != "trajectory.json" || DocumentName(&atif.Trajectory{TrajectoryID: "a b"}) != "a-b.json" {
		t.Error("DocumentName")
	}
	side := &atif.Trajectory{SessionID: "s", TrajectoryID: "leaf"}
	if DocumentName(side) != "s_leaf.json" || MainDocumentName("s/x") != "s-x.json" {
		t.Errorf("DocumentName(side) = %s", DocumentName(side))
	}
	if extensionFor("image/svg+xml") != ".svg-xml" || extensionFor("weird") != ".bin" || extensionFor("audio/mpeg") != ".mp3" {
		t.Error("extensionFor")
	}
	if _, _, err := decodeDataURL("data:nope"); err == nil {
		t.Error("decodeDataURL accepted a URL without a comma")
	}
	if mt, d, err := decodeDataURL("data:text/plain,hi"); err != nil || mt != "text/plain" || string(d) != "hi" {
		t.Errorf("plain data URL = %s %q %v", mt, d, err)
	}
}

func one(doc *atif.Trajectory) func(func(*atif.Trajectory) bool) {
	return func(yield func(*atif.Trajectory) bool) { yield(doc) }
}

func TestTrajectoriesErrors(t *testing.T) {
	// A compaction whose first_kept is off the path yields an error for
	// that leaf but the iteration continues.
	f, err := os.Open(filepath.Join("..", "testdata", "sessions", "bad-first-kept.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	var errs int
	for _, err := range Trajectories(s) {
		if err != nil {
			errs++
		}
	}
	if errs != 1 {
		t.Errorf("errors = %d", errs)
	}
	// An empty session has no trajectories.
	for range Trajectories(agentsession.New(agentsession.Header{})) {
		t.Error("empty session yielded a trajectory")
	}
}

func mustAppend(t *testing.T, s *agentsession.Session, e agentsession.Entry) {
	t.Helper()
	if _, err := s.Append(e); err != nil {
		t.Fatal(err)
	}
}

func TestPreferences(t *testing.T) {
	// The branch fixture forks at r0000001: i0000003 leads to leaf
	// r0000002 and b0000001 to leaf n0000001, the last appended.
	const fork, sideA, sideB = "r0000001", "i0000003", "b0000001"
	children := []string{sideA, sideB}
	tests := []struct {
		name    string
		prepare func(t *testing.T, s *agentsession.Session)
		prefs   []Preference
		want    string // the continued child
		opinion string // what the first preference alone answers
	}{
		{name: "default", want: sideB},
		{name: "latest", prefs: []Preference{PreferLatest}, want: sideB, opinion: sideB},
		{name: "no opinion falls back", prefs: []Preference{func(*agentsession.Session, string, []string) string { return "" }}, want: sideB},
		{name: "unknown child falls back", prefs: []Preference{func(*agentsession.Session, string, []string) string { return "zz" }}, want: sideB, opinion: "zz"},
		{name: "current leaf", prefs: []Preference{PreferCurrentLeaf}, want: sideB, opinion: sideB},
		{
			name:    "current leaf after switching back",
			prepare: func(t *testing.T, s *agentsession.Session) { mustDo(t, s.Branch("r0000002")) },
			prefs:   []Preference{PreferCurrentLeaf},
			want:    sideA, opinion: sideA,
		},
		{name: "label absent", prefs: []Preference{PreferLabel("best")}, want: sideB},
		{
			name: "label",
			prepare: func(t *testing.T, s *agentsession.Session) {
				_, err := s.Append(agentsession.NewLabelEntry("i0000004", "best"))
				mustDo(t, err)
			},
			prefs: []Preference{PreferLabel("best")},
			want:  sideA, opinion: sideA,
		},
		{
			name: "label on both sides",
			prepare: func(t *testing.T, s *agentsession.Session) {
				_, err := s.Append(agentsession.NewLabelEntry("i0000004", "best"))
				mustDo(t, err)
				_, err = s.Append(agentsession.NewLabelEntry("i0000006", "best"))
				mustDo(t, err)
			},
			prefs: []Preference{PreferLabel("best")},
			want:  sideB,
		},
		{name: "score absent", prefs: []Preference{PreferScore}, want: sideB},
		{
			name: "score",
			prepare: func(t *testing.T, s *agentsession.Session) {
				_, err := s.Append(agentsession.NewOutcomeEntry("test", "r0000002").WithScore(1))
				mustDo(t, err)
				_, err = s.Append(agentsession.NewOutcomeEntry("test", "r0000003").WithScore(0.5))
				mustDo(t, err)
			},
			prefs: []Preference{PreferScore},
			want:  sideA, opinion: sideA,
		},
		{
			name: "score tie",
			prepare: func(t *testing.T, s *agentsession.Session) {
				_, err := s.Append(agentsession.NewOutcomeEntry("test", "r0000002").WithScore(1))
				mustDo(t, err)
				_, err = s.Append(agentsession.NewOutcomeEntry("test", "").WithScore(1)) // its own position, side B
				mustDo(t, err)
			},
			prefs: []Preference{PreferScore},
			want:  sideB,
		},
		{
			name: "first opinion wins",
			prepare: func(t *testing.T, s *agentsession.Session) {
				_, err := s.Append(agentsession.NewLabelEntry("i0000004", "best"))
				mustDo(t, err)
			},
			prefs: []Preference{PreferScore, PreferLabel("best"), PreferLatest},
			want:  sideA,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := loadFixture(t, "branch")
			if tt.prepare != nil {
				tt.prepare(t, s)
			}
			if len(tt.prefs) > 0 {
				if got := tt.prefs[0](s, fork, children); got != tt.opinion {
					t.Errorf("preference answered %q, want %q", got, tt.opinion)
				}
			}
			got := map[string]Trajectory{}
			for tr, err := range Trajectories(s, tt.prefs...) {
				if err != nil {
					t.Fatal(err)
				}
				got[tr.LeafID] = tr
			}
			// The leaf under the continued child has no AbandonedAt and
			// lists the other side; the other leaf names the fork.
			for leaf, tr := range got {
				under := childHolding(s, children, leaf)
				if under == tt.want {
					if tr.AbandonedAt != "" || len(tr.PreferredOver) == 0 {
						t.Errorf("continued leaf %s = %+v", leaf, tr)
					}
				} else if tr.AbandonedAt != fork || len(tr.PreferredOver) != 0 {
					t.Errorf("abandoned leaf %s = %+v", leaf, tr)
				}
			}
			// Main follows the file, not the preference.
			if last := s.Entries()[s.Len()-1].Base().ID; !got[last].Main {
				t.Errorf("main trajectory is not at the last appended entry %s", last)
			}
		})
	}
}

func mustDo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestExportKeepsUnknownMembers pushes one undefined member through
// every entry type and expects each to reach the document, since the
// format's forward compatibility is worthless to a consumer that reads
// the export rather than the file.
func TestExportKeepsUnknownMembers(t *testing.T) {
	s := agentsession.New(agentsession.Header{ID: "unk", Records: agentsession.AllRecords})
	fc := &openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "t", Arguments: "{}"}
	entries := []agentsession.Entry{
		&agentsession.ConfigEntry{Model: "m"},
		&agentsession.InfoEntry{Name: "n"},
		agentsession.NewEnvEntry("/w"),
		agentsession.NewRunStart("r", agentsession.SourceInput, ""),
		agentsession.NewItemEntry(openresponses.UserText("hi")),
		&agentsession.ItemEntry{EntryBase: agentsession.EntryBase{ID: "fc-entry"}, Item: fc, ResponseID: "resp"},
		&agentsession.ResponseEntry{ResponseID: "resp", Status: "completed"},
		agentsession.NewDecision("call_1", "fc-entry", agentsession.VerdictProceed, agentsession.ByPolicy),
		agentsession.NewDispatch("call_1", "fc-entry"),
		agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "ok")),
		agentsession.NewRunEnd("r", agentsession.ReasonStopped, "", nil),
		agentsession.NewLabelEntry("", "bookmark"),
		&agentsession.CustomEntry{NS: "acme", Data: json.RawMessage(`1`)},
		agentsession.NewOutcomeEntry(agentsession.OutcomeTest, ""),
		agentsession.NewLinkEntry(agentsession.RelForkOf, "other"),
	}
	var want []string
	for i, e := range entries {
		key := fmt.Sprintf("acme:probe_%s_%d", e.EntryType(), i)
		val := fmt.Sprintf(`"v%d"`, i)
		e.Base().Unknown = map[string]json.RawMessage{key: json.RawMessage(val)}
		if _, err := s.Append(e); err != nil {
			t.Fatalf("%s: %v", e.EntryType(), err)
		}
		want = append(want, fmt.Sprintf(`%q:%s`, key, val))
	}
	var doc *atif.Trajectory
	for tr, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		d, err := ToATIF(tr, Options{})
		if err != nil {
			t.Fatal(err)
		}
		doc = d
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range want {
		if !strings.Contains(string(out), w) {
			t.Errorf("document lacks %s", w)
		}
	}
}
