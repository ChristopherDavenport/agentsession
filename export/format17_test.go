package export

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// TestInstructionsPartStep: the instruction-layers fixture exports the
// message the harness composed from an instruction file as a user step
// carrying instructions_part under extra.agentsession, and the person's
// input as one without it (RFC 0001, Projections/ATIF, 0.17).
func TestInstructionsPartStep(t *testing.T) {
	s := loadFixture(t, "instruction-layers")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) < 2 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	composed, input := doc.Steps[0], doc.Steps[1]
	as, _ := composed.Extra[ExtraAgentSession].(map[string]any)
	if composed.Source != atif.SourceUser || !sameValue(t, as["instructions_part"], `{"id":"/repo/CLAUDE.md","source":"agentsmd"}`) {
		t.Errorf("the composed step is %+v", composed)
	}
	if as, _ := input.Extra[ExtraAgentSession].(map[string]any); input.Source != atif.SourceUser || as["instructions_part"] != nil {
		t.Errorf("the input step is %+v", input)
	}
}

// TestMembersInTheDocument is the document a converter reads back (RFC
// 0001, Projections/ATIF, 0.17): an entry's meaning members travel under
// extra.agentsession, as core members a converter restores; a member the
// format does not define travels as written in the extra of the step,
// observation result or record the entry maps to; and a response's
// history member, here legacy_id, travels in its body as written, for a
// converter to leave out of a new entry or to restore on one it rebuilds
// to the document's own id.
func TestMembersInTheDocument(t *testing.T) {
	ts := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	s := agentsession.New(agentsession.Header{ID: "s", CreatedAt: ts})
	probe := func(e agentsession.Entry, v string) agentsession.Entry {
		e.Base().Unknown = map[string]json.RawMessage{"x:probe": json.RawMessage(`"` + v + `"`)}
		return e
	}
	mustAppend(t, s, &agentsession.ConfigEntry{Model: "gpt-5"})
	mustAppend(t, s, &agentsession.ItemEntry{Item: openresponses.UserText("<memory/>"), InstructionsPart: &agentsession.LayerRef{Source: agentsession.LayerAgentMemory}})
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: openresponses.UserText("read it"), Trust: &agentsession.Trust{Reads: []string{agentsession.TrustUntrusted}}}, "input"))
	call, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "read", Arguments: `{}`}, ResponseID: "resp_1"})
	if err != nil {
		t.Fatal(err)
	}
	resp := probe(&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted}, "response")
	resp.Base().LegacyID = "r0000001"
	mustAppend(t, s, resp)
	mustAppend(t, s, probe(agentsession.NewDispatch("call_1", call), "dispatch"))
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "denied"), Failed: true}, "output"))
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	if len(doc.Steps) != 3 {
		t.Fatalf("steps = %d", len(doc.Steps))
	}
	composed, input, agent := doc.Steps[0], doc.Steps[1], doc.Steps[2]
	if as, _ := composed.Extra[ExtraAgentSession].(map[string]any); !sameValue(t, as["instructions_part"], `{"source":"agentmemory"}`) {
		t.Errorf("the composed step's extra is %v", composed.Extra)
	}
	if as, _ := input.Extra[ExtraAgentSession].(map[string]any); !sameValue(t, as["trust"], `{"reads":["untrusted"]}`) || !sameValue(t, input.Extra["x:probe"], `"input"`) {
		t.Errorf("the input step's extra is %v", input.Extra)
	}
	if !sameValue(t, agent.Extra["x:probe"], `"response"`) {
		t.Errorf("the agent step's extra is %v", agent.Extra)
	}
	or, _ := agent.Extra[ExtraOpenResponses].(map[string]any)
	var body map[string]json.RawMessage
	if raw, ok := or["response"].(json.RawMessage); !ok || json.Unmarshal(raw, &body) != nil || string(body["legacy_id"]) != `"r0000001"` || string(body["x:probe"]) != `"response"` {
		t.Errorf("the response body is %v", or["response"])
	}
	if agent.Observation == nil || len(agent.Observation.Results) != 1 {
		t.Fatalf("observation = %+v", agent.Observation)
	}
	res := agent.Observation.Results[0]
	if as, _ := res.Extra[ExtraAgentSession].(map[string]any); as["failed"] != true || !sameValue(t, res.Extra["x:probe"], `"output"`) {
		t.Errorf("the output's extra is %v", res.Extra)
	}
	calls, _ := agent.Extra[ExtraCalls].(map[string]any)
	rec, _ := calls["call_1"].(map[string]any)
	dispatch, _ := rec["dispatch"].(map[string]any)
	if !sameValue(t, dispatch["x:probe"], `"dispatch"`) {
		t.Errorf("the call's record is %v", rec)
	}
}

