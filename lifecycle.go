package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// Record entry types added in format 0.2. They are never in context.
const (
	TypeRun      = "run"
	TypeDispatch = "dispatch"
	TypeDecision = "decision"
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
// from the run's segment by [ComputeReason]; ReasonError and
// ReasonInterrupted are the two a writer adds where the segment cannot
// show them, and a written one stands over any segment.
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
// or nothing.
const (
	VerdictProceed = "proceed"
	VerdictReject  = "reject"
	VerdictHold    = "hold"
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
// tool runs.
type DispatchEntry struct {
	EntryBase `json:"-"`
	CallID    string `json:"call_id"`
	Target    string `json:"target"`
}

// EntryType returns "dispatch".
func (*DispatchEntry) EntryType() string { return TypeDispatch }

// NewDispatch builds a dispatch for the call callID held by the item
// entry target.
func NewDispatch(callID, target string) *DispatchEntry {
	return &DispatchEntry{CallID: callID, Target: target}
}

// DecisionEntry records that a call's fate was decided outside the
// tool. Target is the item entry holding the function call. Reason is
// required when Verdict is [VerdictReject], since it is what the model
// saw as the output. Args, when present, are the arguments the tool
// ran with after the decision rewrote them; the function call item
// stays as the model produced it.
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

// Workspace says which file system an env entry's cwd is a path in.
// Ref is one string the harness can resolve to it: an image digest, a
// host, an instance ID. A container's Ref should be a digest rather
// than a tag, because a tag moves. Anything richer goes in unknown
// members of the env entry.
type Workspace struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref,omitempty"`
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
	}
	return nil
}

var (
	runKeys      = jsonx.Keys[RunEntry]()
	dispatchKeys = jsonx.Keys[DispatchEntry]()
	decisionKeys = jsonx.Keys[DecisionEntry]()
)

// MarshalJSON emits the entry as one JSON object. A start entry omits
// pending; an end entry always carries it.
func (e *RunEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	if e.Phase == RunStart {
		aux := struct {
			RunID  string `json:"run_id"`
			Phase  string `json:"phase"`
			Source string `json:"source"`
			Ref    string `json:"ref,omitempty"`
		}{e.RunID, e.Phase, e.Source, e.Ref}
		return marshalEntry(TypeRun, &e.EntryBase, aux)
	}
	pending := e.Pending
	if pending == nil {
		pending = []string{}
	}
	aux := struct {
		RunID   string   `json:"run_id"`
		Phase   string   `json:"phase"`
		Reason  string   `json:"reason"`
		Ref     string   `json:"ref,omitempty"`
		Pending []string `json:"pending"`
	}{e.RunID, e.Phase, e.Reason, e.Ref, pending}
	return marshalEntry(TypeRun, &e.EntryBase, aux)
}

// UnmarshalJSON decodes the entry.
func (e *RunEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *RunEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain RunEntry
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), runKeys); err != nil {
		return err
	}
	return validateLifecycle(e)
}

// MarshalJSON emits the entry as one JSON object.
func (e *DispatchEntry) MarshalJSON() ([]byte, error) {
	if err := validateLifecycle(e); err != nil {
		return nil, err
	}
	type plain DispatchEntry
	return marshalEntry(TypeDispatch, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *DispatchEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *DispatchEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain DispatchEntry
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), dispatchKeys); err != nil {
		return err
	}
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
	return e.decodeMembers(data, all)
}

func (e *DecisionEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain DecisionEntry
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), decisionKeys); err != nil {
		return err
	}
	return validateLifecycle(e)
}

var (
	_ Entry = (*RunEntry)(nil)
	_ Entry = (*DispatchEntry)(nil)
	_ Entry = (*DecisionEntry)(nil)
)
