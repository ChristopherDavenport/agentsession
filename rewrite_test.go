package agentsession

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

// hideSecret is the rewrite the tests redact with.
func hideSecret(s string) string { return strings.ReplaceAll(s, "SECRET", "gone") }

// sidecarURL is a sidecar media reference, whose hash a rewrite must not
// touch.
const sidecarURL = "sidecar:sha256:5ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2"

// plantedEntries hold SECRET in every place an entry can hold a string
// of its body: a typed member of each core type that has text, Unknown
// at the top and below, the extras a reader keeps beside a typed member,
// an extension entry's members, inline media, an instruction part, the
// was and raw of a normalised record, and a line the typed fields do not
// write back.
var plantedEntries = []struct{ name, line string }{
	{"item message, inline media, source, unknowns", `{"type":"item","id":"x","parent":"p",` + tsLine + `,"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"a SECRET","x:extra":"SECRET kept beside a typed member"},{"type":"input_image","image_url":"data:text/plain,SECRET"},{"type":"input_file","filename":"SECRET.txt","file_data":"data:text/plain;base64,U0VDUkVU SECRET"}]},"response":"resp-SECRET","source":{"kind":"human","ref":"SECRET-ref","x:when":"SECRET"},"x:note":{"deep":["SECRET",{"a-name":"SECRET"}]}}`},
	{"item function call", `{"type":"item","id":"x","parent":"p",` + tsLine + `,"item":{"type":"function_call","call_id":"call-1","name":"run","arguments":"{\"cmd\":\"cat /home/SECRET\"}"}}`},
	{"response", `{"type":"response","id":"x","parent":"p",` + tsLine + `,"response_id":"resp-SECRET","model":"SECRET-model","status":"failed","error":{"code":"x","message":"SECRET failed","param":""},"x:note":"SECRET"}`},
	{"response the fields do not write back", `{"type":"response","id":"x","parent":"p",` + tsLine + `,"status":"completed","model":"SECRET-model","x:note":{"k":"SECRET"}}`},
	{"config", `{"type":"config","id":"x","parent":null,` + tsLine + `,"model":"m","instructions":"one SECRET\n\ntwo","instructions_parts":[{"id":"a","text":"one SECRET","source":"SECRET-layer"},{"id":"b","text":"two"}],"instructions_omitted":[{"id":"SECRET.md","reason":"SECRET budget","size":3}],"tools_added":[{"type":"function","name":"lookup","description":"Look up SECRET","parameters":{"type":"object"}}],"extra":{"user":"SECRET"}}`},
	{"compaction", `{"type":"compaction","id":"x","parent":"p",` + tsLine + `,"first_kept":"f","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"summary SECRET"}]},"pinned":[{"type":"message","role":"user","content":[{"type":"input_text","text":"pinned SECRET"}]}],"config":{"model":"m","instructions":"SECRET","instructions_parts":[{"id":"a","text":"SECRET"}],"extra":{"user":"SECRET"}}}`},
	{"branch_summary", `{"type":"branch_summary","id":"x","parent":"p",` + tsLine + `,"from":"f","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"left SECRET"}]}}`},
	{"run", `{"type":"run","id":"x","parent":"p",` + tsLine + `,"run_id":"run-SECRET","phase":"start","source":"input","ref":"SECRET-cron","trigger":{"kind":"schedule","ref":"SECRET","x:due":"SECRET"}}`},
	{"dispatch", `{"type":"dispatch","id":"x","parent":"p",` + tsLine + `,"call_id":"c","target":"t","idempotency_key":"key-SECRET"}`},
	{"decision", `{"type":"decision","id":"x","parent":"p",` + tsLine + `,"call_id":"c","target":"t","verdict":"reject","by":"human","reason":"SECRET is private","args":{"path":"/home/SECRET"}}`},
	{"queued", `{"type":"queued","id":"x","parent":"p",` + tsLine + `,"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"steer SECRET"}]},"mode":"steer","trigger":{"kind":"human","ref":"slack:SECRET"},"ref":"inbox-SECRET"}`},
	{"label", `{"type":"label","id":"x","parent":"p",` + tsLine + `,"target":"t","label":"SECRET checkpoint"}`},
	{"info with normalised was and raw", `{"type":"info","id":"x","parent":"p",` + tsLine + `,"name":"SECRET \ufffd","x:cut":"SECRET \ufffd","normalised":[{"at":"/name","was":"\"SECRET \\ud83d\""},{"at":"/x:cut","raw":"` + base64.StdEncoding.EncodeToString([]byte("\"SECRET \xff\"")) + `"}]}`},
	{"env with a workspace's host kept", `{"type":"env","id":"x","parent":"p",` + tsLine + `,"cwd":"/home/SECRET/proj","files":{"read":{"/home/SECRET/a.go":"sha256:4444444444444444444444444444444444444444444444444444444444444444"},"written":{"/home/SECRET/b.go":"sha256:4444444444444444444444444444444444444444444444444444444444444444"}},"vcs":{"system":"git","revision":"r","x:tree":"SECRET"},"tools":{"bash":"SECRET-5.2"},"workspace":{"kind":"container","ref":"img-SECRET","host":"SECRET.example"}}`},
	{"outcome", `{"type":"outcome","id":"x","parent":"p",` + tsLine + `,"kind":"feedback","target":"t","label":"SECRET says","details":{"note":"SECRET"}}`},
	{"link", `{"type":"link","id":"x","parent":"p",` + tsLine + `,"rel":"peer","session":"","call_id":"call-1","url":"https://SECRET.example/a2a","task":"task-SECRET","context":"ctx-SECRET"}`},
	{"custom", `{"type":"custom","id":"x","parent":"p",` + tsLine + `,"ns":"SECRET-app","data":{"text":"SECRET"},"call_id":"call-1"}`},
	{"extension entry", `{"type":"x:note","id":"x","parent":"p",` + tsLine + `,"text":"SECRET","nested":{"list":["SECRET"]},"normalised":[{"at":"/text","was":"\"SECRET\""}]}`},
}

