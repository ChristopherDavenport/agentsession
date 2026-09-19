package agentsession

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

func TestRecordResponse(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	s, err := store.Create(ctx, Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	req := openresponses.Request{
		Model:        "gpt-5",
		Instructions: "Be brief.",
		Tools:        openresponses.Tools{openresponses.NewFunctionTool("lookup", "d", json.RawMessage(`{"type":"object"}`))},
		Extra:        map[string]any{"temperature": 0.2},
	}
	cfg, err := ConfigFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, id, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, id, NewItemEntry(openresponses.UserText("What is 2+2?"))); err != nil {
		t.Fatal(err)
	}

	// The request as the harness sends it: the context plus store false.
	c, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	sent, err := c.Request()
	if err != nil {
		t.Fatal(err)
	}
	resp := &openresponses.Response{
		ID: "resp_1", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted,
		Usage: &openresponses.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		Output: openresponses.Items{
			&openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "easy"}}},
			&openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "lookup", Arguments: `{"q":"2+2"}`},
		},
	}
	respID, err := RecordResponse(ctx, store, id, sent, resp, 1500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 5 || s.Leaf() != respID {
		t.Fatalf("len %d leaf %s, want 5 %s", s.Len(), s.Leaf(), respID)
	}
	e, _ := s.Entry(respID)
	r := e.(*ResponseEntry)
	if r.ResponseID != "resp_1" || r.Model != "gpt-5" || r.Status != openresponses.ResponseStatusCompleted || r.Usage.TotalTokens != 15 || r.LatencyMS != 1500 || r.RequestHash == "" {
		t.Errorf("response entry = %+v", r)
	}
	// The items carry the response ID and the hash verifies.
	for _, oid := range s.Path(respID)[2:4] {
		item, ok := oid.(*ItemEntry)
		if !ok || item.ResponseID != "resp_1" {
			t.Errorf("output entry = %+v", oid)
		}
	}
	if err := s.Verify(respID); err != nil {
		t.Errorf("Verify: %v", err)
	}

	// A failed response with no output still records; no latency when
	// zero; errors from the store surface.
	failed := &openresponses.Response{ID: "resp_2", Status: openresponses.ResponseStatusFailed,
		Error: &openresponses.ErrorPayload{Type: openresponses.ErrorTypeServerError, Code: "boom", Message: "m"}}
	fid, err := RecordResponse(ctx, store, id, sent, failed, 0)
	if err != nil {
		t.Fatal(err)
	}
	e, _ = s.Entry(fid)
	if r := e.(*ResponseEntry); r.Error == nil || r.LatencyMS != 0 || r.Status != openresponses.ResponseStatusFailed {
		t.Errorf("failed entry = %+v", r)
	}
	if _, err := RecordResponse(ctx, store, "missing", sent, resp, 0); err == nil {
		t.Error("RecordResponse on a missing session succeeded")
	}
	if _, err := RecordResponse(ctx, store, id, sent, nil, 0); err == nil {
		t.Error("RecordResponse with a nil response succeeded")
	}
	if _, err := RecordResponse(ctx, store, id, sent, &openresponses.Response{ID: "x", Output: openresponses.Items{nil}}, 0); err == nil {
		t.Error("RecordResponse with a nil output item succeeded")
	}
}

// TestConfigFromRequestRoundTrip replays the config built from a request
// and rebuilds the request; the hash must match for every hash vector
// and for a request that sets most named members.
func TestConfigFromRequestRoundTrip(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "hash", "vectors.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []hashVector
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	strict := true
	full := openresponses.Request{
		Model:             "gpt-5",
		Instructions:      "i",
		Input:             openresponses.Items{openresponses.UserText("hi")},
		Tools:             openresponses.Tools{openresponses.NewFunctionTool("f", "d", nil)},
		ToolChoice:        openresponses.ToolChoiceFunction("f"),
		Metadata:          map[string]string{"k": "v"},
		Text:              openresponses.TextConfig{Format: openresponses.JSONSchemaFormat("out", json.RawMessage(`{"type":"object"}`), true)},
		Temperature:       ptr(0.5),
		TopP:              ptr(0.9),
		PresencePenalty:   ptr(0.1),
		FrequencyPenalty:  ptr(0.2),
		ParallelToolCalls: &strict,
		MaxOutputTokens:   ptr(100),
		MaxToolCalls:      ptr(3),
		Reasoning:         openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortHigh, Summary: openresponses.ReasoningSummaryAuto},
		SafetyIdentifier:  "user-1",
		PromptCacheKey:    "cache-1",
		Truncation:        openresponses.TruncationAuto,
		Store:             ptr(false),
		ServiceTier:       openresponses.ServiceTierDefault,
		TopLogprobs:       ptr(2),
		Include:           []openresponses.Include{openresponses.IncludeReasoningEncryptedContent},
		Extra:             map[string]any{"acme:region": "eu"},
	}
	fullJSON, _ := json.Marshal(full)
	cases := []hashVector{{Name: "every named member", Request: fullJSON}}
	cases = append(cases, vectors...)
	for _, v := range cases {
		t.Run(v.Name, func(t *testing.T) {
			var req openresponses.Request
			if err := json.Unmarshal(v.Request, &req); err != nil {
				t.Fatal(err)
			}
			want, err := RequestHash(req)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ConfigFromRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			if !cfg.Replace {
				t.Error("Replace not set")
			}
			for _, k := range []string{"input", "store", "previous_response_id", "stream", "model", "tools", "instructions", "reasoning", "text"} {
				if _, ok := cfg.Extra[k]; ok {
					t.Errorf("extra carries %s", k)
				}
			}
			// Through the wire form, as a reader would see it.
			line, err := MarshalEntry(&ConfigEntry{EntryBase: EntryBase{ID: "c", Timestamp: fixedTime}, Model: cfg.Model,
				Instructions: cfg.Instructions, Reasoning: cfg.Reasoning, Text: cfg.Text, ToolsAdded: cfg.ToolsAdded, Extra: cfg.Extra, Replace: cfg.Replace})
			if err != nil {
				t.Fatal(err)
			}
			back, err := UnmarshalEntry(line)
			if err != nil {
				t.Fatal(err)
			}
			settings := Settings{Model: "stale", Extra: map[string]json.RawMessage{"temperature": json.RawMessage("9")}}.Apply(back.(*ConfigEntry))
			rebuilt, err := settings.Request(req.Input)
			if err != nil {
				t.Fatal(err)
			}
			got, err := RequestHash(rebuilt)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				a, _ := json.Marshal(req)
				b, _ := json.Marshal(rebuilt)
				t.Errorf("hash %s != %s\nsent    %s\nrebuilt %s", got, want, a, b)
			}
		})
	}
	// Named fields land where Settings expects them.
	cfg, err := ConfigFromRequest(full)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "gpt-5" || *cfg.Instructions != "i" || cfg.Reasoning.Effort != openresponses.ReasoningEffortHigh || cfg.Text.Format.Name != "out" || len(cfg.ToolsAdded) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	if string(cfg.Extra["temperature"]) != "0.5" || string(cfg.Extra["acme:region"]) != `"eu"` || string(cfg.Extra["tool_choice"]) != `{"type":"function","name":"f"}` {
		t.Errorf("extra = %v", cfg.Extra)
	}
	empty, err := ConfigFromRequest(openresponses.Request{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(empty, &ConfigEntry{Replace: true}) {
		t.Errorf("empty config = %+v", empty)
	}
}
