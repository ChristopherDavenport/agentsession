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
	"requested-fold":   requestedFoldFixture,
	"continued-parts":  continuedPartsFixture,
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
// person types the first input, whose trigger says so; a
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
	must(&ItemEntry{Item: openresponses.UserText("Deploy billing."), Source: &Trigger{Kind: TriggerHuman, Source: "terminal"}})
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
	for _, e := range s.Entries() {
		it, ok := e.(*ItemEntry)
		if !ok || it.Item.(*openresponses.Message).Role != openresponses.RoleUser {
			continue
		}
		want := WordsComposed
		if it.QueuedFrom == "" {
			want = WordsPerson
		}
		if got := WordsOf(it); got != want {
			t.Errorf("input %q: %v, want %v", it.Item.(*openresponses.Message).Content.Text(), got, want)
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

// TestWordsOf: an input message is the person's words when its
// trigger's kind is human, and not when the trigger has another kind or
// the message names the layers it was composed from or the peer link it
// was forwarded from; with no trigger it makes no claim, and nor does an
// entry that holds no input message (agentturn#303; the user's decision
// of 2026-11-05).
func TestWordsOf(t *testing.T) {
	typed := &Trigger{Kind: TriggerHuman, Source: "terminal"}
	reminder := &Trigger{Kind: "schedule", Ref: "cron:deploy-window"}
	assistant := NewItemEntry(&openresponses.Message{Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "ok", Annotations: []openresponses.Annotation{}}}})
	layered := &ItemEntry{Item: openresponses.UserText("Memory: the window is Tuesday."), InstructionsPart: &LayerRef{Source: LayerAgentMemory}}
	forwarded := &ItemEntry{Item: openresponses.UserText("The peer says hello."), FromLink: "link-1"}
	for _, tt := range []struct {
		name string
		e    Entry
		want Words
	}{
		{"typed", &ItemEntry{Item: openresponses.UserText("Deploy billing."), Source: typed}, WordsPerson},
		{"a reminder", &ItemEntry{Item: openresponses.UserText("The window opens."), Source: reminder}, WordsComposed},
		{"no trigger", NewItemEntry(openresponses.UserText("Deploy billing.")), WordsUnclaimed},
		{"a layer", layered, WordsComposed},
		{"a peer's text", forwarded, WordsComposed},
		{"queued, typed", NewQueued(openresponses.UserText("And the docs."), ModeSteer).WithTrigger(TriggerHuman, "", "terminal"), WordsPerson},
		{"queued, a reminder", NewQueued(openresponses.UserText("The window opens."), ModeFollowUp).WithTrigger("schedule", "cron:deploy-window", ""), WordsComposed},
		{"queued, no trigger", NewQueued(openresponses.UserText("And the docs."), ModeSteer), WordsUnclaimed},
		{"model output", assistant, WordsUnclaimed},
		{"a tool's output", NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "4")), WordsUnclaimed},
		{"a config", &ConfigEntry{Model: "gpt-5"}, WordsUnclaimed},
	} {
		if got := WordsOf(tt.e); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}

	// A writer does not mark a composed layer as typed.
	q := NewQueued(openresponses.UserText("x"), ModeSteer).WithTrigger(TriggerHuman, "", "")
	q.InstructionsPart = &LayerRef{Source: LayerAgentMemory}
	for _, e := range []Entry{&ItemEntry{Item: openresponses.UserText("x"), Source: typed, InstructionsPart: &LayerRef{Source: LayerAgentMemory}}, q,
		&ItemEntry{Item: openresponses.UserText("x"), Source: typed, FromLink: "link-1"}} {
		if _, err := New(Header{ID: "w"}).Append(e); !errors.Is(err, ErrComposedHuman) {
			t.Errorf("a composed %s with a human trigger: Append = %v", e.EntryType(), err)
		}
	}

	// The drained item carries the trigger as its source, which the
	// record checks, so the claim survives a restart.
	s := New(Header{ID: "w"})
	steer := NewQueued(openresponses.UserText("And the docs."), ModeSteer).WithTrigger(TriggerHuman, "", "terminal")
	if _, err := s.Append(steer); err != nil {
		t.Fatal(err)
	}
	it := steer.Drain()
	if WordsOf(it) != WordsPerson {
		t.Error("the drained item is not the person's words")
	}
	it.Source = &Trigger{Kind: "schedule"}
	if _, err := s.Append(it); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrDrainMismatch) {
		t.Errorf("a drain that changed the trigger: VerifyRecords = %v", err)
	}

	// A redactor keeps the kind the format defines and rewrites any
	// other as text.
	for _, tt := range []struct {
		kind, want string
	}{{TriggerHuman, TriggerHuman}, {"humane-society", "[X]e-society"}} {
		e := &ItemEntry{Item: openresponses.UserText("a human wrote this"), Source: &Trigger{Kind: tt.kind}}
		e.ID = "x"
		out, err := RewriteStrings(e, func(s string) string { return strings.ReplaceAll(s, "human", "[X]") })
		if err != nil {
			t.Fatal(err)
		}
		if got := out.(*ItemEntry).Source.Kind; got != tt.want {
			t.Errorf("kind %s rewritten to %s, want %s", tt.kind, got, tt.want)
		}
	}
}

