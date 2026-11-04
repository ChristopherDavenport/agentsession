package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
	"github.com/ChristopherDavenport/openresponses"
)

// A writer that makes a session out of another, a redactor above all,
// takes each entry's canonical JSON from [MarshalEntry], changes it, and
// decodes the result with [UnmarshalEntry] before it commits it. Which
// members of that JSON are body text, which name an entry or a session,
// and which hold a hash is the format's knowledge, and [RemapRefs] and
// [RewriteStrings] keep it here, in one table, so the two cannot drift
// apart and a writer does not derive it again.

// memberRole is what a member of an entry holds that is not body text.
type memberRole uint8

const (
	// roleFixed is the envelope, legacy_id and redacted, trust labels,
	// a source the format defines and every member drawn from a closed
	// set: the whole value, at any depth, is neither rewritten nor
	// re-pointed.
	roleFixed memberRole = iota + 1
	// roleEntry is a string naming an entry by its id.
	roleEntry
	// roleSession is a string naming a session by its id.
	roleSession
	// roleHash is a string holding a hash of something else.
	roleHash
	// roleCallID is a string naming a function call by its call_id.
	roleCallID
	// roleDataKeys is an object whose member names are data, not names
	// a writer chose: a rewrite of the body reaches them.
	roleDataKeys
)

// special is one member of an entry's JSON that is not body text.
type special struct {
	// path names the member from the top of the entry: a member name at
	// each step, "[]" for every element of an array and "*" for every
	// member of an object.
	path []string
	role memberRole
	// session, on an entry reference, is the member beside it that names
	// the session the entry is in; empty for one into this file.
	session string
	// when, if set, says whether the entry holds the member as the
	// format defines it. A decoder keeps a member in another form, or on
	// a relation that does not define it, as written, and a member so
	// kept names nothing: it is body.
	when func(Entry) bool
	// holds, if set, says the same of the member's place: whether the
	// object it is a member of, an element of a list, is one the format
	// defines the member on.
	holds func(holder map[string]any) bool
	// set marks an entry reference naming one element of a set, which
	// loses the element when the entry it names was left out, rather
	// than naming what stands in its place (RFC 0001, Leaving entries
	// out). up is how many steps of the path above the reference the
	// element is: 1 for a parents element, whose entry is a member of it.
	set bool
	up  int
	// bound marks an entry reference that bounds a range from below,
	// first_kept and images' before, whose left-out entry is replaced by
	// the first entry written after it, not by one above it: [Remap.Bound].
	bound bool
	// value, if set, says whether the member's value v is one the format
	// gives the member's role: a member drawn from a closed set is fixed
	// while it holds a value of the set, and is text otherwise.
	value func(v any) bool
	// vocab marks a member drawn from a closed set, or a type, which the
	// table fixes as the format's vocabulary (RFC 0001, Every redacted
	// entry, since 0.20).
	vocab bool
}

// applies reports whether sp names the member that holder holds, whose
// value is v.
func (sp special) applies(holder map[string]any, v any) bool {
	return (sp.holds == nil || sp.holds(holder)) && (sp.value == nil || sp.value(v))
}

// keepOf reports whether an omitted part is a keep, where alone of is
// defined: a positive integer keep, as the decoder takes one, and no id.
func keepOf(el map[string]any) bool {
	if id, _ := el["id"].(string); id != "" {
		return false
	}
	n, ok := el["keep"].(json.Number)
	if !ok {
		return false
	}
	k, err := strconv.ParseInt(string(n), 10, 32)
	return err == nil && k > 0
}

// knownSource reports whether a part, or an omitted part, holds a source
// the format defines ([KnownLayer]), which a rewrite keeps as written.
func knownSource(el map[string]any) bool {
	s, _ := el["source"].(string)
	return KnownLayer(s)
}

// memberPath is a path, for the table.
func memberPath(steps ...string) []string { return steps }

// oneOf reports whether a value is a string of the closed set values:
// a member holding any other value is text (RFC 0001, Every redacted
// entry, since 0.20).
func oneOf[T ~string](values ...T) func(v any) bool {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[string(v)] = true
	}
	return func(v any) bool {
		s, ok := v.(string)
		return ok && set[s]
	}
}

// closed is a member at path drawn from the closed set values, which a
// rewrite keeps while it holds one of them. A list of such values, such
// as include, is named by a path ending "[]", and each element is kept
// or rewritten by its own value.
func closed[T ~string](path []string, values ...T) special {
	return special{path: path, role: roleFixed, value: oneOf(values...), vocab: true}
}

// discriminator is the type member at path, which says what the object
// is and is kept whatever its value, an extension's included.
func discriminator(path []string) special {
	return special{path: path, role: roleFixed, vocab: true}
}

// under is path with steps appended, a new slice.
func under(path []string, steps ...string) []string {
	return append(slices.Clone(path), steps...)
}

// itemMembers are the members drawn from a closed set of an item held at
// at, as the payload profile defines them: the item's type, role, status
// and phase, the type and detail of each part of its content, summary or
// output, and the type of each of a part's annotations.
func itemMembers(at ...string) []special {
	out := []special{
		discriminator(under(at, "type")),
		closed(under(at, "role"), openresponses.RoleUser, openresponses.RoleAssistant, openresponses.RoleSystem, openresponses.RoleDeveloper),
		closed(under(at, "status"), openresponses.StatusInProgress, openresponses.StatusCompleted, openresponses.StatusIncomplete),
		closed(under(at, "phase"), openresponses.PhaseCommentary, openresponses.PhaseFinalAnswer),
	}
	for _, parts := range []string{"content", "summary", "output"} {
		out = append(out,
			discriminator(under(at, parts, "[]", "type")),
			closed(under(at, parts, "[]", "detail"), openresponses.ImageDetailLow, openresponses.ImageDetailHigh, openresponses.ImageDetailAuto),
			discriminator(under(at, parts, "[]", "annotations", "[]", "type")),
		)
	}
	return out
}

