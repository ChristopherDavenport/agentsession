package agentsession

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// contextGolden is the golden form of a context: the settings, the item
// list as it would be sent, and the IDs of the selected entries.
type contextGolden struct {
	Settings    Settings            `json:"settings"`
	Items       openresponses.Items `json:"items"`
	Entries     []string            `json:"entries"`
	ItemEntries []string            `json:"item_entries"`
}

// TestContextGolden runs the context algorithm at every leaf of every
// positive fixture and compares against testdata/context. Reviewing
// those files is reviewing the algorithm.
func TestContextGolden(t *testing.T) {
	for _, name := range []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved", "instructions", "queued", "resume", "pinned"} {
		t.Run(name, func(t *testing.T) {
			s := loadFixture(t, name)
			got := map[string]contextGolden{}
			for _, leaf := range s.Leaves() {
				ctx, err := s.ContextAt(leaf)
				if err != nil {
					t.Fatalf("ContextAt(%s): %v", leaf, err)
				}
				g := contextGolden{Settings: ctx.Settings, Items: ctx.Items}
				for _, e := range ctx.Entries {
					g.Entries = append(g.Entries, e.Base().ID)
				}
				if len(ctx.ItemEntries) != len(ctx.Items) {
					t.Fatalf("ContextAt(%s): %d item entries for %d items", leaf, len(ctx.ItemEntries), len(ctx.Items))
				}
				for _, e := range ctx.ItemEntries {
					g.ItemEntries = append(g.ItemEntries, e.Base().ID)
				}
				got[leaf] = g
			}
			checkGolden(t, filepath.Join("testdata", "context", name+".json"), got)
		})
	}
}

func TestContextShape(t *testing.T) {
	s := loadFixture(t, "compaction")
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	// The last config on the path replaced everything: model only.
	if ctx.Settings.Model != "gpt-5-nano" || ctx.Settings.Instructions != "" || ctx.Settings.Extra != nil {
		t.Errorf("settings = %+v", ctx.Settings)
	}
	texts := itemTexts(ctx.Items)
	want := []string{"Summary: the user said first and the assistant said one.", "second", "two", "third", "three", "fourth"}
	if !reflect.DeepEqual(texts, want) {
		t.Errorf("items = %q, want %q", texts, want)
	}
	if ctx.Entries[0].Base().ID != "k0000001" || ctx.Entries[1].Base().ID != "i0000003" {
		t.Errorf("entries start %s %s", ctx.Entries[0].Base().ID, ctx.Entries[1].Base().ID)
	}
	// ItemEntries skips the config and response entries that Entries
	// carries, so it lines up with Items.
	var itemIDs []string
	for _, e := range ctx.ItemEntries {
		itemIDs = append(itemIDs, e.Base().ID)
	}
	wantIDs := []string{"k0000001", "i0000003", "i0000004", "i0000005", "i0000006", "i0000007"}
	if !reflect.DeepEqual(itemIDs, wantIDs) {
		t.Errorf("item entries = %q, want %q", itemIDs, wantIDs)
	}
	for i, e := range ctx.ItemEntries {
		if contextItem(e) != ctx.Items[i] && e != ctx.Entries[0] {
			t.Errorf("item %d did not come from entry %s", i, e.Base().ID)
		}
	}

	// Before the compaction, nothing is summarised and the checkpoint
	// is not consulted: the config replay alone gives the settings.
	ctx, err = s.ContextAt("i0000005")
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Settings.Model != "gpt-5-mini" || ctx.Settings.Instructions != "Be brief." {
		t.Errorf("settings before compaction = %+v", ctx.Settings)
	}
	if _, ok := ctx.Settings.Extra["temperature"]; ok {
		t.Error("null extra should have removed temperature")
	}
	if string(ctx.Settings.Extra["acme:region"]) != `"eu"` {
		t.Errorf("extra = %v", ctx.Settings.Extra)
	}
	if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, []string{"first", "one", "second", "two", "third"}) {
		t.Errorf("items before compaction = %q", got)
	}

	empty, err := s.ContextAt("")
	if err != nil || len(empty.Items) != 0 {
		t.Errorf("empty context = %+v, %v", empty, err)
	}
	if _, err := s.ContextAt("missing"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("missing leaf err = %v", err)
	}
	if _, err := loadFixture(t, "bad-first-kept").Context(); err == nil {
		t.Error("compaction with first_kept off the path was accepted")
	}
}

