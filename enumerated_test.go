package agentsession

import (
	"cmp"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// closedWords are values the format and Open Responses define for
// members drawn from a closed set, each of which also reads as a word a
// redactor may look for in text, and searching, which no set holds.
var closedWords = []string{
	"other_models", "searching",
	"user", "assistant", "developer", "system", "completed", "in_progress", "incomplete", "failed",
	"commentary", "final_answer", "message", "function_call_output", "function_call", "reasoning",
	"input_text", "input_image", "output_text", "summary_text", "url_citation", "function",
	"json_schema", "allowed_tools", "required", "auto", "high", "low", "max_output_tokens",
	"server_error", "flex", "start", "end", "input", "resume", "done", "interrupted", "steer",
	"followup", "proceed", "reject", "human", "policy", "local", "container", "subsession",
	"sent", "feedback", "eval",
}

// closedRedactor replaces every closed word wherever it appears in a
// string, as a redactor matching on words does.
func closedRedactor() func(string) string {
	words := slices.Clone(closedWords)
	slices.SortStableFunc(words, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	pairs := make([]string, 0, 2*len(words))
	for _, w := range words {
		pairs = append(pairs, w, "["+strings.ToUpper(w)+"]")
	}
	return strings.NewReplacer(pairs...).Replace
}

// TestRewriteKeepsEnumeratedMembers: a member whose value is drawn from
// a closed set the format or Open Responses defines, an item's type,
// role, status and phase, a content part's type and detail, a
// response's status, a config's reasoning and text settings, a run's
// phase, source and reason, a queued entry's mode, a decision's verdict
// and by, a workspace's kind, a link's rel and cancel and an outcome's
// kind among them, is not body text: a redactor that rewrites the words
// those values spell changes them where they are text and leaves them
// where they are members (RFC 0001, Every redacted entry, since 0.20).
func TestRewriteKeepsEnumeratedMembers(t *testing.T) {
	f := closedRedactor()
	// sink holds every member of an item the table fixes, the type and
	// detail of a part and the type of an annotation in each of content,
	// summary and output.
	part := `{"type":"output_text","text":"for the user","detail":"low","annotations":[{"type":"url_citation","url":"https://example.com/","title":"t","start_index":0,"end_index":1}]}`
	sink := `{"type":"message","role":"assistant","status":"completed","phase":"final_answer","content":[` + part + `],"summary":[` + part + `],"output":[` + part + `]}`
	sinkBody := func(at string) []string {
		return []string{at + "/content/0/text", at + "/summary/0/text", at + "/output/0/text"}
	}
	user := `{"type":"message","role":"user","status":"completed","content":[{"type":"input_text","text":"the user asked the assistant"},{"type":"input_image","image_url":"https://example.com/user.png","detail":"high"}]}`
	for _, tt := range []struct {
		name, line string
		// body are the pointers of the strings that are text, which f
		// rewrites; every other string stays as written.
		body []string
	}{
		{"user message", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":` + user + `}`,
			[]string{"/item/content/0/text", "/item/content/1/image_url"}},
		{"assistant message", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"message","role":"assistant","status":"incomplete","phase":"commentary","content":[{"type":"output_text","text":"completed for the user","annotations":[{"type":"url_citation","url":"https://example.com/assistant","title":"user guide","start_index":0,"end_index":1}]}]}}`,
			[]string{"/item/content/0/text", "/item/content/0/annotations/0/url", "/item/content/0/annotations/0/title"}},
		{"reasoning", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"reasoning","status":"in_progress","summary":[{"type":"summary_text","text":"the user wants it done"}]}}`,
			[]string{"/item/summary/0/text"}},
		{"function call", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"function_call","status":"completed","call_id":"c1","name":"user_lookup","arguments":"{\"q\":\"assistant\"}"}}`,
			[]string{"/item/name", "/item/arguments"}},
		{"function call output", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"function_call_output","status":"completed","call_id":"c1","output":[{"type":"input_text","text":"completed"}]}}`,
			[]string{"/item/output/0/text"}},
		{"a role no set holds", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"acme.user_note","role":"user's own","text":"user"}}`,
			[]string{"/item/role", "/item/text"}},
		{"response", `{"type":"response","id":"x","parent":null,` + tsLine + `,"response_id":"r1","model":"assistant-1","status":"failed","incomplete":{"reason":"max_output_tokens"},"error":{"type":"server_error","code":"c","message":"the user failed","param":null}}`,
			[]string{"/model", "/error/message"}},
		{"config", `{"type":"config","id":"x","parent":null,` + tsLine + `,"model":"m","reasoning":{"effort":"high","summary":"auto"},"text":{"format":{"type":"json_schema","name":"user_schema","schema":{}},"verbosity":"low"},"tools_added":[{"type":"function","name":"user_tool","description":"for the user","parameters":{}}],"extra":{"tool_choice":"required","truncation":"auto","service_tier":"flex","x":"user"}}`,
			[]string{"/text/format/name", "/tools_added/0/name", "/tools_added/0/description", "/extra/x"}},
		{"config tool choice", `{"type":"config","id":"x","parent":null,` + tsLine + `,"extra":{"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"user_tool"}]}}}`,
			[]string{"/extra/tool_choice/tools/0/name"}},
		{"compaction", `{"type":"compaction","id":"x","parent":"p",` + tsLine + `,"first_kept":"f","summary":` + user + `,"pinned":[{"type":"function_call","status":"completed","call_id":"c1","name":"user_lookup","arguments":"{}"}],"config":{"model":"m","reasoning":{"effort":"low"},"tools":[{"type":"function","name":"user_tool","parameters":{}}]}}`,
			[]string{"/summary/content/0/text", "/summary/content/1/image_url", "/pinned/0/name", "/config/tools/0/name"}},
		{"branch summary", `{"type":"branch_summary","id":"x","parent":"p",` + tsLine + `,"from":"f","summary":` + user + `}`,
			[]string{"/summary/content/0/text", "/summary/content/1/image_url"}},
		{"run start", `{"type":"run","id":"x","parent":null,` + tsLine + `,"run_id":"r","phase":"start","source":"input","ref":"user ping"}`,
			[]string{"/ref"}},
		{"run end", `{"type":"run","id":"x","parent":null,` + tsLine + `,"run_id":"r","phase":"end","reason":"interrupted","pending":[]}`,
			nil},
		{"queued", `{"type":"queued","id":"x","parent":null,` + tsLine + `,"item":` + user + `,"mode":"steer","trigger":{"kind":"user"}}`,
			[]string{"/item/content/0/text", "/item/content/1/image_url", "/trigger/kind"}},
		{"decision", `{"type":"decision","id":"x","parent":"p",` + tsLine + `,"call_id":"c1","target":"t","verdict":"reject","by":"human","reason":"the user said reject"}`,
			[]string{"/reason"}},
		{"env", `{"type":"env","id":"x","parent":null,` + tsLine + `,"cwd":"/home/user","workspace":{"kind":"container","ref":"user/image"}}`,
			[]string{"/cwd", "/workspace/ref"}},
		{"link", `{"type":"link","id":"x","parent":null,` + tsLine + `,"rel":"subsession","session":"s","call_id":"c1","task":"help the user"}`,
			[]string{"/task"}},
		{"cancel", `{"type":"link","id":"x","parent":null,` + tsLine + `,"rel":"subsession","session":"s","call_id":"c1","target":"t","cancel":"sent"}`,
			nil},
		{"outcome", `{"type":"outcome","id":"x","parent":null,` + tsLine + `,"kind":"feedback","label":"the user liked it"}`,
			[]string{"/label"}},
		{"every item member", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":` + sink + `}`,
			sinkBody("/item")},
		{"every queued item member", `{"type":"queued","id":"x","parent":null,` + tsLine + `,"item":` + sink + `,"mode":"followup"}`,
			sinkBody("/item")},
		{"every checkpoint member", `{"type":"compaction","id":"x","parent":"p",` + tsLine + `,"first_kept":"f","summary":` + sink + `,"pinned":[` + sink + `],"config":{"model":"m","reasoning":{"effort":"high","summary":"auto"},"text":{"format":{"type":"json_schema","name":"n","schema":{}},"verbosity":"low"},"tools":[{"type":"function","name":"n","parameters":{}}],"omit":{"reasoning":"other_models"},"extra":{"tool_choice":"auto","truncation":"auto","service_tier":"flex","include":["reasoning.encrypted_content"]}}}`,
			append(sinkBody("/summary"), sinkBody("/pinned/0")...)},
		{"every branch summary member", `{"type":"branch_summary","id":"x","parent":"p",` + tsLine + `,"from":"f","summary":` + sink + `}`,
			sinkBody("/summary")},
		{"omit's reasoning", `{"type":"config","id":"x","parent":null,` + tsLine + `,"omit":{"reasoning":"other_models"}}`,
			nil},
		{"include, a value no set holds beside one it does", `{"type":"config","id":"x","parent":null,` + tsLine + `,"extra":{"include":["reasoning.encrypted_content","message.input_image.image_url"]}}`,
			[]string{"/extra/include/1"}},
		{"a status no set holds", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"message","role":"assistant","status":"searching","content":[{"type":"output_text","text":"t","annotations":[]}]}}`,
			[]string{"/item/status"}},
		{"an extension part's type", `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"message","role":"user","content":[{"type":"acme.user_input","text":"the user"}]}}`,
			[]string{"/item/content/0/text"}},
		{"result output annotations", `{"type":"result","id":"x","parent":"p",` + tsLine + `,"call_id":"c1","target":"t","item":{"type":"function_call_output","status":"completed","call_id":"c1","output":[` + part + `]}}`,
			[]string{"/item/output/0/text"}},
		{"result", `{"type":"result","id":"x","parent":"p",` + tsLine + `,"call_id":"c1","target":"t","item":{"type":"function_call_output","status":"completed","call_id":"c1","output":"done for the user"}}`,
			[]string{"/item/output"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := parseValue([]byte(tt.line))
			if err != nil {
				t.Fatal(err)
			}
			set := map[string]string{}
			for _, ptr := range tt.body {
				s, ok := pointerValue(v, ptr).(string)
				if !ok {
					t.Fatalf("%s holds no string", ptr)
				}
				if f(s) == s {
					t.Fatalf("%s: %q holds no closed word, so the case shows nothing", ptr, s)
				}
				set[ptr] = f(s)
			}
			out, err := RewriteStrings(entryOf(t, tt.line), f)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := canonicalEntry(t, out), canonicalEntry(t, entryOf(t, withValues(t, tt.line, set))); got != want {
				t.Errorf("rewritten\n got %s\nwant %s", got, want)
			}
		})
	}
}

// TestRedactionKeepsOmitReasoning: omit's reasoning is drawn from a
// closed set, so a redactor matching "models" leaves other_models as
// written; the rule stays in force, the reasoning items of the other
// model stay out of every request, and each response's request_hash is
// the original's, nothing else in the fixture holding the word.
func TestRedactionKeepsOmitReasoning(t *testing.T) {
	orig := loadFixture(t, "omit")
	red := redact(t, orig, func(s string) string { return strings.ReplaceAll(s, "models", "[MODELS]") }, "p")
	origEntries := orig.Entries()
	hashes := 0
	for i, e := range red.back.Entries() {
		if c, ok := e.(*ConfigEntry); ok && c.Omit != nil && c.Omit.Reasoning != "" && c.Omit.Reasoning != OmitOtherModels {
			t.Errorf("entry %d: omit's reasoning rewritten to %q", i, c.Omit.Reasoning)
		}
		r, ok := e.(*ResponseEntry)
		if !ok {
			continue
		}
		if want := origEntries[i].(*ResponseEntry).RequestHash; want != "" {
			hashes++
			if r.RequestHash != want {
				t.Errorf("response %d: request_hash %s, the original's %s", i, r.RequestHash, want)
			}
		}
	}
	if hashes == 0 {
		t.Fatal("the fixture records no request_hash, so the test shows nothing")
	}
}

// TestTableNamesPayloadMembers: every step of a path the table fixes for
// a closed set is a member name the format or the payload profile
// defines, so a misspelt path, which no entry would ever match, fails
// here rather than leaving the member to a redactor.
func TestTableNamesPayloadMembers(t *testing.T) {
	names := map[string]bool{"[]": true, "*": true, "type": true}
	seen := map[reflect.Type]bool{}
	var collect func(reflect.Type)
	collect = func(typ reflect.Type) {
		for typ.Kind() == reflect.Pointer || typ.Kind() == reflect.Slice || typ.Kind() == reflect.Array || typ.Kind() == reflect.Map {
			typ = typ.Elem()
		}
		if typ.Kind() != reflect.Struct || seen[typ] {
			return
		}
		seen[typ] = true
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			if !f.IsExported() {
				continue
			}
			if name, ok := memberName(f); ok {
				// A field a payload type encodes by hand, untagged, is
				// written under its name in lower case.
				names[name], names[strings.ToLower(name)] = true, true
			}
			collect(f.Type)
		}
	}
	for _, mk := range coreEntries {
		collect(reflect.TypeOf(mk()))
	}
	for _, v := range []any{
		openresponses.Request{}, openresponses.Message{}, openresponses.FunctionCall{}, openresponses.FunctionCallOutput{},
		openresponses.ReasoningItem{}, openresponses.Compaction{}, openresponses.InputImage{}, openresponses.OutputText{},
		openresponses.URLCitation{}, openresponses.FunctionTool{}, openresponses.AllowedTools{}, openresponses.ToolReference{},
		openresponses.TextConfig{}, openresponses.TextFormat{}, openresponses.ReasoningConfig{},
	} {
		collect(reflect.TypeOf(v))
	}
	for typ, specials := range bodyMembers {
		for _, sp := range specials {
			if !sp.vocab {
				continue
			}
			for _, step := range sp.path {
				if !names[step] {
					t.Errorf("%s: the table fixes %s, and no type defines %q", typ, strings.Join(sp.path, "/"), step)
				}
			}
		}
	}
}