// sameValue reports whether v encodes to the JSON value want.
func sameValue(t *testing.T, v any, want string) bool {
	t.Helper()
	got, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var a, b any
	if json.Unmarshal(got, &a) != nil || json.Unmarshal([]byte(want), &b) != nil {
		return false
	}
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	return string(ga) == string(gb)
}

// TestCostCountsAChildOnce: a child two links count at different leaves
// adds the union of its two paths to total_cost_usd, the prefix they
// share once, as RFC 0001's link rule counts delegated work, though the
// document embeds a trajectory for each leaf.
func TestCostCountsAChildOnce(t *testing.T) {
	add := func(s *agentsession.Session, e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	usage := func(n int) *openresponses.Usage { return &openresponses.Usage{InputTokens: n, TotalTokens: n} }
	child := agentsession.New(agentsession.Header{ID: "child", ParentSession: "parent"})
	add(child, &agentsession.ConfigEntry{Model: "m"})
	add(child, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("start"), ResponseID: "r1"})
	fork := add(child, &agentsession.ResponseEntry{ResponseID: "r1", Usage: usage(10)})
	add(child, agentsession.NewItemEntry(openresponses.UserText("one way")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("one"), ResponseID: "r2"})
	one := add(child, &agentsession.ResponseEntry{ResponseID: "r2", Usage: usage(20)})
	if err := child.Branch(fork); err != nil {
		t.Fatal(err)
	}
	add(child, agentsession.NewItemEntry(openresponses.UserText("the other")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("other"), ResponseID: "r3"})
	other := add(child, &agentsession.ResponseEntry{ResponseID: "r3", Usage: usage(30)})

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "m"})
	add(parent, agentsession.NewItemEntry(openresponses.UserText("delegate twice")))
	for i, leaf := range []string{one, other} {
		callID := fmt.Sprintf("call_%d", i)
		add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "spawn", Arguments: `{}`}, ResponseID: "p" + callID})
		add(parent, &agentsession.ResponseEntry{ResponseID: "p" + callID, Usage: usage(1)})
		add(parent, agentsession.NewSubsessionLink("child", callID))
		out := agentsession.NewItemEntry(openresponses.NewFunctionCallOutput(callID, "done"))
		out.Parents = []agentsession.EntryRef{{Session: "child", Entry: leaf}}
		add(parent, out)
	}
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{
		Subsessions: func(id string) (*agentsession.Session, error) {
			if id == "child" {
				return child, nil
			}
			return nil, nil
		},
		Cost: func(_ string, u openresponses.Usage) (float64, bool) { return float64(u.InputTokens), true },
	})
	if err != nil {
		t.Fatal(err)
	}
	if fm := doc.FinalMetrics; fm.TotalCostUSD == nil || *fm.TotalCostUSD != 62 {
		got := -1.0
		if fm.TotalCostUSD != nil {
			got = *fm.TotalCostUSD
		}
		t.Errorf("total_cost_usd = %g, want 62: the path's 2 and the child's 10, 20 and 30 once each", got)
	}
}

