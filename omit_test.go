package agentsession

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// omitScript builds a session from tokens, one entry each, and returns
// it with the entry ID of each token that names one:
//
//	cfg:m          a config setting the model to m
//	cfg:m+rule     the same, with omit reasoning other_models
//	rs:n[:r]       a reasoning item of response r (default resp-n), id n
//	msg:n[:r]      an assistant message of response r, named n
//	user:n         a user message, named n
//	resp:r[:model] a response entry r, with the model it reports
//
// The fixtures are real sessions, so the IDs are hashes; names map to
// them.
func omitScript(t *testing.T, tokens ...string) (*Session, map[string]string) {
	t.Helper()
	s := New(Header{})
	ids := map[string]string{}
	for _, tok := range tokens {
		kind, rest, _ := strings.Cut(tok, ":")
		var e Entry
		name := ""
		switch kind {
		case "cfg":
			model, rule, _ := strings.Cut(rest, "+")
			c := &ConfigEntry{Model: model}
			if rule != "" {
				c.Omit = &Omit{Reasoning: OmitOtherModels}
			}
			e = c
		case "rs", "msg":
			n, r, _ := strings.Cut(rest, ":")
			name = n
			if r == "" {
				r = "resp-" + n
			}
			var item openresponses.Item = &openresponses.ReasoningItem{ID: "rs_" + n, Summary: openresponses.Contents{&openresponses.SummaryText{Text: n}}, EncryptedContent: "enc-" + n}
			if kind == "msg" {
				item = openresponses.AssistantText(n)
			}
			ie := NewItemEntry(item)
			if r != "-" {
				ie.ResponseID = r
			}
			e = ie
		case "user":
			name = rest
			e = NewItemEntry(openresponses.UserText(rest))
		case "resp":
			r, model, _ := strings.Cut(rest, ":")
			e = &ResponseEntry{ResponseID: r, Model: model, Status: openresponses.ResponseStatusCompleted}
		default:
			t.Fatalf("bad token %q", tok)
		}
		id, err := s.Append(e)
		if err != nil {
			t.Fatalf("append %s: %v", tok, err)
		}
		if name != "" {
			ids[name] = id
		}
	}
	return s, ids
}

// itemNames lists the entries a context's items came from by the names
// omitScript gave them, in order.
func itemNames(ctx Context, ids map[string]string) string {
	byID := map[string]string{}
	for name, id := range ids {
		byID[id] = name
	}
	var out []string
	for _, e := range ctx.ItemEntries {
		out = append(out, byID[e.Base().ID])
	}
	return strings.Join(out, ",")
}

// leftOut lists what a context leaves out, by name and reason.
func leftOut(ctx Context, ids map[string]string) string {
	byID := map[string]string{}
	for name, id := range ids {
		byID[id] = name
	}
	var out []string
	for _, o := range ctx.OmittedItems {
		out = append(out, byID[o.Entry.ID]+"/"+o.Reason)
	}
	return strings.Join(out, ",")
}

