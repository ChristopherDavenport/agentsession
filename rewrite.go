package agentsession

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

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
// whose instructions were its parts joined and are not after the
// rewrite: the function changed a string that spans two parts, or
// changes a part by what is beside it, and the entry would break the
// rule that the instructions are their parts joined with "\n\n".
var ErrInstructionsJoin = errors.New("agentsession: rewritten instructions are not the rewritten parts joined")

// Rewriter is [RewriteStrings] over the entries of a session, given to
// it in file order. An instruction part a delta does not repeat is
// named by the hash of its text, which an earlier entry holds; the
// Rewriter remembers what each text it rewrote hashes to now, so a later
// part naming it by hash names the rewritten text. A part named by a
// hash whose text the Rewriter has not met keeps its hash, which then
// names the original's text. The zero value is not usable; see
// [NewRewriter].
type Rewriter struct {
	f func(string) string
	// parts maps the hash of each part text met to the hash of what f
	// made of it.
	parts map[string]string
}

// NewRewriter returns a Rewriter applying f, under the contract
// [RewriteStrings] states.
func NewRewriter(f func(string) string) *Rewriter {
	return &Rewriter{f: f, parts: map[string]string{}}
}

// RewriteStrings returns a new entry with f applied to every string in
// e's body; e is not changed. That is every string the entry's JSON
// holds, at any depth: its typed members, those the format does not
// define (Unknown), the members a reader keeps as written beside a typed
// one (a workspace's host), an extension entry's own members, the items
// of an item, a compaction and a branch summary, inline media as its
// data URL, and the record of what a writer normalised. f is never
// given a member name, a number or a boolean; the envelope (type, id,
// parent, parents, ts) or legacy_id; a member naming an entry or a
// session, which [RemapRefs] re-points; a member holding a hash
// (request_hash, an instruction part's hash, an env entry's file
// hashes); or a sidecar media URL, sidecar:sha256: and the blob's hash,
// whose blob a redactor replaces under the replacement's hash, as the
// format has it.
//
// An instruction part whose text changed has its hash, where it carries
// one, recomputed. A part named by its hash alone has its text in an
// earlier entry, which RewriteStrings does not see; a writer rewriting
// a whole session uses a [Rewriter], which does. Instructions that were
// their parts joined must be after the rewrite, or the entry is refused
// with [ErrInstructionsJoin]. In normalised, each was is JSON source
// text, and f is applied to each string value in it, which is written
// again in canonical form where f changed it, a lone surrogate escaped
// as it was; each raw is the base64 of the original bytes of a string,
// which f is given decoded, its escapes resolved and an ill-formed
// sequence as the bytes it was, and which is encoded again with the
// ill-formed bytes kept. Without that the original value would survive
// in the record, and in raw no string rewrite would ever see it.
//
// f must be deterministic: one string gives one result wherever it
// appears, since a call_id, a response_id and a part's text are named
// across entries by their value, and a rewrite that differs between two
// entries breaks the names. A result that is not valid UTF-8 is
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