// TestEmbedTakesTheResumeLeaf: with no leaf named in parents, the child
// is embedded at the leaf a reader resuming it takes, which a branch or
// a leaf label moves off the last appended entry, and the reference
// records the leaf taken and that no output named it (RFC 0001, link).
func TestEmbedTakesTheResumeLeaf(t *testing.T) {
	add := func(s *agentsession.Session, e agentsession.Entry) string {
		t.Helper()
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	child := agentsession.New(agentsession.Header{ID: "child", ParentSession: "parent", SpawnedBy: "call_1"})
	add(child, &agentsession.ConfigEntry{Model: "m"})
	fork := add(child, agentsession.NewItemEntry(openresponses.UserText("task")))
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("kept"), ResponseID: "r1"})
	kept := add(child, &agentsession.ResponseEntry{ResponseID: "r1"})
	if err := child.Branch(fork); err != nil {
		t.Fatal(err)
	}
	add(child, &agentsession.ItemEntry{Item: openresponses.AssistantText("abandoned"), ResponseID: "r2"})
	add(child, &agentsession.ResponseEntry{ResponseID: "r2"})
	if err := child.Branch(kept); err != nil {
		t.Fatal(err)
	}

	parent := agentsession.New(agentsession.Header{ID: "parent"})
	add(parent, &agentsession.ConfigEntry{Model: "m"})
	add(parent, &agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_1", Name: "spawn", Arguments: `{}`}, ResponseID: "p1"})
	add(parent, &agentsession.ResponseEntry{ResponseID: "p1"})
	add(parent, agentsession.NewSubsessionLink("child", "call_1"))
	add(parent, agentsession.NewItemEntry(openresponses.NewFunctionCallOutput("call_1", "kept")))
	tr, err := At(parent, parent.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{Subsessions: func(id string) (*agentsession.Session, error) {
		if id == "child" {
			return child, nil
		}
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.SubagentTrajectories) != 1 || doc.SubagentTrajectories[0].TrajectoryID != "child/"+kept {
		t.Fatalf("embedded %d, the first %+v", len(doc.SubagentTrajectories), doc.SubagentTrajectories)
	}
	var ref *atif.SubagentTrajectoryRef
	for _, s := range doc.Steps {
		if s.Observation != nil {
			for _, r := range s.Observation.Results {
				if len(r.SubagentTrajectoryRef) > 0 {
					ref = &r.SubagentTrajectoryRef[0]
				}
			}
		}
	}
	if ref == nil || ref.Extra["child_leaf"] != kept || ref.Extra["child_leaf_named"] != false {
		t.Errorf("the reference is %+v", ref)
	}
}

// rebuild is what a converter rebuilding a session from a document this
// package wrote finds, by the reference rule of RFC 0001,
// Projections/ATIF (0.17): each entry's line, put together from its
// envelope, history members and body under entries, an item's item
// taken from its step (items[] by item_entry_ids position, or the item
// of a step or observation result) and a response's body from its
// step; whether it matches, its id the one the document names and,
// since 0.18, the visible fields of its step and of the document hashing
// to the values recorded at the export; and whether the document holds the whole path, from a first
// entry to the leaf trajectory_id names, every step naming an entry.
type rebuild struct {
	order    []string
	lines    map[string]map[string]any
	verified map[string]bool
	whole    bool
	// strays are the step_ids of the steps that name no entry of the
	// document, which a converter lists.
	strays []any
}

// restoresHeader reports whether the converter restores the header's
// redacted and redacted_from: exactly when every entry verifies and the
// document holds the whole path.
func (r rebuild) restoresHeader() bool {
	if !r.whole || len(r.order) == 0 {
		return false
	}
	for _, id := range r.order {
		if !r.verified[id] {
			return false
		}
	}
	return true
}

// rebuildDocument rebuilds the entries of an ATIF document read as JSON.
func rebuildDocument(t *testing.T, raw []byte) rebuild {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	root := obj(obj(doc["extra"])[ExtraAgentSession])
	entries := obj(root["entries"])
	bodies := map[string]map[string]any{}
	steps, _ := doc["steps"].([]any)
	for _, sv := range steps {
		step := obj(sv)
		extra := obj(step["extra"])
		or := obj(extra[ExtraOpenResponses])
		if items, ok := or["items"].([]any); ok {
			ids, _ := extra["item_entry_ids"].([]any)
			if rb := obj(or["response"]); rb != nil {
				body := map[string]any{}
				for k, v := range rb {
					if k != "entry_id" {
						body[k] = v
					}
				}
				bodies[rb["entry_id"].(string)] = body
			}
			for i, it := range items {
				bodies[ids[i].(string)] = map[string]any{"item": it}
			}
		} else if item, ok := or["item"]; ok {
			bodies[obj(extra[ExtraAgentSession])["entry_id"].(string)] = map[string]any{"item": item}
		}
		if o := obj(step["observation"]); o != nil {
			results, _ := o["results"].([]any)
			for _, rv := range results {
				rex := obj(obj(rv)["extra"])
				if item, ok := obj(rex[ExtraOpenResponses])["item"]; ok {
					id, _ := obj(rex[ExtraAgentSession])["entry_id"].(string)
					bodies[id] = map[string]any{"item": item}
				}
			}
		}
	}
	r := rebuild{lines: map[string]map[string]any{}, verified: map[string]bool{}, whole: entries != nil}
	for id, rv := range entries {
		line := map[string]any{"id": id}
		for k, v := range obj(rv) {
			if k == "body" {
				for bk, bv := range obj(v) {
					line[bk] = bv
				}
				continue
			}
			line[k] = v
		}
		for k, v := range bodies[id] {
			if _, set := line[k]; !set {
				line[k] = v
			}
		}
		r.lines[id] = line
		data, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := agentsession.EntryHashes(data)
		r.verified[id] = err == nil && got == id
	}
	// Parent before child; a parent the document does not hold means it
	// does not hold the whole path.
	done := map[string]bool{}
	for len(r.order) < len(r.lines) {
		progressed := false
		for _, id := range sortedKeys(r.lines) {
			if done[id] {
				continue
			}
			p, _ := r.lines[id]["parent"].(string)
			if p == "" || done[p] {
				done[id] = true
				r.order = append(r.order, id)
				progressed = true
			}
		}
		if !progressed {
			r.whole = false
			break
		}
	}
	// The path reaches the leaf trajectory_id names, not only each
	// parent present (0.18): from the leaf up, every entry is held.
	leaf, _ := doc["trajectory_id"].(string)
	for id, n := leaf, 0; ; n++ {
		line, ok := r.lines[id]
		if !ok || n > len(r.lines) {
			r.whole = false
			break
		}
		p, _ := line["parent"].(string)
		if p == "" {
			break
		}
		id = p
	}
	// Every step names an entry of the document (0.18): one appended
	// after the export is not of the path, and is listed.
	named := func(step map[string]any) string {
		id, _ := obj(obj(step["extra"])[ExtraAgentSession])["entry_id"].(string)
		return id
	}
	for _, sv := range steps {
		if _, ok := r.lines[named(obj(sv))]; !ok {
			r.whole = false
			r.strays = append(r.strays, obj(sv)["step_id"])
		}
	}
	// An entry matches only when what a reader sees is what was
	// exported (0.18): the document's visible fields hash to the value
	// its extra records, or no entry matches, and so do those of the
	// step the entry maps to. The id covers the raw item, and not what a
	// reader of the document sees.
	sealed := func(v map[string]any, visible map[string]any) bool {
		want, _ := obj(obj(v["extra"])[ExtraAgentSession])[ExtraVisible].(string)
		got, err := VisibleHash(visible)
		return err == nil && want != "" && got == want
	}
	docMatches := sealed(doc, DocumentVisible(doc)) && StepsInOrder(doc)
	for _, sv := range steps {
		step := obj(sv)
		if docMatches && sealed(step, StepVisible(step)) {
			continue
		}
		for _, id := range stepEntries(step) {
			r.verified[id] = false
		}
	}
	if !docMatches {
		for id := range r.verified {
			r.verified[id] = false
		}
	}
	return r
}

// stepEntries are the ids of the entries a step is made from: its own,
// its items' and response's, and its observation results'.
func stepEntries(step map[string]any) []string {
	obj := func(v any) map[string]any { m, _ := v.(map[string]any); return m }
	extra := obj(step["extra"])
	var ids []string
	add := func(v any) {
		if id, ok := v.(string); ok && id != "" {
			ids = append(ids, id)
		}
	}
	add(obj(extra[ExtraAgentSession])["entry_id"])
	items, _ := extra["item_entry_ids"].([]any)
	for _, id := range items {
		add(id)
	}
	add(obj(obj(extra[ExtraOpenResponses])["response"])["entry_id"])
	if o := obj(step["observation"]); o != nil {
		rs, _ := o["results"].([]any)
		for _, rv := range rs {
			add(obj(obj(obj(rv)["extra"])[ExtraAgentSession])["entry_id"])
		}
	}
	return ids
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestRebuildToTheDocumentsIDs is the round trip RFC 0001's ATIF
// read-back rule allows (0.17, #21 option B), for agentimport to hold a
// converter to: a redacted session's document carries each entry's
// envelope and history members under entries, every entry rebuilt from
// the document hashes to the id it names, and the session so rebuilt has
// the redacted session's ids, its entries' redacted members and its
// header's redacted and redacted_from. A document a redactor changed
// after the export, or one that does not hold the whole path, rebuilds
// entries that do not verify, or no whole path, and the header is not
// restored. Since 0.18 that holds of a document changed in a step's
// visible message alone, whose ids all match, of one with a step
// appended that names no entry, and of one whose path stops short of
// its leaf.
func TestRebuildToTheDocumentsIDs(t *testing.T) {
	red := redaction(t, "basic", "test/v1", nil, func(v string) string {
		if v == "4" {
			return "[n]"
		}
		return v
	})
	var file bytes.Buffer
	if err := agentsession.Write(&file, red); err != nil {
		t.Fatal(err)
	}
	s, err := agentsession.Read(&file)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	raw := encode(t, doc)
	r := rebuildDocument(t, raw)
	path := s.Path(s.Leaf())
	if len(r.order) != len(path) || !r.restoresHeader() {
		t.Fatalf("rebuilt %d entries of %d, whole %v, verified %v", len(r.order), len(path), r.whole, r.verified)
	}
	h := s.Header()
	var out bytes.Buffer
	header := map[string]any{"type": "session", "format": h.Format, "id": doc.SessionID, "created_at": h.CreatedAt.UTC().Format(time.RFC3339),
		"payload": h.Payload, "redacted": true, "redacted_from": obj(t, doc.Extra[ExtraAgentSession])["redacted_from"]}
	for _, v := range append([]any{header}, func() []any {
		var ls []any
		for _, id := range r.order {
			ls = append(ls, r.lines[id])
		}
		return ls
	}()...) {
		data, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(append(data, '\n'))
	}
	back, err := agentsession.Read(&out)
	if err != nil {
		t.Fatalf("the rebuilt session does not read: %v", err)
	}
	if bh := back.Header(); !bh.Redacted || bh.RedactedFrom == nil || bh.RedactedFrom.Session != h.RedactedFrom.Session || bh.RedactedFrom.Profile != h.RedactedFrom.Profile {
		t.Errorf("the rebuilt header is %+v", bh)
	}
	for _, e := range path {
		got, ok := back.Entry(e.Base().ID)
		if !ok {
			t.Errorf("the rebuilt session lacks %s", e.Base().ID)
			continue
		}
		want, _ := agentsession.RedactedOf(e)
		if spans, ok := agentsession.RedactedOf(got); !ok || !slices.Equal(spans, want) || got.Base().LegacyID != e.Base().LegacyID {
			t.Errorf("%s rebuilt with redacted %v, legacy_id %q", e.Base().ID, spans, got.Base().LegacyID)
		}
	}

	// Changed after the export: the entries whose text changed hash to
	// another id, so the header is not restored.
	changed := mustDoc(t, tr)
	if err := Redact(changed, Secrets("What is 2+2?")); err != nil {
		t.Fatal(err)
	}
	if r := rebuildDocument(t, encode(t, changed)); r.restoresHeader() || r.verified[lid(t, s, "i0000001")] {
		t.Errorf("a document changed after the export verifies: %v", r.verified)
	}
	// Without the first entry the document holds no whole path.
	var cut map[string]any
	if err := json.Unmarshal(raw, &cut); err != nil {
		t.Fatal(err)
	}
	delete(obj(t, obj(t, obj(t, cut["extra"])[ExtraAgentSession])["entries"]), path[0].Base().ID)
	cutRaw, err := json.Marshal(cut)
	if err != nil {
		t.Fatal(err)
	}
	if r := rebuildDocument(t, cutRaw); r.whole || r.restoresHeader() {
		t.Error("a document without its first entry holds the whole path")
	}

	// Changed in a step's visible message alone (0.18), as a redaction
	// applied to the document's text and not to its raw items is: every
	// id still matches, and the entry whose step shows other text than it
	// projects is not rebuilt to its id, so the header is not restored.
	first := lid(t, s, "i0000001")
	visible := edit(t, raw, func(doc map[string]any) {
		for _, sv := range doc["steps"].([]any) {
			step := obj(t, sv)
			if obj(t, obj(t, step["extra"])[ExtraAgentSession])["entry_id"] == first {
				step["message"] = "[redacted]"
			}
		}
	})
	if r := rebuildDocument(t, visible); r.restoresHeader() || r.verified[first] || !r.verified[lid(t, s, "i0000002")] {
		t.Errorf("a document changed in a step's message alone: verified %v", r.verified)
	}
	// Every step and the document carry the hash of their visible fields
	// (0.18), and a field the old comparison left out, a step's
	// model_name or the agent's tool_definitions, edited alone is caught.
	var plain map[string]any
	if err := json.Unmarshal(raw, &plain); err != nil {
		t.Fatal(err)
	}
	for _, sv := range plain["steps"].([]any) {
		if h, _ := obj(t, obj(t, obj(t, sv)["extra"])[ExtraAgentSession])["visible"].(string); !visibleHashForm(h) {
			t.Errorf("step %v carries visible %q", obj(t, sv)["step_id"], h)
		}
	}
	if h, _ := obj(t, obj(t, plain["extra"])[ExtraAgentSession])["visible"].(string); !visibleHashForm(h) {
		t.Errorf("the document carries visible %q", h)
	}
	model := edit(t, raw, func(doc map[string]any) {
		for _, sv := range doc["steps"].([]any) {
			if step := obj(t, sv); step["model_name"] != nil {
				step["model_name"] = "another-model"
				return
			}
		}
		t.Fatal("no step names a model")
	})
	if r := rebuildDocument(t, model); r.restoresHeader() {
		t.Error("a document changed in a step's model_name alone restores the header")
	}
	tools := edit(t, raw, func(doc map[string]any) {
		agent := obj(t, doc["agent"])
		defs, _ := agent["tool_definitions"].([]any)
		if len(defs) == 0 {
			t.Fatal("the document defines no tool")
		}
		obj(t, defs[0])["description"] = "edited"
	})
	if r := rebuildDocument(t, tools); r.restoresHeader() {
		t.Error("a document changed in a tool definition alone restores the header")
	}
	// The steps are the export's in number and order: a step duplicated,
	// or two swapped, each still hashing as written, does not match.
	duplicated := edit(t, raw, func(doc map[string]any) {
		steps := doc["steps"].([]any)
		doc["steps"] = append(steps, steps[len(steps)-1])
	})
	swapped := edit(t, raw, func(doc map[string]any) {
		steps := doc["steps"].([]any)
		steps[0], steps[1] = steps[1], steps[0]
	})
	for name, changed := range map[string][]byte{"duplicated": duplicated, "swapped": swapped} {
		if r := rebuildDocument(t, changed); r.restoresHeader() {
			t.Errorf("a document with a step %s restores the header", name)
		}
	}
	// A step appended after the export names no entry: it is not of the
	// path, so the document does not hold the whole path (0.18).
	appended := edit(t, raw, func(doc map[string]any) {
		doc["steps"] = append(doc["steps"].([]any), map[string]any{"step_id": json.Number("99"), "source": "user", "message": "appended"})
	})
	if r := rebuildDocument(t, appended); r.whole || r.restoresHeader() || len(r.strays) != 1 {
		t.Errorf("a document with a step appended: whole %v, strays %v", r.whole, r.strays)
	}
	// Without the leaf every parent is present, and the path stops short
	// of the leaf trajectory_id names (0.18).
	short := edit(t, raw, func(doc map[string]any) {
		delete(obj(t, obj(t, obj(t, doc["extra"])[ExtraAgentSession])["entries"]), s.Leaf())
	})
	if r := rebuildDocument(t, short); r.whole || r.restoresHeader() {
		t.Error("a document without its leaf holds the whole path")
	}
}

// edit returns the document raw holds changed by change, as JSON.
func edit(t *testing.T, raw []byte, change func(doc map[string]any)) []byte {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// obj is v as a JSON object.
func obj(t *testing.T, v any) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%T is no object", v)
	}
	return m
}

// TestRebuildAgentStepItems: an agent step merges its items' trust and
// their members the format does not define into one extra, and each item
// still rebuilds to its id from its body under entries and its item from
// the step's items, as do the output and the response (RFC 0001,
// Projections/ATIF, 0.17).
func TestRebuildAgentStepItems(t *testing.T) {
	s := agentsession.New(agentsession.Header{ID: "s"})
	probe := func(e agentsession.Entry, v string) agentsession.Entry {
		e.Base().Unknown = map[string]json.RawMessage{"x:probe": json.RawMessage(`"` + v + `"`)}
		return e
	}
	mustAppend(t, s, &agentsession.ConfigEntry{Model: "gpt-5"})
	mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserText("look it up")))
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: &openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "search"}}}, ResponseID: "resp_1", Trust: &agentsession.Trust{Reads: []string{agentsession.TrustUntrusted}}}, "reasoning"))
	call, err := s.Append(probe(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "search", Arguments: `{}`}, ResponseID: "resp_1", Trust: &agentsession.Trust{Reads: []string{agentsession.TrustPrivate}}}, "call"))
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted})
	mustAppend(t, s, agentsession.NewDispatch("call_1", call))
	mustAppend(t, s, probe(&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput("call_1", "found"), Failed: true}, "output"))
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	r := rebuildDocument(t, encode(t, mustDoc(t, tr)))
	path := s.Path(s.Leaf())
	if len(r.order) != len(path) || !r.whole {
		t.Fatalf("rebuilt %d entries of %d, whole %v", len(r.order), len(path), r.whole)
	}
	for _, e := range path {
		if !r.verified[e.Base().ID] {
			t.Errorf("%T %s does not rebuild to its id: %v", e, e.Base().ID, r.lines[e.Base().ID])
		}
	}
}

