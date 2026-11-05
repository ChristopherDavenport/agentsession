package agentsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// queuedReminder is a hidden, untrusted input the harness composed from
// memory and queued while a run was in flight: the three members an
// Open Responses item cannot carry (agentsession#278).
func queuedReminder() *QueuedEntry {
	q := NewQueued(openresponses.UserText("Reminder from memory: the deploy window is Tuesday."), ModeSteer)
	q.Visible = ptr(false)
	q.Trust = &Trust{Reads: []string{TrustUntrusted}}
	q.InstructionsPart = &LayerRef{Source: LayerAgentMemory}
	return q
}

// TestQueuedMembers: since 0.21 a queued entry carries visible, trust and
// instructions_part, which survive a write and a read, and the item
// that drains the input and the entry that queues it again carry them as
// the queued entry wrote them. A queued entry's trust is the input's,
// and joins the path's only once the input is drained.
func TestQueuedMembers(t *testing.T) {
	s := New(Header{ID: "q", Records: []string{TypeQueued}})
	if _, err := s.Append(NewItemEntry(openresponses.UserText("Deploy billing."))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(queuedReminder()); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"visible":false`, `"trust":{"reads":["untrusted"]}`, `"instructions_part":{"source":"agentmemory"}`} {
		if !strings.Contains(buf.String(), member) {
			t.Errorf("the queued line does not carry %s:\n%s", member, buf.String())
		}
	}
	back, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := back.PendingQueued(back.Leaf())
	if err != nil || len(pending) != 1 {
		t.Fatalf("PendingQueued = %v, %v", pending, err)
	}
	q := pending[0]
	check := func(what string, visible *bool, trust *Trust, layers []LayerRef) {
		t.Helper()
		if visible == nil || *visible {
			t.Errorf("%s: visible %v, want false", what, visible)
		}
		if trust == nil || !slices.Equal(trust.Reads, []string{TrustUntrusted}) {
			t.Errorf("%s: trust %+v, want untrusted", what, trust)
		}
		if len(layers) != 1 || layers[0].Source != LayerAgentMemory {
			t.Errorf("%s: instructions_part %+v, want agentmemory", what, layers)
		}
	}
	check("read back", q.Visible, q.Trust, q.Layers())
	again := q.Requeue()
	check("requeued", again.Visible, again.Trust, again.Layers())
	if got := PathTrust(back.Path(back.Leaf())); got != nil {
		t.Errorf("a queued entry's trust joined the path's: %+v", got)
	}
	drained := q.Drain()
	check("drained", drained.Visible, drained.Trust, drained.Layers())
	if _, err := back.Append(drained); err != nil {
		t.Fatal(err)
	}
	if got := PathTrust(back.Path(back.Leaf())); got == nil || !slices.Equal(got.Reads, []string{TrustUntrusted}) {
		t.Errorf("the drained input's trust is not the path's: %+v", got)
	}
	if err := back.VerifyRecords(back.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}

	// A rewrite keeps the trust labels and a source the format defines,
	// as on an item.
	out, err := RewriteStrings(q, strings.NewReplacer("untrusted", "U", "agentmemory", "M", "Reminder", "Note").Replace)
	if err != nil {
		t.Fatal(err)
	}
	rq := out.(*QueuedEntry)
	check("rewritten", rq.Visible, rq.Trust, rq.Layers())
	if text := rq.Item.(*openresponses.Message).Content.Text(); !strings.HasPrefix(text, "Note") {
		t.Errorf("the rewrite left the item's text: %q", text)
	}
}

// TestDrainMismatch: an item that drains a queued input and drops or
// changes a member the queued entry carries is reported by
// VerifyRecords; one that carries a member its queued entry lacks, as a
// file written before 0.21 may, is not.
func TestDrainMismatch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		break_ func(*ItemEntry)
		ok     bool
	}{
		{"as queued", func(*ItemEntry) {}, true},
		{"visible dropped", func(it *ItemEntry) { it.Visible = nil }, false},
		{"trust dropped", func(it *ItemEntry) { it.Trust = nil }, false},
		{"trust narrowed", func(it *ItemEntry) { it.Trust = &Trust{Reads: []string{}} }, false},
		{"layer dropped", func(it *ItemEntry) { it.InstructionsPart = nil }, false},
		{"layer changed", func(it *ItemEntry) { it.InstructionsPart = &LayerRef{Source: LayerProduct} }, false},
	} {
		s := New(Header{ID: "q"})
		q := queuedReminder()
		if _, err := s.Append(q); err != nil {
			t.Fatal(err)
		}
		it := q.Drain()
		tt.break_(it)
		if _, err := s.Append(it); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyRecords(s.Leaf()); (err == nil) != tt.ok || err != nil && !errors.Is(err, ErrDrainMismatch) {
			t.Errorf("%s: VerifyRecords = %v", tt.name, err)
		}
	}
	// A queued entry that carries none of them, as one written before
	// 0.21, says nothing about the item that drains it.
	s := New(Header{ID: "q"})
	q := NewQueued(openresponses.UserText("pasted"), ModeFollowUp)
	if _, err := s.Append(q); err != nil {
		t.Fatal(err)
	}
	it := q.Drain()
	it.Trust = &Trust{Reads: []string{TrustUntrusted}}
	it.Visible = ptr(false)
	if _, err := s.Append(it); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("a drained item wider than a 0.20 queued entry: %v", err)
	}
}

// TestQueuedMemberForms: each member is taken in the form the item
// entry's is, in a file of any minor; another form is kept as written,
// and an instructions_part beside an item that is not a message too.
func TestQueuedMemberForms(t *testing.T) {
	msg := `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	out := `{"type":"function_call_output","call_id":"c","output":"x"}`
	for _, tt := range []struct {
		item, key, member string
		typed             bool
	}{
		{msg, "visible", "false", true},
		{msg, "visible", `"no"`, false},
		{msg, "trust", `{"reads":["private"]}`, true},
		{msg, "trust", `{"reads":"private"}`, false},
		{msg, "instructions_part", `{"source":"agentmemory"}`, true},
		{msg, "instructions_part", `[{"source":"agentmemory"},{"source":"product"}]`, true},
		{msg, "instructions_part", `{"id":""}`, false},
		{out, "instructions_part", `{"source":"agentmemory"}`, false},
	} {
		line := `{"type":"queued","id":"x","parent":null,"ts":"2026-11-04T10:00:00Z","item":` + tt.item + `,"mode":"steer","` + tt.key + `":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s %s: %v", tt.key, tt.member, err)
		}
		q := e.(*QueuedEntry)
		if _, kept := q.Unknown[tt.key]; kept == tt.typed {
			t.Errorf("%s %s: kept as unknown %v, want typed %v", tt.key, tt.member, kept, tt.typed)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, members(t, back)[tt.key], []byte(tt.member)) {
			t.Errorf("%s %s: written back as %s", tt.key, tt.member, back)
		}
	}
}

// fixtures021 are the 0.21 conformance fixtures, each built natively by
// its function, as for fixtures019.
var fixtures021 = map[string]func(t *testing.T) *Session{
	"queued-members":   func(t *testing.T) *Session { return queuedMembersFixture(t, false) },
	"bad-queued-drain": func(t *testing.T) *Session { return queuedMembersFixture(t, true) },
}

// writeFixtures021 writes the 0.21 fixtures, positive and negative.
func writeFixtures021(t *testing.T) {
	t.Helper()
	for name, build := range fixtures021 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// queuedMembersFixture builds the session queued-members.jsonl holds: a
// reminder the harness composed from memory, hidden and untrusted, is
// queued as a steer while the first run is in flight and drained into
// it; a block composed from an instruction file and memory, private,
// is queued as a follow-up, closed by the run's end, queued again and
// drained into the second run. With bad set, the second drain drops the
// queued entry's trust, which bad-queued-drain.jsonl holds.
func queuedMembersFixture(t *testing.T, bad bool) *Session {
	t.Helper()
	id := "01995b2a-0000-7000-8000-0000000000b5"
	if bad {
		id = "01995b2a-0000-7000-8000-0000000000b6"
	}
	s, must, say := fixtureSession(t, Header{ID: id, Records: []string{TypeRun, TypeQueued}}, "2026-11-04T09:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("You deploy services.")})
	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Deploy billing.")))
	reminder := queuedReminder().WithTrigger("schedule", "cron:deploy-window", "scheduler")
	must(reminder)
	must(reminder.Drain())
	say("resp_1", "Billing deploys Tuesday, in the window.")
	block := NewQueued(openresponses.UserText("From AGENTS.md and memory: the billing owner is on call."), ModeFollowUp).WithTrigger("hook", "session-start", "harness")
	block.Visible = ptr(false)
	block.Trust = &Trust{Reads: []string{TrustPrivate}}
	block.InstructionsLayers = []LayerRef{{ID: "AGENTS.md", Source: LayerAgentsMD}, {Source: LayerAgentMemory}}
	must(block)
	end, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(end)
	again := block.Requeue()
	must(again)
	must(NewRunStart("run-2", SourceInput, ""))
	drained := again.Drain()
	if bad {
		drained.Trust = nil
	}
	must(drained)
	say("resp_2", "Noted: the billing owner is on call.")
	if end, err = s.EndRun(ReasonDone, ""); err != nil {
		t.Fatal(err)
	}
	must(end)
	return s
}

// TestQueuedMembersFixture reads the 0.21 queued fixtures: each drained
// item carries the members its queued entry wrote, the input queued
// again carries them again, the path's trust holds a queued input's
// labels only once it is drained, and the file whose drain dropped the
// trust is reported.
func TestQueuedMembersFixture(t *testing.T) {
	checkBuilt(t, "queued-members")
	s := loadFixture(t, "queued-members")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	var queued []*QueuedEntry
	var drained []*ItemEntry
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *QueuedEntry:
			queued = append(queued, v)
		case *ItemEntry:
			if v.QueuedFrom != "" {
				drained = append(drained, v)
			}
		}
	}
	if len(queued) != 3 || len(drained) != 2 {
		t.Fatalf("%d queued, %d drained", len(queued), len(drained))
	}
	if queued[2].Visible == nil || *queued[2].Visible || !slices.Equal(queued[2].Trust.Reads, []string{TrustPrivate}) || len(queued[2].Layers()) != 2 {
		t.Errorf("the input queued again: %+v", queued[2])
	}
	for i, it := range drained {
		q := queued[2*i]
		if it.QueuedFrom != q.ID || !sameTrust(it.Trust, q.Trust) || it.IsVisible() || !slices.Equal(it.Layers(), q.Layers()) {
			t.Errorf("drained item %d: %+v, queued %+v", i, it, q)
		}
	}
	before := PathTrust(s.Path(queued[2].ID))
	if !slices.Equal(before.Reads, []string{TrustUntrusted}) {
		t.Errorf("trust before the second drain: %+v", before)
	}
	if after := PathTrust(s.Path(s.Leaf())); !slices.Equal(after.Reads, []string{TrustPrivate, TrustUntrusted}) {
		t.Errorf("trust at the leaf: %+v", after)
	}

	checkBuilt(t, "bad-queued-drain")
	bad := loadFixture(t, "bad-queued-drain")
	if err := bad.VerifyRecords(bad.Leaf()); !errors.Is(err, ErrDrainMismatch) {
		t.Errorf("bad-queued-drain: VerifyRecords = %v", err)
	}
}
