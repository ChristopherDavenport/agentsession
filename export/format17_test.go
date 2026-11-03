package export

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// TestInstructionsPartStep: the instruction-layers fixture exports the
// message the harness composed from an instruction file as a user step
// carrying instructions_part under extra.agentsession, and the person's
// input as one without it (RFC 0001, Projections/ATIF, 0.17).
func TestInstructionsPartStep(t *testing.T) {
	s := loadFixture(t, "instruction-layers")
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
	if composed.Source != atif.SourceUser || !sameValue(t, as["instructions_part"], `{"id":"/repo/CLAUDE.md","source":"agentsmd"}`) {
		t.Errorf("the composed step is %+v", composed)
	}
	if as, _ := input.Extra[ExtraAgentSession].(map[string]any); input.Source != atif.SourceUser || as["instructions_part"] != nil {
		t.Errorf("the input step is %+v", input)
	}
}

// TestMembersInTheDocument is the document a converter reads back (RFC
// 0001, Projections/ATIF, 0.17): an entry's meaning members travel under
// extra.agentsession, as core members a converter restores; a member the
// format does not define travels as written in the extra of the step,
// observation result or record the entry maps to; and a response's
// history member, here legacy_id, travels in its body as written, for a
// converter to leave out of a new entry or to restore on one it rebuilds
// to the document's own id.
func TestMembersInTheDocument(t *testing.T) {
	ts := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := agentsession.New(agentsession.Header{ID: "s", CreatedAt: ts})
	probe := func(e agentsession.Entry, v string) agentsession.Entry {
		e.Base().Unknown = map[string]json.RawMessage{"x:probe": json.RawMessage(`"` + v + `"`)}
		return e
	}
	mustAppend(t, s, &agentsession.ConfigEntry{Model: "gpt-5"})
	mustAppend(t, s, &agentsession.ItemEntry{Item: openresponses.UserText("<memory/>"), InstructionsPart: &agentsession.LayerRef{Source: agentsession.LayerAgentMemory}})
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: openresponses.UserText("read it"), Trust: &agentsession.Trust{Reads: []string{agentsession.TrustUntrusted}}}, "input"))
	call, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "read", Arguments: `{}`}, ResponseID: "resp_1"})
	if err != nil {
		t.Fatal(err)
	}
	resp := probe(&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted}, "response")
	resp.Base().LegacyID = "r0000001"
	mustAppend(t, s, resp)
	mustAppend(t, s, probe(agentsession.NewDispatch("call_1", call), "dispatch"))
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "denied"), Failed: true}, "output"))
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) != 3 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	composed, input, agent := doc.Steps[0], doc.Steps[1], doc.Steps[2]
	if as, _ := composed.Extra[ExtraAgentSession].(map[string]any); !sameValue(t, as["instructions_part"], `{"source":"agentmemory"}`) {
		t.Errorf("the composed step's extra is %v", composed.Extra)
	}
	if as, _ := input.Extra[ExtraAgentSession].(map[string]any); !sameValue(t, as["trust"], `{"reads":["untrusted"]}`) || !sameValue(t, input.Extra["x:probe"], `"input"`) {
		t.Errorf("the input step's extra is %v", input.Extra)
	}
	if !sameValue(t, agent.Extra["x:probe"], `"response"`) {
		t.Errorf("the agent step's extra is %v", agent.Extra)
	}
	or, _ := agent.Extra[ExtraOpenResponses].(map[string]any)
	var body map[string]json.RawMessage
	if raw, ok := or["response"].(json.RawMessage); !ok || json.Unmarshal(raw, &body) != nil || string(body["legacy_id"]) != `"r0000001"` || string(body["x:probe"]) != `"response"` {
		t.Errorf("the response body is %v", or["response"])
	}
	if agent.Observation == nil || len(agent.Observation.Results) != 1 {
		t.Fatalf("observation = %+v", agent.Observation)
	}
	res := agent.Observation.Results[0]
	if as, _ := res.Extra[ExtraAgentSession].(map[string]any); as["failed"] != true || !sameValue(t, res.Extra["x:probe"], `"output"`) {
		t.Errorf("the output's extra is %v", res.Extra)
	}
	calls, _ := agent.Extra[ExtraCalls].(map[string]any)
	rec, _ := calls["call_1"].(map[string]any)
	dispatch, _ := rec["dispatch"].(map[string]any)
	if !sameValue(t, dispatch["x:probe"], `"dispatch"`) {
		t.Errorf("the call's record is %v", rec)
	}
}

