package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// ExtraVisible carries, under ExtraAgentSession in a step's extra, the
// hash of the step's visible fields, and in the document's top-level
// extra the hash of the document's (RFC 0001, Projections/ATIF, since
// 0.18): SHA-256 over the RFC 8785 canonical JSON, in the format's
// notation, sha256: and lowercase hexadecimal. A converter rebuilding an
// entry to the document's id holds the step it maps to, and the
// document, to the hashes, so a change made to what a reader sees and
// not to the raw items behind it is found ([StepVisible],
// [DocumentVisible]). The hashes are of the document as the export
// built it, before [Options.Redactors] run and before [WriteDocument]
// moves inline media to files.
const ExtraVisible = "visible"

// ExtraLeaf carries, under ExtraAgentSession in a document's top-level
// extra, the id of the entry the document was built at: the root's
// trajectory_id, and the leaf an embedded trajectory's trajectory_id,
// session/leaf, names with its session. A converter checks that the
// path it rebuilds reaches it (RFC 0001, Projections/ATIF, since 0.18).
const ExtraLeaf = "leaf"

// StepVisible returns the visible fields of a step, the step as JSON:
// the step as written with extra removed from it, from each element of
// tool_calls, from metrics, and from each observation result and each
// element of its subagent_trajectory_ref. Everything else is visible:
// step_id, timestamp, source, model_name, reasoning_effort, message,
// reasoning_content, tool_calls, observation, metrics,
// is_copied_context, llm_call_count, and any member ATIF does not
// define.
func StepVisible(step map[string]any) map[string]any {
	out := clone(step).(map[string]any)
	delete(out, "extra")
	for _, tc := range list(out["tool_calls"]) {
		dropExtra(tc)
	}
	dropExtra(out["metrics"])
	if o, ok := out["observation"].(map[string]any); ok {
		for _, r := range list(o["results"]) {
			dropExtra(r)
			if rm, ok := r.(map[string]any); ok {
				for _, ref := range list(rm["subagent_trajectory_ref"]) {
					dropExtra(ref)
				}
			}
		}
	}
	return out
}

// DocumentVisible returns the visible fields of a document, the document
// as JSON: every member but subagent_trajectories and extra, with extra
// removed from agent and from final_metrics, and steps replaced by their
// number, so a step removed or added changes the hash. Among them are
// the agent's name, version, model_name and tool_definitions, notes and
// the final metrics. Each step is held to its own hash, and its step_id
// to its position ([StepsInOrder]); an embedded trajectory to its own.
func DocumentVisible(doc map[string]any) map[string]any {
	out := clone(doc).(map[string]any)
	out["steps"] = len(list(doc["steps"]))
	delete(out, "subagent_trajectories")
	delete(out, "extra")
	dropExtra(out["agent"])
	dropExtra(out["final_metrics"])
	return out
}

// StepsInOrder reports whether the steps of a document, as JSON, are
// numbered by their position, step_id 1 for the first: a step duplicated
// or two swapped after the export, each still hashing as written, are
// not, and the document then matches nothing.
func StepsInOrder(doc map[string]any) bool {
	for i, sv := range list(doc["steps"]) {
		step, _ := sv.(map[string]any)
		n, ok := step["step_id"].(json.Number)
		if !ok || n.String() != strconv.Itoa(i+1) {
			return false
		}
	}
	return true
}

// VisibleHash is the hash ExtraVisible holds for visible fields v, a
// value as JSON: SHA-256 over its RFC 8785 canonical form, as sha256:
// and lowercase hexadecimal.
func VisibleHash(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	c, err := jcs.Transform(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(c)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// seal writes the hash of the visible fields of each step of doc, and of
// doc, under ExtraVisible, and does the same for each embedded
// trajectory: once the document is built, before any redactor runs, so
// the hashes are of what the session holds.
func seal(doc *atif.Trajectory) error {
	for i := range doc.Steps {
		step := &doc.Steps[i]
		v, err := asJSON(step)
		if err != nil {
			return err
		}
		h, err := VisibleHash(StepVisible(v))
		if err != nil {
			return err
		}
		if step.Extra == nil {
			step.Extra = map[string]any{}
		}
		agentSessionExtra(step.Extra)[ExtraVisible] = h
	}
	v, err := asJSON(doc)
	if err != nil {
		return err
	}
	h, err := VisibleHash(DocumentVisible(v))
	if err != nil {
		return err
	}
	if doc.Extra == nil {
		doc.Extra = map[string]any{}
	}
	agentSessionExtra(doc.Extra)[ExtraVisible] = h
	for _, sub := range doc.SubagentTrajectories {
		if err := seal(sub); err != nil {
			return err
		}
	}
	return nil
}

// asJSON is v as a JSON object, numbers kept as written.
func asJSON(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var out map[string]any
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("export: %T as JSON: %w", v, err)
	}
	return out, nil
}

// clone copies a JSON value deeply, so removing a member leaves the
// value it was taken from as it was.
func clone(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = clone(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = clone(e)
		}
		return out
	}
	return v
}

// list is v as a JSON array, nil when it is not one.
func list(v any) []any {
	l, _ := v.([]any)
	return l
}

// dropExtra removes extra from v when v is a JSON object.
func dropExtra(v any) {
	if m, ok := v.(map[string]any); ok {
		delete(m, "extra")
	}
}