// TestOmitReasoningOtherModels: a request leaves out a reasoning item
// that carries a response and was written while another model was in
// force, and nothing else; the model a response entry reports is not
// read.
func TestOmitReasoningOtherModels(t *testing.T) {
	for _, tt := range []struct {
		name   string
		script []string
		items  string // the items kept, by name
		left   string // what is left out, by name and reason
	}{
		{"one model leaves nothing out",
			[]string{"cfg:a+rule", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "user:u2"},
			"u1,r1,m1,u2", ""},
		{"the rule leaves out the other model's reasoning and nothing else",
			[]string{"cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule", "user:u2"},
			"u1,m1,u2", "r1/other_models"},
		{"a switch back brings the model's own reasoning back",
			[]string{"cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule", "user:u2", "rs:r2", "msg:m2", "resp:resp-r2:b", "cfg:a", "user:u3"},
			"u1,r1,m1,u2,m2,u3", "r2/other_models"},
		{"the rule is in force from the entry that writes it",
			[]string{"cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b", "user:u2", "rs:r2", "msg:m2", "resp:resp-r2:b", "cfg:a+rule", "user:u3"},
			"u1,r1,m1,u2,m2,u3", "r2/other_models"},
		{"the model a response reports is not read",
			[]string{"cfg:a+rule", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a-2026-08-07", "user:u2"},
			"u1,r1,m1,u2", ""},
		{"a reasoning item that names no response stays",
			[]string{"cfg:a", "user:u1", "rs:r1:-", "msg:m1", "cfg:b+rule"},
			"u1,r1,m1", ""},
		{"a reasoning item of a response whose entry is not on the path is still judged by its model",
			[]string{"cfg:a", "user:u1", "rs:r1", "cfg:b+rule"},
			"u1", "r1/other_models"},
		{"a message of another model stays",
			[]string{"cfg:a", "user:u1", "msg:m1", "resp:resp-m1:a", "cfg:b+rule"},
			"u1,m1", ""},
		{"an item written under no model is never left out",
			[]string{"cfg:+rule", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1", "cfg:b", "user:u2"},
			"u1,r1,m1,u2", ""},
		{"a request under no model leaves nothing out",
			[]string{"cfg:a+rule", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "user:u2", "rs:r2"},
			"u1,r1,m1,u2,r2", ""},
		{"a value the format does not define has no effect",
			[]string{"cfg:a", "user:u1", "rs:r1", "cfg:b"},
			"u1,r1", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, ids := omitScript(t, tt.script...)
			if tt.name == "a value the format does not define has no effect" {
				if _, err := s.Append(&ConfigEntry{Omit: &Omit{Reasoning: "all_models"}}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.name == "a request under no model leaves nothing out" {
				// A replace that sets no model leaves none in force.
				if _, err := s.Append(&ConfigEntry{Replace: true, Omit: &Omit{Reasoning: OmitOtherModels}}); err != nil {
					t.Fatal(err)
				}
			}
			ctx, err := s.Context()
			if err != nil {
				t.Fatal(err)
			}
			if got := itemNames(ctx, ids); got != tt.items {
				t.Errorf("items %s, want %s", got, tt.items)
			}
			if got := leftOut(ctx, ids); got != tt.left {
				t.Errorf("left out %s, want %s", got, tt.left)
			}
			if len(ctx.Items) != len(ctx.ItemEntries) {
				t.Errorf("%d items, %d entries", len(ctx.Items), len(ctx.ItemEntries))
			}
			// The entries the context selected still hold what it left out.
			for _, o := range ctx.OmittedItems {
				if !slices.Contains(ctx.Entries, Entry(o.Entry)) {
					t.Errorf("the entry left out is not among the context's entries")
				}
			}
		})
	}
}

// TestOmitUnknownRule: a reasoning value the format does not define
// leaves the rule in force as it was, and does not clear it.
func TestOmitUnknownRule(t *testing.T) {
	s, ids := omitScript(t, "cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule", "user:u2")
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Reasoning: "all_models"}}); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Settings.Omit.Reasoning != OmitOtherModels {
		t.Errorf("an unknown rule changed the rule in force to %q", ctx.Settings.Omit.Reasoning)
	}
	if got := leftOut(ctx, ids); got != "r1/other_models" {
		t.Errorf("left out %s", got)
	}
}

