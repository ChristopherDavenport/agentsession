package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
	"github.com/ChristopherDavenport/openresponses"
)

// Record entry types added in format 0.2, and queued in 0.3. They are
// never in context.
const (
	TypeRun      = "run"
	TypeDispatch = "dispatch"
	TypeDecision = "decision"
	TypeQueued   = "queued"
)

// Modes a [QueuedEntry] may carry: an input that joins the run in
// flight, and one that waits for it to end.
const (
	ModeSteer    = "steer"
	ModeFollowUp = "followup"
)

// Phases of a [RunEntry].
const (
	RunStart = "start"
	RunEnd   = "end"
)

// Sources of a run, on its start entry. Both are shapes of the path:
// a resume answers a call that was pending when the run began; an
// input is everything else, a retry after an error included.
const (
	SourceInput  = "input"
	SourceResume = "resume"
)

// Reasons a run ended, on its end entry. The first five are computable
// from the run's segment and the path it ends by [ComputeReason];
// ReasonError and ReasonInterrupted are the two a writer adds where
// the segment cannot show them, and a written one stands over any
// segment.
const (
	ReasonDone          = "done"
	ReasonStopped       = "stopped"
	ReasonInterrupted   = "interrupted"
	ReasonInputRequired = "input_required"
	ReasonAborted       = "aborted"
	ReasonError         = "error"
)

// Verdicts a [DecisionEntry] may carry, each defined by what follows
// the decision on the path: a dispatch, an output carrying the reason,
// nothing, or an output the harness wrote for a call that may already
// have run.
const (
	VerdictProceed = "proceed"
	VerdictReject  = "reject"
	VerdictHold    = "hold"
	// VerdictAnswer ends a call that may already have run, one in
	// flight when the record stopped or one the file cannot say about,
	// with an output the harness wrote rather than one its tool
	// returned: the call is not handed to its tool again. By says who
	// answered and Reason why.
	VerdictAnswer = "answer"
)

// Deciders a [DecisionEntry] may name.
const (
	ByHuman  = "human"
	ByPolicy = "policy"
	ByAgent  = "agent"
)

// Workspace kinds an [EnvEntry] may name.
const (
	WorkspaceLocal     = "local"
	WorkspaceContainer = "container"
	WorkspaceRemote    = "remote"
)

// RunEntry marks the start or the end of one pass of the harness's
// loop. Two entries per run share a RunID. A start carries Source and
// an optional Ref naming what triggered the input in the harness's own
// terms; an end carries Reason, the IDs of the calls left pending and
// an optional Ref naming the cause.
type RunEntry struct {
	EntryBase `json:"-"`
	RunID     string `json:"run_id"`
	Phase     string `json:"phase"`
	// Source is set on a start entry.
	Source string `json:"source,omitempty"`
	// Reason is set on an end entry.
	Reason string `json:"reason,omitempty"`
	// Ref names the trigger of a start or the cause of an end in the
	// harness's own terms. Readers treat it as opaque.
	Ref string `json:"ref,omitempty"`
	// Trigger, on a start entry, says how the input that started the run
	// arrived, in parts, beside Ref and changing nothing about it. A
	// harness's richer facts about the firing, such as when it was due
	// or which attempt it is, go in [Trigger.Unknown]; members of the
	// run entry this package does not define, where format 0.6 put
	// them, are kept in [EntryBase.Unknown]. A trigger member this type
	// cannot hold exactly, one written before the member was defined,
	// is kept there as written and Trigger is nil.
	Trigger *Trigger `json:"-" member:"trigger"`
	// Pending lists, on an end entry, the IDs of the calls left without
	// an output. It is written even when empty.
	Pending []string `json:"pending"`
}

// EntryType returns "run".
func (*RunEntry) EntryType() string { return TypeRun }

// IsStart reports whether the entry opens a run.
func (e *RunEntry) IsStart() bool { return e.Phase == RunStart }

// IsEnd reports whether the entry closes a run.
func (e *RunEntry) IsEnd() bool { return e.Phase == RunEnd }

// NewRunStart builds the entry that opens a run. ref may be "".
func NewRunStart(runID, source, ref string) *RunEntry {
	return &RunEntry{RunID: runID, Phase: RunStart, Source: source, Ref: ref}
}

// NewRunEnd builds the entry that closes a run. pending lists the IDs
// of the calls left without an output; [Session.EndRun] computes it
// from the path. ref may be "".
func NewRunEnd(runID, reason, ref string, pending []string) *RunEntry {
	if pending == nil {
		pending = []string{}
	}
	return &RunEntry{RunID: runID, Phase: RunEnd, Reason: reason, Ref: ref, Pending: pending}
}

