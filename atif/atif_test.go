package atif

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// assertSameJSON compares two JSON documents structurally.
func assertSameJSON(t *testing.T, want, got []byte) {
	t.Helper()
	var w, g any
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want is not JSON: %v", err)
	}
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got is not JSON: %v\n%s", err, got)
	}
	if !reflect.DeepEqual(w, g) {
		t.Errorf("documents differ\nwant: %s\ngot:  %s", want, got)
	}
}

// TestRoundTripFixtures decodes the RFC's own example (v1.5) and a
// Harbor golden trajectory (v1.8) and re-encodes them. The output must
// be the same document, so nothing the spec defines is dropped or
// invented, and both must validate.
func TestRoundTripFixtures(t *testing.T) {
	for _, name := range []string{"rfc-example", "harbor-summarization"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "testdata", "atif", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			tr, err := Parse(data)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if tr.HasUnknown() {
				t.Error("fixture reported unknown members")
			}
			out, err := json.Marshal(tr)
			if err != nil {
				t.Fatal(err)
			}
			assertSameJSON(t, data, out)
		})
	}
}

func TestRFCExampleShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "atif", "rfc-example.json"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if tr.SchemaVersion != "ATIF-v1.5" || tr.Agent.ModelName != "gemini-2.5-flash" || len(tr.Agent.ToolDefinitions) != 1 {
		t.Errorf("root = %+v", tr)
	}
	if len(tr.Steps) != 3 || tr.Steps[1].Source != SourceAgent || len(tr.Steps[1].ToolCalls) != 2 {
		t.Fatalf("steps = %+v", tr.Steps)
	}
	s := tr.Steps[1]
	if s.ReasoningEffort != "medium" || s.Message.String() != "I will search for the current trading price and volume for GOOGL." {
		t.Errorf("step 2 = %+v", s)
	}
	if s.ToolCalls[0].Arguments["ticker"] != "GOOGL" || s.Observation.Results[1].SourceCallID != "call_volume_2" {
		t.Errorf("calls = %+v obs = %+v", s.ToolCalls, s.Observation)
	}
	if *s.Metrics.PromptTokens != 520 || *s.Metrics.CostUSD != 0.00045 {
		t.Errorf("metrics = %+v", s.Metrics)
	}
	if len(tr.Steps[2].Metrics.CompletionTokenIDs) != 37 || len(tr.Steps[2].Metrics.Logprobs) != 44 {
		t.Errorf("token ids %d logprobs %d", len(tr.Steps[2].Metrics.CompletionTokenIDs), len(tr.Steps[2].Metrics.Logprobs))
	}
	if *tr.FinalMetrics.TotalSteps != 3 || *tr.FinalMetrics.TotalCostUSD != 0.00078 {
		t.Errorf("final = %+v", tr.FinalMetrics)
	}
	if tr.HasMultimodalContent() {
		t.Error("text-only document reported multimodal")
	}
}

func TestHarborGoldenShape(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "atif", "harbor-summarization.json"))
	if err != nil {
		t.Fatal(err)
	}
	tr, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if tr.SchemaVersion != SchemaVersion {
		t.Errorf("schema = %s", tr.SchemaVersion)
	}
	boundary := tr.Steps[4]
	if boundary.Source != SourceSystem || boundary.Extra["context_management"] == nil {
		t.Errorf("boundary step = %+v", boundary)
	}
	refs := boundary.Observation.Results[0].SubagentTrajectoryRef
	if len(refs) != 3 || refs[0].TrajectoryPath == "" || refs[0].Extra["summary"] == nil {
		t.Errorf("refs = %+v", refs)
	}
}

