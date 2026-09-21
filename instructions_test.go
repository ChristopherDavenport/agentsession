package agentsession

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// composed is the four layers the composition study wired into one
// product, at the sizes it measured: a product prompt, an AGENTS.md
// chain, a skill catalogue and a memory block.
func composed() []InstructionPart {
	return []InstructionPart{
		{ID: "product", Source: "product", Text: strings.Repeat("p", 92)},
		{ID: "agentsmd", Source: "agentsmd", Text: strings.Repeat("a", 438)},
		{ID: "agentskill", Source: "agentskill", Text: strings.Repeat("s", 837)},
		{ID: "agentmemory", Source: "agentmemory", Text: strings.Repeat("m", 1012)},
	}
}

func edit(parts []InstructionPart, id string, text string) []InstructionPart {
	out := append([]InstructionPart(nil), parts...)
	for i := range out {
		if out[i].ID == id {
			out[i].Text = text
		}
	}
	return out
}

func partByID(parts []InstructionPart, id string) InstructionPart {
	for _, p := range parts {
		if p.ID == id {
			return p
		}
	}
	return InstructionPart{}
}

// TestInstructionsDeltaCostsOnePart is the measurement three studies
// filed: every change to any layer used to rewrite every layer into
// the path. A delta now carries the text of the part that moved and
// one id and one hash for each part that did not, so what it costs is
// proportional to the change.
func TestInstructionsDeltaCostsOnePart(t *testing.T) {
	base := composed()
	full, err := ConfigFromRequestParts(openresponses.Request{Model: "gpt-5", Instructions: JoinInstructions(base)}, base...)
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{}.Apply(full)
	if settings.Instructions != JoinInstructions(base) {
		t.Fatalf("the full config does not rebuild the instructions")
	}
	whole := len(settings.Instructions)

	tests := []struct {
		name, part string
		text       string
	}{
		{"ten bytes of one memory entry", "agentmemory", strings.Repeat("m", 1022)},
		{"one skill added to the catalogue", "agentskill", strings.Repeat("s", 1052)},
		{"twenty-seven bytes appended to a file in the chain", "agentsmd", strings.Repeat("a", 465)},
		{"the product prompt reworded", "product", strings.Repeat("q", 92)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := edit(base, tt.part, tt.text)
			delta := settings.InstructionsDelta(next)
			if delta == nil {
				t.Fatal("no delta for a changed part")
			}
			line, err := MarshalEntry(withEnvelope(delta))
			if err != nil {
				t.Fatal(err)
			}
			// The entry carries the changed part once, and a name and
			// a hash for each part that did not change.
			if budget := len(tt.text) + 200*(len(next)-1); len(line) > budget {
				t.Errorf("a change to %s wrote %d bytes, want at most %d", tt.part, len(line), budget)
			}
			// What the same change cost before: the whole block.
			joined := JoinInstructions(next)
			before, err := MarshalEntry(withEnvelope(&ConfigEntry{Instructions: &joined}))
			if err != nil {
				t.Fatal(err)
			}
			if len(line) >= len(before) {
				t.Errorf("a change to %s wrote %d bytes, against %d for the whole %d byte block",
					tt.part, len(line), len(before), whole)
			}
			// Only the changed part carries text.
			for _, p := range delta.InstructionsParts {
				if p.ID == tt.part {
					if p.Text != tt.text || p.Hash != "" {
						t.Errorf("the changed part carries text %d, hash %q", len(p.Text), p.Hash)
					}
					continue
				}
				if p.Text != "" {
					t.Errorf("part %s repeats %d bytes of unchanged text", p.ID, len(p.Text))
				}
				if p.Hash != HashText(partByID(base, p.ID).Text) {
					t.Errorf("part %s carries hash %q", p.ID, p.Hash)
				}
			}
			// And the delta rebuilds the whole prompt.
			got := settings.Apply(delta)
			if got.Instructions != JoinInstructions(next) {
				t.Errorf("replaying the delta gives %d bytes, want %d", len(got.Instructions), len(JoinInstructions(next)))
			}
			if len(got.InstructionsParts) != len(next) {
				t.Fatalf("%d parts in force, want %d", len(got.InstructionsParts), len(next))
			}
			for i, p := range got.InstructionsParts {
				if p.ID != next[i].ID || p.Text != next[i].Text || p.Source != next[i].Source {
					t.Errorf("part %d = %+v, want %+v", i, p, next[i])
				}
				if p.Hash != "" {
					t.Errorf("a part in force carries a hash: %+v", p)
				}
			}
		})
	}
}

