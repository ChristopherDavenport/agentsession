package agentsession

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestFailedOutput: 0.14 lets an output's entry say its call failed, a
// record member a reader maps to its protocol's failure state. Calls
// reads it as Call.Failed, the call is completed all the same, and the
// member is taken where the format defines it, true on an output's
// entry, in a file of any minor; elsewhere it is kept as written (#264).
func TestFailedOutput(t *testing.T) {
	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"t","arguments":"{}"}`
	for _, format := range []string{Format, "agentsession/0.13"} {
		in, ids := hashedLines(t, format, call,
			`"type":"item","item":{"type":"function_call_output","call_id":"c","output":"Error: exit status 1"},"failed":true`,
			`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},"failed":true`,
			`"type":"item","item":{"type":"function_call","id":"fc2","call_id":"d","name":"t","arguments":"{}"}`,
			`"type":"item","item":{"type":"function_call_output","call_id":"d","output":"ok"},"failed":"yes"`)
		s, err := Read(strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		calls, err := s.Calls(s.Leaf())
		if err != nil {
			t.Fatal(err)
		}
		if len(calls) != 2 || !calls[0].Failed() || calls[1].Failed() {
			t.Fatalf("%s: calls = %+v", format, calls)
		}
		if st := calls[0].State(s.Header()); st != CallCompleted {
			t.Errorf("%s: a failed call reads %s, want completed", format, st)
		}
		msg, _ := s.Entry(ids[2])
		if m := msg.(*ItemEntry); m.Failed || string(m.Unknown["failed"]) != "true" {
			t.Errorf("%s: failed on a message: Failed %v, Unknown %s", format, m.Failed, m.Unknown["failed"])
		}
		out, _ := s.Entry(ids[4])
		if o := out.(*ItemEntry); o.Failed || string(o.Unknown["failed"]) != `"yes"` {
			t.Errorf("%s: failed in another form: Failed %v, Unknown %s", format, o.Failed, o.Unknown["failed"])
		}
		var buf bytes.Buffer
		if err := Write(&buf, s); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(&buf); err != nil {
			t.Errorf("%s: written back: %v", format, err)
		}
	}
	// The member changes no request: the output is sent as it is.
	s := New(Header{})
	if _, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	out := NewItemEntry(openresponses.NewFunctionCallOutput("c", "Error: timed out"))
	out.Failed = true
	if _, err := s.Append(out); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	data, err := ctx.RequestJSON()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("failed")) {
		t.Errorf("the request carries the member: %s", data)
	}
	if _, err := s.Append(&ItemEntry{Item: openresponses.UserText("x"), Failed: true}); err == nil {
		t.Error("Append took failed on an item that is not an output")
	}
}

// withdrawalSession is a session that promises dispatches, holding a
// call c, its dispatch and, when withdrawn, the reject that withdraws
// it, and the target of the call.
func withdrawalSession(t *testing.T, withdrawn bool) (*Session, string) {
	t.Helper()
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDispatch("c", target)); err != nil {
		t.Fatal(err)
	}
	if withdrawn {
		if _, err := s.Append(NewWithdrawal("c", target, ByPolicy, "a dispatch subscriber refused the hand-off")); err != nil {
			t.Fatalf("withdrawal: %v", err)
		}
	}
	return s, target
}

// TestWithdrawnDispatch: a reject carrying withdrawn directly after a
// call's dispatch withdraws the hand-off. Calls reads the call as never
// started when it has no other dispatch, and as it was before the
// dispatch otherwise; nothing else reads the reject as a decision, so
// the call may be dispatched again, decided or answered by its output
// (#226). Before 0.14 the reject was refused after a dispatch and the
// call read in flight.
func TestWithdrawnDispatch(t *testing.T) {
	s, target := withdrawalSession(t, true)
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if st := c.State(s.Header()); st != CallNeverStarted {
		t.Errorf("a withdrawn call reads %s, want never_started", st)
	}
	if c.Dispatch != nil || len(c.Dispatches) != 0 || len(c.Decisions) != 0 || len(c.Withdrawn) != 1 || c.Rejected() {
		t.Errorf("call = %+v", c)
	}
	if got := s.Dispatches(target); len(got) != 0 {
		t.Errorf("the session still counts the withdrawn hand-off: %d", len(got))
	}
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("VerifyRecords: %v", err)
	}
	// The call may be handed off again, and then it may have run.
	if _, err := s.Append(NewDispatch("c", target)); err != nil {
		t.Fatalf("a second dispatch: %v", err)
	}
	calls, _ = s.Calls(s.Leaf())
	if st := calls[0].State(s.Header()); st != CallInFlight || len(calls[0].Dispatches) != 1 {
		t.Errorf("dispatched again: %s with %d dispatches", st, len(calls[0].Dispatches))
	}
	// A second hand-off withdrawn leaves the call never started again;
	// a reject after that is a plain one, since no dispatch stands.
	if _, err := s.Append(NewWithdrawal("c", target, ByPolicy, "again")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDecision("c", target, VerdictReject, ByPolicy).WithReason("not now")); err != nil {
		t.Fatalf("a reject after the withdrawals: %v", err)
	}
	calls, _ = s.Calls(s.Leaf())
	if st := calls[0].State(s.Header()); st != CallRejected || len(calls[0].Withdrawn) != 2 {
		t.Errorf("rejected after two withdrawals: %s, %d withdrawn", st, len(calls[0].Withdrawn))
	}

	// Read back, in a file of any minor, it reads the same.
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{Format, "agentsession/0.13"} {
		read, err := Read(bytes.NewReader(bytes.Replace(buf.Bytes(), []byte(`"format":"`+Format+`"`), []byte(`"format":"`+format+`"`), 1)))
		if err != nil {
			t.Fatal(err)
		}
		if err := read.VerifyRecords(read.Leaf()); err != nil {
			t.Errorf("%s: VerifyRecords: %v", format, err)
		}
		calls, _ := read.Calls(read.Leaf())
		if st := calls[0].State(read.Header()); st != CallRejected || len(calls[0].Withdrawn) != 2 {
			t.Errorf("%s: %s, %d withdrawn", format, st, len(calls[0].Withdrawn))
		}
	}
}

// TestWithdrawalOnlyAfterItsDispatch: a withdrawal is written directly
// after the dispatch it withdraws among its call's entries, and nowhere
// else. Append refuses one anywhere else with ErrBadWithdrawal; a reader
// reads one found elsewhere as a reject without the member, which after
// a dispatch is the reject the format forbids.
func TestWithdrawalOnlyAfterItsDispatch(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewWithdrawal("c", target, ByPolicy, "nothing to withdraw")); !errors.Is(err, ErrBadWithdrawal) {
		t.Errorf("a withdrawal before any dispatch: Append = %v, want ErrBadWithdrawal", err)
	}
	for _, e := range []Entry{NewDispatch("c", target), NewDecision("c", target, VerdictHold, ByPolicy)} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Append(NewWithdrawal("c", target, ByPolicy, "too late")); !errors.Is(err, ErrBadWithdrawal) {
		t.Errorf("a withdrawal after a hold: Append = %v, want ErrBadWithdrawal", err)
	}
	if _, err := s.Append(&DecisionEntry{CallID: "c", Target: target, Verdict: VerdictHold, Withdrawn: true}); err == nil {
		t.Error("Append took withdrawn on a hold")
	}

	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"t","arguments":"{}"}`
	_, ids := hashedLines(t, Format, call)
	dispatch := `"type":"dispatch","call_id":"c","target":"` + ids[0] + `"`
	withdrawal := `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"reject","reason":"no","withdrawn":true`
	for name, tt := range map[string]struct {
		bodies []string
		want   error
	}{
		"directly after":       {[]string{call, dispatch, withdrawal}, nil},
		"after a hold":         {[]string{call, dispatch, `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"hold"`, withdrawal}, ErrRejectDispatched},
		"after the output":     {[]string{call, dispatch, `"type":"item","item":{"type":"function_call_output","call_id":"c","output":"ok"}`, withdrawal}, ErrRejectDispatched},
		"twice":                {[]string{call, dispatch, withdrawal, withdrawal + `,"x":1`}, nil},
		"on a hold, kept":      {[]string{call, dispatch, `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"hold","withdrawn":true`}, nil},
		"before any dispatch":  {[]string{call, withdrawal}, nil},
		"a second, after both": {[]string{call, dispatch, dispatch + `,"x":1`, withdrawal, withdrawal + `,"x":2`}, ErrRejectDispatched},
	} {
		in, _ := hashedLines(t, Format, tt.bodies...)
		read, err := Read(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, tt.want) {
			t.Errorf("%s: VerifyRecords = %v, want %v", name, err, tt.want)
		}
	}
}