// TestRedactorsReachNormalised: a normalised record holds the member it
// describes as source text (was) and original bytes (raw, base64), which
// a redactor rewriting strings does not see, so a redactor that would
// change one drops the record, under entries and in a response's body;
// the entry then does not rebuild to its id, which a converter lists.
func TestRedactorsReachNormalised(t *testing.T) {
	s := loadFixture(t, "normalised")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	const raw = "ImJ5dGVzIOKCIGVuZCI=" // "bytes \xe2\x82 end", the output's original bytes
	if plain := string(encode(t, mustDoc(t, tr))); !strings.Contains(plain, raw) {
		t.Fatalf("the document does not carry the output's raw bytes")
	}
	doc, err := ToATIF(tr, Options{Redactors: []Redactor{Secrets("\xe2\x82")}})
	if err != nil {
		t.Fatal(err)
	}
	if out := string(encode(t, doc)); strings.Contains(out, raw) {
		t.Errorf("the secret survives in a normalised record's raw bytes")
	}
	// Environment drops an env entry's body under entries, and its
	// normalised record, which holds the same.
	envs := agentsession.New(agentsession.Header{ID: "e"})
	mustAppend(t, envs, &agentsession.ConfigEntry{Model: "m"})
	env := agentsession.NewEnvEntry("/home/u/secret-project")
	env.Unknown = map[string]json.RawMessage{"x:inode": json.RawMessage(`9007199254740993`)}
	mustAppend(t, envs, env)
	if len(env.Normalised) == 0 {
		t.Fatalf("the env entry was written without a normalised record")
	}
	mustAppend(t, envs, agentsession.NewItemEntry(openresponses.UserText("hi")))
	etr, err := At(envs, envs.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err = ToATIF(etr, Options{Redactors: []Redactor{Environment()}})
	if err != nil {
		t.Fatal(err)
	}
	rec := obj(t, obj(t, obj(t, doc.Extra[ExtraAgentSession])[ExtraEntries])[env.ID])
	if rec["body"] != nil || rec["normalised"] != nil {
		t.Errorf("the env entry keeps %v", rec)
	}
}

// TestPartTrustSurvivesNoPassthrough: the path's trust travels under the
// root's extra.agentsession, so an instructions part's labels, which
// otherwise travel only in its config's body under entries, survive a
// document stripped of its passthrough, as an item's do in its step
// (RFC 0001, Projections/ATIF, 0.17).
func TestPartTrustSurvivesNoPassthrough(t *testing.T) {
	s := loadFixture(t, "instruction-trust")
	tr, err := At(s, s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := ToATIF(tr, Options{Redactors: []Redactor{NoPassthrough()}})
	if err != nil {
		t.Fatal(err)
	}
	as := obj(t, doc.Extra[ExtraAgentSession])
	if !sameValue(t, as["trust"], `{"reads":["private","untrusted"]}`) {
		t.Errorf("the root carries trust %v", as["trust"])
	}
}

// visibleHashForm reports whether h is a hash in the format's notation,
// sha256: and 64 lowercase hexadecimal digits.
func visibleHashForm(h string) bool {
	hex, ok := strings.CutPrefix(h, "sha256:")
	if !ok || len(hex) != 64 {
		return false
	}
	for _, c := range hex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
