// Package otel projects an agent session onto OpenTelemetry spans, as
// RFC 0001 defines the projection: every span carries the session ID
// and the ID of the entry it corresponds to; a tool span links to the
// inference span whose output contained the call; each inference span
// links to the previous turn's; and the first span after a branch
// links to the branched-from entry when that entry's span is known.
//
// [Export] replays a stored session's path into a tracer, with the
// entries' own timestamps, so a session recorded anywhere can be
// looked at in a trace viewer. [Wrap] decorates a store so a live
// harness emits the same spans as it appends, without the harness
// knowing about tracing. Both run one engine, so the trace of a live
// run and the trace of its replayed file are the same shape.
//
// Span names follow the OpenTelemetry generative AI conventions where
// they exist, "chat <model>" for an inference and "execute_tool
// <name>" for a call, and the gen_ai.* attributes are used for what
// they cover. Everything the format adds beyond that is under
// agentsession.*.
package otel

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Span names.
const (
	SpanSession    = "agentsession.session"
	SpanRun        = "agentsession.run"
	SpanCompaction = "agentsession.compaction"
	SpanBranch     = "agentsession.branch"
	// OpInference and OpTool are the gen_ai.operation.name values and
	// the prefixes of the inference and tool span names.
	OpInference = "chat"
	OpTool      = "execute_tool"
)

// Attribute keys. The session ID key is the one the RFC names; the
// gen_ai keys are the OpenTelemetry generative AI conventions.
const (
	AttrSessionID      = "session.id"
	AttrEntryID        = "agentsession.entry.id"
	AttrFormat         = "agentsession.format"
	AttrHarnessName    = "agentsession.harness.name"
	AttrHarnessVersion = "agentsession.harness.version"
	AttrSessionName    = "agentsession.name"
	AttrResumed        = "agentsession.resumed"
	AttrRunID          = "agentsession.run.id"
	AttrRunSource      = "agentsession.run.source"
	AttrRunTrigger     = "agentsession.run.trigger"
	AttrRunReason      = "agentsession.run.reason"
	AttrRunCause       = "agentsession.run.cause"
	AttrRunPending     = "agentsession.run.pending"
	AttrCallState      = "agentsession.call.state"
	AttrArgsRewritten  = "agentsession.call.args_rewritten"
	AttrRequestHash    = "agentsession.request_hash"
	AttrBranchFrom     = "agentsession.branch.from"
	AttrFirstKept      = "agentsession.compaction.first_kept"
	AttrTokensBefore   = "agentsession.compaction.tokens_before"
	AttrSubsession     = "agentsession.subsession"
	AttrDecisionBy     = "agentsession.decision.by"
	AttrDecisionReason = "agentsession.decision.reason"
	AttrVerdict        = "agentsession.decision.verdict"
	AttrOutcomeKind    = "agentsession.outcome.kind"
	AttrOutcomeTarget  = "agentsession.outcome.target"
	AttrOutcomeScore   = "agentsession.outcome.score"
	AttrOutcomePass    = "agentsession.outcome.pass"
	AttrLinkRel        = "agentsession.link.rel"
	AttrLinkSession    = "agentsession.link.session"

	AttrOperation     = "gen_ai.operation.name"
	AttrRequestModel  = "gen_ai.request.model"
	AttrResponseModel = "gen_ai.response.model"
	AttrResponseID    = "gen_ai.response.id"
	AttrInputTokens   = "gen_ai.usage.input_tokens"
	AttrOutputTokens  = "gen_ai.usage.output_tokens"
	AttrToolName      = "gen_ai.tool.name"
	AttrToolCallID    = "gen_ai.tool.call.id"
)

// Event names.
const (
	EventDecision = "decision"
	EventEnv      = "env"
	EventOutcome  = "outcome"
	EventLink     = "link"
)

