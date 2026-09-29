package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// omittedList returns n omitted parts, m0 to m(n-1).
func omittedList(n int) []OmittedPart {
	out := make([]OmittedPart, 0, n)
	for i := range n {
		out = append(out, OmittedPart{ID: fmt.Sprintf("m%d", i), Reason: "budget", Size: 20 + i, Source: "agentmemory"})
	}
	return out
}

// omittedIDs renders a list as its ids, a keep as +n.
func omittedIDs(list []OmittedPart) string {
	out := make([]string, 0, len(list))
	for _, o := range list {
		if o.ID == "" {
			out = append(out, fmt.Sprintf("+%d", o.Keep))
			continue
		}
		out = append(out, o.ID)
	}
	return strings.Join(out, " ")
}

// TestOmittedDeltaKeepsRuns is the letta-memory finding (#115): under
// 0.8 a list that changed was written whole, so one part moving across
// a budget rewrote every part. OmittedDelta names each run of parts in
// force, unchanged and in order, by a keep, and applying its result to
// the list in force gives the new list back.
func TestOmittedDeltaKeepsRuns(t *testing.T) {
	prev := omittedList(10)
	pushed := OmittedPart{ID: "n", Reason: "budget", Size: 20, Source: "agentmemory"}
	changed := slices.Clone(prev)
	changed[4].Size = 99
	// m3 named after m4 moves the cursor back to m4, which the list
	// names, so m5 is written rather than kept.
	swapped := append(append(slices.Clone(prev[:3]), prev[4], prev[3]), prev[5:]...)
	for name, tt := range map[string]struct {
		next []OmittedPart
		want string
	}{
		"one pushed to the head": {append([]OmittedPart{pushed}, prev...), "n +10"},
		"one pushed to the tail": {append(slices.Clone(prev), pushed), "+10 n"},
		"one forgotten":          {append(slices.Clone(prev[:4]), prev[5:]...), "+4 m5 +4"},
		"the first forgotten":    {slices.Clone(prev[1:]), "m1 +8"},
		"the last forgotten":     {slices.Clone(prev[:9]), "+9"},
		"one changed":            {changed, "+4 m4 +5"},
		"two swapped":            {swapped, "+3 m4 m3 m5 +4"},
		"all new":                {[]OmittedPart{pushed}, "n"},
	} {
		t.Run(name, func(t *testing.T) {
			s := Settings{InstructionsOmitted: prev}
			delta := s.OmittedDelta(tt.next)
			if got := omittedIDs(delta); got != tt.want {
				t.Errorf("delta = %s, want %s", got, tt.want)
			}
			if err := validateConfig(&ConfigEntry{InstructionsOmitted: delta}); err != nil {
				t.Errorf("the delta is not one a writer may write: %v", err)
			}
			if got := s.Apply(&ConfigEntry{InstructionsOmitted: delta}).InstructionsOmitted; !slices.Equal(got, tt.next) {
				t.Errorf("applied = %s, want %s", omittedIDs(got), omittedIDs(tt.next))
			}
		})
	}
	s := Settings{InstructionsOmitted: prev}
	if d := s.OmittedDelta(slices.Clone(prev)); d != nil {
		t.Errorf("unchanged: delta = %v, want nil", d)
	}
	if d := s.OmittedDelta(nil); d == nil || len(d) != 0 {
		t.Errorf("cleared: delta = %#v, want []", d)
	}
	if d := (Settings{}).OmittedDelta(nil); d != nil {
		t.Errorf("nothing in force, nothing omitted: delta = %#v, want nil", d)
	}
	// A list in force that names an id twice cannot be counted over, so
	// the list is written whole.
	dup := append(slices.Clone(prev), prev[0])
	next := append(slices.Clone(dup), pushed)
	if d := (Settings{InstructionsOmitted: dup}).OmittedDelta(next); !slices.Equal(d, next) {
		t.Errorf("over a list naming an id twice: delta = %s, want the list whole", omittedIDs(d))
	}
}

