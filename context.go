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
	InstructionsParts []InstructionPart `json:"instructions_parts,omitempty"`
	// InstructionsOmitted are the parts left out of the instructions
	// that are in force: the list the last config entry carrying one
	// wrote, each keep in it resolved against the list before it,
	// cleared by an empty list or a replace without one. A keep the
	// path could not satisfy stays in the list as written: see
	// [OmittedPart.Unresolved]. It
	// reaches no request, so [Settings.Request] leaves it out and the
	// request hash does not cover it; a compaction checkpoint carries
	// it so the list survives the fold. A checkpoint member that does
	// not decode as a list of parts, which a file from before 0.8 may
	// hold, is kept as written and InstructionsOmitted is nil.
	InstructionsOmitted []OmittedPart       `json:"instructions_omitted,omitempty"`
	Tools               openresponses.Tools `json:"tools,omitempty"`
	// Extra carries request members beyond the named ones, keyed by
	// their wire name.
	Extra map[string]json.RawMessage `json:"extra,omitempty"`

	// left holds the parts that have left force since the replay
	// began, a compaction's checkpoint or a replace, which a later
	// delta may still name by hash; see applyInstructionParts.
	left *partHistory
}

// partHistory is the parts that left force, one node per config entry
// that moved any, newest first. A node is never changed once built, so
// settings copied by value share it.
type partHistory struct {
	byID map[string][]InstructionPart
	prev *partHistory
}

// leave returns the history after a delta took the parts in force from
// prev to now: each part of prev, with its text, that now does not hold
// as it was. A part left unresolved has no text to name.
func (h *partHistory) leave(prev, now []InstructionPart) *partHistory {
	if len(prev) == 0 {
		return h
	}
	kept := make(map[InstructionPart]bool, len(now))
	for _, p := range now {
		kept[p] = true
	}
	var byID map[string][]InstructionPart
	for _, p := range prev {
		if p.ID == "" || p.Unresolved() || kept[p] {
			continue
		}
		if byID == nil {
			byID = make(map[string][]InstructionPart)
		}
		byID[p.ID] = append(byID[p.ID], p)
	}
	if byID == nil {
		return h
	}
	return &partHistory{byID: byID, prev: h}
}

// find returns the part that most recently left force under id with
// the text hash names.
func (h *partHistory) find(id, hash string) (InstructionPart, bool) {
	for ; h != nil; h = h.prev {
		for _, p := range h.byID[id] {
			if HashText(p.Text) == hash {
				return p, true
			}
		}
	}
	return InstructionPart{}, false
}

