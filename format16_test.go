package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// fixtures016 are the 0.16 conformance fixtures, each built natively by
// its function, as for fixtures015.
var fixtures016 = map[string]func(t *testing.T) *Session{
	"outputs-groups": outputsGroupsFixture,
}

// badFixtures016 are the 0.16 negative fixtures: each a valid file with
// one line a writer refuses, written by hand.
var badFixtures016 = map[string]func(t *testing.T) []byte{
	"bad-outputs-overlap": badOutputsOverlapLines,
}

// writeFixtures016 writes the 0.16 fixtures, positive and negative.
func writeFixtures016(t *testing.T) {
	t.Helper()
	for name, build := range fixtures016 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for name, build := range badFixtures016 {
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), build(t), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// groupsPath appends the conversation the outputs-groups fixtures share:
// three tool calls and their outputs, each answered, and returns the
// output entries' IDs.
func groupsPath(t *testing.T, must func(Entry) string) []string {
	t.Helper()
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Read what you are given.")})
	must(NewItemEntry(openresponses.UserText("Check the three pages.")))
	var outs []string
	for i, page := range []string{"docs", "inbox", "forum"} {
		call := "call_" + page
		resp := "resp_" + page
		must(&ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_" + page, CallID: call, Name: "fetch_page", Arguments: `{"page":"` + page + `"}`}, ResponseID: resp})
		must(&ResponseEntry{ResponseID: resp, Model: "gpt-5", Status: openresponses.ResponseStatusCompleted})
		outs = append(outs, must(NewItemEntry(openresponses.NewFunctionCallOutput(call, strings.Repeat(page+" ", i+3)))))
	}
	return outs
}

// outputsGroupsFixture builds the session outputs-groups.jsonl holds: a
// host clears the first output with the default group's text, as a 0.15
// file could, and a harness quarantines the second and then the third
// with a group of its own text, in two deltas; the request after them
// sends each output with its group's text, and its response's
// request_hash verifies.
func outputsGroupsFixture(t *testing.T) *Session {
	t.Helper()
	s, must, _ := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000040"}, "2026-11-02T09:00:00Z")
	outs := groupsPath(t, must)
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "[cleared]", Items: []string{outs[0]}}}})
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Groups: []OmitOutputGroup{{Text: "[quarantined]", Items: []string{outs[1]}}}}}})
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Groups: []OmitOutputGroup{{Text: "[quarantined]", Items: []string{outs[2]}}}}}})
	sent := fixtureHash(t, s)
	must(&ItemEntry{Item: &openresponses.Message{ID: "msg_1", Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "Two pages were withheld.", Annotations: []openresponses.Annotation{}}}}, ResponseID: "resp_answer"})
	must(&ResponseEntry{ResponseID: "resp_answer", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent})
	return s
}

