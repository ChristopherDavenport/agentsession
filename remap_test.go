package agentsession

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// entryOf decodes one entry line, failing the test if it does not.
func entryOf(t *testing.T, line string) Entry {
	t.Helper()
	e, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatalf("UnmarshalEntry %s: %v", line, err)
	}
	return e
}

// canonicalEntry is e as MarshalEntry writes it, canonicalised.
func canonicalEntry(t *testing.T, e Entry) string {
	t.Helper()
	data, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	c, err := jcs.Transform(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

// withValues is line canonicalised with each pointer in set given its
// value, every pointer naming a string that is there.
func withValues(t *testing.T, line string, set map[string]string) string {
	t.Helper()
	v, err := parseValue([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	for ptr, val := range set {
		steps := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
		cur := v
		for i, s := range steps {
			last := i == len(steps)-1
			switch c := cur.(type) {
			case map[string]any:
				if _, ok := c[s].(string); last && !ok {
					t.Fatalf("%s: no string at %s", line, ptr)
				}
				if last {
					c[s] = val
				}
				cur = c[s]
			case []any:
				n, err := strconv.Atoi(s)
				if err != nil || n >= len(c) {
					t.Fatalf("%s: no element at %s", line, ptr)
				}
				if last {
					c[n] = val
				}
				cur = c[n]
			default:
				t.Fatalf("%s: nothing at %s", line, ptr)
			}
		}
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	c, err := jcs.Transform(data)
	if err != nil {
		t.Fatal(err)
	}
	return string(c)
}

// prefixRemap maps every entry ID starting o- to n- and every session ID
// starting s- to t-, and leaves the rest.
var prefixRemap = Remap{
	Entry: func(_, entry string) (string, bool) {
		rest, ok := strings.CutPrefix(entry, "o-")
		return "n-" + rest, ok
	},
	Session: func(id string) (string, bool) {
		rest, ok := strings.CutPrefix(id, "s-")
		return "t-" + rest, ok
	},
}

const tsLine = `"ts":"2026-10-16T09:00:00Z"`

// TestRemapRefsEveryMember: every member RFC 0001 has name an entry or a
// session is re-pointed, kept extras beside it stay where they were, and
// nothing else in the entry changes, a member in a form or on a relation
// that does not define it least of all.
func TestRemapRefsEveryMember(t *testing.T) {
	msg := `{"type":"message","role":"user","content":[{"type":"input_text","text":"o-text"}]}`
	for _, tt := range []struct {
		name, line string
		// moved maps each pointer that must be re-pointed to its new value.
		moved  map[string]string
		opaque bool
	}{
		{
			"item: parent, parents with an extra, queued_from",
			`{"type":"item","id":"x","parent":"o-p",` + tsLine + `,"parents":[{"entry":"o-a"},{"entry":"o-b","session":"s-1","x:note":"o-keep"}],"item":` + msg + `,"response":"o-resp","queued_from":"o-q","x:ref":"o-unknown"}`,
			map[string]string{"/parent": "n-p", "/parents/0/entry": "n-a", "/parents/1/entry": "n-b", "/parents/1/session": "t-1", "/queued_from": "n-q"},
			false,
		},
		{
			"config: omit items and omitted parts' of",
			`{"type":"config","id":"x","parent":"o-p",` + tsLine + `,"omit":{"reasoning":"other_models","items":["o-1","o-2"]},"instructions_omitted":[{"keep":2,"of":"o-3","x:note":"o-keep"},{"id":"o-name","reason":"budget","size":1}],"instructions_parts":[{"id":"o-part","hash":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}]}`,
			map[string]string{"/parent": "n-p", "/omit/items/0": "n-1", "/omit/items/1": "n-2", "/instructions_omitted/0/of": "n-3"},
			false,
		},
		{
			"config: an omit in another form names nothing",
			`{"type":"config","id":"x","parent":null,` + tsLine + `,"omit":{"items":["o-1"],"reasoning":5}}`,
			nil,
			false,
		},
		{
			"compaction: first_kept and the checkpoint's omit",
			`{"type":"compaction","id":"x","parent":"o-p",` + tsLine + `,"first_kept":"o-f","summary":` + msg + `,"config":{"model":"m","omit":{"items":["o-i"]},"instructions_omitted":[{"id":"o-name","reason":"r"}]}}`,
			map[string]string{"/parent": "n-p", "/first_kept": "n-f", "/config/omit/items/0": "n-i"},
			false,
		},
		{
			"branch_summary: from",
			`{"type":"branch_summary","id":"x","parent":"o-p",` + tsLine + `,"from":"o-f","summary":` + msg + `}`,
			map[string]string{"/parent": "n-p", "/from": "n-f"},
			false,
		},
		{
			"label: target",
			`{"type":"label","id":"x","parent":"o-p",` + tsLine + `,"target":"o-t","label":"o-label"}`,
			map[string]string{"/parent": "n-p", "/target": "n-t"},
			false,
		},
		{
			"outcome: target",
			`{"type":"outcome","id":"x","parent":"o-p",` + tsLine + `,"kind":"eval","target":"o-t","details":{"target":"o-d"}}`,
			map[string]string{"/parent": "n-p", "/target": "n-t"},
			false,
		},
		{
			"dispatch: target",
			`{"type":"dispatch","id":"x","parent":"o-p",` + tsLine + `,"call_id":"o-call","target":"o-t"}`,
			map[string]string{"/parent": "n-p", "/target": "n-t"},
			false,
		},
		{
			"decision: target",
			`{"type":"decision","id":"x","parent":"o-p",` + tsLine + `,"call_id":"o-call","target":"o-t","verdict":"proceed"}`,
			map[string]string{"/parent": "n-p", "/target": "n-t"},
			false,
		},
		{
			"judged_by link: target and session",
			`{"type":"link","id":"x","parent":"o-p",` + tsLine + `,"rel":"judged_by","session":"s-judge","target":"o-t"}`,
			map[string]string{"/parent": "n-p", "/target": "n-t", "/session": "t-judge"},
			false,
		},
		{
			"cancel notice: target and session",
			`{"type":"link","id":"x","parent":"o-p",` + tsLine + `,"rel":"subsession","session":"s-child","call_id":"o-call","target":"o-link","cancel":"sent"}`,
			map[string]string{"/parent": "n-p", "/target": "n-link", "/session": "t-child"},
			false,
		},
		{
			"subsession link: a target it does not define names nothing",
			`{"type":"link","id":"x","parent":"o-p",` + tsLine + `,"rel":"subsession","session":"s-child","call_id":"o-call","target":"o-t"}`,
			map[string]string{"/parent": "n-p", "/session": "t-child"},
			false,
		},
		{
			"fork_of link: session",
			`{"type":"link","id":"x","parent":"o-p",` + tsLine + `,"rel":"fork_of","session":"s-origin","target":"o-t"}`,
			map[string]string{"/parent": "n-p", "/session": "t-origin"},
			false,
		},
		{
			"peer link with no session",
			`{"type":"link","id":"x","parent":"o-p",` + tsLine + `,"rel":"peer","session":"","call_id":"o-call","url":"https://o-host"}`,
			map[string]string{"/parent": "n-p"},
			false,
		},
		{
			"extension entry: its envelope alone",
			`{"type":"x:thing","id":"x","parent":"o-p",` + tsLine + `,"parents":[{"entry":"o-a","session":"s-1"}],"x:ref":"o-inside"}`,
			map[string]string{"/parent": "n-p", "/parents/0/entry": "n-a", "/parents/0/session": "t-1"},
			true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := entryOf(t, tt.line)
			before := canonicalEntry(t, e)
			r, err := RemapRefs(e, prefixRemap)
			if err != nil {
				t.Fatal(err)
			}
			if got := canonicalEntry(t, e); got != before {
				t.Errorf("the input changed:\n%s\n%s", before, got)
			}
			want := withValues(t, before, tt.moved)
			if got := canonicalEntry(t, r.Entry); got != want {
				t.Errorf("remapped\n got %s\nwant %s", got, want)
			}
			if r.Opaque != tt.opaque {
				t.Errorf("Opaque = %v, want %v", r.Opaque, tt.opaque)
			}
			if len(r.Unmapped) > 0 {
				t.Errorf("Unmapped = %v, want none", r.Unmapped)
			}
			if b := r.Entry.Base(); b.ID != e.Base().ID || !b.Timestamp.Equal(e.Base().Timestamp) || b.ContentHash() != "" {
				t.Errorf("envelope: id %q ts %v content %q", b.ID, b.Timestamp, b.ContentHash())
			}
		})
	}
}

// TestRemapRefsUnmapped: a reference a callback leaves is written as it
// was and reported with its member, a reference of a kind with no
// callback is neither re-pointed nor reported, and an empty one names
// nothing.
func TestRemapRefsUnmapped(t *testing.T) {
	e := entryOf(t, `{"type":"item","id":"x","parent":"o-p",`+tsLine+`,"parents":[{"entry":"o-a"},{"entry":"x-b","session":"s-1"}],"item":{"type":"message","role":"user","content":[]},"queued_from":""}`)
	r, err := RemapRefs(e, Remap{Entry: prefixRemap.Entry})
	if err != nil {
		t.Fatal(err)
	}
	want := []Reference{{At: "/parents/1/entry", Session: "s-1", Entry: "x-b"}}
	if !reflect.DeepEqual(r.Unmapped, want) {
		t.Errorf("Unmapped = %+v, want %+v", r.Unmapped, want)
	}
	got := r.Entry.Base()
	if got.Parent != "n-p" || got.Parents[0] != (EntryRef{Entry: "n-a"}) || got.Parents[1] != (EntryRef{Session: "s-1", Entry: "x-b"}) {
		t.Errorf("parent %s parents %+v", got.Parent, got.Parents)
	}

	r, err = RemapRefs(e, Remap{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unmapped) > 0 || canonicalEntry(t, r.Entry) != canonicalEntry(t, e) {
		t.Errorf("an empty Remap changed %s or reported %v", canonicalEntry(t, r.Entry), r.Unmapped)
	}
}

// TestRemapRefsSortsParents: re-pointed parents are written in the order
// the format has a writer write them, and a migration, which reads and
// does not write, keeps the order the line had.
func TestRemapRefsSortsParents(t *testing.T) {
	e := entryOf(t, `{"type":"item","id":"x","parent":"p",`+tsLine+`,"parents":[{"entry":"o-a"},{"entry":"o-b","x:note":1}],"item":{"type":"message","role":"user","content":[]}}`)
	swap := Remap{Entry: func(_, entry string) (string, bool) {
		switch entry {
		case "o-a":
			return "z", true
		case "o-b":
			return "a", true
		}
		return "", false
	}}
	r, err := RemapRefs(e, swap)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := canonicalEntry(t, r.Entry), withValues(t, `{"type":"item","id":"x","parent":"p",`+tsLine+`,"parents":[{"entry":"a","x:note":1},{"entry":"z"}],"item":{"type":"message","role":"user","content":[]}}`, nil); got != want {
		t.Errorf("sorted\n got %s\nwant %s", got, want)
	}
	r, err = remapRefs(e, swap, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Entry.Base().Parents; got[0].Entry != "z" || got[1].Entry != "a" {
		t.Errorf("a migration reordered parents: %+v", got)
	}
}

// TestRemapRefsKeepsLine: an entry whose typed fields do not write its
// line back is written as its line, with the references changed, rather
// than as what the fields would add, once its id is cleared for a
// commit; parent re-pointed here is what keeps it so.
func TestRemapRefsKeepsLine(t *testing.T) {
	// A response the typed fields would give "response_id":"".
	line := `{"type":"response","id":"x","parent":"o-p",` + tsLine + `,"status":"completed","model":"m"}`
	e := entryOf(t, line)
	if e.Base().line == nil {
		t.Fatal("the fixture line is one the fields write back; pick another")
	}
	s := New(Header{ID: "s", Format: Format, Payload: Payload})
	root := entryOf(t, `{"type":"item","id":"y","parent":null,`+tsLine+`,"item":{"type":"message","role":"user","content":[]}}`)
	root.Base().ID = ""
	if _, err := s.Commit(root); err != nil {
		t.Fatal(err)
	}
	p := s.Leaf()
	r, err := RemapRefs(e, Remap{Entry: func(_, entry string) (string, bool) { return p, entry == "o-p" }})
	if err != nil {
		t.Fatal(err)
	}
	r.Entry.Base().ID = ""
	if _, err := s.Commit(r.Entry); err != nil {
		t.Fatal(err)
	}
	got := canonicalEntry(t, r.Entry)
	if strings.Contains(got, "response_id") {
		t.Errorf("the fields' encoding was written, not the line: %s", got)
	}
	if want := withValues(t, line, map[string]string{"/parent": p, "/id": r.Entry.Base().ID}); got != want {
		t.Errorf("committed\n got %s\nwant %s", got, want)
	}
}

// TestRemapHeaderRefs: base names an entry of this file, parent_session
// a session, and redacted_from the original, which stays.
func TestRemapHeaderRefs(t *testing.T) {
	h := Header{ID: "s", Base: "o-b", ParentSession: "s-parent", Redacted: true, RedactedFrom: &RedactedFrom{Session: "s-orig", Entries: 3}}
	got, unmapped := RemapHeaderRefs(h, prefixRemap)
	if got.Base != "n-b" || got.ParentSession != "t-parent" || got.RedactedFrom.Session != "s-orig" || len(unmapped) > 0 {
		t.Errorf("base %s parent_session %s redacted_from %s unmapped %v", got.Base, got.ParentSession, got.RedactedFrom.Session, unmapped)
	}
	if h.Base != "o-b" {
		t.Error("the input changed")
	}
	_, unmapped = RemapHeaderRefs(Header{Base: "x", ParentSession: "y"}, prefixRemap)
	want := []Reference{{At: "/base", Entry: "x"}, {At: "/parent_session", Session: "y"}}
	if !reflect.DeepEqual(unmapped, want) {
		t.Errorf("unmapped %+v, want %+v", unmapped, want)
	}
}

// TestMigrationRewritesEveryEntryNamingMember: a file before 0.5 holding
// the members later minors define as entry-naming has them rewritten too,
// as RFC 0001's versioning section has every entry-naming member
// rewritten, and one it cannot rewrite is unresolved.
func TestMigrationRewritesEveryEntryNamingMember(t *testing.T) {
	head := `{"type":"session","format":"agentsession/0.4","id":"01995b2a-0000-7000-8000-0000000000f1","created_at":"2026-09-17T12:00:00Z","payload":"openresponses/2026-04-24"}`
	lines := []string{
		head,
		`{"type":"config","id":"c1","parent":null,"ts":"2026-09-17T12:00:00Z","model":"m","instructions_omitted":[{"id":"a","reason":"r","size":1}]}`,
		`{"type":"item","id":"i1","parent":"c1","ts":"2026-09-17T12:00:01Z","item":{"type":"message","role":"user","content":[]}}`,
		`{"type":"config","id":"c2","parent":"i1","ts":"2026-09-17T12:00:02Z","omit":{"items":["i1"]},"instructions_omitted":[{"keep":1,"of":"c1"}]}`,
		`{"type":"link","id":"l1","parent":"c2","ts":"2026-09-17T12:00:03Z","rel":"judged_by","session":"judge","target":"i1"}`,
	}
	s, err := Read(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, unresolved := s.Migrated(); len(unresolved) > 0 {
		t.Fatalf("unresolved %v", unresolved)
	}
	c1, i1 := lid(t, s, "c1"), lid(t, s, "i1")
	e, _ := s.Entry(lid(t, s, "c2"))
	c2 := e.(*ConfigEntry)
	if c2.Omit.Items[0] != i1 || c2.InstructionsOmitted[0].Of != c1 {
		t.Errorf("omit %v omitted %+v", c2.Omit.Items, c2.InstructionsOmitted)
	}
	if l, _ := s.Entry(lid(t, s, "l1")); l.(*LinkEntry).Target != i1 {
		t.Errorf("judged_by target %s, want %s", l.(*LinkEntry).Target, i1)
	}

	lines[3] = `{"type":"config","id":"c2","parent":"i1","ts":"2026-09-17T12:00:02Z","omit":{"items":["nowhere"]}}`
	s, err = Read(strings.NewReader(strings.Join(lines[:4], "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, unresolved := s.Migrated(); !slices.Equal(unresolved, []string{"c2"}) {
		t.Errorf("unresolved %v, want [c2]", unresolved)
	}
}

// coreStringMembers are the members of each core type holding strings
// that are body text: what neither names an entry or a session nor holds
// a hash. With the table RemapRefs and RewriteStrings share, they
// classify every string member of every core type, at any depth through
// the package's own structs, so TestRewriteTableClassifiesEveryMember
// fails when a type gains one that neither lists. Paths are as the table
// writes them, joined by "/".
var coreStringMembers = map[string][]string{
	TypeItem:          {"response", "source/kind", "source/ref", "source/source"},
	TypeResponse:      {"response_id", "model", "status"},
	TypeConfig:        {"model", "instructions", "instructions_parts/[]/id", "instructions_parts/[]/text", "instructions_parts/[]/source", "instructions_omitted/[]/id", "instructions_omitted/[]/reason", "instructions_omitted/[]/source", "tools_removed/[]", "omit/reasoning"},
	TypeCompaction:    {"config/model", "config/instructions", "config/instructions_parts/[]/id", "config/instructions_parts/[]/text", "config/instructions_parts/[]/source", "config/instructions_omitted/[]/id", "config/instructions_omitted/[]/reason", "config/instructions_omitted/[]/source", "config/omit/reasoning"},
	TypeBranchSummary: {},
	TypeRun:           {"run_id", "phase", "source", "reason", "ref", "trigger/kind", "trigger/ref", "trigger/source"},
	TypeDispatch:      {"idempotency_key"},
	TypeDecision:      {"verdict", "by", "reason"},
	TypeQueued:        {"mode", "trigger/kind", "trigger/ref", "trigger/source", "ref"},
	TypeLabel:         {"label"},
	TypeInfo:          {"name"},
	TypeEnv:           {"cwd", "vcs/system", "vcs/revision", "tools/*", "workspace/kind", "workspace/ref"},
	TypeOutcome:       {"kind", "label"},
	TypeLink:          {"rel", "url", "task", "context", "cancel"},
	TypeCustom:        {"ns"},
}

// TestRewriteTableClassifiesEveryMember walks every core type's struct
// and the package's structs it holds, and requires each member holding a
// string to be classified: in the table of members that are not body
// text, or listed as body text above. A member added to a type is a
// failure here until whoever adds it says which it is, so a new
// entry-naming member cannot reach RewriteStrings as text or be missed
// by RemapRefs. Members of the payload profile's types (openresponses)
// are body text and are not walked.
func TestRewriteTableClassifiesEveryMember(t *testing.T) {
	pkg := reflect.TypeOf(Session{}).PkgPath()
	for name, mk := range coreEntries {
		typ := reflect.TypeOf(mk()).Elem()
		got := map[string]bool{}
		stringMembers(typ, nil, pkg, got)
		known := map[string]bool{}
		for _, sp := range bodyMembers[name] {
			m := strings.Join(sp.path, "/")
			if sp.role == roleDataKeys {
				// An object keyed by data: its keys are not a member
				// the walk finds, its values are.
				if !got[m+"/*"] {
					t.Errorf("%s: the table keys %s by data, and it is no object of the type", name, m)
				}
				continue
			}
			known[m] = true
		}
		listed := map[string]bool{}
		for _, m := range coreStringMembers[name] {
			listed[m] = true
			if known[m] {
				t.Errorf("%s: %s is both in the table and listed as body text", name, m)
			}
			if !got[m] {
				t.Errorf("%s: %s is listed as body text and is no member of the type", name, m)
			}
		}
		for _, m := range sortedSet(got) {
			if !known[m] && !listed[m] {
				t.Errorf("%s: member %s holds a string and is unclassified: add it to bodyMembers if it names an entry or a session or holds a hash, and to coreStringMembers if it is text", name, m)
			}
		}
		for m := range known {
			if !got[m] {
				t.Errorf("%s: the table names %s, which is no member of the type", name, m)
			}
		}
	}
}

// stringMembers adds to out the path of every member of typ that holds a
// string, walking the structs of package pkg.
func stringMembers(typ reflect.Type, path []string, pkg string, out map[string]bool) {
	switch typ.Kind() {
	case reflect.String:
		out[strings.Join(path, "/")] = true
		return
	case reflect.Pointer:
		stringMembers(typ.Elem(), path, pkg, out)
		return
	case reflect.Slice, reflect.Array:
		if typ == rawType {
			return // a JSON value, whose strings are text
		}
		stringMembers(typ.Elem(), append(path, "[]"), pkg, out)
		return
	case reflect.Map:
		stringMembers(typ.Elem(), append(path, "*"), pkg, out)
		return
	case reflect.Struct:
		if typ.PkgPath() != pkg {
			return
		}
	default:
		return
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Anonymous || !f.IsExported() {
			continue
		}
		name, ok := memberName(f)
		if !ok {
			continue // Unknown: members the format does not define
		}
		stringMembers(f.Type, append(slices.Clone(path), name), pkg, out)
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestRewriteTableCoversRFC: every member RFC 0001 lists as naming an
// entry or a session is one the table re-points, and the member it says
// keeps data in its member names is one the table keys by data. The lists are read
// from the RFC itself, so a member added there and not here fails.
func TestRewriteTableCoversRFC(t *testing.T) {
	rfc, err := os.ReadFile(filepath.Join("docs", "rfcs", "0001-agent-session-format.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(string(rfc)), " ")
	refs := map[string]bool{"base": true, "parent_session": true} // RemapHeaderRefs
	for _, sps := range append([][]special{envelopeMembers}, slices.Collect(maps.Values(bodyMembers))...) {
		for _, sp := range sps {
			if sp.role == roleEntry || sp.role == roleSession {
				for _, s := range sp.path {
					refs[s] = true
				}
			}
		}
	}
	for typ := range coreEntries {
		refs[typ] = true // "a `link`'s `session`"
	}
	tick := regexp.MustCompile("`([a-z_]+)`")
	for _, span := range [][2]string{
		{"Wherever this document has a member name an entry", "it names it by `id`"},
		{"rewrite `parent` and every entry-naming member", "once the prefix is hashed"},
		{"Then it rewrites the reference to the twin:", "A redaction of a whole store"},
	} {
		i := strings.Index(text, span[0])
		j := strings.Index(text[max(i, 0):], span[1])
		if i < 0 || j < 0 {
			t.Fatalf("RFC 0001 no longer says %q ... %q; find the list again", span[0], span[1])
		}
		for _, m := range tick.FindAllStringSubmatch(text[i:i+j], -1) {
			if !refs[m[1]] && m[1] != "id" {
				t.Errorf("RFC 0001 names %s as a reference (%q) and the table does not re-point it", m[1], span[0])
			}
		}
	}
	// The data the format keeps in member names, which RewriteStrings
	// reaches: the RFC names the type and the member, and the table keys
	// a member of that type by data.
	const keys = "MUST reach data the format keeps in member names:"
	i := strings.Index(text, keys)
	j := strings.Index(text[max(i, 0):], "MUST NOT both be kept")
	if i < 0 || j < 0 {
		t.Fatalf("RFC 0001 no longer says %q; find the rule again", keys)
	}
	names := tick.FindAllStringSubmatch(text[i:i+j], -1)
	if len(names) < 2 {
		t.Fatalf("the rule names no member: %s", text[i:i+j])
	}
	typ, member := names[0][1], names[1][1]
	found := false
	for _, sp := range bodyMembers[typ] {
		found = found || sp.role == roleDataKeys && sp.path[0] == member
	}
	if !found {
		t.Errorf("RFC 0001 keeps data in the member names of %s's %s and the table does not key it by data", typ, member)
	}
}
