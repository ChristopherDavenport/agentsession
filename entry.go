package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
	"github.com/ChristopherDavenport/openresponses"
)

// Entry types defined by the format. Any other type of the form
// "ns:name" is an extension and decodes to [UnknownEntry].
const (
	TypeItem          = "item"
	TypeResponse      = "response"
	TypeConfig        = "config"
	TypeCompaction    = "compaction"
	TypeBranchSummary = "branch_summary"
	TypeLabel         = "label"
	TypeInfo          = "info"
	TypeEnv           = "env"
	TypeOutcome       = "outcome"
	TypeLink          = "link"
	TypeCustom        = "custom"
)

// Relations a [LinkEntry] may carry.
const (
	RelSubsession  = "subsession"
	RelForkOf      = "fork_of"
	RelContinuedIn = "continued_in"
)

// Kinds an [OutcomeEntry] may carry.
const (
	OutcomeFeedback  = "feedback"
	OutcomeTest      = "test"
	OutcomeTask      = "task"
	OutcomeToolError = "tool_error"
	OutcomeEval      = "eval"
	OutcomeCustom    = "custom"
)

// Entry is one line of a session after the header. Concrete types are
// [ItemEntry], [ResponseEntry], [ConfigEntry], [CompactionEntry],
// [BranchSummaryEntry], [RunEntry], [DispatchEntry], [DecisionEntry],
// [QueuedEntry], [LabelEntry], [InfoEntry], [EnvEntry],
// [OutcomeEntry], [LinkEntry],
// [CustomEntry] and [UnknownEntry]. Decoded
// values are always pointers, so switch on *ItemEntry and so on. An
// entry must not be modified once it has been appended to a session;
// corrections are new entries.
type Entry interface {
	Base() *EntryBase
	EntryType() string
}

// EntryBase is the envelope every entry carries.
type EntryBase struct {
	// ID is unique within the session. Session.Append assigns one when
	// it is empty.
	ID string
	// Parent is the ID of the parent entry, or "" for a root. It is the
	// entry's line of descent: exactly one, in this session, and the
	// only edge a context is built from.
	Parent string
	// Parents records further predecessors this entry converges: the
	// result of a subagent session, a branch merged back, several
	// workers joined at once. It is provenance. Nothing it names
	// contributes to any context by virtue of being named — whatever
	// crossed the boundary is in this entry's own payload, materialised
	// — so [Session.Path] and every context the library builds follow
	// Parent alone. Session.Append sorts it.
	Parents []EntryRef
	// Timestamp is when the entry was written.
	Timestamp time.Time
	// Unknown holds envelope members this package does not define,
	// preserved so a later minor version's optional fields survive a
	// rewrite. It is nil when there are none.
	Unknown map[string]json.RawMessage
}

// Base returns the envelope.
func (b *EntryBase) Base() *EntryBase { return b }

// EntryRef names one entry, here or in another session. It is what
// [EntryBase.Parents] holds.
type EntryRef struct {
	// Session names the session Entry is in. It is empty when the entry
	// is in this session, which is the only case a reader can resolve
	// without a store.
	Session string `json:"session,omitempty"`
	// Entry is the ID of the entry referred to.
	Entry string `json:"entry"`
}

// envelope is the wire form of the common members.
type envelope struct {
	Type    string     `json:"type"`
	ID      string     `json:"id"`
	Parent  *string    `json:"parent"`
	Parents []EntryRef `json:"parents,omitempty"`
	TS      time.Time  `json:"ts"`
}

var envelopeKeys = []string{"type", "id", "parent", "parents", "ts"}

// ItemEntry is one conversation item in the payload profile. It is in
// model context.
type ItemEntry struct {
	EntryBase `json:"-"`
	// Item is the Open Responses item, verbatim. Extension items decode
	// to *openresponses.UnknownItem and re-encode byte for byte.
	Item openresponses.Item `json:"item"`
	// ResponseID names the response this item belongs to when the item
	// was model output; it matches ResponseEntry.ResponseID.
	ResponseID string `json:"response,omitempty"`
	// Visible is false for an item that is in context but that a
	// renderer should hide. Nil means visible.
	Visible *bool `json:"visible,omitempty"`
	// Source says how the input arrived, for an item a person or
	// another system sent rather than the model or the loop. It is the
	// trigger a [QueuedEntry] carried, so two people steering one run
	// are told apart.
	Source *Trigger `json:"source,omitempty"`
	// QueuedFrom names the [QueuedEntry] this item was accepted as,
	// for an input that waited before it could be appended.
	QueuedFrom string `json:"queued_from,omitempty"`
}