// settingsMembers are the members drawn from a closed set of the
// settings at at, a config entry's or a compaction's checkpoint, whose
// tools are in the member tools: reasoning's effort and summary, text's
// verbosity and its format's type, each tool's type, and the request
// members of extra the payload profile closes, tool_choice, truncation,
// service_tier and include.
func settingsMembers(tools string, at ...string) []special {
	modes := []openresponses.ToolChoiceMode{openresponses.ToolChoiceNone, openresponses.ToolChoiceAuto, openresponses.ToolChoiceRequired}
	return []special{
		closed(under(at, "reasoning", "effort"), openresponses.ReasoningEffortNone, openresponses.ReasoningEffortMinimal, openresponses.ReasoningEffortLow, openresponses.ReasoningEffortMedium, openresponses.ReasoningEffortHigh, openresponses.ReasoningEffortXHigh),
		closed(under(at, "reasoning", "summary"), openresponses.ReasoningSummaryConcise, openresponses.ReasoningSummaryDetailed, openresponses.ReasoningSummaryAuto),
		closed(under(at, "text", "verbosity"), openresponses.VerbosityLow, openresponses.VerbosityMedium, openresponses.VerbosityHigh),
		discriminator(under(at, "text", "format", "type")),
		discriminator(under(at, tools, "[]", "type")),
		closed(under(at, "extra", "tool_choice"), modes...),
		discriminator(under(at, "extra", "tool_choice", "type")),
		closed(under(at, "extra", "tool_choice", "mode"), modes...),
		discriminator(under(at, "extra", "tool_choice", "tools", "[]", "type")),
		closed(under(at, "extra", "truncation"), openresponses.TruncationAuto, openresponses.TruncationDisabled),
		closed(under(at, "extra", "service_tier"), openresponses.ServiceTierAuto, openresponses.ServiceTierDefault, openresponses.ServiceTierFlex, openresponses.ServiceTierPriority),
		closed(under(at, "extra", "include", "[]"), openresponses.IncludeReasoningEncryptedContent, openresponses.IncludeOutputTextLogprobs),
	}
}

// joined is the lists of specials as one.
func joined(lists ...[]special) []special { return slices.Concat(lists...) }

// envelopeMembers are the members any entry may carry that are not
// body text: the envelope; legacy_id, the name an entry had before its
// file was migrated, which is kept as that name; and redacted, the
// positions in the original of the entries a redacted entry was made
// from, which name entries of another file by place, not by id, so
// neither a rewrite nor a remap touches them, in whatever form the
// member is held. parents is fixed whole, extras included; its entry
// and session members are references.
var envelopeMembers = []special{
	{path: memberPath("type"), role: roleFixed},
	{path: memberPath("id"), role: roleFixed},
	{path: memberPath("ts"), role: roleFixed},
	{path: memberPath("legacy_id"), role: roleFixed},
	{path: memberPath("redacted"), role: roleFixed},
	{path: memberPath("parent"), role: roleEntry},
	{path: memberPath("parents"), role: roleFixed},
	{path: memberPath("parents", "[]", "entry"), role: roleEntry, session: "session", set: true, up: 1},
	{path: memberPath("parents", "[]", "session"), role: roleSession},
}

