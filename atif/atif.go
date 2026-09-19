// Package atif holds Go types for the Agent Trajectory Interchange
// Format (ATIF) v1.8, Harbor's interchange format for agent
// trajectories, as defined by its RFC and the Pydantic models in
// harbor.models.trajectories. Every object keeps members it does not
// declare in an Unknown map and writes them back, so a document from a
// later version survives a round trip. [Trajectory.Validate] applies the
// rules the Harbor validator enforces.
package atif

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// SchemaVersion is the version this package writes.
const SchemaVersion = "ATIF-v1.8"

// Step sources.
const (
	SourceSystem = "system"
	SourceUser   = "user"
	SourceAgent  = "agent"
)

// Content part types.
const (
	PartText  = "text"
	PartImage = "image"
	PartAudio = "audio"
)

// Trajectory is the root document.
type Trajectory struct {
	SchemaVersion          string         `json:"schema_version"`
	SessionID              string         `json:"session_id,omitzero"`
	TrajectoryID           string         `json:"trajectory_id,omitzero"`
	Agent                  Agent          `json:"agent"`
	Steps                  []Step         `json:"steps"`
	Notes                  string         `json:"notes,omitzero"`
	FinalMetrics           *FinalMetrics  `json:"final_metrics,omitzero"`
	ContinuedTrajectoryRef string         `json:"continued_trajectory_ref,omitzero"`
	Extra                  map[string]any `json:"extra,omitzero"`
	SubagentTrajectories   []*Trajectory  `json:"subagent_trajectories,omitzero"`

	// Unknown holds members this package does not declare.
	Unknown map[string]json.RawMessage `json:"-"`
}

