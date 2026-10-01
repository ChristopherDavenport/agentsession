package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/openresponses"
)

// TestCanonicalRequestAcrossStores: a request whose tool schema was
// sent with its members out of canonical order comes back from jsonl as
// written and from cas in canonical order, equal under canonical JSON
// only. After [agentsession.CanonicalRequest] the live request, the
// jsonl rebuild and the cas rebuild encode to the same bytes (#175).
func TestCanonicalRequestAcrossStores(t *testing.T) {
	ctx := context.Background()
	schema := json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"file"}},"required":["path"]}`)
	tool := openresponses.NewFunctionTool("read", "read a file", schema)

	record := func(t *testing.T, st agentsession.Store) string {
		t.Helper()
		s, err := st.Create(ctx, agentsession.Header{CWD: "/p"})
		if err != nil {
			t.Fatal(err)
		}
		id := s.ID()
		for _, e := range []agentsession.Entry{
			&agentsession.ConfigEntry{Model: "m", ToolsAdded: openresponses.Tools{tool}},
			agentsession.NewItemEntry(openresponses.UserText("hi")),
			&agentsession.ItemEntry{Item: openresponses.AssistantText("yo"), ResponseID: "resp_1"},
		} {
			if _, err := st.Append(ctx, id, e); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.Append(ctx, id, &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	rebuild := func(t *testing.T, st agentsession.Store, id string) openresponses.Request {
		t.Helper()
		s, err := st.Open(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		c, err := s.RequestContext(s.Leaf())
		if err != nil {
			t.Fatal(err)
		}
		req, err := c.Request()
		if err != nil {
			t.Fatal(err)
		}
		return req
	}
	encode := func(t *testing.T, req openresponses.Request) []byte {
		t.Helper()
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	canonical := func(t *testing.T, req openresponses.Request) []byte {
		t.Helper()
		c, err := agentsession.CanonicalRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		return encode(t, c)
	}

	jroot := t.TempDir()
	js, err := jsonl.Open(jroot)
	if err != nil {
		t.Fatal(err)
	}
	jid := record(t, js)
	js.Close()
	js, err = jsonl.Open(jroot)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	fromJSONL := rebuild(t, js, jid)

	croot := t.TempDir()
	cs, err := Open(croot)
	if err != nil {
		t.Fatal(err)
	}
	cid := record(t, cs)
	cs.Close()
	cs, err = Open(croot)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	fromCAS := rebuild(t, cs, cid)

	// The difference this helper exists for: without it the two
	// rebuilds are different bytes.
	if bytes.Equal(encode(t, fromJSONL), encode(t, fromCAS)) {
		t.Fatal("jsonl and cas rebuilt the same bytes; the schema's members are already canonical, so this proves nothing")
	}
	hj, _ := agentsession.RequestHash(fromJSONL)
	hc, _ := agentsession.RequestHash(fromCAS)
	if hj != hc {
		t.Fatalf("request hashes differ: jsonl %s, cas %s", hj, hc)
	}

	store := false
	live := openresponses.Request{Model: "m", Tools: openresponses.Tools{tool}, Input: openresponses.Items{openresponses.UserText("hi")}, Store: &store}
	want := canonical(t, live)
	if got := canonical(t, fromJSONL); !bytes.Equal(got, want) {
		t.Errorf("jsonl rebuild canonicalised:\n%s\nlive canonicalised:\n%s", got, want)
	}
	if got := canonical(t, fromCAS); !bytes.Equal(got, want) {
		t.Errorf("cas rebuild canonicalised:\n%s\nlive canonicalised:\n%s", got, want)
	}
	lh, _ := agentsession.RequestHash(live)
	if lh != hj {
		t.Errorf("live request hash %s, rebuilt %s", lh, hj)
	}
}