// TestOmitItems: an item entry whose ID the omit in force lists
// contributes nothing, whatever it holds; an ID that names no item entry
// on the path names nothing; and the lists of every entry since the last
// replace add up.
func TestOmitItems(t *testing.T) {
	s, ids := omitScript(t, "cfg:a", "user:u1", "msg:m1", "user:u2", "msg:m2", "user:u3")
	cfg, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{ids["u1"], ids["m1"]}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := s.Context()
	if got := itemNames(ctx, ids); got != "u2,m2,u3" {
		t.Errorf("items %s", got)
	}
	if got := leftOut(ctx, ids); got != "u1/items,m1/items" {
		t.Errorf("left out %s", got)
	}
	// A later entry adds to the set and need not repeat it; an ID that
	// names the config entry, which is no item entry, names nothing.
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{ids["u3"], cfg, "sha256:nowhere"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = s.Context()
	if got := itemNames(ctx, ids); got != "u2,m2" {
		t.Errorf("items %s after a second list", got)
	}
	if got := ctx.Settings.Omit.Items; len(got) != 5 {
		t.Errorf("the set in force is %v", got)
	}
	// A path that ends before the entry never meets the list.
	before, err := s.ContextAt(ids["u3"])
	if err != nil {
		t.Fatal(err)
	}
	if got := itemNames(before, ids); got != "u1,m1,u2,m2,u3" || len(before.OmittedItems) != 0 {
		t.Errorf("a path above the config leaves out %s: %s", leftOut(before, ids), got)
	}
	// {} clears the set, and a replace clears it with the rest.
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{}}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = s.Context()
	if got := itemNames(ctx, ids); got != "u1,m1,u2,m2,u3" || !ctx.Settings.Omit.IsZero() {
		t.Errorf("after {} the request holds %s under %+v", got, ctx.Settings.Omit)
	}
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{ids["u1"]}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(&ConfigEntry{Replace: true, Model: "b"}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = s.Context()
	if got := itemNames(ctx, ids); got != "u1,m1,u2,m2,u3" || !ctx.Settings.Omit.IsZero() {
		t.Errorf("after a replace the request holds %s under %+v", got, ctx.Settings.Omit)
	}
}

// TestOmitBoth: an entry the list names and the rule reaches is reported
// as listed.
func TestOmitBoth(t *testing.T) {
	s, ids := omitScript(t, "cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule")
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{ids["r1"]}}}); err != nil {
		t.Fatal(err)
	}
	ctx, _ := s.Context()
	if got := leftOut(ctx, ids); got != "r1/items" {
		t.Errorf("left out %s", got)
	}
}

// TestOmitMerge: an omit delta sets the rule it names, keeps the one in
// force when it names none, adds its items to those in force without
// repeats, and clears both when it names neither.
func TestOmitMerge(t *testing.T) {
	rule := Omit{Reasoning: OmitOtherModels}
	for _, tt := range []struct {
		name     string
		deltas   []*Omit
		reasoned string
		items    string
	}{
		{"a rule", []*Omit{{Reasoning: OmitOtherModels}}, OmitOtherModels, ""},
		{"items add to a rule", []*Omit{{Reasoning: OmitOtherModels}, {Items: []string{"a"}}}, OmitOtherModels, "a"},
		{"the set is a union without repeats", []*Omit{{Items: []string{"a", "b"}}, {Items: []string{"b", "c", "a"}}}, "", "a,b,c"},
		{"a rule joins the items in force", []*Omit{{Items: []string{"a"}}, {Reasoning: OmitOtherModels}}, OmitOtherModels, "a"},
		{"{} clears both", []*Omit{{Reasoning: OmitOtherModels, Items: []string{"a"}}, {}}, "", ""},
		{"an empty list clears too", []*Omit{{Reasoning: OmitOtherModels, Items: []string{"a"}}, {Items: []string{}}}, "", ""},
		{"nil leaves what is in force", []*Omit{{Reasoning: OmitOtherModels, Items: []string{"a"}}, nil}, OmitOtherModels, "a"},
		{"what is cleared is written afresh", []*Omit{{Items: []string{"a"}}, {}, {Items: []string{"b"}}}, "", "b"},
		{"an empty id names nothing", []*Omit{{Items: []string{"", "a"}}}, "", "a"},
		{"a list of empty ids names none and clears", []*Omit{{Reasoning: OmitOtherModels, Items: []string{"a"}}, {Items: []string{""}}}, "", ""},
		{"a rule the format does not define leaves the one in force", []*Omit{{Reasoning: OmitOtherModels}, {Reasoning: "later"}}, OmitOtherModels, ""},
		{"a rule the format does not define alone sets nothing", []*Omit{{Reasoning: "later"}}, "", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var settings Settings
			for _, d := range tt.deltas {
				settings = settings.Apply(&ConfigEntry{Omit: d})
			}
			if settings.Omit.Reasoning != tt.reasoned || strings.Join(settings.Omit.Items, ",") != tt.items {
				t.Errorf("omit %+v, want reasoning %q and items %q", settings.Omit, tt.reasoned, tt.items)
			}
		})
	}
	// A delta does not change the settings it was applied to.
	base := Settings{}.Apply(&ConfigEntry{Omit: &Omit{Items: []string{"a"}}})
	_ = base.Apply(&ConfigEntry{Omit: &Omit{Items: []string{"b"}}})
	if len(base.Omit.Items) != 1 {
		t.Errorf("Apply changed its receiver: %v", base.Omit.Items)
	}
	// A replace clears the object with the rest of the settings.
	if got := (Settings{Omit: rule}).Apply(&ConfigEntry{Replace: true, Model: "m"}); !got.Omit.IsZero() {
		t.Errorf("a replace left %+v", got.Omit)
	}
	if got := (Settings{Omit: rule}).Apply(&ConfigEntry{Replace: true, Omit: &Omit{Items: []string{"a"}}}); got.Omit.Reasoning != "" || len(got.Omit.Items) != 1 {
		t.Errorf("a replace carrying omit left %+v", got.Omit)
	}
}

