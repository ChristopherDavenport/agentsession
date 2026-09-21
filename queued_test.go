package agentsession

import (
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestQueuedFixture reads the shape a gateway that answers 202 leaves
// behind: an input accepted while a run was in flight, drained into
// the conversation, and a second that is still waiting.
func TestQueuedFixture(t *testing.T) {
	s := loadFixture(t, "queued")
	tests := []struct {
		name, leaf string
		want       []string
	}{
		{"the steer was drained before the run ended", "i0000003", nil},
		{"the follow-up is still owed", s.Leaf(), []string{"q0000002"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queued, err := s.PendingQueued(tt.leaf)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, q := range queued {
				got = append(got, q.ID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("PendingQueued = %v, want %v", got, tt.want)
			}
		})
	}

	// The queued entries carry no item into the request: the one that
	// was drained is in context through the item entry that drained
	// it, and the one still waiting is in context nowhere.
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := itemTexts(ctx.Items); len(got) != 3 {
		t.Fatalf("context items = %q", got)
	}
	for _, text := range itemTexts(ctx.Items) {
		if strings.Contains(text, "tag the release") {
			t.Errorf("a queued input reached the context: %q", text)
		}
	}
	if n := strings.Count(strings.Join(itemTexts(ctx.Items), "|"), "skip the smoke tests"); n != 1 {
		t.Errorf("the drained input is in context %d times", n)
	}

	// The item that drained it says where it came from.
	e, ok := s.Entry("i0000002")
	if !ok {
		t.Fatal("no drained item")
	}
	item := e.(*ItemEntry)
	if item.QueuedFrom != "q0000001" {
		t.Errorf("queued_from = %q", item.QueuedFrom)
	}
	if item.Source == nil || item.Source.Kind != "human" || item.Source.Ref != "slack:1758412800.0002" || item.Source.Source != "gateway" {
		t.Errorf("source = %+v", item.Source)
	}
	if !s.Header().HasRecord(TypeQueued) {
		t.Error("the header does not promise queued entries")
	}
}

// TestQueuedDrain is the inbox a harness keeps: accept, resume, drain.
func TestQueuedDrain(t *testing.T) {
	s := New(Header{Records: append(AllRecords, TypeQueued)})
	if _, err := s.Append(&ConfigEntry{Model: "gpt-5"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewRunStart("run-1", SourceInput, "cron:nightly")); err != nil {
		t.Fatal(err)
	}
	q := NewQueued(openresponses.UserText("and skip the smoke tests"), ModeSteer).
		WithTrigger("human", "slack:1", "gateway")
	q.Ref = "inbox-1"
	if _, err := s.Append(q); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingQueued(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Ref != "inbox-1" {
		t.Fatalf("pending = %+v", pending)
	}
	// Draining it appends the item with the trigger that brought it.
	item := pending[0].Drain()
	if _, err := s.Append(item); err != nil {
		t.Fatal(err)
	}
	if item.QueuedFrom != q.ID || item.Source == nil || *item.Source != *q.Trigger {
		t.Errorf("drained item = %+v", item)
	}
	if item.Source == q.Trigger {
		t.Error("the drained item shares the queued entry's trigger")
	}
	if pending, _ := s.PendingQueued(s.Leaf()); len(pending) != 0 {
		t.Errorf("%d inputs still owed after draining", len(pending))
	}

	// A run end closes the inbox: an input queued into a run that has
	// ended is not waiting on that run any more.
	q2 := NewQueued(openresponses.UserText("then tag the release"), ModeFollowUp)
	if _, err := s.Append(q2); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingQueued(s.Leaf()); len(pending) != 1 {
		t.Fatalf("the queued follow-up is not owed")
	}
	end, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(end); err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.PendingQueued(s.Leaf()); len(pending) != 0 {
		t.Errorf("%d inputs owed after the run ended", len(pending))
	}
	if _, err := s.PendingQueued("nope"); err == nil {
		t.Error("PendingQueued accepted an unknown leaf")
	}
}

// TestQueuedValidation: the two members the format requires.
func TestQueuedValidation(t *testing.T) {
	tests := []struct {
		name  string
		entry *QueuedEntry
		ok    bool
	}{
		{"an item and a mode", NewQueued(openresponses.UserText("hi"), ModeSteer), true},
		{"no mode", NewQueued(openresponses.UserText("hi"), ""), false},
		{"no item", NewQueued(nil, ModeFollowUp), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Header{})
			_, err := s.Append(tt.entry)
			if tt.ok != (err == nil) {
				t.Errorf("Append = %v", err)
			}
			if !tt.ok {
				return
			}
			line, err := MarshalEntry(tt.entry)
			if err != nil {
				t.Fatal(err)
			}
			back, err := UnmarshalEntry(line)
			if err != nil {
				t.Fatal(err)
			}
			again, err := MarshalEntry(back)
			if err != nil {
				t.Fatal(err)
			}
			if string(line) != string(again) {
				t.Errorf("round trip changed the entry:\n%s\n%s", line, again)
			}
		})
	}
}
