package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// fixtures015 are the 0.15 conformance fixtures, each built natively by
// its function; TestRegenerateFixtures writes them and each test checks
// its file is what the function builds, as for fixtures014.
var fixtures015 = map[string]func(t *testing.T) *Session{
	"copied":        func(t *testing.T) *Session { _, child := copiedFixture(t); return child },
	"copied-answer": func(t *testing.T) *Session { _, fork := copiedAnswerFixture(t); return fork },
	"delivered":     deliveredFixture,
}

// badFixtures015 are the 0.15 negative fixtures: each a valid file with
// one line a writer refuses, written by hand.
var badFixtures015 = map[string]func(t *testing.T) []byte{
	"bad-copied-dispatch": func(t *testing.T) []byte { return badCopiedLines(t, "dispatch") },
	"bad-copied-reject":   func(t *testing.T) []byte { return badCopiedLines(t, "reject") },
	"bad-copied-self":     func(t *testing.T) []byte { return badCopiedLines(t, "self") },
}

// writeFixtures015 writes the 0.15 fixtures, positive and negative.
func writeFixtures015(t *testing.T) {
	t.Helper()
	for name, build := range fixtures015 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, build := range badFixtures015 {
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), build(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// checkBad fails the test when testdata/sessions/<name>.jsonl is not
// what its builder writes, and returns the session read from it.
func checkBad(t *testing.T, name string) *Session {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, badFixtures015[name](t)) {
		t.Errorf("%s.jsonl is not what its builder writes; run go test -update", name)
	}
	return loadFixture(t, name)
}

// held returns the entry s holds under id, failing the test when it
// holds none.
func held(t *testing.T, s *Session, id string) Entry {
	t.Helper()
	e, ok := s.Entry(id)
	if !ok {
		t.Fatalf("%s holds no %s", s.ID(), id)
	}
	return e
}

// fixtureSession starts a fixture session with a clock a second apart
// from at, and the appenders fixtureAppender gives.
func fixtureSession(t *testing.T, h Header, at string) (*Session, func(Entry) string, func(responseID, text string)) {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t.Fatal(err)
	}
	if h.Harness == nil {
		h.Harness = &Harness{Name: "fixture", Version: "1"}
	}
	if h.CWD == "" {
		h.CWD = "/home/u/proj"
	}
	h.CreatedAt = ts
	s := New(h)
	s.setClock(func() time.Time { ts = ts.Add(time.Second); return ts })
	must, say := fixtureAppender(t, s)
	return s, must, say
}

// copiedParentID and copiedChildID are the sessions of the copied
// fixture.
const (
	copiedParentID = "01995b2a-0000-7000-8000-000000000030"
	copiedChildID  = "01995b2a-0000-7000-8000-000000000031"
)

