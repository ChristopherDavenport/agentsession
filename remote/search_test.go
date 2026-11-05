package remote_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// searchSessions seeds st with three sessions, oldest first, each
// holding one user message naming its owner's secret, and returns
// their IDs.
func searchSessions(t *testing.T, st agentsession.Store) []string {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	var ids []string
	for i, id := range []string{"alice-old", "alice-new", "bob-newest"} {
		if _, err := st.Create(ctx, agentsession.Header{ID: id, CreatedAt: base.Add(time.Duration(i) * time.Hour)}); err != nil {
			t.Fatal(err)
		}
		owner, _, _ := strings.Cut(id, "-")
		if _, err := st.Append(ctx, id, user(owner+"'s secret plan")); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func hitSessions(t *testing.T, c *remote.Client, q agentsession.Query) []string {
	t.Helper()
	var out []string
	for h, err := range agentsession.Search(context.Background(), c, q) {
		if err != nil {
			t.Fatalf("search %q: %v", q.Text, err)
		}
		out = append(out, h.Session)
	}
	return out
}

// A server serving several principals searches only the sessions the
// caller may read (RFC 0002 and RFC 0003, Search): another's is not
// found, and does not count toward the filter's limit on sessions.
func TestSearchOnlyWhatTheCallerMayRead(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	read := []remote.Scope{remote.ScopeRead}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{"alice": {Name: "alice", Scopes: read}})),
		remote.WithAuthorize(owners(map[string]string{"alice-old": "alice", "alice-new": "alice", "bob-newest": "bob"})),
	)
	alice := dial(t, url, withToken("alice", nil))
	caps, err := alice.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !caps.Features.Search {
		t.Fatalf("features %+v: a memory store is searched", caps.Features)
	}
	if got := hitSessions(t, alice, agentsession.Query{Text: "secret"}); !slices.Equal(got, []string{"alice-new", "alice-old"}) {
		t.Errorf("alice's search: %v, want her two sessions, newest first", got)
	}
	if got := hitSessions(t, alice, agentsession.Query{Text: "bob"}); len(got) != 0 {
		t.Errorf("alice found bob's secret: %v", got)
	}
	if got := hitSessions(t, alice, agentsession.Query{Text: "secret", Filter: agentsession.ListFilter{Limit: 1}}); !slices.Equal(got, []string{"alice-new"}) {
		t.Errorf("alice's search of one session: %v, want her newest", got)
	}
}

// A search naming a member the server does not know is refused, never
// answered as though the member were not there.
func TestSearchRefusesAnUnknownMember(t *testing.T) {
	url := server(t, agentsession.NewMemoryStore(), remote.WithInsecureNoAuth())
	for _, body := range []string{`{"query":"x","any":true}`, `{"limit":-1}`, `{"filter":{"limit":-2}}`} {
		resp, err := http.Post(url+"/v1/sessions/search", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		p := problemOf(t, resp)
		resp.Body.Close()
		if p.Code != remote.CodeBadRequest {
			t.Errorf("%s: %s, want bad_request", body, p.Code)
		}
	}
}

// The wire carries every hit member, and a hit's ts as written.
func TestSearchStreamsHits(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	url := server(t, st, remote.WithInsecureNoAuth())
	resp, err := http.Post(url+"/v1/sessions/search", "application/json", strings.NewReader(`{"query":"BOB","roles":["user"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	var hit, end map[string]any
	if err := dec.Decode(&hit); err != nil {
		t.Fatal(err)
	}
	if err := dec.Decode(&end); err != nil {
		t.Fatal(err)
	}
	if hit["session"] != "bob-newest" || hit["role"] != "user" || hit["snippet"] != "bob's secret plan" ||
		!strings.HasPrefix(hit["entry"].(string), "sha256:") || hit["ts"] == nil || end["end"] != true {
		t.Errorf("stream %v then %v", hit, end)
	}
}

// A client of a server that serves no search scans through List and
// Read instead, and answers the same.
func TestSearchFallsBackToAScan(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	h := remote.NewHandler(st, remote.WithInsecureNoAuth())
	t.Cleanup(func() { h.Close() })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions/search" {
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 501, "code": remote.CodeUnsupported, "title": "not served"})
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	c := dial(t, srv.URL)
	if got := hitSessions(t, c, agentsession.Query{Text: "secret"}); !slices.Equal(got, []string{"bob-newest", "alice-new", "alice-old"}) {
		t.Errorf("a scan over the client: %v", got)
	}
}