// TestWithdrawalAmongOtherCalls: directly after means among the call's
// own entries, so a withdrawal written after another call's dispatch, as
// a serial batch writes one, still withdraws its own.
func TestWithdrawalAmongOtherCalls(t *testing.T) {
	s := New(Header{Records: AllRecords})
	a, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fa", CallID: "a", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{NewDispatch("a", a), NewDispatch("b", b), NewWithdrawal("a", a, ByPolicy, "vetoed")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	calls, _ := s.Calls(s.Leaf())
	if calls[0].State(s.Header()) != CallNeverStarted || calls[1].State(s.Header()) != CallInFlight {
		t.Errorf("states %s, %s", calls[0].State(s.Header()), calls[1].State(s.Header()))
	}
	if len(s.Dispatches(a)) != 0 || len(s.Dispatches(b)) != 1 {
		t.Errorf("dispatches a %d, b %d", len(s.Dispatches(a)), len(s.Dispatches(b)))
	}
}

// TestFromLink: an item's from_link names a peer link before it on its
// path, and is provenance: the context hash leaves it out, as it does
// queued_from, and a from_link in another form is kept as written and
// names nothing (#248).
func TestFromLink(t *testing.T) {
	msg := `"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"report"}]}`
	peer := `"type":"link","rel":"peer","session":"","call_id":"c","url":"https://peer.example"`
	_, ids := hashedLines(t, Format, peer)
	for name, tt := range map[string]struct {
		bodies []string
		want   error
	}{
		"names the peer link":   {[]string{peer, `"type":"item",` + msg + `,"from_link":"` + ids[0] + `"`}, nil},
		"names no entry":        {[]string{peer, `"type":"item",` + msg + `,"from_link":"sha256:00"`}, ErrBadFromLink},
		"names a later link":    {[]string{`"type":"item",` + msg + `,"from_link":"` + ids[0] + `"`, peer}, ErrBadFromLink},
		"in another form, kept": {[]string{peer, `"type":"item",` + msg + `,"from_link":7`}, nil},
	} {
		in, _ := hashedLines(t, Format, tt.bodies...)
		s, err := Read(strings.NewReader(in))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, tt.want) {
			t.Errorf("%s: VerifyRecords = %v, want %v", name, err, tt.want)
		}
	}
	// Two sessions whose context entries differ only in from_link share
	// a context hash.
	hashes := map[string]bool{}
	for _, from := range []string{"", "a"} {
		s := New(Header{})
		link, err := s.Append(NewPeerLink("c", Callee{URL: "https://peer.example"}))
		if err != nil {
			t.Fatal(err)
		}
		item := NewItemEntry(openresponses.UserText("report"))
		item.Timestamp = fixedTime
		if from != "" {
			item.FromLink = link
		}
		id, err := s.Append(item)
		if err != nil {
			t.Fatal(err)
		}
		h, err := s.ContextHash(id)
		if err != nil {
			t.Fatal(err)
		}
		hashes[h] = true
	}
	if len(hashes) != 1 {
		t.Errorf("from_link moved the context hash: %v", hashes)
	}
}

