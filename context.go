package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ChristopherDavenport/openresponses"
)

// Settings are the request settings in force at a point on a path: the
// result of replaying config entries. The JSON form is the full-config
// checkpoint a compaction entry carries.
type Settings struct {
	Model        string                        `json:"model,omitempty"`
	Instructions string                        `json:"instructions,omitempty"`
	Reasoning    openresponses.ReasoningConfig `json:"reasoning,omitzero"`
	Text         openresponses.TextConfig      `json:"text,omitzero"`
	Tools        openresponses.Tools           `json:"tools,omitempty"`
	// Extra carries request members beyond the named ones, keyed by
	// their wire name.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`
}

// ExtraValue decodes the passthrough member key into v. It reports
// false, and leaves v alone, when the settings carry no such member.
func (s Settings) ExtraValue(key string, v any) (bool, error) {
	raw, ok := s.Extra[key]
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return true, fmt.Errorf("agentsession: settings extra %q: %w", key, err)
	}
	return true, nil
}

// Apply returns the settings after the delta c. The receiver is not
// modified.
func (s Settings) Apply(c *ConfigEntry) Settings {
	out := s
	if c.Replace {
		out = Settings{}
	} else {
		out.Tools = append(openresponses.Tools(nil), s.Tools...)
		out.Extra = cloneRaw(s.Extra)
	}
	if c.Model != "" {
		out.Model = c.Model
	}
	if c.Instructions != nil {
		out.Instructions = *c.Instructions
	}
	if c.Reasoning != nil {
		out.Reasoning = *c.Reasoning
	}
	if c.Text != nil {
		out.Text = *c.Text
	}
	for _, name := range c.ToolsRemoved {
		out.Tools = removeTool(out.Tools, name)
	}
	for _, t := range c.ToolsAdded {
		if name := ToolName(t); name != "" {
			out.Tools = removeTool(out.Tools, name)
		}
		out.Tools = append(out.Tools, t)
	}
	if len(c.Extra) > 0 && out.Extra == nil {
		out.Extra = make(map[string]json.RawMessage)
	}
	for k, v := range c.Extra {
		if isNull(v) {
			delete(out.Extra, k)
			continue
		}
		out.Extra[k] = append(json.RawMessage(nil), v...)
	}
	if len(out.Extra) == 0 {
		out.Extra = nil
	}
	if len(out.Tools) == 0 {
		out.Tools = nil
	}
	return out
}

// Request builds the canonical request for these settings over items:
// store false and no previous_response_id, as the context algorithm
// requires. Extra members ride in Request.Extra and are flattened on
// encode.
func (s Settings) Request(items openresponses.Items) (openresponses.Request, error) {
	store := false
	req := openresponses.Request{
		Model:        s.Model,
		Instructions: s.Instructions,
		Reasoning:    s.Reasoning,
		Text:         s.Text,
		Tools:        s.Tools,
		Input:        items,
		Store:        &store,
	}
	if len(s.Extra) > 0 {
		req.Extra = make(map[string]any, len(s.Extra))
		for k, raw := range s.Extra {
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.UseNumber()
			var v any
			if err := dec.Decode(&v); err != nil {
				return openresponses.Request{}, fmt.Errorf("agentsession: settings extra %q: %w", k, err)
			}
			req.Extra[k] = v
		}
	}
	return req, nil
}

// ToolName returns the name of a tool: the function name for a function
// tool, the "name" member of an extension tool when it has one, else "".
func ToolName(t openresponses.Tool) string {
	switch v := t.(type) {
	case *openresponses.FunctionTool:
		return v.Name
	case *openresponses.UnknownTool:
		var probe struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(v.Raw, &probe)
		return probe.Name
	}
	return ""
}

func removeTool(tools openresponses.Tools, name string) openresponses.Tools {
	out := tools[:0:0]
	for _, t := range tools {
		if ToolName(t) != name {
			out = append(out, t)
		}
	}
	return out
}

