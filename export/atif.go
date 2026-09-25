package export

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// Options configure [ToATIF].
type Options struct {
	// AgentName and AgentVersion fill the ATIF agent object. They
	// default to the session header's harness, then to "agentsession".
	AgentName    string
	AgentVersion string
	// ModelName overrides the model name the document reports, in
	// agent.model_name and on every agent step, without touching the
	// model the request was sent with. It exists because a consumer
	// may want a name the provider would refuse: Harbor derives a
	// provider by splitting model_name on the first slash, and a
	// self-hosted model called "qwen3.5:9b" has none, while sending
	// "ollama/qwen3.5:9b" is a 404. The session still records what was
	// sent; this is what is reported.
	ModelName string
	// Cost returns the price of one model call when a price source is
	// configured. When nil, cost_usd is left absent.
	Cost func(model string, usage openresponses.Usage) (usd float64, ok bool)
	// Subsessions resolves a link entry's session ID to the session so
	// its main trajectory can be embedded as a subagent trajectory. A
	// nil resolver, or one returning nil, leaves a file reference to
	// [MainDocumentName] of the child, which is where [WriteATIF] puts
	// the child's main trajectory.
	Subsessions func(sessionID string) (*agentsession.Session, error)
	// Preferences decide the continued branch at a fork of an embedded
	// subsession; see [Trajectories]. The caller's own session is
	// walked with the preferences it passed to Trajectories.
	Preferences []Preference
	// Redactors run over the finished document in order.
	Redactors []Redactor
	// Notes is copied to the document's notes member.
	Notes string
}

// Root extra members the exporter writes.
const (
	ExtraOpenResponses = "openresponses"
	ExtraAgentSession  = "agentsession"
	ExtraEnvironment   = "environment"
	ExtraOutcome       = "outcome"
	ExtraPreferredOver = "preferred_over"
	ExtraAbandonedAt   = "abandoned_at"
	ExtraBranchFrom    = "branch_from"
	ExtraContextMgmt   = "context_management"
	// ExtraRun carries, as a list, the runs whose records belong to a
	// step: a run's source, trigger, end reason, cause and pending
	// calls go in the extra of the first step its segment produces, or
	// in the root extra when it produces none. It is a list because a
	// run that produces no step, which is what a refusal on resume is,
	// would otherwise be overwritten by the next run's record, and
	// because a reader needs an order where several land in one place.
	ExtraRun = "run"
	// ExtraCalls carries, keyed by call ID in the extra of the agent
	// step that produced the call, the call's decisions and dispatch.
	ExtraCalls = "calls"
	// ExtraQueued carries, as a list, the inputs a harness accepted
	// before it could append them: the item, the mode and the trigger
	// of each. A queued input that was appended is a step of its own
	// carrying the same trigger under "source".
	ExtraQueued = "queued"
)

// ToATIF converts one trajectory into an ATIF document. Every step
// carries the raw items it was built from under extra.openresponses,
// so [Items] can rebuild the item list; the mapping is documented in
// docs/plans/session-layer.md.
func ToATIF(t Trajectory, opts Options) (*atif.Trajectory, error) {
	b := &builder{t: t, opts: opts, stepByEntry: map[string]int{}, callStep: map[string]int{},
		callFrom: map[string][]agentsession.EntryRef{}}
	b.doc = &atif.Trajectory{
		SchemaVersion: atif.SchemaVersion,
		SessionID:     t.Header.ID,
		TrajectoryID:  t.LeafID,
		Notes:         opts.Notes,
	}
	b.doc.Agent = atif.Agent{Name: opts.AgentName, Version: opts.AgentVersion}
	if b.doc.Agent.Name == "" && t.Header.Harness != nil {
		b.doc.Agent.Name = t.Header.Harness.Name
		if b.doc.Agent.Version == "" {
			b.doc.Agent.Version = t.Header.Harness.Version
		}
	}
	if b.doc.Agent.Name == "" {
		b.doc.Agent.Name = "agentsession"
	}
	if b.doc.Agent.Version == "" {
		b.doc.Agent.Version = agentsession.Format
	}
	if err := b.run(); err != nil {
		return nil, err
	}
	for _, r := range opts.Redactors {
		if err := r.Redact(b.doc); err != nil {
			return nil, fmt.Errorf("export: redact: %w", err)
		}
	}
	return b.doc, nil
}

type builder struct {
	t    Trajectory
	opts Options
	doc  *atif.Trajectory

	settings     agentsession.Settings
	pending      map[string]any // extra members for the next step
	group        *agentGroup
	stepByEntry  map[string]int // entry ID -> step index (0-based)
	callStep     map[string]int // call ID -> step index
	copied       bool           // inside the kept window after a compaction
	compactionID string
	sawEnv       bool
	currentRun   map[string]any // the open run's record, shared with the step or root that holds it

	prompt, completion, cached int
	cost                       float64
	hasCost                    bool
	// priced remembers what Options.Cost answered for an entry, so a
	// price source with a cost of its own, or a count, is asked once
	// per model call and not once for the step and again for the
	// totals.
	priced map[string]price
	links  []*agentsession.LinkEntry
	// callFrom is what each call's output entry converged, by call ID:
	// for a call a subagent answered, the leaf of the child the answer
	// came from. It is what lets a subsession reference name a point in
	// the child rather than the projection choosing one.
	callFrom map[string][]agentsession.EntryRef
}

// price is one answer from Options.Cost.
type price struct {
	usd float64
	ok  bool
}

// costOf asks the price source about an entry, or returns what it
// answered the first time.
func (b *builder) costOf(entryID, model string, u openresponses.Usage) (float64, bool) {
	if b.opts.Cost == nil {
		return 0, false
	}
	if p, asked := b.priced[entryID]; asked {
		return p.usd, p.ok
	}
	usd, ok := b.opts.Cost(model, u)
	if b.priced == nil {
		b.priced = map[string]price{}
	}
	b.priced[entryID] = price{usd, ok}
	return usd, ok
}

