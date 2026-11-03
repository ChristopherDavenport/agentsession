package agentsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
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
	// RelPeer names a callee whose work outlives the call that reached
	// it and which keeps its own record; see [NewPeerLink]. A callee the
	// caller started and ends with the call is a [RelSubsession],
	// wherever it runs: the relation follows the callee's lifetime.
	RelPeer = "peer"
)

// CancelSent is the one value of a link's Cancel member: the caller
// told the callee it no longer wanted the work, which says nothing of
// whether the work stopped; see [NewCancelSentLink].
const CancelSent = "sent"

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
	// redacted is the entry's redacted member, a body member any entry
	// may carry from format 0.13: the positions, in the original's file
	// order, of the entries a redactor made this entry from. isRedacted
	// says the entry carries it, so that [] (an entry the redaction
	// added) is told from no member. A member in another form, or read
	// under a header naming an earlier minor, stays in Unknown. See
	// [RedactedOf] and [SetRedacted].
	redacted   []Span
	isRedacted bool
	// redactedRaw is the member as the line spelled it, for an entry
	// that was read, so it writes back and is kept as written; nil for
	// one set in memory.
	redactedRaw json.RawMessage
	// gated is set on an entry read under a header naming a minor before
	// 0.13 that holds a redacted member, which the gate left unknown: a
	// file holding it may not be written at 0.13 or later.
	gated bool
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
	// extra holds the members of the reference the format does not
	// define, as one JSON object, so a rewrite keeps them and the entry
	// keeps the hash its line has: a member inside an object the format
	// defines is preserved and hashed as written (RFC 0001, Entry
	// envelope). A string keeps the reference comparable.
	extra string
}

