package otel

import (
	"context"
	"os"
	"path/filepath"
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

func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "sessions", name+".jsonl"))
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
	if _, ok := known["p0000002"]; !ok {
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