// EntryType returns "item".
func (*ItemEntry) EntryType() string { return TypeItem }

// IsVisible reports whether a renderer should show the item.
func (e *ItemEntry) IsVisible() bool { return e.Visible == nil || *e.Visible }

// NewItemEntry builds an item entry.
func NewItemEntry(item openresponses.Item) *ItemEntry {
	return &ItemEntry{Item: item}
}

// ResponseEntry is the envelope of one model call, written after its
// output items. It is not in context.
type ResponseEntry struct {
	EntryBase   `json:"-"`
	ResponseID  string                           `json:"response_id"`
	Model       string                           `json:"model,omitempty"`
	Status      openresponses.ResponseStatus     `json:"status"`
	Usage       *openresponses.Usage             `json:"usage,omitempty"`
	Incomplete  *openresponses.IncompleteDetails `json:"incomplete,omitempty"`
	Error       *openresponses.ErrorPayload      `json:"error,omitempty"`
	RequestHash string                           `json:"request_hash,omitempty"`
	LatencyMS   int64                            `json:"latency_ms,omitempty"`
}

// EntryType returns "response".
func (*ResponseEntry) EntryType() string { return TypeResponse }

// ConfigEntry is a delta to request settings. Fields left at their zero
// value are unchanged; Replace discards all earlier config on the path
// before this one applies. The first entry on a root should be a config
// carrying full settings.
type ConfigEntry struct {
	EntryBase    `json:"-"`
	Model        string                         `json:"model,omitempty"`
	Instructions *string                        `json:"instructions,omitempty"`
	Reasoning    *openresponses.ReasoningConfig `json:"reasoning,omitempty"`
	Text         *openresponses.TextConfig      `json:"text,omitempty"`
	// InstructionsParts names the parts the instructions are composed
	// of, in the order they are joined. A delta carries the whole
	// ordered list: a part whose text changed carries its text, a part
	// whose text is unchanged carries its Hash alone, and a part left
	// out of the list is removed. Set it through
	// [Settings.InstructionsDelta] rather than by hand, which computes
	// exactly that from the parts in force. When Instructions is set
	// beside it the two must agree, since Instructions is the parts
	// joined with "\n\n".
	InstructionsParts []InstructionPart `json:"instructions_parts,omitempty"`
	// InstructionsOmitted records the parts the writer considered and
	// left out, so a session says what the model was not given as well
	// as what it was. It is not settings: nothing in it reaches the
	// request, and it applies to this entry alone.
	InstructionsOmitted []OmittedPart       `json:"instructions_omitted,omitempty"`
	ToolsAdded          openresponses.Tools `json:"tools_added,omitempty"`
	ToolsRemoved        []string            `json:"tools_removed,omitempty"`
	// Extra carries request members beyond the named ones, such as
	// temperature or provider passthrough keys. A null value removes
	// the key from the settings.
	Extra   map[string]json.RawMessage `json:"extra,omitempty"`
	Replace bool                       `json:"replace,omitempty"`
}

// InstructionPart is one named part of the instructions. A harness
// that composes the instructions from several layers, a product
// prompt, an AGENTS.md chain, a skill catalogue, a memory block,
// gives each layer a part, so a change to one is recorded as a change
// to one and a reader can say which layer an instruction came from.
type InstructionPart struct {
	// ID is the part's stable name, chosen by the harness: the same
	// string across the session, so a delta can name a part it does
	// not repeat.
	ID string `json:"id"`
	// Text is the part's text. On a delta it is absent for a part
	// whose text is unchanged, which carries Hash instead.
	Text string `json:"text,omitempty"`
	// Source names the layer that produced the part, in the harness's
	// own terms.
	Source string `json:"source,omitempty"`
	// Hash is the hash of the text a delta does not repeat, in the
	// format's notation: [HashPrefix] and the SHA-256 of the text.
	// [HashText] computes it. It is set on a delta's unchanged part
	// and empty on a part that carries its text.
	Hash string `json:"hash,omitempty"`
}

