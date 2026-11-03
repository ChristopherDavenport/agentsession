package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"

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
// the step as written, its extra included at every depth, but for
// extra.agentsession.visible, the hash itself. A step's extra holds
// what a reader sees beside the ATIF fields, copies of entry text among
// them, a queued input's item and a decision's args, so all of it is
// held to the hash.
func StepVisible(step map[string]any) map[string]any {
	out := clone(step).(map[string]any)
	if extra, ok := out["extra"].(map[string]any); ok {
		if as, ok := extra[ExtraAgentSession].(map[string]any); ok {
			delete(as, ExtraVisible)
		}
	}
	return out
}

// DocumentVisible returns the visible fields of a document, the document
// as JSON: every member but subagent_trajectories, its extra included at
// every depth but for extra.agentsession.visible, the hash itself, and
// steps replaced by their number, so a step removed or added changes the
// hash. A converter restores the header's redacted_from and each entry's
// history members from the document's extra, so all of it is held to
// the hash. Each step is held to its own hash, and its step_id to its
// position ([StepsInOrder]); an embedded trajectory to its own.
func DocumentVisible(doc map[string]any) map[string]any {
	out := clone(doc).(map[string]any)
	out["steps"] = len(list(doc["steps"]))
	delete(out, "subagent_trajectories")
	if extra, ok := out["extra"].(map[string]any); ok {
		if as, ok := extra[ExtraAgentSession].(map[string]any); ok {
			delete(as, ExtraVisible)
		}
	}
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

// InlineMedia puts back inline, in a document read as JSON, the media
// [WriteDocument] moved to files beside it in dir, so a converter checks
// the hashes against the document as the export built it (RFC 0001,
// Projections/ATIF, since 0.18).
//
// A media part's path that names a file images/ or audio/ holds under
// dir, its name the SHA-256 of its bytes, is the data URL
// data:<media_type>;base64,<bytes in standard base64>, which is the one
// form WriteDocument moves to a file; every string of the document equal
// to such a path, the raw items' in the extras included, is replaced by
// it. A path naming no such file is left as it is, and the hashes then
// fail where it is held.
func InlineMedia(doc map[string]any, dir string) error {
	inline := map[string]string{}
	var collect func(doc map[string]any) error
	parts := func(content any) error {
		for _, pv := range list(content) {
			part, _ := pv.(map[string]any)
			src, _ := part["source"].(map[string]any)
			path, _ := src["path"].(string)
			mediaType, _ := src["media_type"].(string)
			if _, done := inline[path]; done || !spilledPath(path) {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return fmt.Errorf("export: inline %s: %w", path, err)
			}
			sum := sha256.Sum256(data)
			if name := pathpkg.Base(path); strings.TrimSuffix(name, pathpkg.Ext(name)) != hex.EncodeToString(sum[:]) {
				continue
			}
			inline[path] = "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
		return nil
	}
	collect = func(doc map[string]any) error {
		for _, sv := range list(doc["steps"]) {
			step, _ := sv.(map[string]any)
			if err := parts(step["message"]); err != nil {
				return err
			}
			obs, _ := step["observation"].(map[string]any)
			for _, rv := range list(obs["results"]) {
				res, _ := rv.(map[string]any)
				if err := parts(res["content"]); err != nil {
					return err
				}
			}
		}
		for _, sub := range list(doc["subagent_trajectories"]) {
			if m, ok := sub.(map[string]any); ok {
				if err := collect(m); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(doc); err != nil {
		return err
	}
	if len(inline) > 0 {
		replaceStrings(doc, inline)
	}
	return nil
}

// spilledPath reports whether path is one WriteDocument gives a media
// file: images/ or audio/ and a file name.
func spilledPath(path string) bool {
	dir, name, ok := strings.Cut(path, "/")
	return ok && (dir == "images" || dir == "audio") && name != "" && !strings.Contains(name, "/")
}

// replaceStrings replaces, in place, every string of a JSON value that
// is a key of by with its value.
func replaceStrings(v any, by map[string]string) {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			if s, ok := e.(string); ok {
				if r, ok := by[s]; ok {
					x[k] = r
				}
				continue
			}
			replaceStrings(e, by)
		}
	case []any:
		for i, e := range x {
			if s, ok := e.(string); ok {
				if r, ok := by[s]; ok {
					x[i] = r
				}
				continue
			}
			replaceStrings(e, by)
		}
	}
}