func TestBranchContext(t *testing.T) {
	s := loadFixture(t, "branch")
	ctx, err := s.ContextAt("n0000001")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Q", "A1", "An earlier attempt asked follow-up A and got A2.", "follow-up B", "B2"}
	if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("items = %q, want %q", got, want)
	}
	ctx, err = s.ContextAt("r0000002")
	if err != nil {
		t.Fatal(err)
	}
	if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, []string{"Q", "A1", "follow-up A", "A2"}) {
		t.Errorf("abandoned branch items = %q", got)
	}
	if got := s.Labels(); !reflect.DeepEqual(got, map[string]string{"r0000001": "fork"}) {
		t.Errorf("labels = %v", got)
	}
	if s.Name() != "Branching demo" {
		t.Errorf("name = %q", s.Name())
	}
	if got := s.Leaves(); !reflect.DeepEqual(got, []string{"r0000002", "n0000001"}) {
		t.Errorf("leaves = %v", got)
	}
	if got := s.Children("r0000001"); !reflect.DeepEqual(got, []string{"i0000003", "b0000001"}) {
		t.Errorf("children = %v", got)
	}
}

// TestVerifyFixtureHashes rebuilds the request for every response entry
// in the fixtures and checks it against the recorded request_hash.
func TestVerifyFixtureHashes(t *testing.T) {
	for _, name := range []string{"basic", "compaction", "branch", "extensions", "runs", "interleaved", "instructions", "queued", "resume", "pinned"} {
		t.Run(name, func(t *testing.T) {
			s := loadFixture(t, name)
			for _, e := range s.Entries() {
				r, ok := e.(*ResponseEntry)
				if !ok || r.RequestHash == "" {
					continue
				}
				if err := s.Verify(r.ID); err != nil {
					ctx, _ := s.RequestContext(r.ID)
					req, _ := ctx.Request()
					got, _ := RequestHash(req)
					t.Errorf("%s: %v (rebuilt %s)", r.ID, err, got)
				}
			}
		})
	}
}

func TestRequestContext(t *testing.T) {
	s := loadFixture(t, "basic")
	ctx, err := s.RequestContext("r0000001")
	if err != nil {
		t.Fatal(err)
	}
	// The reasoning and function call items belong to resp_1 and are
	// not part of the request that produced them.
	if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, []string{"What is 2+2?"}) {
		t.Errorf("request items = %q", got)
	}
	req, err := ctx.Request()
	if err != nil {
		t.Fatal(err)
	}
	if req.Store == nil || *req.Store || req.PreviousResponseID != "" {
		t.Errorf("canonical request store=%v prev=%q", req.Store, req.PreviousResponseID)
	}
	if req.Extra["temperature"] != json.Number("0.2") {
		t.Errorf("extra temperature = %#v", req.Extra["temperature"])
	}
	if len(req.Tools) != 1 || req.Reasoning.Effort != openresponses.ReasoningEffortLow || req.Instructions != "Be brief." {
		t.Errorf("request = %+v", req)
	}

	ctx, err = s.RequestContext("r0000002")
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Items) != 4 { // user, reasoning, call, output
		t.Errorf("second request has %d items", len(ctx.Items))
	}
	if _, err := s.RequestContext("i0000001"); err == nil {
		t.Error("RequestContext accepted an item entry")
	}
	if _, err := s.RequestContext("nope"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("err = %v", err)
	}

	// A wrong recorded hash is reported.
	bad := &ResponseEntry{ResponseID: "resp_9", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:0000"}
	if _, err := s.Append(bad); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(bad.ID); !errors.Is(err, ErrHashMismatch) {
		t.Errorf("Verify = %v, want ErrHashMismatch", err)
	}
	// A response that recorded no hash is neither verified nor
	// mismatched, and Verify says so rather than returning nil: a
	// caller gating on err == nil is told that nothing was checked.
	none := &ResponseEntry{ResponseID: "resp_10", Status: openresponses.ResponseStatusCompleted}
	if _, err := s.Append(none); err != nil {
		t.Fatal(err)
	}
	err = s.Verify(none.ID)
	if !errors.Is(err, ErrNoHash) {
		t.Errorf("Verify without hash = %v, want ErrNoHash", err)
	}
	if errors.Is(err, ErrHashMismatch) {
		t.Error("ErrNoHash must not match ErrHashMismatch: nothing to check is not a wrong record")
	}
	if !strings.Contains(err.Error(), none.ID) {
		t.Errorf("Verify without hash = %v, want the entry named", err)
	}
}