// TestRewriteStringsPlanted: every planted SECRET is gone from what
// MarshalEntry writes of the rewritten entry, a raw record's bytes
// included, and the input entry is as it was.
func TestRewriteStringsPlanted(t *testing.T) {
	for _, tt := range plantedEntries {
		t.Run(tt.name, func(t *testing.T) {
			e := entryOf(t, tt.line)
			before := canonicalEntry(t, e)
			if !strings.Contains(before, "SECRET") {
				t.Fatalf("nothing planted survived decoding: %s", before)
			}
			got, err := RewriteStrings(e, hideSecret)
			if err != nil {
				t.Fatal(err)
			}
			if after := canonicalEntry(t, e); after != before {
				t.Errorf("the input changed:\n%s\n%s", before, after)
			}
			out := canonicalEntry(t, got)
			if strings.Contains(out, "SECRET") {
				t.Errorf("SECRET survived: %s", out)
			}
			for _, n := range got.Base().Normalised {
				if raw, err := base64.StdEncoding.DecodeString(n.Raw); err != nil || bytes.Contains(raw, []byte("SECRET")) {
					t.Errorf("raw at %s still holds SECRET (%v): %q", n.At, err, raw)
				}
			}
			if b := got.Base(); b.ID != "x" || b.Parent != e.Base().Parent || !b.Timestamp.Equal(e.Base().Timestamp) || b.ContentHash() != "" {
				t.Errorf("envelope: id %q parent %q ts %v content %q", b.ID, b.Parent, b.Timestamp, b.ContentHash())
			}
		})
	}
}

// TestRewriteStringsNormalised: a was record is rewritten as JSON text,
// its lone surrogate escaped as it was, and a raw record keeps its
// ill-formed byte; a record f does not change is left byte for byte.
func TestRewriteStringsNormalised(t *testing.T) {
	e := entryOf(t, `{"type":"info","id":"x","parent":null,`+tsLine+`,"name":"n","normalised":[{"at":"/a","was":"{\"SECRET\":[\"SECRET \\ud83d\", 9007199254740993]}"},{"at":"/b","raw":"`+base64.StdEncoding.EncodeToString([]byte("\"SECRET\\n\xff\""))+`"},{"at":"/c","was":"\"plain\\u0041\""}]}`)
	got, err := RewriteStrings(e, hideSecret)
	if err != nil {
		t.Fatal(err)
	}
	n := got.Base().Normalised
	if want := `{"SECRET":["gone \ud83d", 9007199254740993]}`; n[0].Was != want {
		t.Errorf("was = %s, want %s", n[0].Was, want)
	}
	raw, _ := base64.StdEncoding.DecodeString(n[1].Raw)
	if want := "\"gone\\n\xff\""; string(raw) != want {
		t.Errorf("raw = %q, want %q", raw, want)
	}
	if n[2].Was != `"plain\u0041"` {
		t.Errorf("an unchanged was was written again: %s", n[2].Was)
	}
	if err := checkNormalisations(n); err != nil {
		t.Error(err)
	}
}