// TestOmittedDeltaSize is the measurement the issue filed: one part
// pushed out ahead of 474 already omitted costs about 100 bytes, not
// the 37 KB list.
func TestOmittedDeltaSize(t *testing.T) {
	prev := omittedList(474)
	for i := range prev {
		prev[i].ID = fmt.Sprintf("memory/user/n-%04d", i+126)
	}
	next := append([]OmittedPart{{ID: "memory/user/n-0125", Reason: "budget", Size: 20, Source: "agentmemory"}}, prev...)
	whole, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	delta, err := json.Marshal((Settings{InstructionsOmitted: prev}).OmittedDelta(next))
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"id":"memory/user/n-0125","reason":"budget","size":20,"source":"agentmemory"},{"keep":474}]`; string(delta) != want {
		t.Errorf("delta = %s, want %s", delta, want)
	}
	t.Logf("the list whole is %d bytes, the delta %d", len(whole), len(delta))
}

// TestOmittedKeepRoundTrip: a keep in instructions_omitted is written as
// the element alone, read back as a keep, and resolved against the list
// in force, so the context after it holds the whole list.
func TestOmittedKeepRoundTrip(t *testing.T) {
	prev := omittedList(5)
	s := New(Header{})
	if _, err := s.Append(&ConfigEntry{Model: "m", InstructionsOmitted: prev}); err != nil {
		t.Fatal(err)
	}
	pushed := OmittedPart{ID: "n", Reason: "budget", Size: 7}
	delta := []OmittedPart{pushed, {Keep: 5}}
	if _, err := s.Append(&ConfigEntry{InstructionsOmitted: delta}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"instructions_omitted":[{"id":"n","reason":"budget","size":7},{"keep":5}]`) {
		t.Errorf("the keep was not written as the element alone:\n%s", buf.String())
	}
	written := buf.String()
	read, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	cfg := read.Entries()[len(read.Entries())-1].(*ConfigEntry)
	if !slices.Equal(cfg.InstructionsOmitted, delta) {
		t.Errorf("read back %+v, want %+v", cfg.InstructionsOmitted, delta)
	}
	ctx, err := read.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := ctx.InstructionsOmitted(), append([]OmittedPart{pushed}, prev...); !slices.Equal(got, want) {
		t.Errorf("in force %s, want %s", omittedIDs(got), omittedIDs(want))
	}
	var again bytes.Buffer
	if err := Write(&again, read); err != nil {
		t.Fatal(err)
	}
	if again.String() != written {
		t.Errorf("the rewrite differs:\n%s\nwant:\n%s", again.String(), written)
	}
}