// TestInstructionsDeltaMemoryBlock is the letta-memory measurement: a
// memory block of eight entries just under the entry bound rendered
// 32,392 bytes, and a memory_save of 13 bytes of content wrote a
// config delta of 33,440. With one part per entry the same save costs
// the entry it changed.
func TestInstructionsDeltaMemoryBlock(t *testing.T) {
	const entries, entryBytes = 8, 4049 // 32,392 bytes of block
	base := []InstructionPart{{ID: "product", Source: "product", Text: strings.Repeat("p", 92)}}
	for i := range entries {
		base = append(base, InstructionPart{
			ID:     "memory/" + string(rune('a'+i)),
			Source: "agentmemory",
			Text:   strings.Repeat("m", entryBytes),
		})
	}
	full, err := ConfigFromRequestParts(openresponses.Request{Instructions: JoinInstructions(base)}, base...)
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{}.Apply(full)
	block := len(settings.Instructions)
	if block < 32000 {
		t.Fatalf("the block is %d bytes", block)
	}

	next := edit(base, "memory/c", strings.Repeat("m", entryBytes+13))
	delta := settings.InstructionsDelta(next)
	if delta == nil {
		t.Fatal("no delta for a saved memory")
	}
	line, err := MarshalEntry(withEnvelope(delta))
	if err != nil {
		t.Fatal(err)
	}
	joined := JoinInstructions(next)
	before, err := MarshalEntry(withEnvelope(&ConfigEntry{Instructions: &joined}))
	if err != nil {
		t.Fatal(err)
	}
	if len(line) > entryBytes+2048 {
		t.Errorf("a 13 byte save wrote %d bytes for an entry of %d", len(line), entryBytes)
	}
	if len(before) < block {
		t.Fatalf("the string delta is %d bytes for a %d byte block", len(before), block)
	}
	if len(line)*4 > len(before) {
		t.Errorf("a 13 byte save wrote %d bytes against %d for the whole block", len(line), len(before))
	}
	t.Logf("a 13 byte save: %d bytes as parts, %d bytes as one string", len(line), len(before))
	if got := settings.Apply(delta); got.Instructions != joined {
		t.Errorf("the delta rebuilds %d bytes, want %d", len(got.Instructions), len(joined))
	}
}

