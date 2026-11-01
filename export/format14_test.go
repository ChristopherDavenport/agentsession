package export

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestCostIncludesEmbeddedSubagents: ATIF defines total_cost_usd to
// include subagents, so the root's is the path's cost plus each embedded
// document's, while the token totals and total_steps stay the path's;
// the notes say so, and say the cost leaves out a child not embedded
// (RFC 0001, Projections/ATIF; #220).
func TestCostIncludesEmbeddedSubagents(t *testing.T) {
	ts := time.Date(2026, 9, 17, 12, 0, 0, 0, time.UTC)
	child := agentsession.New(agentsession.Header{ID: "child", CreatedAt: ts, ParentSession: "parent", SpawnedBy: "call_1"})
	mustAppend(t, child, &agentsession.ConfigEntry{Model: "m"})
	mustAppend(t, child, agentsession.NewItemEntry(openresponses.UserText("sub task")))
	mustAppend(t, child, &agentsession.ItemEntry{Item: openresponses.AssistantText("sub done"), ResponseID: "resp_c"})
	mustAppend(t, child, &agentsession.ResponseEntry{ResponseID: "resp_c", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}})

	parent := agentsession.New(agentsession.Header{ID: "parent", CreatedAt: ts})
	mustAppend(t, parent, &agentsession.ConfigEntry{Model: "m"})
	mustAppend(t, parent, agentsession.NewItemEntry(openresponses.UserText("delegate")))
	mustAppend(t, parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{}`}, ResponseID: "resp_p"})
	mustAppend(t, parent, &agentsession.ResponseEntry{ResponseID: "resp_p", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}})
	mustAppend(t, parent, agentsession.NewSubsessionLink("child", "call_1"))
	mustAppend(t, parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "sub done")))
	mustAppend(t, parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_2", Name: "spawn", Arguments: `{}`}, ResponseID: "resp_q"})
	mustAppend(t, parent, &agentsession.ResponseEntry{ResponseID: "resp_q", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 20, OutputTokens: 1, TotalTokens: 21}})
	mustAppend(t, parent, agentsession.NewSubsessionLink("missing", "call_2"))
	mustAppend(t, parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_2", "lost")))

	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	resolver := func(id string) (*agentsession.Session, error) {
		if id == "child" {
			return child, nil
		}
		return nil, nil
	}
	cost := func(_ string, u openresponses.Usage) (float64, bool) { return float64(u.InputTokens), true }
	doc, err := ToATIF(tr, Options{Subsessions: resolver, Cost: cost})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(doc.SubagentTrajectories) != 1 || *doc.SubagentTrajectories[0].FinalMetrics.TotalCostUSD != 3 {
		t.Fatalf("embedded = %+v", doc.SubagentTrajectories)
	}
	fm := doc.FinalMetrics
	if fm.TotalCostUSD == nil || *fm.TotalCostUSD != 33 {
		t.Errorf("total_cost_usd = %v, want 33: the path's 30 and the child's 3", fm.TotalCostUSD)
	}
	if *fm.TotalPromptTokens != 30 || *fm.TotalSteps != 2 {
		t.Errorf("token totals and total_steps are the path's: prompt %d steps %d", *fm.TotalPromptTokens, *fm.TotalSteps)
	}
	for _, want := range []string{"includes the total_cost_usd of 1 embedded subagent", "excludes 1 subagent(s) not embedded"} {
		if !strings.Contains(doc.Notes, want) {
			t.Errorf("notes %q say nothing of %q", doc.Notes, want)
		}
	}

	// Unpriced, the document claims no cost and says nothing of one.
	doc, err = ToATIF(tr, Options{Subsessions: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if doc.FinalMetrics.TotalCostUSD != nil || strings.Contains(doc.Notes, "total_cost_usd") {
		t.Errorf("unpriced: cost %v notes %q", doc.FinalMetrics.TotalCostUSD, doc.Notes)
	}
}

// TestCustomTravelsWithItsCall: a custom entry whose call_id names a call
// whose agent step is in the document travels in that step's extra,
// under calls[call_id].custom in path order, and not on whichever step
// comes next; one naming no call, or a call with no step, travels as
// before (RFC 0001, Projections/ATIF; #224).
func TestCustomTravelsWithItsCall(t *testing.T) {
	s := agentsession.New(agentsession.Header{ID: "s"})
	mustAppend(t, s, &agentsession.ConfigEntry{Model: "m"})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserText("clean up")))
	mustAppend(t, s, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "sh", Arguments: `{"cmd":"rm -rf build"}`}, ResponseID: "resp_1"})
	mustAppend(t, s, &agentsession.ResponseEntry{ResponseID: "resp_1", Status: openresponses.ResponseStatusCompleted})
	mustAppend(t, s, &agentsession.CustomEntry{NS: "agentturn:elicitation", Data: json.RawMessage(`{"ask":"run it?"}`), CallID: "call_1"})
	mustAppend(t, s, &agentsession.CustomEntry{NS: "agentturn:elicitation", Data: json.RawMessage(`{"answer":"use make clean instead"}`), CallID: "call_1"})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "refused")))
	mustAppend(t, s, &agentsession.CustomEntry{NS: "x:note", Data: json.RawMessage(`{"n":1}`)})
	mustAppend(t, s, &agentsession.CustomEntry{NS: "x:note", Data: json.RawMessage(`{"n":2}`), CallID: "call_elsewhere"})
	mustAppend(t, s, &agentsession.ItemEntry{Item: openresponses.AssistantText("ran make clean"), ResponseID: "resp_2"})
	mustAppend(t, s, &agentsession.ResponseEntry{ResponseID: "resp_2", Status: openresponses.ResponseStatusCompleted})

	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) != 3 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	calls, _ := doc.Steps[1].Extra[ExtraCalls].(map[string]any)
	call, _ := calls["call_1"].(map[string]any)
	custom, _ := call["custom"].([]any)
	if len(custom) != 2 || string(custom[0].(map[string]any)["data"].(json.RawMessage)) != `{"ask":"run it?"}` || string(custom[1].(map[string]any)["data"].(json.RawMessage)) != `{"answer":"use make clean instead"}` {
		t.Errorf("the call's step carries custom %v", custom)
	}
	next, _ := doc.Steps[2].Extra["custom"].([]any)
	if len(next) != 2 || next[0].(map[string]any)["ns"] != "x:note" || next[1].(map[string]any)["call_id"] != "call_elsewhere" {
		t.Errorf("the next step carries custom %v, want the two that name no call in the document", next)
	}
}

// TestReplacedCarriesText: each request's placeholder record carries the
// text that request sent, which a later omit may change, so a reader
// rebuilds an earlier step's prompt and not the leaf's (RFC 0001,
// Projections/ATIF). The placeholders fixture clears outputs as
// "[cleared]" and later as "[cleared earlier]".
func TestReplacedCarriesText(t *testing.T) {
	s := loadFixture(t, "placeholders")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	texts := map[string]bool{}
	for _, step := range doc.Steps {
		as, _ := step.Extra[ExtraAgentSession].(map[string]any)
		list, _ := as[ExtraRequestReplaced].([]any)
		for _, r := range list {
			rec := r.(map[string]any)
			text, ok := rec["text"].(string)
			if !ok {
				t.Errorf("step %d: request_replaced %v carries no text", step.StepID, rec)
			}
			texts[rec["reason"].(string)+":"+text] = true
		}
	}
	for _, want := range []string{"outputs:[cleared]", "outputs:[cleared earlier]"} {
		if !texts[want] {
			t.Errorf("no step's request sent %s; texts %v", want, texts)
		}
	}
}