// UnmarshalJSON decodes a checkpoint, taking instructions_omitted only
// when the member is spelled exactly and holds a list of parts that
// encodes back to what the line holds, extra members aside, as a
// member promoted into a typed field is taken. Anything else, such as
// a part whose id is spelled "ID", is left for the rewrite to keep as
// written.
func (s *Settings) UnmarshalJSON(data []byte) error {
	type plain Settings
	var aux struct {
		plain
		// Shadows the typed field, which is decoded below from the
		// exactly spelled member alone.
		Omitted json.RawMessage `json:"instructions_omitted"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*s = Settings(aux.plain)
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	var omitted []OmittedPart
	if raw, ok := all["instructions_omitted"]; ok && holdsExactly(raw, &omitted) && len(omitted) > 0 {
		s.InstructionsOmitted = omitted
	}
	return nil
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
		out.InstructionsOmitted = cloneOmitted(s.InstructionsOmitted)
	}
	if c.InstructionsOmitted != nil {
		// The entry carries the member, so it replaces the list in
		// force; an empty one clears it. Its keeps count over the list
		// before it, which a replace discards.
		prev := s.InstructionsOmitted
		if c.Replace {
			prev = nil
		}
		out.InstructionsOmitted = cloneOmitted(applyOmitted(prev, c.InstructionsOmitted))
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
		out.InstructionsParts = applyInstructionParts(prev, c.InstructionsParts, out.left)
		out.left = out.left.leave(prev, out.InstructionsParts)
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
		out.left = out.left.leave(out.InstructionsParts, nil)
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
// hash alone takes the text the path has given its id with that hash,
// in force or, from left, one that has since left force, a keep takes
// the next parts in force as they are, and a part the list leaves out
// is gone. A hash no text of its id on the path has is kept as it was
// written, and so is a keep that runs past the parts in force or takes
// one the list names elsewhere, so a reader can see that the text is
// missing rather than read an empty part as empty text.
//
// A keep counts from a cursor into the parts in force: an element
// naming a part in force moves it to just after that part, a keep
// moves it past the parts it takes, and a new id leaves it alone. A
// part named with neither text nor hash has empty text, which is how
// a writer spells one. An element with neither an id nor a keep, or a
// hash or keep over a part the path itself could not rebuild, leaves
// the part unresolved.
func applyInstructionParts(prev, delta []InstructionPart, left *partHistory) []InstructionPart {
	byID := make(map[string]InstructionPart, len(prev))
	at := make(map[string]int, len(prev))
	for i, p := range prev {
		byID[p.ID] = p
		at[p.ID] = i
	}
	named := make(map[string]bool, len(delta))
	for _, p := range delta {
		if p.ID != "" {
			named[p.ID] = true
		}
	}
	out := make([]InstructionPart, 0, len(delta))
	cursor := 0
	for _, p := range delta {
		if p.ID == "" && p.Keep > 0 {
			run := prev[min(cursor, len(prev)):min(cursor+p.Keep, len(prev))]
			ok := len(run) == p.Keep
			for _, q := range run {
				ok = ok && !named[q.ID]
			}
			cursor += p.Keep
			if !ok {
				out = append(out, InstructionPart{Keep: p.Keep})
				continue
			}
			// A part kept as it is stays unresolved if it was.
			out = append(out, run...)
			continue
		}
		if p.ID == "" {
			// Neither a part nor a keep: nothing to rebuild.
			out = append(out, InstructionPart{Keep: p.Keep, unresolved: true})
			continue
		}
		if i, ok := at[p.ID]; ok {
			cursor = i + 1
		}
		next := InstructionPart{ID: p.ID, Text: p.Text, Source: p.Source}
		if p.Text == "" && p.Hash != "" {
			old, ok := byID[p.ID]
			if !ok || !old.Unresolved() && HashText(old.Text) != p.Hash {
				// Not the text in force: a text the id had before, or
				// none.
				old, ok = left.find(p.ID, p.Hash)
			}
			switch {
			case ok && !old.Unresolved():
				next.Text = old.Text
				if next.Source == "" {
					next.Source = old.Source
				}
			default:
				// No text of the id's on the path has the hash, or the
				// part in force has none: the hash resolves against
				// nothing.
				next.Hash = p.Hash
			}
		}
		out = append(out, next)
	}
	return out
}

// applyOmitted resolves a delta's omitted list against the list in
// force by the cursor applyInstructionParts counts keeps from: an
// element naming a part in force moves it to just after that part, a
// keep takes the next parts in force as they are and moves it past
// them, and a new id leaves it alone. A keep that runs past the list
// in force or takes a part the delta names elsewhere is kept as it was
// written, and so is an element with neither an id nor a keep, so a
// reader can see that a part is missing. An element that names a part
// carries all of it.
func applyOmitted(prev, delta []OmittedPart) []OmittedPart {
	at := make(map[string]int, len(prev))
	for i, p := range prev {
		if _, dup := at[p.ID]; p.ID != "" && !dup {
			at[p.ID] = i
		}
	}
	named := make(map[string]bool, len(delta))
	for _, p := range delta {
		if p.ID != "" {
			named[p.ID] = true
		}
	}
	out := make([]OmittedPart, 0, len(delta))
	cursor := 0
	for _, p := range delta {
		switch {
		case p.ID == "" && p.Keep > 0:
			run := prev[min(cursor, len(prev)):min(cursor+p.Keep, len(prev))]
			ok := len(run) == p.Keep
			for _, q := range run {
				ok = ok && !named[q.ID]
			}
			cursor += p.Keep
			if !ok {
				out = append(out, OmittedPart{Keep: p.Keep})
				continue
			}
			// A part kept as it is stays unresolved if it was.
			out = append(out, run...)
		case p.ID == "":
			// Neither a part nor a keep: kept as written.
			out = append(out, p)
		default:
			if i, ok := at[p.ID]; ok {
				cursor = i + 1
			}
			p.Keep = 0 // a keep beside an id means nothing
			out = append(out, p)
		}
	}
	return out
}

// OmittedDelta returns the instructions_omitted member that takes the
// omitted parts in force in these settings to omitted, for
// [ConfigEntry.InstructionsOmitted]: nil when omitted is the list in
// force, so a writer that renders its omissions every turn writes
// nothing when nothing moved; an empty, non-nil list, written as [],
// when omitted is empty and a list is in force; and otherwise omitted
// with every run of parts in force, unchanged and in the order they
// are in force, named by a keep, so a part moving across a budget
// costs that part and not the whole list. A part that changed, is new
// or is out of that order is written whole. omitted names each ID
// once; when it does not, or the list in force names an ID twice,
// omitted is returned whole.
//
// A delta with Replace set discards the list in force, so its keeps
// would resolve against nothing: such a delta carries omitted itself.
func (s Settings) OmittedDelta(omitted []OmittedPart) []OmittedPart {
	prev := s.InstructionsOmitted
	if slices.Equal(prev, omitted) {
		return nil
	}
	if len(omitted) == 0 {
		return []OmittedPart{}
	}
	at := make(map[string]int, len(prev))
	whole := false
	for i, p := range prev {
		if p.ID == "" {
			continue // an element naming nothing is never kept by a delta
		}
		if _, dup := at[p.ID]; dup {
			whole = true
		}
		at[p.ID] = i
	}
	seen := make(map[string]bool, len(omitted))
	for _, p := range omitted {
		if p.ID == "" || p.Keep != 0 || seen[p.ID] {
			whole = true
		}
		seen[p.ID] = true
	}
	if whole {
		return append([]OmittedPart{}, omitted...)
	}
	out := make([]OmittedPart, 0, len(omitted))
	cursor, run := 0, 0
	flush := func() {
		if run > 0 {
			out = append(out, OmittedPart{Keep: run})
			cursor += run
			run = 0
		}
	}
	for _, p := range omitted {
		j, ok := at[p.ID]
		if ok && prev[j] == p && j == cursor+run {
			run++
			continue
		}
		flush()
		out = append(out, p)
		if ok {
			cursor = j + 1
		}
	}
	flush()
	return out
}

// unresolvedParts reports whether any part is one the path could not
// rebuild: see [InstructionPart.Unresolved].
func unresolvedParts(parts []InstructionPart) bool {
	for _, p := range parts {
		if p.Unresolved() {
			return true
		}
	}
	return false
}

// InstructionsDelta returns the config delta that takes the
// instructions from these settings to parts: the whole ordered list,
// with the text of every part that is new or whose text or source
// changed, a keep for every run of unchanged parts in the order they
// are in force, and the hash alone of an unchanged part out of that
// order, so a change to one layer costs that layer and not the whole
// prompt, however many parts it has. A part in force that parts leaves
// out is removed by its absence. parts names each ID once.
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
	at := make(map[string]int, len(s.InstructionsParts))
	for i, p := range s.InstructionsParts {
		at[p.ID] = i
	}
	same := len(parts) == len(s.InstructionsParts)
	out := make([]InstructionPart, 0, len(parts))
	cursor, run := 0, 0
	flush := func() {
		if run > 0 {
			out = append(out, InstructionPart{Keep: run})
			cursor += run
			run = 0
		}
	}
	for i, p := range parts {
		j, ok := at[p.ID]
		// A part named by its hash or kept inherits the source it had,
		// so a part whose source moved, cleared above all, carries its
		// text even when the text did not change: neither form can say
		// "this part has no source now".
		// A part in force the path could not rebuild is written out, so
		// the delta resolves it rather than keeping what is missing.
		if !ok || s.InstructionsParts[j].Unresolved() || s.InstructionsParts[j].Text != p.Text || s.InstructionsParts[j].Source != p.Source {
			flush()
			out = append(out, InstructionPart{ID: p.ID, Text: p.Text, Source: p.Source})
			if ok {
				cursor = j + 1
			}
			same = false
			continue
		}
		if j != i {
			same = false
		}
		if j == cursor+run {
			run++
			continue
		}
		flush()
		out = append(out, InstructionPart{ID: p.ID, Hash: HashText(p.Text)})
		cursor = j + 1
	}
	if same {
		return nil
	}
	flush()
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

// cloneOmitted copies the omitted parts, nil for none.
func cloneOmitted(parts []OmittedPart) []OmittedPart {
	if len(parts) == 0 {
		return nil
	}
	return append([]OmittedPart(nil), parts...)
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
	// maps back to the entry that produced it. After a compaction the
	// compaction entry fills one slot for its summary and one for each
	// of its pinned items, so it repeats as many times as it
	// contributed: Items[0] is its summary and the next len(Pinned)
	// items are its pins.
	ItemEntries []Entry
}

// Request returns the canonical request for the context. It equals the
// request the model was sent under canonical JSON (RFC 8785), not in
// bytes: member order inside opaque JSON, such as a tool's parameter
// schema, is what the store gives back. A jsonl file gives members as
// they were written and a cas store gives them in canonical order, so
// the same session rebuilds different bytes from each, with one request
// hash. A renderer that turns the request into tokens passes it, and the
// live request it compares with, through [CanonicalRequest] first.
func (c Context) Request() (openresponses.Request, error) {
	return c.Settings.Request(c.Items)
}

// InstructionsOmitted returns the parts left out of the instructions
// in force: [Settings.InstructionsOmitted]. The list stays in force
// until a later config entry carries the member, so a writer writes it
// only when it changed, and a compaction checkpoint carries it.
func (c Context) InstructionsOmitted() []OmittedPart {
	return c.Settings.InstructionsOmitted
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
		settings.InstructionsOmitted = cloneOmitted(comp.Config.InstructionsOmitted)
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
// not an item entry is skipped. It MUST NOT run past resp: a path
// that continues beyond the response ends in the next call's items,
// the walk stops on the first of them, and the result is an empty
// slice rather than an error. Only the matching item entries are
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

// ContextHash computes the context hash at entry id, as RFC 0002
// defines it: the key a store can compute as it appends and a router can
// key a provider's cached prefix on. It is defined over the path ending
// at the entry by the entry's type and not by any later leaf. An item,
// config, compaction or branch_summary contributes; a response, a record
// entry and an extension entry do not. The value before any
// contributing entry is the hash of the canonical null; a
// non-contributing entry takes its parent's; a contributing entry hashes
// the three-element array of its parent's context hash, its type and
// its content hash as the section defines, with the per-run provenance
// members removed from the body first and a compaction's first_kept
// replaced by the context hash and type of the entry it names.
//
// It excludes ts and parents, it is incremental, and two sessions whose
// contributing entries are byte-identical share it. It is not
// request_hash: on a path with no compaction the two identify the same
// request, and after a fold they part.
func (s *Session) ContextHash(id string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	path := s.path(id)
	if path == nil {
		return "", fmt.Errorf("%w: %s", ErrNoEntry, id)
	}
	ctx, err := HashRequestJSON([]byte("null"))
	if err != nil {
		return "", err
	}
	at := map[string]string{} // context hash at each entry on the path
	types := map[string]string{}
	for _, e := range path {
		b := e.Base()
		types[b.ID] = e.EntryType()
		if !contributes(e) {
			at[b.ID] = ctx
			continue
		}
		content, err := contextContentHash(e, at, types)
		if err != nil {
			return "", err
		}
		step, err := json.Marshal([]string{ctx, e.EntryType(), content})
		if err != nil {
			return "", err
		}
		ctx, err = HashRequestJSON(step)
		if err != nil {
			return "", err
		}
		at[b.ID] = ctx
	}
	return ctx, nil
}

// contributes reports whether an entry enters the context hash: the
// four types that carry context, by type and not by whether a later
// leaf's compaction would select them.
func contributes(e Entry) bool {
	switch e.EntryType() {
	case TypeItem, TypeConfig, TypeCompaction, TypeBranchSummary:
		return true
	}
	return false
}

// contextContentHash is the content hash the context hash uses for one
// contributing entry: the body with legacy_id and normalised removed
// for every type, response, source and queued_from removed from an
// item, from removed from a branch_summary, and a compaction's
// first_kept replaced by [context hash, type] of the entry it names
// when that entry is on the path and by null otherwise.
func contextContentHash(e Entry, at, types map[string]string) (string, error) {
	data, err := MarshalEntry(e)
	if err != nil {
		return "", err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return "", err
	}
	body := make(map[string]json.RawMessage, len(all))
	for k, v := range all {
		if isEnvelopeKey(k) {
			continue
		}
		body[k] = v
	}
	delete(body, "legacy_id")
	delete(body, "normalised")
	switch e.EntryType() {
	case TypeItem:
		delete(body, "response")
		delete(body, "source")
		delete(body, "queued_from")
	case TypeBranchSummary:
		delete(body, "from")
	case TypeCompaction:
		var named string
		if raw, ok := body["first_kept"]; ok {
			_ = json.Unmarshal(raw, &named)
		}
		if ctx, ok := at[named]; ok {
			sub, _ := json.Marshal([]string{ctx, types[named]})
			body["first_kept"] = sub
		} else {
			body["first_kept"] = json.RawMessage("null")
		}
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	return HashRequestJSON(bodyJSON)
}