// TestOmitThroughCompaction: a checkpoint carries the object in force,
// and a reasoning item the fold kept is still judged by the model in
// force where it was written, so the rule keeps covering the window.
func TestOmitThroughCompaction(t *testing.T) {
	s, ids := omitScript(t, "cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule", "user:u2", "rs:r2", "msg:m2", "resp:resp-r2:b")
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{ids["u2"]}}}); err != nil {
		t.Fatal(err)
	}
	ctx, _ := s.Context()
	if got := itemNames(ctx, ids); got != "u1,m1,r2,m2" {
		t.Fatalf("before the fold the request holds %s", got)
	}
	// Keep everything the request holds after the first item.
	comp, err := s.CompactKeeping(3, openresponses.UserText("summary"))
	if err != nil {
		t.Fatal(err)
	}
	if comp.Config.Omit.Reasoning != OmitOtherModels || !slices.Equal(comp.Config.Omit.Items, []string{ids["u2"]}) {
		t.Errorf("the checkpoint carries %+v", comp.Config.Omit)
	}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	ctx, _ = s.Context()
	byID := map[string]string{}
	for name, id := range ids {
		byID[id] = name
	}
	var kept []string
	for _, e := range ctx.ItemEntries {
		kept = append(kept, byID[e.Base().ID])
	}
	if got := strings.Join(kept, ","); got != ",m1,r2,m2" {
		t.Errorf("after the fold the request holds %s", got)
	}
	// And a switch after the fold covers the kept reasoning of b.
	if _, err := s.Append(&ConfigEntry{Model: "a"}); err != nil {
		t.Fatal(err)
	}
	ctx, _ = s.Context()
	if got := leftOut(ctx, ids); got != "u2/items,r2/other_models" {
		t.Errorf("after switching back the request leaves out %s", got)
	}
	// A checkpoint is written and read back with the member.
	back, err := UnmarshalEntry(mustJSON(t, comp))
	if err != nil {
		t.Fatal(err)
	}
	if got := back.(*CompactionEntry).Config.Omit; got.Reasoning != OmitOtherModels || len(got.Items) != 1 {
		t.Errorf("the checkpoint reads back with %+v", got)
	}
}