// TestOmittedInForceAcrossDeltas: the list in force resolves each keep
// against the list before its entry, across deltas, a delta without
// the member, a replace that writes the list whole, and a compaction
// whose checkpoint carries the list whole, and a keep after the fold
// counts over the checkpoint's list.
func TestOmittedInForceAcrossDeltas(t *testing.T) {
	a := omittedList(6)
	x := OmittedPart{ID: "x", Reason: "budget", Size: 3}
	y := OmittedPart{ID: "y", Reason: "stale", Size: 4}
	s := New(Header{})
	step := func(c *ConfigEntry, want []OmittedPart) {
		t.Helper()
		if _, err := s.Append(c); err != nil {
			t.Fatal(err)
		}
		appendText(t, s, "turn")
		ctx, err := s.Context()
		if err != nil {
			t.Fatal(err)
		}
		if got := ctx.InstructionsOmitted(); !slices.Equal(got, want) {
			t.Errorf("in force %s, want %s", omittedIDs(got), omittedIDs(want))
		}
	}
	step(&ConfigEntry{Model: "m", InstructionsOmitted: a}, a)
	// x in front of the six.
	withX := append([]OmittedPart{x}, a...)
	step(&ConfigEntry{InstructionsOmitted: []OmittedPart{x, {Keep: 6}}}, withX)
	// A delta without the member leaves it.
	step(&ConfigEntry{Model: "m2"}, withX)
	// m2 forgotten: x and m0 m1 kept, m3 named moves the cursor past
	// m2, and the rest kept.
	noM2 := append(slices.Clone(withX[:3]), withX[4:]...)
	step(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3}, a[3], {Keep: 2}}}, noM2)
	// A replace writes the list whole.
	whole := append(slices.Clone(noM2), y)
	step(&ConfigEntry{Model: "m3", Replace: true, InstructionsOmitted: whole}, whole)
	comp, err := s.CompactKeeping(1, openresponses.UserText("summary"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(comp.Config.InstructionsOmitted, whole) {
		t.Errorf("the checkpoint carries %s, want %s whole", omittedIDs(comp.Config.InstructionsOmitted), omittedIDs(whole))
	}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	// After the fold, x is dropped and the rest kept from the
	// checkpoint's list.
	step(&ConfigEntry{InstructionsOmitted: []OmittedPart{a[0], {Keep: 5}}}, whole[1:])

	// The same path read back from its lines resolves the same way.
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
	if got := ctx.InstructionsOmitted(); !slices.Equal(got, whole[1:]) {
		t.Errorf("read back, in force %s, want %s", omittedIDs(got), omittedIDs(whole[1:]))
	}
}

// TestOmittedKeepUnsatisfiable: a keep that runs past the list in
// force, one that takes a part the list names elsewhere, one in a
// replace, which counts over nothing, and an element naming nothing
// stay in the list in force as written, as an unresolved instruction
// part does; a later keep that takes one takes it as it is.
func TestOmittedKeepUnsatisfiable(t *testing.T) {
	prev := omittedList(3)
	for name, tt := range map[string]struct {
		delta   *ConfigEntry
		want    string
		pending int
	}{
		"past the end":           {&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 4}}}, "+4", 1},
		"taking a named part":    {&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 2}, prev[1]}}, "+2 m1", 1},
		"in a replace":           {&ConfigEntry{Replace: true, InstructionsOmitted: []OmittedPart{{Keep: 3}}}, "+3", 1},
		"naming nothing":         {&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 3}, {Reason: "budget"}}}, "m0 m1 m2 +0", 1},
		"a keep that is fine":    {&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 1}, prev[2]}}, "m0 m2", 0},
		"a new part moves none":  {&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "n"}, {Keep: 3}}}, "n m0 m1 m2", 0},
		"a keep beside an id":    {&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "m0", Keep: 9}, {Keep: 2}}}, "m0 m1 m2", 0},
		"a part named out of it": {&ConfigEntry{InstructionsOmitted: []OmittedPart{prev[2], {Keep: 1}}}, "m2 +1", 1},
	} {
		t.Run(name, func(t *testing.T) {
			got := (Settings{InstructionsOmitted: prev}).Apply(tt.delta).InstructionsOmitted
			if omittedIDs(got) != tt.want {
				t.Errorf("in force %s, want %s", omittedIDs(got), tt.want)
			}
			n := 0
			for _, o := range got {
				if o.Unresolved() {
					n++
				}
				if o.ID != "" && o.Keep != 0 {
					t.Errorf("%s kept a keep beside its id", o.ID)
				}
			}
			if n != tt.pending {
				t.Errorf("%d unresolved, want %d", n, tt.pending)
			}
		})
	}
	// A later keep that takes an unresolved element takes it as it is.
	s := Settings{InstructionsOmitted: prev}.Apply(&ConfigEntry{InstructionsOmitted: []OmittedPart{{Keep: 4}}})
	s = s.Apply(&ConfigEntry{InstructionsOmitted: []OmittedPart{{ID: "n"}, {Keep: 1}}})
	if got := omittedIDs(s.InstructionsOmitted); got != "n +4" {
		t.Errorf("after a keep of the unresolved element, in force %s, want n +4", got)
	}
}

