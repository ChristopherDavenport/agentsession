package export

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

func TestImageMediaType(t *testing.T) {
	tests := map[string]string{
		"data:image/png;base64,AAA":     "image/png",
		"data:image/jpg;base64,AAA":     "image/jpeg",
		"data:image/svg+xml;base64,AAA": "",
		"data:image/heic;base64,AAA":    "",
		"https://x/y/shot.JPG?w=1":      "image/jpeg",
		"https://x/y/diagram.svg":       "",
		"https://x/y/file.tiff":         "",
		"https://x/y/img":               "image/png",
		"file_123":                      "image/png",
	}
	for in, want := range tests {
		if got := imageMediaType(in); got != want {
			t.Errorf("imageMediaType(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUnsupportedImageDegrades: an image Harbor's models refuse becomes
// a text placeholder, so the document still validates and loads.
func TestUnsupportedImageDegrades(t *testing.T) {
	s := agentsession.New(agentsession.Header{ID: "img"})
	msg := &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{
		&openresponses.InputText{Text: "see"},
		&openresponses.InputImage{ImageURL: "https://x/diagram.svg"},
	}}
	if _, err := s.Append(agentsession.NewItemEntry(msg)); err != nil {
		t.Fatal(err)
	}
	doc := exportOne(t, s, Options{})
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	text := doc.Steps[0].Message.Text
	if !strings.Contains(text, "[image: https://x/diagram.svg]") {
		t.Errorf("message = %q", text)
	}
}

// TestFoldUsageCounted: a compaction's usage reaches the step's extra
// and the document's totals, priced when a price source is given.
func TestFoldUsageCounted(t *testing.T) {
	s := agentsession.New(agentsession.Header{ID: "fold"})
	cfg := &agentsession.ConfigEntry{Model: "m"}
	if _, err := s.Append(cfg); err != nil {
		t.Fatal(err)
	}
	first, _ := s.Append(agentsession.NewItemEntry(openresponses.UserText("a")))
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	comp, err := s.Compact(first, openresponses.UserText("summary"))
	if err != nil {
		t.Fatal(err)
	}
	comp.Usage = &openresponses.Usage{InputTokens: 600, OutputTokens: 40, TotalTokens: 640}
	if _, err := s.Append(comp); err != nil {
		t.Fatal(err)
	}
	doc := exportOne(t, s, Options{Cost: func(model string, u openresponses.Usage) (float64, bool) {
		return float64(u.InputTokens+u.OutputTokens) / 1000, true
	}})
	if err := doc.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	var fold *atif.Step
	for i := range doc.Steps {
		if doc.Steps[i].Extra["first_kept"] != nil {
			fold = &doc.Steps[i]
		}
	}
	if fold == nil {
		t.Fatal("no compaction step")
	}
	if fold.Metrics != nil {
		t.Error("compaction step carries metrics, which ATIF forbids on a system step")
	}
	if fold.Extra["usage"] == nil || fold.Extra["cost_usd"] != 0.64 {
		t.Errorf("fold extra usage=%v cost=%v", fold.Extra["usage"], fold.Extra["cost_usd"])
	}
	fm := doc.FinalMetrics
	if fm == nil || *fm.TotalPromptTokens != 600 || *fm.TotalCompletionTokens != 40 || fm.TotalCostUSD == nil || *fm.TotalCostUSD != 0.64 {
		t.Errorf("final metrics = %+v", fm)
	}
}

// TestNoPassthrough strips the raw items: the document shrinks, still
// validates, and Items reports it has nothing to rebuild from.
func TestNoPassthrough(t *testing.T) {
	s := loadFixture(t, "basic")
	full := exportOne(t, s, Options{})
	lean := exportOne(t, s, Options{Redactors: []Redactor{NoPassthrough()}})
	if err := lean.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	fb, _ := json.Marshal(full)
	lb, _ := json.Marshal(lean)
	if len(lb) >= len(fb) {
		t.Errorf("lean document is %d bytes, full %d", len(lb), len(fb))
	}
	if strings.Contains(string(lb), `"`+ExtraOpenResponses+`":{"item"`) {
		t.Error("lean document still carries raw items")
	}
	if lean.Extra[ExtraOpenResponses] == nil {
		t.Error("root payload profile was stripped")
	}
	if _, err := Items(lean); !errors.Is(err, ErrNoRawItems) {
		t.Errorf("Items(lean) = %v, want ErrNoRawItems", err)
	}
	if _, err := Items(full); err != nil {
		t.Errorf("Items(full) = %v", err)
	}
	if len(lean.Steps) != len(full.Steps) || lean.Steps[1].ToolCalls == nil {
		t.Error("declared fields were lost")
	}
}

// exportOne converts the session's main trajectory.
func exportOne(t *testing.T, s *agentsession.Session, opts Options) *atif.Trajectory {
	t.Helper()
	for tr, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		if !tr.Main {
			continue
		}
		doc, err := ToATIF(tr, opts)
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	t.Fatal("no main trajectory")
	return nil
}

// TestItemsFrom rebuilds the basic fixture's conversation from a lean
// document and compares it, type by type and by the fields the
// declared ATIF members carry, with the lossless rebuild.
func TestItemsFrom(t *testing.T) {
	s := loadFixture(t, "basic")
	full := exportOne(t, s, Options{})
	lean := exportOne(t, s, Options{Redactors: []Redactor{NoPassthrough()}})
	want, err := Items(full)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ItemsFrom(lean)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("ItemsFrom gave %d items, Items gave %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ItemType() != want[i].ItemType() {
			t.Errorf("item %d: %s, want %s", i, got[i].ItemType(), want[i].ItemType())
			continue
		}
		switch w := want[i].(type) {
		case *openresponses.FunctionCall:
			g := got[i].(*openresponses.FunctionCall)
			if g.CallID != w.CallID || g.Name != w.Name || g.Arguments != w.Arguments {
				t.Errorf("call %d: %+v, want %+v", i, g, w)
			}
		case *openresponses.FunctionCallOutput:
			g := got[i].(*openresponses.FunctionCallOutput)
			if g.CallID != w.CallID || g.Output.String() != w.Output.String() {
				t.Errorf("output %d: %+v, want %+v", i, g, w)
			}
		case *openresponses.Message:
			g := got[i].(*openresponses.Message)
			if g.Role != w.Role || g.Content.Text() != w.Content.Text() {
				t.Errorf("message %d: %s %q, want %s %q", i, g.Role, g.Content.Text(), w.Role, w.Content.Text())
			}
		case *openresponses.ReasoningItem:
			g := got[i].(*openresponses.ReasoningItem)
			if g.Summary.Text() != w.Summary.Text() {
				t.Errorf("reasoning %d: %q, want %q", i, g.Summary.Text(), w.Summary.Text())
			}
		}
	}
	// A document from another producer, with an image and a non-object
	// argument, loads too.
	other := &atif.Trajectory{SchemaVersion: atif.SchemaVersion, Agent: atif.Agent{Name: "x", Version: "1"}, Steps: []atif.Step{
		{StepID: 1, Source: atif.SourceUser, Message: atif.Content{Parts: []atif.ContentPart{{Type: atif.PartText, Text: "look"}, {Type: atif.PartImage, Source: &atif.MediaSource{MediaType: "image/png", Path: "shot.png"}}}}},
		{StepID: 2, Source: atif.SourceAgent, Message: atif.Text(""), ToolCalls: []atif.ToolCall{{ToolCallID: "c", FunctionName: "f", Arguments: map[string]any{"_arguments": "raw"}}},
			Observation: &atif.Observation{Results: []atif.ObservationResult{{SourceCallID: "c", Content: atif.Text("r")}}}},
	}}
	items, err := ItemsFrom(other)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 {
		t.Fatalf("items = %d", len(items))
	}
	if m := items[0].(*openresponses.Message); len(m.Content) != 2 || m.Content[1].(*openresponses.InputImage).ImageURL != "shot.png" {
		t.Errorf("user message = %+v", m)
	}
	if fc := items[1].(*openresponses.FunctionCall); fc.Arguments != "raw" {
		t.Errorf("arguments = %q", fc.Arguments)
	}
}