// TestRequestedFold: since 0.21 a compaction records that the fold was
// asked for, and the instructions the summariser was asked to follow
// (agentsession#277). Both survive a write and a read, neither reaches
// the context or its hash, instructions is never written without
// requested, and a rewrite treats the instructions as text and keeps
// requested.
func TestRequestedFold(t *testing.T) {
	fold := func(requested bool, instructions string) (*Session, string) {
		s := New(Header{ID: "f"})
		first, err := s.Append(NewItemEntry(openresponses.UserText("first")))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(NewItemEntry(openresponses.UserText("second"))); err != nil {
			t.Fatal(err)
		}
		c := &CompactionEntry{FirstKept: first, Summary: openresponses.UserText("Summary: the API notes."), Config: Settings{Model: "gpt-5"}, Requested: requested, Instructions: instructions}
		id, err := s.Append(c)
		if err != nil {
			t.Fatal(err)
		}
		return s, id
	}
	asked, at := fold(true, "keep the API notes")
	var buf bytes.Buffer
	if err := Write(&buf, asked); err != nil {
		t.Fatal(err)
	}
	for _, member := range []string{`"requested":true`, `"instructions":"keep the API notes"`} {
		if !strings.Contains(buf.String(), member) {
			t.Errorf("the compaction does not carry %s:\n%s", member, buf.String())
		}
	}
	back, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := back.Entry(at)
	if c := e.(*CompactionEntry); !c.Requested || c.Instructions != "keep the API notes" {
		t.Errorf("read back: requested %v, instructions %q", c.Requested, c.Instructions)
	}
	plain, pat := fold(false, "")
	ha, err := back.ContextHash(at)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := plain.ContextHash(pat)
	if err != nil {
		t.Fatal(err)
	}
	if ha != hb {
		t.Errorf("a requested fold's context hash %s differs from an automatic one's %s", ha, hb)
	}
	if _, err := MarshalEntry(&CompactionEntry{FirstKept: "x", Summary: openresponses.UserText("s"), Instructions: "keep the API notes"}); err == nil {
		t.Error("instructions were written without requested")
	}

	out, err := RewriteStrings(e, strings.NewReplacer("API", "[X]", "requested", "[R]").Replace)
	if err != nil {
		t.Fatal(err)
	}
	if c := out.(*CompactionEntry); !c.Requested || c.Instructions != "keep the [X] notes" {
		t.Errorf("rewritten: requested %v, instructions %q", c.Requested, c.Instructions)
	}

	// Each is taken in the form 0.21 defines, in a file of any minor;
	// another form, or instructions beside no requested, is kept.
	for _, tt := range []struct {
		members        string
		requested      bool
		instructions   string
		keptRequested  bool
		keptInstructed bool
	}{
		{`"requested":true,"instructions":"keep it"`, true, "keep it", false, false},
		{`"requested":true`, true, "", false, false},
		{`"requested":false`, false, "", true, false},
		{`"requested":"yes","instructions":"keep it"`, false, "", true, true},
		{`"instructions":"keep it"`, false, "", false, true},
		{`"requested":true,"instructions":""`, true, "", false, true},
		{`"requested":true,"instructions":["keep it"]`, true, "", false, true},
	} {
		line := `{"type":"compaction","id":"x","parent":null,"ts":"2026-11-04T10:00:00Z","first_kept":"y","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"s"}]},"config":{},` + tt.members + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s: %v", tt.members, err)
		}
		c := e.(*CompactionEntry)
		_, keptR := c.Unknown["requested"]
		_, keptI := c.Unknown["instructions"]
		if c.Requested != tt.requested || c.Instructions != tt.instructions || keptR != tt.keptRequested || keptI != tt.keptInstructed {
			t.Errorf("%s: requested %v, instructions %q, kept %v %v", tt.members, c.Requested, c.Instructions, keptR, keptI)
		}
		again, err := MarshalEntry(e)
		if err != nil {
			t.Fatalf("%s: %v", tt.members, err)
		}
		m := members(t, again)
		for _, k := range []string{"requested", "instructions"} {
			if want, ok := members(t, []byte("{"+tt.members+"}"))[k]; ok && !sameJSON(t, m[k], want) {
				t.Errorf("%s: %s written back as %s", tt.members, k, m[k])
			}
		}
	}
}