// copiedFixture builds the parent session that spawns a helper, and the
// child session copied.jsonl holds: the child's run is prompted with
// the parent's user message, the parent's reasoning and its helper
// call, each copied unchanged and naming its entry, and a placeholder
// output the seed made for the call, which names no entry. The child's
// header promises dispatch and decision records, and it writes neither
// for the copied call. Its own turn follows, one response.
func copiedFixture(t *testing.T) (parent, child *Session) {
	t.Helper()
	parent, pmust, _ := fixtureSession(t, Header{ID: copiedParentID, Records: AllRecords}, "2026-11-01T10:00:00Z")
	pmust(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Delegate.")})
	pmust(NewRunStart("run-p", SourceInput, ""))
	user := pmust(NewItemEntry(openresponses.UserText("Summarise the design doc.")))
	sent := fixtureHash(t, parent)
	reasoning := pmust(&ItemEntry{Item: &openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "Hand it to a helper."}}}, ResponseID: "resp_p1"})
	helper := pmust(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_h", CallID: "call_helper", Name: "helper", Arguments: `{"task":"summarise"}`}, ResponseID: "resp_p1"})
	pmust(&ResponseEntry{ResponseID: "resp_p1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	pmust(NewDispatch("call_helper", helper))
	pmust(NewSubsessionLink(copiedChildID, "call_helper"))

	child, must, say := fixtureSession(t, Header{ID: copiedChildID, ParentSession: copiedParentID, SpawnedBy: "call_helper", Records: AllRecords}, "2026-11-01T10:01:00Z")
	copy := func(id string) {
		t.Helper()
		src := held(t, parent, id).(*ItemEntry)
		must(&ItemEntry{Item: src.Item, CopiedFrom: &CopiedFrom{Session: copiedParentID, Entry: id}})
	}
	must(&ConfigEntry{Model: "gpt-5-mini", Instructions: ptr("You are a helper.")})
	must(NewRunStart("run-c", SourceInput, ""))
	copy(user)
	copy(reasoning)
	copy(helper)
	must(&ItemEntry{Item: openresponses.NewFunctionCallOutput("call_helper", "You are this call's helper; the task follows."), CopiedFrom: &CopiedFrom{Session: copiedParentID}})
	must(NewItemEntry(openresponses.UserText("Summarise the design doc in one line.")))
	say("resp_c1", "The design adds four members and one record entry.")
	e, err := child.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(e)
	return parent, child
}

// copiedAnswerOriginID and copiedAnswerForkID are the sessions of the
// copied-answer fixture.
const (
	copiedAnswerOriginID = "01995b2a-0000-7000-8000-000000000032"
	copiedAnswerForkID   = "01995b2a-0000-7000-8000-000000000033"
	copiedAnswerChildID  = "01995b2a-0000-7000-8000-000000000034"
)

// copiedAnswerFixture builds an origin that delegates a call to a
// subsession, and the fork copied-answer.jsonl holds: based on the
// origin's response that made the call, before its dispatch, it answers
// the call in its prefix with the output the origin recorded, an answer
// decision and then the output, whose copied_from names the origin and
// the origin's output entry.
func copiedAnswerFixture(t *testing.T) (origin, fork *Session) {
	t.Helper()
	origin, omust, _ := fixtureSession(t, Header{ID: copiedAnswerOriginID, Records: AllRecords}, "2026-11-01T11:00:00Z")
	omust(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Delegate.")})
	omust(NewItemEntry(openresponses.UserText("Find the flaky test.")))
	sent := fixtureHash(t, origin)
	call := omust(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_a", CallID: "call_agent", Name: "agent", Arguments: `{"task":"find the flaky test"}`}, ResponseID: "resp_o1"})
	base := omust(&ResponseEntry{ResponseID: "resp_o1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	omust(NewDispatch("call_agent", call))
	omust(NewSubsessionLink(copiedAnswerChildID, "call_agent"))
	out := omust(NewItemEntry(openresponses.NewFunctionCallOutput("call_agent", "TestFollow races the poller.")))

	// The fork promises no records: a redaction that writes its prefix
	// as entries of its own, as TestRedactionVerifies does, would make
	// the prefix call one the promise covers, with no dispatch.
	fork, err := Fork(origin, base, Header{ID: copiedAnswerForkID, CreatedAt: origin.Header().CreatedAt.Add(time.Hour), Harness: &Harness{Name: "fixture", Version: "1"}, CWD: "/home/u/proj"})
	if err != nil {
		t.Fatal(err)
	}
	at := origin.Header().CreatedAt.Add(time.Hour)
	fork.setClock(func() time.Time { at = at.Add(time.Second); return at })
	must, say := fixtureAppender(t, fork)
	must(NewDecision("call_agent", call, VerdictAnswer, ByPolicy).WithReason("answered from the origin's record"))
	src := held(t, origin, out).(*ItemEntry)
	must(&ItemEntry{Item: src.Item, CopiedFrom: &CopiedFrom{Session: copiedAnswerOriginID, Entry: out}})
	say("resp_f1", "TestFollow races the poller; I will fix the wait.")
	return origin, fork
}

// badCopiedLines builds a 0.15 negative fixture from the copied
// fixture's child: a dispatch or a reject naming its copied call, or an
// item whose copied_from names the child itself, each appended by hand
// at the leaf, since a writer refuses it.
func badCopiedLines(t *testing.T, kind string) []byte {
	t.Helper()
	_, child := copiedFixture(t)
	var helper string
	for _, e := range child.Entries() {
		if it, ok := e.(*ItemEntry); ok {
			if _, ok := it.Item.(*openresponses.FunctionCall); ok {
				helper = it.ID
			}
		}
	}
	var bad Entry
	switch kind {
	case "dispatch":
		bad = NewDispatch("call_helper", helper)
	case "reject":
		bad = NewDecision("call_helper", helper, VerdictReject, ByPolicy).WithReason("not this session's call")
	case "self":
		bad = &ItemEntry{Item: openresponses.UserText("Again."), CopiedFrom: &CopiedFrom{Session: copiedChildID}}
	}
	if _, err := child.Append(bad); err == nil {
		t.Fatalf("%s: the writer appended what the fixture breaks", kind)
	}
	b := bad.Base()
	b.ID, b.Parent, b.Timestamp = "x", "", time.Date(2026, 11, 1, 10, 30, 0, 0, time.UTC)
	line, err := MarshalEntry(bad)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, child); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(reparent(t, string(line), child.Leaf()) + "\n")
	return buf.Bytes()
}

// TestCopiedFixture reads the copied-context fixture: the copied call is
// no call of the child's, so the child verifies without a dispatch or a
// decision for it although its header promises both, the call is not
// pending, its runs agree with their segments, every copied entry names
// an equal item in the parent, and the placeholder names none.
func TestCopiedFixture(t *testing.T) {
	raw := checkBuilt(t, "copied")
	parent, _ := copiedFixture(t)
	s := loadFixture(t, "copied")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !calls[0].Copied() || calls[0].State(s.Header()) != CallCompleted {
		t.Fatalf("calls = %+v", calls)
	}
	runs, _ := s.Runs(s.Leaf())
	if len(runs) != 1 || runs[0].End.Reason != ReasonDone || len(runs[0].Calls()) != 0 {
		t.Errorf("run = %+v, calls %d", runs[0].End, len(runs[0].Calls()))
	}
	copied := 0
	for _, e := range s.Entries() {
		it, ok := e.(*ItemEntry)
		if !ok || it.CopiedFrom == nil {
			continue
		}
		copied++
		if it.CopiedFrom.Session != copiedParentID {
			t.Errorf("%s names %s", it.ID, it.CopiedFrom.Session)
		}
		if it.CopiedFrom.Entry == "" {
			if _, ok := it.Item.(*openresponses.FunctionCallOutput); !ok {
				t.Errorf("%s names no entry and is no placeholder output", it.ID)
			}
			continue
		}
		e, _ := parent.Entry(it.CopiedFrom.Entry)
		src, ok := e.(*ItemEntry)
		if !ok {
			t.Fatalf("%s names %s, which the parent does not hold", it.ID, it.CopiedFrom.Entry)
		}
		a, _ := json.Marshal(src.Item)
		b, _ := json.Marshal(it.Item)
		if !bytes.Equal(a, b) {
			t.Errorf("%s is not the item of %s", it.ID, src.ID)
		}
	}
	if copied != 4 || !bytes.Contains(raw, []byte(`"copied_from":{"entry":`)) {
		t.Errorf("%d copied items", copied)
	}
	// The request the child's response names leaves copied_from out, as
	// it does every member that reaches no request.
	for _, e := range s.Entries() {
		if r, ok := e.(*ResponseEntry); ok {
			if err := s.Verify(r.ID); err != nil {
				t.Errorf("response %s: %v", r.ID, err)
			}
		}
	}
}

// TestCopiedContextHash: copied_from is provenance and reaches no
// request, so the context hash leaves it out, as it does from_link.
func TestCopiedContextHash(t *testing.T) {
	plain := New(Header{ID: "a"})
	copied := New(Header{ID: "b"})
	item := openresponses.UserText("hello")
	ida, err := plain.Append(NewItemEntry(item))
	if err != nil {
		t.Fatal(err)
	}
	idb, err := copied.Append(&ItemEntry{Item: item, CopiedFrom: &CopiedFrom{Session: "elsewhere", Entry: "sha256:x"}})
	if err != nil {
		t.Fatal(err)
	}
	ha, _ := plain.ContextHash(ida)
	hb, _ := copied.ContextHash(idb)
	if ha == "" || ha != hb {
		t.Errorf("context hashes %s and %s differ", ha, hb)
	}
}

// TestCopiedFromForms: copied_from is taken in the form 0.15 defines and
// kept as written in any other, and a writer writes it in that form
// alone.
func TestCopiedFromForms(t *testing.T) {
	for _, tt := range []struct {
		member string
		typed  bool
	}{
		{`{"session":"s","entry":"sha256:e"}`, true},
		{`{"session":"s"}`, true},
		{`{"session":"s","x:note":1}`, true},
		{`{"session":""}`, false},
		{`{"entry":"sha256:e"}`, false},
		{`{"session":"s","entry":""}`, false},
		{`{"session":"s","entry":3}`, false},
		{`"s"`, false},
	} {
		line := `{"type":"item","id":"x","parent":null,"ts":"2026-11-01T10:00:00Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"copied_from":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s: %v", tt.member, err)
		}
		it := e.(*ItemEntry)
		if got := it.CopiedFrom != nil; got != tt.typed {
			t.Errorf("%s: typed %v, want %v", tt.member, got, tt.typed)
		}
		if _, kept := it.Unknown["copied_from"]; kept == tt.typed {
			t.Errorf("%s: kept in Unknown %v", tt.member, kept)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(back), `"copied_from":`+tt.member) && !sameJSON(t, members(t, back)["copied_from"], []byte(tt.member)) {
			t.Errorf("%s: written back as %s", tt.member, back)
		}
	}
	if _, err := MarshalEntry(&ItemEntry{Item: openresponses.UserText("hi"), CopiedFrom: &CopiedFrom{}}); err == nil {
		t.Error("a copied_from naming no session was written")
	}
}

// TestCopiedCallRules: a writer refuses a dispatch or a decision naming a
// copied call, an output for one that is not marked copied, and an item
// copied from its own session; a copied call is never pending, and its
// state is unknown, not never started, under a header that promises
// dispatches.
func TestCopiedCallRules(t *testing.T) {
	s := New(Header{ID: "child", Records: AllRecords})
	call, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "helper", Arguments: "{}"}, CopiedFrom: &CopiedFrom{Session: "parent"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := s.PendingCalls(s.Leaf())
	calls, _ := s.Calls(s.Leaf())
	if len(pending) != 0 || len(calls) != 1 || calls[0].State(s.Header()) != CallUnknown {
		t.Errorf("pending %d, state %v", len(pending), calls[0].State(s.Header()))
	}
	for _, e := range []Entry{
		NewDispatch("call_1", call),
		NewDecision("call_1", call, VerdictProceed, ByPolicy),
		NewDecision("call_1", call, VerdictReject, ByPolicy).WithReason("no"),
		NewDecision("call_1", call, VerdictAnswer, ByPolicy),
	} {
		if _, err := s.Append(e); !errors.Is(err, ErrCopiedCall) {
			t.Errorf("%T: %v, want ErrCopiedCall", e, err)
		}
	}
	if _, err := s.Append(NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "done"))); !errors.Is(err, ErrBadCopiedFrom) {
		t.Errorf("an unmarked output of a copied call: %v", err)
	}
	if _, err := s.Append(&ItemEntry{Item: openresponses.UserText("x"), CopiedFrom: &CopiedFrom{Session: "child"}}); !errors.Is(err, ErrBadCopiedFrom) {
		t.Errorf("an item copied from its own session: %v", err)
	}
	if _, err := s.Append(&ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "done"), CopiedFrom: &CopiedFrom{Session: "parent"}}); err != nil {
		t.Errorf("a marked output of a copied call: %v", err)
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
}

// TestBadCopiedFixtures reads the 0.15 negative fixtures for copied
// calls: every line verifies, and VerifyRecords reports the one broken
// rule.
func TestBadCopiedFixtures(t *testing.T) {
	for name, want := range map[string]error{
		"bad-copied-dispatch": ErrCopiedCall,
		"bad-copied-reject":   ErrCopiedCall,
		"bad-copied-self":     ErrBadCopiedFrom,
	} {
		s := checkBad(t, name)
		if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, want) {
			t.Errorf("%s: VerifyRecords = %v, want %v", name, err, want)
		}
	}
}

// TestCopiedAnswerFixture reads the fork that answers a call in its
// prefix with the output its origin recorded: the output is the fork's
// own for that call, so the call completes after its answer and the
// fork verifies; its copied_from names the origin's output, and a
// reader holding the origin follows it to the origin's subsession link
// for the call, which the fork's own record cannot hold.
func TestCopiedAnswerFixture(t *testing.T) {
	checkBuilt(t, "copied-answer")
	origin, _ := copiedAnswerFixture(t)
	s := loadFixture(t, "copied-answer")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	calls, _ := s.Calls(s.Leaf())
	if len(calls) != 1 || calls[0].Copied() || !calls[0].Answered() || calls[0].Output == nil {
		t.Fatalf("calls = %+v", calls)
	}
	from := calls[0].Output.CopiedFrom
	if from == nil {
		t.Fatal("the output carries no copied_from")
	}
	if _, ok := origin.Entry(from.Entry); from.Session != s.Header().ParentSession || !ok {
		t.Fatalf("the output's copied_from is %+v", from)
	}
	var link *LinkEntry
	for _, e := range origin.Path(from.Entry) {
		if l, ok := e.(*LinkEntry); ok && l.Rel == RelSubsession && l.CallID == calls[0].ID() {
			link = l
		}
	}
	if link == nil || link.Session != copiedAnswerChildID {
		t.Errorf("the origin's link for the call is %+v", link)
	}
}

// TestRewriteDropsCopiedEntry: a rewrite that changes a copied item
// removes copied_from's entry, which then names no equal item, and keeps
// its session; one that changes nothing in the item keeps both.
func TestRewriteDropsCopiedEntry(t *testing.T) {
	e := &ItemEntry{Item: openresponses.UserText("call alice"), CopiedFrom: &CopiedFrom{Session: "alice-parent", Entry: "sha256:e"}}
	e.ID, e.Timestamp = "x", time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	out, err := RewriteStrings(e, func(s string) string { return strings.ReplaceAll(s, "alice", "bob") })
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*ItemEntry).CopiedFrom; got == nil || got.Session != "alice-parent" || got.Entry != "" {
		t.Errorf("a changed copy names %+v", got)
	}
	out, err = RewriteStrings(e, func(s string) string { return strings.ReplaceAll(s, "carol", "bob") })
	if err != nil {
		t.Fatal(err)
	}
	if got := out.(*ItemEntry).CopiedFrom; got == nil || got.Entry != "sha256:e" {
		t.Errorf("an unchanged copy names %+v", got)
	}
}

// deliveredFixture builds the session delivered.jsonl holds: a
// background result is delivered while the first run is in flight and
// the run ends without draining it, so the writer queues it again after
// the run end, carrying deliver; the next run starts for it and drains
// it, its model makes a call a policy holds for approval, and a second
// delivery arrives before the record is cut, with no run end.
func deliveredFixture(t *testing.T) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000035", Records: AllRecords}, "2026-11-01T12:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Be brief.")})
	must(NewRunStart("run-1", SourceInput, ""))
	must(NewItemEntry(openresponses.UserText("Start the build in the background.")))
	say("resp_1", "The build is running.")
	first := NewDelivery(openresponses.UserText("Background task build-7 finished: 2 warnings.")).WithTrigger("task", "build-7", "background")
	first.Ref = "inbox-1"
	must(first)
	e, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	must(e)
	again := first.Requeue()
	must(again)
	must(NewRunStart("run-2", SourceInput, ""))
	must(again.Drain())
	sent := fixtureHash(t, s)
	call := must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_fix", Name: "bash", Arguments: `{"cmd":"make fix-warnings"}`}, ResponseID: "resp_2"})
	must(&ResponseEntry{ResponseID: "resp_2", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	must(NewDecision("call_fix", call, VerdictHold, ByPolicy).WithReason("asks a person"))
	must(NewDelivery(openresponses.UserText("Background task lint-3 finished: clean.")).WithTrigger("task", "lint-3", "background"))
	return s
}

// TestDeliveredFixture reads the delivery fixture: the run end closes
// the first delivery, the entry queued again after it carries deliver
// and is owed until the next run drains it, and at the cut the second
// delivery is owed as a delivery while the held call waits.
func TestDeliveredFixture(t *testing.T) {
	raw := checkBuilt(t, "delivered")
	if bytes.Count(raw, []byte(`"deliver":true`)) != 3 {
		t.Errorf("the fixture holds %d deliveries", bytes.Count(raw, []byte(`"deliver":true`)))
	}
	s := loadFixture(t, "delivered")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	var queued []*QueuedEntry
	for _, e := range s.Entries() {
		if q, ok := e.(*QueuedEntry); ok {
			queued = append(queued, q)
		}
	}
	if len(queued) != 3 {
		t.Fatalf("%d queued entries", len(queued))
	}
	owed, _ := s.PendingQueued(queued[1].ID)
	if len(owed) != 1 || owed[0] != queued[1] || !owed[0].Deliver || owed[0].Ref != "inbox-1" {
		t.Errorf("after the run end the owed input is %+v", owed)
	}
	owed, _ = s.PendingQueued(s.Leaf())
	if len(owed) != 1 || owed[0] != queued[2] || !owed[0].Deliver {
		t.Errorf("at the cut the owed input is %+v", owed)
	}
	pending, _ := s.PendingCalls(s.Leaf())
	if len(pending) != 1 || !pending[0].Held() {
		t.Errorf("pending = %+v", pending)
	}
}

// TestDeliverForms: deliver is taken as true on a steer, in a file of
// any minor; on a follow-up, or holding anything but true, it is kept as
// written; a writer refuses it on a follow-up.
func TestDeliverForms(t *testing.T) {
	for _, tt := range []struct {
		mode, member string
		typed        bool
	}{
		{"steer", "true", true},
		{"steer", "false", false},
		{"steer", `"yes"`, false},
		{"followup", "true", false},
	} {
		line := `{"type":"queued","id":"x","parent":null,"ts":"2026-11-01T10:00:00Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"mode":"` + tt.mode + `","deliver":` + tt.member + `}`
		e, err := UnmarshalEntry([]byte(reparent(t, line, "")))
		if err != nil {
			t.Fatalf("%s %s: %v", tt.mode, tt.member, err)
		}
		q := e.(*QueuedEntry)
		if _, kept := q.Unknown["deliver"]; q.Deliver != tt.typed || kept == tt.typed {
			t.Errorf("%s %s: typed %v, kept %v", tt.mode, tt.member, q.Deliver, kept)
		}
		back, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if !sameJSON(t, members(t, back)["deliver"], []byte(tt.member)) {
			t.Errorf("%s %s: written back as %s", tt.mode, tt.member, back)
		}
	}
	if _, err := MarshalEntry(&QueuedEntry{Item: openresponses.UserText("hi"), Mode: ModeFollowUp, Deliver: true}); err == nil {
		t.Error("a follow-up carrying deliver was written")
	}
}