// TestOmitRoundTrip: omit is read and written as a member of a config
// entry, {} included; a member that is not the object, which a file
// from before 0.11 may hold, is kept as written and leaves nothing out.
func TestOmitRoundTrip(t *testing.T) {
	const head = `{"type":"config","id":"c","parent":null,"ts":"2026-10-01T12:00:00Z","model":"m",`
	for _, tt := range []struct {
		name, omit string
		typed      bool
		clears     bool
	}{
		{"the rule and items", `"omit":{"reasoning":"other_models","items":["sha256:a","sha256:b"]}`, true, false},
		{"an empty object", `"omit":{}`, true, true},
		{"a rule this package does not define", `"omit":{"reasoning":"later"}`, true, false},
		{"a member inside it this package does not define", `"omit":{"reasoning":"other_models","why":"x"}`, true, false},
		{"a string", `"omit":"yes"`, false, false},
		{"items that are not strings", `"omit":{"items":[1,2]}`, false, false},
		{"a list", `"omit":["a"]`, false, false},
		{"null", `"omit":null`, false, false},
		{"a key in another case", `"omit":{"Reasoning":"other_models"}`, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			line := head + tt.omit + `}`
			e, err := UnmarshalEntry([]byte(line))
			if err != nil {
				t.Fatal(err)
			}
			c := e.(*ConfigEntry)
			if (c.Omit != nil) != tt.typed {
				t.Errorf("typed = %v, want %v (unknown %v)", c.Omit != nil, tt.typed, c.Unknown)
			}
			if _, kept := c.Unknown["omit"]; kept == tt.typed {
				t.Errorf("omit is in Unknown: %v, want %v", kept, !tt.typed)
			}
			back, err := MarshalEntry(c)
			if err != nil {
				t.Fatal(err)
			}
			if !sameJSON(t, back, []byte(line)) {
				t.Errorf("rewritten as %s", back)
			}
			// What a reader makes of it.
			settings := Settings{Omit: Omit{Reasoning: OmitOtherModels, Items: []string{"x"}}}.Apply(c)
			if got := settings.Omit.IsZero(); got != tt.clears && tt.typed {
				t.Errorf("clears = %v, want %v", got, tt.clears)
			}
			if !tt.typed && (settings.Omit.Reasoning != OmitOtherModels || len(settings.Omit.Items) != 1) {
				t.Errorf("a member that is not the object changed what is in force: %+v", settings.Omit)
			}
		})
	}
	// A checkpoint's omit takes the same care.
	cp := `{"type":"compaction","id":"k","parent":null,"ts":"2026-10-01T12:00:00Z","first_kept":"x","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"s"}]},"config":{"model":"m","omit":"yes"}}`
	e, err := UnmarshalEntry([]byte(cp))
	if err != nil {
		t.Fatal(err)
	}
	if got := e.(*CompactionEntry).Config.Omit; !got.IsZero() {
		t.Errorf("a checkpoint with a string omit reads as %+v", got)
	}
	back, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, back, []byte(cp)) {
		t.Errorf("checkpoint rewritten as %s", back)
	}
}