// DispatchEntry records that a call was handed to its tool. Target is
// the item entry holding the function call. A writer that names
// "dispatch" in the header's records writes it, durably, before the
// tool runs. Each dispatch for a call is one hand-off, so a call run
// again after a restart has a second.
type DispatchEntry struct {
	EntryBase `json:"-"`
	CallID    string `json:"call_id"`
	Target    string `json:"target"`
	// IdempotencyKey is the key the harness gave the tool for this
	// hand-off. A dispatch that repeats an earlier hand-off carries that
	// hand-off's key, and a new key is a new operation; the last
	// dispatch's key is what [Call.IdempotencyKey] returns. An
	// idempotency_key member
	// that is not a non-empty string, which a file from before the
	// member was defined may hold, is kept as written in Unknown and
	// IdempotencyKey is empty.
	IdempotencyKey string `json:"-" member:"idempotency_key"`
}

// EntryType returns "dispatch".
func (*DispatchEntry) EntryType() string { return TypeDispatch }

// NewDispatch builds a dispatch for the call callID held by the item
// entry target.
func NewDispatch(callID, target string) *DispatchEntry {
	return &DispatchEntry{CallID: callID, Target: target}
}

// WithIdempotencyKey records the key the tool is handed and returns
// the entry, for chaining.
func (e *DispatchEntry) WithIdempotencyKey(key string) *DispatchEntry {
	e.IdempotencyKey = key
	return e
}

// DecisionEntry records that a call's fate was decided outside the
// tool. Target is the item entry holding the function call. Reason is
// required when Verdict is [VerdictReject], since it is what the model
// saw as the output. Args, when present, are the arguments the tool
// ran with after the decision rewrote them; the function call item
// stays as the model produced it. An answer, [VerdictAnswer], ran
// nothing, so Args on one names nothing.
type DecisionEntry struct {
	EntryBase `json:"-"`
	CallID    string          `json:"call_id"`
	Target    string          `json:"target"`
	Verdict   string          `json:"verdict"`
	By        string          `json:"by,omitempty"`
	Reason    string          `json:"reason,omitempty"`
	Args      json.RawMessage `json:"args,omitempty"`
}

// EntryType returns "decision".
func (*DecisionEntry) EntryType() string { return TypeDecision }

// NewDecision builds a decision on the call callID held by the item
// entry target. by may be "".
func NewDecision(callID, target, verdict, by string) *DecisionEntry {
	return &DecisionEntry{CallID: callID, Target: target, Verdict: verdict, By: by}
}

// WithReason sets the decider's text and returns the entry, for
// chaining.
func (e *DecisionEntry) WithReason(reason string) *DecisionEntry {
	e.Reason = reason
	return e
}

// WithArgs records the arguments the tool ran with and returns the
// entry, for chaining. args must be a JSON value.
func (e *DecisionEntry) WithArgs(args json.RawMessage) *DecisionEntry {
	e.Args = append(json.RawMessage(nil), args...)
	return e
}

// Trigger says how an input arrived, in the harness's own terms: Kind
// is the sort of thing it came from (a person, a channel, a schedule,
// another agent), Ref names the thing itself (a message ID, a cron
// name) and Source names the layer that took it. A reader treats all
// three as opaque; what the format fixes is where they are written,
// so two people steering one run are two triggers and not one.
type Trigger struct {
	Kind   string `json:"kind,omitempty"`
	Ref    string `json:"ref,omitempty"`
	Source string `json:"source,omitempty"`
	// Unknown holds the members of the trigger the format does not
	// define, the harness's own facts about the arrival such as when a
	// scheduled firing was due and which attempt it is, encoded inline
	// beside kind, ref and source. It is nil when there are none.
	Unknown map[string]json.RawMessage `json:"-"`
}

// triggerKeys are the members of a trigger the format defines.
var triggerKeys = []string{"kind", "ref", "source"}

// SetMember sets a member of the trigger the format does not define,
// such as when a scheduled firing was due or which attempt it is.
func (t *Trigger) SetMember(key string, v any) error {
	if slices.Contains(triggerKeys, key) {
		return fmt.Errorf("agentsession: trigger member %q is defined; set the field", key)
	}
	data, err := jsonx.MarshalNoEscape(v)
	if err != nil {
		return fmt.Errorf("agentsession: trigger member %q: %w", key, err)
	}
	if t.Unknown == nil {
		t.Unknown = make(map[string]json.RawMessage)
	}
	t.Unknown[key] = data
	return nil
}

