package agentsession

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ChristopherDavenport/openresponses"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// partLists are, by core type, where an entry holds instruction parts
// beside the instructions they join to: the parts' path and the
// instructions'.
var partLists = map[string][2][]string{
	TypeConfig:     {memberPath("instructions_parts"), memberPath("instructions")},
	TypeCompaction: {memberPath("config", "instructions_parts"), memberPath("config", "instructions")},
}

// ErrInstructionsJoin is returned by [RewriteStrings] for an entry
// whose instructions sit beside parts it cannot resolve, a part named
// by hash or by keep whose text is in an entry the [Rewriter] was not
// given, when f would change the instructions: the rewritten string
// could no longer be shown to be the rewritten parts joined, which the
// format requires it to stay, so the rewrite fails closed.
var ErrInstructionsJoin = errors.New("agentsession: rewritten instructions cannot be shown to be the rewritten parts joined")

// ErrKeyCollision is returned by [RewriteStrings] for an entry in which
// the rewrite gives two member names that hold data the same name, such
// as two paths of an env entry's files made one, or an extension
// entry's member a name its envelope holds, which the format forbids a
// redaction to keep.
var ErrKeyCollision = errors.New("agentsession: a rewrite makes two member names one")

// Rewriter is [RewriteStrings] over the entries of one session file,
// given to it in file order, prefix included: one Rewriter per file. It
// carries what one entry cannot see. An instruction part a delta does
// not repeat is named by the hash of its text, which an earlier entry
// holds, so the Rewriter remembers what each text it rewrote hashes to
// now, and a later part naming it by hash names the rewritten text. And
// instructions beside parts named by hash or keep are their join only
// as the path resolves the parts, so the Rewriter replays the settings
// along each entry's path as context building does, and rebuilds such
// instructions from the rewritten parts.
//
// A part named by a hash whose text the Rewriter has not met keeps its
// hash, which then names the original's text, and RFC 0001 forbids a
// redactor to write it: [Rewriter.Unmet] reports each such part, so a
// redactor writes it as text of its own or refuses before it commits
// the entry. What it has met depends
// on what it was given, so a Rewriter fed another file's entries, or a
// file's out of order, can rewrite one entry two ways: a fork and its
// origin each redacted by a Rewriter of their own give their shared
// prefix the same bytes. A Rewriter is not safe for concurrent use. The
// zero value is not usable; see [NewRewriter].
type Rewriter struct {
	f func(string) string
	// parts maps the hash of each part text met to the hash of what f
	// made of it.
	parts map[string]string
	// settings holds the settings in force after each entry met, by its
	// ID in the original, as context building replays them.
	settings map[string]Settings
	// unmet lists the parts written under a hash whose text was not met.
	unmet []UnmetPart
}

// UnmetPart is an instruction part a [Rewriter] wrote that the path it
// is on cannot rebuild in the redacted session: a part named by a hash
// whose text no part in force on its path holds, as the Rewriter
// replays the entries it was given and not those it was told were left
// out, or a keep naming parts no entry on its path put in force. The
// first keeps the original's hash, which a redacted session MUST NOT
// keep, as it neither verifies there nor hides the text, and a hash
// rewritten from a text met elsewhere resolves nowhere on the path;
// the second names parts the redacted path lacks (RFC 0001, Every
// redacted entry). A redactor writes such a part as text of its own,
// such as fixed placeholder text, and gives the responses after it no
// request_hash or a recomputed one, or it does not redact the entry.
//
// The part's hash and text are not carried. Entry and ID are values of
// the original, an entry's hash and a part's id as written there, for
// the redactor's own use: a hash of the original can confirm a guessed
// text, and an id may hold what is redacted, so neither is to be logged
// or published.
type UnmetPart struct {
	// Entry is the ID, in the original, of the entry holding the part.
	Entry string
	// At is the part's member in that entry, an RFC 6901 JSON Pointer
	// such as /instructions_parts/2/hash, or /instructions_parts/0/keep
	// for a keep.
	At string
	// ID is the part's id as the original wrote it, empty for a keep
	// and for a part with none.
	ID string
}

// Unmet returns, in the order met, every part the Rewriter has written
// that its path cannot rebuild (see [UnmetPart]). A redactor reads it
// after each [Rewriter.Rewrite], before it commits the entry; an
// entry's parts are added to it by the Rewrite that wrote them.
func (r *Rewriter) Unmet() []UnmetPart {
	return slices.Clone(r.unmet)
}

// LeftOut tells the Rewriter that the redactor leaves e out of what it
// writes. A redactor gives the Rewriter every entry of the file, in file
// order: each it writes to Rewrite, and each it leaves out here, in
// place of Rewrite or after it. The entries under e then read the
// settings in force above it, as the redacted path does, so a part
// whose text only e put in force, or a keep counting parts e put in
// force, is reported by Unmet.
func (r *Rewriter) LeftOut(e Entry) {
	if e == nil || e.Base().ID == "" {
		return
	}
	r.settings[e.Base().ID], _ = r.settingsAt(e.Base().Parent)
}

