package export

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// hostChildDocument exports the host-child fixture at leaf, its child
// resolved and model "priced" priced at a dollar an input token.
func hostChildDocument(t *testing.T, leaf func(*agentsession.Session) string) (*atif.Trajectory, *agentsession.Session) {
	t.Helper()
	parent, child := loadFixture(t, "host-child"), loadFixture(t, "host-child-spawned")
	tr, err := At(parent, leaf(parent))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{
		Subsessions: func(id string) (*agentsession.Session, error) {
			if id == child.ID() {
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
	return doc, child
}

// checkSubsessions holds a subsessions list to the one host-started
// child it names: its link's entry, spawn_id and session, and a
// reference to the child at the leaf the note after the link named.
func checkSubsessions(t *testing.T, where string, list any, child *agentsession.Session) {
	t.Helper()
	subs, _ := list.([]any)
	if len(subs) != 1 {
		t.Fatalf("%s: subsessions %v", where, list)
	}
	rec, _ := subs[0].(map[string]any)
	if rec["spawn_id"] != "0199a000-0000-7000-8000-000000000001" || rec["session"] != child.ID() || rec["entry_id"] == nil {
		t.Errorf("%s: record %v", where, rec)
	}
	ref, ok := rec["subagent_trajectory_ref"].(atif.SubagentTrajectoryRef)
	if !ok || ref.SessionID != child.ID() || ref.Extra["child_leaf"] != child.Leaf() || ref.Extra["child_leaf_named"] != true {
		t.Errorf("%s: reference %#v", where, rec["subagent_trajectory_ref"])
	}
}

// TestHostChildDocument: a subsession link with spawn_id travels under
// agentsession as subsessions in the extra of the next step, with a
// reference to the child at the leaf the note after the link names, the
// child is embedded, and the root's total_cost_usd counts it as
// delegated work (RFC 0001, Projections/ATIF, 0.19): the path's 3 and the
// child's 7.
func TestHostChildDocument(t *testing.T) {
	doc, child := hostChildDocument(t, (*agentsession.Session).Leaf)
	found := false
	for _, step := range doc.Steps {
		as, _ := step.Extra[ExtraAgentSession].(map[string]any)
		if list, ok := as["subsessions"]; ok {
			found = true
			if step.Source != atif.SourceUser {
				t.Errorf("subsessions on a %s step, want the user step after the link", step.Source)
			}
			checkSubsessions(t, "the next step", list, child)
		}
	}
	if !found {
		t.Error("no step carries subsessions")
	}
	if len(doc.SubagentTrajectories) != 1 || !strings.HasPrefix(doc.SubagentTrajectories[0].TrajectoryID, child.ID()+"/") {
		t.Errorf("subagent trajectories: %d", len(doc.SubagentTrajectories))
	}
	if links, ok := doc.Extra["links"]; ok {
		t.Errorf("the spawn link is in links too: %v", links)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 10 {
		t.Errorf("total_cost_usd = %v, want 10", fm.TotalCostUSD)
	}
	if !strings.Contains(doc.Notes, "includes the cost of 1 delegated session(s)") {
		t.Errorf("notes %q", doc.Notes)
	}
}

// TestHostChildDocumentNoStepAfter: built at the note that takes the
// child's work in, no step follows the link, so subsessions travel in
// the top-level extra, as a custom entry does.
func TestHostChildDocumentNoStepAfter(t *testing.T) {
	doc, child := hostChildDocument(t, func(s *agentsession.Session) string {
		for _, e := range s.Entries() {
			if c, ok := e.(*agentsession.CustomEntry); ok {
				return c.ID
			}
		}
		t.Fatal("no note")
		return ""
	})
	as, _ := doc.Extra[ExtraAgentSession].(map[string]any)
	checkSubsessions(t, "the top-level extra", as["subsessions"], child)
}