// OmittedPart is a part the writer considered for the instructions
// and left out: a file the budget did not reach, a memory entry that
// did not fit, a skill out of scope. Reason is the writer's own word
// for why, Size the bytes the part would have added.
type OmittedPart struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
	Size   int    `json:"size,omitempty"`
	Source string `json:"source,omitempty"`
}

// SetExtra records a passthrough request member on the delta: v is
// marshalled and stored under key, replacing any earlier value.
func (c *ConfigEntry) SetExtra(key string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("agentsession: config extra %q: %w", key, err)
	}
	if c.Extra == nil {
		c.Extra = make(map[string]json.RawMessage)
	}
	c.Extra[key] = data
	return nil
}

// ClearExtra records the removal of a passthrough member: the delta
// carries a null for key, which deletes it from the settings on
// replay.
func (c *ConfigEntry) ClearExtra(key string) {
	if c.Extra == nil {
		c.Extra = make(map[string]json.RawMessage)
	}
	c.Extra[key] = json.RawMessage("null")
}

// EntryType returns "config".
func (*ConfigEntry) EntryType() string { return TypeConfig }

// CompactionEntry replaces the context before FirstKept with a summary.
// It is in context through its summary and its pinned items.
type CompactionEntry struct {
	EntryBase `json:"-"`
	// FirstKept names the earliest entry on the path that stays in
	// context.
	FirstKept string `json:"first_kept"`
	// Summary is an item: the server's compaction item verbatim, or a
	// message for a local summary.
	Summary openresponses.Item `json:"summary"`
	// Pinned are items kept verbatim from before FirstKept. They are in
	// context immediately after Summary and before the entries from
	// FirstKept.
	Pinned openresponses.Items `json:"pinned,omitempty"`
	// Config is a full settings checkpoint so a reader need not replay
	// config entries from before the compaction.
	Config       Settings             `json:"config"`
	TokensBefore int                  `json:"tokens_before,omitempty"`
	Usage        *openresponses.Usage `json:"usage,omitempty"`
}

// EntryType returns "compaction".
func (*CompactionEntry) EntryType() string { return TypeCompaction }

// BranchSummaryEntry carries context across a branch switch. Its parent
// is where the new branch continues; From is the leaf that was left. It
// is in context through its summary.
type BranchSummaryEntry struct {
	EntryBase `json:"-"`
	From      string               `json:"from"`
	Summary   openresponses.Item   `json:"summary"`
	Usage     *openresponses.Usage `json:"usage,omitempty"`
}

// EntryType returns "branch_summary".
func (*BranchSummaryEntry) EntryType() string { return TypeBranchSummary }

// LeafLabel is the reserved label that makes the leaf durable. A label
// entry carrying it names the branch the next append should continue,
// not a fixed entry to pin the leaf at: [Session.Append] keeps the leaf
// at the label's target rather than moving it to the label entry, and
// [Read] resolves the leaf to the newest entry appended under that
// target after the label, so a branch marked and then written on
// resumes where it was written to rather than rewinding to the mark. A
// mark nothing followed resolves to itself. A null label on the same
// target clears it. The format holds this as a library convention; see
// the RFC's open questions.
const LeafLabel = "leaf"

// LabelEntry bookmarks another entry. A nil Label clears an earlier
// label on the same target.
type LabelEntry struct {
	EntryBase `json:"-"`
	Target    string  `json:"target"`
	Label     *string `json:"label"`
}

// EntryType returns "label".
func (*LabelEntry) EntryType() string { return TypeLabel }

// InfoEntry carries display metadata such as a session name. Further
// members are kept in Unknown.
type InfoEntry struct {
	EntryBase `json:"-"`
	Name      string `json:"name,omitempty"`
}

// EntryType returns "info".
func (*InfoEntry) EntryType() string { return TypeInfo }

