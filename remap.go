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
	// roleFixed is the envelope and legacy_id: the whole value, at any
	// depth, is neither rewritten nor re-pointed.
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
}

// applies reports whether sp names the member that holder holds.
func (sp special) applies(holder map[string]any) bool {
	return sp.holds == nil || sp.holds(holder)
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

// memberPath is a path, for the table.
func memberPath(steps ...string) []string { return steps }

// envelopeMembers are the members every entry carries that are not its
// body: the envelope, and legacy_id, the name an entry had before its
// file was migrated, which is kept as that name. parents is fixed whole,
// extras included; its entry and session members are references.
var envelopeMembers = []special{
	{path: memberPath("type"), role: roleFixed},
	{path: memberPath("id"), role: roleFixed},
	{path: memberPath("ts"), role: roleFixed},
	{path: memberPath("legacy_id"), role: roleFixed},
	{path: memberPath("parent"), role: roleEntry},
	{path: memberPath("parents"), role: roleFixed},
	{path: memberPath("parents", "[]", "entry"), role: roleEntry, session: "session"},
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
// data in a member name, which a rewrite of the body reaches.
//
// A call_id pairs a call with its output, its dispatches, decisions and
// links, and a subsession's ID is derived from it, so a rewrite of the
// body leaves it where the format defines it: on an item (and so on a
// queued entry's item and a compaction's pinned items), on a dispatch, a
// decision, a link and a custom entry, and in a run end's pending list.
// A call_id inside data the format does not define, a decision's args,
// a custom entry's data, an outcome's details or a member it does not
// define, is the writer's text and body.
var bodyMembers = map[string][]special{
	TypeItem:     {{path: memberPath("queued_from"), role: roleEntry}, {path: memberPath("item", "call_id"), role: roleCallID}},
	TypeQueued:   {{path: memberPath("item", "call_id"), role: roleCallID}},
	TypeResponse: {{path: memberPath("request_hash"), role: roleHash}},
	TypeConfig: {
		{path: memberPath("omit", "items", "[]"), role: roleEntry, when: func(e Entry) bool { return e.(*ConfigEntry).Omit != nil }},
		{path: memberPath("instructions_omitted", "[]", "of"), role: roleEntry, holds: keepOf},
		{path: memberPath("instructions_parts", "[]", "hash"), role: roleHash},
	},
	TypeCompaction: {
		{path: memberPath("first_kept"), role: roleEntry},
		{path: memberPath("config", "omit", "items", "[]"), role: roleEntry, when: func(e Entry) bool { return !e.(*CompactionEntry).Config.Omit.IsZero() }},
		{path: memberPath("config", "instructions_parts", "[]", "hash"), role: roleHash},
		{path: memberPath("pinned", "[]", "call_id"), role: roleCallID},
	},
	TypeBranchSummary: {{path: memberPath("from"), role: roleEntry}},
	TypeRun:           {{path: memberPath("pending", "[]"), role: roleCallID}},
	TypeDispatch:      {{path: memberPath("target"), role: roleEntry}, {path: memberPath("call_id"), role: roleCallID}},
	TypeDecision:      {{path: memberPath("target"), role: roleEntry}, {path: memberPath("call_id"), role: roleCallID}},
	TypeLabel:         {{path: memberPath("target"), role: roleEntry}},
	TypeOutcome:       {{path: memberPath("target"), role: roleEntry}},
	TypeLink: {
		{path: memberPath("target"), role: roleEntry, when: func(e Entry) bool { return e.(*LinkEntry).Target != "" }},
		{path: memberPath("session"), role: roleSession},
		{path: memberPath("call_id"), role: roleCallID},
	},
	TypeCustom: {{path: memberPath("call_id"), role: roleCallID}},
	TypeEnv: {
		{path: memberPath("files", "read"), role: roleDataKeys},
		{path: memberPath("files", "written"), role: roleDataKeys},
		{path: memberPath("files", "read", "*"), role: roleHash},
		{path: memberPath("files", "written", "*"), role: roleHash},
	},
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
	Entry func(session, entry string) (string, bool)
	// Session maps a reference to a session by its ID.
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
	// Opaque is set for an extension entry. Its members are its own and
	// this package cannot read them, so only its envelope, parent and
	// parents, is re-pointed, and a member of its own may still name an
	// entry or a session as written.
	Opaque bool
}

// RemapRefs re-points every member of e that names an entry or a
// session, through m, and returns the result as a new entry; e is not
// changed. The entry references are parent, each parents element's
// entry, an item's queued_from, a compaction's first_kept and the omit
// items of its checkpoint, a branch summary's from, the target of a
// label, an outcome, a dispatch and a decision, a config entry's omit
// items and the of of each omitted part that is a keep, a positive keep
// and no id, and the target of a judged_by link and of a cancel notice,
// which name an entry of this session. The session references are each
// parents element's session and a link's session. A member of one of
// those names in a form or place the format does not define it in, kept
// as written by the decoder, such as an of beside an id, names nothing
// and is left alone, as is an item's response, which is a provider's ID
// and not an entry's.
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
// members are [RemapHeaderRefs]'s. Leaving an entry out, or
// re-parenting its children, is not a re-pointing: the entries a
// writer keeps say what replaces one it leaves out, and the callback
// returns that.
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
	)
	for _, sp := range specialsOf(e) {
		var mapped func(at string, s string, holder map[string]any) (string, bool)
		switch {
		case sp.role == roleEntry && m.Entry != nil:
			mapped = func(at, s string, holder map[string]any) (string, bool) {
				session := ""
				if sp.session != "" && holder != nil {
					session, _ = holder[sp.session].(string)
				}
				id, ok := m.Entry(session, s)
				if !ok {
					out.Unmapped = append(out.Unmapped, Reference{At: at, Session: session, Entry: s})
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
			if s == "" || !sp.applies(holder) {
				return s, false // names nothing, or is not the member defined here
			}
			id, ok := mapped(at, s, holder)
			if ok && id != s && sp.path[0] == "parents" {
				parents = true
			}
			return id, ok && id != s
		})
	}
	if parents && sortParents {
		sortParentsJSON(root)
	}
	if out.Entry, err = entryFromJSON(e, root); err != nil {
		return Remapped{}, err
	}
	return out, nil
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
			cp.Parent = id
		} else {
			out.Unmapped = append(out.Unmapped, Reference{At: "/parent", Entry: cp.Parent})
		}
	}
	for i, r := range cp.Parents {
		at := "/parents/" + strconv.Itoa(i)
		if m.Entry != nil && r.Entry != "" {
			if id, ok := m.Entry(r.Session, r.Entry); ok {
				cp.Parents[i].Entry = id
			} else {
				out.Unmapped = append(out.Unmapped, Reference{At: at + "/entry", Session: r.Session, Entry: r.Entry})
			}
		}
		if m.Session != nil && r.Session != "" {
			if id, ok := m.Session(r.Session); ok {
				cp.Parents[i].Session = id
			} else {
				out.Unmapped = append(out.Unmapped, Reference{At: at + "/session", Session: r.Session})
			}
		}
	}
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
// which a twin does not replace, so it stays as written.
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
// built.
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
