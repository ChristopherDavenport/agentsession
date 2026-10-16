package export

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
)

// redaction makes an aligned redaction of the named fixture as a
// redactor makes one with the exported helpers: every body string
// through one Rewriter in file order, every reference re-pointed with
// RemapRefs to the entries already written, each entry committed, its
// ID cleared, to a session whose header carries redacted and
// redacted_from, under the ID RedactedSessionID derives. The request
// hashes are kept, so they are the original's. It returns the session
// as a reader of its file has it.
func redaction(t *testing.T, name, profile string, extra map[string]json.RawMessage, f func(string) string) *agentsession.Session {
	t.Helper()
	orig := loadFixture(t, name)
	oh := orig.Header()
	if oh.Base != "" {
		t.Fatalf("%s has a base; redaction does not re-point one", name)
	}
	order := agentsession.FileOrder(orig)
	h := oh
	h.ID = agentsession.RedactedSessionID(oh.ID, len(order), profile)
	h.Redacted = true
	h.RedactedFrom = &agentsession.RedactedFrom{Session: oh.ID, Entries: len(order), Profile: profile, Aligned: true, Extra: extra}
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
	for _, e := range order {
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
		res, err := red.Commit(n)
		if err != nil {
			t.Fatalf("commit %s: %v", e.Base().ID, err)
		}
		ids[e.Base().ID] = res.ID
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, red); err != nil {
		t.Fatal(err)
	}
	s, err := agentsession.Read(&buf)
	if err != nil {
		t.Fatalf("the redacted file does not read: %v", err)
	}
	return s
}

// TestExportRedacted: a redacted session exports like any other, though
// a request hash it kept no longer matches, since nothing in export
// checks one; the document's root extra.agentsession carries redacted
// and redacted_from, the header's object with every member it holds,
// so a published document links back to its original as the session
// file does; and both survive the trip through the file form, beside
// the items. A session that is not redacted carries neither.
func TestExportRedacted(t *testing.T) {
	extra := map[string]json.RawMessage{"ex.tool": json.RawMessage(`{"name":"scrub","version":1}`)}
	s := redaction(t, "basic", "test/v1", extra, func(v string) string {
		if v == "4" {
			return "[n]"
		}
		return v
	})
	if v := s.Verify(lid(t, s, "r0000002")); v == nil {
		t.Fatal("the redaction left the second request hash matching; the case needs one that does not")
	}
	wantFrom, err := json.Marshal(s.Header().RedactedFrom)
	if err != nil {
		t.Fatal(err)
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
		checkGolden(t, filepath.Join("..", "testdata", "export", "redacted-basic."+legacyOf(s, tr.LeafID)+".json"), got)
		if !strings.Contains(string(got), `"[n]"`) || strings.Contains(string(got), `"output": "4"`) {
			t.Errorf("the document does not carry the redacted output:\n%s", got)
		}

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
		as, ok := back.Extra[ExtraAgentSession].(map[string]any)
		if !ok {
			t.Fatalf("root extra.agentsession = %v", back.Extra[ExtraAgentSession])
		}
		if as["redacted"] != true {
			t.Errorf("root extra.agentsession.redacted = %v, want true", as["redacted"])
		}
		gotFrom, err := json.Marshal(as["redacted_from"])
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, wantFrom, gotFrom)

		items, err := Items(&back)
		if err != nil {
			t.Fatalf("Items after the file form: %v", err)
		}
		wantItems, _ := json.Marshal(tr.Context.Items)
		gotItems, _ := json.Marshal(items)
		assertSameJSON(t, wantItems, gotItems)

		// A redactor run at export keeps the link: it is the redacted
		// session's own, and Environment drops only the cwd beside it.
		red, err := ToATIF(tr, Options{Redactors: []Redactor{Environment(), NoPassthrough()}})
		if err != nil {
			t.Fatal(err)
		}
		gotFrom, err = json.Marshal(red.Extra[ExtraAgentSession].(map[string]any)["redacted_from"])
		if err != nil {
			t.Fatal(err)
		}
		assertSameJSON(t, wantFrom, gotFrom)
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
	as := doc.Extra[ExtraAgentSession].(map[string]any)
	for _, k := range []string{"redacted", "redacted_from"} {
		if _, ok := as[k]; ok {
			t.Errorf("a session that is not redacted exports %s: %v", k, as[k])
		}
	}
}
