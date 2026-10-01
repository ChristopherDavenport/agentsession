package export

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// TestLogprobs: metrics.logprobs holds the output_text logprobs of a
// step, in item and then part order, only when they cover every
// completion token; otherwise it is left out (#176).
func TestLogprobs(t *testing.T) {
	text := func(s string, lps ...float64) *openresponses.OutputText {
		p := &openresponses.OutputText{Text: s, Annotations: []openresponses.Annotation{}}
		for _, lp := range lps {
			p.Logprobs = append(p.Logprobs, openresponses.LogProb{Token: "t", Logprob: lp})
		}
		return p
	}
	msg := func(parts ...openresponses.Content) openresponses.Item {
		return &openresponses.Message{Status: "completed", Role: openresponses.RoleAssistant, Content: parts}
	}
	usage := func(reasoning int) *openresponses.Usage {
		return &openresponses.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5, OutputTokensDetails: openresponses.OutputTokensDetails{ReasoningTokens: reasoning}}
	}
	call := &openresponses.FunctionCall{CallID: "c1", Name: "f", Arguments: "{}", Status: "completed"}
	for _, tc := range []struct {
		name  string
		items []openresponses.Item
		usage *openresponses.Usage
		want  []float64
	}{
		{"text only", []openresponses.Item{msg(text("hi there", -0.1, -0.2))}, usage(0), []float64{-0.1, -0.2}},
		{"no usage", []openresponses.Item{msg(text("hi", -0.5))}, nil, []float64{-0.5}},
		{"two messages", []openresponses.Item{msg(text("a", -1), text("b", -2)), msg(text("c", -3))}, usage(0), []float64{-1, -2, -3}},
		{"function call", []openresponses.Item{msg(text("hi", -0.1)), call}, usage(0), nil},
		{"message lacking logprobs", []openresponses.Item{msg(text("a", -1)), msg(text("b"))}, usage(0), nil},
		{"refusal", []openresponses.Item{msg(text("a", -1), &openresponses.Refusal{Refusal: "no"})}, usage(0), nil},
		{"reasoning tokens", []openresponses.Item{msg(text("a", -1))}, usage(4), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := agentsession.New(agentsession.Header{ID: "logprobs"})
			mustAppend(t, s, &agentsession.ConfigEntry{Model: "m"})
			mustAppend(t, s, agentsession.NewItemEntry(openresponses.UserText("q")))
			for _, it := range tc.items {
				e := agentsession.NewItemEntry(it)
				e.ResponseID = "resp_1"
				mustAppend(t, s, e)
			}
			mustAppend(t, s, &agentsession.ResponseEntry{ResponseID: "resp_1", Status: "completed", Usage: tc.usage})
			var buf bytes.Buffer
			if err := agentsession.Write(&buf, s); err != nil {
				t.Fatal(err)
			}
			back, err := agentsession.Read(&buf)
			if err != nil {
				t.Fatal(err)
			}
			tr, err := At(back, back.Leaf())
			if err != nil {
				t.Fatal(err)
			}
			var steps []atif.Step
			for _, step := range mustDoc(t, tr).Steps {
				if step.Source == atif.SourceAgent {
					steps = append(steps, step)
				}
			}
			if len(steps) != 1 {
				t.Fatalf("%d agent steps, want 1", len(steps))
			}
			m := steps[0].Metrics
			var got []float64
			if m != nil {
				got = m.Logprobs
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("logprobs = %v, want %v", got, tc.want)
			}
			if tc.usage == nil && m != nil && (m.PromptTokens != nil || m.CompletionTokens != nil) {
				t.Errorf("metrics without usage carry token counts: %+v", m)
			}
		})
	}
}