// TestInstructionsDeltaShapes covers what a delta does to the list
// beyond changing one part's text.
func TestInstructionsDeltaShapes(t *testing.T) {
	base := composed()
	full, err := ConfigFromRequestParts(openresponses.Request{Instructions: JoinInstructions(base)}, base...)
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{}.Apply(full)

	t.Run("a part whose source moved carries its text", func(t *testing.T) {
		// A part named by its hash inherits the source it had, so the
		// hash form cannot say that a part has no source now.
		for _, next := range [][]InstructionPart{
			edit(base, "agentsmd", base[1].Text), // unchanged text, source below
			composed(),
		} {
			next[1].Source = ""
			delta := settings.InstructionsDelta(next)
			if delta == nil {
				t.Fatal("no delta for a cleared source")
			}
			got := settings.Apply(delta)
			if len(got.InstructionsParts) != len(next) {
				t.Fatalf("%d parts in force", len(got.InstructionsParts))
			}
			for i, p := range got.InstructionsParts {
				if p.ID != next[i].ID || p.Text != next[i].Text || p.Source != next[i].Source {
					t.Errorf("part %d = %+v, want %+v", i, p, next[i])
				}
			}
			if got.Instructions != JoinInstructions(next) {
				t.Error("the instructions are not the parts joined")
			}
		}
	})
	t.Run("a new source alone still replays", func(t *testing.T) {
		next := composed()
		next[0].Source = "harness"
		got := settings.Apply(settings.InstructionsDelta(next))
		if got.InstructionsParts[0].Source != "harness" {
			t.Errorf("source = %q", got.InstructionsParts[0].Source)
		}
	})
	t.Run("an unchanged render writes nothing", func(t *testing.T) {
		if delta := settings.InstructionsDelta(composed()); delta != nil {
			t.Errorf("delta for an unchanged render = %+v", delta.InstructionsParts)
		}
	})
	t.Run("a part that leaves the list is removed", func(t *testing.T) {
		next := []InstructionPart{base[0], base[1], base[3]}
		delta := settings.InstructionsDelta(next)
		if delta == nil {
			t.Fatal("no delta")
		}
		got := settings.Apply(delta)
		if len(got.InstructionsParts) != 3 {
			t.Fatalf("%d parts in force", len(got.InstructionsParts))
		}
		if strings.Contains(got.Instructions, "s") {
			t.Error("the removed part is still in the instructions")
		}
		if got.Instructions != JoinInstructions(next) {
			t.Error("the instructions are not the remaining parts joined")
		}
	})
	t.Run("order is explicit in every delta", func(t *testing.T) {
		next := []InstructionPart{base[3], base[2], base[1], base[0]}
		delta := settings.InstructionsDelta(next)
		if delta == nil {
			t.Fatal("no delta for a reordering")
		}
		for _, p := range delta.InstructionsParts {
			if p.Text != "" {
				t.Errorf("a reordering repeats the text of %s", p.ID)
			}
		}
		got := settings.Apply(delta)
		if got.Instructions != JoinInstructions(next) {
			t.Error("the reordered instructions are wrong")
		}
	})
	t.Run("a new part costs its text alone", func(t *testing.T) {
		next := append(composed(), InstructionPart{ID: "scratch", Source: "product", Text: "remember the ticket number"})
		delta := settings.InstructionsDelta(next)
		if delta == nil {
			t.Fatal("no delta")
		}
		got := settings.Apply(delta)
		if got.Instructions != JoinInstructions(next) {
			t.Error("the new part is not in the instructions")
		}
	})
	t.Run("a string replaces the composition", func(t *testing.T) {
		one := "Be brief."
		got := settings.Apply(&ConfigEntry{Instructions: &one})
		if got.Instructions != one {
			t.Errorf("instructions = %q", got.Instructions)
		}
		if got.InstructionsParts != nil {
			t.Errorf("%d parts survive a string that replaced them", len(got.InstructionsParts))
		}
	})
	t.Run("the delta from a string is the whole composition", func(t *testing.T) {
		one := "Be brief."
		plain := settings.Apply(&ConfigEntry{Instructions: &one})
		delta := plain.InstructionsDelta(composed())
		if delta == nil {
			t.Fatal("no delta")
		}
		for _, p := range delta.InstructionsParts {
			if p.Text == "" {
				t.Errorf("part %s is named as unchanged against a string", p.ID)
			}
		}
	})
	t.Run("an empty list clears the instructions", func(t *testing.T) {
		delta := settings.InstructionsDelta(nil)
		if delta == nil {
			t.Fatal("no delta")
		}
		got := settings.Apply(delta)
		if got.Instructions != "" || got.InstructionsParts != nil {
			t.Errorf("settings after clearing = %+v", got)
		}
		if (Settings{}).InstructionsDelta(nil) != nil {
			t.Error("clearing nothing writes an entry")
		}
	})
	t.Run("a replace resolves no hash against what it discarded", func(t *testing.T) {
		unresolved := &ConfigEntry{
			Replace:           true,
			Model:             "gpt-5-nano",
			InstructionsParts: []InstructionPart{{ID: "product", Hash: HashText(base[0].Text)}},
		}
		got := settings.Apply(unresolved)
		if len(got.InstructionsParts) != 1 {
			t.Fatalf("%d parts after a replace", len(got.InstructionsParts))
		}
		if got.InstructionsParts[0].Text != "" || got.Instructions != "" {
			t.Errorf("a replaced part kept %d bytes of discarded text", len(got.InstructionsParts[0].Text))
		}
		if got.InstructionsParts[0].Hash == "" {
			t.Error("the unresolved part lost the hash that says its text is missing")
		}
		// A reader replays such an entry, since it may be in a file it
		// did not write; a writer is refused it.
		s := New(Header{})
		if _, err := s.Append(unresolved); err == nil {
			t.Error("Append took a replacing config whose part nothing can resolve")
		}
		// With the string beside them the entry stands, and the string
		// is what the request was sent with.
		whole := JoinInstructions(base)
		withString := &ConfigEntry{
			Replace:           true,
			Instructions:      &whole,
			InstructionsParts: []InstructionPart{{ID: "product", Hash: HashText(base[0].Text)}},
		}
		if _, err := s.Append(withString); err != nil {
			t.Errorf("Append = %v", err)
		}
		if got := settings.Apply(withString); got.Instructions != whole {
			t.Errorf("instructions = %d bytes, want the string the entry carried", len(got.Instructions))
		}
	})
	t.Run("replace drops the parts", func(t *testing.T) {
		got := settings.Apply(&ConfigEntry{Replace: true, Model: "gpt-5-nano"})
		if got.Instructions != "" || got.InstructionsParts != nil {
			t.Errorf("settings after replace = %+v", got)
		}
	})
}

