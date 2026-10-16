package agentsession

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
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
	{"item function call", `{"type":"item","id":"x","parent":"p",` + tsLine + `,"item":{"type":"function_call","call_id":"call-SECRET","name":"run","arguments":"{\"cmd\":\"cat /home/SECRET\"}"}}`},
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
	{"env with a workspace's host kept", `{"type":"env","id":"x","parent":"p",` + tsLine + `,"cwd":"/home/SECRET/proj","vcs":{"system":"git","revision":"r","x:tree":"SECRET"},"tools":{"bash":"SECRET-5.2"},"workspace":{"kind":"container","ref":"img-SECRET","host":"SECRET.example"}}`},
	{"outcome", `{"type":"outcome","id":"x","parent":"p",` + tsLine + `,"kind":"feedback","target":"t","label":"SECRET says","details":{"note":"SECRET"}}`},
	{"link", `{"type":"link","id":"x","parent":"p",` + tsLine + `,"rel":"peer","session":"","call_id":"call-SECRET","url":"https://SECRET.example/a2a","task":"task-SECRET","context":"ctx-SECRET"}`},
	{"custom", `{"type":"custom","id":"x","parent":"p",` + tsLine + `,"ns":"SECRET-app","data":{"text":"SECRET"},"call_id":"call-SECRET"}`},
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
// envelope, legacy_id, every reference, every hash, a sidecar URL, a
// member name, a normalised record's at.
var protectedEntries = []string{
	`{"type":"item","id":"P-id","parent":"P-parent",` + tsLine + `,"parents":[{"entry":"P-entry","session":"P-session","x:note":"P-extra"}],"legacy_id":"P-legacy","item":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + sidecarURL + `"},{"type":"input_text","text":"B-text"}]},"queued_from":"P-queued","P-name":"B-value","normalised":[{"at":"/P-name","was":"\"B-was\""}]}`,
	`{"type":"response","id":"P-id","parent":"P-parent",` + tsLine + `,"response_id":"B-resp","status":"completed","request_hash":"sha256:1111111111111111111111111111111111111111111111111111111111111111"}`,
	`{"type":"config","id":"P-id","parent":"P-parent",` + tsLine + `,"omit":{"reasoning":"other_models","items":["P-omit"]},"instructions_omitted":[{"keep":1,"of":"P-of"}],"instructions_parts":[{"id":"B-id","hash":"sha256:2222222222222222222222222222222222222222222222222222222222222222"}]}`,
	`{"type":"compaction","id":"P-id","parent":"P-parent",` + tsLine + `,"first_kept":"P-first","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"B-sum"}]},"config":{"omit":{"items":["P-omit"]},"instructions_parts":[{"id":"B-id","text":"B-part"}]}}`,
	`{"type":"branch_summary","id":"P-id","parent":"P-parent",` + tsLine + `,"from":"P-from","summary":{"type":"message","role":"user","content":[{"type":"input_text","text":"B-sum"}]}}`,
	`{"type":"dispatch","id":"P-id","parent":"P-parent",` + tsLine + `,"call_id":"B-call","target":"P-target"}`,
	`{"type":"decision","id":"P-id","parent":"P-parent",` + tsLine + `,"call_id":"B-call","target":"P-target","verdict":"proceed"}`,
	`{"type":"label","id":"P-id","parent":"P-parent",` + tsLine + `,"target":"P-target","label":"B-label"}`,
	`{"type":"outcome","id":"P-id","parent":"P-parent",` + tsLine + `,"kind":"eval","target":"P-target"}`,
	`{"type":"link","id":"P-id","parent":"P-parent",` + tsLine + `,"rel":"judged_by","session":"P-session","target":"P-target"}`,
	`{"type":"link","id":"P-id","parent":"P-parent",` + tsLine + `,"rel":"subsession","session":"P-session","call_id":"B-call","target":"P-target","cancel":"sent"}`,
	`{"type":"env","id":"P-id","parent":"P-parent",` + tsLine + `,"cwd":"B-cwd","files":{"read":{"P-path":"sha256:3333333333333333333333333333333333333333333333333333333333333333"}}}`,
	`{"type":"x:note","id":"P-id","parent":"P-parent",` + tsLine + `,"parents":[{"entry":"P-entry"}],"legacy_id":"P-legacy","text":"B-text","P-name":"` + sidecarURL + `"}`,
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
			if strings.ReplaceAll(after, `"R-B-`, `"B-`) != before && !strings.Contains(line, `"was"`) {
				t.Errorf("a member other than body text changed:\n%s\n%s", before, after)
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
	if _, err := RewriteStrings(join, func(s string) string { return strings.ReplaceAll(s, "b\n\nc", "-") }); !errors.Is(err, ErrInstructionsJoin) {
		t.Errorf("a rewrite across the join: %v, want ErrInstructionsJoin", err)
	}
	if _, err := RewriteStrings(join, func(s string) string { return strings.ReplaceAll(s, "b", "B") }); err != nil {
		t.Errorf("a rewrite within the parts: %v", err)
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

// TestAlignedRedactionVerifies makes an aligned redaction of each fixture
// with the exported API alone, as a redactor outside the package would
// make one:
// every entry in file order through a Rewriter and RemapRefs, its id
// cleared and committed to a new session whose header carries redacted
// and redacted_from and the remapped base. The file written verifies as
// it is read, holds one entry for each of the original's, every string
// the rewrite was to change is changed, and the instructions every path
// rebuilds are the rewritten ones.
func TestAlignedRedactionVerifies(t *testing.T) {
	// No closed value of the format or the payload holds a space, and
	// no ID or hash does, so every space a redacted line holds would be
	// one the rewrite missed.
	f := func(s string) string { return strings.ReplaceAll(s, " ", "_") }
	for _, name := range redactableFixtures {
		t.Run(name, func(t *testing.T) {
			orig := loadFixture(t, name)
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
			h.ID = RedactedSessionID(oh.ID, orig.Len(), "test.spaces@1")
			h.Base = ""
			h.Redacted = true
			h.RedactedFrom = &RedactedFrom{Session: oh.ID, Entries: orig.Len(), Profile: "test.spaces@1", Aligned: true}
			red := New(h)
			rw := NewRewriter(f)
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
			}
			if oh.Base != "" {
				// Last the header's base, once the prefix is hashed.
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
			file := head.String() + entries
			back, err := Read(strings.NewReader(file))
			if err != nil {
				t.Fatalf("the redacted file does not read: %v\n%s", err, file)
			}
			if back.Len() != orig.Len() {
				t.Errorf("%d entries, original %d", back.Len(), orig.Len())
			}
			if bh := back.Header(); !bh.Redacted || bh.RedactedFrom == nil || bh.RedactedFrom.Session != oh.ID || bh.Base != h.Base {
				t.Errorf("header %+v", bh)
			}
			for i, line := range strings.Split(strings.TrimSuffix(entries, "\n"), "\n") {
				if strings.Contains(line, " ") {
					t.Errorf("entry %d keeps a space: %s", i, line)
				}
			}
			for i, e := range orig.Entries() {
				oc, err := orig.ContextAt(e.Base().ID)
				if err != nil {
					continue
				}
				rc, err := back.ContextAt(back.Entries()[i].Base().ID)
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
