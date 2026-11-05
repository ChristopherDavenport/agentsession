package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
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

// noSearch serves st but answers its search unsupported, as a server
// without the capability does, and counts the listings it serves.
func noSearch(t *testing.T, st agentsession.Store) (url string, lists *atomic.Int32) {
	t.Helper()
	h := remote.NewHandler(st, remote.WithInsecureNoAuth())
	t.Cleanup(func() { h.Close() })
	lists = new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/sessions/search":
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 501, "code": remote.CodeUnsupported, "title": "not served"})
			return
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
			lists.Add(1)
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(func() { srv.CloseClientConnections(); srv.Close() })
	return srv.URL, lists
}

// searchErr runs a search of c and returns its first error.
func searchErr(c *remote.Client, q agentsession.Query) error {
	for _, err := range agentsession.Search(context.Background(), c, q) {
		if err != nil {
			return err
		}
	}
	return nil
}

// A client of a server that serves no search answers unsupported and
// lists nothing: a scan in the server's place reads in full every
// session the filter selects, so it is the caller's to ask for, with
// WithSearchScan, and then it answers as the server would have.
func TestSearchScansOnlyWhenAsked(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	url, lists := noSearch(t, st)
	if err := searchErr(dial(t, url), agentsession.Query{Text: "secret"}); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("search of a server without search: %v, want ErrUnsupported", err)
	}
	if n := lists.Load(); n != 0 {
		t.Errorf("the client listed the store %d times without being asked to scan", n)
	}
	if got := hitSessions(t, dial(t, url, remote.WithSearchScan()), agentsession.Query{Text: "secret"}); !slices.Equal(got, []string{"bob-newest", "alice-new", "alice-old"}) {
		t.Errorf("a scan over the client: %v", got)
	}
}

// A query carrying its own read check cannot be sent, so it too is
// scanned here only when asked.
func TestSearchWithReadableScansOnlyWhenAsked(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	url := server(t, st, remote.WithInsecureNoAuth())
	q := agentsession.Query{Text: "secret", Readable: func(id string) bool { return id != "bob-newest" }}
	if err := searchErr(dial(t, url), q); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("a search carrying Readable: %v, want ErrUnsupported", err)
	}
	if got := hitSessions(t, dial(t, url, remote.WithSearchScan()), q); !slices.Equal(got, []string{"alice-new", "alice-old"}) {
		t.Errorf("a scan with a read check: %v", got)
	}
}

// listOnly is a store with no Reader, which a server can neither
// search nor read a session of.
type listOnly struct{ agentsession.Store }

// A scan against a server that cannot read a session fails with the
// server's answer to the search, not with the first read the scan
// tried.
func TestSearchScanOfAnUnreadableStore(t *testing.T) {
	st := agentsession.NewMemoryStore()
	searchSessions(t, st)
	url := server(t, listOnly{st}, remote.WithInsecureNoAuth())
	err := searchErr(dial(t, url, remote.WithSearchScan()), agentsession.Query{Text: "secret"})
	var p *remote.Problem
	if !errors.As(err, &p) || p.Code != remote.CodeUnsupported || p.Title != "the store cannot be searched" {
		t.Errorf("scan of a store the server cannot read: %v, want the search's own unsupported", err)
	}
}