// Clone returns a copy of the trigger that shares no map with it, or
// nil for nil.
func (t *Trigger) Clone() *Trigger {
	if t == nil {
		return nil
	}
	out := *t
	out.Unknown = cloneRaw(t.Unknown)
	return &out
}

// Equal reports whether two triggers are the same: kind, ref, source
// and every member the format does not define, compared in canonical
// form, so the order and spacing a line gave them do not count. Two
// nil triggers are equal. A Trigger holds a map, so == does not
// compile on it.
func (t *Trigger) Equal(u *Trigger) bool {
	if t == nil || u == nil {
		return t == u
	}
	ct, err1 := canonicalJSON(t)
	cu, err2 := canonicalJSON(u)
	return err1 == nil && err2 == nil && bytes.Equal(ct, cu)
}

// MarshalJSON emits kind, ref, source and the unknown members as one
// object.
func (t Trigger) MarshalJSON() ([]byte, error) {
	for key := range t.Unknown {
		if slices.Contains(triggerKeys, key) {
			return nil, fmt.Errorf("agentsession: trigger has %s both typed and unknown", key)
		}
	}
	type plain Trigger
	b, err := jsonx.MarshalNoEscape(plain(t))
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(b, nil, t.Unknown), nil
}

// UnmarshalJSON takes kind, ref and source, spelled exactly, and keeps
// every other member in Unknown.
func (t *Trigger) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*t = Trigger{}
	for key, raw := range all {
		var err error
		switch key {
		case "kind":
			err = json.Unmarshal(raw, &t.Kind)
		case "ref":
			err = json.Unmarshal(raw, &t.Ref)
		case "source":
			err = json.Unmarshal(raw, &t.Source)
		default:
			if t.Unknown == nil {
				t.Unknown = make(map[string]json.RawMessage)
			}
			t.Unknown[key] = raw
		}
		if err != nil {
			return fmt.Errorf("trigger %s: %w", key, err)
		}
	}
	return nil
}

// QueuedEntry records an input a harness accepted before it could be
// appended to the conversation: a steer typed while the model is
// running, or a follow-up that waits for the run to end. It is a
// record entry, so the context algorithm ignores it and the item it
// holds reaches no request until it is appended as an [ItemEntry].
//
// A queued entry with no item entry naming it and no run end after it
// on the path is an input the harness still owes the conversation;
// [Session.PendingQueued] lists them, which is what a gateway that
// answered 202 drains on resume. The entry is where the trigger of
// that input lives, since the run entry names the trigger of the run
// and an input that joins a run in flight has a different one.
type QueuedEntry struct {
	EntryBase `json:"-"`
	// Item is the input as it will be appended.
	Item openresponses.Item `json:"item"`
	// Mode is ModeSteer or ModeFollowUp.
	Mode string `json:"mode"`
	// Trigger is what brought the input in.
	Trigger *Trigger `json:"trigger,omitempty"`
	// Ref names the queued input in the harness's own terms, for a
	// caller that holds a handle to it.
	Ref string `json:"ref,omitempty"`
}

// EntryType returns "queued".
func (*QueuedEntry) EntryType() string { return TypeQueued }

// NewQueued builds a queued entry for an input accepted in mode
// [ModeSteer] or [ModeFollowUp].
func NewQueued(item openresponses.Item, mode string) *QueuedEntry {
	return &QueuedEntry{Item: item, Mode: mode}
}

// WithTrigger records what brought the input in and returns the
// entry, for chaining.
func (e *QueuedEntry) WithTrigger(kind, ref, source string) *QueuedEntry {
	e.Trigger = &Trigger{Kind: kind, Ref: ref, Source: source}
	return e
}

// Drain returns the item entry that appends this queued input to the
// conversation: the item, the trigger as its source and QueuedFrom
// naming this entry, so the record says why the item is there. The
// entry must already have an ID, which it has once it is appended.
// The trigger is copied whole, the members the format does not define
// included, so the two entries do not share one once both are appended
// and neither may be modified.
func (e *QueuedEntry) Drain() *ItemEntry {
	return &ItemEntry{Item: e.Item, QueuedFrom: e.ID, Source: e.Trigger.Clone()}
}

// Workspace says which file system an env entry's cwd is a path in.
// Ref is one string the harness can resolve to it: an image digest, a
// host, an instance ID. A container's Ref should be a digest rather
// than a tag, because a tag moves. Anything else that tells one file
// system from another, the host a container runs on or its instance,
// goes in Unknown, inside workspace and not beside it, since the
// format's substitution rule compares workspace alone: see
// [SameWorkspace].
type Workspace struct {
	Kind string `json:"kind,omitempty"`
	Ref  string `json:"ref,omitempty"`
	// Unknown holds the members of workspace the format does not
	// define, such as host or instance, encoded inline beside kind and
	// ref. It is nil when there are none.
	Unknown map[string]json.RawMessage `json:"-"`
}

