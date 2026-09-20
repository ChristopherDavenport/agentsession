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
	OutcomeCustom    = "custom"
)

// Entry is one line of a session after the header. Concrete types are
// [ItemEntry], [ResponseEntry], [ConfigEntry], [CompactionEntry],
// [BranchSummaryEntry], [LabelEntry], [InfoEntry], [EnvEntry],
// [OutcomeEntry], [LinkEntry], [CustomEntry] and [UnknownEntry]. Decoded
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
	// Parent is the ID of the parent entry, or "" for a root.
	Parent string
	// Timestamp is when the entry was written.
	Timestamp time.Time
	// Unknown holds envelope members this package does not define,
	// preserved so a later minor version's optional fields survive a
	// rewrite. It is nil when there are none.
	Unknown map[string]json.RawMessage
}

// Base returns the envelope.
func (b *EntryBase) Base() *EntryBase { return b }

// envelope is the wire form of the common members.
type envelope struct {
	Type   string    `json:"type"`
	ID     string    `json:"id"`
	Parent *string   `json:"parent"`
	TS     time.Time `json:"ts"`
}

var envelopeKeys = []string{"type", "id", "parent", "ts"}

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
	ToolsAdded   openresponses.Tools            `json:"tools_added,omitempty"`
	ToolsRemoved []string                       `json:"tools_removed,omitempty"`
	// Extra carries request members beyond the named ones, such as
	// temperature or provider passthrough keys. A null value removes
	// the key from the settings.
	Extra   map[string]json.RawMessage `json:"extra,omitempty"`
	Replace bool                       `json:"replace,omitempty"`
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
// It is in context through its summary.
type CompactionEntry struct {
	EntryBase `json:"-"`
	// FirstKept names the earliest entry on the path that stays in
	// context.
	FirstKept string `json:"first_kept"`
	// Summary is an item: the server's compaction item verbatim, or a
	// message for a local summary.
	Summary openresponses.Item `json:"summary"`
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

// EnvEntry is a snapshot of the environment for replay.
type EnvEntry struct {
	EntryBase `json:"-"`
	CWD       string            `json:"cwd,omitempty"`
	VCS       *VCS              `json:"vcs,omitempty"`
	Files     *FileHashes       `json:"files,omitempty"`
	Tools     map[string]string `json:"tools,omitempty"`
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

// OutcomeEntry is a signal about how the session, or a range of it,
// went.
type OutcomeEntry struct {
	EntryBase `json:"-"`
	Kind      string          `json:"kind"`
	Target    string          `json:"target,omitempty"`
	Score     *float64        `json:"score,omitempty"`
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
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	e.Type = env.Type
	e.ID = env.ID
	if env.Parent != nil {
		e.Parent = *env.Parent
	}
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
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
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
	var e Entry
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
	if err := json.Unmarshal(data, e); err != nil {
		return nil, fmt.Errorf("agentsession: entry %s (%s): %w", env.ID, env.Type, err)
	}
	return e, nil
}

// marshalEntry joins the envelope, the type-specific body and the
// unknown members into one object.
func marshalEntry(typ string, base *EntryBase, body any) ([]byte, error) {
	env := envelope{Type: typ, ID: base.ID, TS: base.Timestamp}
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
func unmarshalEntry(data []byte, base *EntryBase, v any, known map[string]bool) error {
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	base.ID = env.ID
	base.Parent = ""
	if env.Parent != nil {
		base.Parent = *env.Parent
	}
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
	var aux struct {
		Item       json.RawMessage `json:"item"`
		ResponseID string          `json:"response"`
		Visible    *bool           `json:"visible"`
	}
	if err := unmarshalEntry(data, &e.EntryBase, &aux, itemKeys); err != nil {
		return err
	}
	if len(aux.Item) == 0 || string(aux.Item) == "null" {
		return errors.New("item is required")
	}
	item, err := openresponses.UnmarshalItem(aux.Item)
	if err != nil {
		return err
	}
	e.Item = item
	e.ResponseID = aux.ResponseID
	e.Visible = aux.Visible
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *ResponseEntry) MarshalJSON() ([]byte, error) {
	type plain ResponseEntry
	return marshalEntry(TypeResponse, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *ResponseEntry) UnmarshalJSON(data []byte) error {
	type plain ResponseEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), responseKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *ConfigEntry) MarshalJSON() ([]byte, error) {
	type plain ConfigEntry
	return marshalEntry(TypeConfig, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *ConfigEntry) UnmarshalJSON(data []byte) error {
	type plain ConfigEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), configKeys)
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
	var aux struct {
		FirstKept    string               `json:"first_kept"`
		Summary      json.RawMessage      `json:"summary"`
		Config       Settings             `json:"config"`
		TokensBefore int                  `json:"tokens_before"`
		Usage        *openresponses.Usage `json:"usage"`
	}
	if err := unmarshalEntry(data, &e.EntryBase, &aux, compactionKeys); err != nil {
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
	var aux struct {
		From    string               `json:"from"`
		Summary json.RawMessage      `json:"summary"`
		Usage   *openresponses.Usage `json:"usage"`
	}
	if err := unmarshalEntry(data, &e.EntryBase, &aux, branchSummaryKeys); err != nil {
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
	type plain LabelEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), labelKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *InfoEntry) MarshalJSON() ([]byte, error) {
	type plain InfoEntry
	return marshalEntry(TypeInfo, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *InfoEntry) UnmarshalJSON(data []byte) error {
	type plain InfoEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), infoKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *EnvEntry) MarshalJSON() ([]byte, error) {
	type plain EnvEntry
	return marshalEntry(TypeEnv, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *EnvEntry) UnmarshalJSON(data []byte) error {
	type plain EnvEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), envKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *OutcomeEntry) MarshalJSON() ([]byte, error) {
	type plain OutcomeEntry
	return marshalEntry(TypeOutcome, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *OutcomeEntry) UnmarshalJSON(data []byte) error {
	type plain OutcomeEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), outcomeKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *LinkEntry) MarshalJSON() ([]byte, error) {
	type plain LinkEntry
	return marshalEntry(TypeLink, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *LinkEntry) UnmarshalJSON(data []byte) error {
	type plain LinkEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), linkKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *CustomEntry) MarshalJSON() ([]byte, error) {
	type plain CustomEntry
	return marshalEntry(TypeCustom, &e.EntryBase, (*plain)(e))
}

// UnmarshalJSON decodes the entry.
func (e *CustomEntry) UnmarshalJSON(data []byte) error {
	type plain CustomEntry
	return unmarshalEntry(data, &e.EntryBase, (*plain)(e), customKeys)
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