// sameValue reports whether v encodes to the JSON value want.
func sameValue(t *testing.T, v any, want string) bool {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return string(ga) == string(gb)
}

// TestCostCountsAChildOnce: a child two links count at different leaves
// adds the union of its two paths to total_cost_usd, the prefix they
// share once, as RFC 0001's link rule counts delegated work, though the
// document embeds a trajectory for each leaf.
func TestCostCountsAChildOnce(t *testing.T) {
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
	add(child, &agentsession.ConfigEntry{Model: "m"})
	add(child, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("start"), ResponseID: "r1"})
	fork := add(child, &agentsession.ResponseEntry{ResponseID: "r1", Usage: usage(10)})
	add(child, agentsession.NewItemEntry(openresponses.UserText("one way")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("one"), ResponseID: "r2"})
	one := add(child, &agentsession.ResponseEntry{ResponseID: "r2", Usage: usage(20)})
	if err := child.Branch(fork); err != nil {
		t.Fatal(err)
	}
	add(child, agentsession.NewItemEntry(openresponses.UserText("the other")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("other"), ResponseID: "r3"})
	other := add(child, &agentsession.ResponseEntry{ResponseID: "r3", Usage: usage(30)})

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "m"})
	add(parent, agentsession.NewItemEntry(openresponses.UserText("delegate twice")))
	for i, leaf := range []string{one, other} {
		callID := fmt.Sprintf("call_%d", i)
		add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "spawn", Arguments: `{}`}, ResponseID: "p" + callID})
		add(parent, &agentsession.ResponseEntry{ResponseID: "p" + callID, Usage: usage(1)})
		add(parent, agentsession.NewSubsessionLink("child", callID))
		out := agentsession.NewItemEntry(openresponses.NewFunctionCallOutput(callID, "done"))
		out.Parents = []agentsession.EntryRef{{Session: "child", Entry: leaf}}
		add(parent, out)
	}
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
		Cost: func(_ string, u openresponses.Usage) (float64, bool) { return float64(u.InputTokens), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 62 {
		got := -1.0
		if fm.TotalCostUSD != nil {
			got = *fm.TotalCostUSD
		}
		t.Errorf("total_cost_usd = %g, want 62: the path's 2 and the child's 10, 20 and 30 once each", got)
	}
}

// TestEmbedTakesTheResumeLeaf: with no leaf named in parents, the child
// is embedded at the leaf a reader resuming it takes, which a branch or
// a leaf label moves off the last appended entry, and the reference
// records the leaf taken and that no output named it (RFC 0001, link).
func TestEmbedTakesTheResumeLeaf(t *testing.T) {
	add := func(s *agentsession.Session, e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	child := agentsession.New(agentsession.Header{ID: "child", ParentSession: "parent", SpawnedBy: "call_1"})
	add(child, &agentsession.ConfigEntry{Model: "m"})
	fork := add(child, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("kept"), ResponseID: "r1"})
	kept := add(child, &agentsession.ResponseEntry{ResponseID: "r1"})
	if err := child.Branch(fork); err != nil {
		t.Fatal(err)
	}
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("abandoned"), ResponseID: "r2"})
	add(child, &agentsession.ResponseEntry{ResponseID: "r2"})
	if err := child.Branch(kept); err != nil {
		t.Fatal(err)
	}

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "m"})
	add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{}`}, ResponseID: "p1"})
	add(parent, &agentsession.ResponseEntry{ResponseID: "p1"})
	add(parent, agentsession.NewSubsessionLink("child", "call_1"))
	add(parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "kept")))
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{Subsessions: func(id string) (*agentsession.Session, error) {
		if id == "child" {
			return child, nil
		}
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.SubagentTrajectories) != 1 || doc.SubagentTrajectories[0].TrajectoryID != "child/"+kept {
		t.Fatalf("embedded %d, the first %+v", len(doc.SubagentTrajectories), doc.SubagentTrajectories)
	}
	var ref *atif.SubagentTrajectoryRef
	for _, s := range doc.Steps {
		if s.Observation != nil {
			for _, r := range s.Observation.Results {
				if len(r.SubagentTrajectoryRef) > 0 {
					ref = &r.SubagentTrajectoryRef[0]
				}
			}
		}
	}
	if ref == nil || ref.Extra["child_leaf"] != kept || ref.Extra["child_leaf_named"] != false {
		t.Errorf("the reference is %+v", ref)
	}
}
