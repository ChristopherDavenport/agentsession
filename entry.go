package agentsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
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
	// RelJudgedBy is written into a judged session and names the
	// session of its judge; see [NewJudgedByLink] and [Judges].
	RelJudgedBy = "judged_by"
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
	// Timestamp is when the entry was written. The format admits one
	// spelling, UTC with at most nine fractional digits, so Append
	// converts a caller's time to UTC.
	Timestamp time.Time
	// LegacyID is the id an entry had before its file was migrated to
	// 0.5, when its id became the envelope hash. It is a body member,
	// so it is hashed; a projection may emit it beside the new id so
	// output made from the earlier file still resolves.
	LegacyID string
	// Normalised records what a writer changed in the body before
	// writing so that it passed the I-JSON test: a lone surrogate to
	// U+FFFD, an integer outside binary64 to a string or to its rounded
	// value. It is sorted by At as UTF-16 code units.
	Normalised []Normalisation
	// Unknown holds body members this package does not define,
	// preserved so a later minor version's optional fields survive a
	// rewrite. It is nil when there are none.
	Unknown map[string]json.RawMessage

	// content is the content hash, computed when the entry was appended
	// or read; tsRaw is the ts member as the line spelled it.
	content string
	tsRaw   string
	// kept holds, for an entry that was read, each member the line held
	// more of than the typed fields encode: see keepAsRead.
	kept map[string]keptMember
	// line is, for an entry that was read, the line itself when what
	// the typed fields encode does not reproduce it: see keptLine.
	line *keptLine
}

// keptMember is a member as the line held it, raw, which is nil when
// the line did not have it, and as the typed fields encoded it when the
// entry was read, seen, which is nil when they left it out.
type keptMember struct{ raw, seen json.RawMessage }

// keptLine is a read line's canonical bytes, kept for a core entry
// whose typed fields, with what keepAsRead kept grafted back, encode
// something else: a member the line leaves out that a field adds, or
// spells in another form. The line's id is the hash of the line, so the
// line is what marshalEntry writes back for as long as the fields
// encode what they did at read; fp is the hash of that encoding with
// the id left out, so a caller's change to any member drops the line
// and an id assigned again does not.
type keptLine struct {
	c   []byte
	id  string
	fp  [sha256.Size]byte
	set bool
}

// emit returns the kept line with id as its id.
func (l *keptLine) emit(id string) ([]byte, error) {
	if id == l.id {
		return append([]byte(nil), l.c...), nil
	}
	members, ok := canonicalMembers(l.c)
	if !ok {
		return nil, errors.New("agentsession: kept line is not an object")
	}
	for _, m := range members {
		if m.key != "id" {
			continue
		}
		v, err := jsonx.MarshalNoEscape(id)
		if err != nil {
			return nil, err
		}
		at := cap(l.c) - cap(m.val) // m.val is a slice of l.c
		out := make([]byte, 0, len(l.c)+len(v))
		out = append(out, l.c[:at]...)
		out = append(out, v...)
		return append(out, l.c[at+len(m.val):]...), nil
	}
	return nil, errors.New("agentsession: kept line has no id")
}

// ContentHash returns the hash of the entry's body, the members outside
// the envelope, as computed when the entry was appended or read. It is
// empty for an entry that has been neither.
func (b *EntryBase) ContentHash() string { return b.content }

// Normalisation is one change a writer made to a body before writing
// it, recorded in [EntryBase.Normalised].
type Normalisation struct {
	// At is an RFC 6901 JSON Pointer relative to the body.
	At string `json:"at"`
	// Was is the member's original JSON source text, escapes included,
	// so it is I-JSON whatever it describes. It is empty when the
	// output was not valid UTF-8 and had no JSON text to record.
	Was string `json:"was,omitempty"`
	// Raw carries the original bytes, base64 under RFC 4648 §4 with
	// padding, when Was cannot: output that was not valid UTF-8.
	Raw string `json:"raw,omitempty"`
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

// UnmarshalJSON decodes a reference by its members' exact names, as the
// format names them: a key in another case is a member the format does
// not define. An entry member that is missing or not a string decodes
// as no entry, which the convergence rules refuse.
func (r *EntryRef) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	*r = EntryRef{Entry: jsonx.PeekString(all["entry"])}
	if raw, ok := all["session"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &r.Session); err != nil {
			return fmt.Errorf("session: %w", err)
		}
	}
	return nil
}

// envelope is the wire form of the common members.
type envelope struct {
	Type    string     `json:"type"`
	ID      string     `json:"id"`
	Parent  *string    `json:"parent"`
	Parents []EntryRef `json:"parents,omitempty"`
	TS      time.Time  `json:"ts"`
}

