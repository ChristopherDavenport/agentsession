package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

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
	// InstructionsParts are the parts the instructions are composed
	// of, in order, when the path named them, each carrying its text.
	// Instructions is always their texts joined with [PartSeparator],
	// so a reader that does not care about the composition reads the
	// string as before. It is empty for a path that set the
	// instructions as one string.
	InstructionsParts []InstructionPart   `json:"instructions_parts,omitempty"`
	Tools             openresponses.Tools `json:"tools,omitempty"`
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
		out.InstructionsParts = cloneParts(s.InstructionsParts)
	}
	if c.Model != "" {
		out.Model = c.Model
	}
	switch {
	case len(c.InstructionsParts) > 0:
		// The delta carries the whole ordered list, so it decides both
		// the order and which parts are in force; a part it leaves out
		// is removed. A replace discards the parts in force with the
		// rest, so nothing a hash names survives it.
		prev := s.InstructionsParts
		if c.Replace {
			prev = nil
		}
		out.InstructionsParts = applyInstructionParts(prev, c.InstructionsParts)
		out.Instructions = JoinInstructions(out.InstructionsParts)
		if c.Instructions != nil && unresolvedParts(out.InstructionsParts) {
			// A part the path cannot resolve has no text to join, so
			// the string the writer put beside the parts is the only
			// record of what the model was sent.
			out.Instructions = *c.Instructions
		}
	case c.Instructions != nil:
		// One string replaces the composition: the parts no longer
		// describe what is in force.
		out.Instructions = *c.Instructions
		out.InstructionsParts = nil
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

// PartSeparator joins the instruction parts into the instructions
// string. The rule is the format's, so a writer that records parts
// and a reader that rebuilds the string produce the same bytes and
// the request hash verifies.
const PartSeparator = "\n\n"

// JoinInstructions returns the instructions the parts compose: their
// texts joined with [PartSeparator], in order.
func JoinInstructions(parts []InstructionPart) string {
	texts := make([]string, 0, len(parts))
	for _, p := range parts {
		texts = append(texts, p.Text)
	}
	return strings.Join(texts, PartSeparator)
}

// applyInstructionParts resolves a delta's ordered list against the
// parts in force: a part carrying text sets it, a part carrying a
// hash alone keeps the text the path has, and a part the list leaves
// out is gone. A hash whose part is not on the path is kept as it
// was written, so a reader can see that the text is missing rather
// than read an empty part as empty text.
func applyInstructionParts(prev, delta []InstructionPart) []InstructionPart {
	byID := make(map[string]InstructionPart, len(prev))
	for _, p := range prev {
		byID[p.ID] = p
	}
	out := make([]InstructionPart, 0, len(delta))
	for _, p := range delta {
		next := InstructionPart{ID: p.ID, Text: p.Text, Source: p.Source}
		if p.Text == "" && p.Hash != "" {
			old, ok := byID[p.ID]
			switch {
			case ok:
				next.Text = old.Text
				if next.Source == "" {
					next.Source = old.Source
				}
			default:
				next.Hash = p.Hash
			}
		}
		out = append(out, next)
	}
	return out
}

// unresolvedParts reports whether any part still carries the hash it
// was named by, which is what [applyInstructionParts] leaves behind
// for a part whose text is not on the path.
func unresolvedParts(parts []InstructionPart) bool {
	for _, p := range parts {
		if p.Hash != "" {
			return true
		}
	}
	return false
}

// InstructionsDelta returns the config delta that takes the
// instructions from these settings to parts: the whole ordered list
// of IDs, with the text of every part that is new or whose text
// changed and the hash alone of every part that is unchanged, so a
// change to one layer costs that layer and not the whole prompt. A
// part in force that parts leaves out is removed by its absence.
//
// It returns nil when parts are exactly the ones in force, so a
// harness that re-renders its layers every turn writes nothing when
// nothing moved.
func (s Settings) InstructionsDelta(parts []InstructionPart) *ConfigEntry {
	if len(parts) == 0 {
		if len(s.InstructionsParts) == 0 && s.Instructions == "" {
			return nil
		}
		empty := ""
		return &ConfigEntry{Instructions: &empty}
	}
	byID := make(map[string]InstructionPart, len(s.InstructionsParts))
	for _, p := range s.InstructionsParts {
		byID[p.ID] = p
	}
	same := len(parts) == len(s.InstructionsParts)
	out := make([]InstructionPart, 0, len(parts))
	for i, p := range parts {
		old, ok := byID[p.ID]
		// A part named by its hash inherits the source it had, so a
		// part whose source moved, cleared above all, carries its text
		// even when the text did not change: the hash form cannot say
		// "this part has no source now".
		if !ok || old.Text != p.Text || old.Source != p.Source {
			out = append(out, InstructionPart{ID: p.ID, Text: p.Text, Source: p.Source})
			same = false
			continue
		}
		if same && s.InstructionsParts[i].ID != p.ID {
			same = false
		}
		out = append(out, InstructionPart{ID: p.ID, Source: p.Source, Hash: HashText(p.Text)})
	}
	if same {
		return nil
	}
	return &ConfigEntry{InstructionsParts: out}
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

// cloneParts copies the parts so two Settings never share one array:
// a Settings taken from a compaction checkpoint would otherwise alias
// the entry's own slice, which is immutable once appended.
func cloneParts(parts []InstructionPart) []InstructionPart {
	if parts == nil {
		return nil
	}
	return append([]InstructionPart(nil), parts...)
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

// InstructionsOmitted returns the parts the last config entry of the
// context considered for the instructions and left out. It is not
// settings, so it does not replay and a compaction checkpoint does
// not carry it: it is the most recent record of what the model was
// not given, from the entries the context holds.
func (c Context) InstructionsOmitted() []OmittedPart {
	for i := len(c.Entries) - 1; i >= 0; i-- {
		if cfg, ok := c.Entries[i].(*ConfigEntry); ok && len(cfg.InstructionsOmitted) > 0 {
			return cfg.InstructionsOmitted
		}
	}
	return nil
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
		settings.InstructionsParts = cloneParts(comp.Config.InstructionsParts)
		ctx.Entries = append(ctx.Entries, comp)
		ctx.Items = append(ctx.Items, comp.Summary)
		ctx.ItemEntries = append(ctx.ItemEntries, comp)
		for _, item := range comp.Pinned {
			ctx.Items = append(ctx.Items, item)
			ctx.ItemEntries = append(ctx.ItemEntries, comp)
		}
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

// OutputEntries returns the item entries on path that hold the output
// of resp, in path order. It is the format's rule for finding a
// response's own output items, stated in RFC 0001 under "Request
// context of a response", and it is the one implementation: a reader
// that needs those items, and one that needs to exclude them, must
// agree or the same file rebuilds two different requests.
//
// Walking back from the end of path: an entry that is not an item
// entry is skipped, an item entry whose Response names resp is one of
// its output items, and the walk stops at the first item entry that
// names another response or none. A response with no ResponseID has
// no output items.
//
// path is resp's path with resp itself at the end, or that path
// without it; either gives the same answer, since an entry that is
// not an item entry is skipped. Only the matching item entries are
// returned, never the entries skipped between them: those are on the
// path for their own reasons and stay there.
//
// The returned entries are the session's own, not copies. A caller
// that serves their items to something that records must clone them;
// a caller that only reads the path need not.
func OutputEntries(path []Entry, resp *ResponseEntry) []*ItemEntry {
	if resp == nil || resp.ResponseID == "" {
		return nil
	}
	var output []*ItemEntry
	for i := len(path) - 1; i >= 0; i-- {
		item, ok := path[i].(*ItemEntry)
		if !ok {
			continue
		}
		if item.ResponseID == "" || item.ResponseID != resp.ResponseID {
			break
		}
		output = append(output, item)
	}
	// The walk runs backward; the result is in path order, which is
	// the order the model produced the items in.
	slices.Reverse(output)
	return output
}

// RequestContext rebuilds the context of the request that produced the
// response entry id: the path to the entry with the response's own
// output items removed, as [OutputEntries] finds them. Only those item
// entries are removed; everything else on the path stays, so an entry
// another layer wrote between two output items of one response, which
// contributes nothing to context, leaves the rebuilt request and its
// hash alone.
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
	path = path[:len(path)-1] // drop the response entry itself
	output := OutputEntries(path, resp)
	if len(output) == 0 {
		return BuildContext(path)
	}
	drop := make(map[*ItemEntry]struct{}, len(output))
	for _, item := range output {
		drop[item] = struct{}{}
	}
	request := make([]Entry, 0, len(path))
	for _, e := range path {
		if item, ok := e.(*ItemEntry); ok {
			if _, skip := drop[item]; skip {
				continue
			}
		}
		request = append(request, e)
	}
	return BuildContext(request)
}

// ErrHashMismatch is returned by [Session.Verify] when the rebuilt
// request does not hash to the recorded value.
var ErrHashMismatch = errors.New("agentsession: request hash mismatch")

// ErrNoHash is returned by [Session.Verify] for a response entry that
// recorded no request hash. The response is not verified and not
// mismatched: there was nothing to check. A writer records no hash
// when it cannot stand behind one, which is what a layer that edits
// the request outside the transcript leaves behind, so a caller that
// accepts unverified requests opts in with [errors.Is] rather than by
// reading err == nil.
var ErrNoHash = errors.New("agentsession: no request hash recorded")

// Verify rebuilds the request for the response entry id and checks that
// it hashes to the entry's RequestHash. A response that recorded no
// hash returns [ErrNoHash]: nil means the request was checked.
func (s *Session) Verify(id string) error {
	ctx, err := s.RequestContext(id)
	if err != nil {
		return err
	}
	e, _ := s.Entry(id)
	resp := e.(*ResponseEntry)
	if resp.RequestHash == "" {
		return fmt.Errorf("%w: response %s", ErrNoHash, id)
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