// Rewrite is [RewriteStrings] with what earlier entries' instruction
// parts were rewritten to.
func (r *Rewriter) Rewrite(e Entry) (Entry, error) {
	if e == nil {
		return nil, errors.New("agentsession: nil entry")
	}
	if u, ok := e.(*UnknownEntry); ok {
		return r.rewriteUnknown(u)
	}
	root, err := entryJSON(e)
	if err != nil {
		return nil, err
	}
	out := r.rewriteObject(root, specialsOf(e))
	if lists, ok := partLists[e.EntryType()]; ok {
		if err := r.rewriteParts(root, out, lists[0], lists[1]); err != nil {
			return nil, fmt.Errorf("agentsession: entry %s: %w", e.Base().ID, err)
		}
	}
	return entryFromJSON(e, out)
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
	data, err := jsonx.MarshalNoEscape(r.rewriteObject(root, specialsOf(u)))
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
// are and normalised rewritten as its records need.
func (r *Rewriter) rewriteObject(root map[string]any, specials []special) map[string]any {
	out := make(map[string]any, len(root))
	for k, v := range root {
		if k == "normalised" {
			out[k] = r.rewriteNormalised(v)
			continue
		}
		out[k] = r.walk(v, onward(specials, 0, k, false), 1)
	}
	return out
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

// walk returns a copy of v with f applied to its body strings. specials
// are the members whose path has led here, depth steps down; one that
// ends here leaves v as it is, wholly when it is fixed and as a string
// otherwise, since a reference or a hash in another form names nothing.
func (r *Rewriter) walk(v any, specials []special, depth int) any {
	for _, sp := range specials {
		if len(sp.path) != depth {
			continue
		}
		if sp.role == roleFixed {
			return v
		}
		if _, ok := v.(string); ok {
			return v
		}
	}
	switch x := v.(type) {
	case string:
		if isSidecarURL(x) {
			return x
		}
		return r.f(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			out[k] = r.walk(child, onward(specials, depth, k, false), depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		next := onward(specials, depth, "", true)
		for i, child := range x {
			out[i] = r.walk(child, next, depth+1)
		}
		return out
	}
	return v
}

// isSidecarURL reports whether s is a sidecar media reference, the URL
// of a blob beside the file named by its hash.
func isSidecarURL(s string) bool {
	h, ok := strings.CutPrefix(s, "sidecar:")
	return ok && ValidHash(h)
}

// rewriteNormalised rewrites the records of a normalised member: at is
// a pointer of member names and stays, was is JSON text whose string
// values are rewritten, raw is a string's bytes in base64, rewritten
// decoded, and anything else is body.
func (r *Rewriter) rewriteNormalised(v any) any {
	list, ok := v.([]any)
	if !ok {
		return r.walk(v, nil, 1)
	}
	out := make([]any, len(list))
	for i, el := range list {
		rec, ok := el.(map[string]any)
		if !ok {
			out[i] = r.walk(el, nil, 2)
			continue
		}
		cp := make(map[string]any, len(rec))
		for k, x := range rec {
			s, isString := x.(string)
			switch {
			case k == "at" && isString:
				cp[k] = s
			case k == "was" && isString:
				if text, changed := rewriteJSONText([]byte(s), r.f, false); changed {
					s = string(text)
				}
				cp[k] = s
			case k == "raw" && isString:
				if b, err := base64.StdEncoding.Strict().DecodeString(s); err == nil {
					if text, changed := rewriteJSONText(b, r.f, true); changed {
						s = base64.StdEncoding.EncodeToString(text)
					}
				}
				cp[k] = s
			default:
				cp[k] = r.walk(x, nil, 3)
			}
		}
		out[i] = cp
	}
	return out
}

// rewriteParts gives each instruction part in after, the rewritten
// copy of before, the hash of its rewritten text: one carrying its text
// and a hash has the hash recomputed when the text changed, and one
// named by a hash alone takes the hash of the rewritten text it names
// when an earlier entry's part held that text. It refuses instructions
// that were their parts joined and are not after the rewrite.
func (r *Rewriter) rewriteParts(before, after map[string]any, partsPath, instrPath []string) error {
	oldList, _ := lookup(before, partsPath).([]any)
	newList, _ := lookup(after, partsPath).([]any)
	if oldList == nil || len(oldList) != len(newList) {
		return nil
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
	oldInstr, ok1 := lookup(before, instrPath).(string)
	newInstr, ok2 := lookup(after, instrPath).(string)
	if !ok1 || !ok2 {
		return nil
	}
	oldJoin, ok := joinedTexts(oldList)
	if !ok || oldJoin != oldInstr {
		return nil // not their join before, or not one this entry can say
	}
	if newJoin, _ := joinedTexts(newList); newJoin != newInstr {
		return ErrInstructionsJoin
	}
	return nil
}

// joinedTexts returns the texts of a list of parts joined as the format
// joins them, when every part carries its text in the list: one with an
// id and neither a text nor a hash has empty text, and a part named by
// hash or by keep has its text elsewhere.
func joinedTexts(list []any) (string, bool) {
	texts := make([]string, 0, len(list))
	for _, el := range list {
		part, ok := el.(map[string]any)
		if !ok {
			return "", false
		}
		if t, ok := part["text"].(string); ok {
			texts = append(texts, t)
			continue
		}
		_, hash := part["hash"]
		_, keep := part["keep"]
		if id, ok := part["id"].(string); !ok || id == "" || hash || keep {
			return "", false
		}
		texts = append(texts, "")
	}
	return strings.Join(texts, "\n\n"), true
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