// requestedFoldFixture builds the session requested-fold.jsonl holds: a
// fold the budget forced, which carries neither member, and a later one
// a person asked for with instructions, which the compaction records as
// requested with those instructions. Every response's request_hash is
// computed over the context, which neither member reaches.
func requestedFoldFixture(t *testing.T) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-0000000000b7"}, "2026-11-04T11:00:00Z")
	fold := func(firstKept, summary, instructions string) {
		t.Helper()
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		must(&CompactionEntry{FirstKept: firstKept, Summary: openresponses.SystemText(summary), Config: ctx.Settings, Requested: instructions != "", Instructions: instructions, TokensBefore: 5000})
	}
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	must(NewItemEntry(openresponses.UserText("The API takes a bearer token.")))
	say("resp_1", "Noted.")
	second := must(NewItemEntry(openresponses.UserText("The deploy runs on Tuesdays.")))
	say("resp_2", "Noted.")
	fold(second, "Summary: the API takes a bearer token.", "")
	third := must(NewItemEntry(openresponses.UserText("What does the API take?")))
	say("resp_3", "A bearer token.")
	fold(third, "Summary: the API takes a bearer token; the deploy runs on Tuesdays.", "keep the API notes")
	must(NewItemEntry(openresponses.UserText("And when is the deploy?")))
	say("resp_4", "Tuesdays.")
	return s
}

// TestRequestedFoldFixture reads requested-fold.jsonl: the first fold
// was not asked for, the second was, with instructions, and every
// response verifies over a context neither member reaches.
func TestRequestedFoldFixture(t *testing.T) {
	checkBuilt(t, "requested-fold")
	s := loadFixture(t, "requested-fold")
	var folds []*CompactionEntry
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *CompactionEntry:
			folds = append(folds, v)
		case *ResponseEntry:
			if err := s.Verify(v.ID); err != nil {
				t.Errorf("response %s: %v", v.ResponseID, err)
			}
		}
	}
	if len(folds) != 2 || folds[0].Requested || folds[0].Instructions != "" || !folds[1].Requested || folds[1].Instructions != "keep the API notes" {
		t.Fatalf("folds: %+v", folds)
	}
}