// TestOmitPlaceholderForms: omit's outputs and images are taken in the
// forms 0.14 defines, in a file of any minor; a member in another form
// is kept as written and replaces nothing, and the line writes back as
// it was (#262).
func TestOmitPlaceholderForms(t *testing.T) {
	for _, tt := range []struct {
		omit            string
		outputs, images bool
	}{
		{`{"outputs":{"text":"[x]","items":["a"]}}`, true, false},
		{`{"outputs":{"text":"[x]"}}`, true, false},
		{`{"images":{"keep":0,"placeholder":"[x]"}}`, false, true},
		{`{"images":{"before":"a","placeholder":"[x]"}}`, false, true},
		{`{"items":["a"],"outputs":{"items":["a"]}}`, false, false},
		{`{"items":["a"],"images":{"keep":2,"before":"a","placeholder":"[x]"}}`, false, false},
		{`{"items":["a"],"images":{"keep":-1,"placeholder":"[x]"}}`, false, false},
		{`{"items":["a"],"images":{"keep":2}}`, false, false},
		{`{"items":["a"],"images":{"before":"","placeholder":"[x]"}}`, false, false},
	} {
		for _, format := range []string{Format, "agentsession/0.13"} {
			in, _ := hashedLines(t, format, `"type":"config","model":"m","omit":`+tt.omit)
			s, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatalf("%s: %v", tt.omit, err)
			}
			c := s.Entries()[0].(*ConfigEntry)
			if c.Omit == nil || (c.Omit.Outputs != nil) != tt.outputs || (c.Omit.Images != nil) != tt.images {
				t.Errorf("%s as %s: omit = %+v", tt.omit, format, c.Omit)
			}
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			if _, err := Read(&buf); err != nil {
				t.Errorf("%s as %s: written back: %v", tt.omit, format, err)
			}
		}
	}
	// A delta adds outputs' items and sets the text, replaces images, and
	// {} clears everything.
	var o Omit
	o = o.merged(Omit{Outputs: &OmitOutputs{Text: "a", Items: []string{"x"}}, Images: &OmitImages{Keep: 3, Placeholder: "p"}})
	o = o.merged(Omit{Outputs: &OmitOutputs{Text: "b", Items: []string{"y", "x"}}, Images: &OmitImages{Before: "e", Placeholder: "q"}})
	if o.Outputs.Text != "b" || !slices.Equal(o.Outputs.Items, []string{"x", "y"}) || *o.Images != (OmitImages{Before: "e", Placeholder: "q"}) {
		t.Errorf("merged = %+v %+v", o.Outputs, o.Images)
	}
	if o.merged(Omit{}).IsZero() != true {
		t.Error("{} does not clear outputs and images")
	}
	// OmitDelta writes what changed, and refuses to drop what a delta
	// cannot.
	st := Settings{Omit: o}
	if d, ok := st.OmitDelta(o); !ok || d != nil {
		t.Errorf("OmitDelta(in force) = %+v, %v", d, ok)
	}
	if d, ok := st.OmitDelta(Omit{Outputs: &OmitOutputs{Text: "b", Items: []string{"x", "y", "z"}}, Images: o.Images}); !ok || d == nil || d.Images != nil || !slices.Equal(d.Outputs.Items, []string{"z"}) {
		t.Errorf("OmitDelta(one more output) = %+v, %v", d, ok)
	}
	if _, ok := st.OmitDelta(Omit{Outputs: &OmitOutputs{Text: "b", Items: []string{"x"}}, Images: o.Images}); ok {
		t.Error("OmitDelta dropped an output in force")
	}
	if _, ok := st.OmitDelta(Omit{Outputs: o.Outputs}); ok {
		t.Error("OmitDelta dropped the images rule in force")
	}
}