// Export replays the path to leaf into tracer as spans, with the
// entries' own timestamps, under any span in ctx. Pending calls are
// closed at the path's last timestamp with their state as the format
// reads it. It returns the span contexts created, by entry ID, so a
// second export of another leaf can link the first span after a
// branch to the branched-from entry: pass them back through
// [WithKnownSpans].
func Export(ctx context.Context, tracer trace.Tracer, s *agentsession.Session, leaf string, opts ...ExportOption) (map[string]trace.SpanContext, error) {
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("otel: %w: %s", agentsession.ErrNoEntry, leaf)
	}
	var cfg exportConfig
	for _, o := range opts {
		o(&cfg)
	}
	h := s.Header()
	t := newTracker(ctx, tracer, h, s.Name(), h.CreatedAt, false)
	for id, sc := range cfg.known {
		t.spans[id] = sc
	}
	for _, e := range path {
		t.entry(e)
	}
	end := h.CreatedAt
	if n := len(path); n > 0 {
		end = path[n-1].Base().Timestamp
	}
	t.finish(end)
	return t.spans, nil
}

// ExportOption configures Export.
type ExportOption func(*exportConfig)

type exportConfig struct {
	known map[string]trace.SpanContext
}

// WithKnownSpans supplies span contexts from an earlier Export of the
// same session, so a branch on this path can link to the entry it
// left when that entry's span was created then.
func WithKnownSpans(spans map[string]trace.SpanContext) ExportOption {
	return func(c *exportConfig) { c.known = spans }
}

// Store decorates an [agentsession.Store] so every append emits the
// spans the format projects, as [Export] would emit them for the file
// afterwards. Create starts a session span; Open starts one marked
// resumed and primes it with the calls already on the path so a
// resume's outputs close spans for them; Append feeds the entry;
// Delete and [Store.Close] end the session's spans.
type Store struct {
	agentsession.Store
	tracer trace.Tracer

	mu   sync.Mutex
	live map[string]*tracker
}

// Wrap returns a Store over inner that emits spans through tracer.
func Wrap(inner agentsession.Store, tracer trace.Tracer) *Store {
	return &Store{Store: inner, tracer: tracer, live: map[string]*tracker{}}
}

// Create implements agentsession.Store and starts the session span.
func (s *Store) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	sess, err := s.Store.Create(ctx, h)
	if err != nil {
		return nil, err
	}
	hdr := sess.Header()
	s.mu.Lock()
	s.live[hdr.ID] = newTracker(ctx, s.tracer, hdr, sess.Name(), hdr.CreatedAt, false)
	s.mu.Unlock()
	return sess, nil
}

// Open implements agentsession.Store. A session this store has not
// seen gets a session span marked resumed, primed with the calls on
// the current path so later outputs close their spans.
func (s *Store) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	sess, err := s.Store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if _, ok := s.live[id]; !ok {
		t := newTracker(ctx, s.tracer, sess.Header(), sess.Name(), time.Now(), true)
		t.prime(sess.Path(sess.Leaf()))
		s.live[id] = t
	}
	s.mu.Unlock()
	return sess, nil
}

// Append implements agentsession.Store and feeds the entry to the
// session's spans once the inner store has accepted it.
func (s *Store) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	id, err := s.Store.Append(ctx, sessionID, e)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	t, ok := s.live[sessionID]
	s.mu.Unlock()
	if !ok {
		// Appended without Create or Open through this store: start
		// tracing from here.
		if _, err := s.Open(ctx, sessionID); err != nil {
			return id, nil
		}
		s.mu.Lock()
		t = s.live[sessionID]
		s.mu.Unlock()
	}
	t.entry(e)
	return id, nil
}

// Delete implements agentsession.Store, ending the session's spans
// first.
func (s *Store) Delete(ctx context.Context, id string) error {
	s.Close(id)
	return s.Store.Delete(ctx, id)
}

// Close ends the spans of a session this store is tracing, closing
// pending calls with their state, and forgets it. A session that is
// not being traced is ignored. Call it when the harness is done with
// the session; nothing else ends the session span.
func (s *Store) Close(id string) {
	s.mu.Lock()
	t, ok := s.live[id]
	delete(s.live, id)
	s.mu.Unlock()
	if ok {
		t.finish(time.Now())
	}
}

// CloseAll ends the spans of every session this store is tracing.
func (s *Store) CloseAll() {
	s.mu.Lock()
	all := s.live
	s.live = map[string]*tracker{}
	s.mu.Unlock()
	now := time.Now()
	for _, t := range all {
		t.finish(now)
	}
}