// TestOmittedKeepRefused: Append holds a writer to the form a reader
// resolves: a keep is the element's only member and positive, a list
// with a keep names each id once, and a replace, which discards the
// list a keep counts over, writes the list whole.
func TestOmittedKeepRefused(t *testing.T) {
	for name, c := range map[string]*ConfigEntry{
		"a keep with an id":      {InstructionsOmitted: []OmittedPart{{ID: "a", Keep: 1}}},
		"a keep with a reason":   {InstructionsOmitted: []OmittedPart{{Keep: 1, Reason: "budget"}}},
		"a negative keep":        {InstructionsOmitted: []OmittedPart{{Keep: -1}}},
		"a keep in a replace":    {Model: "m", Replace: true, InstructionsOmitted: []OmittedPart{{ID: "a"}, {Keep: 1}}},
		"an id twice with keeps": {InstructionsOmitted: []OmittedPart{{ID: "a"}, {Keep: 1}, {ID: "a"}}},
		"an element naming none": {InstructionsOmitted: []OmittedPart{{Reason: "budget"}}},
	} {
		t.Run(name, func(t *testing.T) {
			s := New(Header{})
			if _, err := s.Append(&ConfigEntry{Model: "m", InstructionsOmitted: omittedList(2)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(c); err == nil {
				t.Error("Append accepted it")
			}
		})
	}
}

// TestNothingFollowsAReject: what follows a reject is the call's
// refusal output and nothing else, and a reject is for a call that did
// not run, so none follows a dispatch. Append refuses each, and
// VerifyRecords reports each in a file another writer produced.
func TestNothingFollowsAReject(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDecision("c", target, VerdictReject, ByHuman).WithReason("no")); err != nil {
		t.Fatal(err)
	}
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if got := calls[0].State(s.Header()); got != CallRejected {
		t.Errorf("State = %s, want %s", got, CallRejected)
	}
	for name, e := range map[string]Entry{
		"hold":     NewDecision("c", target, VerdictHold, ByPolicy),
		"proceed":  NewDecision("c", target, VerdictProceed, ByHuman),
		"reject":   NewDecision("c", target, VerdictReject, ByHuman).WithReason("no"),
		"answer":   NewDecision("c", target, VerdictAnswer, ByHuman),
		"dispatch": NewDispatch("c", target),
	} {
		if _, err := s.Append(e); !errors.Is(err, ErrCallRejected) {
			t.Errorf("%s after a reject: Append = %v, want ErrCallRejected", name, err)
		}
	}
	if _, err := s.Append(NewItemEntry(&openresponses.FunctionCallOutput{CallID: "c", Output: openresponses.FunctionCallOutputData{Text: "no"}})); err != nil {
		t.Fatalf("the output after the reject: %v", err)
	}

	ran := New(Header{Records: AllRecords})
	target, err = ran.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ran.Append(NewDispatch("c", target)); err != nil {
		t.Fatal(err)
	}
	if _, err := ran.Append(NewDecision("c", target, VerdictReject, ByPolicy).WithReason("no")); !errors.Is(err, ErrRejectDispatched) {
		t.Errorf("reject after a dispatch: Append = %v, want ErrRejectDispatched", err)
	}
	if _, err := ran.Append(NewDecision("c", target, VerdictAnswer, ByPolicy).WithReason("replay unknown")); err != nil {
		t.Errorf("answer after a dispatch: %v", err)
	}

	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"notify","arguments":"{}"}`
	_, ids := hashedLines(t, Format, call)
	dispatch := `"type":"dispatch","call_id":"c","target":"` + ids[0] + `"`
	reject := `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"reject","reason":"no"`
	hold := `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"hold"`
	for name, tt := range map[string]struct {
		bodies []string
		want   error
	}{
		"a hold after a reject":     {[]string{call, reject, hold}, ErrCallRejected},
		"a reject after a dispatch": {[]string{call, dispatch, reject}, ErrRejectDispatched},
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

// TestRejectAfterOutput: a reject, like an answer, is for a call with
// no output. One after the output would also hide a missing dispatch,
// since a rejected call needs none.
func TestRejectAfterOutput(t *testing.T) {
	s := New(Header{Records: AllRecords})
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "c", Name: "notify", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(openresponses.NewFunctionCallOutput("c", "sent"))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDecision("c", target, VerdictReject, ByPolicy).WithReason("no")); !errors.Is(err, ErrCallCompleted) {
		t.Errorf("reject after the output: Append = %v, want ErrCallCompleted", err)
	}
	call := `"type":"item","item":{"type":"function_call","id":"fc","call_id":"c","name":"notify","arguments":"{}"}`
	_, ids := hashedLines(t, Format, call)
	output := `"type":"item","item":{"type":"function_call_output","call_id":"c","output":"x"}`
	reject := `"type":"decision","call_id":"c","target":"` + ids[0] + `","verdict":"reject","reason":"no"`
	in, _ := hashedLines(t, Format, call, output, reject)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, ErrCallCompleted) {
		t.Errorf("VerifyRecords = %v, want ErrCallCompleted", err)
	}
}

// TestTargetNamesTheCall: a decision or dispatch names its call by
// target, the function call's entry, and its call ID must agree.
func TestTargetNamesTheCall(t *testing.T) {
	s := New(Header{Records: AllRecords})
	a, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fa", CallID: "a", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "t", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	for name, e := range map[string]Entry{
		"a decision naming another call's entry": NewDecision("b", a, VerdictHold, ByPolicy),
		"a dispatch naming another call's entry": NewDispatch("b", a),
		"a decision for a call not on the path":  NewDecision("z", "sha256:0", VerdictHold, ByPolicy),
		"a dispatch naming no entry":             NewDispatch("a", "sha256:0"),
	} {
		if _, err := s.Append(e); !errors.Is(err, ErrBadTarget) {
			t.Errorf("%s: Append = %v, want ErrBadTarget", name, err)
		}
	}
	if _, err := s.Append(NewDispatch("a", a)); err != nil {
		t.Errorf("a dispatch naming its call: %v", err)
	}

	call := `"type":"item","item":{"type":"function_call","id":"fa","call_id":"a","name":"t","arguments":"{}"}`
	other := `"type":"item","item":{"type":"function_call","id":"fb","call_id":"b","name":"t","arguments":"{}"}`
	_, ids := hashedLines(t, Format, call, other)
	hold := `"type":"decision","call_id":"b","target":"` + ids[0] + `","verdict":"hold"`
	in, _ := hashedLines(t, Format, call, other, hold)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if err := read.VerifyRecords(read.Leaf()); !errors.Is(err, ErrBadTarget) {
		t.Errorf("VerifyRecords = %v, want ErrBadTarget", err)
	}
	// The target binds: the hold is call a's, though it says b.
	calls, err := read.Calls(read.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if !calls[0].Held() || calls[1].Held() {
		t.Errorf("held: a %v, b %v; want the target's call held", calls[0].Held(), calls[1].Held())
	}
}

// TestCallIDPerSession: a call ID names one call in a session, on any
// branch, since a subsession's ID is derived from it; and a function
// call needs one.
func TestCallIDPerSession(t *testing.T) {
	s := New(Header{})
	root, err := s.Append(NewItemEntry(openresponses.UserText("hi")))
	if err != nil {
		t.Fatal(err)
	}
	fc := func() *ItemEntry {
		return &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "a", Name: "t", Arguments: "{}"}}
	}
	if _, err := s.Append(fc()); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(root); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(fc()); !errors.Is(err, ErrCallIDRepeated) {
		t.Errorf("the call ID on another branch: Append = %v, want ErrCallIDRepeated", err)
	}
	if _, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", Name: "t", Arguments: "{}"}}); err == nil {
		t.Error("a function call with no call_id was accepted")
	}
}

// TestHeldReappendAfterCallRules: appending again an entry the session
// holds is a no-op, whatever followed it on the path.
func TestHeldReappendAfterCallRules(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	target, err := s.Append(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "a", Name: "t", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	hold := NewDecision("a", target, VerdictHold, ByPolicy)
	if _, err := s.Append(hold); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewDecision("a", target, VerdictReject, ByHuman).WithReason("no")); err != nil {
		t.Fatal(err)
	}
	again := *hold
	if r, err := s.Commit(&again); err != nil || r.Outcome != Held {
		t.Errorf("the hold again: %v, %v; want held", r.Outcome, err)
	}
	fc := &ItemEntry{Item: &openresponses.FunctionCall{ID: "fc", CallID: "a", Name: "t", Arguments: "{}"}}
	fc.Parent, fc.Timestamp = s.byID[target].Base().Parent, s.byID[target].Base().Timestamp
	if r, err := s.Commit(fc); err != nil || r.Outcome != Held {
		t.Errorf("the call again: %v, %v; want held", r.Outcome, err)
	}
}

// TestRunCallsByBinding: in a file that repeats a call ID, what a run
// takes up is the call its entries bind to, so the superseded call is
// not the run's.
func TestRunCallsByBinding(t *testing.T) {
	first := `"type":"item","item":{"type":"function_call","id":"f1","call_id":"x","name":"t","arguments":"{}"}`
	second := `"type":"item","item":{"type":"function_call","id":"f2","call_id":"x","name":"t","arguments":"{}"}`
	start := `"type":"run","run_id":"r","phase":"start","source":"resume"`
	output := `"type":"item","item":{"type":"function_call_output","call_id":"x","output":"ok"}`
	in, _ := hashedLines(t, Format, first, second, start, output)
	read, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	runs, err := read.Runs(read.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	calls := runs[0].Calls()
	if len(calls) != 1 || calls[0].Call.ID != "f2" {
		t.Errorf("run calls = %v, want the second call alone", calls)
	}
	if p := runs[0].Pending(); len(p) != 0 {
		t.Errorf("Pending = %v, want none", p)
	}
}