// SetMember sets a member of the workspace the format does not
// define, such as the host a container runs on or its instance.
func (w *Workspace) SetMember(key string, v any) error {
	if key == "kind" || key == "ref" {
		return fmt.Errorf("agentsession: workspace member %q is defined; set the field", key)
	}
	data, err := jsonx.MarshalNoEscape(v)
	if err != nil {
		return fmt.Errorf("agentsession: workspace member %q: %w", key, err)
	}
	if w.Unknown == nil {
		w.Unknown = make(map[string]json.RawMessage)
	}
	w.Unknown[key] = data
	return nil
}

// MarshalJSON emits kind, ref and the unknown members as one object.
func (w Workspace) MarshalJSON() ([]byte, error) {
	for key := range w.Unknown {
		if key == "kind" || key == "ref" {
			return nil, fmt.Errorf("agentsession: workspace has %s both typed and unknown", key)
		}
	}
	type plain Workspace
	b, err := jsonx.MarshalNoEscape(plain(w))
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(b, nil, w.Unknown), nil
}

// UnmarshalJSON takes kind and ref, spelled exactly, and keeps every
// other member in Unknown.
func (w *Workspace) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*w = Workspace{}
	for key, raw := range all {
		var err error
		switch key {
		case "kind":
			err = json.Unmarshal(raw, &w.Kind)
		case "ref":
			err = json.Unmarshal(raw, &w.Ref)
		default:
			if w.Unknown == nil {
				w.Unknown = make(map[string]json.RawMessage)
			}
			w.Unknown[key] = raw
		}
		if err != nil {
			return fmt.Errorf("workspace %s: %w", key, err)
		}
	}
	return nil
}

// SameWorkspace reports whether two workspace members are the same
// under the format's substitution rule: compared member by member in
// their canonical form, the ones the format does not define included,
// and an absent one equal only to another absent one. An empty kind or
// ref is the same as an absent one, as the format says. An env entry whose workspace is not the same as
// the one in force before it on the path is a substitution.
func SameWorkspace(a, b *Workspace) bool {
	if a == nil || b == nil {
		return a == b
	}
	ca, err1 := canonicalWorkspace(a)
	cb, err2 := canonicalWorkspace(b)
	return err1 == nil && err2 == nil && bytes.Equal(ca, cb)
}

func canonicalWorkspace(w *Workspace) ([]byte, error) { return canonicalJSON(w) }

// canonicalJSON is v's encoding in the canonical form the entry hash
// writes.
func canonicalJSON(v json.Marshaler) ([]byte, error) {
	data, err := v.MarshalJSON()
	if err != nil {
		return nil, err
	}
	return jcs.Transform(data)
}

// validateLifecycle checks the members the format requires on the
// three record entries, before they are appended or encoded.
func validateLifecycle(e Entry) error {
	switch v := e.(type) {
	case *RunEntry:
		if v.RunID == "" {
			return errors.New("agentsession: run entry has no run_id")
		}
		switch v.Phase {
		case RunStart:
			if v.Source == "" {
				return errors.New("agentsession: run start has no source")
			}
		case RunEnd:
			if v.Reason == "" {
				return errors.New("agentsession: run end has no reason")
			}
		default:
			return fmt.Errorf("agentsession: run entry phase %q", v.Phase)
		}
	case *DispatchEntry:
		if v.CallID == "" || v.Target == "" {
			return errors.New("agentsession: dispatch entry needs call_id and target")
		}
	case *DecisionEntry:
		if v.CallID == "" || v.Target == "" || v.Verdict == "" {
			return errors.New("agentsession: decision entry needs call_id, target and verdict")
		}
		if v.Verdict == VerdictReject && v.Reason == "" {
			return errors.New("agentsession: a reject decision needs a reason")
		}
	case *QueuedEntry:
		if v.Item == nil {
			return errors.New("agentsession: queued entry has no item")
		}
		if v.Mode == "" {
			return errors.New("agentsession: queued entry has no mode")
		}
	}
	return nil
}

var (
	runKeys      = jsonx.Keys[RunEntry]()
	dispatchKeys = jsonx.Keys[DispatchEntry]()
	decisionKeys = jsonx.Keys[DecisionEntry]()
	queuedKeys   = jsonx.Keys[QueuedEntry]()
)