// tracker is the engine: it consumes entries in path order and keeps
// the open spans. Export and Store both drive it.
type tracker struct {
	tracer trace.Tracer
	header agentsession.Header

	sessionCtx context.Context
	session    trace.Span
	runCtx     context.Context
	run        trace.Span
	runID      string
	runStart   time.Time

	model         string
	lastInference trace.SpanContext
	inference     map[string]trace.SpanContext // by response ID
	firstItemTS   map[string]time.Time         // first output item per response ID
	spans         map[string]trace.SpanContext // by entry ID
	calls         map[string]*callState
	order         []string // call IDs in the order seen
	branchLink    *trace.Link
}

type callState struct {
	name, callID, entryID, responseID string
	seen                              time.Time
	span                              trace.Span
	events                            []pendingEvent
	rewritten, held, rejected         bool
	dispatched                        bool
	output                            bool
}

type pendingEvent struct {
	at    time.Time
	attrs []attribute.KeyValue
}

func newTracker(ctx context.Context, tracer trace.Tracer, h agentsession.Header, name string, at time.Time, resumed bool) *tracker {
	t := &tracker{
		tracer:      tracer,
		header:      h,
		inference:   map[string]trace.SpanContext{},
		firstItemTS: map[string]time.Time{},
		spans:       map[string]trace.SpanContext{},
		calls:       map[string]*callState{},
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrSessionID, h.ID),
		attribute.String(AttrFormat, h.Format),
	}
	if h.Harness != nil {
		attrs = append(attrs, attribute.String(AttrHarnessName, h.Harness.Name))
		if h.Harness.Version != "" {
			attrs = append(attrs, attribute.String(AttrHarnessVersion, h.Harness.Version))
		}
	}
	if name != "" {
		attrs = append(attrs, attribute.String(AttrSessionName, name))
	}
	if resumed {
		attrs = append(attrs, attribute.Bool(AttrResumed, true))
	}
	t.sessionCtx, t.session = tracer.Start(ctx, SpanSession, trace.WithTimestamp(at), trace.WithAttributes(attrs...))
	t.runCtx = t.sessionCtx
	return t
}

// prime registers the calls and settings on an existing path without
// emitting spans, so a resumed session's outputs close spans for the
// calls the previous process left pending.
func (t *tracker) prime(path []agentsession.Entry) {
	for _, e := range path {
		switch v := e.(type) {
		case *agentsession.ConfigEntry:
			if v.Model != "" {
				t.model = v.Model
			}
		case *agentsession.CompactionEntry:
			if v.Config.Model != "" {
				t.model = v.Config.Model
			}
		case *agentsession.ItemEntry:
			switch it := v.Item.(type) {
			case *openresponses.FunctionCall:
				t.register(v, it)
			case *openresponses.FunctionCallOutput:
				if c, ok := t.calls[it.CallID]; ok {
					c.output = true
				}
			}
		case *agentsession.DecisionEntry:
			if c, ok := t.calls[v.CallID]; ok {
				t.noteDecision(c, v)
			}
		case *agentsession.DispatchEntry:
			if c, ok := t.calls[v.CallID]; ok {
				c.dispatched = true
			}
		}
	}
}

func (t *tracker) register(e *agentsession.ItemEntry, fc *openresponses.FunctionCall) {
	if _, ok := t.calls[fc.CallID]; ok {
		return
	}
	t.calls[fc.CallID] = &callState{name: fc.Name, callID: fc.CallID, entryID: e.ID, responseID: e.ResponseID, seen: e.Timestamp}
	t.order = append(t.order, fc.CallID)
}

func (t *tracker) noteDecision(c *callState, d *agentsession.DecisionEntry) {
	switch d.Verdict {
	case agentsession.VerdictHold:
		c.held = true
	case agentsession.VerdictReject:
		c.rejected = true
	}
	if len(d.Args) > 0 {
		c.rewritten = true
	}
}