// MarshalJSON writes the reference with the members it was read with
// that the format does not define.
func (r EntryRef) MarshalJSON() ([]byte, error) {
	type plain struct {
		Session string `json:"session,omitempty"`
		Entry   string `json:"entry"`
	}
	head, err := jsonx.MarshalNoEscape(plain{Session: r.Session, Entry: r.Entry})
	if err != nil || r.extra == "" {
		return head, err
	}
	return jsonx.JoinObjects(head, []byte(r.extra), nil), nil
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
	delete(all, "entry")
	delete(all, "session")
	if len(all) > 0 {
		r.extra = string(jsonx.JoinObjects([]byte("{}"), []byte("{}"), all))
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
	// Failed marks, in format 0.14, an entry holding a function call
	// output whose tool reported failure, or which the harness gave up:
	// cut off, timed out, its outcome unknown. The output describes it.
	// It is a record member: the item is sent as it is, and the call is
	// completed by its output all the same. A reader maps it to its
	// protocol's failure state; [Call.Failed] reads it. A failed member
	// on any other item, or one that is not true, is kept as written in
	// Unknown and Failed is false.
	Failed bool `json:"-" member:"failed"`
	// FromLink names, in format 0.14, a peer link entry before this item
	// on its path, by ID: the item carries that callee's later report,
	// such as the result a peer whose work outlived its call delivers
	// some turns after the call's acknowledgement. It is provenance: it
	// does not make the call answered, and it reaches no request.
	// [Session.VerifyRecords] reports one that names no peer link before
	// the item. A from_link member that is not a non-empty string is
	// kept as written in Unknown and FromLink is empty.
	FromLink string `json:"-" member:"from_link"`
	// CopiedFrom says, in format 0.15, that the item was not produced in
	// this session but taken from the conversation of another: a run
	// prompted with the conversation of the session that spawned it, or
	// a fork answering a call with the output its origin recorded. It is
	// provenance and reaches no request. A function call whose entry
	// carries it is a copied call ([Call.Copied]): the session named made
	// it, and this one holds it as context alone. A copied_from member
	// in another form, one whose session is not a non-empty string or
	// whose entry is present and not one, is kept as written in Unknown
	// and CopiedFrom is nil.
	CopiedFrom *CopiedFrom `json:"-" member:"copied_from"`
	// Trust says, in format 0.15, what the harness knows about where the
	// item's content came from; see [Trust]. A copied item carries the
	// trust of the entry it was copied from. Nil says nothing. A trust
	// member in another form is kept as written in Unknown and Trust is
	// nil.
	Trust *Trust `json:"-" member:"trust"`
	// InstructionsPart says, in format 0.17, that the harness composed
	// the item, a message, from an instruction layer and sent it as a
	// message rather than in the instructions: an instruction file
	// wrapped in a reminder, or a memory block. The item carries the
	// layer's text as composed. It is a record member and reaches no
	// request, and the item is no input of its run ([PathInputs]). An
	// instructions_part on any other item, or in another form (see
	// [LayerRef]), is kept as written in Unknown and InstructionsPart
	// is nil.
	InstructionsPart *LayerRef `json:"-" member:"instructions_part"`
}

// Instruction layers: the source values RFC 0001 defines, since 0.17,
// for an instructions part ([InstructionPart]), an omitted part
// ([OmittedPart]) and an item's instructions_part ([LayerRef]). A reader
// may rely on them in a file of any minor; every other source is in the
// harness's own terms. A redaction keeps them as written.
const (
	// LayerProduct is the harness's own prompt for the product it is.
	LayerProduct = "product"
	// LayerAgentsMD is an instruction file, AGENTS.md or a harness's
	// equivalent: one part per file, its id naming the file.
	LayerAgentsMD = "agentsmd"
	// LayerAgentSkill is the catalogue of skills offered.
	LayerAgentSkill = "agentskill"
	// LayerAgentMemory is memory kept between sessions.
	LayerAgentMemory = "agentmemory"
)

// KnownLayer reports whether source is one of the instruction layers
// the format defines.
func KnownLayer(source string) bool {
	switch source {
	case LayerProduct, LayerAgentsMD, LayerAgentSkill, LayerAgentMemory:
		return true
	}
	return false
}

// LayerRef names the instruction layer an item was composed from (RFC
// 0001, item, instructions_part, since 0.17), with the meanings ID and
// Source have on an [InstructionPart]. Source is required. ID is
// required when the layer has more than one part, as [LayerAgentsMD]
// does, one part per file; when present it is not empty.
type LayerRef struct {
	ID     string `json:"id,omitempty"`
	Source string `json:"source"`
}

// valid reports whether the reference is in the form the format defines.
func (r *LayerRef) valid() bool {
	return r.Source != "" && (r.ID != "" || r.Source != LayerAgentsMD)
}

// layerRefForm reports whether raw holds an instructions_part in the
// form the format defines: an object whose source is a non-empty string
// and whose id, present, is one too, as it must be for an agentsmd
// layer.
func layerRefForm(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	var ref LayerRef
	if json.Unmarshal(m["source"], &ref.Source) != nil {
		return false
	}
	if rawID, ok := m["id"]; ok {
		if json.Unmarshal(rawID, &ref.ID) != nil || ref.ID == "" {
			return false
		}
	}
	return ref.valid()
}

// Trust labels (RFC 0001, item, since 0.15).
const (
	// TrustUntrusted says the content came from a source the harness
	// does not trust, such as a web page or an inbox.
	TrustUntrusted = "untrusted"
	// TrustPrivate says the content holds data the session's owner would
	// not have leave it.
	TrustPrivate = "private"
)

// Trust is what a harness knows about where content came from: the
// labels of what it reads. {"reads": []} says the harness judged the
// content and found neither label; an absent trust says nothing. A label
// the format does not define is kept as written, and a reader that
// decides anything by trust reads it as [TrustUntrusted]. It is a record
// member and reaches no request.
type Trust struct {
	// Reads are the labels, without repeats.
	Reads []string `json:"reads"`
}

// MarshalJSON writes reads as a list, empty when there are none.
func (t Trust) MarshalJSON() ([]byte, error) {
	reads := t.Reads
	if reads == nil {
		reads = []string{}
	}
	seen := make(map[string]bool, len(reads))
	for _, r := range reads {
		if seen[r] {
			return nil, fmt.Errorf("agentsession: trust repeats the label %q", r)
		}
		seen[r] = true
	}
	return json.Marshal(struct {
		Reads []string `json:"reads"`
	}{reads})
}

// Untrusted reports whether the labels hold one a reader that decides by
// trust reads as untrusted: untrusted itself, or a label the format does
// not define.
func (t *Trust) Untrusted() bool {
	if t == nil {
		return false
	}
	for _, r := range t.Reads {
		if r != TrustPrivate {
			return true
		}
	}
	return false
}

// trustForm reports whether raw holds a trust in the form the format
// defines: an object whose reads is a list of strings without repeats.
func trustForm(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	var reads []string
	if json.Unmarshal(m["reads"], &reads) != nil || reads == nil {
		return false
	}
	seen := make(map[string]bool, len(reads))
	for _, r := range reads {
		if seen[r] {
			return false
		}
		seen[r] = true
	}
	return true
}

// promoteTrust takes a trust member in the form the format defines, in a
// file of any minor, and keeps anything else as written.
func promoteTrust(base *EntryBase, dst **Trust) {
	*dst = nil
	if raw, ok := base.Unknown["trust"]; ok && trustForm(raw) {
		promote(base, "trust", dst)
		if *dst != nil && (*dst).Reads == nil {
			(*dst).Reads = []string{}
		}
	}
}

// UnionTrust returns the union of the labels of every trust given, sorted,
// or nil when none is given: a path's trust, or a fold's, which a
// writer never narrows.
func UnionTrust(ts ...*Trust) *Trust {
	var out *Trust
	seen := map[string]bool{}
	for _, t := range ts {
		if t == nil {
			continue
		}
		if out == nil {
			out = &Trust{Reads: []string{}}
		}
		for _, r := range t.Reads {
			if !seen[r] {
				seen[r] = true
				out.Reads = append(out.Reads, r)
			}
		}
	}
	if out != nil {
		slices.Sort(out.Reads)
	}
	return out
}

// PathTrust returns the trust of a root-first path (RFC 0001, item,
// since 0.15): the union of the trust of every item entry on it, those
// an omit leaves out or sends as a placeholder included, since the model
// may have read them on an earlier request, of every compaction and
// branch summary, whose trust is that of the path they were made from,
// and, since 0.17, of every instructions part a config entry or a
// compaction's checkpoint writes with trust.
// It is nil when nothing on the path carries trust.
func PathTrust(path []Entry) *Trust {
	var ts []*Trust
	for _, e := range path {
		switch v := e.(type) {
		case *ItemEntry:
			ts = append(ts, v.Trust)
		case *ConfigEntry:
			for _, p := range v.InstructionsParts {
				ts = append(ts, p.Trust)
			}
		case *CompactionEntry:
			ts = append(ts, v.Trust)
			for _, p := range v.Config.InstructionsParts {
				ts = append(ts, p.Trust)
			}
		case *BranchSummaryEntry:
			ts = append(ts, v.Trust)
		}
	}
	return UnionTrust(ts...)
}

// CopiedFrom names where a copied item was taken from (RFC 0001, item,
// since 0.15).
type CopiedFrom struct {
	// Session is the session the item was taken from, never the session
	// that holds the entry.
	Session string `json:"session"`
	// Entry is the entry of Session whose item this entry's item is,
	// member for member, named only for an item copied unchanged. It is
	// empty for an item changed in the copy, and for one made for it,
	// such as the output a seed gives a call the copied conversation had
	// not answered.
	Entry string `json:"entry,omitempty"`
}

// copiedFromForm reports whether raw holds a copied_from in the form
// the format defines: an object whose session is a non-empty string and
// whose entry, when present, is one too.
func copiedFromForm(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil || m == nil {
		return false
	}
	var session string
	if json.Unmarshal(m["session"], &session) != nil || session == "" {
		return false
	}
	if rawEntry, ok := m["entry"]; ok {
		var entry string
		if json.Unmarshal(rawEntry, &entry) != nil || entry == "" {
			return false
		}
	}
	return true
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
	// Omit records the items a request leaves out of the context, in
	// format 0.11: a rule that leaves out a reasoning item written under
	// another model, and a list of entries whose items contribute
	// nothing. It is in force as a setting is: nil leaves the object in
	// force as it was, an empty, non-nil one, written as {}, clears it,
	// and any other sets the reasoning rule it names and adds its items
	// to those in force. See [Omit] and [Settings.OmitDelta]. An omit
	// member that does not decode as the object, which a file from before
	// the member was defined may hold, is kept as written in Unknown and
	// Omit is nil.
	Omit *Omit `json:"-" member:"omit"`
}

// Omit is the omit setting: what a request leaves out of the context
// though it is on the path, as format 0.11 records it, and, from 0.14,
// what it sends a placeholder for. Settings carry the object in force,
// which a config entry's Omit extends: its Reasoning replaces the rule
// in force when it names one, its Items are added to the set in force,
// its Outputs add their items to those in force and set the text, its
// Images replace the rule in force, and an object naming none of them
// clears all of them.
type Omit struct {
	// Reasoning is a rule over the reasoning items on the path. It is
	// closed: [OmitOtherModels] is the one value defined, and a value
	// this package does not define has no effect, leaving the rule in
	// force as it was.
	Reasoning string `json:"reasoning,omitempty"`
	// Items are entry IDs. An item entry on the path whose ID is listed
	// contributes nothing, whatever it holds; an ID that names no item
	// entry on the path names nothing, and a writer writes no empty one.
	// The set in force is the union of every list since the last replace
	// or compaction's checkpoint.
	Items []string `json:"items,omitempty"`
	// Outputs, from 0.14, sends the function call output of each item
	// entry it lists with a placeholder text in place of its output.
	Outputs *OmitOutputs `json:"outputs,omitempty"`
	// Images, from 0.14, sends input_image parts with a placeholder text
	// in their place: all but the last few, or all before an entry.
	Images *OmitImages `json:"images,omitempty"`
	// undefined is set on an object read with an outputs or images member
	// in a form the format does not define, which its entry keeps as
	// written: the member is carried and changes nothing, as an
	// undefined reasoning value changes nothing, so the object is not
	// the empty one that clears what is in force.
	undefined bool
}

// OmitOutputs is the outputs member of omit: each item entry on the
// path whose ID Items lists, and that holds a function call output, is
// sent with its output the string Text, its call ID and every other
// member as they are. The items in force are the union of every list
// since the last replace or compaction's checkpoint, as for
// [Omit.Items], and the text is the one the latest member wrote: the
// default group, as format 0.14 defines the member.
//
// From 0.16 Groups sends other outputs with other texts, each group
// named by its Text: a group's items in force are the union of every
// group of that text since the last replace or checkpoint, and its text
// never changes. With Groups set, an empty Text and no Items write no
// default group, and the member is written as groups alone. An entry is
// in one group at most, the default among them: a writer refuses an
// outputs that would put one in two ([ErrOmitOverlap]), and a reader
// takes one as a form the format does not define, which changes nothing.
type OmitOutputs struct {
	Text   string            `json:"text"`
	Items  []string          `json:"items,omitempty"`
	Groups []OmitOutputGroup `json:"groups,omitempty"`

	// textSet records a text present, which writes the default group
	// whatever it holds, the empty string beside groups included.
	textSet bool
}

// UnmarshalJSON takes text, items and groups, recording whether text
// is present, and each groups element only as an object holding a
// string text and a list items and nothing else: anything else is an
// error, so the [Omit] holding it keeps the member as written and it
// changes nothing.
func (o *OmitOutputs) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	*o = OmitOutputs{}
	if raw, ok := all["text"]; ok {
		if json.Unmarshal(raw, &o.Text) != nil || isNull(raw) {
			return errors.New("omit outputs: text is a string")
		}
		o.textSet = true
	}
	if raw, ok := all["items"]; ok {
		if err := json.Unmarshal(raw, &o.Items); err != nil {
			return err
		}
	}
	raw, ok := all["groups"]
	if !ok {
		return nil
	}
	var elems []json.RawMessage
	if json.Unmarshal(raw, &elems) != nil || isNull(raw) {
		return errors.New("omit outputs: groups is a list")
	}
	for _, el := range elems {
		m, err := splitMembers(el)
		text, hasText := m["text"]
		items, hasItems := m["items"]
		if err != nil || len(m) != 2 || !hasText || !hasItems || isNull(text) || isNull(items) {
			return errors.New("omit outputs: a group is text and items alone")
		}
		var g OmitOutputGroup
		if json.Unmarshal(text, &g.Text) != nil || json.Unmarshal(items, &g.Items) != nil {
			return errors.New("omit outputs: a group's text is a string and its items a list")
		}
		o.Groups = append(o.Groups, g)
	}
	return nil
}