// TestOmitInAnEarlierFile: the format's rule for a member a later minor
// defines is about its form, not the header: a file that declares 0.10
// and holds the object this document defines reads it as 0.11 does.
func TestOmitInAnEarlierFile(t *testing.T) {
	in, _ := hashedLines(t, "agentsession/0.10",
		`"type":"config","model":"a"`,
		`"type":"item","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		`"type":"item","response":"resp-1","item":{"type":"reasoning","summary":[],"encrypted_content":"x"}`,
		`"type":"config","model":"b","omit":{"reasoning":"other_models"}`,
	)
	s, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(ctx.Items) != 1 || len(ctx.OmittedItems) != 1 {
		t.Errorf("%d items, %d left out", len(ctx.Items), len(ctx.OmittedItems))
	}
}

// TestOmitVerifyDivergence: a response whose recorded hash is the
// request with the items the omit setting leaves out fails as a
// divergence from the rule, which wraps the ordinary mismatch, and one
// whose hash matches neither is an ordinary mismatch.
func TestOmitVerifyDivergence(t *testing.T) {
	s, ids := omitScript(t, "cfg:a", "user:u1", "rs:r1", "msg:m1", "resp:resp-r1:a", "cfg:b+rule", "user:u2")
	path := s.Path(s.Leaf())
	hashOf := func(omit bool) string {
		t.Helper()
		ctx, err := buildContext(path, omit)
		if err != nil {
			t.Fatal(err)
		}
		req, err := ctx.Request()
		if err != nil {
			t.Fatal(err)
		}
		h, err := RequestHash(req)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, tt := range []struct {
		name, hash string
		want       error
		divergence bool
	}{
		{"the request as the rule builds it", hashOf(true), nil, false},
		{"the request with the items in", hashOf(false), ErrHashMismatch, true},
		{"another request", "sha256:" + strings.Repeat("0", 64), ErrHashMismatch, false},
		{"no hash", "", ErrNoHash, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := New(Header{})
			for _, e := range s.Path(s.Leaf()) {
				cp := e
				if _, err := f.Append(cp); err != nil {
					t.Fatal(err)
				}
			}
			out := NewItemEntry(openresponses.AssistantText("ok"))
			out.ResponseID = "resp-2"
			if _, err := f.Append(out); err != nil {
				t.Fatal(err)
			}
			id, err := f.Append(&ResponseEntry{ResponseID: "resp-2", Status: openresponses.ResponseStatusCompleted, RequestHash: tt.hash})
			if err != nil {
				t.Fatal(err)
			}
			err = f.Verify(id)
			if tt.want == nil && err != nil || tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("Verify = %v, want %v", err, tt.want)
			}
			if got := errors.Is(err, ErrOmitDivergence); got != tt.divergence {
				t.Errorf("divergence = %v, want %v (%v)", got, tt.divergence, err)
			}
		})
	}
	_ = ids
}

// TestOmitDelta: the member that takes the omit in force to the one a
// writer wants adds what is new, writes nothing when nothing moved, and
// says when the change is not one delta.
func TestOmitDelta(t *testing.T) {
	rule := Omit{Reasoning: OmitOtherModels}
	for _, tt := range []struct {
		name string
		have Omit
		want Omit
		out  string // the delta, "nil" for none, "{}" for a clear
		ok   bool
	}{
		{"nothing in force, nothing wanted", Omit{}, Omit{}, "nil", true},
		{"the rule is new", Omit{}, rule, "reasoning=other_models", true},
		{"the rule is already in force", rule, rule, "nil", true},
		{"items are new", rule, Omit{Reasoning: OmitOtherModels, Items: []string{"a", "b"}}, "items=a,b", true},
		{"only the items not yet in force", Omit{Items: []string{"a"}}, Omit{Items: []string{"b", "a"}}, "items=b", true},
		{"the same set in another order", Omit{Items: []string{"a", "b"}}, Omit{Items: []string{"b", "a"}}, "nil", true},
		{"the rule joins the items", Omit{Items: []string{"a"}}, Omit{Reasoning: OmitOtherModels, Items: []string{"a"}}, "reasoning=other_models", true},
		{"nothing wanted clears", Omit{Reasoning: OmitOtherModels, Items: []string{"a"}}, Omit{}, "{}", true},
		{"dropping the rule while keeping items is two entries", Omit{Reasoning: OmitOtherModels, Items: []string{"a"}}, Omit{Items: []string{"a"}}, "", false},
		{"dropping an item while keeping the rule is two entries", Omit{Reasoning: OmitOtherModels, Items: []string{"a", "b"}}, Omit{Reasoning: OmitOtherModels, Items: []string{"a"}}, "", false},
		{"a rule swapped for another in one delta", Omit{Reasoning: "x"}, rule, "reasoning=other_models", true},
		{"a rule the format does not define is no delta", Omit{}, Omit{Reasoning: "x"}, "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			settings := Settings{Omit: tt.have}
			d, ok := settings.OmitDelta(tt.want)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				if d != nil {
					t.Errorf("a delta %+v beside false", d)
				}
				return
			}
			got := "nil"
			switch {
			case d == nil:
			case d.IsZero():
				got = "{}"
			default:
				var parts []string
				if d.Reasoning != "" {
					parts = append(parts, "reasoning="+d.Reasoning)
				}
				if len(d.Items) > 0 {
					parts = append(parts, "items="+strings.Join(d.Items, ","))
				}
				got = strings.Join(parts, " ")
			}
			if got != tt.out {
				t.Errorf("delta %s, want %s", got, tt.out)
			}
			// Applied, the delta takes the settings to what was wanted.
			after := settings.Apply(&ConfigEntry{Omit: d})
			if d != nil && fmt.Sprint(after.Omit.Reasoning, sortedCopy(after.Omit.Items)) != fmt.Sprint(tt.want.Reasoning, sortedCopy(tt.want.Items)) {
				t.Errorf("after the delta %+v, want %+v", after.Omit, tt.want)
			}
		})
	}
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}

// TestOmitValidation: a writer writes no empty id in an items list.
func TestOmitValidation(t *testing.T) {
	s := New(Header{})
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Items: []string{"a", ""}}}); err == nil {
		t.Error("an empty id was appended")
	}
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{Reasoning: OmitOtherModels, Items: []string{"a"}}}); err != nil {
		t.Error(err)
	}
	if _, err := s.Append(&ConfigEntry{Omit: &Omit{}}); err != nil {
		t.Errorf("a clearing omit: %v", err)
	}
}

// TestContinueCarriesOmit: a session rolled over into a successor keeps
// the omit in force, as a compaction's checkpoint does, so a model
// switch in the successor is covered by the rule written before the
// rollover, and a response after it verifies.
func TestContinueCarriesOmit(t *testing.T) {
	ctx := t.Context()
	store := NewMemoryStore()
	old, err := store.Create(ctx, Header{})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		&ConfigEntry{Model: "a", Instructions: ptr("Be brief.")},
		NewItemEntry(openresponses.UserText("hi")),
		&ConfigEntry{Model: "b", Omit: &Omit{Reasoning: OmitOtherModels, Items: []string{"sha256:gone"}}},
	} {
		if _, err := store.Append(ctx, old.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	next, err := Continue(ctx, store, old.ID(), openresponses.UserText("summary"))
	if err != nil {
		t.Fatal(err)
	}
	cx, err := next.Context()
	if err != nil {
		t.Fatal(err)
	}
	if cx.Settings.Omit.Reasoning != OmitOtherModels || len(cx.Settings.Omit.Items) != 1 {
		t.Fatalf("the successor's omit is %+v", cx.Settings.Omit)
	}
	// Under b a response reasons; then the session switches to a, and
	// the request leaves b's reasoning out and hashes without it.
	appendEntry := func(e Entry) string {
		t.Helper()
		id, err := store.Append(ctx, next.ID(), e)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	appendEntry(NewItemEntry(openresponses.UserText("go on")))
	rs := NewItemEntry(&openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{}, EncryptedContent: "enc"})
	rs.ResponseID = "resp_1"
	appendEntry(rs)
	appendEntry(&ResponseEntry{ResponseID: "resp_1", Status: openresponses.ResponseStatusCompleted})
	appendEntry(&ConfigEntry{Model: "a"})
	appendEntry(NewItemEntry(openresponses.UserText("and again")))
	next, err = store.Open(ctx, next.ID())
	if err != nil {
		t.Fatal(err)
	}
	cx, err = next.Context()
	if err != nil {
		t.Fatal(err)
	}
	if len(cx.OmittedItems) != 1 || cx.OmittedItems[0].Reason != OmitOtherModels {
		t.Fatalf("the request leaves out %+v", cx.OmittedItems)
	}
	req, err := cx.Request()
	if err != nil {
		t.Fatal(err)
	}
	h, err := RequestHash(req)
	if err != nil {
		t.Fatal(err)
	}
	out := NewItemEntry(openresponses.AssistantText("ok"))
	out.ResponseID = "resp_2"
	appendEntry(out)
	id := appendEntry(&ResponseEntry{ResponseID: "resp_2", Status: openresponses.ResponseStatusCompleted, RequestHash: h})
	next, err = store.Open(ctx, next.ID())
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Verify(id); err != nil {
		t.Errorf("a response after a switch in the successor: %v", err)
	}
}
