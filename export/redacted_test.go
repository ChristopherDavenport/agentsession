package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/openresponses"
)

// redaction makes a redaction of the named fixture as a redactor makes
// one with the exported helpers: every body string through one Rewriter
// in file order, every reference re-pointed with RemapRefs to the
// entries already written, each entry marked with the position it was
// made from by SetRedacted, a response given the request_hash of its
// request in the redacted session by SetRequestHash, and committed, its
// ID cleared, to a session whose header carries redacted and
// redacted_from, under the ID RedactedSessionID derives. It returns the
// session in memory, for a test to grow.
func redaction(t *testing.T, name, profile string, extra map[string]json.RawMessage, f func(string) string) *agentsession.Session {
	t.Helper()
	orig := loadFixture(t, name)
	oh := orig.Header()
	if oh.Base != "" {
		t.Fatalf("%s has a base; redaction does not re-point one", name)
	}
	h := oh
	h.ID = agentsession.RedactedSessionID(oh.ID, profile)
	h.Redacted = true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Profile: profile, Extra: extra}
	red := agentsession.New(h)
	ids := map[string]string{}
	remap := agentsession.Remap{Entry: func(session, entry string) (string, bool) {
		if session != "" && session != oh.ID {
			return "", false
		}
		id, ok := ids[entry]
		return id, ok
	}}
	rw := agentsession.NewRewriter(f)
	for i, e := range agentsession.FileOrder(orig) {
		body, err := rw.Rewrite(e)
		if err != nil {
			t.Fatalf("rewrite %s: %v", e.Base().ID, err)
		}
		r, err := agentsession.RemapRefs(body, remap)
		if err != nil {
			t.Fatalf("remap %s: %v", e.Base().ID, err)
		}
		n := r.Entry
		n.Base().ID = ""
		if err := agentsession.SetRedacted(n, []agentsession.Span{{From: i + 1, To: i + 1}}); err != nil {
			t.Fatal(err)
		}
		if resp, ok := n.(*agentsession.ResponseEntry); ok {
			if err := red.SetRequestHash(resp); err != nil {
				t.Fatalf("request hash of %s: %v", e.Base().ID, err)
			}
		}
		res, err := red.Commit(n)
		if err != nil {
			t.Fatalf("commit %s: %v", e.Base().ID, err)
		}
		ids[e.Base().ID] = res.ID
	}
	return red
}

