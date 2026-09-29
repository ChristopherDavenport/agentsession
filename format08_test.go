package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// TestOmittedInForce: the omitted parts stay in force until a config
// entry carries the member. A delta without it leaves the list, one
// with it replaces the list whole, [] clears it, and a replace without
// it clears it with the rest.
func TestOmittedInForce(t *testing.T) {
	a := []OmittedPart{{ID: "a", Reason: "budget", Size: 10}}
	b := []OmittedPart{{ID: "b", Reason: "stale", Size: 20}}
	model := func(m string) *ConfigEntry { return &ConfigEntry{Model: m} }
	for name, tt := range map[string]struct {
		deltas []*ConfigEntry
		want   []string
	}{
		"written once":            {[]*ConfigEntry{{InstructionsOmitted: a}}, []string{"a"}},
		"a delta without it":      {[]*ConfigEntry{{InstructionsOmitted: a}, model("m2")}, []string{"a"}},
		"a delta with a new list": {[]*ConfigEntry{{InstructionsOmitted: a}, {InstructionsOmitted: b}}, []string{"b"}},
		"cleared by []":           {[]*ConfigEntry{{InstructionsOmitted: a}, {InstructionsOmitted: []OmittedPart{}}}, nil},
		"cleared by a replace":    {[]*ConfigEntry{{InstructionsOmitted: a}, {Model: "m2", Replace: true}}, nil},
		"a replace carrying one":  {[]*ConfigEntry{{InstructionsOmitted: a}, {Model: "m2", Replace: true, InstructionsOmitted: b}}, []string{"b"}},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Header{})
			if _, err := s.Append(model("m")); err != nil {
				t.Fatal(err)
			}
			for _, d := range tt.deltas {
				if _, err := s.Append(d); err != nil {
					t.Fatal(err)
				}
			}
			// Read back, so [] is the line's and not the caller's slice.
			var buf bytes.Buffer
			if err := Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			read, err := Read(&buf)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := read.Context()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, o := range ctx.InstructionsOmitted() {
				got = append(got, o.ID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("omitted in force = %v, want %v", got, tt.want)
			}
			req, err := ctx.Request()
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(req)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(data, []byte("omitted")) {
				t.Errorf("the omitted parts reached the request: %s", data)
			}
		})
	}
}