// agentGroup accumulates model output items until their response
// entry arrives.
type agentGroup struct {
	responseID string
	entries    []*agentsession.ItemEntry
}

func (b *builder) run() error {
	entries := b.t.Context.Entries
	if len(entries) > 0 {
		if c, ok := entries[0].(*agentsession.CompactionEntry); ok {
			b.settings = c.Config
			b.compactionID = c.ID
			b.copied = true
		}
	}
	// Settings at the first step: the agent's default model and tools.
	b.setAgentDefaults()
	for _, e := range entries {
		if b.compactionID != "" && b.copied && e.Base().Parent == b.compactionID {
			b.copied = false
		}
		if err := b.entry(e); err != nil {
			return err
		}
	}
	b.flushGroup(nil)
	b.finish()
	return nil
}

func (b *builder) setAgentDefaults() {
	// Replay leading config entries so the agent block reflects the
	// settings the first step ran under.
	for _, e := range b.t.Context.Entries {
		switch v := e.(type) {
		case *agentsession.ConfigEntry:
			b.settings = b.settings.Apply(v)
			continue
		case *agentsession.CompactionEntry, *agentsession.EnvEntry, *agentsession.InfoEntry, *agentsession.LabelEntry, *agentsession.CustomEntry,
			*agentsession.RunEntry, *agentsession.DispatchEntry, *agentsession.DecisionEntry:
			continue
		}
		break
	}
	b.doc.Agent.ModelName = b.modelName(b.settings.Model)
	b.doc.Agent.ToolDefinitions = toolDefinitions(b.settings.Tools)
	// The replay above is repeated by entry(); reset so the second pass
	// applies deltas from the same baseline.
	if len(b.t.Context.Entries) > 0 {
		if c, ok := b.t.Context.Entries[0].(*agentsession.CompactionEntry); ok {
			b.settings = c.Config
			return
		}
	}
	b.settings = agentsession.Settings{}
}

func (b *builder) entry(e agentsession.Entry) error {
	switch v := e.(type) {
	case *agentsession.ItemEntry:
		return b.item(v)
	case *agentsession.ResponseEntry:
		b.flushGroup(v)
	case *agentsession.ConfigEntry:
		before := b.settings
		b.settings = b.settings.Apply(v)
		if change := toolChange(before.Tools, b.settings.Tools); change != nil {
			b.addPending("tool_changes", change)
		}
		b.addPendingList("config_entries", e.Base().ID)
		if len(v.Unknown) > 0 {
			b.addPendingList("config_extensions", copyUnknown(map[string]any{"entry_id": v.ID}, v.Unknown))
		}
	case *agentsession.CompactionEntry:
		b.flushGroup(nil)
		text := itemText(v.Summary)
		// The pinned items are context the fold kept, and the entries
		// they were copied from are before first_kept and so not in
		// the document. They travel raw beside the summary, which is
		// what keeps the projection lossless.
		payload := map[string]any{"item": rawItem(v.Summary)}
		if len(v.Pinned) > 0 {
			pinned := make([]json.RawMessage, 0, len(v.Pinned))
			for _, item := range v.Pinned {
				pinned = append(pinned, rawItem(item))
			}
			payload["pinned"] = pinned
		}
		step := atif.Step{
			Source:          atif.SourceSystem,
			Message:         atif.Text(text),
			Observation:     &atif.Observation{Results: []atif.ObservationResult{{Content: atif.Text(text)}}},
			IsCopiedContext: atif.Ptr(true),
			Extra: map[string]any{
				ExtraContextMgmt:   map[string]any{"type": "compaction", "boundary": "replace"},
				ExtraOpenResponses: payload,
				"first_kept":       v.FirstKept,
			},
		}
		if v.TokensBefore > 0 {
			step.Extra["tokens_before"] = v.TokensBefore
		}
		b.foldUsage(v.ID, step.Extra, v.Config.Model, v.Usage)
		copyUnknown(step.Extra, v.Unknown)
		b.addStep(step, e)
	case *agentsession.BranchSummaryEntry:
		b.flushGroup(nil)
		step := atif.Step{
			Source:          atif.SourceSystem,
			Message:         messageContent(v.Summary),
			IsCopiedContext: atif.Ptr(true),
			Extra: map[string]any{
				ExtraBranchFrom:    v.From,
				ExtraOpenResponses: map[string]any{"item": rawItem(v.Summary)},
			},
		}
		b.foldUsage(v.ID, step.Extra, b.settings.Model, v.Usage)
		copyUnknown(step.Extra, v.Unknown)
		b.addStep(step, e)
	case *agentsession.LabelEntry:
		label := any(nil)
		if v.Label != nil {
			label = *v.Label
		}
		b.addPendingList("labels", copyUnknown(map[string]any{"target": v.Target, "label": label, "entry_id": v.ID}, v.Unknown))
	case *agentsession.InfoEntry:
		info := map[string]any{"entry_id": v.ID}
		if v.Name != "" {
			info["name"] = v.Name
		}
		b.addPendingList("info", copyUnknown(info, v.Unknown))
	case *agentsession.CustomEntry:
		b.addPendingList("custom", copyUnknown(map[string]any{"ns": v.NS, "data": json.RawMessage(v.Data), "entry_id": v.ID}, v.Unknown))
	case *agentsession.EnvEntry:
		env := envExtra(v)
		if !b.sawEnv {
			b.sawEnv = true
			b.rootExtra()[ExtraEnvironment] = env
		} else {
			b.addPending(ExtraEnvironment, env)
		}
	case *agentsession.OutcomeEntry:
		b.outcome(v)
	case *agentsession.LinkEntry:
		b.links = append(b.links, v)
	case *agentsession.RunEntry:
		b.runEntry(v)
	case *agentsession.DispatchEntry:
		rec := copyUnknown(map[string]any{"entry_id": v.ID}, v.Unknown)
		b.callExtra(v.CallID)["dispatch"] = rec
	case *agentsession.DecisionEntry:
		rec := map[string]any{"entry_id": v.ID, "verdict": v.Verdict}
		if v.By != "" {
			rec["by"] = v.By
		}
		if v.Reason != "" {
			rec["reason"] = v.Reason
		}
		if len(v.Args) > 0 {
			rec["args"] = json.RawMessage(v.Args)
		}
		copyUnknown(rec, v.Unknown)
		call := b.callExtra(v.CallID)
		list, _ := call["decisions"].([]any)
		call["decisions"] = append(list, rec)
	case *agentsession.QueuedEntry:
		rec := map[string]any{"entry_id": v.ID, "mode": v.Mode, "item": rawItem(v.Item)}
		if v.Trigger != nil {
			rec["trigger"] = v.Trigger
		}
		if v.Ref != "" {
			rec["ref"] = v.Ref
		}
		copyUnknown(rec, v.Unknown)
		b.addPendingList(ExtraQueued, rec)
	case *agentsession.UnknownEntry:
		b.addPendingList("extensions", json.RawMessage(v.Raw))
	default:
		return fmt.Errorf("export: unhandled entry type %T", e)
	}
	return nil
}