func TestUnknownMembersRoundTrip(t *testing.T) {
	in := `{"schema_version":"ATIF-v1.9","future_root":1,"agent":{"name":"a","version":"1","future_agent":true},
	"steps":[{"step_id":1,"source":"user","message":[{"type":"text","text":"hi","future_part":[1]}],"future_step":"x",
	"observation":{"results":[{"content":"r","future_result":{}}],"future_obs":null}},
	{"step_id":2,"source":"agent","message":"","tool_calls":[{"tool_call_id":"c","function_name":"f","arguments":{},"future_call":2}],
	"observation":{"results":[{"source_call_id":"c","content":[{"type":"image","source":{"media_type":"image/png","path":"p.png","future_src":1}}],"subagent_trajectory_ref":[{"trajectory_path":"x.json","future_ref":1}]}]},
	"metrics":{"prompt_tokens":1,"future_metric":2}}],
	"final_metrics":{"total_steps":2,"future_final":3}}`
	tr, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if !tr.HasUnknown() {
		t.Error("HasUnknown = false")
	}
	if !tr.HasMultimodalContent() {
		t.Error("HasMultimodalContent = false")
	}
	out, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	assertSameJSON(t, []byte(in), out)
	for _, key := range []string{"future_root", "future_agent", "future_part", "future_step", "future_result", "future_obs", "future_call", "future_src", "future_ref", "future_metric", "future_final"} {
		if !strings.Contains(string(out), `"`+key+`"`) {
			t.Errorf("%s dropped from %s", key, out)
		}
	}
	clean, err := Parse([]byte(`{"schema_version":"ATIF-v1.8","agent":{"name":"a","version":"1"},"steps":[{"step_id":1,"source":"user","message":"m"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if clean.HasUnknown() {
		t.Error("clean document reported unknown members")
	}
}

func TestContentEncoding(t *testing.T) {
	tests := []struct {
		name string
		c    Content
		want string
	}{
		{"text", Text("hi"), `"hi"`},
		{"empty text", Content{}, `""`},
		{"parts", Content{Parts: []ContentPart{{Type: PartText, Text: "a"}, {Type: PartImage, Source: &MediaSource{MediaType: "image/png", Path: "i.png"}}}}, `[{"type":"text","text":"a"},{"type":"image","source":{"media_type":"image/png","path":"i.png"}}]`},
		{"empty parts", Content{Parts: []ContentPart{}}, `[]`},
		{"audio with duration", Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: "audio/wav", Path: "a.wav", DurationSec: Ptr(1.5)}}}}, `[{"type":"audio","source":{"media_type":"audio/wav","path":"a.wav","duration_sec":1.5}}]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := json.Marshal(tt.c)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Errorf("Marshal = %s, want %s", got, tt.want)
			}
			var back Content
			if err := json.Unmarshal(got, &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tt.c) {
				t.Errorf("round trip = %+v, want %+v", back, tt.c)
			}
		})
	}
	var c Content
	if err := json.Unmarshal([]byte(`null`), &c); err != nil || !c.IsZero() {
		t.Errorf("null content = %+v, %v", c, err)
	}
	if err := json.Unmarshal([]byte(`3`), &c); err == nil {
		t.Error("number accepted as content")
	}
	multi := Content{Parts: []ContentPart{{Type: PartText, Text: "a"}, {Type: PartImage, Source: &MediaSource{}}, {Type: PartText, Text: "b"}}}
	if multi.String() != "ab" {
		t.Errorf("String = %q", multi.String())
	}
	// An optional content is omitted when zero and a required one is not.
	r := ObservationResult{SourceCallID: "c"}
	out, _ := json.Marshal(r)
	if string(out) != `{"source_call_id":"c"}` {
		t.Errorf("zero content emitted: %s", out)
	}
	s := Step{StepID: 1, Source: SourceUser}
	out, _ = json.Marshal(s)
	if string(out) != `{"step_id":1,"source":"user","message":""}` {
		t.Errorf("step = %s", out)
	}
	tc := ToolCall{ToolCallID: "c", FunctionName: "f"}
	out, _ = json.Marshal(tc)
	if string(out) != `{"tool_call_id":"c","function_name":"f","arguments":{}}` {
		t.Errorf("tool call = %s", out)
	}
	out, _ = json.Marshal(Observation{})
	if string(out) != `{"results":[]}` {
		t.Errorf("observation = %s", out)
	}
	out, _ = json.Marshal(Trajectory{SchemaVersion: SchemaVersion, Agent: Agent{Name: "a", Version: "1"}})
	if string(out) != `{"schema_version":"ATIF-v1.8","agent":{"name":"a","version":"1"},"steps":[]}` {
		t.Errorf("trajectory = %s", out)
	}
}