// TestInstructionsPartsValidation holds a writer to the rules the
// format states, on the way in rather than at the far end of a file.
func TestInstructionsPartsValidation(t *testing.T) {
	joined := "one\n\ntwo"
	tests := []struct {
		name  string
		entry *ConfigEntry
		ok    bool
	}{
		{"parts and the string they join to", &ConfigEntry{
			Instructions:      &joined,
			InstructionsParts: []InstructionPart{{ID: "a", Text: "one"}, {ID: "b", Text: "two"}},
		}, true},
		{"parts alone", &ConfigEntry{
			InstructionsParts: []InstructionPart{{ID: "a", Text: "one"}},
		}, true},
		{"a delta beside a string it cannot be checked against", &ConfigEntry{
			Instructions:      &joined,
			InstructionsParts: []InstructionPart{{ID: "a", Hash: HashText("one")}, {ID: "b", Text: "two"}},
		}, true},
		{"a part with no id", &ConfigEntry{
			InstructionsParts: []InstructionPart{{Text: "one"}},
		}, false},
		{"a part named twice", &ConfigEntry{
			InstructionsParts: []InstructionPart{{ID: "a", Text: "one"}, {ID: "a", Text: "two"}},
		}, false},
		{"a string that is not the join", &ConfigEntry{
			Instructions:      &joined,
			InstructionsParts: []InstructionPart{{ID: "a", Text: "one"}, {ID: "b", Text: "three"}},
		}, false},
		{"an omitted part with no id", &ConfigEntry{
			InstructionsOmitted: []OmittedPart{{Reason: "budget"}},
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Header{})
			_, err := s.Append(tt.entry)
			if tt.ok && err != nil {
				t.Errorf("Append = %v", err)
			}
			if !tt.ok && err == nil {
				t.Error("Append accepted it")
			}
		})
	}
}