// TestPlaceholderDivergence: a response whose recorded hash is the
// request as the path holds it, though the omit in force sends a
// placeholder, is a divergence, as one recorded over an item the omit
// leaves out is.
func TestPlaceholderDivergence(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&ConfigEntry{Model: "m"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(&openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputImage{ImageURL: "data:image/png;base64,AA"}}})); err != nil {
		t.Fatal(err)
	}
	unedited := fixtureHash(t, s)
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Images: &OmitImages{Placeholder: "[image]"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.ReplacedItems) != 1 || ctx.ReplacedItems[0].Parts != 1 {
		t.Fatalf("replaced = %+v", ctx.ReplacedItems)
	}
	if text, ok := ctx.Items[0].(*openresponses.Message).Content[0].(*openresponses.InputText); !ok || text.Text != "[image]" {
		t.Errorf("sent %+v", ctx.Items[0])
	}
	id, err := s.Append(&ResponseEntry{ResponseID: "r", Status: openresponses.ResponseStatusCompleted, RequestHash: unedited})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(id); !errors.Is(err, ErrOmitDivergence) {
		t.Errorf("Verify = %v, want ErrOmitDivergence", err)
	}
}

// TestSidecarRequestHash: in a sidecar session the request hash covers
// the request as the path rebuilds it, each sidecar: URL as recorded,
// and not the request with the blob's bytes the writer sends (#5).
func TestSidecarRequestHash(t *testing.T) {
	ref := "sidecar:sha256:" + strings.Repeat("ab", 32)
	s := New(Header{Media: MediaSidecar})
	if _, err := s.Append(NewItemEntry(&openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputImage{ImageURL: ref}}})); err != nil {
		t.Fatal(err)
	}
	recorded := fixtureHash(t, s)
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	data, err := ctx.RequestJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(ref)) {
		t.Fatalf("the rebuilt request does not carry the reference: %s", data)
	}
	sent, err := HashRequestJSON(bytes.Replace(data, []byte(ref), []byte("data:image/png;base64,AA"), 1))
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.Append(&ResponseEntry{ResponseID: "r", Status: openresponses.ResponseStatusCompleted, RequestHash: recorded})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(id); err != nil {
		t.Errorf("the reference-form hash: %v", err)
	}
	if sent == recorded {
		t.Error("the resolved request hashes as the recorded one")
	}
}