// protectedEntries hold, in every member that is not body text, a value
// beginning P- or a hash, and in body text values beginning B-: the
// envelope, legacy_id, every reference, every hash, every call_id where
// the format defines one, a sidecar URL, a member name, a normalised
// record's at and a record of a member that is not body text. An env
// entry's file paths are data in member names, and body; so are a
// call_id inside data the format does not define and an of where the
// format does not define it.
var protectedEntries = []string{
	`{"type":"item","id":"P-id","parent":"P-parent",` + tsLine + `,"parents":[{"entry":"P-entry","session":"P-session","x:note":"P-extra"}],"legacy_id":"P-legacy","item":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + sidecarURL + `"},{"type":"input_text","text":"B-text"}]},"queued_from":"P-queued","P-name":"B-value","normalised":[{"at":"/P-name","was":"\"B-was\""}]}`,
	`{"type":"response","id":"P-id","parent":"P-parent",` + tsLine + `,"response_id":"B-resp","status":"completed","request_hash":"sha256:1111111111111111111111111111111111111111111111111111111111111111"}`,
	`{"type":"config","id":"P-id","parent":"P-parent",` + tsLine + `,"omit":{"reasoning":"other_models","items":["P-omit"]},"instructions_omitted":[{"keep":1,"of":"P-of"}],"instructions_parts":[{"id":"B-id","hash":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}]}`,
	`{"type":"compaction","id":"P-id","parent":"P-parent",` + tsLine + `,"first_kept":"P-first","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"B-sum"}]},"config":{"omit":{"items":["P-omit"]},"instructions_parts":[{"id":"B-id","text":"B-part"}]}}`,
	`{"type":"branch_summary","id":"P-id","parent":"P-parent",` + tsLine + `,"from":"P-from","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"B-sum"}]}}`,
	`{"type":"dispatch","id":"P-id","parent":"P-parent",` + tsLine + `,"call_id":"P-call","target":"P-target"}`,
	`{"type":"decision","id":"P-id","parent":"P-parent",` + tsLine + `,"call_id":"P-call","target":"P-target","verdict":"proceed"}`,
	`{"type":"label","id":"P-id","parent":"P-parent",` + tsLine + `,"target":"P-target","label":"B-label"}`,
	`{"type":"outcome","id":"P-id","parent":"P-parent",` + tsLine + `,"kind":"eval","target":"P-target"}`,
	`{"type":"link","id":"P-id","parent":"P-parent",` + tsLine + `,"rel":"judged_by","session":"P-session","target":"P-target"}`,
	`{"type":"link","id":"P-id","parent":"P-parent",` + tsLine + `,"rel":"subsession","session":"P-session","call_id":"P-call","target":"P-target","cancel":"sent"}`,
	`{"type":"env","id":"P-id","parent":"P-parent",` + tsLine + `,"cwd":"B-cwd","files":{"read":{"B-path":"sha256:3333333333333333333333333333333333333333333333333333333333333333"}}}`,
	`{"type":"item","id":"P-id","parent":"P-parent",` + tsLine + `,"item":{"type":"function_call","call_id":"P-call","name":"B-name","arguments":"B-args"},"normalised":[{"at":"/item/call_id","was":"\"P-call\""}]}`,
	`{"type":"item","id":"P-id","parent":"P-parent",` + tsLine + `,"item":{"type":"function_call_output","call_id":"P-call","output":"B-out"}}`,
	`{"type":"run","id":"P-id","parent":"P-parent",` + tsLine + `,"run_id":"B-run","phase":"end","reason":"input_required","pending":["P-call","P-call2"]}`,
	`{"type":"custom","id":"P-id","parent":"P-parent",` + tsLine + `,"ns":"B-ns","call_id":"P-call","data":{"call_id":"B-data-call","note":"B-note"}}`,
	`{"type":"decision","id":"P-id","parent":"P-parent",` + tsLine + `,"call_id":"P-call","target":"P-target","verdict":"proceed","args":{"call_id":"B-args-call"},"x:note":{"call_id":"B-unknown-call"}}`,
	`{"type":"queued","id":"P-id","parent":"P-parent",` + tsLine + `,"item":{"type":"function_call","call_id":"P-call","name":"B-name","arguments":"B-args"},"mode":"steer"}`,
	`{"type":"compaction","id":"P-id","parent":"P-parent",` + tsLine + `,"first_kept":"P-first","summary":{"type":"message","role":"user","content":[]},"pinned":[{"type":"function_call","call_id":"P-call","name":"B-name","arguments":"B-args"}],"config":{"instructions_omitted":[{"id":"B-part","reason":"B-reason","of":"B-of-beside-an-id"}],"omit":{"items":["P-omit"]}}}`,
	`{"type":"config","id":"P-id","parent":"P-parent",` + tsLine + `,"instructions_omitted":[{"id":"B-part","reason":"B-reason","of":"B-of-beside-an-id"},{"keep":2,"of":"P-of"},{"keep":0,"of":"B-of-no-keep"}]}`,
	`{"type":"x:note","id":"P-id","parent":"P-parent",` + tsLine + `,"parents":[{"entry":"P-entry"}],"legacy_id":"P-legacy","text":"B-text","P-name":"` + sidecarURL + `","call_id":"B-ext-call"}`,
}