// badOutputsOverlapLines builds bad-outputs-overlap.jsonl: the default
// group clears the first output, and a later outputs puts it in a group
// of another text too, which a writer refuses.
func badOutputsOverlapLines(t *testing.T) []byte {
	t.Helper()
	s, must, _ := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-000000000041"}, "2026-11-02T10:00:00Z")
	outs := groupsPath(t, must)
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "[cleared]", Items: []string{outs[0]}}}})
	bad := &ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Groups: []OmitOutputGroup{{Text: "[quarantined]", Items: []string{outs[0], outs[1]}}}}}}
	if _, err := s.Append(bad); !errors.Is(err, ErrOmitOverlap) {
		t.Fatalf("the writer appended what the fixture breaks: %v", err)
	}
	b := bad.Base()
	b.ID, b.Parent, b.Timestamp = "x", "", time.Date(2026, 11, 2, 10, 30, 0, 0, time.UTC)
	line, err := MarshalEntry(bad)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	buf.WriteString(reparent(t, string(line), s.Leaf()) + "\n")
	// The model is asked after it, under the request a reader rebuilds,
	// which the bad line changes nothing in.
	read, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	sent := fixtureHash(t, read)
	at := time.Date(2026, 11, 2, 10, 31, 0, 0, time.UTC)
	answer := &ItemEntry{Item: &openresponses.Message{ID: "msg_1", Status: "completed", Role: openresponses.RoleAssistant, Content: openresponses.Contents{&openresponses.OutputText{Text: "One page was cleared.", Annotations: []openresponses.Annotation{}}}}, ResponseID: "resp_answer"}
	answer.Timestamp = at
	resp := &ResponseEntry{ResponseID: "resp_answer", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted, RequestHash: sent}
	resp.Timestamp = at.Add(time.Second)
	for _, e := range []Entry{answer, resp} {
		if _, err := read.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	buf.Reset()
	if err := Write(&buf, read); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// sentOutputs is the output text a context sends for each output entry
// of outs, "" for one it sends as recorded.
func sentOutputs(t *testing.T, s *Session, outs []string) []string {
	t.Helper()
	c, err := s.ContextAt(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	replaced := map[string]bool{}
	for _, r := range c.ReplacedItems {
		if r.Reason == OmitOutputsReason {
			replaced[r.Entry.ID] = true
		}
	}
	got := make([]string, len(outs))
	for i, id := range outs {
		if !replaced[id] {
			continue
		}
		for j, e := range c.ItemEntries {
			if e.Base().ID == id {
				got[i] = c.Items[j].(*openresponses.FunctionCallOutput).Output.Text
			}
		}
	}
	return got
}

// outputIDs are the function call output entries of s, in order.
func outputIDs(s *Session) []string {
	var out []string
	for _, e := range s.Entries() {
		if ie, ok := e.(*ItemEntry); ok {
			if _, ok := ie.Item.(*openresponses.FunctionCallOutput); ok {
				out = append(out, ie.ID)
			}
		}
	}
	return out
}

// TestOutputsGroupsFixture reads the 0.16 fixture: each output is sent
// with its group's text, the default group's among them, the response's
// request_hash verifies, and the groups line is written without a
// default text.
func TestOutputsGroupsFixture(t *testing.T) {
	raw := checkBuilt(t, "outputs-groups")
	if !bytes.Contains(raw, []byte(`"omit":{"outputs":{"groups":[{"items":["`)) {
		t.Errorf("the groups line is not written in the 0.16 form:\n%s", raw)
	}
	s := loadFixture(t, "outputs-groups")
	outs := outputIDs(s)
	if got := sentOutputs(t, s, outs); !slices.Equal(got, []string{"[cleared]", "[quarantined]", "[quarantined]"}) {
		t.Errorf("sent %q", got)
	}
	if err := s.Verify(s.Leaf()); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestBadOutputsOverlapFixture reads the 0.16 negative fixture: the
// line verifies, and its outputs, which would put an entry in two
// groups, changes nothing, so the first output is still sent with the
// default group's text and the second as recorded.
func TestBadOutputsOverlapFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions", "bad-outputs-overlap.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, badOutputsOverlapLines(t)) {
		t.Error("bad-outputs-overlap.jsonl is not what its builder writes; run go test -update")
	}
	s := loadFixture(t, "bad-outputs-overlap")
	outs := outputIDs(s)
	if got := sentOutputs(t, s, outs); !slices.Equal(got, []string{"[cleared]", "", ""}) {
		t.Errorf("sent %q, want the overlapping outputs to change nothing", got)
	}
	if err := s.Verify(s.Leaf()); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// readOutputsLine reads the shared path with the default group clearing
// the first output and then a hand-written config line whose omit is
// omit, and returns the text each output is sent with.
func readOutputsLine(t *testing.T, omit string) []string {
	t.Helper()
	s, must, _ := fixtureSession(t, Header{ID: "r"}, "2026-11-02T12:00:00Z")
	outs := groupsPath(t, must)
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "[cleared]", Items: []string{outs[0]}}}})
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	omit = strings.NewReplacer("X0", outs[0], "X1", outs[1], "X2", outs[2]).Replace(omit)
	line := `{"type":"config","ts":"2026-11-02T12:30:00Z","omit":` + omit + `}`
	buf.WriteString(reparent(t, line, s.Leaf()) + "\n")
	read, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return sentOutputs(t, read, outs)
}