// TestInstructionsOmitted: the parts a writer considered and left out
// are on the record, which is where agentsmd's Omitted and
// agentmemory's manifest go.
func TestInstructionsOmitted(t *testing.T) {
	s := New(Header{})
	cfg := &ConfigEntry{
		InstructionsParts: []InstructionPart{{ID: "agentsmd", Source: "agentsmd", Text: "root conventions"}},
		InstructionsOmitted: []OmittedPart{
			{ID: "service/AGENTS.md", Reason: "budget", Size: 4096, Source: "agentsmd"},
			{ID: "service/CLAUDE.md", Reason: "shadowed", Size: 512, Source: "agentsmd"},
		},
	}
	if _, err := s.Append(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	omitted := ctx.InstructionsOmitted()
	if len(omitted) != 2 || omitted[0].ID != "service/AGENTS.md" || omitted[0].Reason != "budget" || omitted[0].Size != 4096 {
		t.Fatalf("omitted = %+v", omitted)
	}
	// Nothing omitted reaches the request.
	req, err := ctx.Request()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(req.Instructions, "service") {
		t.Errorf("an omitted part reached the instructions: %q", req.Instructions)
	}
}

// TestConfigFromRequestParts: the parts must be what the request
// carried, since the hash covers the request.
func TestConfigFromRequestParts(t *testing.T) {
	parts := composed()
	if _, err := ConfigFromRequestParts(openresponses.Request{Instructions: "something else"}, parts...); err == nil {
		t.Error("parts that do not join to the request's instructions were accepted")
	}
	cfg, err := ConfigFromRequestParts(openresponses.Request{Model: "gpt-5", Instructions: JoinInstructions(parts)}, parts...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instructions != nil {
		t.Error("the full config repeats the joined string beside the parts")
	}
	if !cfg.Replace {
		t.Error("the full config does not stand alone")
	}
	settings := Settings{}.Apply(cfg)
	req, err := settings.Request(nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Instructions != JoinInstructions(parts) {
		t.Error("the rebuilt request does not carry the instructions that were sent")
	}
}

// withEnvelope fills the envelope a marshalled entry needs.
func withEnvelope(e Entry) Entry {
	b := e.Base()
	b.ID = "c0000002"
	b.Parent = "c0000001"
	b.Timestamp = fixedTime
	return e
}

// TestSettingsDoNotShareParts: Settings is a value, and two of them
// must not share one array; a Settings taken from a compaction
// checkpoint would otherwise alias an appended entry's slice, which
// nothing may modify.
func TestSettingsDoNotShareParts(t *testing.T) {
	base := composed()
	full, err := ConfigFromRequestParts(openresponses.Request{Instructions: JoinInstructions(base)}, base...)
	if err != nil {
		t.Fatal(err)
	}
	settings := Settings{}.Apply(full)
	other := settings.Apply(&ConfigEntry{Model: "gpt-5-nano"})
	other.InstructionsParts[0].Text = "mutated"
	if settings.InstructionsParts[0].Text == "mutated" {
		t.Error("two Settings share one parts array")
	}
	if full.InstructionsParts[0].Text == "mutated" {
		t.Error("the settings alias the config entry's parts")
	}

	// The same through a compaction checkpoint, which is the entry a
	// context is built from.
	s := New(Header{})
	if _, err := s.Append(full); err != nil {
		t.Fatal(err)
	}
	first, err := s.Append(NewItemEntry(openresponses.UserText("one")))
	if err != nil {
		t.Fatal(err)
	}
	comp, err := s.Compact(first, openresponses.AssistantText("so far"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	ctx.Settings.InstructionsParts[0].Text = "mutated"
	if comp.Config.InstructionsParts[0].Text == "mutated" {
		t.Error("the context aliases the compaction entry's parts")
	}
}

// TestInstructionsPartsThroughCompaction: the compaction checkpoint
// carries the parts in force, so a delta after the fold that names a
// part by hash alone still resolves and the rebuilt request is the
// one that was sent.
func TestInstructionsPartsThroughCompaction(t *testing.T) {
	base := composed()
	s := New(Header{})
	full, err := ConfigFromRequestParts(openresponses.Request{Model: "gpt-5", Instructions: JoinInstructions(base)}, base...)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(full); err != nil {
		t.Fatal(err)
	}
	first, err := s.Append(NewItemEntry(openresponses.UserText("one")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(openresponses.AssistantText("two"))); err != nil {
		t.Fatal(err)
	}
	comp, err := s.Compact(first, openresponses.AssistantText("so far: one and two"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(comp.Config.InstructionsParts); got != len(base) {
		t.Fatalf("the checkpoint carries %d parts, want %d", got, len(base))
	}
	for i, p := range comp.Config.InstructionsParts {
		if p.Text != base[i].Text || p.Hash != "" {
			t.Errorf("checkpoint part %s carries %d bytes and hash %q", p.ID, len(p.Text), p.Hash)
		}
	}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	// A memory edit after the fold names the other three by hash.
	ctx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	next := edit(base, "agentmemory", strings.Repeat("m", 1022))
	delta := ctx.Settings.InstructionsDelta(next)
	if delta == nil {
		t.Fatal("no delta after a compaction")
	}
	if _, err := s.Append(delta); err != nil {
		t.Fatal(err)
	}
	ctx, err = s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Settings.Instructions != JoinInstructions(next) {
		t.Errorf("the instructions after the fold are %d bytes, want %d",
			len(ctx.Settings.Instructions), len(JoinInstructions(next)))
	}
	for _, p := range ctx.Settings.InstructionsParts {
		if p.Text == "" {
			t.Errorf("part %s lost its text across the fold", p.ID)
		}
	}
	// And the checkpoint survives a trip through the file form.
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	back, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	again, err := back.Context()
	if err != nil {
		t.Fatal(err)
	}
	if again.Settings.Instructions != ctx.Settings.Instructions {
		t.Errorf("the instructions changed on a round trip")
	}
}