// TestRewriteStringsNeverSeesProtected: f is handed every body string
// and nothing else, and what it is not handed comes through unchanged.
func TestRewriteStringsNeverSeesProtected(t *testing.T) {
	for i, line := range protectedEntries {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			e := entryOf(t, line)
			var seen []string
			got, err := RewriteStrings(e, func(s string) string {
				seen = append(seen, s)
				if strings.HasPrefix(s, "B-") {
					return "R-" + s
				}
				return s
			})
			if err != nil {
				t.Fatal(err)
			}
			body := 0
			for _, s := range seen {
				if strings.HasPrefix(s, "P-") || strings.Contains(s, "sha256:") {
					t.Errorf("f was given %q", s)
				}
				if strings.HasPrefix(s, "B-") {
					body++
				}
			}
			if want := strings.Count(line, `"B-`); body != want {
				t.Errorf("f was given %d body strings of %d: %q", body, want, seen)
			}
			before, after := canonicalEntry(t, e), canonicalEntry(t, got)
			if want := strings.ReplaceAll(before, "B-", "R-B-"); after != want {
				t.Errorf("a member other than body text changed:\n got %s\nwant %s", after, want)
			}
		})
	}
}

// TestRewriteStringsPartHashes: a part carrying its text and a hash has
// the hash recomputed, a Rewriter gives a later part named by hash the
// hash of the rewritten text it names, and instructions that would no
// longer be their parts joined are refused.
func TestRewriteStringsPartHashes(t *testing.T) {
	both := entryOf(t, `{"type":"config","id":"x","parent":null,`+tsLine+`,"instructions_parts":[{"id":"a","text":"SECRET","hash":"`+HashText("SECRET")+`"}]}`)
	got, err := RewriteStrings(both, hideSecret)
	if err != nil {
		t.Fatal(err)
	}
	if p := got.(*ConfigEntry).InstructionsParts[0]; p.Text != "gone" || p.Hash != HashText("gone") {
		t.Errorf("part %+v", p)
	}

	r := NewRewriter(hideSecret)
	first := entryOf(t, `{"type":"config","id":"x","parent":null,`+tsLine+`,"instructions_parts":[{"id":"a","text":"one SECRET"},{"id":"b","text":"two"}]}`)
	later := entryOf(t, `{"type":"config","id":"y","parent":"x",`+tsLine+`,"instructions_parts":[{"id":"a","hash":"`+HashText("one SECRET")+`"},{"id":"b","hash":"`+HashText("two")+`"},{"id":"c","hash":"`+HashText("unseen")+`"}]}`)
	if _, err := r.Rewrite(first); err != nil {
		t.Fatal(err)
	}
	got, err = r.Rewrite(later)
	if err != nil {
		t.Fatal(err)
	}
	parts := got.(*ConfigEntry).InstructionsParts
	if parts[0].Hash != HashText("one gone") || parts[1].Hash != HashText("two") || parts[2].Hash != HashText("unseen") {
		t.Errorf("parts %+v", parts)
	}
	if got, _ := RewriteStrings(later, hideSecret); got.(*ConfigEntry).InstructionsParts[0].Hash != HashText("one SECRET") {
		t.Error("RewriteStrings alone resolved a hash it never met the text of")
	}

	join := entryOf(t, `{"type":"config","id":"x","parent":null,`+tsLine+`,"instructions":"ab\n\ncd","instructions_parts":[{"id":"a","text":"ab"},{"id":"b","text":"cd"}]}`)
	got, err = RewriteStrings(join, func(s string) string { return strings.ReplaceAll(s, "b\n\nc", "-") })
	if err != nil {
		t.Fatalf("a rewrite across the join: %v", err)
	}
	if c := got.(*ConfigEntry); *c.Instructions != "ab\n\ncd" {
		t.Errorf("instructions %q crossed the part boundary", *c.Instructions)
	}
	got, err = RewriteStrings(join, func(s string) string { return strings.ReplaceAll(s, "b", "B") })
	if err != nil || *got.(*ConfigEntry).Instructions != "aB\n\ncd" {
		t.Errorf("a rewrite within the parts: %v", err)
	}
	notJoin := entryOf(t, `{"type":"config","id":"x","parent":null,`+tsLine+`,"instructions":"SECRET whole","instructions_parts":[{"id":"a","text":"other"}]}`)
	if got, err := RewriteStrings(notJoin, hideSecret); err != nil || *got.(*ConfigEntry).Instructions != "gone whole" {
		t.Errorf("instructions that are no join are not given to f whole: %v", err)
	}
}

