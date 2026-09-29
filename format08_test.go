package agentsession

import (
	"bytes"
	"encoding/json"
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
	if len(c.Dispatches) != 2 || c.Dispatch != first || c.IdempotencyKey() != "k-1" || c.Held() || c.State(s.Header()) != CallInFlight {
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
