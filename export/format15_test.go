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