// OmitOutputGroup is one element of [OmitOutputs.Groups]: the entries
// whose outputs are sent with Text.
type OmitOutputGroup struct {
	Text  string   `json:"text"`
	Items []string `json:"items"`
}

// MarshalJSON writes the group with its items a list, empty or not.
func (g OmitOutputGroup) MarshalJSON() ([]byte, error) {
	items := g.Items
	if items == nil {
		items = []string{}
	}
	return jsonx.MarshalNoEscape(struct {
		Text  string   `json:"text"`
		Items []string `json:"items"`
	}{g.Text, items})
}

// hasDefault reports whether the member writes the default group: a
// text or items, or no groups, as every member before 0.16 is.
func (o OmitOutputs) hasDefault() bool {
	return o.textSet || o.Text != "" || len(o.Items) > 0 || len(o.Groups) == 0
}

// MarshalJSON writes the default group's text and items when the member
// has one, and the groups when it has any.
func (o OmitOutputs) MarshalJSON() ([]byte, error) {
	if o.hasDefault() {
		return jsonx.MarshalNoEscape(struct {
			Text   string            `json:"text"`
			Items  []string          `json:"items,omitempty"`
			Groups []OmitOutputGroup `json:"groups,omitempty"`
		}{o.Text, o.Items, o.Groups})
	}
	return jsonx.MarshalNoEscape(struct {
		Groups []OmitOutputGroup `json:"groups"`
	}{o.Groups})
}