func (b *builder) item(e *agentsession.ItemEntry) error {
	if e.ResponseID != "" || isModelOutput(e.Item) {
		if b.group != nil && b.group.responseID != "" && e.ResponseID != "" && b.group.responseID != e.ResponseID {
			b.flushGroup(nil)
		}
		if b.group == nil {
			b.group = &agentGroup{responseID: e.ResponseID}
		}
		if b.group.responseID == "" {
			b.group.responseID = e.ResponseID
		}
		b.group.entries = append(b.group.entries, e)
		return nil
	}
	b.flushGroup(nil)
	switch v := e.Item.(type) {
	case *openresponses.Message:
		source := atif.SourceUser
		if v.Role != openresponses.RoleUser {
			source = atif.SourceSystem
		}
		step := atif.Step{Source: source, Message: messageContent(v), Extra: b.itemExtra(e)}
		if v.Role != openresponses.RoleUser {
			step.Extra["role"] = string(v.Role)
		}
		b.addStep(step, e)
	case *openresponses.FunctionCallOutput:
		if len(e.Parents) > 0 {
			// Where the answer came from, which the subsession
			// reference needs and which nothing else on the path says.
			b.callFrom[v.CallID] = e.Parents
		}
		idx, ok := b.callStep[v.CallID]
		if ok && b.doc.Steps[idx].Source == atif.SourceAgent {
			step := &b.doc.Steps[idx]
			if step.Observation == nil {
				step.Observation = &atif.Observation{}
			}
			step.Observation.Results = append(step.Observation.Results, atif.ObservationResult{
				SourceCallID: v.CallID,
				Content:      outputContent(v.Output),
				Extra:        b.itemExtra(e),
			})
			b.stepByEntry[e.ID] = idx
			return nil
		}
		// An output without its call in this path: a system step so the
		// document still validates.
		step := atif.Step{
			Source:      atif.SourceSystem,
			Message:     atif.Text(""),
			Observation: &atif.Observation{Results: []atif.ObservationResult{{Content: outputContent(v.Output)}}},
			Extra:       b.itemExtra(e),
		}
		step.Extra["orphan_call_id"] = v.CallID
		b.addStep(step, e)
	default:
		// Extension items, item references and anything else that is
		// not model output: a system step by the item's role if it
		// declares one.
		step := atif.Step{Source: atif.SourceSystem, Message: atif.Text(itemText(v)), Extra: b.itemExtra(e)}
		step.Extra["item_type"] = v.ItemType()
		if u, ok := v.(*openresponses.UnknownItem); ok {
			var probe struct {
				Role string `json:"role"`
			}
			_ = json.Unmarshal(u.Raw, &probe)
			if probe.Role == string(openresponses.RoleUser) {
				step.Source = atif.SourceUser
			}
		}
		b.addStep(step, e)
	}
	return nil
}

// itemExtra is the extra block of a step built from one item entry.
func (b *builder) itemExtra(e *agentsession.ItemEntry) map[string]any {
	extra := map[string]any{
		ExtraOpenResponses: map[string]any{"item": rawItem(e.Item)},
	}
	if !e.IsVisible() {
		extra["visible"] = false
	}
	if e.Source != nil {
		extra["source"] = e.Source
	}
	if e.QueuedFrom != "" {
		extra["queued_from"] = e.QueuedFrom
	}
	return copyUnknown(extra, e.Unknown)
}

// flushGroup emits the agent step for the open group, with the response
// entry when it has arrived.
func (b *builder) flushGroup(resp *agentsession.ResponseEntry) {
	g := b.group
	b.group = nil
	if g == nil && resp == nil {
		return
	}
	if g == nil {
		g = &agentGroup{responseID: resp.ResponseID}
	}
	if resp != nil && g.responseID != "" && g.responseID != resp.ResponseID {
		// The group belongs to another call; close it on its own first.
		b.flushGroupItems(g, nil)
		g = &agentGroup{responseID: resp.ResponseID}
	}
	b.flushGroupItems(g, resp)
}

// modelName is the name the document reports for a model, which
// Options.ModelName overrides.
func (b *builder) modelName(model string) string {
	if b.opts.ModelName != "" {
		return b.opts.ModelName
	}
	return model
}