// checkParts adds to unmet each part of a config delta that the
// settings in force on its path, as the Rewriter replays them, cannot
// rebuild: a part named by a hash alone whose text none of them holds,
// and a keep over parts they do not hold.
func (r *Rewriter) checkParts(c *ConfigEntry) {
	if len(c.InstructionsParts) == 0 {
		return
	}
	prev, _ := r.settingsAt(c.Parent)
	applied := prev.Apply(c).InstructionsParts
	report := func(i int, member, id string) {
		r.unmet = append(r.unmet, UnmetPart{Entry: c.ID, At: "/instructions_parts/" + strconv.Itoa(i) + "/" + member, ID: id})
	}
	// A delta element is one part in force, but a keep, which is as
	// many as it keeps when it resolves and one when it does not.
	j := 0
	for i, p := range c.InstructionsParts {
		if p.ID == "" && p.Keep > 0 {
			if j < len(applied) && applied[j].ID == "" && applied[j].Keep == p.Keep && applied[j].Unresolved() {
				report(i, "keep", "")
				j++
			} else {
				j += p.Keep
			}
			continue
		}
		if p.Text == "" && p.Hash != "" && j < len(applied) && applied[j].Unresolved() {
			report(i, "hash", p.ID)
		}
		j++
	}
}

// NewRewriter returns a Rewriter applying f, under the contract
// [RewriteStrings] states.
func NewRewriter(f func(string) string) *Rewriter {
	return &Rewriter{f: f, parts: map[string]string{}, settings: map[string]Settings{}}
}

// RewriteStrings returns a new entry with f applied to every string in
// e's body; e is not changed. That is every string the entry's JSON
// holds, at any depth: its typed members, those the format does not
// define (Unknown), the members a reader keeps as written beside a typed
// one (a workspace's host), an extension entry's own members, the items
// of an item, a compaction and a branch summary, inline media as its
// data URL, whose base64 payload f is given undecoded, and the record of
// what a writer normalised. f is given member names too where a name is
// data rather than the format's (RFC 0001, Every redacted entry): the
// paths an env entry's files are keyed by and the tool names its tools
// are keyed by, and every name, at any depth, inside an object whose
// members the format does not define: a custom entry's data, an
// outcome's details, a decision's args, a config's extra, the value of
// any member the format does not define, and an extension entry's own
// members, their names included. Two names of one object that the
// rewrite makes one, or an extension entry's name made one the envelope
// holds, are refused with [ErrKeyCollision].
//
// f is never given any other member name, a number or a boolean: in an
// object the format defines, an entry, a trigger or a workspace among
// them, a member name is the format's, and stays as written even when
// the member is one the format does not define, whose value is data; and
// the names inside an item, which the payload profile defines, stay as
// written. Nor is f
// given the envelope (type, id, parent, parents, ts), legacy_id or
// redacted, which is kept as it was in whatever form it is held; a
// member naming an entry or a session, which [RemapRefs] re-points; a
// member holding a hash (request_hash, an instruction part's hash, an
// env entry's file hashes); or a sidecar media URL, sidecar:sha256: and
// the blob's hash, whose blob a redactor replaces under the
// replacement's hash, as the format has it. A call_id where the format
// defines one, on an item, a dispatch, a decision, a link or a custom
// entry, and a run end's pending call IDs, are left too: a call_id
// pairs a call with its output, its dispatches, decisions and links, and
// a subsession's ID is derived from it, so a redactor that must change
// call IDs changes every one in step itself, and the subsessions' IDs
// with them. A call_id inside a decision's args, a custom entry's data
// or any member the format does not define is the writer's text and is
// given to f. Each member is taken where the format defines it: one of
// those names in another form or place, such as an of beside an id, is
// body text.
//
// An instruction part whose text changed has its hash, where it carries
// one, recomputed, and a part named by its hash alone, whose text is in
// an earlier entry, is given the hash of the rewritten text by a
// [Rewriter] that met it; one it did not meet keeps the original's hash
// and is reported by [Rewriter.Unmet]. Instructions that are the join of their parts,
// as the path resolves them, are not given to f whole: they are rebuilt
// as the join of the rewritten parts, so a rewrite cannot cross a part
// boundary. Instructions that are not such a join are given to f whole.
// Instructions beside a part the rewrite cannot resolve, a part named by
// hash or keep with its text in an entry it was not given, which
// RewriteStrings alone never is, are left when f leaves them and refused
// with [ErrInstructionsJoin] when it would change them.
//
// In normalised, each was is JSON source text, and f is applied to each
// string value in it, which is written again in canonical form where f
// changed it, a lone surrogate escaped as it was; a was that is a number,
// the digits of a member the payload carries as a string, is given to f
// as those digits and written as the JSON string of what f returns when
// it changes them, since the record can then no longer be the original
// text and must not keep the value. Each raw is the base64 of the
// original bytes of a string, which f is given decoded, its escapes
// resolved and an ill-formed sequence as the bytes it was, and which is
// encoded again with the ill-formed bytes kept. Without that the
// original value would survive in the record, and in raw no string
// rewrite would ever see it. A record is rewritten as the member it
// describes: its at follows a path the rewrite renamed, and a record of
// a member f is not given is left.
//
// f must be deterministic: one string gives one result wherever it
// appears, since a response_id and a part's text are named across
// entries by their value, and a rewrite that differs between two
// entries breaks the names. It is called in the same order on every
// run: an entry's members in sorted order, the elements of an array in
// index order, at every depth. A result that is not valid UTF-8 is
// repaired by the encoder, and a value the entry's type cannot hold,
// such as a closed member given a value it does not define, is an
// error from the decode.
//
// The result keeps e's ID, parent and timestamp, and what it keeps as
// written is what the rewritten JSON holds: it never writes e's line.
// Its hashes are a session's to compute, by [Session.Commit] once the
// writer has cleared the ID, and its content hash is empty until then.
// A response's request_hash is the session's to compute too, over the
// request the session it is committed to rebuilds for it, which no
// rewrite of one entry can see: the result still holds the original's,
// or none, and a redactor sets it with [Session.SetRequestHash] just
// before the commit. A redacted session hashes independently, so the
// original's is never kept (RFC 0001, Redaction).
func RewriteStrings(e Entry, f func(string) string) (Entry, error) {
	return NewRewriter(f).Rewrite(e)
}