// TextFor returns the text the member sends the output of the entry id
// with, and false when it lists no such entry.
func (o OmitOutputs) TextFor(id string) (string, bool) {
	for _, g := range o.Groups {
		if slices.Contains(g.Items, id) {
			return g.Text, true
		}
	}
	if o.hasDefault() && slices.Contains(o.Items, id) {
		return o.Text, true
	}
	return "", false
}

// owners maps each ID the member lists to the group listing it, the
// default group's key apart from every named group's, and reports false
// when the member lists one ID in two of its groups or holds two groups
// of one text.
func (o *OmitOutputs) owners() (map[string]string, bool) {
	own := map[string]string{}
	if o == nil {
		return own, true
	}
	add := func(id, key string) bool {
		if id == "" {
			return true
		}
		if k, ok := own[id]; ok && k != key {
			return false
		}
		own[id] = key
		return true
	}
	if o.hasDefault() {
		for _, id := range o.Items {
			add(id, "d")
		}
	}
	texts := make(map[string]bool, len(o.Groups))
	for _, g := range o.Groups {
		if texts[g.Text] {
			return nil, false
		}
		texts[g.Text] = true
		for _, id := range g.Items {
			if !add(id, "g"+g.Text) {
				return nil, false
			}
		}
	}
	return own, true
}

// outputsWellFormed reports whether the member lists no entry in two of
// its groups and holds no two groups of one text.
func outputsWellFormed(o *OmitOutputs) bool {
	_, ok := o.owners()
	return ok
}

// outputsOverlap reports whether d, written as a delta over held in
// force, would put an entry in two groups: two of its own, or one of its
// own and one of held of another text.
func outputsOverlap(held, d *OmitOutputs) bool {
	dOwn, ok := d.owners()
	if !ok {
		return true
	}
	hOwn, _ := held.owners()
	for id, k := range dOwn {
		if hk, ok := hOwn[id]; ok && hk != k {
			return true
		}
	}
	return false
}