// TestExportRedacted: a redacted session exports like any other, and
// verifies like any other: each redacted response's request_hash is
// the one its redacted request rebuilds to, the second's moved by the
// redaction, and the native response records none. The document's root
// extra.agentsession carries
// redacted and redacted_from, the header's object with every member it
// holds, so a published document links back to its original as the
// session file does. Each step made from a redacted entry carries that
// entry's redacted under agentsession in its extra, and an agent step,
// made from its items, its response and the outputs observed in it,
// carries each one's under redacted_entries; the steps a writer
// appended natively, resuming the session, carry neither. All of it
// survives the trip through the file form, beside the items, and a
// session that is not redacted carries none of it.
func TestExportRedacted(t *testing.T) {
	extra := map[string]json.RawMessage{"ex.tool": json.RawMessage(`{"name":"scrub","version":1}`)}
	red := redaction(t, "basic", "test/v1", extra, func(v string) string {
		if v == "4" {
			return "[n]"
		}
		return v
	})
	// Resumed: a user turn and a response appended natively, at fixed
	// times so the golden document is stable.
	turn := agentsession.NewItemEntry(openresponses.UserText("and 3+3?"))
	turn.Timestamp = time.Date(2026, 9, 17, 12, 1, 0, 0, time.UTC)
	if _, err := red.Append(turn); err != nil {
		t.Fatal(err)
	}
	native := &agentsession.ResponseEntry{ResponseID: "resp_3", Model: "gpt-5", Status: openresponses.ResponseStatusCompleted}
	native.Timestamp = time.Date(2026, 9, 17, 12, 1, 1, 0, time.UTC)
	if _, err := red.Append(native); err != nil {
		t.Fatal(err)
	}
	var file bytes.Buffer
	if err := agentsession.Write(&file, red); err != nil {
		t.Fatal(err)
	}
	s, err := agentsession.Read(&file)
	if err != nil {
		t.Fatalf("the redacted file does not read: %v", err)
	}
	orig := loadFixture(t, "basic")
	for _, name := range []string{"r0000001", "r0000002"} {
		if err := s.Verify(lid(t, s, name)); err != nil {
			t.Errorf("redacted response %s does not verify: %v", name, err)
		}
	}
	if r1, r2 := responseOf(t, s, "r0000001"), responseOf(t, orig, "r0000001"); r1.RequestHash != r2.RequestHash {
		t.Errorf("the first request, which the redaction left alone, hashes %s, the original's %s", r1.RequestHash, r2.RequestHash)
	}
	if r1, r2 := responseOf(t, s, "r0000002"), responseOf(t, orig, "r0000002"); r1.RequestHash == r2.RequestHash {
		t.Error("the second request_hash is the original's; the redaction changed its request")
	}
	if err := s.Verify(native.ID); !errors.Is(err, agentsession.ErrNoHash) {
		t.Errorf("the native response: %v, want ErrNoHash", err)
	}
	wantFrom, err := json.Marshal(s.Header().RedactedFrom)
	if err != nil {
		t.Fatal(err)
	}
	// What each step's extra.agentsession holds, by its entries' legacy
	// ids and their positions in basic: config 1, the user item 2, the
	// first response's reasoning 3, call 4, response 5 and output 6, the
	// answer 7 and its response 8, the outcome 9.
	as := func(entry string, redacted string, each ...string) string {
		out := fmt.Sprintf(`{"entry_id":%q`, entry)
		if redacted != "" {
			out += `,"redacted":` + redacted
		}
		if len(each) > 0 {
			var kv []string
			for i := 0; i < len(each); i += 2 {
				kv = append(kv, fmt.Sprintf(`%q:%s`, lid(t, s, each[i]), each[i+1]))
			}
			out += `,"redacted_entries":{` + strings.Join(kv, ",") + `}`
		}
		return out + "}"
	}
	wantSteps := []string{
		as(lid(t, s, "i0000001"), "[2]"),
		as(lid(t, s, "r0000001"), "[5]", "i0000002", "[3]", "i0000003", "[4]", "r0000001", "[5]", "i0000004", "[6]"),
		as(lid(t, s, "r0000002"), "[8]", "i0000005", "[7]", "r0000002", "[8]"),
	}
	n := 0
	for tr, err := range Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		n++
		doc, err := ToATIF(tr, Options{})
		if err != nil {
			t.Fatalf("ToATIF(%s): %v", tr.LeafID, err)
		}
		if err := doc.Validate(); err != nil {
			t.Errorf("invalid document: %v", err)
		}
		if doc.SessionID != s.ID() {
			t.Errorf("session_id %s, want the redacted session's %s", doc.SessionID, s.ID())
		}
		got := encode(t, doc)
		checkGolden(t, filepath.Join("..", "testdata", "export", "redacted-basic.json"), got)
		if !strings.Contains(string(got), `"[n]"`) || strings.Contains(string(got), `"output": "4"`) {
			t.Errorf("the document does not carry the redacted output:\n%s", got)
		}
		checkSteps := func(where string, doc *atif.Trajectory) {
			t.Helper()
			if len(doc.Steps) != len(wantSteps)+2 {
				t.Fatalf("%s: %d steps, want %d redacted and 2 native", where, len(doc.Steps), len(wantSteps))
			}
			for i, step := range doc.Steps {
				m, ok := step.Extra[ExtraAgentSession].(map[string]any)
				if !ok {
					t.Fatalf("%s: step %d has no extra.agentsession", where, step.StepID)
				}
				if i >= len(wantSteps) {
					if _, ok := m["redacted"]; ok {
						t.Errorf("%s: native step %d carries redacted: %v", where, step.StepID, m)
					}
					if _, ok := m["redacted_entries"]; ok {
						t.Errorf("%s: native step %d carries redacted_entries: %v", where, step.StepID, m)
					}
					continue
				}
				g, err := json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				assertSameJSON(t, []byte(wantSteps[i]), g)
			}
		}
		checkSteps("built", doc)

		// Through the file form, as WriteDocument writes it.
		path := filepath.Join(t.TempDir(), DocumentName(doc))
		if err := WriteDocument(path, doc); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var back atif.Trajectory
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		root, ok := back.Extra[ExtraAgentSession].(map[string]any)
		if !ok {
			t.Fatalf("root extra.agentsession = %v", back.Extra[ExtraAgentSession])
		}
		if root["redacted"] != true {
			t.Errorf("root extra.agentsession.redacted = %v, want true", root["redacted"])
		}
		gotFrom, err := json.Marshal(root["redacted_from"])
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, wantFrom, gotFrom)
		checkSteps("read back", &back)
		items, err := Items(&back)
		if err != nil {
			t.Fatalf("Items after the file form: %v", err)
		}
		wantItems, _ := json.Marshal(tr.Context.Items)
		gotItems, _ := json.Marshal(items)
		assertSameJSON(t, wantItems, gotItems)

		// A redactor run at export keeps the link and the marks: they
		// are the redacted session's own, and Secrets rewrites every
		// string of the document through its JSON form.
		redone, err := ToATIF(tr, Options{Redactors: []Redactor{Environment(), NoPassthrough(), Secrets("2+2")}})
		if err != nil {
			t.Fatal(err)
		}
		gotFrom, err = json.Marshal(redone.Extra[ExtraAgentSession].(map[string]any)["redacted_from"])
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, wantFrom, gotFrom)
		checkSteps("redacted at export", redone)
	}
	if n != len(s.Leaves()) {
		t.Errorf("%d trajectories for %d leaves", n, len(s.Leaves()))
	}

	plain := loadFixture(t, "basic")
	tr, err := At(plain, plain.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	doc := mustDoc(t, tr)
	root := doc.Extra[ExtraAgentSession].(map[string]any)
	for _, k := range []string{"redacted", "redacted_from"} {
		if _, ok := root[k]; ok {
			t.Errorf("a session that is not redacted exports %s: %v", k, root[k])
		}
	}
	for _, step := range doc.Steps {
		m := step.Extra[ExtraAgentSession].(map[string]any)
		if _, ok := m["redacted"]; ok {
			t.Errorf("a step of a session that is not redacted carries redacted: %v", m)
		}
		if _, ok := m["redacted_entries"]; ok {
			t.Errorf("a step of a session that is not redacted carries redacted_entries: %v", m)
		}
	}
}

// responseOf is the response entry of s whose legacy_id is name.
func responseOf(t *testing.T, s *agentsession.Session, name string) *agentsession.ResponseEntry {
	t.Helper()
	e, _ := s.Entry(lid(t, s, name))
	r, ok := e.(*agentsession.ResponseEntry)
	if !ok {
		t.Fatalf("%s is not a response", name)
	}
	return r
}