func TestValidate(t *testing.T) {
	good := func() *Trajectory {
		return &Trajectory{
			SchemaVersion: SchemaVersion,
			Agent:         Agent{Name: "a", Version: "1"},
			Steps: []Step{
				{StepID: 1, Source: SourceUser, Message: Text("hi"), Timestamp: "2026-09-17T12:00:00Z"},
				{StepID: 2, Source: SourceAgent, Message: Text(""), ToolCalls: []ToolCall{{ToolCallID: "c1", FunctionName: "f"}},
					Observation: &Observation{Results: []ObservationResult{{SourceCallID: "c1", Content: Text("ok")}}},
					Metrics:     &Metrics{PromptTokens: Ptr(1)}, LLMCallCount: Ptr(1), ReasoningEffort: "low"},
			},
		}
	}
	if err := good().Validate(); err != nil {
		t.Fatalf("good document: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Trajectory)
		want   string
	}{
		{"schema version", func(tr *Trajectory) { tr.SchemaVersion = "ATIF-v2.0" }, "schema_version"},
		{"agent name", func(tr *Trajectory) { tr.Agent.Name = "" }, "agent.name"},
		{"agent version", func(tr *Trajectory) { tr.Agent.Version = "" }, "agent.version"},
		{"no steps", func(tr *Trajectory) { tr.Steps = nil }, "at least one step"},
		{"step ids", func(tr *Trajectory) { tr.Steps[1].StepID = 3 }, "steps[1].step_id"},
		{"source", func(tr *Trajectory) { tr.Steps[0].Source = "tool" }, "source"},
		{"timestamp", func(tr *Trajectory) { tr.Steps[0].Timestamp = "yesterday" }, "timestamp"},
		{"model on user", func(tr *Trajectory) { tr.Steps[0].ModelName = "m" }, "model_name"},
		{"effort on user", func(tr *Trajectory) { tr.Steps[0].ReasoningEffort = "low" }, "reasoning_effort"},
		{"reasoning on user", func(tr *Trajectory) { tr.Steps[0].ReasoningContent = "r" }, "reasoning_content"},
		{"tool calls on user", func(tr *Trajectory) { tr.Steps[0].ToolCalls = []ToolCall{} }, "tool_calls"},
		{"metrics on user", func(tr *Trajectory) { tr.Steps[0].Metrics = &Metrics{} }, "metrics"},
		{"negative llm calls", func(tr *Trajectory) { tr.Steps[1].LLMCallCount = Ptr(-1) }, "llm_call_count"},
		{"zero llm calls with metrics", func(tr *Trajectory) { tr.Steps[1].LLMCallCount = Ptr(0) }, "llm_call_count 0"},
		{"effort type", func(tr *Trajectory) { tr.Steps[1].ReasoningEffort = []int{1} }, "reasoning_effort must"},
		{"call id", func(tr *Trajectory) { tr.Steps[1].ToolCalls[0].ToolCallID = "" }, "tool_call_id"},
		{"function name", func(tr *Trajectory) { tr.Steps[1].ToolCalls[0].FunctionName = "" }, "function_name"},
		{"dangling source_call_id", func(tr *Trajectory) { tr.Steps[1].Observation.Results[0].SourceCallID = "c9" }, "source_call_id"},
		{"unresolvable ref", func(tr *Trajectory) {
			tr.Steps[1].Observation.Results[0].SubagentTrajectoryRef = []SubagentTrajectoryRef{{SessionID: "s"}}
		}, "trajectory_id or trajectory_path"},
		{"text part with source", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartText, Text: "x", Source: &MediaSource{}}}}
		}, "source is not allowed"},
		{"image without source", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage}}}
		}, "source is required"},
		{"image with audio type", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage, Source: &MediaSource{MediaType: "audio/wav", Path: "p"}}}}
		}, "image part with media_type"},
		{"audio with image type", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartAudio, Source: &MediaSource{MediaType: "image/png", Path: "p"}}}}
		}, "audio part with media_type"},
		{"media without path", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: PartImage, Source: &MediaSource{MediaType: "image/png"}}}}
		}, "media_type and path"},
		{"unknown part type", func(tr *Trajectory) {
			tr.Steps[0].Message = Content{Parts: []ContentPart{{Type: "video"}}}
		}, "unknown part type"},
		{"observation content parts", func(tr *Trajectory) {
			tr.Steps[1].Observation.Results[0].Content = Content{Parts: []ContentPart{{Type: "x"}}}
		}, "observation.results[0].content"},
		{"negative total steps", func(tr *Trajectory) { tr.FinalMetrics = &FinalMetrics{TotalSteps: Ptr(-1)} }, "total_steps"},
		{"embedded without id", func(tr *Trajectory) { tr.SubagentTrajectories = []*Trajectory{good()} }, "trajectory_id is required"},
		{"embedded duplicate id", func(tr *Trajectory) {
			a, b := good(), good()
			a.TrajectoryID, b.TrajectoryID = "t", "t"
			tr.SubagentTrajectories = []*Trajectory{a, b}
		}, "not unique"},
		{"embedded null", func(tr *Trajectory) { tr.SubagentTrajectories = []*Trajectory{nil} }, "is null"},
		{"embedded invalid", func(tr *Trajectory) {
			a := good()
			a.TrajectoryID, a.Steps[0].Source = "t", "bad"
			tr.SubagentTrajectories = []*Trajectory{a}
		}, "subagent_trajectories[0].steps[0].source"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := good()
			tt.mutate(tr)
			err := tr.Validate()
			if err == nil {
				t.Fatal("Validate accepted the document")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want substring %q", err, tt.want)
			}
		})
	}
	// Embedded subagents validate when well formed.
	tr := good()
	sub := good()
	sub.TrajectoryID = "sub"
	tr.SubagentTrajectories = []*Trajectory{sub}
	tr.Steps[1].Observation.Results[0].SubagentTrajectoryRef = []SubagentTrajectoryRef{{TrajectoryID: "sub"}}
	if err := tr.Validate(); err != nil {
		t.Errorf("embedded: %v", err)
	}
	if _, err := Parse([]byte(`{`)); err == nil {
		t.Error("Parse accepted malformed JSON")
	}
	if _, err := Parse([]byte(`{"schema_version":"x","agent":{},"steps":[]}`)); !errors.Is(err, ErrInvalid) {
		t.Errorf("Parse err = %v, want ErrInvalid", err)
	}
}
