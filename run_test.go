package agentsession

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// seg builds a run segment from a compact script so the cascade can
// be tested shape by shape. Tokens: "start[:resume]", "end:<reason>",
// "user", "resp[:err|:incomplete]" (a response whose output is the
// calls named in the previous "calls:a,b" token), "calls:a,b",
// "hold:a", "proceed:a", "reject:a", "dispatch:a", "out:a". Each
// "start" opens a new run and each response gets its own ID, so a
// script can hold several runs.
func seg(t *testing.T, script string) []Entry {
	t.Helper()
	s := New(Header{Records: AllRecords})
	var pendingCalls []string
	n, runs, responses := 0, 0, 0
	runID := func() string { return fmt.Sprintf("run-%d", runs) }
	respID := func() string { return fmt.Sprintf("resp-%d", responses) }
	for _, tok := range strings.Fields(script) {
		n++
		kind, arg, _ := strings.Cut(tok, ":")
		var e Entry
		switch kind {
		case "start":
			runs++
			source := SourceInput
			if arg != "" {
				source = arg
			}
			e = NewRunStart(runID(), source, "")
		case "end":
			e = NewRunEnd(runID(), arg, "", nil)
		case "user":
			e = NewItemEntry(openresponses.UserText("hi"))
		case "calls":
			pendingCalls = strings.Split(arg, ",")
			for _, c := range pendingCalls {
				fc := &openresponses.FunctionCall{ID: "fc_" + c, CallID: c, Name: "tool", Arguments: "{}"}
				if _, err := s.Append(&ItemEntry{Item: fc, ResponseID: respID()}); err != nil {
					t.Fatal(err)
				}
			}
			continue
		case "resp":
			r := &ResponseEntry{ResponseID: respID(), Status: "completed"}
			responses++
			switch arg {
			case "err":
				r.Error = &openresponses.ErrorPayload{Code: "server_error", Message: "boom"}
			case "incomplete":
				r.Incomplete = &openresponses.IncompleteDetails{Reason: "max_output_tokens"}
			}
			e = r
		case "hold", "proceed", "reject":
			d := NewDecision(arg, "fc_"+arg, kind, ByPolicy)
			if kind == "reject" {
				d.WithReason("no")
			}
			e = d
		case "dispatch":
			e = NewDispatch(arg, "fc_"+arg)
		case "out":
			e = NewItemEntry(openresponses.NewFunctionCallOutput(arg, "ok"))
		default:
			t.Fatalf("bad token %q", tok)
		}
		if _, err := s.Append(e); err != nil {
			t.Fatalf("%s: %v", tok, err)
		}
	}
	return s.Path(s.Leaf())
}