// leapSecond matches the seconds field of an RFC 3339 time spelled 60.
var leapSecond = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}[Tt]\d{2}:\d{2}:)60((?:\.\d+)?(?:[Zz]|[+-]\d{2}:\d{2}))$`)

// envelopeKeys are the envelope's names, all reserved: a body MUST NOT
// carry a top-level member by any of them. content is computed and
// never written, but a body carrying one would collide with it.
var envelopeKeys = []string{"type", "id", "parent", "parents", "ts", "content"}

// commonBodyKeys are body members every entry type may carry.
var commonBodyKeys = []string{"legacy_id", "normalised"}

// ErrReservedMember is returned for a body carrying a top-level member
// by one of the envelope's names.
var ErrReservedMember = errors.New("agentsession: body carries a reserved envelope name")

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
	// Attempts is the number of calls to the model this response took,
	// itself included, when the harness retried calls that failed and
	// recorded none of them as a response entry of its own. Zero means
	// one. An attempts member that is not a positive integer, which a
	// file from before the member was defined may hold, is kept as
	// written in Unknown and Attempts is zero.
	Attempts int `json:"-" member:"attempts"`
}

// Calls returns the number of calls to the model the response took:
// Attempts, or one when it is not set.
func (e *ResponseEntry) Calls() int {
	return max(e.Attempts, 1)
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
	// as what it was. Nothing in it reaches the request, but it stays
	// in force until a later config entry carries it: nil leaves the
	// list in force as it was, and an empty, non-nil list, written as
	// [], clears it. A writer sets it only when the list changed, and
	// through [Settings.OmittedDelta], which names each run of parts
	// that stay omitted by a keep, and, in a session of format 0.11, a
	// run of the list an earlier entry wrote by a keep carrying of.
	InstructionsOmitted []OmittedPart       `json:"instructions_omitted,omitzero"`
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
	// not repeat. It is empty on a keep, and on a part in force that
	// an element naming nothing left unresolved.
	ID string `json:"id,omitempty"`
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
	// Keep, on a delta, stands for the next Keep parts in force,
	// unchanged, and is the element's only member: see
	// [Settings.InstructionsDelta]. A keep member that is not a
	// positive integer, which a file from before the member was defined
	// may hold, is kept as written and Keep is zero; on an element that
	// names an ID it means nothing.
	Keep int `json:"keep,omitempty"`

	// unresolved marks a part in force whose text the path could not
	// rebuild and that carries neither the Hash nor the Keep that says
	// so: an element with no ID and no keep.
	unresolved bool
}

// Unresolved reports whether the path could not rebuild the part's
// text: it was named by a hash or a keep the path could not resolve,
// or by an element that names nothing.
func (p InstructionPart) Unresolved() bool {
	return p.Text == "" && p.Hash != "" || p.ID == "" && p.Keep > 0 || p.unresolved
}

// UnmarshalJSON decodes the part, taking keep only when it is a
// positive integer written as digits, so an earlier file's keep in any
// other form stays a member this package does not define.
func (p *InstructionPart) UnmarshalJSON(data []byte) error {
	type plain InstructionPart
	var v struct {
		plain
		Keep json.RawMessage `json:"keep"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*p = InstructionPart(v.plain)
	p.Keep = 0
	if n, err := strconv.ParseInt(string(v.Keep), 10, 32); err == nil && n > 0 {
		p.Keep = int(n)
	}
	return nil
}

// OmittedPart is a part the writer considered for the instructions
// and left out: a file the budget did not reach, a memory entry that
// did not fit, a skill out of scope. Reason is the writer's own word
// for why, Size the bytes the part would have added.
type OmittedPart struct {
	// ID names the part. It is empty on a keep, and on an element in
	// force that names no part: see [OmittedPart.Unresolved].
	ID     string `json:"id,omitempty"`
	Reason string `json:"reason,omitempty"`
	Size   int    `json:"size,omitempty"`
	Source string `json:"source,omitempty"`
	// Keep, on a config delta, stands for the next Keep parts of the
	// list in force, unchanged, and is the element's only member: see
	// [Settings.OmittedDelta]. A keep member that is not a positive
	// integer, which a file from before the member was defined may
	// hold, is kept as written and Keep is zero; on an element that
	// names an ID it means nothing.
	Keep int `json:"keep,omitempty"`
	// Of, on a keep, names a config entry earlier on the path: the keep
	// counts over the list that entry put in force instead of the list
	// in force before this entry, as format 0.11 lets a writer name the
	// list an earlier entry wrote. It is the id of that entry. It means
	// nothing beside an ID or without a Keep, and an of member that is
	// not a non-empty string, which a file from before the member was
	// defined may hold, is kept as written and Of is empty. See
	// [Settings.OmittedDelta].
	Of string `json:"of,omitempty"`
}

// Unresolved reports whether the element names no part: a keep the
// path could not satisfy, left in the list in force as written, or an
// element with neither an ID nor a keep.
func (p OmittedPart) Unresolved() bool { return p.ID == "" }