// Agent identifies the agent system.
type Agent struct {
	Name            string            `json:"name"`
	Version         string            `json:"version"`
	ModelName       string            `json:"model_name,omitzero"`
	ToolDefinitions []json.RawMessage `json:"tool_definitions,omitzero"`
	Extra           map[string]any    `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// Step is one interaction turn.
type Step struct {
	StepID    int    `json:"step_id"`
	Timestamp string `json:"timestamp,omitzero"`
	Source    string `json:"source"`
	ModelName string `json:"model_name,omitzero"`
	// ReasoningEffort is a string such as "medium" or a number.
	ReasoningEffort  any            `json:"reasoning_effort,omitzero"`
	Message          Content        `json:"message"`
	ReasoningContent string         `json:"reasoning_content,omitzero"`
	ToolCalls        []ToolCall     `json:"tool_calls,omitzero"`
	Observation      *Observation   `json:"observation,omitzero"`
	Metrics          *Metrics       `json:"metrics,omitzero"`
	IsCopiedContext  *bool          `json:"is_copied_context,omitzero"`
	LLMCallCount     *int           `json:"llm_call_count,omitzero"`
	Extra            map[string]any `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// Content is a message or observation body: a plain string, or a list
// of parts for multimodal content. Parts wins when it is non-nil.
type Content struct {
	Text  string
	Parts []ContentPart
}

// Text builds text-only content.
func Text(s string) Content { return Content{Text: s} }

// IsZero reports whether the content is an empty string with no parts,
// which is how an optional Content is omitted.
func (c Content) IsZero() bool { return c.Text == "" && c.Parts == nil }

// String returns the text form: the text, or the text parts joined.
func (c Content) String() string {
	if c.Parts == nil {
		return c.Text
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Type == PartText {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// MarshalJSON emits the parts when set, else the string.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.Parts != nil {
		return json.Marshal(c.Parts)
	}
	return json.Marshal(c.Text)
}

// UnmarshalJSON accepts a string or an array of parts.
func (c *Content) UnmarshalJSON(data []byte) error {
	*c = Content{}
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if data[0] == '"' {
		return json.Unmarshal(data, &c.Text)
	}
	c.Parts = []ContentPart{}
	return json.Unmarshal(data, &c.Parts)
}

// ContentPart is one part of multimodal content.
type ContentPart struct {
	Type   string       `json:"type"`
	Text   string       `json:"text"`
	Source *MediaSource `json:"source,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// MarshalJSON emits text only for text parts, because Harbor rejects a
// text member on media parts.
func (p ContentPart) MarshalJSON() ([]byte, error) {
	if p.Type == PartText {
		type plain ContentPart
		return jsonx.MarshalWithUnknown(plain(p), p.Unknown)
	}
	return jsonx.MarshalWithUnknown(struct {
		Type   string       `json:"type"`
		Source *MediaSource `json:"source,omitzero"`
	}{p.Type, p.Source}, p.Unknown)
}

// UnmarshalJSON decodes a part, keeping unknown members.
func (p *ContentPart) UnmarshalJSON(data []byte) error {
	type plain ContentPart
	var err error
	p.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(p), contentPartKeys)
	return err
}

// MediaSource locates an image or audio file.
type MediaSource struct {
	MediaType   string   `json:"media_type"`
	Path        string   `json:"path"`
	DurationSec *float64 `json:"duration_sec,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// ToolCall is one action the agent requested.
type ToolCall struct {
	ToolCallID   string         `json:"tool_call_id"`
	FunctionName string         `json:"function_name"`
	Arguments    map[string]any `json:"arguments"`
	Extra        map[string]any `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// Observation is the feedback after actions or system events.
type Observation struct {
	Results []ObservationResult `json:"results"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// ObservationResult is one result within an observation.
type ObservationResult struct {
	SourceCallID          string                  `json:"source_call_id,omitzero"`
	Content               Content                 `json:"content,omitzero"`
	SubagentTrajectoryRef []SubagentTrajectoryRef `json:"subagent_trajectory_ref,omitzero"`
	Extra                 map[string]any          `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// SubagentTrajectoryRef points at a delegated subagent trajectory,
// embedded by TrajectoryID or external by TrajectoryPath.
type SubagentTrajectoryRef struct {
	TrajectoryID   string         `json:"trajectory_id,omitzero"`
	SessionID      string         `json:"session_id,omitzero"`
	TrajectoryPath string         `json:"trajectory_path,omitzero"`
	Extra          map[string]any `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// Metrics is the LLM accounting for one step.
type Metrics struct {
	PromptTokens       *int           `json:"prompt_tokens,omitzero"`
	CompletionTokens   *int           `json:"completion_tokens,omitzero"`
	CachedTokens       *int           `json:"cached_tokens,omitzero"`
	CostUSD            *float64       `json:"cost_usd,omitzero"`
	PromptTokenIDs     []int          `json:"prompt_token_ids,omitzero"`
	CompletionTokenIDs []int          `json:"completion_token_ids,omitzero"`
	Logprobs           []float64      `json:"logprobs,omitzero"`
	Extra              map[string]any `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// FinalMetrics aggregates a trajectory.
type FinalMetrics struct {
	TotalPromptTokens     *int           `json:"total_prompt_tokens,omitzero"`
	TotalCompletionTokens *int           `json:"total_completion_tokens,omitzero"`
	TotalCachedTokens     *int           `json:"total_cached_tokens,omitzero"`
	TotalCostUSD          *float64       `json:"total_cost_usd,omitzero"`
	TotalSteps            *int           `json:"total_steps,omitzero"`
	Extra                 map[string]any `json:"extra,omitzero"`

	Unknown map[string]json.RawMessage `json:"-"`
}

// --- unknown-member passthrough ---

var (
	trajectoryKeys   = jsonx.Keys[Trajectory]()
	agentKeys        = jsonx.Keys[Agent]()
	stepKeys         = jsonx.Keys[Step]()
	contentPartKeys  = jsonx.Keys[ContentPart]()
	mediaSourceKeys  = jsonx.Keys[MediaSource]()
	toolCallKeys     = jsonx.Keys[ToolCall]()
	observationKeys  = jsonx.Keys[Observation]()
	resultKeys       = jsonx.Keys[ObservationResult]()
	refKeys          = jsonx.Keys[SubagentTrajectoryRef]()
	metricsKeys      = jsonx.Keys[Metrics]()
	finalMetricsKeys = jsonx.Keys[FinalMetrics]()
)

// MarshalJSON emits the document with unknown members appended.
func (t Trajectory) MarshalJSON() ([]byte, error) {
	type plain Trajectory
	cp := plain(t)
	if cp.Steps == nil {
		cp.Steps = []Step{}
	}
	return jsonx.MarshalWithUnknown(cp, t.Unknown)
}

// UnmarshalJSON decodes the document, keeping unknown members.
func (t *Trajectory) UnmarshalJSON(data []byte) error {
	type plain Trajectory
	var err error
	t.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(t), trajectoryKeys)
	return err
}

// MarshalJSON emits the agent with unknown members appended.
func (a Agent) MarshalJSON() ([]byte, error) {
	type plain Agent
	return jsonx.MarshalWithUnknown(plain(a), a.Unknown)
}

// UnmarshalJSON decodes the agent, keeping unknown members.
func (a *Agent) UnmarshalJSON(data []byte) error {
	type plain Agent
	var err error
	a.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(a), agentKeys)
	return err
}

// MarshalJSON emits the step with unknown members appended.
func (s Step) MarshalJSON() ([]byte, error) {
	type plain Step
	return jsonx.MarshalWithUnknown(plain(s), s.Unknown)
}

// UnmarshalJSON decodes the step, keeping unknown members.
func (s *Step) UnmarshalJSON(data []byte) error {
	type plain Step
	var err error
	s.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(s), stepKeys)
	return err
}

// MarshalJSON emits the source with unknown members appended.
func (m MediaSource) MarshalJSON() ([]byte, error) {
	type plain MediaSource
	return jsonx.MarshalWithUnknown(plain(m), m.Unknown)
}

// UnmarshalJSON decodes the source, keeping unknown members.
func (m *MediaSource) UnmarshalJSON(data []byte) error {
	type plain MediaSource
	var err error
	m.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(m), mediaSourceKeys)
	return err
}

// MarshalJSON emits the call with unknown members appended. Arguments
// is always an object.
func (c ToolCall) MarshalJSON() ([]byte, error) {
	type plain ToolCall
	cp := plain(c)
	if cp.Arguments == nil {
		cp.Arguments = map[string]any{}
	}
	return jsonx.MarshalWithUnknown(cp, c.Unknown)
}

// UnmarshalJSON decodes the call, keeping unknown members.
func (c *ToolCall) UnmarshalJSON(data []byte) error {
	type plain ToolCall
	var err error
	c.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(c), toolCallKeys)
	return err
}

// MarshalJSON emits the observation with unknown members appended.
// Results is always an array.
func (o Observation) MarshalJSON() ([]byte, error) {
	type plain Observation
	cp := plain(o)
	if cp.Results == nil {
		cp.Results = []ObservationResult{}
	}
	return jsonx.MarshalWithUnknown(cp, o.Unknown)
}

// UnmarshalJSON decodes the observation, keeping unknown members.
func (o *Observation) UnmarshalJSON(data []byte) error {
	type plain Observation
	var err error
	o.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(o), observationKeys)
	return err
}

// MarshalJSON emits the result with unknown members appended.
func (r ObservationResult) MarshalJSON() ([]byte, error) {
	type plain ObservationResult
	return jsonx.MarshalWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON decodes the result, keeping unknown members.
func (r *ObservationResult) UnmarshalJSON(data []byte) error {
	type plain ObservationResult
	var err error
	r.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(r), resultKeys)
	return err
}

// MarshalJSON emits the ref with unknown members appended.
func (r SubagentTrajectoryRef) MarshalJSON() ([]byte, error) {
	type plain SubagentTrajectoryRef
	return jsonx.MarshalWithUnknown(plain(r), r.Unknown)
}

// UnmarshalJSON decodes the ref, keeping unknown members.
func (r *SubagentTrajectoryRef) UnmarshalJSON(data []byte) error {
	type plain SubagentTrajectoryRef
	var err error
	r.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(r), refKeys)
	return err
}

// MarshalJSON emits the metrics with unknown members appended.
func (m Metrics) MarshalJSON() ([]byte, error) {
	type plain Metrics
	return jsonx.MarshalWithUnknown(plain(m), m.Unknown)
}

// UnmarshalJSON decodes the metrics, keeping unknown members.
func (m *Metrics) UnmarshalJSON(data []byte) error {
	type plain Metrics
	var err error
	m.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(m), metricsKeys)
	return err
}

// MarshalJSON emits the metrics with unknown members appended.
func (m FinalMetrics) MarshalJSON() ([]byte, error) {
	type plain FinalMetrics
	return jsonx.MarshalWithUnknown(plain(m), m.Unknown)
}

// UnmarshalJSON decodes the metrics, keeping unknown members.
func (m *FinalMetrics) UnmarshalJSON(data []byte) error {
	type plain FinalMetrics
	var err error
	m.Unknown, err = jsonx.UnmarshalWithUnknown(data, (*plain)(m), finalMetricsKeys)
	return err
}

// --- validation ---

// Validate applies the rules Harbor's validator enforces: a known
// schema version, agent name and version, at least one step with
// sequential IDs from 1, valid sources, agent-only fields on agent
// steps, ISO 8601 timestamps, observation results that reference the
// step's own tool calls, well-formed content parts, resolvable
// subagent references and unique IDs on embedded subagents. Unknown
// members are not an error: Harbor's models reject them, so a document
// meant for Harbor should carry none, and [Trajectory.HasUnknown]
// reports whether one does.
func (t *Trajectory) Validate() error {
	return t.validate("")
}

func (t *Trajectory) validate(prefix string) error {
	if !strings.HasPrefix(t.SchemaVersion, "ATIF-v1.") {
		return fmt.Errorf("%sschema_version %q is not an ATIF-v1 version", prefix, t.SchemaVersion)
	}
	if t.Agent.Name == "" {
		return fmt.Errorf("%sagent.name is required", prefix)
	}
	if t.Agent.Version == "" {
		return fmt.Errorf("%sagent.version is required", prefix)
	}
	if len(t.Steps) == 0 {
		return fmt.Errorf("%ssteps must have at least one step", prefix)
	}
	for i := range t.Steps {
		if err := t.Steps[i].validate(fmt.Sprintf("%ssteps[%d].", prefix, i), i+1); err != nil {
			return err
		}
	}
	if t.FinalMetrics != nil && t.FinalMetrics.TotalSteps != nil && *t.FinalMetrics.TotalSteps < 0 {
		return fmt.Errorf("%sfinal_metrics.total_steps must not be negative", prefix)
	}
	seen := map[string]bool{}
	for i, sub := range t.SubagentTrajectories {
		p := fmt.Sprintf("%ssubagent_trajectories[%d].", prefix, i)
		if sub == nil {
			return fmt.Errorf("%sis null", p)
		}
		if sub.TrajectoryID == "" {
			return fmt.Errorf("%strajectory_id is required for embedded subagents", p)
		}
		if seen[sub.TrajectoryID] {
			return fmt.Errorf("%strajectory_id %q is not unique", p, sub.TrajectoryID)
		}
		seen[sub.TrajectoryID] = true
		if err := sub.validate(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *Step) validate(prefix string, want int) error {
	if s.StepID != want {
		return fmt.Errorf("%sstep_id: expected %d (sequential from 1), got %d", prefix, want, s.StepID)
	}
	switch s.Source {
	case SourceSystem, SourceUser, SourceAgent:
	default:
		return fmt.Errorf("%ssource %q must be system, user or agent", prefix, s.Source)
	}
	if s.Timestamp != "" {
		if _, err := time.Parse(time.RFC3339Nano, s.Timestamp); err != nil {
			return fmt.Errorf("%stimestamp %q is not ISO 8601: %w", prefix, s.Timestamp, err)
		}
	}
	if s.Source != SourceAgent {
		switch {
		case s.ModelName != "":
			return fmt.Errorf("%smodel_name is only applicable to agent steps", prefix)
		case s.ReasoningEffort != nil:
			return fmt.Errorf("%sreasoning_effort is only applicable to agent steps", prefix)
		case s.ReasoningContent != "":
			return fmt.Errorf("%sreasoning_content is only applicable to agent steps", prefix)
		case s.ToolCalls != nil:
			return fmt.Errorf("%stool_calls is only applicable to agent steps", prefix)
		case s.Metrics != nil:
			return fmt.Errorf("%smetrics is only applicable to agent steps", prefix)
		}
	}
	if s.LLMCallCount != nil {
		if *s.LLMCallCount < 0 {
			return fmt.Errorf("%sllm_call_count must not be negative", prefix)
		}
		if *s.LLMCallCount == 0 && s.Source == SourceAgent && (s.Metrics != nil || s.ReasoningContent != "") {
			return fmt.Errorf("%sllm_call_count 0 forbids metrics and reasoning_content", prefix)
		}
	}
	switch s.ReasoningEffort.(type) {
	case nil, string, float64, float32, int, int64, json.Number:
	default:
		return fmt.Errorf("%sreasoning_effort must be a string or a number", prefix)
	}
	if err := s.Message.validate(prefix + "message"); err != nil {
		return err
	}
	calls := map[string]bool{}
	for i, c := range s.ToolCalls {
		p := fmt.Sprintf("%stool_calls[%d].", prefix, i)
		if c.ToolCallID == "" {
			return fmt.Errorf("%stool_call_id is required", p)
		}
		if c.FunctionName == "" {
			return fmt.Errorf("%sfunction_name is required", p)
		}
		calls[c.ToolCallID] = true
	}
	if s.Observation != nil {
		for i, r := range s.Observation.Results {
			p := fmt.Sprintf("%sobservation.results[%d].", prefix, i)
			if r.SourceCallID != "" && !calls[r.SourceCallID] {
				return fmt.Errorf("%ssource_call_id %q is not in the step's tool_calls", p, r.SourceCallID)
			}
			if err := r.Content.validate(p + "content"); err != nil {
				return err
			}
			for j, ref := range r.SubagentTrajectoryRef {
				if ref.TrajectoryID == "" && ref.TrajectoryPath == "" {
					return fmt.Errorf("%ssubagent_trajectory_ref[%d] must set trajectory_id or trajectory_path", p, j)
				}
			}
		}
	}
	return nil
}

func (c Content) validate(prefix string) error {
	for i, p := range c.Parts {
		pp := fmt.Sprintf("%s[%d]", prefix, i)
		switch p.Type {
		case PartText:
			if p.Source != nil {
				return fmt.Errorf("%s: source is not allowed on a text part", pp)
			}
		case PartImage, PartAudio:
			if p.Source == nil {
				return fmt.Errorf("%s: source is required on a %s part", pp, p.Type)
			}
			if p.Source.MediaType == "" || p.Source.Path == "" {
				return fmt.Errorf("%s: source needs media_type and path", pp)
			}
			if p.Type == PartImage && !strings.HasPrefix(p.Source.MediaType, "image/") {
				return fmt.Errorf("%s: image part with media_type %q", pp, p.Source.MediaType)
			}
			if p.Type == PartAudio && !strings.HasPrefix(p.Source.MediaType, "audio/") {
				return fmt.Errorf("%s: audio part with media_type %q", pp, p.Source.MediaType)
			}
			if p.Text != "" {
				return fmt.Errorf("%s: text is not allowed on a %s part", pp, p.Type)
			}
		default:
			return fmt.Errorf("%s: unknown part type %q", pp, p.Type)
		}
	}
	return nil
}

// HasUnknown reports whether the document or anything in it carries a
// member this package does not declare, which Harbor's strict models
// would reject.
func (t *Trajectory) HasUnknown() bool {
	if len(t.Unknown) > 0 || len(t.Agent.Unknown) > 0 || (t.FinalMetrics != nil && len(t.FinalMetrics.Unknown) > 0) {
		return true
	}
	for _, s := range t.Steps {
		if s.hasUnknown() {
			return true
		}
	}
	for _, sub := range t.SubagentTrajectories {
		if sub != nil && sub.HasUnknown() {
			return true
		}
	}
	return false
}

func (s *Step) hasUnknown() bool {
	if len(s.Unknown) > 0 || (s.Metrics != nil && len(s.Metrics.Unknown) > 0) {
		return true
	}
	for _, p := range s.Message.Parts {
		if len(p.Unknown) > 0 || (p.Source != nil && len(p.Source.Unknown) > 0) {
			return true
		}
	}
	for _, c := range s.ToolCalls {
		if len(c.Unknown) > 0 {
			return true
		}
	}
	if s.Observation != nil {
		if len(s.Observation.Unknown) > 0 {
			return true
		}
		for _, r := range s.Observation.Results {
			if len(r.Unknown) > 0 {
				return true
			}
			for _, p := range r.Content.Parts {
				if len(p.Unknown) > 0 {
					return true
				}
			}
			for _, ref := range r.SubagentTrajectoryRef {
				if len(ref.Unknown) > 0 {
					return true
				}
			}
		}
	}
	return false
}

// HasMultimodalContent reports whether any step message or observation
// carries an image or audio part.
func (t *Trajectory) HasMultimodalContent() bool {
	for _, s := range t.Steps {
		for _, p := range s.Message.Parts {
			if p.Type == PartImage || p.Type == PartAudio {
				return true
			}
		}
		if s.Observation != nil {
			for _, r := range s.Observation.Results {
				for _, p := range r.Content.Parts {
					if p.Type == PartImage || p.Type == PartAudio {
						return true
					}
				}
			}
		}
	}
	return false
}

// ErrInvalid wraps validation failures reported by [Parse].
var ErrInvalid = errors.New("atif: invalid trajectory")

// Parse decodes and validates a document.
func Parse(data []byte) (*Trajectory, error) {
	var t Trajectory
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("atif: decode: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return &t, nil
}

// Ptr returns a pointer to v, for the optional numeric members.
func Ptr[T any](v T) *T { return &v }