func (b *builder) flushGroupItems(g *agentGroup, resp *agentsession.ResponseEntry) {
	step := atif.Step{Source: atif.SourceAgent, ModelName: b.modelName(b.settings.Model), LLMCallCount: atif.Ptr(1)}
	if b.settings.Reasoning.Effort != "" {
		step.ReasoningEffort = string(b.settings.Reasoning.Effort)
	}
	var (
		texts     []string
		parts     []atif.ContentPart
		mixed     bool
		reasoning []string
		phases    []string
		raws      = []json.RawMessage{}
		ids       []string
	)
	for _, e := range g.entries {
		raws = append(raws, rawItem(e.Item))
		ids = append(ids, e.ID)
		switch v := e.Item.(type) {
		case *openresponses.Message:
			c := messageContent(v)
			if c.Parts != nil {
				mixed = true
				parts = append(parts, c.Parts...)
			} else {
				texts = append(texts, c.Text)
				parts = append(parts, atif.ContentPart{Type: atif.PartText, Text: c.Text})
			}
			if v.Phase != "" {
				phases = append(phases, string(v.Phase))
			}
		case *openresponses.ReasoningItem:
			if t := v.Content.Text(); t != "" {
				reasoning = append(reasoning, t)
			} else if t := v.Summary.Text(); t != "" {
				reasoning = append(reasoning, t)
			}
		case *openresponses.FunctionCall:
			step.ToolCalls = append(step.ToolCalls, atif.ToolCall{
				ToolCallID:   v.CallID,
				FunctionName: v.Name,
				Arguments:    parseArguments(v.Arguments),
			})
		}
	}
	if mixed {
		step.Message = atif.Content{Parts: parts}
	} else {
		step.Message = atif.Text(strings.Join(texts, "\n"))
	}
	step.ReasoningContent = strings.Join(reasoning, "\n")
	or := map[string]any{"items": raws}
	step.Extra = map[string]any{ExtraOpenResponses: or}
	if len(phases) > 0 {
		step.Extra["phases"] = phases
	}
	if len(ids) > 0 {
		step.Extra["item_entry_ids"] = ids
	}
	for _, e := range g.entries {
		copyUnknown(step.Extra, e.Unknown)
	}
	var anchor agentsession.Entry
	if len(g.entries) > 0 {
		anchor = g.entries[0]
	}
	if resp != nil {
		anchor = resp
		or["response"] = responseBody(resp)
		wire := b.settings.Model
		if resp.Model != "" {
			wire = resp.Model
			step.ModelName = b.modelName(resp.Model)
		}
		step.Timestamp = resp.Timestamp.UTC().Format(time.RFC3339Nano)
		if resp.Usage != nil {
			u := resp.Usage
			step.Metrics = &atif.Metrics{
				PromptTokens:     atif.Ptr(u.InputTokens),
				CompletionTokens: atif.Ptr(u.OutputTokens),
				CachedTokens:     atif.Ptr(u.InputTokensDetails.CachedTokens),
			}
			if u.OutputTokensDetails.ReasoningTokens > 0 {
				step.Metrics.Extra = map[string]any{"reasoning_tokens": u.OutputTokensDetails.ReasoningTokens}
			}
			// Priced by the model the request was sent with, not by the
			// name Options.ModelName reports it under.
			if usd, ok := b.costOf(resp.ID, wire, *u); ok {
				step.Metrics.CostUSD = atif.Ptr(usd)
				b.cost += usd
				b.hasCost = true
			}
			b.prompt += u.InputTokens
			b.completion += u.OutputTokens
			b.cached += u.InputTokensDetails.CachedTokens
		}
		if resp.Status != "" && resp.Status != openresponses.ResponseStatusCompleted {
			step.Extra["status"] = string(resp.Status)
		}
		if resp.Error != nil {
			step.Extra["error"] = resp.Error
		}
		if resp.Incomplete != nil {
			step.Extra["incomplete"] = resp.Incomplete.Reason
		}
		if resp.LatencyMS > 0 {
			step.Extra["latency_ms"] = resp.LatencyMS
		}
	} else if g.responseID != "" {
		step.Extra["response_id"] = g.responseID
	}
	if resp != nil {
		copyUnknown(step.Extra, resp.Unknown)
	}
	if anchor == nil {
		return
	}
	idx := b.addStep(step, anchor)
	for _, e := range g.entries {
		b.stepByEntry[e.ID] = idx
		if fc, ok := e.Item.(*openresponses.FunctionCall); ok {
			b.callStep[fc.CallID] = idx
		}
	}
}

// addStep appends a step built from entry, numbering it, stamping it
// with the entry's ID and time, marking copied context and attaching
// pending extras. It returns the step's index.
func (b *builder) addStep(step atif.Step, e agentsession.Entry) int {
	step.StepID = len(b.doc.Steps) + 1
	if step.Timestamp == "" {
		step.Timestamp = e.Base().Timestamp.UTC().Format(time.RFC3339Nano)
	}
	if step.Extra == nil {
		step.Extra = map[string]any{}
	}
	step.Extra[ExtraAgentSession] = map[string]any{"entry_id": e.Base().ID}
	if b.copied && step.IsCopiedContext == nil {
		step.IsCopiedContext = atif.Ptr(true)
	}
	for k, v := range b.pending {
		step.Extra[k] = v
	}
	b.pending = nil
	b.doc.Steps = append(b.doc.Steps, step)
	idx := len(b.doc.Steps) - 1
	b.stepByEntry[e.Base().ID] = idx
	return idx
}

func (b *builder) addPending(key string, v any) {
	if b.pending == nil {
		b.pending = map[string]any{}
	}
	b.pending[key] = v
}

func (b *builder) addPendingList(key string, v any) {
	if b.pending == nil {
		b.pending = map[string]any{}
	}
	list, _ := b.pending[key].([]any)
	b.pending[key] = append(list, v)
}

func (b *builder) rootExtra() map[string]any {
	if b.doc.Extra == nil {
		b.doc.Extra = map[string]any{}
	}
	return b.doc.Extra
}