// entry feeds one entry, in path order, to the open spans.
func (t *tracker) entry(e agentsession.Entry) {
	ts := e.Base().Timestamp
	switch v := e.(type) {
	case *agentsession.ConfigEntry:
		if v.Model != "" {
			t.model = v.Model
		}
	case *agentsession.ItemEntry:
		switch it := v.Item.(type) {
		case *openresponses.FunctionCall:
			t.register(v, it)
			if v.ResponseID != "" {
				if _, ok := t.firstItemTS[v.ResponseID]; !ok {
					t.firstItemTS[v.ResponseID] = ts
				}
			}
		case *openresponses.FunctionCallOutput:
			t.output(it.CallID, v, ts)
		default:
			if v.ResponseID != "" {
				if _, ok := t.firstItemTS[v.ResponseID]; !ok {
					t.firstItemTS[v.ResponseID] = ts
				}
			}
		}
	case *agentsession.ResponseEntry:
		t.response(v, ts)
	case *agentsession.RunEntry:
		if v.IsStart() {
			t.startRun(v, ts)
		} else {
			t.endRun(v, ts)
		}
	case *agentsession.DecisionEntry:
		t.decision(v, ts)
	case *agentsession.DispatchEntry:
		t.dispatch(v, ts)
	case *agentsession.CompactionEntry:
		if v.Config.Model != "" {
			t.model = v.Config.Model
		}
		attrs := []attribute.KeyValue{attribute.String(AttrFirstKept, v.FirstKept)}
		if v.TokensBefore > 0 {
			attrs = append(attrs, attribute.Int(AttrTokensBefore, v.TokensBefore))
		}
		attrs = append(attrs, usageAttrs(v.Usage)...)
		t.instant(SpanCompaction, e, ts, attrs)
	case *agentsession.BranchSummaryEntry:
		attrs := []attribute.KeyValue{attribute.String(AttrBranchFrom, v.From)}
		if sc, ok := t.spans[v.From]; ok {
			t.branchLink = &trace.Link{SpanContext: sc, Attributes: []attribute.KeyValue{attribute.String(AttrBranchFrom, v.From)}}
		}
		t.instant(SpanBranch, e, ts, attrs)
	case *agentsession.EnvEntry:
		attrs := []attribute.KeyValue{attribute.String(AttrEntryID, v.ID)}
		if v.CWD != "" {
			attrs = append(attrs, attribute.String("cwd", v.CWD))
		}
		if v.Workspace != nil {
			attrs = append(attrs, attribute.String("workspace.kind", v.Workspace.Kind))
			if v.Workspace.Ref != "" {
				attrs = append(attrs, attribute.String("workspace.ref", v.Workspace.Ref))
			}
		}
		t.session.AddEvent(EventEnv, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	case *agentsession.OutcomeEntry:
		attrs := []attribute.KeyValue{attribute.String(AttrEntryID, v.ID), attribute.String(AttrOutcomeKind, v.Kind)}
		if v.Target != "" {
			attrs = append(attrs, attribute.String(AttrOutcomeTarget, v.Target))
		}
		if v.Score != nil {
			attrs = append(attrs, attribute.Float64(AttrOutcomeScore, *v.Score))
		}
		if v.Pass != nil {
			attrs = append(attrs, attribute.Bool(AttrOutcomePass, *v.Pass))
		}
		t.session.AddEvent(EventOutcome, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	case *agentsession.LinkEntry:
		attrs := []attribute.KeyValue{attribute.String(AttrEntryID, v.ID), attribute.String(AttrLinkRel, v.Rel), attribute.String(AttrLinkSession, v.Session)}
		if c, ok := t.calls[v.CallID]; ok && v.Rel == agentsession.RelSubsession {
			c.events = append(c.events, pendingEvent{at: ts, attrs: attrs})
			if c.span != nil {
				c.span.SetAttributes(attribute.String(AttrSubsession, v.Session))
			}
		}
		t.session.AddEvent(EventLink, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
	case *agentsession.InfoEntry:
		if v.Name != "" {
			t.session.SetAttributes(attribute.String(AttrSessionName, v.Name))
		}
	}
}

func (t *tracker) startRun(r *agentsession.RunEntry, ts time.Time) {
	if t.run != nil {
		// A start closes the run before it, as a branch does.
		t.run.SetAttributes(attribute.String(AttrRunReason, "cut_off"))
		t.run.End(trace.WithTimestamp(ts))
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrSessionID, t.header.ID),
		attribute.String(AttrEntryID, r.ID),
		attribute.String(AttrRunID, r.RunID),
		attribute.String(AttrRunSource, r.Source),
	}
	if r.Ref != "" {
		attrs = append(attrs, attribute.String(AttrRunTrigger, r.Ref))
	}
	opts := []trace.SpanStartOption{trace.WithTimestamp(ts), trace.WithAttributes(attrs...)}
	opts = t.withBranchLink(opts)
	t.runCtx, t.run = t.tracer.Start(t.sessionCtx, SpanRun, opts...)
	t.runID, t.runStart = r.RunID, ts
	t.spans[r.ID] = t.run.SpanContext()
}

func (t *tracker) endRun(r *agentsession.RunEntry, ts time.Time) {
	if t.run == nil || r.RunID != t.runID {
		return
	}
	attrs := []attribute.KeyValue{attribute.String(AttrRunReason, r.Reason)}
	if r.Ref != "" {
		attrs = append(attrs, attribute.String(AttrRunCause, r.Ref))
	}
	if len(r.Pending) > 0 {
		attrs = append(attrs, attribute.StringSlice(AttrRunPending, r.Pending))
	}
	t.run.SetAttributes(attrs...)
	if r.Reason == agentsession.ReasonError {
		t.run.SetStatus(codes.Error, r.Ref)
	}
	t.spans[r.ID] = t.run.SpanContext()
	t.run.End(trace.WithTimestamp(ts))
	t.run, t.runCtx = nil, t.sessionCtx
}

func (t *tracker) response(r *agentsession.ResponseEntry, ts time.Time) {
	start := ts
	if first, ok := t.firstItemTS[r.ResponseID]; ok && first.Before(start) {
		start = first
	}
	if r.LatencyMS > 0 {
		if s := ts.Add(-time.Duration(r.LatencyMS) * time.Millisecond); s.Before(start) {
			start = s
		}
	}
	model := r.Model
	if model == "" {
		model = t.model
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrSessionID, t.header.ID),
		attribute.String(AttrEntryID, r.ID),
		attribute.String(AttrOperation, OpInference),
		attribute.String(AttrRequestModel, t.model),
		attribute.String(AttrResponseID, r.ResponseID),
	}
	if r.Model != "" {
		attrs = append(attrs, attribute.String(AttrResponseModel, r.Model))
	}
	if r.RequestHash != "" {
		attrs = append(attrs, attribute.String(AttrRequestHash, r.RequestHash))
	}
	attrs = append(attrs, usageAttrs(r.Usage)...)
	opts := []trace.SpanStartOption{trace.WithTimestamp(start), trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...)}
	if t.lastInference.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: t.lastInference, Attributes: []attribute.KeyValue{attribute.String("agentsession.link", "previous_turn")}}))
	}
	opts = t.withBranchLink(opts)
	_, span := t.tracer.Start(t.runCtx, OpInference+" "+model, opts...)
	if r.Error != nil {
		span.SetStatus(codes.Error, r.Error.Message)
	}
	if r.Incomplete != nil {
		span.SetAttributes(attribute.String("gen_ai.response.finish_reasons", string(r.Incomplete.Reason)))
	}
	span.End(trace.WithTimestamp(ts))
	sc := span.SpanContext()
	t.lastInference = sc
	t.inference[r.ResponseID] = sc
	t.spans[r.ID] = sc
	// The decisions on the response's calls are made after it, so
	// nothing is pending here; the tool spans link to it when they start.
}