// clone returns a copy that shares no slice with o.
func (o OmitOutputs) clone() *OmitOutputs {
	out := &OmitOutputs{Text: o.Text, Items: slices.Clone(o.Items), textSet: o.textSet}
	for _, g := range o.Groups {
		out.Groups = append(out.Groups, OmitOutputGroup{Text: g.Text, Items: slices.Clone(g.Items)})
	}
	return out
}

// mergedOutputs returns the outputs in force after a delta d over held:
// d's default group adds its items to held's and sets the text, and each
// of d's groups adds its items to held's group of that text.
func mergedOutputs(held, d *OmitOutputs) *OmitOutputs {
	out := &OmitOutputs{}
	if held != nil {
		out = held.clone()
	}
	if d.hasDefault() {
		out.Text, out.Items, out.textSet = d.Text, union(out.Items, d.Items), true
	}
	for _, g := range d.Groups {
		i := slices.IndexFunc(out.Groups, func(h OmitOutputGroup) bool { return h.Text == g.Text })
		if i < 0 {
			out.Groups = append(out.Groups, OmitOutputGroup{Text: g.Text, Items: union(nil, g.Items)})
		} else {
			out.Groups[i].Items = union(out.Groups[i].Items, g.Items)
		}
	}
	return out
}

// OmitImages is the images member of omit, in one of two forms: with
// Before empty, every input_image part the request's item entries
// contribute but the last Keep is sent as an input_text part holding
// Placeholder; with Before set, every input_image part of an item entry
// on the path before the entry Before names is. A later images member
// replaces the one in force.
type OmitImages struct {
	Keep        int    `json:"keep"`
	Before      string `json:"before"`
	Placeholder string `json:"placeholder"`
}

// MarshalJSON writes the form the member is in: before, or keep.
func (o OmitImages) MarshalJSON() ([]byte, error) {
	if o.Before != "" {
		return jsonx.MarshalNoEscape(struct {
			Before      string `json:"before"`
			Placeholder string `json:"placeholder"`
		}{o.Before, o.Placeholder})
	}
	return jsonx.MarshalNoEscape(struct {
		Keep        int    `json:"keep"`
		Placeholder string `json:"placeholder"`
	}{o.Keep, o.Placeholder})
}

// UnmarshalJSON takes either form, spelled exactly: a non-empty string
// before or a non-negative integer keep, not both, beside a string
// placeholder. Anything else is an error, so an [Omit] holding it keeps
// the member as written.
func (o *OmitImages) UnmarshalJSON(data []byte) error {
	all, err := splitMembers(data)
	if err != nil {
		return err
	}
	*o = OmitImages{}
	if json.Unmarshal(all["placeholder"], &o.Placeholder) != nil {
		return errors.New("omit images: placeholder is a string")
	}
	_, hasKeep := all["keep"]
	_, hasBefore := all["before"]
	switch {
	case hasKeep && !hasBefore:
		var n json.Number
		if json.Unmarshal(all["keep"], &n) != nil {
			return errors.New("omit images: keep is a number")
		}
		k, err := strconv.Atoi(n.String())
		if err != nil || k < 0 {
			return errors.New("omit images: keep is a non-negative integer")
		}
		o.Keep = k
	case hasBefore && !hasKeep:
		if json.Unmarshal(all["before"], &o.Before) != nil || o.Before == "" {
			return errors.New("omit images: before is a non-empty string")
		}
	default:
		return errors.New("omit images: one of keep and before")
	}
	return nil
}

// UnmarshalJSON takes reasoning and items as the struct names them, and
// outputs and images only in the form the format defines; a member in
// another form, which a file from before 0.14 was free to write, is left
// for the entry to keep as written.
func (o *Omit) UnmarshalJSON(data []byte) error {
	var plain struct {
		Reasoning string          `json:"reasoning,omitempty"`
		Items     []string        `json:"items,omitempty"`
		Outputs   json.RawMessage `json:"outputs"`
		Images    json.RawMessage `json:"images"`
	}
	if err := json.Unmarshal(data, &plain); err != nil {
		return err
	}
	*o = Omit{Reasoning: plain.Reasoning, Items: plain.Items}
	if len(plain.Outputs) > 0 && !isNull(plain.Outputs) {
		var outputs OmitOutputs
		if holdsExactly(plain.Outputs, &outputs) && outputsWellFormed(&outputs) {
			o.Outputs = &outputs
		} else {
			o.undefined = true
		}
	}
	if len(plain.Images) > 0 && !isNull(plain.Images) {
		var images OmitImages
		if holdsExactly(plain.Images, &images) {
			o.Images = &images
		} else {
			o.undefined = true
		}
	}
	return nil
}

// OmitOtherModels is the one value of [Omit.Reasoning] the format
// defines: a request leaves out each item entry holding a reasoning
// item that carries a response and was written while a model other than
// the request's was in force. It is also the reason a [Context] reports
// for an item left out by that rule.
const OmitOtherModels = "other_models"

// OmitItems is the reason a [Context] reports for an item left out
// because [Omit.Items] lists its entry.
const OmitItems = "items"