// RewriteHeaderStrings returns a copy of h with f applied to every
// string of it that is text, under the contract [RewriteStrings]
// states: cwd, the harness's name and version, and every string and
// member name at any depth in the value of a member the format does not
// define (Extra), whose own name stays, as in an entry. What a
// redaction writes, type, format, id, created_at, payload, records,
// media, redacted and redacted_from, is left for the redactor to set;
// base and parent_session are references, which [RemapHeaderRefs]
// re-points; and spawned_by is a call_id, which a redactor that changes
// call IDs changes in step with the parent's, as it does every call_id
// RewriteStrings leaves (RFC 0001, The redacted session). One table in
// this package says which header member is which, so a header that
// gains a member cannot drift out of step with a redactor.
func RewriteHeaderStrings(h Header, f func(string) string) (Header, error) {
	data, err := jsonx.MarshalNoEscape(h)
	if err != nil {
		return Header{}, err
	}
	v, err := parseValue(data)
	if err != nil {
		return Header{}, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return Header{}, errors.New("agentsession: header is not an object")
	}
	out, err := NewRewriter(f).rewriteObject(root, headerMembers, shape{t: headerGoType}, false, false)
	if err != nil {
		return Header{}, fmt.Errorf("agentsession: header: %w", err)
	}
	if data, err = jsonx.MarshalNoEscape(out); err != nil {
		return Header{}, err
	}
	var nh Header
	if err := json.Unmarshal(data, &nh); err != nil {
		return Header{}, fmt.Errorf("agentsession: header: %w", err)
	}
	return nh, nil
}

// Rewrite is [RewriteStrings] with what the entries the Rewriter was
// given before, earlier in the file, say about this one: the rewritten
// hashes of the instruction parts they hold, and the parts in force on
// its path.
func (r *Rewriter) Rewrite(e Entry) (Entry, error) {
	if e == nil {
		return nil, errors.New("agentsession: nil entry")
	}
	var (
		out Entry
		err error
	)
	if u, ok := e.(*UnknownEntry); ok {
		out, err = r.rewriteUnknown(u)
	} else {
		out, err = r.rewriteCore(e)
	}
	if err != nil {
		return nil, fmt.Errorf("agentsession: entry %s: %w", e.Base().ID, err)
	}
	if c, ok := e.(*ConfigEntry); ok {
		r.checkParts(c)
	}
	r.record(e)
	return out, nil
}

// rewriteCore is Rewrite for a core entry.
func (r *Rewriter) rewriteCore(e Entry) (Entry, error) {
	root, err := entryJSON(e)
	if err != nil {
		return nil, err
	}
	specials := specialsOf(e)
	lists, hasParts := partLists[e.EntryType()]
	instr, rebuilt := "", false
	if hasParts {
		if instr, rebuilt, err = r.joinedInstructions(e); err != nil {
			return nil, err
		}
		if rebuilt {
			specials = append(specials, special{path: lists[1], role: roleFixed})
		}
	}
	out, err := r.rewriteObject(root, specials, entryShape(e), false, true)
	if err != nil {
		return nil, err
	}
	if hasParts {
		r.rewriteParts(root, out, lists[0])
		if rebuilt {
			if obj, ok := lookup(out, lists[1][:len(lists[1])-1]).(map[string]any); ok {
				obj[lists[1][len(lists[1])-1]] = instr
			}
		}
	}
	return entryFromJSON(e, out)
}