func (t *tracker) decision(d *agentsession.DecisionEntry, ts time.Time) {
	c, ok := t.calls[d.CallID]
	if !ok {
		return
	}
	t.noteDecision(c, d)
	attrs := []attribute.KeyValue{attribute.String(AttrEntryID, d.ID), attribute.String(AttrVerdict, d.Verdict)}
	if d.By != "" {
		attrs = append(attrs, attribute.String(AttrDecisionBy, d.By))
	}
	if d.Reason != "" {
		attrs = append(attrs, attribute.String(AttrDecisionReason, d.Reason))
	}
	if len(d.Args) > 0 {
		attrs = append(attrs, attribute.Bool(AttrArgsRewritten, true))
	}
	if c.span != nil {
		c.span.AddEvent(EventDecision, trace.WithTimestamp(ts), trace.WithAttributes(attrs...))
		return
	}
	c.events = append(c.events, pendingEvent{at: ts, attrs: attrs})
	t.spans[d.ID] = trace.SpanContext{}
}

func (t *tracker) dispatch(d *agentsession.DispatchEntry, ts time.Time) {
	c, ok := t.calls[d.CallID]
	if !ok {
		return
	}
	c.dispatched = true
	t.startCall(c, ts, d.ID)
}

// startCall opens the tool span for a call, linked to the inference
// span whose output contained it, and flushes the decisions made
// before it started as events.
func (t *tracker) startCall(c *callState, at time.Time, entryID string) {
	if c.span != nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String(AttrSessionID, t.header.ID),
		attribute.String(AttrEntryID, entryID),
		attribute.String(AttrOperation, OpTool),
		attribute.String(AttrToolName, c.name),
		attribute.String(AttrToolCallID, c.callID),
	}
	if c.rewritten {
		attrs = append(attrs, attribute.Bool(AttrArgsRewritten, true))
	}
	opts := []trace.SpanStartOption{trace.WithTimestamp(at), trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...)}
	if sc, ok := t.inference[c.responseID]; ok {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: sc, Attributes: []attribute.KeyValue{attribute.String("agentsession.link", "produced_by")}}))
	}
	opts = t.withBranchLink(opts)
	_, c.span = t.tracer.Start(t.runCtx, OpTool+" "+c.name, opts...)
	for _, ev := range c.events {
		c.span.AddEvent(EventDecision, trace.WithTimestamp(ev.at), trace.WithAttributes(ev.attrs...))
	}
	c.events = nil
	t.spans[entryID] = c.span.SpanContext()
}