// EnvEntry is a snapshot of the environment for replay: where the
// tools ran and what they saw. It applies from its position on the
// path until the next one, and its CWD takes precedence over the
// header's.
type EnvEntry struct {
	EntryBase `json:"-"`
	CWD       string            `json:"cwd,omitempty"`
	VCS       *VCS              `json:"vcs,omitempty"`
	Files     *FileHashes       `json:"files,omitempty"`
	Tools     map[string]string `json:"tools,omitempty"`
	// Workspace says which file system CWD is a path in. A local run
	// may leave it nil.
	Workspace *Workspace `json:"workspace,omitempty"`
}

// EntryType returns "env".
func (*EnvEntry) EntryType() string { return TypeEnv }

// VCS is the version-control state of the working directory.
type VCS struct {
	System   string `json:"system"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
}

// FileHashes maps paths to content hashes ("sha256:...") for files read
// and written.
type FileHashes struct {
	Read    map[string]string `json:"read,omitempty"`
	Written map[string]string `json:"written,omitempty"`
}

// OutcomeEntry is a judgement of how the session, or a range of it,
// went. Target names an entry in this session, usually the last entry
// of the range judged; a task or test name belongs in Details. Score
// is any finite number on the judge's own scale, named by Label; Pass
// is the judge's verdict when it has one.
type OutcomeEntry struct {
	EntryBase `json:"-"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target,omitempty"`
	Score     *float64        `json:"score,omitempty"`
	Pass      *bool           `json:"pass,omitempty"`
	Label     string          `json:"label,omitempty"`
	Details   json.RawMessage `json:"details,omitempty"`
}

// EntryType returns "outcome".
func (*OutcomeEntry) EntryType() string { return TypeOutcome }

// LinkEntry references another session, for subagents and forks.
type LinkEntry struct {
	EntryBase `json:"-"`
	Rel       string `json:"rel"`
	Session   string `json:"session"`
	// CallID ties a subsession to the function call that spawned it.
	CallID string `json:"call_id,omitempty"`
}

// EntryType returns "link".
func (*LinkEntry) EntryType() string { return TypeLink }

// CustomEntry is application state that is not in context. State that
// the model should see is an [ItemEntry] holding a namespaced item.
type CustomEntry struct {
	EntryBase `json:"-"`
	NS        string          `json:"ns"`
	Data      json.RawMessage `json:"data,omitempty"`
}

// EntryType returns "custom".
func (*CustomEntry) EntryType() string { return TypeCustom }

// UnknownEntry is an entry whose type this package does not define,
// typically a namespaced extension. Raw holds the original line and is
// re-emitted verbatim. It is not in context.
type UnknownEntry struct {
	EntryBase
	Type string
	Raw  json.RawMessage
}

// EntryType returns the wire type.
func (e *UnknownEntry) EntryType() string { return e.Type }

// MarshalJSON emits the original bytes.
func (e *UnknownEntry) MarshalJSON() ([]byte, error) {
	if len(e.Raw) == 0 {
		return nil, errors.New("agentsession: unknown entry has no raw bytes")
	}
	return e.Raw, nil
}

// UnmarshalJSON records the envelope and the raw bytes.
func (e *UnknownEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *UnknownEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	env, err := envelopeFrom(all)
	if err != nil {
		return err
	}
	e.Type = env.Type
	e.ID = env.ID
	if env.Parent != nil {
		e.Parent = *env.Parent
	}
	e.Parents = env.Parents
	e.Timestamp = env.TS
	e.Raw = append(json.RawMessage(nil), data...)
	return nil
}

// InContext reports whether an entry contributes an item to the model
// context: item, branch_summary and compaction entries do. Whether a
// compaction is actually selected depends on the path; see
// [Session.ContextAt].
func InContext(e Entry) bool {
	switch e.(type) {
	case *ItemEntry, *BranchSummaryEntry, *CompactionEntry:
		return true
	}
	return false
}

// IsExtension reports whether typ is a namespaced extension type.
func IsExtension(typ string) bool {
	return strings.Contains(typ, ":")
}

// --- encoding ---