// record replays e into the settings in force after it: a compaction's
// checkpoint starts them afresh, a config entry applies to those of its
// parent, and any other entry has its parent's.
func (r *Rewriter) record(e Entry) {
	id := e.Base().ID
	if id == "" {
		return
	}
	prev, _ := r.settingsAt(e.Base().Parent)
	switch v := e.(type) {
	case *CompactionEntry:
		r.settings[id] = checkpointSettings(v)
	case *ConfigEntry:
		r.settings[id] = prev.Apply(v)
	default:
		r.settings[id] = prev
	}
}

// settingsAt returns the settings in force after the entry id, and
// whether the Rewriter met it: the universal root, a first entry's
// parent, has none in force.
func (r *Rewriter) settingsAt(id string) (Settings, bool) {
	if id == "" {
		return Settings{}, true
	}
	s, ok := r.settings[id]
	return s, ok
}

// joinedInstructions decides how e's instructions are rewritten when it
// carries them beside parts. When they are the join of the parts as the
// path resolves them, it returns the join of the rewritten parts and
// rebuilt; when they are not, f is to be given them whole. When a part
// cannot be resolved, it returns them unchanged and rebuilt if f leaves
// them, and ErrInstructionsJoin if f would change them.
func (r *Rewriter) joinedInstructions(e Entry) (string, bool, error) {
	var (
		instr string
		parts []InstructionPart
	)
	switch v := e.(type) {
	case *ConfigEntry:
		if v.Instructions == nil || len(v.InstructionsParts) == 0 {
			return "", false, nil
		}
		prev, _ := r.settingsAt(v.Parent)
		instr, parts = *v.Instructions, prev.Apply(v).InstructionsParts
	case *CompactionEntry:
		if v.Config.Instructions == "" || len(v.Config.InstructionsParts) == 0 {
			return "", false, nil
		}
		instr, parts = v.Config.Instructions, v.Config.InstructionsParts
	default:
		return "", false, nil
	}
	if unresolvedParts(parts) {
		if r.f(instr) != instr {
			return "", false, ErrInstructionsJoin
		}
		return instr, true, nil
	}
	if JoinInstructions(parts) != instr {
		return "", false, nil
	}
	texts := make([]string, len(parts))
	for i, p := range parts {
		if p.Text != "" {
			texts[i] = r.f(p.Text)
		}
	}
	return strings.Join(texts, PartSeparator), true, nil
}

// rewriteUnknown is Rewrite for an extension entry: its line is written
// from its envelope first, as a commit writes it, and every member but
// the envelope's is rewritten.
func (r *Rewriter) rewriteUnknown(u *UnknownEntry) (Entry, error) {
	cp := *u
	cp.Parents = append([]EntryRef(nil), u.Parents...)
	cp.content = ""
	raw, err := rewriteEnvelope(&cp)
	if err != nil {
		return nil, err
	}
	v, err := parseValue(raw)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("agentsession: unknown entry is not an object")
	}
	rewritten, err := r.rewriteObject(root, specialsOf(u), shape{data: true}, true, true)
	if err != nil {
		return nil, err
	}
	data, err := jsonx.MarshalNoEscape(rewritten)
	if err != nil {
		return nil, err
	}
	all, err := splitMembers(data)
	if err != nil {
		return nil, err
	}
	if err := cp.decodeMembers(data, all); err != nil {
		return nil, err
	}
	cp.tsRaw = u.tsRaw
	keepRedactedForm(u, &cp)
	return &cp, nil
}

// rewriteObject returns a copy of an entry's top-level object, or a
// header's, with f applied to its body strings, the members specials
// names left as they are and, with normalised, a normalised member
// rewritten as its records need. top is what the format defines of the
// object; with rename, the object is an extension entry's, whose names
// but the envelope's are data and are given to f. Members are taken in
// sorted order, so f is called in one order.
func (r *Rewriter) rewriteObject(root map[string]any, specials []special, top shape, rename, normalised bool) (map[string]any, error) {
	out := make(map[string]any, len(root))
	from := map[string]string{}
	for _, k := range sortedKeys(root) {
		v := root[k]
		if normalised && k == "normalised" {
			out[k] = r.rewriteNormalised(v, root, specials, top, rename)
			from[k] = k
			continue
		}
		name, sh := k, top.member(k)
		if rename && !reservedName[k] {
			name, sh = r.f(k), shape{data: true}
		}
		if was, taken := from[name]; taken || name != k && reservedName[name] {
			if !taken {
				was = name
			}
			return nil, fmt.Errorf("%w: %q and %q are both %q", ErrKeyCollision, was, k, name)
		}
		from[name] = k
		w, err := r.walk(v, onward(specials, 0, k, false), 1, root, sh)
		if err != nil {
			return nil, err
		}
		out[name] = w
	}
	return out, nil
}

