package agentsession

import (
	"bytes"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestDurableLeaf branches back, marks the leaf, and expects a reopened
// session to resume from the mark rather than from the last line, with
// the live session hanging its next entry from the same place.
func TestDurableLeaf(t *testing.T) {
	s := New(Header{ID: "leaf"})
	first, _ := s.Append(NewItemEntry(openresponses.UserText("a")))
	if _, err := s.Append(NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(first); err != nil {
		t.Fatal(err)
	}
	mark, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	markID, err := s.Append(mark)
	if err != nil {
		t.Fatal(err)
	}
	if s.Leaf() != first {
		t.Fatalf("live leaf after mark = %s, want %s", s.Leaf(), first)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	again, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if again.Leaf() != first {
		t.Errorf("reopened leaf = %s, want %s", again.Leaf(), first)
	}
	// Both hang the next entry from the mark's target.
	liveID, _ := s.Append(NewItemEntry(openresponses.UserText("c")))
	reopenID, _ := again.Append(NewItemEntry(openresponses.UserText("c")))
	le, _ := s.Entry(liveID)
	re, _ := again.Entry(reopenID)
	if le.Base().Parent != first || re.Base().Parent != first {
		t.Errorf("parents = %s and %s, want %s", le.Base().Parent, re.Base().Parent, first)
	}
	if m, _ := s.Entry(markID); m.Base().Parent != first {
		t.Errorf("mark parent = %s, want %s", m.Base().Parent, first)
	}

	// Clearing the mark restores the last-line rule.
	if _, err := s.Append(NewLabelEntry(first, "")); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	cleared, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if cleared.Leaf() != s.Leaf() {
		t.Errorf("cleared leaf = %s, live %s", cleared.Leaf(), s.Leaf())
	}
	if _, err := New(Header{}).MarkLeaf(); err == nil {
		t.Error("MarkLeaf with no leaf succeeded")
	}
}

// markLeaf appends the durable leaf marker at the current leaf.
func markLeaf(t *testing.T, s *Session) {
	t.Helper()
	m, err := s.MarkLeaf()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(m); err != nil {
		t.Fatal(err)
	}
}

// appendText appends a user message and returns its entry ID.
func appendText(t *testing.T, s *Session, v string) string {
	t.Helper()
	id, err := s.Append(NewItemEntry(openresponses.UserText(v)))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// reopen writes the session and reads it back.
func reopen(t *testing.T, s *Session) *Session {
	t.Helper()
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	again, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return again
}

// TestLeafResolvesToTheBranch covers every shape a durable leaf mark can
// be in when a session is reopened. In all but the last, a reopen must
// land where the live session stood: the mark names the branch that is
// live, not a fixed entry, so appending past it moves the resumed leaf
// with it. The last is the case the mark exists for — the host left the
// marked branch without marking again, so the mark stands and the reopen
// does not follow work done elsewhere.
func TestLeafResolvesToTheBranch(t *testing.T) {
	for _, tc := range []struct {
		name string
		// build returns the leaf a reopen should resolve to, or "" for
		// the live leaf, which is the usual expectation.
		build func(t *testing.T, s *Session) string
	}{
		{"no mark at all", func(t *testing.T, s *Session) string {
			appendText(t, s, "a")
			appendText(t, s, "b")
			return ""
		}},
		{"mark with nothing after it", func(t *testing.T, s *Session) string {
			a := appendText(t, s, "a")
			appendText(t, s, "b")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			return ""
		}},
		{"mark then append", func(t *testing.T, s *Session) string {
			a := appendText(t, s, "a")
			appendText(t, s, "b")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			appendText(t, s, "c")
			appendText(t, s, "d")
			return ""
		}},
		{"mark, append, mark the same point again, append", func(t *testing.T, s *Session) string {
			a := appendText(t, s, "a")
			appendText(t, s, "b")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			appendText(t, s, "c")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			appendText(t, s, "d")
			return ""
		}},
		{"mark an interior entry with two branches below it", func(t *testing.T, s *Session) string {
			appendText(t, s, "a")
			x := appendText(t, s, "x")
			appendText(t, s, "p1")
			if err := s.Branch(x); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			appendText(t, s, "q1")
			appendText(t, s, "q2")
			return ""
		}},
		{"mark then cleared with a null label", func(t *testing.T, s *Session) string {
			a := appendText(t, s, "a")
			appendText(t, s, "b")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			if _, err := s.Append(NewLabelEntry(a, "")); err != nil {
				t.Fatal(err)
			}
			appendText(t, s, "c")
			return ""
		}},
		{"mark then work on a branch not under it", func(t *testing.T, s *Session) string {
			a := appendText(t, s, "a")
			y := appendText(t, s, "y")
			if err := s.Branch(a); err != nil {
				t.Fatal(err)
			}
			x := appendText(t, s, "x")
			if err := s.Branch(x); err != nil {
				t.Fatal(err)
			}
			markLeaf(t, s)
			if err := s.Branch(y); err != nil {
				t.Fatal(err)
			}
			appendText(t, s, "y2")
			// The mark is the last thing the file says about which
			// branch is live, and it is still x.
			return x
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Header{ID: "leaf"})
			want := tc.build(t, s)
			if want == "" {
				want = s.Leaf()
			}
			again := reopen(t, s)
			if again.Leaf() != want {
				t.Errorf("reopened leaf = %s, want %s (live %s)", again.Leaf(), want, s.Leaf())
			}
			wantCtx, err := s.ContextAt(want)
			if err != nil {
				t.Fatal(err)
			}
			gotCtx, err := again.Context()
			if err != nil {
				t.Fatal(err)
			}
			if len(gotCtx.Items) != len(wantCtx.Items) {
				t.Errorf("reopened context = %d items, want %d", len(gotCtx.Items), len(wantCtx.Items))
			}
		})
	}
}

// TestLeafResolvesPendingWork is the consequence that is not about
// context: every path-derived query reads the leaf, so a reopen that
// rewinds past a mark reports no pending call and no open run for work
// that was in flight. A dispatch is durable before its side effect, so a
// resume that cannot see the call cannot answer one that may have run.
func TestLeafResolvesPendingWork(t *testing.T) {
	s := New(Header{ID: "resume", Records: []string{"run", "dispatch"}})
	a := appendText(t, s, "a")
	appendText(t, s, "b")
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	markLeaf(t, s)

	if _, err := s.Append(&RunEntry{Phase: RunStart, RunID: "run-1", Source: "user"}); err != nil {
		t.Fatal(err)
	}
	fc := &openresponses.FunctionCall{ID: "fc_x", CallID: "call_x", Name: "tool", Arguments: "{}"}
	target, err := s.Append(&ItemEntry{Item: fc})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&DispatchEntry{CallID: "call_x", Target: target}); err != nil {
		t.Fatal(err)
	}
	// The record stops here: no output, no run end.

	again := reopen(t, s)
	if again.Leaf() != s.Leaf() {
		t.Fatalf("reopened leaf = %s, want %s", again.Leaf(), s.Leaf())
	}
	pending, err := again.PendingCalls(again.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID() != "call_x" {
		t.Errorf("pending calls = %v, want [call_x]", pending)
	}
	run, err := again.OpenRun(again.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if run == nil || run.RunID() != "run-1" {
		t.Errorf("open run = %v, want run-1", run)
	}
}
