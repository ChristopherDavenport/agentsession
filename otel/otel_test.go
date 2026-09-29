package otel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func recorder() (*tracetest.SpanRecorder, trace.Tracer) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	return sr, tp.Tracer("test")
}

// loadFixture reads a session fixture from this module's own testdata,
// which holds copies of the root module's generated fixtures so the
// tests run from the published module as well as from the repository.
// The root's TestRegenerateFixtures writes the copies and
// TestNestedModuleFixtures keeps them in step.
func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type spans []sdktrace.ReadOnlySpan

func (ss spans) named(name string) spans {
	var out spans
	for _, s := range ss {
		if s.Name() == name {
			out = append(out, s)
		}
	}
	return out
}

func (ss spans) withAttr(key, value string) spans {
	var out spans
	for _, s := range ss {
		if attr(s, key) == value {
			out = append(out, s)
		}
	}
	return out
}

func attr(s sdktrace.ReadOnlySpan, key string) string {
	for _, kv := range s.Attributes() {
		if string(kv.Key) == key {
			return kv.Value.String()
		}
	}
	return ""
}

func linksTo(s sdktrace.ReadOnlySpan, target sdktrace.ReadOnlySpan) bool {
	for _, l := range s.Links() {
		if l.SpanContext.SpanID() == target.SpanContext().SpanID() {
			return true
		}
	}
	return false
}

func eventAttr(s sdktrace.ReadOnlySpan, event, key string) []string {
	var out []string
	for _, ev := range s.Events() {
		if ev.Name != event {
			continue
		}
		for _, kv := range ev.Attributes {
			if string(kv.Key) == key {
				out = append(out, kv.Value.String())
			}
		}
	}
	return out
}

// TestExportWorkspaceMembers: every string member a workspace holds
// reaches the env event beside its kind and ref, so a trace shows which
// container a session ran in, and on which node.
func TestExportWorkspaceMembers(t *testing.T) {
	sr, tracer := recorder()
	s := agentsession.New(agentsession.Header{})
	env := &agentsession.EnvEntry{CWD: "/w"}
	w := env.SetWorkspace(agentsession.WorkspaceContainer, "sha256:ab")
	for key, value := range map[string]any{"host": "build-7", "instance": "ctr-1", "node": "n-4", "zone": 3} {
		if err := w.SetMember(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(env); err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), tracer, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	session := spans(sr.Ended()).named(SpanSession)
	if len(session) != 1 {
		t.Fatalf("session spans = %d", len(session))
	}
	for key, want := range map[string]string{"workspace.kind": "container", "workspace.ref": "sha256:ab", "workspace.host": "build-7", "workspace.instance": "ctr-1", "workspace.node": "n-4"} {
		if got := eventAttr(session[0], EventEnv, key); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v, want %s", key, got, want)
		}
	}
	if got := eventAttr(session[0], EventEnv, "workspace.zone"); got != nil {
		t.Errorf("workspace.zone = %v; only string members are exported", got)
	}
	if got := eventAttr(session[0], EventEnv, AttrSubstitution); got != nil {
		t.Errorf("a first env entry is marked a substitution: %v", got)
	}
}

// nodeEnv is an env entry in a container that differs from another
// only by the node it ran on.
func nodeEnv(t *testing.T, node string) *agentsession.EnvEntry {
	t.Helper()
	env := &agentsession.EnvEntry{CWD: "/w"}
	w := env.SetWorkspace(agentsession.WorkspaceContainer, "sha256:ab")
	for key, value := range map[string]string{"host": "build-7", "instance": "ctr-1", "node": node} {
		if err := w.SetMember(key, value); err != nil {
			t.Fatal(err)
		}
	}
	return env
}

// substitutions is, for each env event on the session span in order,
// whether it is marked a substitution, and its workspace.node.
func substitutions(s sdktrace.ReadOnlySpan) (marked []bool, nodes []string) {
	for _, ev := range s.Events() {
		if ev.Name != EventEnv {
			continue
		}
		sub, node := false, ""
		for _, kv := range ev.Attributes {
			switch string(kv.Key) {
			case AttrSubstitution:
				sub = kv.Value.AsBool()
			case "workspace.node":
				node = kv.Value.AsString()
			}
		}
		marked, nodes = append(marked, sub), append(nodes, node)
	}
	return marked, nodes
}

