package export

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

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