// MarshalEntry encodes one entry as a single JSON line without the
// trailing newline. The envelope members come first in a fixed order,
// then the type's members, then any unknown members in key order.
func MarshalEntry(e Entry) ([]byte, error) {
	if e == nil {
		return nil, errors.New("agentsession: nil entry")
	}
	return jsonx.MarshalNoEscape(e)
}

// UnmarshalEntry decodes one line, dispatching on its type. Types this
// package does not define decode to [*UnknownEntry].
func UnmarshalEntry(data []byte) (Entry, error) {
	// The line is split into its members once; the envelope, the
	// unknown-member check and, for item entries, the body all read
	// from the split, so a large line is not parsed again for each.
	all, err := splitMembers(data)
	if err != nil {
		return nil, err
	}
	env, err := envelopeFrom(all)
	if err != nil {
		return nil, err
	}
	if env.Type == "" {
		return nil, errors.New("agentsession: entry has no type")
	}
	if env.ID == "" {
		return nil, fmt.Errorf("agentsession: %s entry has no id", env.Type)
	}
	if env.TS.IsZero() {
		return nil, fmt.Errorf("agentsession: entry %s has no ts", env.ID)
	}
	var e memberDecoder
	switch env.Type {
	case TypeItem:
		e = &ItemEntry{}
	case TypeResponse:
		e = &ResponseEntry{}
	case TypeConfig:
		e = &ConfigEntry{}
	case TypeCompaction:
		e = &CompactionEntry{}
	case TypeBranchSummary:
		e = &BranchSummaryEntry{}
	case TypeRun:
		e = &RunEntry{}
	case TypeDispatch:
		e = &DispatchEntry{}
	case TypeDecision:
		e = &DecisionEntry{}
	case TypeQueued:
		e = &QueuedEntry{}
	case TypeLabel:
		e = &LabelEntry{}
	case TypeInfo:
		e = &InfoEntry{}
	case TypeEnv:
		e = &EnvEntry{}
	case TypeOutcome:
		e = &OutcomeEntry{}
	case TypeLink:
		e = &LinkEntry{}
	case TypeCustom:
		e = &CustomEntry{}
	default:
		e = &UnknownEntry{}
	}
	if err := e.decodeMembers(data, all); err != nil {
		return nil, fmt.Errorf("agentsession: entry %s (%s): %w", env.ID, env.Type, err)
	}
	return e, nil
}

// memberDecoder is implemented by every entry type: decode from the
// line and its split members. UnmarshalJSON on each type splits the
// line itself and calls this, so both paths decode identically.
type memberDecoder interface {
	Entry
	decodeMembers(data []byte, all map[string]json.RawMessage) error
}

// splitMembers parses one line into its top-level members.
func splitMembers(data []byte) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	if all == nil {
		return nil, errors.New("agentsession: entry is not an object")
	}
	return all, nil
}

// envelopeFrom reads the common members out of a split line.
func envelopeFrom(all map[string]json.RawMessage) (envelope, error) {
	env := envelope{Type: jsonx.PeekString(all["type"]), ID: jsonx.PeekString(all["id"])}
	if raw, ok := all["parent"]; ok && !isNull(raw) {
		var parent string
		if err := json.Unmarshal(raw, &parent); err != nil {
			return env, fmt.Errorf("parent: %w", err)
		}
		env.Parent = &parent
	}
	if raw, ok := all["parents"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &env.Parents); err != nil {
			return env, fmt.Errorf("parents: %w", err)
		}
	}
	if raw, ok := all["ts"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &env.TS); err != nil {
			return env, fmt.Errorf("ts: %w", err)
		}
	}
	return env, nil
}

// marshalEntry joins the envelope, the type-specific body and the
// unknown members into one object.
func marshalEntry(typ string, base *EntryBase, body any) ([]byte, error) {
	env := envelope{Type: typ, ID: base.ID, Parents: base.Parents, TS: base.Timestamp}
	if base.Parent != "" {
		env.Parent = &base.Parent
	}
	head, err := jsonx.MarshalNoEscape(env)
	if err != nil {
		return nil, err
	}
	b, err := jsonx.MarshalNoEscape(body)
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(head, b, base.Unknown), nil
}