// TestOutputsLinesAReaderTakes: a text present beside groups writes the
// default group, the empty string included; an outputs listing one
// entry in two of its own groups, or a group element with a member the
// format does not define, changes nothing; and a group re-listing what
// it lists, or the default group re-listing its own, is no conflict.
func TestOutputsLinesAReaderTakes(t *testing.T) {
	for _, tt := range []struct {
		name, omit string
		want       []string
	}{
		{"empty default text beside groups", `{"outputs":{"text":"","groups":[{"text":"[q]","items":["X1"]}]}}`, []string{"", "[q]", ""}},
		{"one entry in two of its groups", `{"outputs":{"groups":[{"text":"a","items":["X1"]},{"text":"b","items":["X1"]}]}}`, []string{"[cleared]", "", ""}},
		{"a group element with an extra member", `{"outputs":{"groups":[{"text":"[q]","items":["X1"],"x":1}]}}`, []string{"[cleared]", "", ""}},
		{"the default group re-listing its own", `{"outputs":{"text":"[cleared2]","items":["X0","X2"]}}`, []string{"[cleared2]", "", "[cleared2]"}},
		{"a group re-listing its own", `{"outputs":{"groups":[{"text":"[q]","items":["X1","X1"]}]}}`, []string{"[cleared]", "[q]", ""}},
	} {
		if got := readOutputsLine(t, tt.omit); !slices.Equal(got, tt.want) {
			t.Errorf("%s: sent %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestOutputsGroupsReplace: a replace starts the groups afresh, so it
// may put an entry the default group held into a group of its own.
func TestOutputsGroupsReplace(t *testing.T) {
	s, must, _ := fixtureSession(t, Header{ID: "rp"}, "2026-11-02T13:00:00Z")
	outs := groupsPath(t, must)
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "[cleared]", Items: []string{outs[0]}}}})
	if _, err := s.Append(&ConfigEntry{Replace: true, Model: "gpt-5", Omit: &Omit{Outputs: &OmitOutputs{Groups: []OmitOutputGroup{{Text: "[q]", Items: []string{outs[0]}}}}}}); err != nil {
		t.Fatalf("a replace moving an entry to a group of its own: %v", err)
	}
	if got := sentOutputs(t, s, outs); !slices.Equal(got, []string{"[q]", "", ""}) {
		t.Errorf("sent %q", got)
	}
}