// TestRewriteStringsJoinResolved: instructions beside parts named by keep
// or hash are rebuilt from the rewritten parts as the path resolves
// them, by a Rewriter given the earlier entries, and refused when the
// parts cannot be resolved and f would change the instructions, the
// review's case of a rewrite across a keep.
func TestRewriteStringsJoinResolved(t *testing.T) {
	first := `{"type":"config","id":"c1","parent":null,` + tsLine + `,"instructions_parts":[{"id":"a","text":"A SECRET"},{"id":"m","text":"memory"}]}`
	keep := `{"type":"config","id":"c2","parent":"c1",` + tsLine + `,"instructions":"A SECRET\n\nSECRET-B","instructions_parts":[{"keep":1},{"id":"b","text":"SECRET-B"}]}`
	hash := `{"type":"config","id":"c3","parent":"c2",` + tsLine + `,"instructions":"A SECRET\n\nSECRET-B\n\nmemory","instructions_parts":[{"keep":2},{"id":"m","hash":"` + HashText("memory") + `"}]}`
	across := func(s string) string { return strings.ReplaceAll(s, "SECRET\n\nSECRET", "X") }
	if _, err := RewriteStrings(entryOf(t, keep), across); !errors.Is(err, ErrInstructionsJoin) {
		t.Errorf("unresolvable keep, f changes the instructions: %v, want ErrInstructionsJoin", err)
	}
	if got, err := RewriteStrings(entryOf(t, keep), func(s string) string { return s }); err != nil || *got.(*ConfigEntry).Instructions != "A SECRET\n\nSECRET-B" {
		t.Errorf("unresolvable keep, f leaves the instructions: %v", err)
	}
	for _, f := range []func(string) string{hideSecret, across} {
		r := NewRewriter(f)
		var out []*ConfigEntry
		for _, line := range []string{first, keep, hash} {
			got, err := r.Rewrite(entryOf(t, line))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, got.(*ConfigEntry))
		}
		want2 := f("A SECRET") + "\n\n" + f("SECRET-B")
		if got := *out[1].Instructions; got != want2 {
			t.Errorf("keep: instructions %q, want %q", got, want2)
		}
		if got, want := *out[2].Instructions, want2+"\n\n"+f("memory"); got != want {
			t.Errorf("keep and hash: instructions %q, want %q", got, want)
		}
	}
}