// TestExportSubstitution: a second env entry whose workspace differs
// from the first only by a member the format does not define is a
// substitution, and its event says so; the first is not one, and an
// env entry repeating the workspace in force is not either.
func TestExportSubstitution(t *testing.T) {
	sr, tracer := recorder()
	s := agentsession.New(agentsession.Header{})
	for _, node := range []string{"n-1", "n-2", "n-2"} {
		if _, err := s.Append(nodeEnv(t, node)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Export(context.Background(), tracer, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	session := spans(sr.Ended()).named(SpanSession)
	if len(session) != 1 {
		t.Fatalf("session spans = %d", len(session))
	}
	marked, nodes := substitutions(session[0])
	if !slices.Equal(marked, []bool{false, true, false}) || !slices.Equal(nodes, []string{"n-1", "n-2", "n-2"}) {
		t.Errorf("env events: substitution %v, node %v", marked, nodes)
	}
}

// TestStoreSubstitution: the Store decorator marks the substitution as
// Export does, including against a workspace a resumed session put in
// force before this process opened it.
func TestStoreSubstitution(t *testing.T) {
	ctx := context.Background()
	sr, tracer := recorder()
	mem := agentsession.NewMemoryStore()
	st := Wrap(mem, tracer)
	sess, err := st.Create(ctx, agentsession.Header{ID: "live"})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	for _, node := range []string{"n-1", "n-2"} {
		if _, err := st.Append(ctx, id, nodeEnv(t, node)); err != nil {
			t.Fatal(err)
		}
	}
	st.Close(id)
	session := spans(sr.Ended()).named(SpanSession)
	if len(session) != 1 {
		t.Fatalf("session spans = %d", len(session))
	}
	if marked, nodes := substitutions(session[0]); !slices.Equal(marked, []bool{false, true}) || !slices.Equal(nodes, []string{"n-1", "n-2"}) {
		t.Errorf("env events: substitution %v, node %v", marked, nodes)
	}

	st2 := Wrap(mem, tracer)
	if _, err := st2.Open(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"n-2", "n-3"} {
		if _, err := st2.Append(ctx, id, nodeEnv(t, node)); err != nil {
			t.Fatal(err)
		}
	}
	st2.CloseAll()
	resumed := spans(sr.Ended()).named(SpanSession).withAttr(AttrResumed, "true")
	if len(resumed) != 1 {
		t.Fatalf("resumed session spans = %d", len(resumed))
	}
	if marked, _ := substitutions(resumed[0]); !slices.Equal(marked, []bool{false, true}) {
		t.Errorf("resumed env events: substitution %v", marked)
	}
}

// TestExportRunsFixture replays the runs fixture and checks the shape
// the RFC's projection promises.
func TestExportRunsFixture(t *testing.T) {
	sr, tracer := recorder()
	s := loadFixture(t, "runs")
	known, err := Export(context.Background(), tracer, s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	all := spans(sr.Ended())

	session := all.named(SpanSession)
	if len(session) != 1 || attr(session[0], AttrSessionID) != s.ID() {
		t.Fatalf("session spans = %d", len(session))
	}
	if !session[0].StartTime().Equal(s.Header().CreatedAt) {
		t.Errorf("session start = %s, want header created_at", session[0].StartTime())
	}
	if got := eventAttr(session[0], EventOutcome, AttrOutcomePass); len(got) != 1 || got[0] != "true" {
		t.Errorf("outcome event pass = %v", got)
	}
	if got := eventAttr(session[0], EventEnv, "workspace.kind"); len(got) != 1 || got[0] != "container" {
		t.Errorf("env event workspace = %v", got)
	}

	runs := all.named(SpanRun)
	if len(runs) != 2 {
		t.Fatalf("run spans = %d", len(runs))
	}
	run1, run2 := runs.withAttr(AttrRunID, "run-1"), runs.withAttr(AttrRunID, "run-2")
	if len(run1) != 1 || len(run2) != 1 {
		t.Fatal("runs not found by id")
	}
	if attr(run1[0], AttrRunReason) != "input_required" || attr(run1[0], AttrRunPending) != `["call_1"]` || attr(run1[0], AttrRunTrigger) != "cron:nightly-clean" {
		t.Errorf("run-1 attrs = %v", run1[0].Attributes())
	}
	if attr(run2[0], AttrRunReason) != "done" || attr(run2[0], AttrRunSource) != "resume" {
		t.Errorf("run-2 attrs = %v", run2[0].Attributes())
	}
	if run1[0].Parent().SpanID() != session[0].SpanContext().SpanID() {
		t.Error("run-1 is not a child of the session span")
	}

	inf := all.named(OpInference + " gpt-5")
	if len(inf) != 2 {
		t.Fatalf("inference spans = %d", len(inf))
	}
	first, second := inf.withAttr(AttrResponseID, "resp_1")[0], inf.withAttr(AttrResponseID, "resp_2")[0]
	if !linksTo(second, first) {
		t.Error("second inference does not link the first")
	}
	if attr(first, AttrInputTokens) != "50" || attr(first, AttrRequestHash) == "" {
		t.Errorf("inference attrs = %v", first.Attributes())
	}
	// The span starts at the first output item, which the fixture
	// stamps three seconds before the response entry.
	if first.EndTime().Sub(first.StartTime()) != 3*time.Second {
		t.Errorf("inference duration = %s, want 3s", first.EndTime().Sub(first.StartTime()))
	}

	tools := all.named(OpTool + " bash")
	if len(tools) != 3 {
		t.Fatalf("tool spans = %d", len(tools))
	}
	c1 := tools.withAttr(AttrToolCallID, "call_1")[0]
	c2 := tools.withAttr(AttrToolCallID, "call_2")[0]
	c3 := tools.withAttr(AttrToolCallID, "call_3")[0]
	for _, c := range []sdktrace.ReadOnlySpan{c1, c2, c3} {
		if !linksTo(c, first) {
			t.Errorf("%s does not link the inference that produced it", attr(c, AttrToolCallID))
		}
	}
	if attr(c2, AttrArgsRewritten) != "true" || attr(c2, AttrCallState) != "completed" {
		t.Errorf("call_2 attrs = %v", c2.Attributes())
	}
	if got := eventAttr(c2, EventDecision, AttrVerdict); len(got) != 1 || got[0] != "proceed" {
		t.Errorf("call_2 decisions = %v", got)
	}
	if attr(c3, AttrCallState) != "rejected" || len(eventAttr(c3, EventDecision, AttrDecisionReason)) != 1 {
		t.Errorf("call_3 attrs = %v events %v", c3.Attributes(), c3.Events())
	}
	if c1.Parent().SpanID() != run2[0].SpanContext().SpanID() {
		t.Error("call_1, dispatched in run-2, is not under run-2")
	}
	if got := eventAttr(c1, EventDecision, AttrVerdict); len(got) != 1 || got[0] != "hold" {
		t.Errorf("call_1 decisions = %v", got)
	}
	if attr(c1, AttrCallState) != "completed" {
		t.Errorf("call_1 state = %s", attr(c1, AttrCallState))
	}
	if _, ok := known[lid(t, s, "p0000002")]; !ok {
		t.Error("known spans lack the dispatch entry")
	}
}

// TestStoreWrap drives the wrapper over the memory store and checks a
// live run and a resume trace the same way a replay does.
func TestStoreWrap(t *testing.T) {
	ctx := context.Background()
	sr, tracer := recorder()
	mem := agentsession.NewMemoryStore()
	st := Wrap(mem, tracer)
	sess, err := st.Create(ctx, agentsession.Header{ID: "live", Records: agentsession.AllRecords})
	if err != nil {
		t.Fatal(err)
	}
	id := sess.ID()
	must := func(e agentsession.Entry) string {
		t.Helper()
		eid, err := st.Append(ctx, id, e)
		if err != nil {
			t.Fatal(err)
		}
		return eid
	}
	must(&agentsession.ConfigEntry{Model: "m"})
	must(agentsession.NewRunStart("r1", agentsession.SourceInput, ""))
	must(agentsession.NewItemEntry(openresponses.UserText("go")))
	fcID := must(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c1", Name: "slow", Arguments: "{}"}, ResponseID: "r"})
	must(&agentsession.ResponseEntry{ResponseID: "r", Status: "completed"})
	must(agentsession.NewDispatch("c1", fcID))
	// The process stops with the call in flight.
	st.Close(id)

	ended := spans(sr.Ended())
	tool := ended.named(OpTool + " slow")
	if len(tool) != 1 || attr(tool[0], AttrCallState) != "in_flight" || tool[0].Status().Code != codes.Error {
		t.Fatalf("in-flight tool span = %v", tool)
	}
	if len(ended.named(SpanSession)) != 1 || len(ended.named(SpanRun)) != 1 {
		t.Errorf("session/run spans = %d/%d", len(ended.named(SpanSession)), len(ended.named(SpanRun)))
	}

	// A new process resumes: the output closes a span for the call the
	// previous process left, under a session span marked resumed.
	st2 := Wrap(mem, tracer)
	if _, err := st2.Open(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Append(ctx, id, agentsession.NewRunStart("r2", agentsession.SourceResume, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Append(ctx, id, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("c1", "done"))); err != nil {
		t.Fatal(err)
	}
	st2.CloseAll()
	ended = spans(sr.Ended())
	resumed := ended.named(SpanSession).withAttr(AttrResumed, "true")
	if len(resumed) != 1 {
		t.Fatalf("resumed session spans = %d", len(resumed))
	}
	closed := ended.named(OpTool+" slow").withAttr(AttrCallState, "completed")
	if len(closed) != 1 {
		t.Fatalf("completed tool spans after resume = %d", len(closed))
	}
	r2 := ended.named(SpanRun).withAttr(AttrRunID, "r2")
	if len(r2) != 1 || closed[0].Parent().SpanID() != r2[0].SpanContext().SpanID() {
		t.Error("the resumed call's span is not under the resume run")
	}
}

// TestBranchLink exports two leaves of a forked session and expects
// the first span after the branch on the second path to link the
// entry the branch left, whose span the first export created.
func TestBranchLink(t *testing.T) {
	sr, tracer := recorder()
	s := loadFixture(t, "branch")
	leaves := s.Leaves()
	if len(leaves) != 2 {
		t.Fatalf("branch fixture has %d leaves", len(leaves))
	}
	// Find the leaf whose path holds the branch summary; export the
	// other one first so the abandoned leaf's span is known.
	var withSummary, other string
	for _, leaf := range leaves {
		for _, e := range s.Path(leaf) {
			if _, ok := e.(*agentsession.BranchSummaryEntry); ok {
				withSummary = leaf
			}
		}
	}
	for _, leaf := range leaves {
		if leaf != withSummary {
			other = leaf
		}
	}
	known, err := Export(context.Background(), tracer, s, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Export(context.Background(), tracer, s, withSummary, WithKnownSpans(known)); err != nil {
		t.Fatal(err)
	}
	var from string
	for _, e := range s.Path(withSummary) {
		if b, ok := e.(*agentsession.BranchSummaryEntry); ok {
			from = b.From
		}
	}
	target, ok := known[from]
	if !ok {
		t.Fatalf("no span known for branched-from entry %s", from)
	}
	linked := false
	for _, sp := range sr.Ended() {
		for _, l := range sp.Links() {
			if l.SpanContext.SpanID() == target.SpanID() {
				linked = true
				for _, kv := range l.Attributes {
					if kv.Key == attribute.Key(AttrBranchFrom) && kv.Value.AsString() != from {
						t.Errorf("link attr = %s", kv.Value.AsString())
					}
				}
			}
		}
	}
	if !linked {
		t.Error("no span links the branched-from entry")
	}
}

// lid returns the id of the fixture entry whose legacy_id is name; the
// fixtures are generated from 0.4 sources by migration, so every entry
// carries the readable id its source gave it.
func lid(t *testing.T, s *agentsession.Session, name string) string {
	t.Helper()
	for _, e := range s.Entries() {
		if e.Base().LegacyID == name {
			return e.Base().ID
		}
	}
	t.Fatalf("no fixture entry with legacy id %s", name)
	return ""
}

// TestExportReplay: a call run again after a crash has a tool span for
// each hand-off: the first ends in flight with the run the crash cut
// off, and the second, in the next run, links to it. A call answered
// without running again ends its hand-off with that run too, and the
// answer's span, linked to it, ends answered (#99, #100).
func TestExportReplay(t *testing.T) {
	sr, tracer := recorder()
	s := loadFixture(t, "replay")
	if _, err := Export(context.Background(), tracer, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	all := spans(sr.Ended())
	cut := all.named(SpanRun).withAttr(AttrRunReason, agentsession.ReasonError)
	if len(cut) != 1 {
		t.Fatalf("cut runs = %d", len(cut))
	}
	deploy := all.named(OpTool + " deploy")
	if len(deploy) != 2 {
		t.Fatalf("deploy spans = %d, want one per hand-off", len(deploy))
	}
	first, again := deploy.withAttr(AttrDispatch, "1"), deploy.withAttr(AttrDispatch, "2")
	if len(first) != 1 || len(again) != 1 {
		t.Fatalf("deploy spans by hand-off: %d and %d", len(first), len(again))
	}
	if attr(first[0], AttrCallState) != agentsession.CallInFlight.String() || first[0].Status().Code != codes.Error || !first[0].EndTime().Equal(cut[0].EndTime()) {
		t.Errorf("first hand-off state %s, status %v, ends %s, run ends %s", attr(first[0], AttrCallState), first[0].Status(), first[0].EndTime(), cut[0].EndTime())
	}
	if !linksTo(again[0], first[0]) || attr(again[0], AttrCallState) != agentsession.CallCompleted.String() {
		t.Errorf("second hand-off state %s, linked %v", attr(again[0], AttrCallState), linksTo(again[0], first[0]))
	}
	notify := all.named(OpTool + " notify")
	handoff, answered := notify.withAttr(AttrCallState, agentsession.CallInFlight.String()), notify.withAttr(AttrCallState, agentsession.CallAnswered.String())
	if len(notify) != 2 || len(handoff) != 1 || len(answered) != 1 || !linksTo(answered[0], handoff[0]) || len(eventAttr(answered[0], EventDecision, AttrVerdict)) != 1 {
		t.Errorf("notify spans = %d: hand-off %d, answered %d", len(notify), len(handoff), len(answered))
	}
}

// TestHeldAfterProceedIsNot: a hold answered by a proceed and a
// dispatch does not leave the call reading held.
func TestHeldAfterProceedIsNot(t *testing.T) {
	sr, tracer := recorder()
	s := agentsession.New(agentsession.Header{ID: "held", Records: agentsession.AllRecords})
	fc := &agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "deploy", Arguments: "{}"}}
	target, err := s.Append(fc)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []agentsession.Entry{
		agentsession.NewDecision("c", target, agentsession.VerdictHold, agentsession.ByPolicy),
		agentsession.NewDecision("c", target, agentsession.VerdictProceed, agentsession.ByHuman),
		agentsession.NewDispatch("c", target),
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Export(context.Background(), tracer, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	tool := spans(sr.Ended()).named(OpTool + " deploy")
	if len(tool) != 1 || attr(tool[0], AttrCallState) != agentsession.CallInFlight.String() {
		t.Errorf("tool spans = %d, state %s", len(tool), attr(tool[0], AttrCallState))
	}
}

// TestStateAfterTheRunEnd: a hand-off its run left pending ends with
// the run in the state the path reads, answered rather than in flight
// for an answer owed its output, and a decision after the run end that
// changes that state reaches a span of its own at finish, which starts
// no earlier than its run.
func TestStateAfterTheRunEnd(t *testing.T) {
	build := func(t *testing.T, after ...func(target string) agentsession.Entry) *agentsession.Session {
		t.Helper()
		s := agentsession.New(agentsession.Header{ID: "later", Records: agentsession.AllRecords})
		if _, err := s.Append(agentsession.NewRunStart("r1", agentsession.SourceInput, "")); err != nil {
			t.Fatal(err)
		}
		target, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "deploy", Arguments: "{}"}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(agentsession.NewDispatch("c", target)); err != nil {
			t.Fatal(err)
		}
		for _, f := range after {
			if _, err := s.Append(f(target)); err != nil {
				t.Fatal(err)
			}
		}
		return s
	}
	end := func(reason string) func(string) agentsession.Entry {
		return func(string) agentsession.Entry { return agentsession.NewRunEnd("r1", reason, "", []string{"c"}) }
	}
	decide := func(verdict string) func(string) agentsession.Entry {
		return func(target string) agentsession.Entry {
			return agentsession.NewDecision("c", target, verdict, agentsession.ByPolicy).WithReason("after the crash")
		}
	}
	start2 := func(string) agentsession.Entry {
		return agentsession.NewRunStart("r2", agentsession.SourceInput, "")
	}
	for name, tt := range map[string]struct {
		after  []func(string) agentsession.Entry
		states []string
	}{
		"answered, output owed": {[]func(string) agentsession.Entry{decide(agentsession.VerdictAnswer), end(agentsession.ReasonAborted)}, []string{"answered"}},
		"held after the run":    {[]func(string) agentsession.Entry{end(agentsession.ReasonError), start2, decide(agentsession.VerdictHold)}, []string{"in_flight", "held"}},
	} {
		t.Run(name, func(t *testing.T) {
			sr, tracer := recorder()
			s := build(t, tt.after...)
			if _, err := Export(context.Background(), tracer, s, s.Leaf()); err != nil {
				t.Fatal(err)
			}
			tool := spans(sr.Ended()).named(OpTool + " deploy")
			var states []string
			for _, sp := range tool {
				states = append(states, attr(sp, AttrCallState))
				if attr(sp, AttrCallState) == "answered" && sp.Status().Code == codes.Error {
					t.Error("an answered call's span reads as an error")
				}
			}
			if fmt.Sprint(states) != fmt.Sprint(tt.states) {
				t.Errorf("tool span states = %v, want %v", states, tt.states)
			}
			runs := spans(sr.Ended()).named(SpanRun)
			if len(tool) == 2 && tool[1].StartTime().Before(runs[len(runs)-1].StartTime()) {
				t.Error("the later span starts before its run")
			}
		})
	}
}
