package otel

import (
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// hashedFile builds a session file from entry bodies, each the child
// of the one before, with its id computed.
func hashedFile(t *testing.T, bodies ...func(ids []string) string) *agentsession.Session {
	t.Helper()
	lines := []string{`{"type":"session","format":"` + agentsession.Format + `","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24","records":["run","dispatch","decision"]}`}
	parent := "null"
	var ids []string
	for _, body := range bodies {
		l := `{` + body(ids) + `,"parent":` + parent + `,"ts":"2026-09-17T16:00:01Z"}`
		id, _, err := agentsession.EntryHashes([]byte(l))
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, `{"id":"`+id+`",`+l[1:])
		ids = append(ids, id)
		parent = `"` + id + `"`
	}
	s, err := agentsession.Read(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func body(b string) func([]string) string { return func([]string) string { return b } }

// callStates exports the session and returns each tool span's call
// state by span name.
func callStates(t *testing.T, s *agentsession.Session) map[string]string {
	t.Helper()
	sr, tr := recorder()
	if _, err := Export(context.Background(), tr, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, sp := range sr.Ended() {
		for _, a := range sp.Attributes() {
			if string(a.Key) == AttrCallState {
				out[sp.Name()] = a.Value.AsString()
			}
		}
	}
	return out
}

// TestRepeatedCallIDLatest: in a file that repeats a call ID, the
// exporter reads what follows as naming the latest call with it, as
// the library does: a dispatch names its call by target, an output by
// call ID.
func TestRepeatedCallIDLatest(t *testing.T) {
	s := hashedFile(t,
		body(`"type":"item","item":{"type":"function_call","id":"fc1","call_id":"x","name":"alpha","arguments":"{}"}`),
		body(`"type":"item","item":{"type":"function_call","id":"fc2","call_id":"x","name":"beta","arguments":"{}"}`),
		func(ids []string) string { return `"type":"dispatch","call_id":"x","target":"` + ids[1] + `"` },
		body(`"type":"item","item":{"type":"function_call_output","call_id":"x","output":"ok"}`),
	)
	calls, err := s.Calls(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{}
	for _, c := range calls {
		want["execute_tool "+c.Call.Name] = c.State(s.Header()).String()
	}
	got := callStates(t, s)
	for name, state := range want {
		if got[name] != state {
			t.Errorf("%s: span state %q, library reads %q", name, got[name], state)
		}
	}
	if want["execute_tool beta"] != "completed" || want["execute_tool alpha"] != "never_started" {
		t.Errorf("library states %v", want)
	}
}

// TestRejectedPending: a reject whose refusal output the record
// stopped before reads as rejected.
func TestRejectedPending(t *testing.T) {
	s := hashedFile(t,
		body(`"type":"item","item":{"type":"function_call","id":"fc1","call_id":"x","name":"alpha","arguments":"{}"}`),
		func(ids []string) string {
			return `"type":"decision","call_id":"x","target":"` + ids[0] + `","verdict":"reject","reason":"no"`
		},
	)
	if got := callStates(t, s)["execute_tool alpha"]; got != agentsession.CallRejected.String() {
		t.Errorf("span state %q, want %q", got, agentsession.CallRejected)
	}
}

// TestDispatchAfterOutputEndsEverySpan: a dispatch after a call's
// output changes nothing the path reads for it, and every span the
// exporter starts ends.
func TestDispatchAfterOutputEndsEverySpan(t *testing.T) {
	dispatch := func(ids []string) string { return `"type":"dispatch","call_id":"x","target":"` + ids[0] + `"` }
	s := hashedFile(t,
		body(`"type":"item","item":{"type":"function_call","id":"fc1","call_id":"x","name":"alpha","arguments":"{}"}`),
		dispatch,
		body(`"type":"item","item":{"type":"function_call_output","call_id":"x","output":"ok"}`),
		dispatch,
	)
	sr, tr := recorder()
	if _, err := Export(context.Background(), tr, s, s.Leaf()); err != nil {
		t.Fatal(err)
	}
	if started, ended := len(sr.Started()), len(sr.Ended()); started != ended {
		t.Errorf("%d spans started, %d ended", started, ended)
	}
}

// TestForkPrefixCallUnknown: the exporter reads a call the fork's
// prefix made, with no dispatch, as unknown, as the library does, and
// the fork's own as never started.
func TestForkPrefixCallUnknown(t *testing.T) {
	origin := agentsession.New(agentsession.Header{})
	call, err := origin.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fa", CallID: "a", Name: "alpha", Arguments: "{}"}})
	if err != nil {
		t.Fatal(err)
	}
	fork, err := agentsession.Fork(origin, call, agentsession.Header{Records: []string{agentsession.TypeDispatch}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fb", CallID: "b", Name: "beta", Arguments: "{}"}}); err != nil {
		t.Fatal(err)
	}
	got := callStates(t, fork)
	if got["execute_tool alpha"] != agentsession.CallUnknown.String() || got["execute_tool beta"] != agentsession.CallNeverStarted.String() {
		t.Errorf("span states %v", got)
	}
}