// OmitOutputsReason and OmitImagesReason are the reasons a [Context]
// reports for an item sent with a placeholder: its output, because
// [Omit.Outputs] lists its entry, or image parts, which [Omit.Images]
// reaches.
const (
	OmitOutputsReason = "outputs"
	OmitImagesReason  = "images"
)

// IsZero reports whether the object names no rule, no entry and no
// placeholder, which is the object a config entry writes to clear the
// one in force, and the settings of a path that left nothing out. An
// empty id names no entry, so a list of them names none. An object read
// with an outputs or images member in a form the format does not define
// carries it, and is not zero.
func (o Omit) IsZero() bool {
	if o.Reasoning != "" || o.Outputs != nil || o.Images != nil || o.undefined {
		return false
	}
	for _, id := range o.Items {
		if id != "" {
			return false
		}
	}
	return true
}

// clone returns a copy of the object that shares no slice or pointer
// with it.
func (o Omit) clone() Omit {
	out := Omit{Reasoning: o.Reasoning, Items: slices.Clone(o.Items)}
	if o.Outputs != nil {
		out.Outputs = o.Outputs.clone()
	}
	if o.Images != nil {
		images := *o.Images
		out.Images = &images
	}
	return out
}

// union returns the IDs of a and then b, each once, empty ones left out,
// in the order they were first written.
func union(a, b []string) []string {
	var out []string
	seen := make(map[string]bool, len(a)+len(b))
	for _, list := range [][]string{a, b} {
		for _, id := range list {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

// merged returns the object in force after a delta d: d's rule replaces
// this one's when it is one the format defines, a rule it does not
// define leaves this one's in force, d's items are added to this one's
// in the order they were first written, d's outputs add their items to
// those in force and set the text, its groups each add theirs to the
// group of their text, unless they would put an entry in two groups,
// when they change nothing, d's images replace those in force,
// and a delta that names none of them clears all of them. The receiver
// and d are not modified.
func (o Omit) merged(d Omit) Omit {
	if d.IsZero() {
		return Omit{}
	}
	out := o.clone()
	if d.Reasoning == OmitOtherModels {
		out.Reasoning = d.Reasoning
	}
	out.Items = union(o.Items, d.Items)
	if d.Outputs != nil && !outputsOverlap(o.Outputs, d.Outputs) {
		out.Outputs = mergedOutputs(o.Outputs, d.Outputs)
	}
	if d.Images != nil {
		images := *d.Images
		out.Images = &images
	}
	return out
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
	// Trust says, in format 0.17, what the harness knows about where
	// the part's text came from, as on an item ([Trust]), such as a
	// memory part that holds what a web page said. A part named by its
	// hash alone keeps the trust it had, as it keeps its source. It is a
	// record member, reaches no request, and the path's trust takes it
	// in ([PathTrust]).
	Trust *Trust `json:"trust,omitempty"`
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
		Keep  json.RawMessage `json:"keep"`
		Trust json.RawMessage `json:"trust"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*p = InstructionPart(v.plain)
	p.Keep = 0
	if n, err := strconv.ParseInt(string(v.Keep), 10, 32); err == nil && n > 0 {
		p.Keep = int(n)
	}
	// trust is taken in the form an item's is (0.17); another form is a
	// member the part does not define, which the line keeps as written.
	p.Trust = nil
	if len(v.Trust) > 0 && trustForm(v.Trust) {
		var t Trust
		if json.Unmarshal(v.Trust, &t) == nil {
			if t.Reads == nil {
				t.Reads = []string{}
			}
			p.Trust = &t
		}
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
	// Trust is, in format 0.15, the trust of the path up to the
	// compaction ([PathTrust]), so a reader holding only the path from
	// the fold on, or the context alone, knows what the summary was made
	// from. It may be wider than what a reader recomputes from the
	// entries it holds, since a redaction may have left some out; a
	// reader takes the union of both, and a writer never narrows it.
	Trust *Trust `json:"-" member:"trust"`
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
	// Trust is, in format 0.15, the trust of the path From names, since
	// the summary was made from that branch; see [CompactionEntry.Trust].
	Trust *Trust `json:"-" member:"trust"`
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
// label on the same target. An empty Target, null on the wire, names
// the universal root, which only a leaf label marks: the head at the
// empty context.
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
	// names. On a cancel notice (Cancel set) it names the link, in this
	// session, of the callee the cancel was sent to. It is a member of
	// those links alone: a target member on another link, or one that
	// is not a non-empty string, which a file from before the member
	// was defined may hold, is kept as written in Unknown and Target is
	// empty.
	Target string `json:"-" member:"target"`
	// URL, Task and Context, on a [RelSubsession] or [RelPeer] link,
	// name a callee in another system, reached over a protocol such as
	// A2A: the endpoint the call was sent to as the caller resolved it,
	// never a name the callee claims; the callee's ID for the work the
	// call started, absent when the callee answered without one (an A2A
	// message rather than a task); and its ID for the conversation that
	// work belongs to. Task and Context come with a URL. Session is then
	// the callee's own session ID when it publishes one, and otherwise
	// empty. Like Target, each is a member of those relations alone,
	// kept as written in Unknown on any other, when it is not a
	// non-empty string, or (task, context) when no url is beside it.
	URL     string `json:"-" member:"url"`
	Task    string `json:"-" member:"task"`
	Context string `json:"-" member:"context"`
	// Cancel, on a later [RelSubsession] or [RelPeer] link of the same
	// call, is [CancelSent]: the caller told the callee the link Target
	// names that it no longer wanted the work. It records the request,
	// not that the work stopped. A link carrying it names no new
	// callee; see [NewCancelSentLink]. Any other value, or one with no
	// target beside it, is kept as written in Unknown.
	Cancel string `json:"-" member:"cancel"`
}

// callee reports whether the relation is one a callee's members (url,
// task, context, cancel) are defined for.
func (e *LinkEntry) callee() bool { return e.Rel == RelSubsession || e.Rel == RelPeer }

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

// Calls returns the number of model calls the entry records, which RFC
// 0001 has a custom entry record when its data is an object with a
// top-level usage member in the payload profile's usage shape: calls the
// path paid for that produced no response. They are the data's attempts
// member beside the usage, a positive integer with a response's
// meaning, or one when it is absent or in another form. An entry whose
// data holds no such usage records none, and Calls returns zero.
func (e *CustomEntry) Calls() int {
	var paid struct {
		Usage    *openresponses.Usage `json:"usage"`
		Attempts json.RawMessage      `json:"attempts"`
	}
	if json.Unmarshal(e.Data, &paid) != nil || paid.Usage == nil {
		return 0
	}
	if n, ok := positiveInt(paid.Attempts); ok {
		return n
	}
	return 1
}

// positiveInt reads raw as a positive integer written as digits.
func positiveInt(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 || raw[0] < '1' || raw[0] > '9' {
		return 0, false
	}
	n, err := strconv.Atoi(string(raw))
	return n, err == nil && n > 0
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
	e.takeRedacted(all)
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
	TypeResult:        func() memberDecoder { return &ResultEntry{} },
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
// the base carries added, for marshalling: legacy_id, normalised and a
// typed redacted.
func (b *EntryBase) extraMembers() map[string]json.RawMessage {
	if b.LegacyID == "" && len(b.Normalised) == 0 && !b.isRedacted {
		return b.Unknown
	}
	out := make(map[string]json.RawMessage, len(b.Unknown)+3)
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
	if b.isRedacted {
		out["redacted"] = b.redactedJSON()
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
	base.takeRedacted(all)
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
	if !e.Failed && e.FromLink == "" && e.CopiedFrom == nil && e.Trust == nil && e.InstructionsPart == nil {
		return marshalEntry(TypeItem, &e.EntryBase, (*plain)(e))
	}
	if _, ok := e.Item.(*openresponses.FunctionCallOutput); e.Failed && !ok {
		return nil, errors.New("agentsession: failed is defined on a function call output's entry alone")
	}
	if p := e.InstructionsPart; p != nil {
		if _, ok := e.Item.(*openresponses.Message); !ok {
			return nil, errors.New("agentsession: instructions_part is defined on a message's entry alone")
		}
		if !p.valid() {
			return nil, fmt.Errorf("agentsession: instructions_part %+v names no source, or an agentsmd layer with no id", *p)
		}
	}
	if e.CopiedFrom != nil && e.CopiedFrom.Session == "" {
		return nil, errors.New("agentsession: copied_from names no session")
	}
	for _, m := range []struct {
		key string
		set bool
	}{{"failed", e.Failed}, {"from_link", e.FromLink != ""}, {"copied_from", e.CopiedFrom != nil}, {"trust", e.Trust != nil}, {"instructions_part", e.InstructionsPart != nil}} {
		if _, dup := e.Unknown[m.key]; dup && m.set {
			return nil, fmt.Errorf("agentsession: item entry has %s both typed and unknown", m.key)
		}
	}
	return marshalEntry(TypeItem, &e.EntryBase, struct {
		*plain
		Failed           bool        `json:"failed,omitempty"`
		FromLink         string      `json:"from_link,omitempty"`
		CopiedFrom       *CopiedFrom `json:"copied_from,omitempty"`
		Trust            *Trust      `json:"trust,omitempty"`
		InstructionsPart *LayerRef   `json:"instructions_part,omitempty"`
	}{(*plain)(e), e.Failed, e.FromLink, e.CopiedFrom, e.Trust, e.InstructionsPart})
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
	// failed is taken where 0.14 defines it, true on an output's entry,
	// in a file of any minor; anything else is kept as written.
	e.Failed, e.FromLink, e.CopiedFrom = false, "", nil
	if _, ok := e.Item.(*openresponses.FunctionCallOutput); ok {
		promote(&e.EntryBase, "failed", &e.Failed)
	}
	promote(&e.EntryBase, "from_link", &e.FromLink)
	// copied_from is taken in the form 0.15 defines, in a file of any
	// minor; anything else is kept as written.
	if raw, ok := e.Unknown["copied_from"]; ok && copiedFromForm(raw) {
		promote(&e.EntryBase, "copied_from", &e.CopiedFrom)
	}
	promoteTrust(&e.EntryBase, &e.Trust)
	// instructions_part is taken in the form 0.17 defines, on a
	// message's entry, in a file of any minor; anything else is kept as
	// written.
	e.InstructionsPart = nil
	if _, ok := e.Item.(*openresponses.Message); ok {
		if raw, ok := e.Unknown["instructions_part"]; ok && layerRefForm(raw) {
			promote(&e.EntryBase, "instructions_part", &e.InstructionsPart)
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
	if e.Omit != nil {
		if _, dup := e.Unknown["omit"]; dup {
			return nil, errors.New("agentsession: config entry has omit both typed and unknown")
		}
	}
	return marshalEntry(TypeConfig, &e.EntryBase, struct {
		*plain
		Omit *Omit `json:"omit,omitempty"`
	}{(*plain)(e), e.Omit})
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
	e.Omit = nil
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), configKeys); err != nil {
		return err
	}
	promote(&e.EntryBase, "omit", &e.Omit)
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *CompactionEntry) MarshalJSON() ([]byte, error) {
	if e.Summary == nil {
		return nil, errors.New("agentsession: compaction entry has no summary")
	}
	type plain CompactionEntry
	if e.Trust == nil {
		return marshalEntry(TypeCompaction, &e.EntryBase, (*plain)(e))
	}
	if _, dup := e.Unknown["trust"]; dup {
		return nil, errors.New("agentsession: compaction entry has trust both typed and unknown")
	}
	return marshalEntry(TypeCompaction, &e.EntryBase, struct {
		*plain
		Trust *Trust `json:"trust"`
	}{(*plain)(e), e.Trust})
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
	promoteTrust(&e.EntryBase, &e.Trust)
	return nil
}

// MarshalJSON emits the entry as one JSON object.
func (e *BranchSummaryEntry) MarshalJSON() ([]byte, error) {
	if e.Summary == nil {
		return nil, errors.New("agentsession: branch_summary entry has no summary")
	}
	type plain BranchSummaryEntry
	if e.Trust == nil {
		return marshalEntry(TypeBranchSummary, &e.EntryBase, (*plain)(e))
	}
	if _, dup := e.Unknown["trust"]; dup {
		return nil, errors.New("agentsession: branch_summary entry has trust both typed and unknown")
	}
	return marshalEntry(TypeBranchSummary, &e.EntryBase, struct {
		*plain
		Trust *Trust `json:"trust"`
	}{(*plain)(e), e.Trust})
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
	promoteTrust(&e.EntryBase, &e.Trust)
	return nil
}

// MarshalJSON emits the entry as one JSON object. An empty Target is
// written null: a leaf label on the universal root.
func (e *LabelEntry) MarshalJSON() ([]byte, error) {
	if e.Target == "" {
		body := struct {
			Target *string `json:"target"`
			Label  *string `json:"label"`
		}{nil, e.Label}
		return marshalEntry(TypeLabel, &e.EntryBase, &body)
	}
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
	for _, m := range [][2]string{{"target", e.Target}, {"url", e.URL}, {"task", e.Task}, {"context", e.Context}, {"cancel", e.Cancel}} {
		if m[1] == "" {
			continue
		}
		if _, dup := e.Unknown[m[0]]; dup {
			return nil, fmt.Errorf("agentsession: link entry has %s both typed and unknown", m[0])
		}
	}
	return marshalEntry(TypeLink, &e.EntryBase, struct {
		*plain
		Target  string `json:"target,omitempty"`
		URL     string `json:"url,omitempty"`
		Task    string `json:"task,omitempty"`
		Context string `json:"context,omitempty"`
		Cancel  string `json:"cancel,omitempty"`
	}{(*plain)(e), e.Target, e.URL, e.Task, e.Context, e.Cancel})
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
	e.Target, e.URL, e.Task, e.Context, e.Cancel = "", "", "", "", ""
	if err := unmarshalEntry(data, all, &e.EntryBase, (*plain)(e), linkKeys); err != nil {
		return err
	}
	if e.Rel == RelJudgedBy {
		promote(&e.EntryBase, "target", &e.Target)
	}
	if e.callee() {
		// Each member is taken only where the format defines it, so a
		// link read back always holds what a writer may write: task
		// and context beside a url, and cancel beside the target that
		// names the link it is about. What is not taken is kept as
		// written.
		promote(&e.EntryBase, "url", &e.URL)
		if e.URL != "" {
			promote(&e.EntryBase, "task", &e.Task)
			promote(&e.EntryBase, "context", &e.Context)
		}
		var target string
		if raw, ok := e.Unknown["target"]; ok && holdsExactly(raw, &target) && target != "" {
			promoteIf(&e.EntryBase, "cancel", &e.Cancel, func(v string) bool { return v == CancelSent })
		}
		if e.Cancel != "" {
			promote(&e.EntryBase, "target", &e.Target)
		}
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