// TestRewriteStringsWritesNoReadLine: an entry read with a line its
// fields do not write back, and an extra those fields do not hold, gives
// a rewritten entry that writes neither the old line nor its value, with
// its id kept and once its id is cleared and its parent changed.
func TestRewriteStringsWritesNoReadLine(t *testing.T) {
	e := entryOf(t, `{"type":"response","id":"x","parent":"p",`+tsLine+`,"status":"completed","x:note":{"k":"SECRET"},"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"x:who":"SECRET"}}`)
	if e.Base().line == nil {
		t.Fatal("the line is one the fields write back; pick another")
	}
	got, err := RewriteStrings(e, hideSecret)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*EntryBase){
		func(*EntryBase) {},
		func(b *EntryBase) { b.ID = "" },
		func(b *EntryBase) { b.Parent = "q" },
	} {
		change(got.Base())
		if out := canonicalEntry(t, got); strings.Contains(out, "SECRET") {
			t.Errorf("the read line or its extra was written: %s", out)
		}
	}
}

// redactableFixtures are fixtures an aligned redaction is made of in
// TestAlignedRedactionVerifies: between them they hold every core type,
// an extension, kept extras, instruction parts named by hash, omitted
// parts, omit, a fork's prefix and base, convergence, normalised records,
// callee links and judges.
var redactableFixtures = []string{"basic", "branch", "compaction", "converge", "extensions", "fork", "handback", "instructions", "interleaved", "judged", "normalised", "omit", "omitted", "parts", "peer", "pinned", "queued", "replay", "resume", "runs"}

// redaction is an aligned redaction of a session, read back from the
// file it writes.
type redaction struct {
	back    *Session
	entries []string          // the redacted file's entry lines
	byOrig  map[string]string // each original entry's id to its redacted line
}

// redact makes an aligned redaction of orig with the exported API alone,
// as a redactor outside the package would make one: every entry in file
// order through one Rewriter for the file and RemapRefs, its id cleared
// and committed to a new session whose header carries redacted and
// redacted_from and, last, the remapped base. The file it writes must
// read, which verifies every hash.
func redact(t *testing.T, orig *Session, f func(string) string, profile string) redaction {
	t.Helper()
	oh := orig.Header()
	ids := map[string]string{}
	remap := Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != oh.ID {
			return "", false // not redacted alongside
		}
		id, ok := ids[entry]
		return id, ok
	}}
	h := oh
	h.ID = RedactedSessionID(oh.ID, profile)
	h.Base = ""
	h.Redacted = true
	h.RedactedFrom = &RedactedFrom{Session: oh.ID, Profile: profile}
	red := New(h)
	rw := NewRewriter(f)
	var order []string
	for _, e := range orig.Entries() {
		old := e.Base().ID
		body, err := rw.Rewrite(e)
		if err != nil {
			t.Fatalf("rewrite %s: %v", old, err)
		}
		r, err := RemapRefs(body, remap)
		if err != nil {
			t.Fatalf("remap %s: %v", old, err)
		}
		for _, u := range r.Unmapped {
			if u.Session == "" || u.Session == oh.ID {
				t.Errorf("%s: %s names %s in this file and was not mapped", old, u.At, u.Entry)
			}
		}
		n := r.Entry
		n.Base().ID = ""
		res, err := red.Commit(n)
		if err != nil {
			t.Fatalf("commit %s: %v", old, err)
		}
		if res.Outcome == Held {
			t.Fatalf("%s: the redaction merged two entries", old)
		}
		ids[old] = res.ID
		order = append(order, old)
	}
	if oh.Base != "" {
		var unmapped []Reference
		h.Base = oh.Base
		if h, unmapped = RemapHeaderRefs(h, Remap{Entry: remap.Entry}); len(unmapped) > 0 {
			t.Fatalf("base not mapped: %v", unmapped)
		}
	}
	var head, body bytes.Buffer
	if err := Write(&head, New(h)); err != nil {
		t.Fatal(err)
	}
	if err := Write(&body, red); err != nil {
		t.Fatal(err)
	}
	entries := strings.SplitAfterN(body.String(), "\n", 2)[1]
	back, err := Read(strings.NewReader(head.String() + entries))
	if err != nil {
		t.Fatalf("the redacted file does not read: %v\n%s%s", err, head.String(), entries)
	}
	if back.Len() != orig.Len() {
		t.Errorf("%d entries, original %d", back.Len(), orig.Len())
	}
	if bh := back.Header(); !bh.Redacted || bh.RedactedFrom == nil || bh.RedactedFrom.Session != oh.ID || bh.Base != h.Base {
		t.Errorf("header %+v", bh)
	}
	out := redaction{back: back, entries: strings.Split(strings.TrimSuffix(entries, "\n"), "\n"), byOrig: map[string]string{}}
	for i, old := range order {
		out.byOrig[old] = out.entries[i]
	}
	return out
}