// foldUsage records what a fold cost. ATIF allows metrics on agent
// steps only, and a compaction or branch summary is a system step, so
// the usage rides under "usage" in the step's extra, priced under
// "cost_usd" when a price source is configured, and both are added to
// the document's totals. Without this a compacting agent reports a
// fraction of what it spent.
func (b *builder) foldUsage(entryID string, extra map[string]any, model string, u *openresponses.Usage) {
	if u == nil {
		return
	}
	extra["usage"] = u
	if usd, ok := b.costOf(entryID, model, *u); ok {
		extra["cost_usd"] = usd
		b.cost += usd
		b.hasCost = true
	}
	b.prompt += u.InputTokens
	b.completion += u.OutputTokens
	b.cached += u.InputTokensDetails.CachedTokens
}

// runEntry records a run start under ExtraRun on the next step, and a
// run end on the record its start opened. The record is a map shared
// with the step or root that holds it, so the end's members land
// where the start's did.
func (b *builder) runEntry(r *agentsession.RunEntry) {
	if r.IsStart() {
		rec := map[string]any{"run_id": r.RunID, "source": r.Source, "start_entry_id": r.ID}
		if r.Ref != "" {
			rec["trigger"] = r.Ref
		}
		copyUnknown(rec, r.Unknown)
		b.currentRun = rec
		b.addPendingList(ExtraRun, rec)
		return
	}
	rec := b.currentRun
	if rec == nil || rec["run_id"] != r.RunID {
		// An end without its start on this path: record it on its own.
		rec = map[string]any{"run_id": r.RunID}
		b.addPendingList(ExtraRun, rec)
	}
	rec["reason"] = r.Reason
	rec["end_entry_id"] = r.ID
	if r.Ref != "" {
		rec["cause"] = r.Ref
	}
	pending := make([]any, 0, len(r.Pending))
	for _, id := range r.Pending {
		pending = append(pending, id)
	}
	rec["pending"] = pending
	copyUnknown(rec, r.Unknown)
	b.currentRun = nil
}

// callExtra returns the record for a call under ExtraCalls in the
// extra of the agent step that produced it, creating it on first use.
// A call whose step is not on this path gets a record in the root
// extra instead.
func (b *builder) callExtra(callID string) map[string]any {
	var extra map[string]any
	if idx, ok := b.callStep[callID]; ok {
		extra = b.doc.Steps[idx].Extra
	} else {
		extra = b.rootExtra()
	}
	calls, _ := extra[ExtraCalls].(map[string]any)
	if calls == nil {
		calls = map[string]any{}
		extra[ExtraCalls] = calls
	}
	rec, _ := calls[callID].(map[string]any)
	if rec == nil {
		rec = map[string]any{}
		calls[callID] = rec
	}
	return rec
}

func (b *builder) outcome(o *agentsession.OutcomeEntry) {
	rec := map[string]any{"kind": o.Kind, "entry_id": o.ID}
	if o.Target != "" {
		rec["target"] = o.Target
	}
	if o.Score != nil {
		rec["score"] = *o.Score
	}
	if o.Pass != nil {
		rec["pass"] = *o.Pass
	}
	if o.Label != "" {
		rec["label"] = o.Label
	}
	if len(o.Details) > 0 {
		rec["details"] = json.RawMessage(o.Details)
	}
	copyUnknown(rec, o.Unknown)
	if b.doc.FinalMetrics == nil {
		b.doc.FinalMetrics = &atif.FinalMetrics{}
	}
	if b.doc.FinalMetrics.Extra == nil {
		b.doc.FinalMetrics.Extra = map[string]any{}
	}
	list, _ := b.doc.FinalMetrics.Extra[ExtraOutcome].([]any)
	b.doc.FinalMetrics.Extra[ExtraOutcome] = append(list, rec)
	if idx, ok := b.stepByEntry[o.Target]; ok {
		step := &b.doc.Steps[idx]
		list, _ := step.Extra[ExtraOutcome].([]any)
		step.Extra[ExtraOutcome] = append(list, rec)
	}
}

// finish writes the root extras, final metrics, subagent links and
// anything still pending.
func (b *builder) finish() {
	root := b.rootExtra()
	root[ExtraOpenResponses] = map[string]any{"payload": b.t.Header.Payload}
	as := map[string]any{"format": b.t.Header.Format, "leaf": b.t.LeafID}
	if b.t.Header.CWD != "" {
		as["cwd"] = b.t.Header.CWD
	}
	if b.t.Header.ParentSession != "" {
		as["parent_session"] = b.t.Header.ParentSession
	}
	if b.t.Header.SpawnedBy != "" {
		as["spawned_by"] = b.t.Header.SpawnedBy
	}
	if len(b.t.Header.Records) > 0 {
		as["records"] = b.t.Header.Records
	}
	if b.t.Name != "" {
		as["name"] = b.t.Name
	}
	if len(b.t.Labels) > 0 {
		as["labels"] = b.t.Labels
	}
	if b.t.Main {
		as["main"] = true
	}
	root[ExtraAgentSession] = as
	if len(b.t.PreferredOver) > 0 {
		root[ExtraPreferredOver] = b.t.PreferredOver
	}
	if b.t.AbandonedAt != "" {
		root[ExtraAbandonedAt] = b.t.AbandonedAt
	}
	for k, v := range b.pending {
		root[k] = v
	}
	b.pending = nil
	b.subsessions()

	if b.doc.FinalMetrics == nil {
		b.doc.FinalMetrics = &atif.FinalMetrics{}
	}
	hidden := b.pathTotals()
	fm := b.doc.FinalMetrics
	fm.TotalPromptTokens = atif.Ptr(b.prompt)
	fm.TotalCompletionTokens = atif.Ptr(b.completion)
	fm.TotalCachedTokens = atif.Ptr(b.cached)
	fm.TotalSteps = atif.Ptr(len(b.doc.Steps) + hidden)
	if b.hasCost {
		fm.TotalCostUSD = atif.Ptr(b.cost)
	}
	if hidden > 0 {
		note := fmt.Sprintf("The steps are the context after compaction and leave out %d model call(s) that were folded away; total_steps counts them and final_metrics totals the whole path.", hidden)
		if b.doc.Notes != "" {
			note = b.doc.Notes + "\n" + note
		}
		b.doc.Notes = note
	}
}

