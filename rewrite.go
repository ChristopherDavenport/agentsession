package agentsession

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

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
// the rewrite gives two member names that hold data the same name, two
// paths of an env entry's files made one, which the format forbids a
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
// hash, which then names the original's text. What it has met depends
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
// what a writer normalised. It is also the one place the format keeps
// data in member names: the paths an env entry's files are keyed by. Two
// paths the rewrite makes one are refused with [ErrKeyCollision].
//
// f is never given any other member name, a number or a boolean. A tool
// name keying an env entry's tools, an extra key in a config entry's
// settings, and a key inside a custom entry's data or an outcome's
// details, which may hold a task or a test name, are names a writer
// chose and stay as written, though the same string elsewhere, a tool's
// name in a config entry or a function call, is rewritten. Nor is f
// given the envelope (type, id, parent, parents, ts) or legacy_id; a
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
// [Rewriter] that met it. Instructions that are the join of their parts,
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
func RewriteStrings(e Entry, f func(string) string) (Entry, error) {
	return NewRewriter(f).Rewrite(e)
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
	out, err := r.rewriteObject(root, specials)
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
// whether the Rewriter met it: a root's parent has none in force.
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
	rewritten, err := r.rewriteObject(root, specialsOf(u))
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
	return &cp, nil
}

// rewriteObject returns a copy of an entry's top-level object with f
// applied to its body strings, the members specials names left as they
// are and normalised rewritten as its records need. Members are taken
// in sorted order, so f is called in one order.
func (r *Rewriter) rewriteObject(root map[string]any, specials []special) (map[string]any, error) {
	out := make(map[string]any, len(root))
	for _, k := range sortedKeys(root) {
		v := root[k]
		if k == "normalised" {
			out[k] = r.rewriteNormalised(v, root, specials)
			continue
		}
		w, err := r.walk(v, onward(specials, 0, k, false), 1, root)
		if err != nil {
			return nil, err
		}
		out[k] = w
	}
	return out, nil
}

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
// whose path has led here, depth steps down. One that ends here leaves v
// as it is, wholly when it is fixed and as a string otherwise, since a
// reference, a hash or a call_id in another form names nothing, except
// an object whose member names are data, whose names f is applied to as
// well. Members are taken in sorted order and elements in index order.
func (r *Rewriter) walk(v any, specials []special, depth int, holder map[string]any) (any, error) {
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
			if dataKeys {
				name = r.f(k)
				if was, taken := from[name]; taken {
					return nil, fmt.Errorf("%w: %q and %q are both %q", ErrKeyCollision, was, k, name)
				}
				from[name] = k
			}
			w, err := r.walk(x[k], onward(specials, depth, k, false), depth+1, x)
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
			w, err := r.walk(child, next, depth+1, nil)
			if err != nil {
				return nil, err
			}
			out[i] = w
		}
		return out, nil
	}
	return v, nil
}

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
func (r *Rewriter) rewriteNormalised(v any, root map[string]any, specials []special) any {
	list, ok := v.([]any)
	if !ok {
		w, _ := r.walk(v, nil, 1, nil)
		return w
	}
	out := make([]any, len(list))
	moved := false
	for i, el := range list {
		rec, ok := el.(map[string]any)
		if !ok {
			out[i], _ = r.walk(el, nil, 2, nil)
			continue
		}
		body, newAt := true, ""
		if at, ok := rec["at"].(string); ok {
			newAt, body = r.rewriteAt(at, root, specials)
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
				cp[k], _ = r.walk(x, nil, 3, nil)
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
func (r *Rewriter) rewriteAt(at string, root map[string]any, specials []special) (string, bool) {
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
	)
	for i, t := range tokens {
		if ending(cur, i, holder, roleFixed) {
			return at, false
		}
		if ending(cur, i, holder, roleDataKeys) {
			tokens[i] = r.f(t)
		}
		var next []special
		switch n := node.(type) {
		case map[string]any:
			next, holder, node = onward(cur, i, t, false), n, n[t]
		case []any:
			k, err := strconv.Atoi(t)
			next, holder, node = onward(cur, i, "", true), nil, nil
			if err == nil && k >= 0 && k < len(n) {
				node = n[k]
			}
		default:
			next, holder, node = nil, nil, nil
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