// TestAlignedRedactionVerifies makes an aligned redaction of each fixture
// with redact. The file holds one entry for each of the original's,
// every string the rewrite was to change is changed, a normalised raw's
// decoded bytes among them, and the instructions every path rebuilds are
// the rewritten ones.
func TestAlignedRedactionVerifies(t *testing.T) {
	// No closed value of the format or the payload holds a space, and
	// no ID, hash or call_id does, so every space a redacted line holds,
	// or the bytes a raw record decodes to, would be one the rewrite
	// missed.
	f := func(s string) string { return strings.ReplaceAll(s, " ", "_") }
	for _, name := range redactableFixtures {
		t.Run(name, func(t *testing.T) {
			orig := loadFixture(t, name)
			red := redact(t, orig, f, "test.spaces@1")
			for i, line := range red.entries {
				if strings.Contains(line, " ") {
					t.Errorf("entry %d keeps a space: %s", i, line)
				}
			}
			for i, e := range red.back.Entries() {
				for _, n := range e.Base().Normalised {
					raw, err := base64.StdEncoding.DecodeString(n.Raw)
					if err != nil || bytes.Contains(raw, []byte(" ")) {
						t.Errorf("entry %d: raw at %s keeps a space (%v): %q", i, n.At, err, raw)
					}
				}
			}
			for i, e := range orig.Entries() {
				oc, err := orig.ContextAt(e.Base().ID)
				if err != nil {
					continue
				}
				rc, err := red.back.ContextAt(red.back.Entries()[i].Base().ID)
				if err != nil {
					t.Fatalf("entry %d: the original builds a context and the redaction does not: %v", i, err)
				}
				if want := f(oc.Settings.Instructions); rc.Settings.Instructions != want {
					t.Errorf("entry %d: instructions %q, want %q", i, rc.Settings.Instructions, want)
				}
			}
		})
	}
}

// TestRedactedForkConverges: a fork and its origin, each redacted with a
// Rewriter of its own, give the prefix they share the same bytes, and so
// the same IDs: one redaction of a store converges.
func TestRedactedForkConverges(t *testing.T) {
	f := strings.NewReplacer("2+2", "[sum]", "brief", "[style]").Replace
	origin := redact(t, loadFixture(t, "basic"), f, "test.words@1")
	fork := loadFixture(t, "fork")
	red := redact(t, fork, f, "test.words@1")
	shared, changed := 0, 0
	for _, e := range fork.Entries() {
		old := e.Base().ID
		want, ok := origin.byOrig[old]
		if !ok {
			continue
		}
		shared++
		if got := red.byOrig[old]; got != want {
			t.Errorf("prefix entry %s redacts two ways:\nfork   %s\norigin %s", old, got, want)
		}
		if strings.Contains(want, "[sum]") || strings.Contains(want, "[style]") {
			changed++
		}
	}
	if shared == 0 || changed == 0 {
		t.Fatalf("the fork shares %d entries with its origin, %d of them redacted; the test proves nothing", shared, changed)
	}
}