// unmarshalEntry fills base from the envelope in data, decodes the body
// into v and records members outside known as unknown.
func unmarshalEntry(data []byte, all map[string]json.RawMessage, base *EntryBase, v any, known map[string]bool) error {
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	return fillBase(all, base, known)
}

// fillBase sets the envelope and the unknown members from a split
// line.
func fillBase(all map[string]json.RawMessage, base *EntryBase, known map[string]bool) error {
	env, err := envelopeFrom(all)
	if err != nil {
		return err
	}
	base.ID = env.ID
	base.Parent = ""
	if env.Parent != nil {
		base.Parent = *env.Parent
	}
	base.Parents = env.Parents
	base.Timestamp = env.TS
	base.Unknown = jsonx.ExtraKeys(all, known, envelopeKeys...)
	return nil
}

var (
	itemKeys          = jsonx.Keys[ItemEntry]()
	responseKeys      = jsonx.Keys[ResponseEntry]()
	configKeys        = jsonx.Keys[ConfigEntry]()
	compactionKeys    = jsonx.Keys[CompactionEntry]()
	branchSummaryKeys = jsonx.Keys[BranchSummaryEntry]()
	labelKeys         = jsonx.Keys[LabelEntry]()
	infoKeys          = jsonx.Keys[InfoEntry]()
	envKeys           = jsonx.Keys[EnvEntry]()
	outcomeKeys       = jsonx.Keys[OutcomeEntry]()
	linkKeys          = jsonx.Keys[LinkEntry]()
	customKeys        = jsonx.Keys[CustomEntry]()
)