// pathTotals replaces the totals accumulated from the steps with the
// totals of the whole path, and returns the number of model calls the
// path holds that the document does not show. A document's steps are
// the context after compaction, so a run that folded is described by
// its last summary and what followed; its cost is not, or a
// leaderboard reads the tail's cost as the run's. It does nothing for
// a Trajectory built without a Path, whose context is all there is.
func (b *builder) pathTotals() int {
	if len(b.t.Path) == 0 {
		return 0
	}
	inContext := make(map[string]bool, len(b.t.Context.Entries))
	for _, e := range b.t.Context.Entries {
		inContext[e.Base().ID] = true
	}
	b.prompt, b.completion, b.cached, b.cost, b.hasCost = 0, 0, 0, 0, false
	hidden, model := 0, ""
	for _, e := range b.t.Path {
		var u *openresponses.Usage
		priced := ""
		switch v := e.(type) {
		case *agentsession.ConfigEntry:
			// The model in force, for a fold or a response that does
			// not name its own.
			if v.Replace {
				model = ""
			}
			if v.Model != "" {
				model = v.Model
			}
			continue
		case *agentsession.ResponseEntry:
			u, priced = v.Usage, model
			if v.Model != "" {
				priced = v.Model
			}
		case *agentsession.CompactionEntry:
			model = v.Config.Model
			u, priced = v.Usage, v.Config.Model
		case *agentsession.BranchSummaryEntry:
			u, priced = v.Usage, model
		default:
			continue
		}
		if !inContext[e.Base().ID] {
			hidden++
		}
		if u == nil {
			continue
		}
		b.prompt += u.InputTokens
		b.completion += u.OutputTokens
		b.cached += u.InputTokensDetails.CachedTokens
		if usd, ok := b.costOf(e.Base().ID, priced, *u); ok {
			b.cost += usd
			b.hasCost = true
		}
	}
	return hidden
}

// subsessions resolves link entries: a subsession is embedded and
// referenced from the observation of the call that spawned it; other
// links are recorded in the root extra.
func (b *builder) subsessions() {
	var others []any
	for _, l := range b.links {
		rec := map[string]any{"rel": l.Rel, "session": l.Session, "entry_id": l.ID}
		if l.CallID != "" {
			rec["call_id"] = l.CallID
		}
		copyUnknown(rec, l.Unknown)
		if l.Rel != agentsession.RelSubsession {
			others = append(others, rec)
			continue
		}
		ref := atif.SubagentTrajectoryRef{SessionID: l.Session, Extra: map[string]any{"entry_id": l.ID}}
		// The link names the session; the output entry names the point
		// in it the answer was taken from. With the second, the
		// document says which of a branched child's leaves was used;
		// without it, the projection picks the child's main path and
		// the document turns on that choice rather than on the record.
		leaf := b.answeredFrom(l)
		if leaf != "" {
			ref.Extra["child_leaf"] = leaf
			ref.TrajectoryID = trajectoryID(l.Session, leaf)
		}
		if embedded := b.embed(l.Session, leaf); embedded != nil {
			ref.TrajectoryID = embedded.TrajectoryID
		} else {
			// Nothing was embedded, and when the record named a leaf
			// no other trajectory is put in its place. trajectory_id
			// still says which leaf the record named; the path is where
			// the session's document would be, which is all a document
			// that could not load the child can offer.
			ref.TrajectoryPath = MainDocumentName(l.Session)
		}
		if !b.attachRef(l.CallID, ref) {
			rec["subagent_trajectory_ref"] = ref
			others = append(others, rec)
		}
	}
	if len(others) > 0 {
		b.rootExtra()["links"] = others
	}
}

// answeredFrom returns the entry in the link's session that the call's
// output entry converged, or "" when it recorded none. An output may
// converge several predecessors — a join over more than one child —
// and it is the one naming this link's session that answered this
// link's call.
func (b *builder) answeredFrom(l *agentsession.LinkEntry) string {
	if l.CallID == "" || l.Session == "" {
		return ""
	}
	for _, r := range b.callFrom[l.CallID] {
		if r.Session == l.Session {
			return r.Entry
		}
	}
	return ""
}

// trajectoryID is the ID a document built at leaf of sessionID carries,
// which is the one [builder.embed] assigns.
func trajectoryID(sessionID, leaf string) string { return sessionID + "/" + leaf }

// embed converts a path of the subsession and adds it to the document,
// returning it, or nil when it cannot be resolved.
//
// leaf is the entry the parent recorded the answer as coming from. Given
// one, that path is embedded and no other: a record naming a leaf this
// export cannot resolve leaves the reference unembedded rather than
// quietly substituting a different path. Given "", the child's main
// path is chosen, which is the projection choosing where the record did
// not say.
func (b *builder) embed(sessionID, leaf string) *atif.Trajectory {
	if b.opts.Subsessions == nil {
		return nil
	}
	sub, err := b.opts.Subsessions(sessionID)
	if err != nil || sub == nil {
		return nil
	}
	var chosen *Trajectory
	for t, err := range Trajectories(sub, b.opts.Preferences...) {
		if err != nil {
			continue
		}
		if (leaf == "" && t.Main) || (leaf != "" && t.LeafID == leaf) {
			t := t
			chosen = &t
		}
	}
	if chosen == nil {
		return nil
	}
	opts := b.opts
	opts.Redactors = nil // the parent's redactors run over the whole document
	opts.Notes = ""
	doc, err := ToATIF(*chosen, opts)
	if err != nil {
		return nil
	}
	doc.TrajectoryID = trajectoryID(sessionID, chosen.LeafID)
	for _, existing := range b.doc.SubagentTrajectories {
		if existing.TrajectoryID == doc.TrajectoryID {
			return existing
		}
	}
	b.doc.SubagentTrajectories = append(b.doc.SubagentTrajectories, doc)
	return doc
}