// TestRewriteStringsPathKeys: an env entry's file paths are rewritten
// with their hashes kept, a record of a normalised hash follows its
// renamed path in sorted order, and two paths made one are refused.
func TestRewriteStringsPathKeys(t *testing.T) {
	h1, h2 := "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64)
	e := entryOf(t, `{"type":"env","id":"x","parent":null,`+tsLine+`,"files":{"read":{"/home/SECRET/z":"`+h1+`","/a":"`+h2+`"},"written":{"/home/SECRET/w":"`+h1+`"}},"tools":{"SECRET-tool":"1"},"normalised":[{"at":"/files/read/~1home~1SECRET~1z","was":"\"SECRET\""},{"at":"/files/read/~1x","was":"\"SECRET\""}]}`)
	got, err := RewriteStrings(e, func(s string) string { return strings.ReplaceAll(s, "/home/SECRET", "/~") })
	if err != nil {
		t.Fatal(err)
	}
	env := got.(*EnvEntry)
	if env.Files.Read["/~/z"] != h1 || env.Files.Read["/a"] != h2 || env.Files.Written["/~/w"] != h1 || len(env.Files.Read) != 2 {
		t.Errorf("files %+v", env.Files)
	}
	if _, ok := env.Tools["SECRET-tool"]; !ok {
		t.Errorf("a tool name was rewritten: %v", env.Tools)
	}
	n := env.Normalised
	if n[0].At != "/files/read/~1x" || n[1].At != "/files/read/~1~0~1z" || n[1].Was != `"SECRET"` {
		t.Errorf("normalised %+v", n)
	}

	merge := entryOf(t, `{"type":"env","id":"x","parent":null,`+tsLine+`,"files":{"read":{"/home/a/f":"`+h1+`","/home/b/f":"`+h2+`"}}}`)
	if _, err := RewriteStrings(merge, func(s string) string { return strings.NewReplacer("/home/a", "/~", "/home/b", "/~").Replace(s) }); !errors.Is(err, ErrKeyCollision) {
		t.Errorf("two paths made one: %v, want ErrKeyCollision", err)
	}
}

// TestRewriteStringsNumberWas: a was holding the digits of a member the
// payload carries as a string is given to f as those digits, and where f
// changes them it holds the JSON string of the result, not the value.
func TestRewriteStringsNumberWas(t *testing.T) {
	e := entryOf(t, `{"type":"item","id":"x","parent":null,`+tsLine+`,"item":{"type":"function_call_output","call_id":"c","output":"12345678901234567890"},"normalised":[{"at":"/item/output","was":"12345678901234567890"},{"at":"/x:n","was":"42"}],"x:n":42}`)
	mask := regexp.MustCompile(`[0-9]{16,}`)
	got, err := RewriteStrings(e, func(s string) string { return mask.ReplaceAllString(s, "[ACCT]") })
	if err != nil {
		t.Fatal(err)
	}
	out := canonicalEntry(t, got)
	if strings.Contains(out, "1234567890123456") {
		t.Errorf("the digits survived: %s", out)
	}
	if n := got.Base().Normalised; n[0].Was != `"[ACCT]"` || n[1].Was != "42" {
		t.Errorf("normalised %+v", n)
	}
	if err := checkNormalisations(got.Base().Normalised); err != nil {
		t.Error(err)
	}
}

// TestRewriteStringsCallOrder: f is called in one order, so a rewrite
// that numbers what it meets, which one string to one result allows,
// gives the same bytes on every run.
func TestRewriteStringsCallOrder(t *testing.T) {
	line := `{"type":"item","id":"x","parent":null,` + tsLine + `,"item":{"type":"message","role":"user","content":[{"type":"input_text","text":"alice"},{"type":"input_text","text":"bob"}]},"x:a":"carol","x:b":{"z":"dave","a":"erin","m":["frank","gina"]},"x:c":"hal","normalised":[{"at":"/x:a","was":"\"ivan\"","x:z":"judy","x:a":"kim"}]}`
	run := func() string {
		names := map[string]string{}
		pseudonym := func(s string) string {
			if _, ok := names[s]; !ok {
				names[s] = fmt.Sprintf("PERSON_%d", len(names)+1)
			}
			return names[s]
		}
		got, err := RewriteStrings(entryOf(t, line), pseudonym)
		if err != nil {
			t.Fatal(err)
		}
		return canonicalEntry(t, got)
	}
	first := run()
	for range 20 {
		if again := run(); again != first {
			t.Fatalf("two runs differ:\n%s\n%s", first, again)
		}
	}
}