// TestResumeWithAResponseIsNotEmpty: a response in a resume's segment
// is a model call, which an input's retry makes, so a segment holding
// one is not empty even with no entry in context, and a run written
// resume over it is reported, as 0.13 reported it.
func TestResumeWithAResponseIsNotEmpty(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		NewDecision("c", target, VerdictHold, ByPolicy),
		NewRunStart("r", SourceResume, ""),
		&ResponseEntry{Status: openresponses.ResponseStatusFailed, Error: &openresponses.ErrorPayload{Code: "server_error", Message: "x"}},
		NewRunEnd("r", ReasonError, "", []string{}),
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	runs, _ := s.Runs(s.Leaf())
	if runs[0].Empty() {
		t.Error("a resume whose segment holds a response is empty")
	}
	if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrSourceMismatch) {
		t.Errorf("VerifyRecords = %v, want ErrSourceMismatch", err)
	}
}

// TestMalformedPlaceholderClearsNothing: an outputs or images member in
// a form the format does not define is carried, as an undefined
// reasoning value is: it changes nothing, and the object is not the
// empty one that clears what is in force (review finding 3).
func TestMalformedPlaceholderClearsNothing(t *testing.T) {
	msg := `"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"x"}]}`
	_, ids := hashedLines(t, Format, msg)
	for _, member := range []string{`{"images":{"keep":3}}`, `{"outputs":{"items":["a"]}}`, `{"images":"all"}`} {
		in, _ := hashedLines(t, Format, msg,
			`"type":"config","model":"m","omit":{"items":["`+ids[0]+`"]}`,
			`"type":"config","omit":`+member)
		s, err := Read(strings.NewReader(in))
		if err != nil {
			t.Fatal(err)
		}
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		if len(ctx.OmittedItems) != 1 || len(ctx.Items) != 0 {
			t.Errorf("%s cleared the omit in force: %+v", member, ctx.Settings.Omit)
		}
	}
}