// MarshalJSON emits the entry as one JSON object.
func (e *ItemEntry) MarshalJSON() ([]byte, error) {
	if e.Item == nil {
		return nil, errors.New("agentsession: item entry has no item")
	}
	type plain ItemEntry
	return marshalEntry(TypeItem, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry, dispatching the item through the
// openresponses item registry.
func (e *ItemEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

// decodeMembers reads the item entry from the split alone: item lines
// carry the bulk of a session, and the item is decoded by the
// openresponses registry from its raw member without another pass
// over the line.
func (e *ItemEntry) decodeMembers(_ []byte, all map[string]json.RawMessage) error {
	if err := fillBase(all, &e.EntryBase, itemKeys); err != nil {
		return err
	}
	raw := all["item"]
	if len(raw) == 0 || isNull(raw) {
		return errors.New("item is required")
	}
	item, err := openresponses.UnmarshalItem(raw)
	if err != nil {
		return err
	}
	e.Item = item
	e.ResponseID = ""
	if raw, ok := all["response"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &e.ResponseID); err != nil {
			return fmt.Errorf("response: %w", err)
		}
	}
	e.Visible = nil
	if raw, ok := all["visible"]; ok && !isNull(raw) {
		var visible bool
		if err := json.Unmarshal(raw, &visible); err != nil {
			return fmt.Errorf("visible: %w", err)
		}
		e.Visible = &visible
	}
	e.Source = nil
	if raw, ok := all["source"]; ok && !isNull(raw) {
		var source Trigger
		if err := json.Unmarshal(raw, &source); err != nil {
			return fmt.Errorf("source: %w", err)
		}
		e.Source = &source
	}
	e.QueuedFrom = ""
	if raw, ok := all["queued_from"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &e.QueuedFrom); err != nil {
			return fmt.Errorf("queued_from: %w", err)
		}
	}
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *ResponseEntry) MarshalJSON() ([]byte, error) {
	type plain ResponseEntry
	return marshalEntry(TypeResponse, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *ResponseEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *ResponseEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain ResponseEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), responseKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *ConfigEntry) MarshalJSON() ([]byte, error) {
	type plain ConfigEntry
	return marshalEntry(TypeConfig, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *ConfigEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *ConfigEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain ConfigEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), configKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *CompactionEntry) MarshalJSON() ([]byte, error) {
	if e.Summary == nil {
		return nil, errors.New("agentsession: compaction entry has no summary")
	}
	type plain CompactionEntry
	return marshalEntry(TypeCompaction, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *CompactionEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

// decodeMembers decodes into aux and then assigns, because Summary is
// an interface that needs the item registry.
//
// The aux struct must list every member CompactionEntry declares. A
// member added to the struct and not to aux is dropped in silence: it
// is in compactionKeys, so the envelope rule treats it as known and
// does not preserve it in Unknown either, and the only symptom is a
// round trip that loses it. TestCompactionMembersSurviveARoundTrip
// guards this.
func (e *CompactionEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	var aux struct {
		FirstKept    string               `json:"first_kept"`
		Summary      json.RawMessage      `json:"summary"`
		Pinned       openresponses.Items  `json:"pinned"`
		Config       Settings             `json:"config"`
		TokensBefore int                  `json:"tokens_before"`
		Usage        *openresponses.Usage `json:"usage"`
	}
	if err := unmarshalEntry(data, all, &e.EntryBase, &aux, compactionKeys); err != nil {
		return err
	}
	if len(aux.Summary) == 0 || string(aux.Summary) == "null" {
		return errors.New("summary is required")
	}
	summary, err := openresponses.UnmarshalItem(aux.Summary)
	if err != nil {
		return err
	}
	e.FirstKept = aux.FirstKept
	e.Summary = summary
	e.Pinned = aux.Pinned
	e.Config = aux.Config
	e.TokensBefore = aux.TokensBefore
	e.Usage = aux.Usage
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *BranchSummaryEntry) MarshalJSON() ([]byte, error) {
	if e.Summary == nil {
		return nil, errors.New("agentsession: branch_summary entry has no summary")
	}
	type plain BranchSummaryEntry
	return marshalEntry(TypeBranchSummary, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *BranchSummaryEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *BranchSummaryEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	var aux struct {
		From    string               `json:"from"`
		Summary json.RawMessage      `json:"summary"`
		Usage   *openresponses.Usage `json:"usage"`
	}
	if err := unmarshalEntry(data, all, &e.EntryBase, &aux, branchSummaryKeys); err != nil {
		return err
	}
	if len(aux.Summary) == 0 || string(aux.Summary) == "null" {
		return errors.New("summary is required")
	}
	summary, err := openresponses.UnmarshalItem(aux.Summary)
	if err != nil {
		return err
	}
	e.From = aux.From
	e.Summary = summary
	e.Usage = aux.Usage
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *LabelEntry) MarshalJSON() ([]byte, error) {
	type plain LabelEntry
	return marshalEntry(TypeLabel, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *LabelEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *LabelEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain LabelEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), labelKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *InfoEntry) MarshalJSON() ([]byte, error) {
	type plain InfoEntry
	return marshalEntry(TypeInfo, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *InfoEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *InfoEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain InfoEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), infoKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *EnvEntry) MarshalJSON() ([]byte, error) {
	type plain EnvEntry
	return marshalEntry(TypeEnv, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *EnvEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *EnvEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain EnvEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), envKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *OutcomeEntry) MarshalJSON() ([]byte, error) {
	type plain OutcomeEntry
	return marshalEntry(TypeOutcome, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *OutcomeEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *OutcomeEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain OutcomeEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), outcomeKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *LinkEntry) MarshalJSON() ([]byte, error) {
	type plain LinkEntry
	return marshalEntry(TypeLink, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *LinkEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *LinkEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain LinkEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), linkKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *CustomEntry) MarshalJSON() ([]byte, error) {
	type plain CustomEntry
	return marshalEntry(TypeCustom, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *CustomEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return e.decodeMembers(data, all)
}

func (e *CustomEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain CustomEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), customKeys)
}

var (
	_ Entry = (*ItemEntry)(nil)
	_ Entry = (*ResponseEntry)(nil)
	_ Entry = (*ConfigEntry)(nil)
	_ Entry = (*CompactionEntry)(nil)
	_ Entry = (*BranchSummaryEntry)(nil)
	_ Entry = (*LabelEntry)(nil)
	_ Entry = (*InfoEntry)(nil)
	_ Entry = (*EnvEntry)(nil)
	_ Entry = (*OutcomeEntry)(nil)
	_ Entry = (*LinkEntry)(nil)
	_ Entry = (*CustomEntry)(nil)
	_ Entry = (*UnknownEntry)(nil)
)