// reservedName holds the members of an extension entry whose names are
// the format's: the envelope and the body members any entry may carry.
var reservedName = func() map[string]bool {
	m := map[string]bool{}
	for _, k := range envelopeKeys {
		m[k] = true
	}
	for _, k := range commonBodyKeys {
		m[k] = true
	}
	m["redacted"] = true
	return m
}()

// onward returns the specials whose path continues past depth through the
// member key, or through an element when elem is set.
func onward(specials []special, depth int, key string, elem bool) []special {
	var out []special
	for _, sp := range specials {
		if len(sp.path) <= depth {
			continue
		}
		s := sp.path[depth]
		if elem && s == "[]" || !elem && (s == "*" || s == key) {
			out = append(out, sp)
		}
	}
	return out
}

// ending reports whether a special of one of roles ends at depth, at a
// member of holder it applies to.
func ending(specials []special, depth int, holder map[string]any, roles ...memberRole) bool {
	for _, sp := range specials {
		if len(sp.path) == depth && slices.Contains(roles, sp.role) && sp.applies(holder) {
			return true
		}
	}
	return false
}

// walk returns a copy of v, a member of holder or an element when holder
// is nil, with f applied to its body strings. specials are the members
// whose path has led here, depth steps down, and sh is what the format
// defines of v. A special that ends here leaves v as it is, wholly when
// it is fixed and as a string otherwise, since a reference, a hash or a
// call_id in another form names nothing. f is applied to the member
// names of an object whose names are data, a special's or the shape's
// saying so. Members are taken in sorted order and elements in index
// order.
func (r *Rewriter) walk(v any, specials []special, depth int, holder map[string]any, sh shape) (any, error) {
	if ending(specials, depth, holder, roleFixed) {
		return v, nil
	}
	if _, ok := v.(string); ok && ending(specials, depth, holder, roleEntry, roleSession, roleHash, roleCallID) {
		return v, nil
	}
	switch x := v.(type) {
	case string:
		if isSidecarURL(x) {
			return x, nil
		}
		return r.f(x), nil
	case map[string]any:
		dataKeys := ending(specials, depth, holder, roleDataKeys)
		out := make(map[string]any, len(x))
		from := map[string]string{}
		for _, k := range sortedKeys(x) {
			name := k
			if dataKeys || sh.dataName(k) {
				name = r.f(k)
				if was, taken := from[name]; taken {
					return nil, fmt.Errorf("%w: %q and %q are both %q", ErrKeyCollision, was, k, name)
				}
				from[name] = k
			}
			w, err := r.walk(x[k], onward(specials, depth, k, false), depth+1, x, sh.member(k))
			if err != nil {
				return nil, err
			}
			out[name] = w
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		next := onward(specials, depth, "", true)
		for i, child := range x {
			w, err := r.walk(child, next, depth+1, nil, sh.elem())
			if err != nil {
				return nil, err
			}
			out[i] = w
		}
		return out, nil
	}
	return v, nil
}

// shape is what the format defines of a JSON value a rewrite walks: the
// Go type that decodes it, or, with data set, nothing, a value whose
// member names are data at every depth. A zero shape is a value whose
// names are not data and whose members are not known, an item's, which
// the payload profile defines, among them.
type shape struct {
	t    reflect.Type
	data bool
	// paid is a custom entry's data, whose names are data but for those
	// RFC 0001 defines at its top level, usage, model and attempts; the
	// usage is the payload profile's.
	paid bool
	// request is a config's or a checkpoint's extra, keyed by request
	// member names, which the provider reads, and whose values are data.
	request bool
}

// paidNames are the members RFC 0001 defines at the top level of a
// custom entry's data, beside the writer's own.
var paidNames = map[string]bool{"usage": true, "model": true, "attempts": true}

var (
	rawJSONType  = reflect.TypeOf(json.RawMessage(nil))
	headerGoType = reflect.TypeOf(Header{})
	customType   = reflect.TypeOf(CustomEntry{})
	configType   = reflect.TypeOf(ConfigEntry{})
	settingsType = reflect.TypeOf(Settings{})
	payloadPkg   = reflect.TypeOf(openresponses.Usage{}).PkgPath()
	ownPkg       = reflect.TypeOf(Header{}).PkgPath()
)

// entryShape is the shape of a core entry's top-level object.
func entryShape(e Entry) shape { return shape{t: reflect.TypeOf(e)} }

// resolve follows pointers, and makes a JSON value of any form, a raw
// message or an interface, data.
func (s shape) resolve() shape {
	if s.data || s.paid || s.request || s.t == nil {
		return s
	}
	t := s.t
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.PkgPath() == payloadPkg {
		return shape{} // an item or another payload value: the profile's names
	}
	if t == rawJSONType || t.Kind() == reflect.Interface {
		return shape{data: true}
	}
	return shape{t: t}
}

// dataName reports whether the member name of an object of this shape
// is data: in a map keyed by data, or a value the format does not
// define, but for the names a custom entry's data and a config's extra
// hold that are the format's or the request's.
func (s shape) dataName(name string) bool {
	s = s.resolve()
	switch {
	case s.paid:
		return !paidNames[name]
	case s.request:
		return false
	}
	return s.data || s.t != nil && s.t.Kind() == reflect.Map
}

// member is the shape of the member name of an object of this shape. A
// member the format does not define, of an object it does, is data.
func (s shape) member(name string) shape {
	s = s.resolve()
	switch {
	case s.data:
		return s
	case s.paid && name == "usage":
		return shape{}
	case s.paid, s.request:
		return shape{data: true}
	case s.t == nil:
		return shape{}
	case s.t == customType && name == "data":
		return shape{paid: true}
	case (s.t == configType || s.t == settingsType) && name == "extra":
		return shape{request: true}
	case s.t.Kind() == reflect.Map:
		return shape{t: s.t.Elem()}.resolve()
	case s.t.Kind() == reflect.Struct && s.t.PkgPath() == ownPkg:
		if t, ok := structMembers(s.t)[name]; ok {
			return shape{t: t}.resolve()
		}
		return shape{data: true}
	}
	return shape{}
}

// elem is the shape of an element of an array of this shape.
func (s shape) elem() shape {
	s = s.resolve()
	switch {
	case s.data, s.paid, s.request:
		return shape{data: true}
	case s.t != nil && (s.t.Kind() == reflect.Slice || s.t.Kind() == reflect.Array):
		return shape{t: s.t.Elem()}.resolve()
	}
	return shape{}
}

// structMembers maps each member name a struct of this package defines
// to the type of the field holding it: a field's json name, or the
// member tag of a field decoded by hand; an entry's envelope and the
// body members every entry may carry, which no field holds, map to nil,
// a value whose names are not data. Unknown and the other fields json
// leaves out hold members the format does not define, and are not
// named.
func structMembers(t reflect.Type) map[string]reflect.Type {
	if v, ok := structMembersCache.Load(t); ok {
		return v.(map[string]reflect.Type)
	}
	names := map[string]reflect.Type{}
	if reflect.PointerTo(t).Implements(entryInterface) {
		for _, k := range envelopeKeys {
			names[k] = nil
		}
		for _, k := range commonBodyKeys {
			names[k] = nil
		}
		names["redacted"] = nil
	}
	if t == headerGoType {
		names["type"], names["redacted_from"] = nil, nil
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous || !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("json")
		if tag == "-" {
			if m := f.Tag.Get("member"); m != "" {
				names[m] = f.Type
			}
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			name = f.Name
		}
		names[name] = f.Type
	}
	structMembersCache.Store(t, names)
	return names
}

var (
	structMembersCache sync.Map
	entryInterface     = reflect.TypeOf((*Entry)(nil)).Elem()
)

// isSidecarURL reports whether s is a sidecar media reference, the URL
// of a blob beside the file named by its hash.
func isSidecarURL(s string) bool {
	h, ok := strings.CutPrefix(s, "sidecar:")
	return ok && ValidHash(h)
}

// jsonNumber matches a JSON number token.
var jsonNumber = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?$`)

// rewriteNormalised rewrites the records of a normalised member of the
// entry root, each as holding the member it describes: at is a pointer
// of member names, rewritten where it passes through a member named by
// data; was is JSON text whose string values are rewritten, or a number
// given to f as its digits, and raw a string's bytes in base64,
// rewritten decoded, unless at names a member that is not body text;
// anything else is body. The records are taken in order, each one's
// members sorted, and sorted again when an at changed.
func (r *Rewriter) rewriteNormalised(v any, root map[string]any, specials []special, top shape, rename bool) any {
	list, ok := v.([]any)
	if !ok {
		w, _ := r.walk(v, nil, 1, nil, shape{})
		return w
	}
	out := make([]any, len(list))
	moved := false
	for i, el := range list {
		rec, ok := el.(map[string]any)
		if !ok {
			out[i], _ = r.walk(el, nil, 2, nil, shape{})
			continue
		}
		body, newAt := true, ""
		if at, ok := rec["at"].(string); ok {
			newAt, body = r.rewriteAt(at, root, specials, top, rename)
			moved = moved || newAt != at
		}
		cp := make(map[string]any, len(rec))
		for _, k := range sortedKeys(rec) {
			x := rec[k]
			s, isString := x.(string)
			switch {
			case k == "at" && isString:
				cp[k] = newAt
			case k == "was" && isString:
				if body {
					s = r.rewriteWas(s)
				}
				cp[k] = s
			case k == "raw" && isString:
				if b, err := base64.StdEncoding.Strict().DecodeString(s); body && err == nil {
					if text, changed := rewriteJSONText(b, r.f, true); changed {
						s = base64.StdEncoding.EncodeToString(text)
					}
				}
				cp[k] = s
			default:
				cp[k], _ = r.walk(x, nil, 3, nil, shape{})
			}
		}
		out[i] = cp
	}
	if moved {
		at := func(v any) string { s, _ := v.(map[string]any)["at"].(string); return s }
		sort.SliceStable(out, func(i, j int) bool { return jcs.LessUTF16(at(out[i]), at(out[j])) })
	}
	return out
}

// rewriteWas rewrites a was record: a number, the digits of a member
// carried as a string, is given to f as those digits, and any other
// text has f applied to its string values.
func (r *Rewriter) rewriteWas(was string) string {
	if jsonNumber.MatchString(was) {
		if t := r.f(was); t != was {
			return string(quoteLoose(t, false))
		}
		return was
	}
	if text, changed := rewriteJSONText([]byte(was), r.f, false); changed {
		return string(text)
	}
	return was
}

// rewriteAt returns the pointer at into the entry root with f applied to
// each token that is a member name the format keeps data in, and whether
// the member it names is body text: not inside a fixed member, and not a
// reference, a hash or a call_id where the format defines one.
func (r *Rewriter) rewriteAt(at string, root map[string]any, specials []special, top shape, rename bool) (string, bool) {
	if !strings.HasPrefix(at, "/") {
		return at, true
	}
	tokens := strings.Split(at[1:], "/")
	for i, t := range tokens {
		tokens[i] = strings.ReplaceAll(strings.ReplaceAll(t, "~1", "/"), "~0", "~")
	}
	var (
		cur        = specials
		node   any = root
		holder map[string]any
		sh     = top
	)
	for i, t := range tokens {
		if ending(cur, i, holder, roleFixed) {
			return at, false
		}
		var next []special
		switch n := node.(type) {
		case map[string]any:
			child := sh.member(t)
			switch {
			case i == 0 && rename && !reservedName[t]:
				tokens[i], child = r.f(t), shape{data: true}
			case i > 0 && (ending(cur, i, holder, roleDataKeys) || sh.dataName(t)):
				tokens[i] = r.f(t)
			}
			next, holder, node, sh = onward(cur, i, t, false), n, n[t], child
		case []any:
			k, err := strconv.Atoi(t)
			next, holder, node, sh = onward(cur, i, "", true), nil, nil, sh.elem()
			if err == nil && k >= 0 && k < len(n) {
				node = n[k]
			}
		default:
			next, holder, node, sh = nil, nil, nil, shape{}
		}
		cur = next
	}
	body := !ending(cur, len(tokens), holder, roleFixed, roleEntry, roleSession, roleHash, roleCallID)
	for i, t := range tokens {
		tokens[i] = pointerToken(t)
	}
	return "/" + strings.Join(tokens, "/"), body
}

// rewriteParts gives each instruction part in after, the rewritten
// copy of before, the hash of its rewritten text: one carrying its text
// and a hash has the hash recomputed when the text changed, and one
// named by a hash alone takes the hash of the rewritten text it names
// when an earlier entry's part held that text.
func (r *Rewriter) rewriteParts(before, after map[string]any, partsPath []string) {
	oldList, _ := lookup(before, partsPath).([]any)
	newList, _ := lookup(after, partsPath).([]any)
	if oldList == nil || len(oldList) != len(newList) {
		return
	}
	for i := range oldList {
		o, _ := oldList[i].(map[string]any)
		n, _ := newList[i].(map[string]any)
		ot, ok1 := o["text"].(string)
		nt, ok2 := n["text"].(string)
		if !ok1 || !ok2 {
			continue
		}
		r.parts[HashText(ot)] = HashText(nt)
		if _, ok := n["hash"].(string); ok && ot != nt {
			n["hash"] = HashText(nt)
		}
	}
	for i := range newList {
		n, _ := newList[i].(map[string]any)
		if _, hasText := n["text"]; hasText {
			continue
		}
		if h, ok := n["hash"].(string); ok {
			if to, ok := r.parts[h]; ok {
				n["hash"] = to
			}
		}
	}
}

// lookup returns the value a path of member names reaches in v, nil if
// none.
func lookup(v any, path []string) any {
	for _, k := range path {
		obj, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = obj[k]
	}
	return v
}

// rewriteJSONText applies f to each string value in src, JSON text,
// leaving member names, every other token and every string f leaves
// unchanged as written. A string f is given has its escapes resolved, a
// lone surrogate as the three bytes its code unit would encode to, and
// is written back canonically, such a surrogate escaped again; with
// keepInvalid, a byte that is not UTF-8 is written as it is, and
// otherwise as U+FFFD. It reports whether any string changed.
func rewriteJSONText(src []byte, f func(string) string, keepInvalid bool) ([]byte, bool) {
	type frame struct{ object, expectKey bool }
	var (
		stack   []frame
		out     = make([]byte, 0, len(src))
		changed bool
	)
	for i := 0; i < len(src); {
		c := src[i]
		switch c {
		case '{':
			stack = append(stack, frame{object: true, expectKey: true})
		case '[':
			stack = append(stack, frame{})
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case ',':
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].expectKey = true
			}
		case '"':
			end := skipString(src, i)
			if end <= i {
				end = len(src)
			}
			tok := src[i:end]
			i = end
			if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].expectKey {
				stack[n-1].expectKey = false
				out = append(out, tok...)
				continue
			}
			s := unquoteLoose(tok)
			if t := f(s); t != s {
				out = append(out, quoteLoose(t, keepInvalid)...)
				changed = true
				continue
			}
			out = append(out, tok...)
			continue
		}
		out = append(out, c)
		i++
	}
	return out, changed
}

// unquoteLoose decodes a JSON string token, quotes included, without
// repairing it: escapes are resolved, a lone surrogate escape becomes
// the three bytes the code unit would encode to, and every other byte,
// valid UTF-8 or not, is kept.
func unquoteLoose(tok []byte) string {
	s := tok
	if len(s) > 0 && s[0] == '"' {
		s = s[1:]
	}
	if len(s) > 0 && s[len(s)-1] == '"' {
		s = s[:len(s)-1]
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			out = append(out, s[i])
			continue
		}
		if u, ok := surrogateAt(s, i); ok {
			i += 5
			if u >= 0xD800 && u <= 0xDBFF {
				if lo, ok := surrogateAt(s, i+1); ok && lo >= 0xDC00 && lo <= 0xDFFF {
					out = utf8.AppendRune(out, rune(0x10000+(u-0xD800)<<10+(lo-0xDC00)))
					i += 6
					continue
				}
			}
			if u >= 0xD800 && u <= 0xDFFF {
				out = append(out, 0xE0|byte(u>>12), 0x80|byte(u>>6)&0x3F, 0x80|byte(u)&0x3F)
				continue
			}
			out = utf8.AppendRune(out, rune(u))
			continue
		}
		i++
		switch s[i] {
		case '"', '\\', '/':
			out = append(out, s[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		default:
			out = append(out, '\\', s[i])
		}
	}
	return string(out)
}

// quoteLoose writes s as a JSON string in canonical form: the
// two-character escapes, \u00xx for the other control characters, every
// other character as it is, and the three bytes of a surrogate code
// unit, which unquoteLoose makes of a lone surrogate escape, escaped as
// that code unit. A byte that is not UTF-8 is written as it is with
// keepInvalid, and each maximal subpart of one as U+FFFD otherwise.
func quoteLoose(s string, keepInvalid bool) []byte {
	out := make([]byte, 0, len(s)+2)
	out = append(out, '"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			switch c {
			case '"':
				out = append(out, '\\', '"')
			case '\\':
				out = append(out, '\\', '\\')
			case '\b':
				out = append(out, '\\', 'b')
			case '\f':
				out = append(out, '\\', 'f')
			case '\n':
				out = append(out, '\\', 'n')
			case '\r':
				out = append(out, '\\', 'r')
			case '\t':
				out = append(out, '\\', 't')
			default:
				if c < 0x20 {
					out = fmt.Appendf(out, `\u%04x`, c)
				} else {
					out = append(out, c)
				}
			}
			i++
			continue
		}
		if c == 0xED && i+2 < len(s) && s[i+1] >= 0xA0 && s[i+1] <= 0xBF && s[i+2] >= 0x80 && s[i+2] <= 0xBF {
			u := 0xD000 | uint32(s[i+1]&0x3F)<<6 | uint32(s[i+2]&0x3F)
			out = fmt.Appendf(out, `\u%04x`, u)
			i += 3
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size <= 1 {
			if keepInvalid {
				out = append(out, c)
				i++
				continue
			}
			out = utf8.AppendRune(out, utf8.RuneError)
			i += maximalSubpart([]byte(s[i:]))
			continue
		}
		out = append(out, s[i:i+size]...)
		i += size
	}
	return append(out, '"')
}
