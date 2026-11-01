package export

import (
	"testing"

	"github.com/ChristopherDavenport/agentsession/atif"
)

// TestCopiedSteps: the copied fixture's child exports the parent's user
// message as a copied user step and the parent's reasoning and helper
// call as one copied agent step, with no model_name, metrics or
// llm_call_count, the placeholder output as that step's observation,
// and copied_from under extra.agentsession; total_steps counts the
// child's own response alone (RFC 0001, Projections/ATIF, 0.15).
func TestCopiedSteps(t *testing.T) {
	s := loadFixture(t, "copied")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) != 4 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	user, agent := doc.Steps[0], doc.Steps[1]
	if user.Source != atif.SourceUser || user.IsCopiedContext == nil || !*user.IsCopiedContext {
		t.Errorf("the copied user step is %+v", user)
	}
	if agent.Source != atif.SourceAgent || agent.IsCopiedContext == nil || !*agent.IsCopiedContext ||
		agent.ModelName != "" || agent.LLMCallCount != nil || agent.Metrics != nil || len(agent.ToolCalls) != 1 {
		t.Errorf("the copied agent step is %+v", agent)
	}
	if agent.Observation == nil || len(agent.Observation.Results) != 1 || agent.Observation.Results[0].SourceCallID != "call_helper" {
		t.Errorf("the copied call's observation is %+v", agent.Observation)
	}
	for i, step := range doc.Steps[:2] {
		as, _ := step.Extra[ExtraAgentSession].(map[string]any)
		if as["copied_from"] == nil {
			t.Errorf("step %d carries no copied_from", i+1)
		}
	}
	own := doc.Steps[3]
	if own.IsCopiedContext != nil || own.ModelName == "" || own.LLMCallCount == nil {
		t.Errorf("the child's own step is %+v", own)
	}
	if *doc.FinalMetrics.TotalSteps != 1 {
		t.Errorf("total_steps = %d, want the child's one response", *doc.FinalMetrics.TotalSteps)
	}
}

// TestResultTravelsWithItsCall: a call's result travels under
// calls[call_id].result with its time and failed, and with its item only
// when the document holds no output for the call, so a document cut
// before the append still says the tool returned (RFC 0001,
// Projections/ATIF, 0.15).
func TestResultTravelsWithItsCall(t *testing.T) {
	s := loadFixture(t, "returned")
	runs, err := s.Runs(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	result := func(doc *atif.Trajectory, callID string) map[string]any {
		for _, step := range doc.Steps {
			calls, _ := step.Extra[ExtraCalls].(map[string]any)
			if call, ok := calls[callID].(map[string]any); ok {
				r, _ := call["result"].(map[string]any)
				return r
			}
		}
		return nil
	}
	tr, err := At(s, runs[0].End.Parent)
	if err != nil {
		t.Fatal(err)
	}
	cut := mustDoc(t, tr)
	if r := result(cut, "call_a"); r == nil || r["item"] == nil || r["ts"] == nil || r["failed"] != nil {
		t.Errorf("at the cut call_a's result is %v", r)
	}
	if r := result(cut, "call_c"); r == nil || r["failed"] != true || r["item"] == nil {
		t.Errorf("at the cut call_c's result is %v", r)
	}
	if r := result(cut, "call_b"); r != nil {
		t.Errorf("call_b, still in flight, carries a result %v", r)
	}
	tr, err = At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if r := result(mustDoc(t, tr), "call_a"); r == nil || r["item"] != nil {
		t.Errorf("at the leaf, which holds the output, call_a's result is %v", r)
	}
}