// attachRef adds the ref to the observation result of callID.
func (b *builder) attachRef(callID string, ref atif.SubagentTrajectoryRef) bool {
	idx, ok := b.callStep[callID]
	if !ok {
		return false
	}
	step := &b.doc.Steps[idx]
	if step.Observation == nil {
		step.Observation = &atif.Observation{}
	}
	for i := range step.Observation.Results {
		r := &step.Observation.Results[i]
		if r.SourceCallID == callID {
			r.SubagentTrajectoryRef = append(r.SubagentTrajectoryRef, ref)
			return true
		}
	}
	step.Observation.Results = append(step.Observation.Results, atif.ObservationResult{
		SourceCallID:          callID,
		SubagentTrajectoryRef: []atif.SubagentTrajectoryRef{ref},
	})
	return true
}

// --- item helpers ---

// isModelOutput reports whether an item is something only the model
// produces, so it belongs to an agent step even without a response ID.
func isModelOutput(item openresponses.Item) bool {
	switch v := item.(type) {
	case *openresponses.Message:
		return v.Role == openresponses.RoleAssistant
	case *openresponses.ReasoningItem, *openresponses.FunctionCall:
		return true
	}
	return false
}

// rawItem returns the item's wire form.
func rawItem(item openresponses.Item) json.RawMessage {
	data, err := json.Marshal(item)
	if err != nil {
		return json.RawMessage(`null`)
	}
	return data
}

// responseBody returns the response entry without its envelope.
func responseBody(r *agentsession.ResponseEntry) json.RawMessage {
	data, err := agentsession.MarshalEntry(r)
	if err != nil {
		return json.RawMessage(`null`)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return data
	}
	all["entry_id"] = json.RawMessage(`"` + r.ID + `"`)
	delete(all, "type")
	delete(all, "id")
	delete(all, "parent")
	out, err := json.Marshal(all)
	if err != nil {
		return data
	}
	return out
}

// itemText returns the readable text of an item.
func itemText(item openresponses.Item) string {
	switch v := item.(type) {
	case *openresponses.Message:
		return v.Text()
	case *openresponses.ReasoningItem:
		if t := v.Content.Text(); t != "" {
			return t
		}
		return v.Summary.Text()
	case *openresponses.FunctionCall:
		return v.Name + "(" + v.Arguments + ")"
	case *openresponses.FunctionCallOutput:
		return v.Output.String()
	case *openresponses.Compaction:
		return ""
	case *openresponses.ItemReference:
		return ""
	case *openresponses.UnknownItem:
		var probe struct {
			Text    string `json:"text"`
			Content any    `json:"content"`
		}
		_ = json.Unmarshal(v.Raw, &probe)
		if probe.Text != "" {
			return probe.Text
		}
		if s, ok := probe.Content.(string); ok {
			return s
		}
		return ""
	}
	return ""
}

// messageContent converts message parts: text only becomes a string,
// anything else a part list.
func messageContent(item openresponses.Item) atif.Content {
	m, ok := item.(*openresponses.Message)
	if !ok {
		return atif.Text(itemText(item))
	}
	return contentParts(m.Content)
}

// outputContent converts a function call output.
func outputContent(d openresponses.FunctionCallOutputData) atif.Content {
	if d.Parts == nil {
		return atif.Text(d.Text)
	}
	return contentParts(d.Parts)
}

func contentParts(parts openresponses.Contents) atif.Content {
	var out []atif.ContentPart
	textOnly := true
	for _, p := range parts {
		switch v := p.(type) {
		case *openresponses.InputText:
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: v.Text})
		case *openresponses.OutputText:
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: v.Text})
		case *openresponses.Text:
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: v.Text})
		case *openresponses.Refusal:
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: "[refusal] " + v.Refusal})
		case *openresponses.InputImage:
			ref := v.ImageURL
			if ref == "" {
				ref = v.FileID
			}
			mt := imageMediaType(v.ImageURL)
			if mt == "" {
				// A type Harbor's models refuse: keep the reference as
				// text, as file and video parts already are.
				out = append(out, atif.ContentPart{Type: atif.PartText, Text: "[image: " + describeRef(ref) + "]"})
				continue
			}
			textOnly = false
			out = append(out, atif.ContentPart{Type: atif.PartImage, Source: &atif.MediaSource{MediaType: mt, Path: ref}})
		case *openresponses.InputFile:
			name := v.Filename
			if name == "" {
				name = v.FileID
			}
			if name == "" {
				name = v.FileURL
			}
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: "[file: " + name + "]"})
		case *openresponses.InputVideo:
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: "[video: " + v.VideoURL + "]"})
		default:
			data, _ := json.Marshal(p)
			out = append(out, atif.ContentPart{Type: atif.PartText, Text: string(data)})
		}
	}
	if textOnly {
		var sb strings.Builder
		for _, p := range out {
			sb.WriteString(p.Text)
		}
		return atif.Text(sb.String())
	}
	return atif.Content{Parts: out}
}

// imageMediaType returns the ATIF media type of an image URL when it
// is one of the four Harbor's models accept, read from a data URL
// prefix or a file extension, with a bare extensionless URL taken as
// PNG. It returns "" for a type Harbor refuses, such as SVG or HEIC,
// so the caller can degrade the part rather than emit a document that
// fails to load later.
func imageMediaType(url string) string {
	lower := strings.ToLower(url)
	if strings.HasPrefix(lower, "data:") {
		mt, _, _ := strings.Cut(strings.TrimPrefix(lower, "data:"), ";")
		mt, _, _ = strings.Cut(mt, ",")
		if mt == "image/jpg" {
			mt = "image/jpeg"
		}
		for _, known := range atif.ImageMediaTypes {
			if mt == known {
				return mt
			}
		}
		return ""
	}
	path := lower
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	switch ext := path[strings.LastIndex(path, ".")+1:]; {
	case !strings.Contains(path[strings.LastIndex(path, "/")+1:], "."):
		return "image/png" // no extension to go on
	case ext == "jpg", ext == "jpeg":
		return "image/jpeg"
	case ext == "png":
		return "image/png"
	case ext == "gif":
		return "image/gif"
	case ext == "webp":
		return "image/webp"
	}
	return ""
}