// TestOutputsDeltaToEmptyText: OmitDelta writes a change of the default
// group's text to the empty string beside groups, as the text present.
func TestOutputsDeltaToEmptyText(t *testing.T) {
	have := Settings{Omit: Omit{Outputs: &OmitOutputs{Text: "[cleared]", Items: []string{"x"}}}}
	d, ok := have.OmitDelta(Omit{Outputs: &OmitOutputs{Text: "", Items: []string{"x"}, Groups: []OmitOutputGroup{{Text: "[q]", Items: []string{"y"}}}}})
	if !ok || d == nil || d.Outputs == nil {
		t.Fatalf("OmitDelta = %+v, %v", d, ok)
	}
	data, err := json.Marshal(d.Outputs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"text":""`)) {
		t.Errorf("delta %s writes no empty text", data)
	}
	if got := have.Omit.merged(*d).Outputs; got.Text != "" || !got.hasDefault() {
		t.Errorf("in force after the delta: %+v", *got)
	}
}

// TestOutputsGroupsWriter: a writer refuses an outputs that lists an
// entry in two of its groups, holds two groups of one text, or lists one
// a group in force of another text holds; a group of the text in force
// adds to it, and the default group's text still changes for its own.
func TestOutputsGroupsWriter(t *testing.T) {
	s, must, _ := fixtureSession(t, Header{ID: "w"}, "2026-11-02T11:00:00Z")
	outs := groupsPath(t, must)
	groups := func(gs ...OmitOutputGroup) *ConfigEntry {
		return &ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Groups: gs}}}
	}
	for name, e := range map[string]*ConfigEntry{
		"one entry in two groups":    groups(OmitOutputGroup{Text: "a", Items: []string{outs[0]}}, OmitOutputGroup{Text: "b", Items: []string{outs[0]}}),
		"two groups of one text":     groups(OmitOutputGroup{Text: "a", Items: []string{outs[0]}}, OmitOutputGroup{Text: "a", Items: []string{outs[1]}}),
		"in the default and a group": {Omit: &Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{outs[0]}, Groups: []OmitOutputGroup{{Text: "a", Items: []string{outs[0]}}}}}},
	} {
		if _, err := s.Append(e); !errors.Is(err, ErrOmitOverlap) {
			t.Errorf("%s: Append = %v, want ErrOmitOverlap", name, err)
		}
	}
	must(groups(OmitOutputGroup{Text: "a", Items: []string{outs[0]}}))
	if _, err := s.Append(groups(OmitOutputGroup{Text: "b", Items: []string{outs[0]}})); !errors.Is(err, ErrOmitOverlap) {
		t.Errorf("an entry a group in force of another text lists: Append = %v, want ErrOmitOverlap", err)
	}
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{outs[0]}}}}); !errors.Is(err, ErrOmitOverlap) {
		t.Errorf("an entry a group lists, given to the default group: Append = %v, want ErrOmitOverlap", err)
	}
	must(groups(OmitOutputGroup{Text: "a", Items: []string{outs[1]}}))
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "d1", Items: []string{outs[2]}}}})
	must(&ConfigEntry{Omit: &Omit{Outputs: &OmitOutputs{Text: "d2"}}})
	if got := sentOutputs(t, s, outs); !slices.Equal(got, []string{"a", "a", "d2"}) {
		t.Errorf("sent %q", got)
	}
	// The settings in force carry the groups through a checkpoint's
	// encoding.
	c, err := s.ContextAt(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(c.Settings)
	if err != nil {
		t.Fatal(err)
	}
	var back Settings
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if o := back.Omit.Outputs; o == nil || o.Text != "d2" || !slices.Equal(o.Items, []string{outs[2]}) || len(o.Groups) != 1 || o.Groups[0].Text != "a" || !slices.Equal(o.Groups[0].Items, outs[:2]) {
		t.Errorf("the outputs in force through encoding: %s", data)
	}
}

// TestOutputsGroupsDelta: OmitDelta writes what a wanted outputs adds to
// the one in force, a group's new items alone, and refuses one that would
// drop a group or move an entry between groups.
func TestOutputsGroupsDelta(t *testing.T) {
	have := Settings{Omit: Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{"x"}, Groups: []OmitOutputGroup{{Text: "a", Items: []string{"y"}}}}}}
	want := Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{"x"}, Groups: []OmitOutputGroup{{Text: "a", Items: []string{"y", "z"}}, {Text: "b", Items: []string{"w"}}}}}
	d, ok := have.OmitDelta(want)
	if !ok || d == nil || d.Outputs == nil {
		t.Fatalf("OmitDelta = %+v, %v", d, ok)
	}
	if d.Outputs.Text != "" || len(d.Outputs.Items) != 0 || len(d.Outputs.Groups) != 2 ||
		d.Outputs.Groups[0].Text != "a" || !slices.Equal(d.Outputs.Groups[0].Items, []string{"z"}) ||
		d.Outputs.Groups[1].Text != "b" || !slices.Equal(d.Outputs.Groups[1].Items, []string{"w"}) {
		t.Errorf("delta = %+v", *d.Outputs)
	}
	if got := have.Omit.merged(*d); !sameOutputsForm(got.Outputs, want.Outputs) {
		t.Errorf("in force after the delta = %+v, want %+v", *got.Outputs, *want.Outputs)
	}
	if _, ok := have.OmitDelta(Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{"x"}}}); ok {
		t.Error("a delta that drops a group was offered")
	}
	if _, ok := have.OmitDelta(Omit{Outputs: &OmitOutputs{Text: "d", Items: []string{"x"}, Groups: []OmitOutputGroup{{Text: "b", Items: []string{"y"}}}}}); ok {
		t.Error("a delta that moves an entry between groups was offered")
	}
}

// sameOutputsForm compares two outputs members by what they send.
func sameOutputsForm(a, b *OmitOutputs) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	if a.Text != b.Text || !slices.Equal(a.Items, b.Items) || len(a.Groups) != len(b.Groups) {
		return false
	}
	for i := range a.Groups {
		if a.Groups[i].Text != b.Groups[i].Text || !slices.Equal(a.Groups[i].Items, b.Groups[i].Items) {
			return false
		}
	}
	return true
}