// MarshalJSON emits the entry as one JSON object. Which members are
// written depends on the phase: a start carries source, an end carries
// reason and always carries pending, even when empty, since a run that
// ended owing nothing is a fact a reader relies on.
//
// The other phase's members are written when they are set rather than
// dropped. Nothing this library builds sets them, and validateLifecycle
// asks only for the phase's own, so a well-formed entry is written
// exactly as it was before. A file from elsewhere that carries one is
// the case that matters: the envelope rule is that a reader preserves
// what it does not itself need, and rewriting such a file to drop a
// member it declared breaks that promise silently. Rejecting the shape
// on the way in would be the alternative, and a larger decision than
// an encoder.
func (e *RunEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	if e.Trigger != nil {
		if _, dup := e.Unknown["trigger"]; dup {
			return nil, errors.New("agentsession: run entry has trigger both typed and unknown")
		}
	}
	if e.Phase == RunStart {
		aux := struct {
			RunID   string   `json:"run_id"`
			Phase   string   `json:"phase"`
			Source  string   `json:"source"`
			Reason  string   `json:"reason,omitempty"`
			Ref     string   `json:"ref,omitempty"`
			Pending []string `json:"pending,omitempty"`
			Trigger *Trigger `json:"trigger,omitempty"`
		}{e.RunID, e.Phase, e.Source, e.Reason, e.Ref, e.Pending, e.Trigger}
		return marshalEntry(TypeRun, &e.EntryBase, aux)
	}
	pending := e.Pending
	if pending == nil {
		pending = []string{}
	}
	aux := struct {
		RunID   string   `json:"run_id"`
		Phase   string   `json:"phase"`
		Source  string   `json:"source,omitempty"`
		Reason  string   `json:"reason"`
		Ref     string   `json:"ref,omitempty"`
		Pending []string `json:"pending"`
		Trigger *Trigger `json:"trigger,omitempty"`
	}{e.RunID, e.Phase, e.Source, e.Reason, e.Ref, pending, e.Trigger}
	return marshalEntry(TypeRun, &e.EntryBase, aux)
}

// UnmarshalJSON decodes the entry.
func (e *RunEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *RunEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain RunEntry
	e.Trigger = nil
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), runKeys); err != nil {
		return err
	}
	promote(&e.EntryBase, "trigger", &e.Trigger)
	return validateLifecycle(e)
}

// MarshalJSON emits the entry as one JSON object.
func (e *DispatchEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	type plain DispatchEntry
	if e.IdempotencyKey != "" {
		if _, dup := e.Unknown["idempotency_key"]; dup {
			return nil, errors.New("agentsession: dispatch entry has idempotency_key both typed and unknown")
		}
	}
	return marshalEntry(TypeDispatch, &e.EntryBase, struct {
		*plain
		IdempotencyKey string `json:"idempotency_key,omitempty"`
	}{(*plain)(e), e.IdempotencyKey})
}

// UnmarshalJSON decodes the entry.
func (e *DispatchEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *DispatchEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain DispatchEntry
	e.IdempotencyKey = ""
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), dispatchKeys); err != nil {
		return err
	}
	promote(&e.EntryBase, "idempotency_key", &e.IdempotencyKey)
	return validateLifecycle(e)
}

// MarshalJSON emits the entry as one JSON object.
func (e *DecisionEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	type plain DecisionEntry
	return marshalEntry(TypeDecision, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *DecisionEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *DecisionEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain DecisionEntry
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), decisionKeys); err != nil {
		return err
	}
	return validateLifecycle(e)
}

// MarshalJSON emits the entry as one JSON object.
func (e *QueuedEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	type plain QueuedEntry
	return marshalEntry(TypeQueued, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry, dispatching the item through the
// openresponses item registry.
func (e *QueuedEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *QueuedEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	var aux struct {
		Item    json.RawMessage `json:"item"`
		Mode    string          `json:"mode"`
		Trigger *Trigger        `json:"trigger"`
		Ref     string          `json:"ref"`
	}
	if err := unmarshalEntry(data, all, &e.EntryBase, &aux, queuedKeys); err != nil {
		return err
	}
	if len(aux.Item) == 0 || isNull(aux.Item) {
		return errors.New("item is required")
	}
	item, err := openresponses.UnmarshalItem(aux.Item)
	if err != nil {
		return err
	}
	e.Item = item
	e.Mode = aux.Mode
	e.Trigger = aux.Trigger
	e.Ref = aux.Ref
	return validateLifecycle(e)
}

var (
	_ Entry = (*RunEntry)(nil)
	_ Entry = (*DispatchEntry)(nil)
	_ Entry = (*DecisionEntry)(nil)
	_ Entry = (*QueuedEntry)(nil)
)