// UnmarshalJSON decodes the part, taking keep only when it is a
// positive integer written as digits and of only when it is a
// non-empty string, so an earlier file's member in any other form stays
// a member this package does not define.
func (p *OmittedPart) UnmarshalJSON(data []byte) error {
	type plain OmittedPart
	var v struct {
		plain
		Keep json.RawMessage `json:"keep"`
		Of   json.RawMessage `json:"of"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*p = OmittedPart(v.plain)
	p.Keep, p.Of = 0, ""
	if n, err := strconv.ParseInt(string(v.Keep), 10, 32); err == nil && n > 0 {
		p.Keep = int(n)
	}
	var of string
	if json.Unmarshal(v.Of, &of) == nil {
		p.Of = of
	}
	return nil
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
//
// What a harness records beyond these members, such as the identity of
// a working tree's contents, goes in a namespaced member: one of the
// entry, kept in Unknown (EntryBase), or one inside vcs, set with
// [VCS.SetMember]. Only workspace is compared for a substitution, so a
// change to any other member, those the format does not define
// included, is not one: see [SameWorkspace].
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

// VCS is the version-control state of the working directory. Revision
// and Dirty cannot tell two dirty trees on one revision apart; a
// harness that needs to, as one that restores files to a checkpoint
// does, records the tree's identity in a namespaced member, such as
// "cline:tree", with [VCS.SetMember]. A change to it is not a
// substitution, since only workspace is compared. Replacing an entry's
// VCS whole drops such members; a caller changing a field edits the VCS
// it has, or a copy of it, which keeps them.
type VCS struct {
	System   string `json:"system"`
	Revision string `json:"revision,omitempty"`
	Dirty    bool   `json:"dirty,omitempty"`
	// Unknown holds the members of vcs the format does not define,
	// encoded inline beside system, revision and dirty. It is nil when
	// there are none.
	Unknown map[string]json.RawMessage `json:"-"`
}

// vcsMembers are the members of vcs the format defines.
var vcsMembers = map[string]bool{"system": true, "revision": true, "dirty": true}

// SetMember sets a member of vcs the format does not define, which
// should be namespaced, such as "cline:tree".
func (v *VCS) SetMember(key string, val any) error {
	if vcsMembers[key] {
		return fmt.Errorf("agentsession: vcs member %q is defined; set the field", key)
	}
	data, err := jsonx.MarshalNoEscape(val)
	if err != nil {
		return fmt.Errorf("agentsession: vcs member %q: %w", key, err)
	}
	if v.Unknown == nil {
		v.Unknown = make(map[string]json.RawMessage)
	}
	v.Unknown[key] = data
	return nil
}

// MarshalJSON emits the defined members and the unknown ones as one
// object.
func (v VCS) MarshalJSON() ([]byte, error) {
	for key := range v.Unknown {
		if vcsMembers[key] {
			return nil, fmt.Errorf("agentsession: vcs has %s both typed and unknown", key)
		}
	}
	type plain VCS
	b, err := jsonx.MarshalNoEscape(plain(v))
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(b, nil, v.Unknown), nil
}

// UnmarshalJSON takes system, revision and dirty, spelled exactly, and
// keeps every other member in Unknown.
func (v *VCS) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	*v = VCS{}
	for key, raw := range all {
		var err error
		switch key {
		case "system":
			err = json.Unmarshal(raw, &v.System)
		case "revision":
			err = json.Unmarshal(raw, &v.Revision)
		case "dirty":
			err = json.Unmarshal(raw, &v.Dirty)
		default:
			if v.Unknown == nil {
				v.Unknown = make(map[string]json.RawMessage)
			}
			v.Unknown[key] = raw
		}
		if err != nil {
			return fmt.Errorf("vcs %s: %w", key, err)
		}
	}
	return nil
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
	// Target, on a [RelJudgedBy] link, names the entry of the judged
	// session the judgement is about: the entry the judge's outcome
	// names. It is a member of that relation alone: a target member on
	// a link of another relation, or one that is not a non-empty
	// string, which a file from before the member was defined may hold,
	// is kept as written in Unknown and Target is empty.
	Target string `json:"-" member:"target"`
}

// EntryType returns "link".
func (*LinkEntry) EntryType() string { return TypeLink }

// CustomEntry is application state that is not in context. State that
// the model should see is an [ItemEntry] holding a namespaced item.
type CustomEntry struct {
	EntryBase `json:"-"`
	NS        string          `json:"ns"`
	Data      json.RawMessage `json:"data,omitempty"`
	// CallID names the function call the record belongs to, when the
	// writer knows it: a record a tool writes while it runs. A record's
	// position cannot say which call of a batch in flight it belongs to;
	// this can. A reader may use it and must not require it. A call_id
	// member that is not a non-empty string, which a file from before
	// the member was defined may hold, is kept as written in Unknown and
	// CallID is empty.
	CallID string `json:"-" member:"call_id"`
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
	return finishDecode(e, data, all)
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
	e.tsRaw = ""
	if raw, ok := all["ts"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			e.tsRaw = s
		}
	}
	if e.LegacyID, e.Normalised, err = commonBody(all["legacy_id"], all["normalised"]); err != nil {
		return err
	}
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
	e, _, err := decodeLine(data)
	return e, err
}

// decodeLine is UnmarshalEntry that also returns the line's canonical
// bytes when decoding came by them, which is what Read hashes: the id is
// the hash of the line, never of what the entry's fields encode. It is
// nil for an extension entry, and for a line that is not I-JSON.
func decodeLine(data []byte) (e Entry, c []byte, err error) {
	// The line is split into its members once; the envelope, the
	// unknown-member check and, for item entries, the body all read
	// from the split, so a large line is not parsed again for each.
	all, err := splitMembers(data)
	if err != nil {
		return nil, nil, err
	}
	env, err := envelopeFrom(all)
	if err != nil {
		return nil, nil, err
	}
	if env.Type == "" {
		return nil, nil, errors.New("agentsession: entry has no type")
	}
	if env.ID == "" {
		return nil, nil, fmt.Errorf("agentsession: %s entry has no id", env.Type)
	}
	if raw, ok := all["ts"]; !ok || isNull(raw) {
		return nil, nil, fmt.Errorf("agentsession: entry %s has no ts", env.ID)
	}
	if _, _, err := commonBody(all["legacy_id"], all["normalised"]); err != nil {
		return nil, nil, fmt.Errorf("agentsession: entry %s: %w", env.ID, err)
	}
	var d memberDecoder = &UnknownEntry{}
	if mk, ok := coreEntries[env.Type]; ok {
		d = mk()
	}
	c, err = decodeForm(d, data, all)
	if err != nil {
		return nil, nil, fmt.Errorf("agentsession: entry %s (%s): %w", env.ID, env.Type, &memberError{err})
	}
	return d, c, nil
}

// memberError is a decode error in what a core entry type holds of its
// own members, which Scan leaves to Decode.
type memberError struct{ err error }

func (m *memberError) Error() string { return m.err.Error() }
func (m *memberError) Unwrap() error { return m.err }

// commonBody decodes the body members every entry type may carry, which
// every reader checks whatever the type: legacy_id is a string and
// normalised a list of changes.
func commonBody(legacyID, normalised json.RawMessage) (string, []Normalisation, error) {
	var (
		id  string
		out []Normalisation
	)
	if legacyID != nil && !isNull(legacyID) {
		if err := json.Unmarshal(legacyID, &id); err != nil {
			return "", nil, fmt.Errorf("legacy_id: %w", err)
		}
	}
	if normalised != nil && !isNull(normalised) {
		if err := json.Unmarshal(normalised, &out); err != nil {
			return "", nil, fmt.Errorf("normalised: %w", err)
		}
	}
	return id, out, nil
}

// finishDecode decodes a split line into e and, for a core entry,
// remembers what its typed fields could not hold.
func finishDecode(e memberDecoder, data []byte, all map[string]json.RawMessage) error {
	_, err := decodeForm(e, data, all)
	return err
}

// decodeForm is finishDecode returning the line's canonical bytes when
// keepAsRead came by them, nil for an extension entry.
func decodeForm(e memberDecoder, data []byte, all map[string]json.RawMessage) ([]byte, error) {
	if err := e.decodeMembers(data, all); err != nil {
		return nil, err
	}
	if _, unknown := e.(*UnknownEntry); unknown {
		return nil, nil // written back from its raw line
	}
	c, typed, suspect, err := keepAsRead(e, data, all)
	if err != nil || len(suspect) == 0 {
		return c, err
	}
	// A member did not reproduce, or held more than the fields encode.
	// Where that is Go's decoder matching a key in another case to a
	// member, which a conforming reader would not, decode again without
	// such keys.
	clean, folded, changed := unfold(all, typed, suspect, definedMembers(e))
	if !changed {
		return c, nil
	}
	rv := reflect.ValueOf(e).Elem()
	rv.Set(reflect.Zero(rv.Type()))
	if err := e.decodeMembers(jsonx.JoinObjects([]byte("{}"), []byte("{}"), clean), clean); err != nil {
		return nil, err
	}
	if len(folded) > 0 {
		b := e.Base()
		if b.Unknown == nil {
			b.Unknown = map[string]json.RawMessage{}
		}
		for k, v := range folded {
			b.Unknown[k] = v
		}
	}
	c, _, _, err = keepAsRead(e, data, all)
	return c, err
}

// definedMembers returns the member names an entry's type defines: its
// fields' json names, a hand-decoded field's member tag, and the
// envelope and common body members.
func definedMembers(e Entry) map[string]bool {
	t := reflect.TypeOf(e).Elem()
	if v, ok := definedCache.Load(t); ok {
		return v.(map[string]bool)
	}
	names := map[string]bool{}
	for _, k := range envelopeKeys {
		names[k] = true
	}
	for _, k := range commonBodyKeys {
		names[k] = true
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			if m := f.Tag.Get("member"); m != "" {
				names[m] = true
			}
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		names[name] = true
	}
	definedCache.Store(t, names)
	return names
}

var definedCache sync.Map

// untracked are the members keepAsRead leaves to the envelope's own
// rules: the id is recomputed, and type, parent and ts have one form
// each that the reader checks or a migration rewrites.
var untracked = map[string]bool{"id": true, "type": true, "parent": true, "ts": true}

// keepAsRead compares each member of the line with what the entry's
// typed fields encode it as, and remembers every member the line holds
// more of: one with a member nested inside it that the typed object does
// not define, such as a workspace's host, a member the typed fields omit
// because it holds a zero value, or a null the typed fields add where
// the line has nothing. The format has a reader preserve every member
// and hash the line as written, so marshalEntry grafts what was
// remembered back onto what the fields encode.
//
// An extra is anything the typed encoding does not name, so a field a
// decoder of another package should know and drops is kept too, and
// cannot be told from a member no one defined: the entry still verifies
// and writes back whole, and only the typed value is missing.
//
// Only extras are remembered member by member. A member the typed
// encoding has and the line lacks, or holds with a different value, such
// as a member a field adds as "" where the line has none, is not; the
// line is kept whole instead, and written back while the fields still
// encode what they did here, since the id is the hash of the line and
// not of what this reader made of it. suspect names every member that
// did not reproduce exactly, remembered or not.
//
// c is the line's canonical bytes, which Read hashes. A conforming
// writer's line is canonical, so the typed encoding canonicalised once
// is usually the line's own bytes, and then there is nothing to compare
// member by member. c is nil for a line that has no canonical form, which
// is not I-JSON and which Read refuses.
func keepAsRead(e Entry, data []byte, all map[string]json.RawMessage) (c []byte, typed map[string]json.RawMessage, suspect map[string]bool, err error) {
	b := e.Base()
	b.kept, b.line = nil, nil
	form, err := jsonx.MarshalNoEscape(e)
	if err != nil {
		return nil, nil, nil, err
	}
	fc, ferr := jcs.Transform(form)
	if ferr == nil && bytes.Equal(fc, data) {
		return fc, nil, nil, nil
	}
	typed, err = splitMembers(form)
	if err != nil {
		return nil, nil, nil, err
	}
	mark := func(key string) {
		if suspect == nil {
			suspect = map[string]bool{}
		}
		suspect[key] = true
	}
	for key, raw := range all {
		if untracked[key] {
			continue
		}
		seen, ok := typed[key]
		if ok && bytes.Equal(raw, seen) {
			continue
		}
		if ok {
			if c, err := jcs.Transform(seen); err == nil && bytes.Equal(raw, c) {
				continue
			}
		}
		mark(key)
		rv, err := parseValue(raw)
		if err != nil {
			continue
		}
		if !ok {
			if isZeroValue(rv) {
				b.keep(key, raw, nil)
			}
			continue
		}
		sv, err := parseValue(seen)
		if err != nil {
			continue
		}
		if same, extras := extrasOnly(rv, sv); same && extras {
			b.keep(key, raw, seen)
		}
	}
	for key, seen := range typed {
		if _, ok := all[key]; ok || untracked[key] {
			continue
		}
		mark(key)
		if string(seen) == "null" {
			b.keep(key, nil, seen)
		}
	}
	c, err = jcs.Transform(data)
	if err != nil {
		return nil, typed, suspect, nil
	}
	restored := fc
	if len(b.kept) > 0 || ferr != nil {
		if form, err = restoreKept(form, b.kept); err == nil {
			restored, err = jcs.Transform(form)
		}
		if err != nil {
			restored = nil
		}
	}
	if _, hasID := all["id"]; hasID && b.ID != "" && !bytes.Equal(restored, c) {
		// The fields do not write the line back: keep it, and have
		// marshalEntry note what the fields encode now. A line with no
		// id is not one a reader verifies, and is left to the fields.
		b.line = &keptLine{c: c, id: b.ID}
		if _, err := jsonx.MarshalNoEscape(e); err != nil {
			return nil, nil, nil, err
		}
	}
	return c, typed, suspect, nil
}

// foldKey is key under simple case folding, which is how Go's decoder
// matches a key to a struct field.
func foldKey(key string) string { return strings.ToLower(strings.ToUpper(key)) }

// lowerASCII reports whether key is lower-case ASCII, as every member
// name this package and its payload define is, so a key that is not
// cannot name a defined member exactly.
func lowerASCII(key string) bool {
	for i := 0; i < len(key); i++ {
		if c := key[i]; c >= 0x80 || ('A' <= c && c <= 'Z') {
			return false
		}
	}
	return true
}

// unfold returns the line's members with every key removed that Go's
// decoder may have matched, in another case, to a member the type
// defines. Such a key is a member the format does not define, which a
// conforming reader ignores, and the decoder must not read it as the
// member it resembles. At the top level the defined names are the
// type's; below, the members suspect names are walked beside their
// typed encoding. Keys removed at the top level are returned apart, to
// keep as unknown members; changed reports whether anything was removed.
func unfold(all, typed map[string]json.RawMessage, suspect, defined map[string]bool) (clean, folded map[string]json.RawMessage, changed bool) {
	definedFolds := make(map[string]bool, len(defined))
	for k := range defined {
		definedFolds[foldKey(k)] = true
	}
	clean = make(map[string]json.RawMessage, len(all))
	for key, raw := range all {
		if !defined[key] && definedFolds[foldKey(key)] {
			if folded == nil {
				folded = map[string]json.RawMessage{}
			}
			folded[key] = raw
			changed = true
			continue
		}
		clean[key] = raw
		seen, ok := typed[key]
		if !suspect[key] || !ok {
			continue
		}
		rv, err1 := parseValue(raw)
		sv, err2 := parseValue(seen)
		if err1 != nil || err2 != nil {
			continue
		}
		out, ch := unfoldValue(rv, sv)
		if !ch {
			continue
		}
		if data, err := jsonx.MarshalNoEscape(out); err == nil {
			clean[key] = data
			changed = true
		}
	}
	return clean, folded, changed
}

// unfoldValue removes from raw, in each object the typed encoding seen
// also has, every key seen does not hold exactly that is not lower-case
// ASCII and folds to a key of that object in seen or in raw: the key a
// decoder that folds case may have read into a field. A key seen holds
// exactly, as a map's keys are, is never removed.
func unfoldValue(raw, seen any) (any, bool) {
	switch r := raw.(type) {
	case map[string]any:
		s, ok := seen.(map[string]any)
		if !ok {
			return raw, false
		}
		folds := make(map[string]int, len(r)+len(s))
		for k := range r {
			folds[foldKey(k)]++
		}
		for k := range s {
			if _, inRaw := r[k]; !inRaw {
				folds[foldKey(k)]++
			}
		}
		changed := false
		out := make(map[string]any, len(r))
		for k, v := range r {
			sv, exact := s[k]
			if !exact && !lowerASCII(k) && folds[foldKey(k)] > 1 {
				changed = true
				continue
			}
			if exact {
				var ch bool
				v, ch = unfoldValue(v, sv)
				changed = changed || ch
			}
			out[k] = v
		}
		return out, changed
	case []any:
		s, ok := seen.([]any)
		if !ok || len(s) != len(r) {
			return raw, false
		}
		changed := false
		out := make([]any, len(r))
		for i := range r {
			var ch bool
			out[i], ch = unfoldValue(r[i], s[i])
			changed = changed || ch
		}
		return out, changed
	}
	return raw, false
}

// remapKeptParents applies a migration's rewrite of parents to the kept
// copy, so each reference's extras stay on the reference they described:
// ref returns the new entry ID for a reference it rewrites.
func (b *EntryBase) remapKeptParents(ref func(session, entry string) (string, bool)) {
	k, ok := b.kept["parents"]
	if !ok || k.seen == nil {
		return
	}
	remap := func(data json.RawMessage) json.RawMessage {
		v, err := parseValue(data)
		if err != nil {
			return data
		}
		list, ok := v.([]any)
		if !ok {
			return data
		}
		for _, el := range list {
			obj, ok := el.(map[string]any)
			if !ok {
				continue
			}
			entry, _ := obj["entry"].(string)
			session, _ := obj["session"].(string)
			if id, ok := ref(session, entry); ok {
				obj["entry"] = id
			}
		}
		out, err := jsonx.MarshalNoEscape(list)
		if err != nil {
			return data
		}
		return out
	}
	b.kept["parents"] = keptMember{raw: remap(k.raw), seen: remap(k.seen)}
}

func (b *EntryBase) keep(key string, raw, seen json.RawMessage) {
	if b.kept == nil {
		b.kept = map[string]keptMember{}
	}
	b.kept[key] = keptMember{raw: raw, seen: seen}
}

// parseValue decodes a JSON value keeping numbers as written.
func parseValue(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// isZeroValue reports whether v is a value an omitempty field leaves
// out: null, false, zero, an empty string, array or object.
func isZeroValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case bool:
		return !x
	case string:
		return x == ""
	case json.Number:
		f, err := x.Float64()
		return err == nil && f == 0
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// extrasOnly reports whether raw holds everything seen does with the
// same values, keys matched exactly, differing at most by object members
// seen lacks at any depth; extras reports whether it has any.
func extrasOnly(raw, seen any) (same, extras bool) {
	switch s := seen.(type) {
	case map[string]any:
		r, ok := raw.(map[string]any)
		if !ok {
			return false, false
		}
		for k, sv := range s {
			rv, ok := r[k]
			if !ok {
				if sv == nil {
					// A null the typed encoding adds where the line has
					// nothing: the same value, spelled by omission.
					extras = true
					continue
				}
				return false, false
			}
			same, ex := extrasOnly(rv, sv)
			if !same {
				return false, false
			}
			extras = extras || ex
		}
		for k := range r {
			if _, ok := s[k]; !ok {
				extras = true
				break
			}
		}
		return true, extras
	case []any:
		r, ok := raw.([]any)
		if !ok || len(r) != len(s) {
			return false, false
		}
		for i := range s {
			same, ex := extrasOnly(r[i], s[i])
			if !same {
				return false, false
			}
			extras = extras || ex
		}
		return true, extras
	case json.Number:
		r, ok := raw.(json.Number)
		if !ok {
			return false, false
		}
		if r == s {
			return true, false
		}
		ri, errRI := r.Int64()
		si, errSI := s.Int64()
		if errRI == nil && errSI == nil {
			return ri == si, false
		}
		rf, errR := r.Float64()
		sf, errS := s.Float64()
		return errR == nil && errS == nil && rf == sf, false
	case string:
		r, ok := raw.(string)
		return ok && r == s, false
	case bool:
		r, ok := raw.(bool)
		return ok && r == s, false
	case nil:
		return raw == nil, false
	}
	return false, false
}

// overlay grafts onto cur, what the typed fields encode now, the members
// raw held that seen, what they encoded at read, did not: at every depth
// where the three are objects, and onto each array element that still
// encodes as one did at read. What the caller changed is cur's; what the
// reader could not hold is raw's.
func overlay(cur, raw, seen any) any {
	switch c := cur.(type) {
	case map[string]any:
		r, rok := raw.(map[string]any)
		s, sok := seen.(map[string]any)
		if !rok || !sok {
			return cur
		}
		out := make(map[string]any, len(c)+len(r))
		for k, v := range c {
			out[k] = v
		}
		for k, rv := range r {
			sv, inSeen := s[k]
			cv, inCur := c[k]
			switch {
			case !inSeen && !inCur:
				out[k] = rv
			case inSeen && inCur:
				out[k] = overlay(cv, rv, sv)
			}
		}
		for k, sv := range s {
			if _, inRaw := r[k]; !inRaw && sv == nil && out[k] == nil {
				delete(out, k)
			}
		}
		return out
	case []any:
		r, rok := raw.([]any)
		s, sok := seen.([]any)
		if !rok || !sok || len(r) != len(s) {
			return cur
		}
		unchanged := func(cv, sv any) bool {
			same, extras := extrasOnly(cv, sv)
			return same && !extras
		}
		used := make([]bool, len(s))
		done := make([]bool, len(c))
		out := make([]any, len(c))
		copy(out, c)
		// An element still where it was and as it was is matched first,
		// so a changed element cannot claim another's extras; then each
		// element left over takes the extras of the one unused element
		// it matches, when exactly one does. Equal elements that differ
		// only in their extras cannot be told apart once one is gone.
		for i, cv := range c {
			if i < len(s) && unchanged(cv, s[i]) {
				used[i], done[i] = true, true
				out[i] = r[i]
			}
		}
		for i, cv := range c {
			if done[i] {
				continue
			}
			match := -1
			for j, sv := range s {
				if !used[j] && unchanged(cv, sv) {
					if match >= 0 {
						match = -2
						break
					}
					match = j
				}
			}
			if match >= 0 {
				used[match] = true
				out[i] = r[match]
			}
		}
		return out
	}
	return cur
}

// restoreKept grafts each kept member back onto the encoded entry out.
// A member the caller also set in Unknown under a typed member's name
// is left alone, so the duplicate reaches the check that refuses it.
func restoreKept(out []byte, kept map[string]keptMember) ([]byte, error) {
	members, err := splitMembers(out)
	if err != nil {
		return nil, err
	}
	for key, k := range kept {
		cur, ok := members[key]
		if k.raw == nil {
			// A null the fields add that the line did not have.
			if ok && bytes.Equal(cur, k.seen) {
				delete(members, key)
			}
			continue
		}
		if k.seen == nil {
			if !ok {
				members[key] = k.raw
			}
			continue
		}
		if !ok {
			continue // the caller removed it
		}
		if bytes.Equal(cur, k.seen) {
			members[key] = k.raw
			continue
		}
		cv, err1 := parseValue(cur)
		rv, err2 := parseValue(k.raw)
		sv, err3 := parseValue(k.seen)
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		merged, err := jsonx.MarshalNoEscape(overlay(cv, rv, sv))
		if err != nil {
			return nil, err
		}
		members[key] = merged
	}
	return jsonx.JoinObjects([]byte("{}"), []byte("{}"), members), nil
}

// coreEntries makes an empty entry of each core type, by type name. It is
// the one list of core types: UnmarshalEntry decodes through it and the
// member round-trip test walks it, so a type added here is covered.
var coreEntries = map[string]func() memberDecoder{
	TypeItem:          func() memberDecoder { return &ItemEntry{} },
	TypeResponse:      func() memberDecoder { return &ResponseEntry{} },
	TypeConfig:        func() memberDecoder { return &ConfigEntry{} },
	TypeCompaction:    func() memberDecoder { return &CompactionEntry{} },
	TypeBranchSummary: func() memberDecoder { return &BranchSummaryEntry{} },
	TypeRun:           func() memberDecoder { return &RunEntry{} },
	TypeDispatch:      func() memberDecoder { return &DispatchEntry{} },
	TypeDecision:      func() memberDecoder { return &DecisionEntry{} },
	TypeQueued:        func() memberDecoder { return &QueuedEntry{} },
	TypeLabel:         func() memberDecoder { return &LabelEntry{} },
	TypeInfo:          func() memberDecoder { return &InfoEntry{} },
	TypeEnv:           func() memberDecoder { return &EnvEntry{} },
	TypeOutcome:       func() memberDecoder { return &OutcomeEntry{} },
	TypeLink:          func() memberDecoder { return &LinkEntry{} },
	TypeCustom:        func() memberDecoder { return &CustomEntry{} },
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
	if all, ok := jsonx.Members(data); ok {
		return all, nil
	}
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
	if _, ok := all["content"]; ok {
		return envelope{}, fmt.Errorf("%w: content", ErrReservedMember)
	}
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
			// A second 60, which time.Time cannot hold: the format has a
			// writer and a migration write it as 59 with the same
			// fraction. The raw spelling is still checked by a 0.5
			// reader, so this only lets an earlier file through.
			var str string
			if json.Unmarshal(raw, &str) != nil {
				return env, fmt.Errorf("ts: %w", err)
			}
			fixed := leapSecond.ReplaceAllString(str, "${1}59${2}")
			if fixed == str {
				return env, fmt.Errorf("ts: %w", err)
			}
			if env.TS, err = time.Parse(time.RFC3339Nano, fixed); err != nil {
				return env, fmt.Errorf("ts: %w", err)
			}
		}
	}
	return env, nil
}

// marshalEntry joins the envelope, the type-specific body and the
// unknown members into one object.
func marshalEntry(typ string, base *EntryBase, body any) ([]byte, error) {
	env := envelope{Type: typ, ID: base.ID, Parents: base.Parents, TS: base.Timestamp.UTC()}
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
	extra := base.extraMembers()
	if l := base.line; l != nil {
		fp, err := lineFingerprint(env, b, extra)
		if err != nil {
			return nil, err
		}
		switch {
		case !l.set:
			l.fp, l.set = fp, true // at read: what the fields encode
		case fp == l.fp:
			return l.emit(base.ID)
		}
	}
	out := jsonx.JoinObjects(head, b, extra)
	if len(base.kept) > 0 && !collides(head, b, extra) {
		return restoreKept(out, base.kept)
	}
	return out, nil
}

// lineFingerprint hashes what an entry's fields encode, its id left
// out, for keptLine.
func lineFingerprint(env envelope, body []byte, extra map[string]json.RawMessage) ([sha256.Size]byte, error) {
	env.ID = ""
	head, err := jsonx.MarshalNoEscape(env)
	if err != nil {
		return [sha256.Size]byte{}, err
	}
	return sha256.Sum256(jsonx.JoinObjects(head, body, extra)), nil
}

// collides reports whether an extra member repeats a member of the
// envelope or the body, a line the I-JSON check refuses and restoreKept
// would otherwise hide by rebuilding the object.
func collides(head, body []byte, extra map[string]json.RawMessage) bool {
	if len(extra) == 0 {
		return false
	}
	for _, part := range [][]byte{head, body} {
		m, err := splitMembers(part)
		if err != nil {
			return true
		}
		for k := range m {
			if _, dup := extra[k]; dup {
				return true
			}
		}
	}
	return false
}

// extraMembers returns the unknown members with the common body members
// the base carries added, for marshalling.
func (b *EntryBase) extraMembers() map[string]json.RawMessage {
	if b.LegacyID == "" && len(b.Normalised) == 0 {
		return b.Unknown
	}
	out := make(map[string]json.RawMessage, len(b.Unknown)+2)
	for k, v := range b.Unknown {
		out[k] = v
	}
	if b.LegacyID != "" {
		raw, _ := json.Marshal(b.LegacyID)
		out["legacy_id"] = raw
	}
	if len(b.Normalised) > 0 {
		raw, _ := json.Marshal(b.Normalised)
		out["normalised"] = raw
	}
	return out
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
	base.tsRaw = ""
	if raw, ok := all["ts"]; ok {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			base.tsRaw = s
		}
	}
	base.kept, base.line = nil, nil
	if base.LegacyID, base.Normalised, err = commonBody(all["legacy_id"], all["normalised"]); err != nil {
		return err
	}
	base.Unknown = jsonx.ExtraKeys(all, known, append(append([]string{}, envelopeKeys...), commonBodyKeys...)...)
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
	return finishDecode(e, data, all)
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
	if e.Attempts != 0 {
		if _, dup := e.Unknown["attempts"]; dup {
			return nil, errors.New("agentsession: response entry has attempts both typed and unknown")
		}
	}
	return marshalEntry(TypeResponse, &e.EntryBase, struct {
		*plain
		Attempts int `json:"attempts,omitempty"`
	}{(*plain)(e), e.Attempts})
}

// UnmarshalJSON decodes the entry.
func (e *ResponseEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *ResponseEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain ResponseEntry
	e.Attempts = 0
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), responseKeys); err != nil {
		return err
	}
	promoteIf(&e.EntryBase, "attempts", &e.Attempts, func(n int) bool { return n > 0 })
	return nil
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
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
	return finishDecode(e, data, all)
}

func (e *OutcomeEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain OutcomeEntry
	return unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), outcomeKeys)
}

// MarshalJSON emits the entry as one JSON object.
func (e *LinkEntry) MarshalJSON() ([]byte, error) {
	type plain LinkEntry
	if e.Target != "" {
		if _, dup := e.Unknown["target"]; dup {
			return nil, errors.New("agentsession: link entry has target both typed and unknown")
		}
	}
	return marshalEntry(TypeLink, &e.EntryBase, struct {
		*plain
		Target string `json:"target,omitempty"`
	}{(*plain)(e), e.Target})
}

// UnmarshalJSON decodes the entry.
func (e *LinkEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *LinkEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain LinkEntry
	e.Target = ""
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), linkKeys); err != nil {
		return err
	}
	if e.Rel == RelJudgedBy {
		promote(&e.EntryBase, "target", &e.Target)
	}
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *CustomEntry) MarshalJSON() ([]byte, error) {
	if e.CallID != "" {
		if _, dup := e.Unknown["call_id"]; dup {
			return nil, errors.New("agentsession: custom entry has call_id both typed and unknown")
		}
	}
	return marshalEntry(TypeCustom, &e.EntryBase, struct {
		NS     string          `json:"ns"`
		Data   json.RawMessage `json:"data,omitempty"`
		CallID string          `json:"call_id,omitempty"`
	}{e.NS, e.Data, e.CallID})
}

// UnmarshalJSON decodes the entry.
func (e *CustomEntry) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	return finishDecode(e, data, all)
}

func (e *CustomEntry) decodeMembers(data []byte, all map[string]json.RawMessage) error {
	type plain CustomEntry
	e.CallID = ""
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), customKeys); err != nil {
		return err
	}
	promote(&e.EntryBase, "call_id", &e.CallID)
	return nil
}

// promote moves the unknown member key into *dst when it decodes into
// dst's type as a non-zero value holding at least what that value
// encodes, and leaves it unknown otherwise. It is
// for members a minor version added: a file from before the member was
// defined may spell it any way at all. What the typed field cannot hold
// of a member it does take, such as a member nested in it that the type
// does not define, is kept by keepAsRead and written back as read, so
// the entry's hash still verifies and a rewrite loses nothing. A field
// filled this way is tagged json:"-" with a member tag naming its wire
// member, which the member round-trip test reads.
func promote[T any](base *EntryBase, key string, dst *T) {
	promoteIf(base, key, dst, nil)
}

// promoteIf is promote for a member the typed field takes only when
// valid says the decoded value is one it can hold.
func promoteIf[T any](base *EntryBase, key string, dst *T, valid func(T) bool) {
	raw, ok := base.Unknown[key]
	if !ok {
		return
	}
	var v T
	if !holdsExactly(raw, &v) || reflect.ValueOf(v).IsZero() || valid != nil && !valid(v) {
		return
	}
	*dst = v
	delete(base.Unknown, key)
	if len(base.Unknown) == 0 {
		base.Unknown = nil
	}
}

// holdsExactly decodes raw into v and reports whether v holds what raw
// does: the decoder matches keys in any case, so a value is taken only
// when raw holds what it encodes, exactly keyed, and more.
func holdsExactly[T any](raw json.RawMessage, v *T) bool {
	if json.Unmarshal(raw, v) != nil {
		return false
	}
	back, err := jsonx.MarshalNoEscape(*v)
	if err != nil {
		return false
	}
	rv, err1 := parseValue(raw)
	sv, err2 := parseValue(back)
	same, _ := extrasOnly(rv, sv)
	return err1 == nil && err2 == nil && same
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