// TestContinueKeepsParts: a writer that composes instructions from a
// layer whose trust it knows records them as parts carrying that trust
// (RFC 0001, Instructions as parts, since 0.21; agentsession#281).
// Continue writes the successor's first config from the settings in
// force, so it carries the parts, each with its source and trust, and
// the successor's path holds the memory part's untrusted label; the
// request it rebuilds is the one before the rollover.
func TestContinueKeepsParts(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	old, err := store.Create(ctx, Header{})
	if err != nil {
		t.Fatal(err)
	}
	parts := []InstructionPart{
		{ID: "product", Text: "You deploy services.", Source: LayerProduct},
		{ID: "memory", Text: "The deploy window is Tuesday.", Source: LayerAgentMemory, Trust: &Trust{Reads: []string{TrustUntrusted}}},
	}
	for _, e := range []Entry{
		&ConfigEntry{Model: "gpt-5", InstructionsParts: parts},
		NewItemEntry(openresponses.UserText("hi")),
	} {
		if _, err := store.Append(ctx, old.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	before, err := old.Context()
	if err != nil {
		t.Fatal(err)
	}
	next, err := Continue(ctx, store, old.ID(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cx, err := next.Context()
	if err != nil {
		t.Fatal(err)
	}
	got := cx.Settings.InstructionsParts
	if len(got) != 2 || got[1].Source != LayerAgentMemory || got[1].Trust == nil || !slices.Equal(got[1].Trust.Reads, []string{TrustUntrusted}) {
		t.Errorf("the successor's parts: %+v", got)
	}
	if cx.Trust == nil || !slices.Equal(cx.Trust.Reads, []string{TrustUntrusted}) {
		t.Errorf("the successor's path trust: %+v", cx.Trust)
	}
	if cx.Settings.Instructions != before.Settings.Instructions {
		t.Errorf("instructions %q, before the rollover %q", cx.Settings.Instructions, before.Settings.Instructions)
	}
}

// continuedPartsFixture builds the session continued-parts.jsonl holds:
// the successor of instruction-trust.jsonl as [Continue] writes it, a
// full config of the settings in force at its leaf whose instructions
// are the parts, the memory part marked untrusted and the instruction
// file marked private, followed by a summary and a turn. The path's
// trust holds both labels from the first entry on.
func continuedPartsFixture(t *testing.T) *Session {
	t.Helper()
	old := instructionTrustFixture(t, true)
	cx, err := old.Context()
	if err != nil {
		t.Fatal(err)
	}
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-0000000000b8", ParentSession: old.ID()}, "2026-11-04T12:00:00Z")
	cfg := configFromSettings(cx.Settings)
	cfg.InstructionsOmitted = cloneOmitted(cx.Settings.InstructionsOmitted)
	must(cfg)
	must(NewItemEntry(openresponses.UserText("Summary: v2 ships Friday, as the release page said.")))
	must(NewItemEntry(openresponses.UserText("Is that still the plan?")))
	say("resp_1", "As far as the release page says, yes.")
	return s
}

// TestContinuedPartsFixture reads continued-parts.jsonl: the successor's
// first config carries the parts in force, each with its source and
// trust, the path's trust holds their labels, its instructions are the
// ones in force before the rollover, and its response verifies.
func TestContinuedPartsFixture(t *testing.T) {
	checkBuilt(t, "continued-parts")
	s := loadFixture(t, "continued-parts")
	old := loadFixture(t, "instruction-trust")
	was, err := old.Context()
	if err != nil {
		t.Fatal(err)
	}
	first := s.Path(s.Leaf())[0].(*ConfigEntry)
	if first.Instructions != nil || len(first.InstructionsParts) != len(was.Settings.InstructionsParts) {
		t.Fatalf("the first config: %+v", first)
	}
	for i, p := range first.InstructionsParts {
		w := was.Settings.InstructionsParts[i]
		if p.ID != w.ID || p.Text != w.Text || p.Source != w.Source || !sameTrust(p.Trust, w.Trust) {
			t.Errorf("part %d: %+v, before the rollover %+v", i, p, w)
		}
	}
	cx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if !sameTrust(cx.Trust, PathTrust([]Entry{first})) || cx.Trust == nil || !slices.Contains(cx.Trust.Reads, TrustUntrusted) {
		t.Errorf("the path's trust: %+v", cx.Trust)
	}
	if cx.Settings.Instructions != was.Settings.Instructions {
		t.Errorf("instructions %q, before the rollover %q", cx.Settings.Instructions, was.Settings.Instructions)
	}
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("response %s: %v", r.ResponseID, err)
			}
		}
	}
}

// TestWordsAsWritten: a trigger's kind is read as written in a file of
// any minor, so a raise draws no line through a file: human is the
// person's words, another kind is not, and no trigger makes no claim
// (RFC 0001, queued, since 0.21; the user's decision of 2026-11-05).
func TestWordsAsWritten(t *testing.T) {
	line := func(id, source string) string {
		item := `{"type":"item","id":"` + id + `","parent":null,"ts":"2026-11-04T10:00:01Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"Deploy billing."}]}` + source + `}`
		return reparent(t, item, "") + "\n"
	}
	for _, tt := range []struct {
		name, format, source string
		want                 Words
	}{
		{"human at 0.20", "agentsession/0.20", `,"source":{"kind":"human"}`, WordsPerson},
		{"a schedule at 0.20", "agentsession/0.20", `,"source":{"kind":"schedule"}`, WordsComposed},
		{"no trigger at 0.20", "agentsession/0.20", ``, WordsUnclaimed},
		{"human at 0.21", Format, `,"source":{"kind":"human"}`, WordsPerson},
		{"a schedule at 0.21", Format, `,"source":{"kind":"schedule"}`, WordsComposed},
		{"no trigger at 0.21", Format, ``, WordsUnclaimed},
	} {
		in := `{"type":"session","format":"` + tt.format + `","id":"x","created_at":"2026-11-04T10:00:00Z","payload":"openresponses/2026-04-24"}` + "\n" + line("x", tt.source)
		s, err := Read(strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		if got := WordsOf(s.Entries()[0]); got != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, got, tt.want)
		}
	}

	// A 0.20 file raised by an append: the new typed input is the
	// person's in the process that raised it.
	in := `{"type":"session","format":"agentsession/0.20","id":"x","created_at":"2026-11-04T10:00:00Z","payload":"openresponses/2026-04-24"}` + "\n" + line("x", ``)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	typed := &ItemEntry{Item: openresponses.UserText("And the docs."), Source: &Trigger{Kind: TriggerHuman}}
	if _, err := s.Append(typed); err != nil {
		t.Fatal(err)
	}
	if got := WordsOf(typed); got != WordsPerson {
		t.Errorf("typed input appended to a raised file: %v", got)
	}
	if got := WordsOf(s.Entries()[0]); got != WordsUnclaimed {
		t.Errorf("an untagged input before the raise: %v", got)
	}
}