// describeRef shortens a data URL for a text placeholder.
func describeRef(ref string) string {
	if strings.HasPrefix(strings.ToLower(ref), "data:") {
		mt, _, _ := strings.Cut(strings.TrimPrefix(ref, "data:"), ";")
		return "data URL " + mt
	}
	return ref
}

// parseArguments decodes a function call's arguments into an object.
// Arguments that are not a JSON object ride under "_arguments".
func parseArguments(s string) map[string]any {
	var m map[string]any
	if s != "" && json.Unmarshal([]byte(s), &m) == nil && m != nil {
		return m
	}
	if strings.TrimSpace(s) == "" {
		return map[string]any{}
	}
	return map[string]any{"_arguments": s}
}

// toolDefinitions renders tools in the OpenAI function-calling shape
// ATIF expects; extension tools are passed through as they are.
func toolDefinitions(tools openresponses.Tools) []json.RawMessage {
	if len(tools) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(tools))
	for _, t := range tools {
		switch v := t.(type) {
		case *openresponses.FunctionTool:
			fn := map[string]any{"name": v.Name, "description": v.Description}
			if len(v.Parameters) > 0 {
				fn["parameters"] = json.RawMessage(v.Parameters)
			}
			if v.Strict != nil {
				fn["strict"] = *v.Strict
			}
			data, _ := json.Marshal(map[string]any{"type": "function", "function": fn})
			out = append(out, data)
		default:
			data, _ := json.Marshal(t)
			out = append(out, data)
		}
	}
	return out
}

// toolChange describes the difference between two tool lists by name,
// or nil when there is none.
func toolChange(before, after openresponses.Tools) map[string]any {
	names := func(ts openresponses.Tools) map[string]bool {
		m := map[string]bool{}
		for _, t := range ts {
			m[agentsession.ToolName(t)] = true
		}
		return m
	}
	b, a := names(before), names(after)
	var added, removed []string
	for n := range a {
		if !b[n] {
			added = append(added, n)
		}
	}
	for n := range b {
		if !a[n] {
			removed = append(removed, n)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return nil
	}
	change := map[string]any{}
	if len(added) > 0 {
		sortStrings(added)
		change["added"] = added
	}
	if len(removed) > 0 {
		sortStrings(removed)
		change["removed"] = removed
	}
	return change
}

func envExtra(e *agentsession.EnvEntry) map[string]any {
	env := map[string]any{"entry_id": e.ID}
	if e.CWD != "" {
		env["cwd"] = e.CWD
	}
	if e.VCS != nil {
		env["vcs"] = e.VCS
	}
	if e.Files != nil {
		env["files"] = e.Files
	}
	if len(e.Tools) > 0 {
		env["tools"] = e.Tools
	}
	if e.Workspace != nil {
		env["workspace"] = e.Workspace
	}
	return copyUnknown(env, e.Unknown)
}

// copyUnknown passes an entry's undefined members through to the
// record built from its defined ones, as the format's forward
// compatibility requires of a consumer. Members are copied under their
// own names, never promoted or renamed. It returns rec.
func copyUnknown(rec map[string]any, unknown map[string]json.RawMessage) map[string]any {
	for k, raw := range unknown {
		rec[k] = json.RawMessage(raw)
	}
	return rec
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// ErrNoRawItems is returned by [Items] for a document that was not
// written by this package.
var ErrNoRawItems = errors.New("export: document carries no raw items")

// Items rebuilds the item list of the path a document was exported
// from, reading the raw items every step carries under
// extra.openresponses. Steps without them are skipped; a document with
// none at all yields ErrNoRawItems.
func Items(doc *atif.Trajectory) (openresponses.Items, error) {
	var items openresponses.Items
	found := false
	for _, s := range doc.Steps {
		or, ok := s.Extra[ExtraOpenResponses].(map[string]any)
		if !ok {
			continue
		}
		var raws []any
		if item, ok := or["item"]; ok {
			raws = append(raws, item)
		}
		// A fold's pinned items follow its summary, which is the order
		// the context algorithm places them in.
		if list, ok := or["pinned"]; ok {
			switch l := list.(type) {
			case []any:
				raws = append(raws, l...)
			case []json.RawMessage:
				for _, r := range l {
					raws = append(raws, r)
				}
			}
		}
		if list, ok := or["items"]; ok {
			switch l := list.(type) {
			case []any:
				raws = append(raws, l...)
			case []json.RawMessage:
				for _, r := range l {
					raws = append(raws, r)
				}
			}
		}
		for _, raw := range raws {
			found = true
			data, err := json.Marshal(raw)
			if err != nil {
				return nil, fmt.Errorf("export: step %d: %w", s.StepID, err)
			}
			item, err := openresponses.UnmarshalItem(data)
			if err != nil {
				return nil, fmt.Errorf("export: step %d: %w", s.StepID, err)
			}
			items = append(items, item)
		}
		// Function call outputs attached as observations come after the
		// call's items, in result order.
		if s.Observation != nil {
			for _, r := range s.Observation.Results {
				or, ok := r.Extra[ExtraOpenResponses].(map[string]any)
				if !ok {
					continue
				}
				raw, ok := or["item"]
				if !ok {
					continue
				}
				found = true
				data, err := json.Marshal(raw)
				if err != nil {
					return nil, fmt.Errorf("export: step %d: %w", s.StepID, err)
				}
				item, err := openresponses.UnmarshalItem(data)
				if err != nil {
					return nil, fmt.Errorf("export: step %d: %w", s.StepID, err)
				}
				items = append(items, item)
			}
		}
	}
	if !found {
		return nil, ErrNoRawItems
	}
	return items, nil
}