func cloneRaw(m map[string]json.RawMessage) map[string]json.RawMessage {
	if m == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func isNull(raw json.RawMessage) bool {
	return len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null"
}

// Context is what a model call at a point on a path receives: the
// replayed settings, the item list ready to be the request input and
// the entries the list was built from, for renderers.
type Context struct {
	Settings Settings
	Items    openresponses.Items
	// Entries are the entries selected by the context algorithm, in
	// order: after a compaction, the compaction itself, then the kept
	// entries from FirstKept up to it, then everything after. Entries
	// that contribute no item are included so a renderer can show them.
	Entries []Entry
	// ItemEntries is aligned with Items: ItemEntries[i] is the entry
	// that contributed Items[i], so an index into the request input
	// maps back to the entry that produced it. After a compaction
	// ItemEntries[0] is the compaction entry, whose summary is Items[0].
	ItemEntries []Entry
}

// Request returns the canonical request for the context.
func (c Context) Request() (openresponses.Request, error) {
	return c.Settings.Request(c.Items)
}

// BuildContext runs the context algorithm over a root-first path. Only
// the last compaction on the path is applied. FirstKept must name an
// entry on the path before the compaction.
func BuildContext(path []Entry) (Context, error) {
	var ctx Context
	var settings Settings
	start := 0
	compIdx := -1
	for i, e := range path {
		if _, ok := e.(*CompactionEntry); ok {
			compIdx = i
		}
	}
	if compIdx >= 0 {
		comp := path[compIdx].(*CompactionEntry)
		kept := -1
		for i := 0; i < compIdx; i++ {
			if path[i].Base().ID == comp.FirstKept {
				kept = i
				break
			}
		}
		if kept < 0 {
			return Context{}, fmt.Errorf("agentsession: compaction %s: first_kept %q is not on the path before it", comp.ID, comp.FirstKept)
		}
		settings = comp.Config
		ctx.Entries = append(ctx.Entries, comp)
		ctx.Items = append(ctx.Items, comp.Summary)
		ctx.ItemEntries = append(ctx.ItemEntries, comp)
		for _, e := range path[kept:compIdx] {
			ctx.Entries = append(ctx.Entries, e)
			if item := contextItem(e); item != nil {
				ctx.Items = append(ctx.Items, item)
				ctx.ItemEntries = append(ctx.ItemEntries, e)
			}
		}
		start = compIdx + 1
	}
	// The checkpoint stands in for every config entry up to the
	// compaction, kept window included; only entries after it replay.
	for _, e := range path[start:] {
		if c, ok := e.(*ConfigEntry); ok {
			settings = settings.Apply(c)
		}
		ctx.Entries = append(ctx.Entries, e)
		if item := contextItem(e); item != nil {
			ctx.Items = append(ctx.Items, item)
			ctx.ItemEntries = append(ctx.ItemEntries, e)
		}
	}
	ctx.Settings = settings
	return ctx, nil
}

// contextItem returns the item an entry contributes to the context, or
// nil. A compaction is handled by BuildContext, not here, because only
// the last one on the path applies.
func contextItem(e Entry) openresponses.Item {
	switch v := e.(type) {
	case *ItemEntry:
		return v.Item
	case *BranchSummaryEntry:
		return v.Summary
	}
	return nil
}

// Context builds the context at the current leaf. With no leaf it is
// empty.
func (s *Session) Context() (Context, error) {
	return s.ContextAt(s.Leaf())
}

// ContextAt builds the context at entry id, the request a model call
// appended after that entry would receive. An empty id yields an empty
// context.
func (s *Session) ContextAt(id string) (Context, error) {
	if id == "" {
		return Context{}, nil
	}
	path := s.Path(id)
	if path == nil {
		return Context{}, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, id)
	}
	return BuildContext(path)
}

// RequestContext rebuilds the context of the request that produced the
// response entry id: the path to the entry with the response's own
// output items removed. Output items are the item entries directly
// before the response entry whose ResponseID matches.
func (s *Session) RequestContext(id string) (Context, error) {
	e, ok := s.Entry(id)
	if !ok {
		return Context{}, fmt.Errorf("agentsession: %w: %s", ErrNoEntry, id)
	}
	resp, ok := e.(*ResponseEntry)
	if !ok {
		return Context{}, fmt.Errorf("agentsession: entry %s is a %s, not a response", id, e.EntryType())
	}
	path := s.Path(id)
	end := len(path) - 1 // drop the response entry itself
	for end > 0 {
		item, ok := path[end-1].(*ItemEntry)
		if !ok || item.ResponseID == "" || item.ResponseID != resp.ResponseID {
			break
		}
		end--
	}
	return BuildContext(path[:end])
}

// ErrHashMismatch is returned by [Session.Verify] when the rebuilt
// request does not hash to the recorded value.
var ErrHashMismatch = errors.New("agentsession: request hash mismatch")

// Verify rebuilds the request for the response entry id and checks that
// it hashes to the entry's RequestHash. A response without a recorded
// hash verifies trivially.
func (s *Session) Verify(id string) error {
	ctx, err := s.RequestContext(id)
	if err != nil {
		return err
	}
	e, _ := s.Entry(id)
	resp := e.(*ResponseEntry)
	if resp.RequestHash == "" {
		return nil
	}
	req, err := ctx.Request()
	if err != nil {
		return err
	}
	got, err := RequestHash(req)
	if err != nil {
		return err
	}
	if got != resp.RequestHash {
		return fmt.Errorf("%w: response %s recorded %s, rebuilt %s", ErrHashMismatch, id, resp.RequestHash, got)
	}
	return nil
}
