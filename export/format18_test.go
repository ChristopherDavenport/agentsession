package export

import (
	"strings"
	"testing"

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