func TestComputeReason(t *testing.T) {
	tests := []struct {
		name, script, want string
	}{
		{"error wins", "start user calls:a resp:err", ReasonError},
		{"held call, nothing dispatched", "start user calls:a resp hold:a", ReasonInputRequired},
		{"held beside completed", "start user calls:a,b resp hold:a dispatch:b out:b", ReasonInputRequired},
		{"held beside undecided", "start user calls:a,b resp hold:a", ReasonInputRequired},
		{"held beside in flight", "start user calls:a,b resp hold:a dispatch:b", ReasonAborted},
		{"in flight", "start user calls:a resp dispatch:a", ReasonAborted},
		{"undecided pending", "start user calls:a resp", ReasonAborted},
		{"incomplete response", "start user resp:incomplete", ReasonAborted},
		{"no response", "start user", ReasonAborted},
		{"model finished", "start user resp", ReasonDone},
		{"calls all answered", "start user calls:a resp dispatch:a out:a", ReasonStopped},
		{"rejected call answered", "start user calls:a resp reject:a out:a", ReasonStopped},
		{"answered hold", "start user calls:a resp hold:a dispatch:a out:a", ReasonStopped},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := seg(t, tt.script)
			if got := ComputeReason(p, p); got != tt.want {
				t.Errorf("ComputeReason = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestComputeReasonLastRun is the cascade over the last run of a path
// that holds several, which is where the stopped step has to look
// back: a run that answers a call an earlier run's model call made and
// ends without calling the model again holds no response of its own.
// The four shapes the agenteval study measured are the first four
// rows; a Harbor-side reader filtering on the reason saw two of them
// as aborted.
func TestComputeReasonLastRun(t *testing.T) {
	tests := []struct {
		name, script, want string
	}{
		{"step limit", "start user calls:a,b resp dispatch:a out:a dispatch:b out:b", ReasonStopped},
		{"guard stop", "start user calls:a resp dispatch:a out:a", ReasonStopped},
		{"refusal on resume", "start user calls:a resp hold:a end:input_required start:resume reject:a out:a", ReasonStopped},
		{"terminating resume", "start user calls:a resp hold:a end:input_required start:resume proceed:a dispatch:a out:a", ReasonStopped},

		{"resume that answers one of two calls", "start user calls:a,b resp hold:a hold:b end:input_required start:resume proceed:a dispatch:a out:a", ReasonAborted},
		{"resume that leaves the call in flight", "start user calls:a resp hold:a end:input_required start:resume proceed:a dispatch:a", ReasonAborted},
		{"resume that answers nothing", "start user calls:a resp hold:a end:input_required start:resume user", ReasonAborted},
		{"resume that calls the model again", "start user calls:a resp hold:a end:input_required start:resume proceed:a dispatch:a out:a resp", ReasonDone},
		{"a call an earlier run left pending", "start user calls:a resp dispatch:a end:aborted start:resume user calls:b resp dispatch:b out:b", ReasonAborted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := seg(t, tt.script)
			runs := Runs(path)
			if len(runs) == 0 {
				t.Fatal("no runs")
			}
			last := runs[len(runs)-1]
			if got := ComputeReason(last.Path, last.Segment); got != tt.want {
				t.Errorf("ComputeReason = %s, want %s", got, tt.want)
			}
			// A written reason that matches the shape verifies; the
			// value the recorder used to write for these does not.
			last.End = NewRunEnd(last.RunID(), tt.want, "", last.Pending())
			last.Segment = append(last.Segment, last.End)
			last.Path = append(last.Path, last.End)
			if err := last.Verify(); err != nil {
				t.Errorf("Verify a %s run: %v", tt.want, err)
			}
			// Appending to one run's path leaves its siblings alone.
			for i, r := range runs[:len(runs)-1] {
				if got := r.Path[len(r.Path)-1]; got != r.Segment[len(r.Segment)-1] {
					t.Errorf("run %d's path now ends at %s, not at the end of its segment", i, got.Base().ID)
				}
			}
		})
	}
}

// TestComputeReasonCallsOutsideTheSegment: whether the model asked
// for a tool is a property of its response's output items, which a
// writer may put outside the run they are answered in. A run that
// starts between a function call and its response holds the response
// and not the call, and the cascade must still see a call the path
// never answered.
func TestComputeReasonCallsOutsideTheSegment(t *testing.T) {
	s := New(Header{Records: AllRecords})
	for _, e := range []Entry{
		&ConfigEntry{Model: "gpt-5"},
		NewItemEntry(openresponses.UserText("hi")),
		&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_a", CallID: "a", Name: "tool", Arguments: "{}"}, ResponseID: "resp-0"},
		NewRunStart("run-1", SourceInput, ""),
		&ResponseEntry{ResponseID: "resp-0", Status: "completed"},
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.Runs(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	r := runs[len(runs)-1]
	if got := ComputeReason(r.Path, r.Segment); got != ReasonAborted {
		t.Errorf("ComputeReason = %s, want %s: call a is on the path with no output", got, ReasonAborted)
	}
	// And a run end that calls it done disagrees with the shape.
	end := NewRunEnd(r.RunID(), ReasonDone, "", nil)
	if _, err := s.Append(end); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrReasonMismatch) {
		t.Errorf("VerifyRecords = %v, want ErrReasonMismatch", err)
	}
}

func TestCallState(t *testing.T) {
	promised := Header{Records: AllRecords}
	silent := Header{}
	tests := []struct {
		name, script string
		h            Header
		want         CallState
	}{
		{"completed", "calls:a resp dispatch:a out:a", promised, CallCompleted},
		{"rejected is completed", "calls:a resp reject:a out:a", promised, CallCompleted},
		{"held", "calls:a resp hold:a", promised, CallHeld},
		{"held then answered", "calls:a resp hold:a dispatch:a", promised, CallInFlight},
		{"in flight", "calls:a resp dispatch:a", promised, CallInFlight},
		{"never started", "calls:a resp", promised, CallNeverStarted},
		{"unknown without promise", "calls:a resp", silent, CallUnknown},
		{"in flight without promise", "calls:a resp dispatch:a", silent, CallInFlight},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := Calls(seg(t, tt.script))
			if len(calls) != 1 {
				t.Fatalf("%d calls", len(calls))
			}
			if got := calls[0].State(tt.h); got != tt.want {
				t.Errorf("State = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestCallArgs(t *testing.T) {
	s := New(Header{})
	fc := &openresponses.FunctionCall{ID: "fc_a", CallID: "a", Name: "tool", Arguments: `{"x":1}`}
	if _, err := s.Append(&ItemEntry{Item: fc}); err != nil {
		t.Fatal(err)
	}
	calls := Calls(s.Path(s.Leaf()))
	if got := calls[0].Args(); got != `{"x":1}` {
		t.Errorf("Args = %s", got)
	}
	if _, err := s.Append(NewDecision("a", "fc_a", VerdictProceed, ByHuman).WithArgs([]byte(`{"x":2}`))); err != nil {
		t.Fatal(err)
	}
	calls = Calls(s.Path(s.Leaf()))
	if got := calls[0].Args(); got != `{"x":2}` {
		t.Errorf("Args after rewrite = %s", got)
	}
}

func TestRunsAcrossBranch(t *testing.T) {
	s := New(Header{Records: AllRecords})
	if _, err := s.Append(NewRunStart("r1", SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	userID, err := s.Append(NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ResponseEntry{ResponseID: "x", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	// Branch back to the user item mid-run: the branch closes r1 and
	// the next append on the new branch begins r2.
	if err := s.Branch(userID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewRunStart("r2", SourceInput, "retry")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ResponseEntry{ResponseID: "y", Status: "completed"}); err != nil {
		t.Fatal(err)
	}
	end, err := s.EndRun(ReasonDone, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(end); err != nil {
		t.Fatal(err)
	}
	runs, err := s.Runs(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].RunID() != "r1" || runs[0].End != nil || runs[1].RunID() != "r2" || runs[1].End == nil {
		t.Fatalf("runs = %+v", runs)
	}
	if len(runs[0].Segment) != 2 {
		t.Errorf("r1 segment has %d entries, want start and user item", len(runs[0].Segment))
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	open, err := s.OpenRun(s.Leaf())
	if err != nil || open != nil {
		t.Errorf("OpenRun = %v, %v; want none", open, err)
	}
	if _, err := s.EndRun(ReasonDone, ""); err == nil {
		t.Error("EndRun with no open run succeeded")
	}
}

func TestEndRunPending(t *testing.T) {
	s := New(Header{Records: AllRecords})
	for _, e := range seg(t, "start user calls:a,b resp hold:a dispatch:b out:b") {
		b := e.Base()
		b.ID, b.Parent, b.Timestamp = "", "", fixedTime
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	end, err := s.EndRun(ReasonInputRequired, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(end.Pending) != 1 || end.Pending[0] != "a" {
		t.Errorf("pending = %v, want [a]", end.Pending)
	}
	pending, err := s.PendingCalls(s.Leaf())
	if err != nil || len(pending) != 1 || pending[0].ID() != "a" || !pending[0].Held() {
		t.Errorf("PendingCalls = %v, %v", pending, err)
	}
}

func TestVerifyRecords(t *testing.T) {
	tests := []struct {
		name   string
		script string
		header Header
		want   error
	}{
		{"agrees", "start user calls:a resp hold:a end:input_required", Header{}, nil},
		{"pending disagrees", "start user calls:a resp hold:a end:input_required", Header{}, nil},
		{"reason disagrees", "start user calls:a resp hold:a end:done", Header{}, ErrReasonMismatch},
		{"written error stands", "start user resp end:error", Header{}, nil},
		{"written interrupted stands", "start user calls:a resp dispatch:a end:interrupted", Header{}, nil},
		{"missing dispatch, promised", "user calls:a resp out:a", Header{Records: []string{TypeDispatch}}, ErrRecordMissing},
		{"missing dispatch, not promised", "user calls:a resp out:a", Header{}, nil},
		{"rejected call needs no dispatch", "user calls:a resp reject:a out:a", Header{Records: []string{TypeDispatch}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.header)
			for _, e := range seg(t, tt.script) {
				b := e.Base()
				b.ID, b.Parent = "", ""
				if r, ok := e.(*RunEntry); ok && r.IsEnd() {
					// Write the pending list the segment implies, as a
					// harness would through EndRun.
					if end, err := s.EndRun(r.Reason, ""); err == nil {
						e = end
					}
				}
				if _, err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			err := s.VerifyRecords(s.Leaf())
			if !errors.Is(err, tt.want) {
				t.Errorf("VerifyRecords = %v, want %v", err, tt.want)
			}
		})
	}
	t.Run("pending list disagrees", func(t *testing.T) {
		s := New(Header{})
		for _, e := range seg(t, "start user calls:a resp hold:a") {
			b := e.Base()
			b.ID, b.Parent = "", ""
			if _, err := s.Append(e); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Append(NewRunEnd("run-1", ReasonInputRequired, "", []string{"b"})); err != nil {
			t.Fatal(err)
		}
		if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrReasonMismatch) {
			t.Errorf("VerifyRecords = %v", err)
		}
	})
}

func TestDispatchAfterReject(t *testing.T) {
	s := New(Header{})
	for _, e := range seg(t, "user calls:a resp reject:a") {
		b := e.Base()
		b.ID, b.Parent = "", ""
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(NewDispatch("a", "fc_a")); !errors.Is(err, ErrCallRejected) {
		t.Errorf("Append dispatch after reject = %v, want ErrCallRejected", err)
	}
}

func TestSubsessionID(t *testing.T) {
	got := SubsessionID("01995b2a-0000-7000-8000-000000000001", "call_1")
	if got != SubsessionID("01995b2a-0000-7000-8000-000000000001", "call_1") {
		t.Fatal("not deterministic")
	}
	if len(got) != 36 || got[14] != '5' || !strings.ContainsRune("89ab", rune(got[19])) {
		t.Errorf("not a UUIDv5: %s", got)
	}
	if got == SubsessionID("01995b2a-0000-7000-8000-000000000001", "call_2") {
		t.Error("two calls share an ID")
	}
	// Pinned so another implementation can check its derivation.
	if want := "91d0242a-bd8b-551a-a26c-d3b0d4889b4f"; got != want {
		t.Errorf("SubsessionID = %s, want %s", got, want)
	}
}

func TestLifecycleValidation(t *testing.T) {
	tests := []struct {
		name string
		e    Entry
		want string
	}{
		{"run without id", &RunEntry{Phase: RunStart, Source: SourceInput}, "run_id"},
		{"run start without source", &RunEntry{RunID: "r", Phase: RunStart}, "source"},
		{"run end without reason", &RunEntry{RunID: "r", Phase: RunEnd}, "reason"},
		{"run bad phase", &RunEntry{RunID: "r", Phase: "middle"}, "phase"},
		{"dispatch without target", &DispatchEntry{CallID: "a"}, "target"},
		{"decision without verdict", &DecisionEntry{CallID: "a", Target: "t"}, "verdict"},
		{"reject without reason", &DecisionEntry{CallID: "a", Target: "t", Verdict: VerdictReject}, "reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Header{})
			_, err := s.Append(tt.e)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Append = %v, want error mentioning %q", err, tt.want)
			}
			if _, err := MarshalEntry(tt.e); err == nil {
				t.Error("MarshalEntry succeeded")
			}
		})
	}
}

func TestLifecycleRoundTrip(t *testing.T) {
	lines := []string{
		`{"type":"run","id":"a","parent":null,"ts":"2026-09-20T12:00:00Z","run_id":"r","phase":"start","source":"input","ref":"cron:x","acme:extra":1}`,
		`{"type":"run","id":"b","parent":"a","ts":"2026-09-20T12:00:01Z","run_id":"r","phase":"end","reason":"done","pending":[]}`,
		`{"type":"dispatch","id":"c","parent":"b","ts":"2026-09-20T12:00:02Z","call_id":"call_1","target":"x","acme:host":"h"}`,
		`{"type":"decision","id":"d","parent":"c","ts":"2026-09-20T12:00:03Z","call_id":"call_1","target":"x","verdict":"proceed","by":"human","args":{"cmd":"ls"}}`,
	}
	for _, line := range lines {
		e, err := UnmarshalEntry([]byte(line))
		if err != nil {
			t.Fatalf("%s: %v", line, err)
		}
		out, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != line {
			t.Errorf("round trip changed the line\nwant %s\ngot  %s", line, out)
		}
	}
}

func TestHeaderRecords(t *testing.T) {
	h := Header{Records: []string{TypeDispatch, "acme:audit"}}
	if !h.HasRecord(TypeDispatch) || !h.HasRecord("acme:audit") || h.HasRecord(TypeRun) {
		t.Errorf("HasRecord over %v", h.Records)
	}
	if (Header{}).HasRecord(TypeDispatch) {
		t.Error("empty header promises a record")
	}
}