// TestRequestContextInterleaved is the composed product's shape: a
// layer writes a custom entry between two output items of one
// response, which is where an output guard's verdict lands. The
// entries of that response are still its output, wherever the other
// layer wrote, so every request still rebuilds.
func TestRequestContextInterleaved(t *testing.T) {
	s := loadFixture(t, "interleaved")
	tests := []struct {
		name, response string
		items          []string
	}{
		{"first request keeps the user item alone", "r0000001", []string{"List the files."}},
		{"second request keeps the first turn", "r0000002", []string{
			"List the files.", "reasoning:A shell call will do.", "call:bash", "output:a.txt\nb.txt"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := s.RequestContext(tt.response)
			if err != nil {
				t.Fatal(err)
			}
			if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, tt.items) {
				t.Fatalf("request items = %q, want %q", got, tt.items)
			}
			for _, e := range ctx.Entries {
				if item, ok := e.(*ItemEntry); ok && item.ResponseID == responseIDOf(t, s, tt.response) {
					t.Errorf("entry %s is output of the response it is a request for", e.Base().ID)
				}
			}
			if err := s.Verify(tt.response); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
	// The custom entries stay on the path: they carry no item, so they
	// change neither the request nor its hash.
	ctx, err := s.RequestContext("r0000002")
	if err != nil {
		t.Fatal(err)
	}
	custom := 0
	for _, e := range ctx.Entries {
		if _, ok := e.(*CustomEntry); ok {
			custom++
		}
	}
	if custom != 2 {
		t.Errorf("request context holds %d custom entries, want both the ones on the path", custom)
	}
}

// TestPinnedContext is the shape a harness that holds one item out of
// a fold writes: the compaction carries the item it kept, and the
// reader places it immediately after the summary. Without the member
// the rebuilt request is short of the item the model was sent, which
// is a hash mismatch the writer avoids only by recording no hash.
func TestPinnedContext(t *testing.T) {
	s := loadFixture(t, "pinned")
	comp, ok := s.Entry("k0000001")
	if !ok {
		t.Fatal("no compaction entry")
	}
	k := comp.(*CompactionEntry)
	if len(k.Pinned) != 1 {
		t.Fatalf("compaction carries %d pinned items, want 1", len(k.Pinned))
	}
	pin := "House rule: never use Box::leak."

	// The pinned item sits between the summary and the kept window,
	// which is where the request that was sent had it.
	ctx, err := s.RequestContext("r0000003")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Summary: the user said first and the assistant said one.", pin, "second", "two", "third"}
	if got := itemTexts(ctx.Items); !reflect.DeepEqual(got, want) {
		t.Errorf("request items = %q, want %q", got, want)
	}
	if err := s.Verify("r0000003"); err != nil {
		t.Errorf("Verify: %v", err)
	}

	// Every item has the entry that contributed it, and the compaction
	// contributes its summary and its pinned items alike.
	if len(ctx.ItemEntries) != len(ctx.Items) {
		t.Fatalf("%d item entries for %d items", len(ctx.ItemEntries), len(ctx.Items))
	}
	if ctx.ItemEntries[0] != comp || ctx.ItemEntries[1] != comp {
		t.Error("the summary and the pinned item should both name the compaction as their entry")
	}

	// Context() carries it too, which is what makes a pin survive a
	// resume: Continue and Rebase seed from Context, and a pin the
	// transcript no longer holds cannot be matched again.
	ctx, err = s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if got := itemTexts(ctx.Items); !slices.Contains(got, pin) {
		t.Errorf("Context() at the leaf = %q, want the pinned item", got)
	}

	// The pinned item is a copy of context already recorded, never a
	// new input: it is reachable as an entry on the path before
	// first_kept, so a reader that ignores the member loses context
	// but never invents it.
	e, ok := s.Entry("i0000001")
	if !ok {
		t.Fatal("no entry i0000001")
	}
	if got := itemTexts(openresponses.Items{e.(*ItemEntry).Item}); got[0] != pin {
		t.Errorf("entry i0000001 = %q, want the pinned item", got)
	}
}

// TestPinnedOmitted keeps the member optional: a compaction with no
// pinned items writes no `pinned` and rebuilds as it did before.
func TestPinnedOmitted(t *testing.T) {
	s := loadFixture(t, "compaction")
	e, _ := s.Entry("k0000001")
	if got := e.(*CompactionEntry).Pinned; got != nil {
		t.Errorf("compaction fixture gained %d pinned items", len(got))
	}
	data, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "pinned") {
		t.Errorf("an empty Pinned was written: %s", data)
	}
}

// TestOutputEntries pins the contract a second implementation of the
// rule has to match. The rule is written twice in the workspace —
// here and in agenteval's replay, which serves the items rather than
// excluding them — so what this asserts is what keeps the two from
// drifting: order, membership, and what is left out.
func TestOutputEntries(t *testing.T) {
	s := loadFixture(t, "interleaved")
	path := s.Path("r0000002")
	resp := path[len(path)-1].(*ResponseEntry)

	// Path order, not the backward order the walk runs in. Serving
	// these reversed changes every later request of a replayed run,
	// and the divergence names two hashes and no field.
	got := OutputEntries(path, resp)
	var ids []string
	for _, e := range got {
		ids = append(ids, e.ID)
	}
	if want := []string{"i0000005", "i0000006"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("output entries = %v, want %v", ids, want)
	}

	// The custom entry between the two output items is skipped and not
	// returned: it is on the path for its own reasons and stays there.
	for _, e := range got {
		if e.ID == "u0000002" {
			t.Error("a skipped non-item entry was returned as output")
		}
	}

	// Passing the path with the response entry still on the end gives
	// the same answer, since an entry that is not an item is skipped.
	if trimmed := OutputEntries(path[:len(path)-1], resp); !reflect.DeepEqual(trimmed, got) {
		t.Error("trimming the response entry off the path changed the answer")
	}

	// The entries are the session's own, so a caller that serves their
	// items knows it has to clone.
	if e, _ := s.Entry("i0000006"); got[len(got)-1] != e {
		t.Error("OutputEntries returned a copy, not the session's own entry")
	}

	// A response with no ResponseID has no output items, and neither
	// does one whose items are all somebody else's.
	if out := OutputEntries(path, &ResponseEntry{}); out != nil {
		t.Errorf("a response with no ResponseID has %d output entries", len(out))
	}
	if out := OutputEntries(path, &ResponseEntry{ResponseID: "resp_absent"}); out != nil {
		t.Errorf("an unmatched response has %d output entries", len(out))
	}
	if out := OutputEntries(nil, resp); out != nil {
		t.Errorf("an empty path has %d output entries", len(out))
	}

	// The walk stops at the first item entry belonging to something
	// else rather than running to the root: the first response's items
	// are not the second's.
	first := s.Path("r0000001")
	out := OutputEntries(first, first[len(first)-1].(*ResponseEntry))
	ids = nil
	for _, e := range out {
		ids = append(ids, e.ID)
	}
	if want := []string{"i0000002", "i0000003"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("first response output = %v, want %v", ids, want)
	}
}

// TestOutputEntriesStopsRatherThanSkips is the case that separates the
// rule from the looser one that selects every item entry naming the
// response, wherever it sits. They differ only when an item entry that
// is not part of the response lands between two that are, which is an
// input item written mid-response — something the writing discipline
// says SHOULD NOT happen. The walk stops there, so such a file fails
// loudly on its hash instead of quietly rebuilding a request that
// includes an input the model never saw.
func TestOutputEntriesStopsRatherThanSkips(t *testing.T) {
	item := func(id, responseID, text string) *ItemEntry {
		return &ItemEntry{
			EntryBase:  EntryBase{ID: id},
			Item:       openresponses.UserMessage(&openresponses.InputText{Text: text}),
			ResponseID: responseID,
		}
	}
	resp := &ResponseEntry{EntryBase: EntryBase{ID: "r1"}, ResponseID: "resp_1"}
	path := []Entry{
		item("i1", "", "the request"),
		item("i2", "resp_1", "first output"),
		// An input item written while the response was in flight.
		item("i3", "", "steered in mid-response"),
		item("i4", "resp_1", "second output"),
		resp,
	}
	var ids []string
	for _, e := range OutputEntries(path, resp) {
		ids = append(ids, e.ID)
	}
	// i2 names the response but is behind the stop, so it is not
	// output. Returning it would be the looser rule.
	if want := []string{"i4"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("output entries = %v, want %v: the walk must stop at i3, not skip it", ids, want)
	}
}

func responseIDOf(t *testing.T, s *Session, entryID string) string {
	t.Helper()
	e, ok := s.Entry(entryID)
	if !ok {
		t.Fatalf("no entry %s", entryID)
	}
	return e.(*ResponseEntry).ResponseID
}

func TestSettingsApply(t *testing.T) {
	lookup := openresponses.NewFunctionTool("lookup", "v1", nil)
	lookup2 := openresponses.NewFunctionTool("lookup", "v2", nil)
	write := openresponses.NewFunctionTool("write", "", nil)
	ext := &openresponses.UnknownTool{Type: "acme:search", Raw: json.RawMessage(`{"type":"acme:search","name":"search"}`)}
	base := Settings{
		Model:        "a",
		Instructions: "i",
		Reasoning:    openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortHigh},
		Tools:        openresponses.Tools{lookup, write},
		Extra:        map[string]json.RawMessage{"temperature": json.RawMessage("1"), "top_p": json.RawMessage("0.5")},
	}
	tests := []struct {
		name  string
		delta ConfigEntry
		want  Settings
	}{
		{
			name:  "empty delta changes nothing",
			delta: ConfigEntry{},
			want:  base,
		},
		{
			name:  "model only",
			delta: ConfigEntry{Model: "b"},
			want: Settings{Model: "b", Instructions: "i", Reasoning: base.Reasoning, Tools: base.Tools,
				Extra: base.Extra},
		},
		{
			name:  "clear instructions and reasoning",
			delta: ConfigEntry{Instructions: ptr(""), Reasoning: &openresponses.ReasoningConfig{}},
			want:  Settings{Model: "a", Tools: base.Tools, Extra: base.Extra},
		},
		{
			name:  "replace tool by name and remove another",
			delta: ConfigEntry{ToolsAdded: openresponses.Tools{lookup2, ext}, ToolsRemoved: []string{"write"}},
			want: Settings{Model: "a", Instructions: "i", Reasoning: base.Reasoning,
				Tools: openresponses.Tools{lookup2, ext}, Extra: base.Extra},
		},
		{
			name:  "remove every tool",
			delta: ConfigEntry{ToolsRemoved: []string{"lookup", "write"}},
			want:  Settings{Model: "a", Instructions: "i", Reasoning: base.Reasoning, Extra: base.Extra},
		},
		{
			name:  "extra merge and null delete",
			delta: ConfigEntry{Extra: map[string]json.RawMessage{"temperature": json.RawMessage("null"), "seed": json.RawMessage("7")}},
			want: Settings{Model: "a", Instructions: "i", Reasoning: base.Reasoning, Tools: base.Tools,
				Extra: map[string]json.RawMessage{"top_p": json.RawMessage("0.5"), "seed": json.RawMessage("7")}},
		},
		{
			name:  "replace discards everything first",
			delta: ConfigEntry{Replace: true, Model: "c", ToolsAdded: openresponses.Tools{write}},
			want:  Settings{Model: "c", Tools: openresponses.Tools{write}},
		},
		{
			name:  "text format",
			delta: ConfigEntry{Text: &openresponses.TextConfig{Verbosity: openresponses.VerbosityLow}},
			want: Settings{Model: "a", Instructions: "i", Reasoning: base.Reasoning, Tools: base.Tools, Extra: base.Extra,
				Text: openresponses.TextConfig{Verbosity: openresponses.VerbosityLow}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := base.Apply(&tt.delta)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Apply =\n%+v\nwant\n%+v", got, tt.want)
			}
			// The receiver is untouched.
			if len(base.Tools) != 2 || len(base.Extra) != 2 || base.Model != "a" {
				t.Errorf("Apply modified its receiver: %+v", base)
			}
		})
	}
	if ToolName(nil) != "" || ToolName(ext) != "search" || ToolName(lookup) != "lookup" {
		t.Error("ToolName")
	}
}

