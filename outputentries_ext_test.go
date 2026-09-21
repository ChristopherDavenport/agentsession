package agentsession_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// TestOutputEntriesFromOutside is the mechanical test of whether the
// export is worth having. The rule that selects a response's own
// output items is implemented twice in the workspace: here, where
// Session.RequestContext excludes those entries from the rebuilt
// request, and in agenteval's replay, where NewModel serves their
// items to a replayed run. The two must agree or one file rebuilds
// two different requests, and until now a comment said so where a
// compiler could not.
//
// This test is deliberately in the external test package: it may use
// only what a second repository can use. It reproduces the shape of
// agenteval's NewModel loop — one pass over a path it already holds,
// folding settings in path order, taking each response's output from
// the entries before it by index — and checks that what that reader
// would serve is exactly what RequestContext dropped. If this
// compiles and passes, agenteval can delete its own walk.
func TestOutputEntriesFromOutside(t *testing.T) {
	for _, name := range []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved", "instructions", "queued", "resume", "pinned"} {
		t.Run(name, func(t *testing.T) {
			s := readSession(t, name)
			for _, leaf := range s.Leaves() {
				path := s.Path(leaf)
				// The settings fold is here because it is what makes
				// the serving reader's pass single, and the helper was
				// shaped against that. It asserts nothing on its own:
				// settings are not the thing being shared.
				var settings agentsession.Settings
				for i, e := range path {
					switch v := e.(type) {
					case *agentsession.ConfigEntry:
						settings = settings.Apply(v)
					case *agentsession.ResponseEntry:
						// The serving reader's line, in place of its
						// own backward walk.
						var served openresponses.Items
						for _, out := range agentsession.OutputEntries(path[:i], v) {
							// The entries are the session's own, not
							// copies: this reader has to clone before
							// it serves, and it can only know that if
							// the contract holds.
							own, ok := s.Entry(out.ID)
							if !ok || own != agentsession.Entry(out) {
								t.Errorf("%s: OutputEntries returned a copy of %s, not the session's own entry", v.ID, out.ID)
							}
							served = append(served, out.Item)
						}

						req, err := s.RequestContext(v.ID)
						if err != nil {
							t.Fatalf("RequestContext(%s): %v", v.ID, err)
						}
						full, err := s.ContextAt(v.ID)
						if err != nil {
							t.Fatalf("ContextAt(%s): %v", v.ID, err)
						}
						if len(full.Items) != len(req.Items)+len(served) {
							t.Fatalf("%s: context has %d items, request %d, served %d: the two readers disagree",
								v.ID, len(full.Items), len(req.Items), len(served))
						}
						// And in the same order: the served items are
						// the tail of the context the request is the
						// head of.
						for j, item := range served {
							if got := full.Items[len(req.Items)+j]; got != item {
								t.Errorf("%s: served item %d is not the one the context ends with", v.ID, j)
							}
						}
					}
				}
				_ = settings
			}
		})
	}
}

func readSession(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatalf("Read %s: %v", name, err)
	}
	return s
}
