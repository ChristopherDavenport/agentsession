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