func TestSettingsRequestBadExtra(t *testing.T) {
	s := Settings{Extra: map[string]json.RawMessage{"x": json.RawMessage("{")}}
	if _, err := s.Request(nil); err == nil {
		t.Error("Request accepted malformed extra")
	}
}

// itemTexts returns the text of each message item, for assertions.
func itemTexts(items openresponses.Items) []string {
	var out []string
	for _, it := range items {
		switch v := it.(type) {
		case *openresponses.Message:
			out = append(out, v.Text())
		case *openresponses.FunctionCall:
			out = append(out, "call:"+v.Name)
		case *openresponses.FunctionCallOutput:
			out = append(out, "output:"+v.Output.String())
		case *openresponses.ReasoningItem:
			out = append(out, "reasoning:"+v.Summary.Text())
		default:
			out = append(out, it.ItemType())
		}
	}
	return out
}

func TestExtraHelpers(t *testing.T) {
	c := &ConfigEntry{}
	if err := c.SetExtra("temperature", 0.2); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExtra("acme:region", "eu"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetExtra("bad", func() {}); err == nil {
		t.Error("SetExtra accepted an unmarshalable value")
	}
	if got := string(c.Extra["temperature"]); got != "0.2" {
		t.Errorf("temperature raw = %s", got)
	}
	st := Settings{}.Apply(c)

	var temp float64
	if ok, err := st.ExtraValue("temperature", &temp); !ok || err != nil || temp != 0.2 {
		t.Errorf("ExtraValue temperature = %v, %v, %v", temp, ok, err)
	}
	var region string
	if ok, err := st.ExtraValue("acme:region", &region); !ok || err != nil || region != "eu" {
		t.Errorf("ExtraValue region = %q, %v, %v", region, ok, err)
	}
	if ok, err := st.ExtraValue("missing", &region); ok || err != nil || region != "eu" {
		t.Errorf("ExtraValue missing = %v, %v, region %q", ok, err, region)
	}
	var wrong int
	if ok, err := st.ExtraValue("acme:region", &wrong); !ok || err == nil {
		t.Errorf("ExtraValue into the wrong type = %v, %v", ok, err)
	}

	// ClearExtra writes the null that deletes the key on replay, and
	// survives a round trip through JSON.
	d := &ConfigEntry{}
	d.ClearExtra("temperature")
	line, err := MarshalEntry(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), `"extra":{"temperature":null}`) {
		t.Errorf("cleared delta = %s", line)
	}
	after := st.Apply(d)
	if _, ok := after.Extra["temperature"]; ok {
		t.Error("ClearExtra did not remove the key on replay")
	}
	if ok, _ := after.ExtraValue("acme:region", &region); !ok {
		t.Error("ClearExtra removed a different key")
	}
}