func (t *tracker) output(callID string, e *agentsession.ItemEntry, ts time.Time) {
	c, ok := t.calls[callID]
	if !ok {
		return
	}
	c.output = true
	state := agentsession.CallCompleted.String()
	if c.span == nil {
		// No dispatch: a rejected call, or a file that does not record
		// dispatches. The span covers the call from the moment the
		// path knows of it to its output.
		start := c.seen
		if c.rejected && len(c.events) > 0 {
			start = c.events[0].at
			state = "rejected"
		}
		t.startCall(c, start, e.ID)
	}
	c.span.SetAttributes(attribute.String(AttrCallState, state))
	if c.rejected {
		c.span.SetAttributes(attribute.String(AttrCallState, "rejected"))
	}
	t.spans[e.ID] = c.span.SpanContext()
	c.span.End(trace.WithTimestamp(ts))
	delete(t.calls, callID)
}

// finish ends everything still open at ts: pending calls with the
// state the format reads for them, the open run, and the session.
func (t *tracker) finish(ts time.Time) {
	for _, id := range t.order {
		c, ok := t.calls[id]
		if !ok || c.output {
			continue
		}
		state := agentsession.CallUnknown
		switch {
		case c.held:
			state = agentsession.CallHeld
		case c.dispatched:
			state = agentsession.CallInFlight
		case t.header.HasRecord(agentsession.TypeDispatch):
			state = agentsession.CallNeverStarted
		}
		if c.span == nil {
			t.startCall(c, c.seen, c.entryID)
		}
		c.span.SetAttributes(attribute.String(AttrCallState, state.String()))
		if state == agentsession.CallInFlight {
			c.span.SetStatus(codes.Error, "no output before the record stopped")
		}
		c.span.End(trace.WithTimestamp(ts))
		delete(t.calls, id)
	}
	if t.run != nil {
		t.run.End(trace.WithTimestamp(ts))
		t.run, t.runCtx = nil, t.sessionCtx
	}
	t.session.End(trace.WithTimestamp(ts))
}

// instant emits a span of no duration for an entry that is an event
// in the record rather than an interval.
func (t *tracker) instant(name string, e agentsession.Entry, ts time.Time, attrs []attribute.KeyValue) {
	attrs = append(attrs, attribute.String(AttrSessionID, t.header.ID), attribute.String(AttrEntryID, e.Base().ID))
	opts := []trace.SpanStartOption{trace.WithTimestamp(ts), trace.WithAttributes(attrs...)}
	opts = t.withBranchLink(opts)
	_, span := t.tracer.Start(t.runCtx, name, opts...)
	span.End(trace.WithTimestamp(ts))
	t.spans[e.Base().ID] = span.SpanContext()
}

// withBranchLink attaches the link a branch left for the next span.
func (t *tracker) withBranchLink(opts []trace.SpanStartOption) []trace.SpanStartOption {
	if t.branchLink != nil {
		opts = append(opts, trace.WithLinks(*t.branchLink))
		t.branchLink = nil
	}
	return opts
}

func usageAttrs(u *openresponses.Usage) []attribute.KeyValue {
	if u == nil {
		return nil
	}
	return []attribute.KeyValue{
		attribute.Int(AttrInputTokens, u.InputTokens),
		attribute.Int(AttrOutputTokens, u.OutputTokens),
	}
}