// TestOmittedAcrossCompaction: a compaction's checkpoint carries the
// list in force, so it survives the fold; a checkpoint from before 0.8
// without the list has none in force, and one whose member is not a
// list of parts is kept as written.
func TestOmittedAcrossCompaction(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&ConfigEntry{Model: "m", InstructionsOmitted: []OmittedPart{{ID: "a", Reason: "budget"}}}); err != nil {
		t.Fatal(err)
	}
	first := appendText(t, s, "one")
	appendText(t, s, "two")
	comp, err := s.Compact(first, openresponses.UserText("summary"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	if len(comp.Config.InstructionsOmitted) != 1 {
		t.Fatalf("checkpoint = %+v", comp.Config)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.InstructionsOmitted(); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("after the fold, omitted = %v", got)
	}

	for name, member := range map[string]string{"absent": ``, "another form": `,"instructions_omitted":"budget"`} {
		t.Run("0.7 checkpoint, "+name, func(t *testing.T) {
			head := `{"type":"session","format":"agentsession/0.7","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
			hashed := func(body, parent string) (string, string) {
				t.Helper()
				p := "null"
				if parent != "" {
					p = `"` + parent + `"`
				}
				line := `{` + body + `,"parent":` + p + `,"ts":"2026-09-17T16:00:01Z"}`
				id, _, err := EntryHashes([]byte(line))
				if err != nil {
					t.Fatal(err)
				}
				return id, `{"id":"` + id + `",` + line[1:]
			}
			cfg, cfgLine := hashed(`"type":"config","model":"m","instructions_omitted":[{"id":"a"}]`, "")
			msg, msgLine := hashed(`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}`, cfg)
			_, compLine := hashed(`"type":"compaction","first_kept":"`+msg+`","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"s"}]},"config":{"model":"m"`+member+`}`, msg)
			in := strings.Join([]string{head, cfgLine, msgLine, compLine}, "\n") + "\n"
			read, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := read.Context()
			if err != nil {
				t.Fatal(err)
			}
			if got := ctx.InstructionsOmitted(); got != nil {
				t.Errorf("omitted after a checkpoint with no list = %v", got)
			}
			var buf bytes.Buffer
			if err := Write(&buf, read); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(buf.String(), strings.TrimPrefix(member, ",")) {
				t.Errorf("the checkpoint's member was not kept as written:\n%s", buf.String())
			}
		})
	}
}

// TestDispatchIdempotencyKey: the key is typed from a string and kept
// as written from anything else, which a file from before 0.8 may hold.
func TestDispatchIdempotencyKey(t *testing.T) {
	for name, tt := range map[string]struct {
		member  string
		typed   string
		unknown bool
	}{
		"a string": {`"idempotency_key":"k-1"`, "k-1", false},
		"a number": {`"idempotency_key":7`, "", true},
		"empty":    {`"idempotency_key":""`, "", true},
	} {
		t.Run(name, func(t *testing.T) {
			line := []byte(`{"type":"dispatch","call_id":"c","target":"t",` + tt.member + `}`)
			var d DispatchEntry
			if err := json.Unmarshal(line, &d); err != nil {
				t.Fatal(err)
			}
			if d.IdempotencyKey != tt.typed {
				t.Errorf("IdempotencyKey = %q, want %q", d.IdempotencyKey, tt.typed)
			}
			if _, ok := d.Unknown["idempotency_key"]; ok != tt.unknown {
				t.Errorf("kept as unknown = %v, want %v", ok, tt.unknown)
			}
			out, err := json.Marshal(&d)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(out, []byte(tt.member)) {
				t.Errorf("written as %s, want %s", out, tt.member)
			}
		})
	}
	both := NewDispatch("c", "t").WithIdempotencyKey("k")
	both.Unknown = map[string]json.RawMessage{"idempotency_key": json.RawMessage(`"j"`)}
	if _, err := json.Marshal(both); err == nil {
		t.Error("a key both typed and unknown was written")
	}
}

// TestCallDispatches: every hand-off is kept, the first is Dispatch,
// the key is the first's, and a hold after a dispatch holds the call.
func TestCallDispatches(t *testing.T) {
	s := New(Header{Records: []string{TypeDispatch, TypeDecision}})
	fc := &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "deploy", Arguments: "{}"}}
	target, err := s.Append(fc)
	if err != nil {
		t.Fatal(err)
	}
	first := NewDispatch("c", target).WithIdempotencyKey("k-1")
	for _, e := range []Entry{first, NewDecision("c", target, VerdictHold, ByPolicy).WithReason("may have run")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := s.PendingCalls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if c := calls[0]; !c.Held() || c.State(s.Header()) != CallHeld {
		t.Errorf("a hold after a dispatch: held %v, state %v", c.Held(), c.State(s.Header()))
	}
	for _, e := range []Entry{NewDecision("c", target, VerdictProceed, ByHuman), NewDispatch("c", target).WithIdempotencyKey("k-1")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	calls, err = s.PendingCalls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if len(c.Dispatches) != 2 || c.Dispatch != first || c.IdempotencyKey() != "k-1" || c.DispatchedArgs() != "{}" || c.Held() || c.State(s.Header()) != CallInFlight {
		t.Errorf("dispatches %d, first %v, key %q, held %v, state %v", len(c.Dispatches), c.Dispatch == first, c.IdempotencyKey(), c.Held(), c.State(s.Header()))
	}
}

// TestTriggerMembers: a trigger keeps the members the format does not
// define, refuses to set one it does, and compares by its canonical
// form.
func TestTriggerMembers(t *testing.T) {
	var tr Trigger
	if err := json.Unmarshal([]byte(`{"kind":"schedule","ref":"nightly","attempt":2,"due":"2026-09-29T03:00:00Z"}`), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.Kind != "schedule" || string(tr.Unknown["attempt"]) != "2" {
		t.Errorf("trigger = %+v", tr)
	}
	out, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	var back Trigger
	if err := json.Unmarshal(out, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Equal(&tr) {
		t.Errorf("round trip %s differs", out)
	}
	if err := tr.SetMember("kind", "x"); err == nil {
		t.Error("SetMember set a defined member")
	}
	clone := tr.Clone()
	if err := clone.SetMember("attempt", 3); err != nil {
		t.Fatal(err)
	}
	if string(tr.Unknown["attempt"]) != "2" || clone.Equal(&tr) {
		t.Error("the clone shares its members with the original")
	}
	tr.Unknown["kind"] = json.RawMessage(`"x"`)
	if _, err := json.Marshal(tr); err == nil {
		t.Error("a trigger with kind both typed and unknown was written")
	}
	var nilTrigger *Trigger
	if !nilTrigger.Equal(nil) || nilTrigger.Equal(&back) {
		t.Error("Equal on nil")
	}
}

// hashedLines builds a file from bodies, each entry the child of the
// one before, with ids computed as a writer would, under a header of
// format.
func hashedLines(t *testing.T, format string, bodies ...string) (string, []string) {
	t.Helper()
	lines := []string{`{"type":"session","format":"` + format + `","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24","records":["run","dispatch","decision"]}`}
	var ids []string
	parent := "null"
	for _, body := range bodies {
		line := `{` + body + `,"parent":` + parent + `,"ts":"2026-09-17T16:00:01Z"}`
		id, _, err := EntryHashes([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, `{"id":"`+id+`",`+line[1:])
		ids = append(ids, id)
		parent = `"` + id + `"`
	}
	return strings.Join(lines, "\n") + "\n", ids
}

// TestOmittedNullIsAbsent: a null instructions_omitted leaves the list
// in force as it was.
func TestOmittedNullIsAbsent(t *testing.T) {
	in, _ := hashedLines(t, Format,
		`"type":"config","model":"m","instructions_omitted":[{"id":"a"}]`,
		`"type":"config","model":"m2","instructions_omitted":null`)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.InstructionsOmitted(); len(got) != 1 || got[0].ID != "a" {
		t.Errorf("omitted after a null = %v", got)
	}
}

// TestCheckpointOmittedFoldedKey: a 0.7 checkpoint whose omitted part
// spells its id "ID" reads, verifies and is written back as it was;
// the list is not taken, so none is in force.
func TestCheckpointOmittedFoldedKey(t *testing.T) {
	_, ids := hashedLines(t, "agentsession/0.7",
		`"type":"config","model":"m"`,
		`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}`)
	in, _ := hashedLines(t, "agentsession/0.7",
		`"type":"config","model":"m"`,
		`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}`,
		`"type":"compaction","first_kept":"`+ids[1]+`","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"s"}]},"config":{"model":"m","instructions_omitted":[{"ID":"a"}]}`)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := ctx.InstructionsOmitted(); got != nil {
		t.Errorf("omitted = %v, want none: no conforming reader sees an id", got)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"instructions_omitted":[{"ID":"a"}]`) {
		t.Errorf("the member was not written as read:\n%s", buf.String())
	}
	if _, err := Read(&buf); err != nil {
		t.Errorf("read back: %v", err)
	}
}

// TestKeyPairsWithItsDispatch: a call run again under a new key, for
// new arguments, reads the last dispatch's key beside the arguments
// that dispatch ran with, through two crashes; a later decision's
// arguments are what a new hand-off would run with.
func TestKeyPairsWithItsDispatch(t *testing.T) {
	s := New(Header{Records: AllRecords})
	fc := &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "deploy", Arguments: `{"v":"A"}`}}
	target, err := s.Append(fc)
	if err != nil {
		t.Fatal(err)
	}
	check := func(key, dispatched, args string) {
		t.Helper()
		calls, err := s.Calls(s.Leaf())
		if err != nil {
			t.Fatal(err)
		}
		c := calls[0]
		if c.IdempotencyKey() != key || c.DispatchedArgs() != dispatched || c.Args() != args {
			t.Errorf("key %q args dispatched %s now %s, want %q %s %s", c.IdempotencyKey(), c.DispatchedArgs(), c.Args(), key, dispatched, args)
		}
	}
	for _, e := range []Entry{NewDispatch("c", target).WithIdempotencyKey("k1")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	check("k1", `{"v":"A"}`, `{"v":"A"}`)
	// First crash; the rerun rewrites the arguments under a new key.
	for _, e := range []Entry{NewDecision("c", target, VerdictProceed, ByHuman).WithArgs(json.RawMessage(`{"v":"B"}`)), NewDispatch("c", target).WithIdempotencyKey("k2")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	check("k2", `{"v":"B"}`, `{"v":"B"}`)
	// Second crash; a decision gives new arguments and nothing is
	// handed over yet.
	if _, err := s.Append(NewDecision("c", target, VerdictHold, ByPolicy).WithArgs(json.RawMessage(`{"v":"C"}`))); err != nil {
		t.Fatal(err)
	}
	check("k2", `{"v":"B"}`, `{"v":"C"}`)
}

// TestHeldAfterDispatch: a hold after a dispatch holds a call that may
// have run: its state is held, it is not in flight, it keeps its
// dispatches, and a run left with it ends input_required.
func TestHeldAfterDispatch(t *testing.T) {
	s := New(Header{Records: AllRecords})
	for _, e := range []Entry{NewRunStart("r", SourceInput, "")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "deploy", Arguments: "{}"}, ResponseID: "resp_1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		&ResponseEntry{ResponseID: "resp_1", Status: openresponses.ResponseStatusCompleted},
		NewDispatch("c", target),
		NewDecision("c", target, VerdictHold, ByPolicy).WithReason("may have run; ask"),
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := s.PendingCalls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	c := calls[0]
	if c.State(s.Header()) != CallHeld || c.InFlight() || len(c.Dispatches) != 1 {
		t.Errorf("state %v, in flight %v, dispatches %d", c.State(s.Header()), c.InFlight(), len(c.Dispatches))
	}
	run, err := s.OpenRun(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if got := ComputeReason(s.Path(s.Leaf()), run.Segment); got != ReasonInputRequired {
		t.Errorf("run reads %s, want input_required", got)
	}
}

// TestAnsweredOwesItsOutput: an answer whose output the record stopped
// before is answered, not in flight; no dispatch may follow it, and in
// a file that promises dispatch records an answer needs a dispatch
// before it.
func TestAnsweredOwesItsOutput(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDecision("c", target, VerdictAnswer, ByPolicy)); !errors.Is(err, ErrAnswerNotDispatched) {
		t.Errorf("answer before any dispatch: Append = %v, want ErrAnswerNotDispatched", err)
	}
	for _, e := range []Entry{NewDispatch("c", target), NewDecision("c", target, VerdictAnswer, ByPolicy).WithReason("replay unknown")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	calls, err := s.PendingCalls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if c := calls[0]; c.State(s.Header()) != CallAnswered || c.InFlight() {
		t.Errorf("state %v, in flight %v", c.State(s.Header()), c.InFlight())
	}
	if _, err := s.Append(NewDispatch("c", target)); !errors.Is(err, ErrCallAnswered) {
		t.Errorf("dispatch after the answer: Append = %v, want ErrCallAnswered", err)
	}
	// A file another writer produced, answering a call it never
	// dispatched and writing no output.
	_, ids := hashedLines(t, Format,
		`"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"notify","arguments":"{}"}`)
	in, _ := hashedLines(t, Format,
		`"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"notify","arguments":"{}"}`,
		`"type":"decision","call_id":"c","target":"`+ids[0]+`","verdict":"answer"`)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, ErrAnswerNotDispatched) {
		t.Errorf("VerifyRecords = %v, want ErrAnswerNotDispatched", err)
	}
}

// TestNothingFollowsAnAnswer: after an answer the call's output is all
// that may follow, so a decision or a dispatch is refused, and an
// answer after the output is refused too; VerifyRecords reports each
// in a file another writer produced.
func TestNothingFollowsAnAnswer(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{NewDispatch("c", target), NewDecision("c", target, VerdictAnswer, ByPolicy).WithReason("replay unknown")} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	for name, e := range map[string]Entry{
		"hold":     NewDecision("c", target, VerdictHold, ByPolicy),
		"proceed":  NewDecision("c", target, VerdictProceed, ByHuman),
		"reject":   NewDecision("c", target, VerdictReject, ByHuman).WithReason("no"),
		"answer":   NewDecision("c", target, VerdictAnswer, ByHuman),
		"dispatch": NewDispatch("c", target),
	} {
		if _, err := s.Append(e); !errors.Is(err, ErrCallAnswered) {
			t.Errorf("%s after an answer: Append = %v, want ErrCallAnswered", name, err)
		}
	}
	if _, err := s.Append(NewItemEntry(&openresponses.FunctionCallOutput{CallID: "c", Output: openresponses.FunctionCallOutputData{Text: "outcome unknown"}})); err != nil {
		t.Fatalf("the output after the answer: %v", err)
	}

	done := New(Header{Records: AllRecords})
	target, err = done.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{NewDispatch("c", target), NewItemEntry(&openresponses.FunctionCallOutput{CallID: "c", Output: openresponses.FunctionCallOutputData{Text: "sent"}})} {
		if _, err := done.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := done.Append(NewDecision("c", target, VerdictAnswer, ByPolicy)); !errors.Is(err, ErrCallCompleted) {
		t.Errorf("answer after the output: Append = %v, want ErrCallCompleted", err)
	}

	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"notify","arguments":"{}"}`
	_, ids := hashedLines(t, Format, call)
	dispatch := `"type":"dispatch","call_id":"c","target":"` + ids[0] + `"`
	answer := `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"answer"`
	output := `"type":"item","item":{"type":"function_call_output","call_id":"c","output":"x"}`
	for name, tt := range map[string]struct {
		bodies []string
		want   error
	}{
		"a hold after an answer":    {[]string{call, dispatch, answer, `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"hold"`}, ErrCallAnswered},
		"an answer after an output": {[]string{call, dispatch, output, answer}, ErrCallCompleted},
	} {
		t.Run(name, func(t *testing.T) {
			in, _ := hashedLines(t, Format, tt.bodies...)
			read, err := Read(strings.NewReader(in))
			if err != nil {
				t.Fatal(err)
			}
			if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, tt.want) {
				t.Errorf("VerifyRecords = %v, want %v", err, tt.want)
			}
		})
	}
}