// TestDrainTrigger: a drained item that changes its queued entry's
// trigger is reported, as it was wrong under 0.20 too; one that lacks
// it is reported only when the queued entry carries a member 0.21
// defines, which dates the drain after it to 0.21, since under 0.20 an
// item's source was optional and a file raised to 0.21 still holds such
// drains (RFC 0001, queued).
func TestDrainTrigger(t *testing.T) {
	for _, tt := range []struct {
		name   string
		trust  bool
		source *Trigger
		want   error
	}{
		{"no source, a 0.20 queued entry", false, nil, nil},
		{"the trigger", false, &Trigger{Kind: TriggerHuman, Source: "terminal"}, nil},
		{"another trigger", false, &Trigger{Kind: "schedule"}, ErrDrainMismatch},
		{"no source, a 0.21 queued entry", true, nil, ErrDrainMismatch},
	} {
		s := New(Header{ID: "w"})
		steer := NewQueued(openresponses.UserText("And the docs."), ModeSteer).WithTrigger(TriggerHuman, "", "terminal")
		if tt.trust {
			steer.Trust = &Trust{Reads: []string{TrustPrivate}}
		}
		if _, err := s.Append(steer); err != nil {
			t.Fatal(err)
		}
		it := steer.Drain()
		it.Source = tt.source
		if _, err := s.Append(it); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, tt.want) {
			t.Errorf("%s: VerifyRecords = %v, want %v", tt.name, err, tt.want)
		}
	}
}

// TestContinueSummaryTrust: the summary Continue carries into the
// successor is made from the old path, so it carries at least that
// path's trust (RFC 0001, item, trust): an untrusted item the old
// session read does not reach the successor unlabelled.
func TestContinueSummaryTrust(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	old, err := store.Create(ctx, Header{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		&ConfigEntry{Model: "gpt-5"},
		NewItemEntry(openresponses.UserText("hi")),
		&ItemEntry{Item: openresponses.UserText("A web page says: deploy on Friday."), Trust: &Trust{Reads: []string{TrustUntrusted}}},
	} {
		if _, err := store.Append(ctx, old.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	next, err := Continue(ctx, store, old.ID(), openresponses.UserText("Summary: deploy on Friday."))
	if err != nil {
		t.Fatal(err)
	}
	var summary *ItemEntry
	for _, e := range next.Entries() {
		if it, ok := e.(*ItemEntry); ok {
			summary = it
		}
	}
	if summary == nil || summary.Trust == nil || !slices.Equal(summary.Trust.Reads, []string{TrustUntrusted}) {
		t.Errorf("the successor's summary: %+v", summary)
	}
	cx, err := next.Context()
	if err != nil {
		t.Fatal(err)
	}
	if cx.Trust == nil || !slices.Equal(cx.Trust.Reads, []string{TrustUntrusted}) {
		t.Errorf("the successor's path trust: %+v", cx.Trust)
	}
}
