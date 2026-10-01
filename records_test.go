package agentsession

import (
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestCallIDEmpty: a function call with no call ID is refused on
// append and reported by VerifyRecords in a file another writer made
// (#135).
func TestCallIDEmpty(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", Name: "t", Arguments: "{}"}}); !errors.Is(err, ErrCallIDEmpty) {
		t.Errorf("Append = %v, want ErrCallIDEmpty", err)
	}
	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"","name":"t","arguments":"{}"}`
	in, _ := hashedLines(t, Format, call)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, ErrCallIDEmpty) {
		t.Errorf("VerifyRecords = %v, want ErrCallIDEmpty", err)
	}
}

// TestSourceShape: a run start's source is the shape of its segment, a
// resume taking up a call pending when the run began and an input
// otherwise, and VerifyRecords reports one that is not (#147).
func TestSourceShape(t *testing.T) {
	held := "start user calls:a resp hold:a end:input_required "
	for _, tt := range []struct {
		name, script string
		want         error
	}{
		{"resume takes up the held call", held + "start:resume proceed:a dispatch:a out:a resp end:done", nil},
		{"resume after a message", held + "start:resume user proceed:a dispatch:a out:a resp end:done", nil},
		{"input takes up the held call", held + "start:input user proceed:a dispatch:a out:a resp end:done", ErrSourceMismatch},
		{"resume takes up nothing", held + "start:resume user resp end:done", ErrSourceMismatch},
		{"first run as a resume", "start:resume user resp end:done", ErrSourceMismatch},
		{"input with its own call", "start user calls:b resp dispatch:b out:b resp end:done", nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Header{Records: AllRecords})
			for _, e := range seg(t, tt.script) {
				b := e.Base()
				b.ID, b.Parent = "", ""
				if r, ok := e.(*RunEntry); ok && r.IsEnd() {
					if end, err := s.EndRun(r.Reason, ""); err == nil {
						e = end
					}
				}
				if _, err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, tt.want) {
				t.Errorf("VerifyRecords = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestForkPromise: the rules resting on a header's records promise
// apply to what the session wrote, after its base; a fork promising
// dispatch of a session that promised nothing verifies, and its own
// call with no dispatch does not (#144).
func TestForkPromise(t *testing.T) {
	origin := New(Header{})
	for _, e := range seg(t, "user calls:a resp out:a resp") {
		b := e.Base()
		b.ID, b.Parent = "", ""
		if _, err := origin.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if err := origin.VerifyRecords(origin.Leaf()); err != nil {
		t.Fatalf("the origin: %v", err)
	}
	fork, err := Fork(origin, origin.Leaf(), Header{Records: []string{TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if err := fork.VerifyRecords(fork.Leaf()); err != nil {
		t.Errorf("a fork whose prefix promised nothing: %v", err)
	}
	target, err := fork.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(NewDispatch("b", target)); err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(NewItemEntry(openresponses.NewFunctionCallOutput("b", "ran"))); err != nil {
		t.Fatal(err)
	}
	if err := fork.VerifyRecords(fork.Leaf()); err != nil {
		t.Errorf("the fork's own dispatched call: %v", err)
	}
	// Read back as the file it projects to, the prefix carried in it.
	var buf strings.Builder
	if err := Write(&buf, fork); err != nil {
		t.Fatal(err)
	}
	read, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); err != nil {
		t.Errorf("the fork read back: %v", err)
	}
	// The fork's own call with an output and no dispatch is still one.
	bare, err := Fork(origin, origin.Leaf(), Header{Records: []string{TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := bare.Append(NewItemEntry(openresponses.NewFunctionCallOutput("c", "ran"))); err != nil {
		t.Fatal(err)
	}
	if err := bare.VerifyRecords(bare.Leaf()); !errors.Is(err, ErrRecordMissing) {
		t.Errorf("the fork's own undispatched call: %v, want ErrRecordMissing", err)
	}
}

// TestForkTakesUpPrefixCall: a fork whose base falls between a call
// and its output holds the call pending. Running the tool there is the
// fork's own act, owed a dispatch under its promise, and may get
// another output than the origin's; answering it needs none, since the
// prefix promised nothing and a dispatch there may have gone
// unrecorded. An answer the prefix holds stands for the run, so the
// output the fork writes after it needs none either.
func TestForkTakesUpPrefixCall(t *testing.T) {
	promise := Header{Records: []string{TypeDispatch}}
	origin := New(Header{})
	call, err := origin.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fa", CallID: "a", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := origin.Append(NewItemEntry(openresponses.NewFunctionCallOutput("a", "the origin's"))); err != nil {
		t.Fatal(err)
	}
	fork := func(at string) *Session {
		t.Helper()
		f, err := Fork(origin, at, promise)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	verify := func(name string, s *Session, want error) {
		t.Helper()
		var buf strings.Builder
		if err := Write(&buf, s); err != nil {
			t.Fatal(err)
		}
		read, err := Read(strings.NewReader(buf.String()))
		if err != nil {
			t.Fatal(err)
		}
		if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, want) {
			t.Errorf("%s: VerifyRecords = %v, want %v", name, err, want)
		}
	}

	ran := fork(call)
	if _, err := ran.Append(NewDispatch("a", call)); err != nil {
		t.Fatal(err)
	}
	if _, err := ran.Append(NewItemEntry(openresponses.NewFunctionCallOutput("a", "the fork's own"))); err != nil {
		t.Fatal(err)
	}
	verify("run again with a dispatch", ran, nil)

	bare := fork(call)
	if _, err := bare.Append(NewItemEntry(openresponses.NewFunctionCallOutput("a", "the fork's own"))); err != nil {
		t.Fatal(err)
	}
	verify("run again with no dispatch", bare, ErrRecordMissing)

	answered := fork(call)
	if _, err := answered.Append(NewDecision("a", call, VerdictAnswer, ByPolicy)); err != nil {
		t.Fatalf("an answer to a prefix call: %v", err)
	}
	if _, err := answered.Append(NewItemEntry(openresponses.NewFunctionCallOutput("a", "another answer"))); err != nil {
		t.Fatal(err)
	}
	verify("answered differently", answered, nil)

	// The prefix holds the answer; the fork writes the output.
	held := New(Header{})
	hc, _ := held.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}})
	leaf, err := held.Append(NewDecision("b", hc, VerdictAnswer, ByPolicy))
	if err != nil {
		t.Fatal(err)
	}
	after, err := Fork(held, leaf, promise)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := after.Append(NewItemEntry(openresponses.NewFunctionCallOutput("b", "the answer"))); err != nil {
		t.Fatal(err)
	}
	verify("output after a prefix answer", after, nil)

	// A call the fork makes and answers still owes a dispatch.
	own := fork(call)
	oc, _ := own.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}})
	if _, err := own.Append(NewDecision("c", oc, VerdictAnswer, ByPolicy)); !errors.Is(err, ErrAnswerNotDispatched) {
		t.Errorf("an answer to the fork's own call: Append = %v, want ErrAnswerNotDispatched", err)
	}
}

// TestSourceOrphanOutput: the segment's first output decides the
// source even when it is owed to no call, and then takes up nothing.
func TestSourceOrphanOutput(t *testing.T) {
	s := New(Header{Records: AllRecords})
	for _, e := range seg(t, "start user calls:a resp hold:a end:input_required") {
		b := e.Base()
		b.ID, b.Parent = "", ""
		if r, ok := e.(*RunEntry); ok && r.IsEnd() {
			if end, err := s.EndRun(r.Reason, ""); err == nil {
				e = end
			}
		}
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	s.Append(NewRunStart("run-2", SourceResume, ""))
	s.Append(NewItemEntry(openresponses.NewFunctionCallOutput("zzz", "for no call")))
	// The held call taken up after it does not make the run a resume.
	a := Calls(s.Path(s.Leaf()))[0]
	if _, err := s.Append(NewDecision("a", a.Entry.ID, VerdictProceed, ByPolicy)); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrSourceMismatch) {
		t.Errorf("VerifyRecords = %v, want ErrSourceMismatch", err)
	}
}

// TestForkCallState: a call the fork's prefix made, with no dispatch,
// is unknown, since the origin may have promised nothing and run it
// unrecorded; a call the fork made under its own promise with no
// dispatch never started.
func TestForkCallState(t *testing.T) {
	origin := New(Header{})
	call, err := origin.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fa", CallID: "a", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := Fork(origin, call, Header{Records: []string{TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := Write(&buf, fork); err != nil {
		t.Fatal(err)
	}
	read, err := Read(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]*Session{"in memory": fork, "read back": read} {
		pending, err := s.PendingCalls(s.Leaf())
		if err != nil || len(pending) != 2 {
			t.Fatalf("%s: PendingCalls = %v, %v", name, pending, err)
		}
		want := map[string]CallState{"a": CallUnknown, "b": CallNeverStarted}
		for _, c := range pending {
			if got := c.State(s.Header()); got != want[c.ID()] {
				t.Errorf("%s: call %s is %s, want %s", name, c.ID(), got, want[c.ID()])
			}
		}
	}
}