// bodyMembers are, by core type, the members of the body that are not
// body text: every member RFC 0001 has name an entry or a session, every
// one holding a hash, and every call_id the format defines. Each is
// taken only where the format defines it, and a member of one of those
// names in another form or place is body text. A judged_by link's
// target and a cancel notice's name an entry of this session, as a
// dispatch's does; on any other link the member is body. An omitted
// part's of names an entry beside a positive keep and no id, and on no
// other element. A compaction's checkpoint holds the settings in force,
// whose omit items name entries as a config entry's do; it writes its
// omitted parts whole, so carries no of. An env entry's files map a path
// to the hash of the file: the hash is not body text, and the path is
// data in a member name, which a rewrite of the body reaches, as is the
// tool name keying its tools. Every other object keyed by data, one
// whose members the format does not define, is found from the type that
// decodes the entry, by the rewrite (see shape).
//
// A call_id pairs a call with its output, its dispatches, decisions and
// links, and a subsession's ID is derived from it, so a rewrite of the
// body leaves it where the format defines it: on an item (and so on a
// queued entry's item and a compaction's pinned items), on a dispatch, a
// decision, a link and a custom entry, and in a run end's pending list;
// and so does a subsession link's spawn_id, since 0.19, which pairs a
// child the host started with its header's spawned_by.
// A call_id inside data the format does not define, a decision's args,
// a custom entry's data, an outcome's details or a member it does not
// define, is the writer's text and body.
//
// A trust member's labels, on an item, a result, a compaction and a
// branch summary, are the format's vocabulary and not text, so a rewrite
// leaves them as they are (RFC 0001, Every redacted entry). So is a
// source the format defines ([KnownLayer]) on an instructions part, an
// omitted part and an item's instructions_part, since 0.17, and each
// element of an instructions_part list, since 0.18; any other
// source, and every id, is text. A part's trust labels, since 0.17, are
// the format's vocabulary too.
//
// So is, since 0.20, every member whose value is drawn from a closed set
// the format or the payload profile defines, while it holds a value of
// that set (RFC 0001, Every redacted entry): a run's phase, source and
// reason, a queued entry's mode, a decision's verdict and by, a
// workspace's kind, a link's rel and cancel, an outcome's kind; an
// item's role, status and phase, wherever an entry holds an item, and a
// part's detail; a response's status, incomplete reason and error type;
// and the settings' omit reasoning, which the context algorithm reads,
// their reasoning and text settings and the request members of extra the
// payload closes. A type in the payload, an item's, a
// part's, an annotation's, a text format's, a tool's or a tool choice's,
// is kept whatever its value, since it says what the object is. Any
// other value of one of those members is text.
var bodyMembers = map[string][]special{
	TypeItem: joined(itemMembers("item"), []special{
		{path: memberPath("queued_from"), role: roleEntry, set: true},
		{path: memberPath("from_link"), role: roleEntry, set: true, when: func(e Entry) bool { return e.(*ItemEntry).FromLink != "" }},
		{path: memberPath("copied_from", "entry"), role: roleEntry, session: "session", set: true, when: func(e Entry) bool { return e.(*ItemEntry).CopiedFrom != nil }},
		{path: memberPath("copied_from", "session"), role: roleSession, when: func(e Entry) bool { return e.(*ItemEntry).CopiedFrom != nil }},
		{path: memberPath("trust", "reads", "[]"), role: roleFixed, when: func(e Entry) bool { return e.(*ItemEntry).Trust != nil }},
		{path: memberPath("instructions_part", "source"), role: roleFixed, when: func(e Entry) bool {
			p := e.(*ItemEntry).InstructionsPart
			return p != nil && KnownLayer(p.Source)
		}},
		{path: memberPath("instructions_part", "[]", "source"), role: roleFixed, holds: knownSource, when: func(e Entry) bool {
			return e.(*ItemEntry).InstructionsLayers != nil
		}},
		{path: memberPath("item", "call_id"), role: roleCallID},
	}),
	TypeQueued: joined(itemMembers("item"), []special{
		{path: memberPath("item", "call_id"), role: roleCallID},
		closed(memberPath("mode"), ModeSteer, ModeFollowUp),
	}),
	TypeResponse: {
		{path: memberPath("request_hash"), role: roleHash},
		closed(memberPath("status"), openresponses.ResponseStatusQueued, openresponses.ResponseStatusInProgress, openresponses.ResponseStatusCompleted, openresponses.ResponseStatusIncomplete, openresponses.ResponseStatusFailed),
		closed(memberPath("incomplete", "reason"), openresponses.IncompleteReasonMaxOutputTokens, openresponses.IncompleteReasonContentFilter),
		closed(memberPath("error", "type"), openresponses.ErrorTypeInvalidRequest, openresponses.ErrorTypeNotFound, openresponses.ErrorTypeTooManyRequests, openresponses.ErrorTypeModelError, openresponses.ErrorTypeServerError),
	},
	TypeConfig: joined(settingsMembers("tools_added"), []special{
		{path: memberPath("omit", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { return e.(*ConfigEntry).Omit != nil }},
		{path: memberPath("omit", "outputs", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { o := e.(*ConfigEntry).Omit; return o != nil && o.Outputs != nil }},
		{path: memberPath("omit", "outputs", "groups", "[]", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { o := e.(*ConfigEntry).Omit; return o != nil && o.Outputs != nil }},
		{path: memberPath("omit", "images", "before"), role: roleEntry, bound: true, when: func(e Entry) bool {
			o := e.(*ConfigEntry).Omit
			return o != nil && o.Images != nil && o.Images.Before != ""
		}},
		{path: memberPath("instructions_omitted", "[]", "of"), role: roleEntry, holds: keepOf},
		{path: memberPath("instructions_parts", "[]", "hash"), role: roleHash},
		{path: memberPath("instructions_parts", "[]", "source"), role: roleFixed, holds: knownSource},
		{path: memberPath("instructions_parts", "[]", "trust", "reads", "[]"), role: roleFixed},
		{path: memberPath("instructions_omitted", "[]", "source"), role: roleFixed, holds: knownSource},
		closed(memberPath("omit", "reasoning"), OmitOtherModels),
	}),
	TypeCompaction: joined(itemMembers("summary"), itemMembers("pinned", "[]"), settingsMembers("tools", "config"), []special{
		{path: memberPath("first_kept"), role: roleEntry, bound: true},
		{path: memberPath("config", "omit", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { return !e.(*CompactionEntry).Config.Omit.IsZero() }},
		{path: memberPath("config", "omit", "outputs", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { return e.(*CompactionEntry).Config.Omit.Outputs != nil }},
		{path: memberPath("config", "omit", "outputs", "groups", "[]", "items", "[]"), role: roleEntry, set: true, when: func(e Entry) bool { return e.(*CompactionEntry).Config.Omit.Outputs != nil }},
		{path: memberPath("config", "omit", "images", "before"), role: roleEntry, bound: true, when: func(e Entry) bool { i := e.(*CompactionEntry).Config.Omit.Images; return i != nil && i.Before != "" }},
		{path: memberPath("config", "instructions_parts", "[]", "hash"), role: roleHash},
		{path: memberPath("config", "instructions_parts", "[]", "source"), role: roleFixed, holds: knownSource},
		{path: memberPath("config", "instructions_parts", "[]", "trust", "reads", "[]"), role: roleFixed},
		{path: memberPath("config", "instructions_omitted", "[]", "source"), role: roleFixed, holds: knownSource},
		closed(memberPath("config", "omit", "reasoning"), OmitOtherModels),
		{path: memberPath("pinned", "[]", "call_id"), role: roleCallID},
		{path: memberPath("trust", "reads", "[]"), role: roleFixed, when: func(e Entry) bool { return e.(*CompactionEntry).Trust != nil }},
	}),
	TypeBranchSummary: joined(itemMembers("summary"), []special{
		{path: memberPath("from"), role: roleEntry},
		{path: memberPath("trust", "reads", "[]"), role: roleFixed, when: func(e Entry) bool { return e.(*BranchSummaryEntry).Trust != nil }},
	}),
	TypeRun: {
		{path: memberPath("pending", "[]"), role: roleCallID},
		closed(memberPath("phase"), RunStart, RunEnd),
		closed(memberPath("source"), SourceInput, SourceResume),
		closed(memberPath("reason"), ReasonDone, ReasonStopped, ReasonInterrupted, ReasonInputRequired, ReasonAborted, ReasonError),
	},
	TypeDispatch: {{path: memberPath("target"), role: roleEntry}, {path: memberPath("call_id"), role: roleCallID}},
	TypeDecision: {
		{path: memberPath("target"), role: roleEntry},
		{path: memberPath("call_id"), role: roleCallID},
		closed(memberPath("verdict"), VerdictProceed, VerdictReject, VerdictHold, VerdictAnswer),
		closed(memberPath("by"), ByHuman, ByPolicy, ByAgent),
	},
	TypeResult: joined(itemMembers("item"), []special{
		{path: memberPath("target"), role: roleEntry},
		{path: memberPath("call_id"), role: roleCallID},
		{path: memberPath("item", "call_id"), role: roleCallID},
		{path: memberPath("trust", "reads", "[]"), role: roleFixed, when: func(e Entry) bool { return e.(*ResultEntry).Trust != nil }},
	}),
	TypeLabel: {{path: memberPath("target"), role: roleEntry}},
	TypeOutcome: {
		{path: memberPath("target"), role: roleEntry},
		closed(memberPath("kind"), OutcomeFeedback, OutcomeTest, OutcomeTask, OutcomeToolError, OutcomeEval, OutcomeCustom),
	},
	TypeLink: {
		closed(memberPath("rel"), RelSubsession, RelForkOf, RelContinuedIn, RelJudgedBy, RelPeer),
		closed(memberPath("cancel"), CancelSent),
		{path: memberPath("target"), role: roleEntry, when: func(e Entry) bool { return e.(*LinkEntry).Target != "" }},
		// A link carrying a url names the callee's session in another
		// system, an ID of that system's and not of this format's, which
		// is text since 0.19 (RFC 0001, Every redacted entry).
		{path: memberPath("session"), role: roleSession, when: func(e Entry) bool { return e.(*LinkEntry).URL == "" }},
		{path: memberPath("call_id"), role: roleCallID},
		{path: memberPath("spawn_id"), role: roleCallID, when: func(e Entry) bool { return e.(*LinkEntry).SpawnID != "" }},
	},
	TypeCustom: {{path: memberPath("call_id"), role: roleCallID}},
	TypeEnv: {
		{path: memberPath("tools"), role: roleDataKeys},
		{path: memberPath("files", "read"), role: roleDataKeys},
		{path: memberPath("files", "written"), role: roleDataKeys},
		{path: memberPath("files", "read", "*"), role: roleHash},
		{path: memberPath("files", "written", "*"), role: roleHash},
		closed(memberPath("workspace", "kind"), WorkspaceLocal, WorkspaceContainer, WorkspaceRemote),
	},
}

// headerMembers are the members of a header that are not text (RFC
// 0001, The redacted session): those a redaction writes, type, format,
// id, created_at, payload, records, media, redacted and redacted_from,
// fixed whole; base, an entry of this file, and parent_session, a
// session, which are references; and spawned_by, a call_id, which
// changes only in step with the call IDs. Every other member, cwd,
// harness and each member the format does not define, is text.
var headerMembers = []special{
	{path: memberPath("type"), role: roleFixed},
	{path: memberPath("format"), role: roleFixed},
	{path: memberPath("id"), role: roleFixed},
	{path: memberPath("created_at"), role: roleFixed},
	{path: memberPath("payload"), role: roleFixed},
	{path: memberPath("records"), role: roleFixed},
	{path: memberPath("media"), role: roleFixed},
	{path: memberPath("redacted"), role: roleFixed},
	{path: memberPath("redacted_from"), role: roleFixed},
	{path: memberPath("base"), role: roleEntry},
	{path: memberPath("parent_session"), role: roleSession},
	{path: memberPath("spawned_by"), role: roleCallID},
}

// specialsOf returns the members of e that are not body text, those its
// decoder did not take as the format defines them left out. An extension
// entry's are its envelope's alone, since this package cannot read the
// rest.
func specialsOf(e Entry) []special {
	out := append([]special(nil), envelopeMembers...)
	if _, unknown := e.(*UnknownEntry); unknown {
		return out
	}
	for _, sp := range bodyMembers[e.EntryType()] {
		if sp.when == nil || sp.when(e) {
			out = append(out, sp)
		}
	}
	return out
}

// ErrNoPlace is returned by [RemapRefs] when a [Remap] says an entry
// was left out with nothing in its place, an empty ID, for a member that
// names one predecessor and so must name what stands in the left-out
// entry's place (RFC 0001, Leaving entries out): a target, a from, a
// first_kept, images' before or an omitted part's of. A leaf label's
// target is not refused: it names the universal root, as parent does. A
// cancel notice's target is refused too, and a writer leaves the notice
// out with the link it names, since nothing standing in a link's place
// is a link.
var ErrNoPlace = errors.New("agentsession: a member naming one entry has nothing in the place of the entry left out")

// ErrUnmappedCopy is returned by [RemapRefs] for an entry reference
// into another session, a copied item's copied_from entry or a parents
// element's, whose session the remap re-points while it leaves the
// entry as written, by its Entry callback or for want of one: the
// session is named by its redaction, so the entry names an entry of the
// original inside the redaction, which neither verifies there nor hides
// what was redacted (RFC 0001, Every redacted entry). A redactor maps
// such an entry to the one the session's redaction made from it, or
// drops it.
var ErrUnmappedCopy = errors.New("agentsession: an entry reference's session is re-pointed and its entry is not mapped")

// Remap says where the references [RemapRefs] meets now point. A
// callback returns the ID a reference is to name instead, or false to
// leave it as written; a nil callback leaves every reference of its kind
// as written and reports none of them.
type Remap struct {
	// Entry maps a reference to an entry: entry is the ID written, and
	// session the session the reference names the entry in, "" when it
	// names none, which is this file. Only a parents element names a
	// session, and it may name this file's own session by its ID, which
	// the callback takes as it takes "".
	//
	// An empty ID and true say the entry was left out and nothing a
	// member may name stands in its place, and RemapRefs applies RFC
	// 0001's rules for leaving entries out: a member naming one element
	// of a set loses it (a parents element, an omit items or outputs
	// items ID, an item's queued_from or from_link), a list so emptied
	// is removed, and an omit left with no member with it; parent names
	// the universal root, null, or Base in a file with one; a leaf
	// label's target, the one other member that may, names the root;
	// and any other member, which names one predecessor and must name
	// what stands in the place, is refused with [ErrNoPlace].
	Entry func(session, entry string) (string, bool)
	// Bound maps a reference that bounds a range from below, a
	// compaction's first_kept and images' before in a config or a
	// checkpoint, which RFC 0001 gives a place of its own: a left-out
	// entry is replaced there by the first entry written after it on its
	// path, since naming one above it would put that entry back into
	// every later request, or send its images. An empty ID and true say
	// nothing was written after it, which is refused with [ErrNoPlace].
	// When Bound is nil, Entry maps those members too, which is right
	// only while every entry they name was kept: a writer that leaves
	// entries out sets it.
	Bound func(entry string) (string, bool)
	// Base is, for entries written to a file with a base, that base as
	// the file names it. Every entry given is then one of the session's
	// own, and the base is above each on its path, so a parent with
	// nothing else in its place names the base and not the universal
	// root, which would break the base rule (RFC 0001, Leaving entries
	// out). Empty for a file without one.
	Base string
	// Session maps a reference to a session by its ID. An entry
	// reference into a session it re-points, a parents element's entry or
	// a copied_from's, must then be mapped or dropped by Entry: one left
	// as written, or with Entry nil, is refused with [ErrUnmappedCopy].
	Session func(id string) (string, bool)
}

// Reference is one member naming an entry or a session that a [Remap]
// left as written.
type Reference struct {
	// At is the member, an RFC 6901 JSON Pointer into the entry or the
	// header, such as /parent, /parents/1/entry or /omit/items/0.
	At string
	// Session is the session the reference names: a session reference's
	// ID, or the session an entry reference names its entry in, "" for
	// this file.
	Session string
	// Entry is the ID an entry reference names; empty for a session
	// reference.
	Entry string
}

// Remapped is what [RemapRefs] made of an entry.
type Remapped struct {
	// Entry is the entry with its references re-pointed: a new entry,
	// whose ID, timestamp and every other member are the input's.
	Entry Entry
	// Unmapped lists the references a callback left as written, member
	// by member.
	Unmapped []Reference
	// Dropped lists the references removed because the entry they named
	// was left out, each at the place it had in the input.
	Dropped []Reference
	// Opaque is set for an extension entry. Its members are its own and
	// this package cannot read them, so only its envelope, parent and
	// parents, is re-pointed, and a member of its own may still name an
	// entry or a session as written.
	Opaque bool
}

// RemapRefs re-points every member of e that names an entry or a
// session, through m, and returns the result as a new entry; e is not
// changed. The entry references are parent, each parents element's
// entry, an item's queued_from and from_link, a compaction's first_kept
// and the omit items, outputs items and images before of its
// checkpoint, a branch summary's from, the target of a label, an
// outcome, a dispatch and a decision, a config entry's omit items,
// outputs items and images before and the of of each omitted part that
// is a keep, a positive keep and no id, and the target of a judged_by link and of a cancel notice,
// which name an entry of this session. The session references are each
// parents element's session and the session of a link without a url;
// a link carrying a url names a session of another system, which is
// text (RFC 0001, Every redacted entry, since 0.19). A member of one of
// those names in a form or place the format does not define it in, kept
// as written by the decoder, such as an of beside an id, names nothing
// and is left alone, as is an item's response, which is a provider's ID
// and not an entry's, and a redacted member, which names entries of the
// original by position and not by id.
//
// Every other member, kept extras and members the format does not
// define among them, comes through unchanged, and so does the order of
// parents unless an element of it was re-pointed: then the elements are
// sorted by session and entry, as a writer must write them, since the
// entry is a new one, an extension entry's as a core entry's. Its ID is the input's, which no longer hashes it,
// so a writer clears it before a commit, which would refuse it with
// [ErrBadID], and the content hash is empty until the commit computes
// both. A writer that re-points parent
// here, rather than setting it afterwards, keeps a line the fields did
// not reproduce as the line, with the references changed.
//
// What it cannot read, an extension entry's members, is reported as
// Opaque; a reference a callback left as written, in Unmapped. Header
// members are [RemapHeaderRefs]'s. Which entry stands in the place of
// one a writer leaves out, a placeholder or the nearest entry kept above
// it, is the writer's to say, and the callback returns it; when nothing
// does, the callback returns an empty ID and RemapRefs removes or
// refuses each member that named the entry as RFC 0001's Leaving
// entries out has it, reporting what it removed in Dropped.
func RemapRefs(e Entry, m Remap) (Remapped, error) {
	return remapRefs(e, m, true)
}

// remapRefs is RemapRefs, with sort saying whether re-pointed parents
// are sorted: a migration keeps the order the file had, as it keeps
// every other member, since what it reads is not a new entry.
func remapRefs(e Entry, m Remap, sortParents bool) (Remapped, error) {
	if e == nil {
		return Remapped{}, errors.New("agentsession: nil entry")
	}
	if u, ok := e.(*UnknownEntry); ok {
		return remapUnknown(u, m)
	}
	root, err := entryJSON(e)
	if err != nil {
		return Remapped{}, err
	}
	var (
		out     Remapped
		parents bool
		gone    []string // pointers to remove, in the order met
		root0   bool     // parent names the universal root
		leaf0   bool     // a leaf label's target names the universal root
		noPlace error
	)
	l, isLabel := e.(*LabelEntry)
	leafLabel := isLabel && l.Label != nil && *l.Label == LeafLabel
	for _, sp := range specialsOf(e) {
		var mapped func(at string, s string, holder map[string]any) (string, bool)
		switch {
		case sp.role == roleEntry && (m.Entry != nil || sp.bound && m.Bound != nil):
			mapped = func(at, s string, holder map[string]any) (string, bool) {
				session := ""
				if sp.session != "" && holder != nil {
					session, _ = holder[sp.session].(string)
				}
				var (
					id string
					ok bool
				)
				if sp.bound && m.Bound != nil {
					id, ok = m.Bound(s)
				} else {
					id, ok = m.Entry(session, s)
				}
				switch {
				case !ok:
					out.Unmapped = append(out.Unmapped, Reference{At: at, Session: session, Entry: s})
				case id != "":
				case sp.set:
					out.Dropped = append(out.Dropped, Reference{At: at, Session: session, Entry: s})
					gone = append(gone, pointerUp(at, sp.up))
					return s, false
				case len(sp.path) == 1 && sp.path[0] == "parent":
					root0 = true
					return s, false
				case leafLabel && len(sp.path) == 1 && sp.path[0] == "target":
					// A leaf label may name the universal root.
					leaf0 = true
					return s, false
				case noPlace == nil:
					noPlace = fmt.Errorf("%w: %s names %s", ErrNoPlace, at, s)
					return s, false
				default:
					return s, false
				}
				return id, ok
			}
		case sp.role == roleSession && m.Session != nil:
			mapped = func(at, s string, _ map[string]any) (string, bool) {
				id, ok := m.Session(s)
				if !ok {
					out.Unmapped = append(out.Unmapped, Reference{At: at, Session: s})
				}
				return id, ok
			}
		default:
			continue
		}
		eachString(root, sp.path, "", nil, func(at, s string, holder map[string]any) (string, bool) {
			if s == "" || !sp.applies(holder, s) {
				return s, false // names nothing, or is not the member defined here
			}
			id, ok := mapped(at, s, holder)
			if ok && id != s && sp.path[0] == "parents" {
				parents = true
			}
			return id, ok && id != s
		})
	}
	if noPlace != nil {
		return Remapped{}, noPlace
	}
	if m.Session != nil {
		// An entry reference into a session the remap re-points, left as
		// written, names the original's entry inside the redaction.
		for _, r := range sessionEntryRefs(e) {
			if _, repointed := m.Session(r.Session); !repointed {
				continue
			}
			if m.Entry == nil || slices.ContainsFunc(out.Unmapped, func(u Reference) bool { return u.At == r.At }) {
				return Remapped{}, fmt.Errorf("%w: %s %s names %s in %s", ErrUnmappedCopy, e.Base().ID, r.At, r.Entry, r.Session)
			}
		}
	}
	if root0 && m.Base != "" {
		root["parent"] = m.Base
	} else if root0 {
		root["parent"] = nil
	}
	if leaf0 {
		root["target"] = nil
	}
	removeAll(root, gone)
	if parents && sortParents {
		sortParentsJSON(root)
	}
	if out.Entry, err = entryFromJSON(e, root); err != nil {
		return Remapped{}, err
	}
	return out, nil
}

// sessionEntryRefs lists the entry references of e that name an entry
// in a session, as the format defines them: each parents element naming
// both, and a copied item's copied_from entry.
func sessionEntryRefs(e Entry) []Reference {
	var out []Reference
	for i, r := range e.Base().Parents {
		if r.Session != "" && r.Entry != "" {
			out = append(out, Reference{At: "/parents/" + strconv.Itoa(i) + "/entry", Session: r.Session, Entry: r.Entry})
		}
	}
	if it, ok := e.(*ItemEntry); ok && it.CopiedFrom != nil && it.CopiedFrom.Entry != "" {
		out = append(out, Reference{At: "/copied_from/entry", Session: it.CopiedFrom.Session, Entry: it.CopiedFrom.Entry})
	}
	return out
}

// remapUnknown re-points an extension entry's envelope, which is all of
// it this package can read, and writes its line again from it.
func remapUnknown(u *UnknownEntry, m Remap) (Remapped, error) {
	cp := *u
	cp.Parents = append([]EntryRef(nil), u.Parents...)
	cp.content = ""
	out := Remapped{Opaque: true}
	if m.Entry != nil && cp.Parent != "" {
		if id, ok := m.Entry("", cp.Parent); ok {
			if id == "" {
				id = m.Base // the base in a file with one, else the universal root
			}
			cp.Parent = id
		} else {
			out.Unmapped = append(out.Unmapped, Reference{At: "/parent", Entry: cp.Parent})
		}
	}
	kept := cp.Parents[:0:0]
	for i, r := range cp.Parents {
		at := "/parents/" + strconv.Itoa(i)
		left := m.Entry == nil
		if m.Entry != nil && r.Entry != "" {
			if id, ok := m.Entry(r.Session, r.Entry); !ok {
				left = true
				out.Unmapped = append(out.Unmapped, Reference{At: at + "/entry", Session: r.Session, Entry: r.Entry})
			} else if id == "" {
				out.Dropped = append(out.Dropped, Reference{At: at + "/entry", Session: r.Session, Entry: r.Entry})
				continue
			} else {
				r.Entry = id
			}
		}
		if m.Session != nil && r.Session != "" {
			if id, ok := m.Session(r.Session); ok {
				if left && r.Entry != "" {
					return Remapped{}, fmt.Errorf("%w: %s %s names %s in %s", ErrUnmappedCopy, u.ID, at+"/entry", r.Entry, r.Session)
				}
				r.Session = id
			} else {
				out.Unmapped = append(out.Unmapped, Reference{At: at + "/session", Session: r.Session})
			}
		}
		kept = append(kept, r)
	}
	if len(kept) == 0 {
		kept = nil
	}
	cp.Parents = kept
	if !slices.Equal(cp.Parents, u.Parents) {
		sortParents(cp.Parents) // a re-pointed reference is a writer's, sorted
	}
	raw, err := rewriteEnvelope(&cp)
	if err != nil {
		return Remapped{}, err
	}
	cp.Raw = raw
	out.Entry = &cp
	return out, nil
}

// RemapHeaderRefs is [RemapRefs] for a header: base names the entry the
// session continues from, which the file holds as the last line of its
// prefix and so names in this file, and parent_session names a session.
// redacted_from names the original a redacted session was made from,
// which a redaction does not re-point, so it stays as written.
func RemapHeaderRefs(h Header, m Remap) (Header, []Reference) {
	var unmapped []Reference
	if m.Entry != nil && h.Base != "" {
		if id, ok := m.Entry("", h.Base); ok {
			h.Base = id
		} else {
			unmapped = append(unmapped, Reference{At: "/base", Entry: h.Base})
		}
	}
	if m.Session != nil && h.ParentSession != "" {
		if id, ok := m.Session(h.ParentSession); ok {
			h.ParentSession = id
		} else {
			unmapped = append(unmapped, Reference{At: "/parent_session", Session: h.ParentSession})
		}
	}
	return h, unmapped
}

// eachString calls fn with every string the pattern path names in v, v
// being the value at pointer at whose object holder holds it, and puts
// in its place what fn returns when fn reports a change. It changes v in
// place.
func eachString(v any, path []string, at string, holder map[string]any, fn func(at, s string, holder map[string]any) (string, bool)) any {
	if len(path) == 0 {
		s, ok := v.(string)
		if !ok {
			return v
		}
		if out, changed := fn(at, s, holder); changed {
			return out
		}
		return v
	}
	switch step := path[0]; step {
	case "[]":
		list, ok := v.([]any)
		if !ok {
			return v
		}
		for i := range list {
			list[i] = eachString(list[i], path[1:], at+"/"+strconv.Itoa(i), nil, fn)
		}
	case "*":
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		for _, k := range sortedKeys(obj) {
			obj[k] = eachString(obj[k], path[1:], at+"/"+pointerToken(k), obj, fn)
		}
	default:
		obj, ok := v.(map[string]any)
		if !ok {
			return v
		}
		if child, ok := obj[step]; ok {
			obj[step] = eachString(child, path[1:], at+"/"+pointerToken(step), obj, fn)
		}
	}
	return v
}

// pointerUp is the pointer at with its last n tokens removed.
func pointerUp(at string, n int) string {
	for ; n > 0; n-- {
		if i := strings.LastIndexByte(at, '/'); i >= 0 {
			at = at[:i]
		}
	}
	return at
}

// removeAll removes from root the member or element each pointer names,
// the last met first, so an element removed never moves one still to
// be. A list it empties is removed, and so is an omit object left with
// no member: an empty omit clears the one in force, where the list
// removed only added to it. An outputs group whose items it empties is
// removed whole, since a group sets nothing else, then groups emptied,
// an outputs left with no member, and its omit if that leaves it none
// (RFC 0001, Leaving entries out); nothing it did not empty is touched.
func removeAll(root map[string]any, pointers []string) {
	var emptied []string
	for i := len(pointers) - 1; i >= 0; i-- {
		at := pointers[i]
		up := pointerUp(at, 1)
		key := unescapeToken(at[len(up)+1:])
		switch c := pointerValue(root, up).(type) {
		case map[string]any:
			delete(c, key)
		case []any:
			k, err := strconv.Atoi(key)
			if err != nil || k < 0 || k >= len(c) {
				continue
			}
			setPointer(root, up, slices.Delete(c, k, k+1))
		}
		emptied = append(emptied, up)
	}
	slices.Sort(emptied)
	// groups are the outputs groups, by the pointer of their list, whose
	// items this removal emptied: each goes whole, since a group sets
	// nothing else. A group that held no items before is a form the
	// format does not define, which no removal reaches, and is left as
	// written.
	groups := map[string][]int{}
	for _, up := range slices.Compact(emptied) {
		list, ok := pointerValue(root, up).([]any)
		if !ok || len(list) > 0 {
			continue
		}
		holder := pointerUp(up, 1)
		if strings.HasSuffix(up, "/items") && strings.HasSuffix(pointerUp(holder, 1), "/outputs/groups") {
			if k, err := strconv.Atoi(holder[len(pointerUp(holder, 1))+1:]); err == nil {
				groups[pointerUp(holder, 1)] = append(groups[pointerUp(holder, 1)], k)
				continue
			}
		}
		if obj, ok := pointerValue(root, holder).(map[string]any); ok {
			delete(obj, unescapeToken(up[len(holder)+1:]))
			dropEmptyOmit(root, holder, obj)
		}
	}
	for at, ks := range groups {
		list, ok := pointerValue(root, at).([]any)
		if !ok {
			continue
		}
		slices.Sort(ks)
		for i := len(ks) - 1; i >= 0; i-- {
			if ks[i] < len(list) {
				list = slices.Delete(list, ks[i], ks[i]+1)
			}
		}
		if len(list) > 0 {
			setPointer(root, at, list)
			continue
		}
		outputsAt := pointerUp(at, 1)
		outputs, ok := pointerValue(root, outputsAt).(map[string]any)
		if !ok {
			continue
		}
		delete(outputs, "groups")
		if len(outputs) > 0 {
			continue
		}
		omitAt := pointerUp(outputsAt, 1)
		if omit, ok := pointerValue(root, omitAt).(map[string]any); ok {
			delete(omit, "outputs")
			dropEmptyOmit(root, omitAt, omit)
		}
	}
}

// dropEmptyOmit removes the omit object obj at the pointer holder when
// it is an omit left with no member: an empty omit clears the one in
// force, where what was removed only added to it.
func dropEmptyOmit(root map[string]any, holder string, obj map[string]any) {
	if len(obj) == 0 && strings.HasSuffix(holder, "/omit") {
		if h, ok := pointerValue(root, pointerUp(holder, 1)).(map[string]any); ok {
			delete(h, "omit")
		}
	}
}

// pointerValue returns the value pointer at names in v, nil if none.
func pointerValue(v any, at string) any {
	if at == "" {
		return v
	}
	for _, t := range strings.Split(at[1:], "/") {
		switch c := v.(type) {
		case map[string]any:
			v = c[unescapeToken(t)]
		case []any:
			k, err := strconv.Atoi(t)
			if err != nil || k < 0 || k >= len(c) {
				return nil
			}
			v = c[k]
		default:
			return nil
		}
	}
	return v
}

// setPointer puts val at the member or element pointer at names in
// root, which holds it already.
func setPointer(root map[string]any, at string, val any) {
	up := pointerUp(at, 1)
	key := unescapeToken(at[len(up)+1:])
	switch c := pointerValue(root, up).(type) {
	case map[string]any:
		c[key] = val
	case []any:
		if k, err := strconv.Atoi(key); err == nil && k >= 0 && k < len(c) {
			c[k] = val
		}
	}
}

// unescapeToken undoes pointerToken.
func unescapeToken(t string) string {
	return strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
}

// sortParentsJSON sorts an entry's parents elements as sortParents sorts
// references: by session, those naming none first, then by entry. An
// element that is not a reference sorts as one naming nothing.
func sortParentsJSON(root map[string]any) {
	list, ok := root["parents"].([]any)
	if !ok {
		return
	}
	key := func(v any) (string, string) {
		obj, _ := v.(map[string]any)
		session, _ := obj["session"].(string)
		entry, _ := obj["entry"].(string)
		return session, entry
	}
	sort.SliceStable(list, func(i, j int) bool {
		si, ei := key(list[i])
		sj, ej := key(list[j])
		if si != sj {
			return si < sj
		}
		return ei < ej
	})
}

// entryJSON returns e's JSON, as MarshalEntry writes it, decoded with
// its numbers as written.
func entryJSON(e Entry) (map[string]any, error) {
	data, err := MarshalEntry(e)
	if err != nil {
		return nil, err
	}
	v, err := parseValue(data)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("agentsession: entry is not an object")
	}
	return root, nil
}

// entryFromJSON decodes root as an entry of e's core type, afresh, so
// what the new entry keeps as written is what root holds and no line
// read with e can be written for it. The timestamp keeps e's spelling,
// which is how a session tells an entry it read from one a caller
// built, and a redacted member e did not hold typed, one read under a
// minor before 0.13 among them, stays unknown.
func entryFromJSON(e Entry, root map[string]any) (Entry, error) {
	data, err := jsonx.MarshalNoEscape(root)
	if err != nil {
		return nil, err
	}
	all, err := splitMembers(data)
	if err != nil {
		return nil, err
	}
	mk, ok := coreEntries[e.EntryType()]
	if !ok {
		return nil, fmt.Errorf("agentsession: %s is not a core entry type", e.EntryType())
	}
	d := mk()
	if _, err := decodeForm(d, data, all); err != nil {
		return nil, fmt.Errorf("agentsession: entry %s (%s): %w", e.Base().ID, e.EntryType(), err)
	}
	d.Base().tsRaw = e.Base().tsRaw
	keepRedactedForm(e, d)
	return d, nil
}

// sortedKeys returns obj's keys in order, so a walk is deterministic.
func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// pointerToken escapes a member name for an RFC 6901 pointer.
func pointerToken(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
