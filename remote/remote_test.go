package remote_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/openresponses"
)

func user(text string) agentsession.Entry {
	return agentsession.NewItemEntry(openresponses.UserText(text))
}

// seeded is a memory store holding one session of n entries, and its
// entry IDs.
func seeded(t *testing.T, n int) (*agentsession.MemoryStore, *agentsession.Session, []string) {
	t.Helper()
	st := agentsession.NewMemoryStore()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range n {
		id, err := st.Append(context.Background(), s.ID(), user(fmt.Sprint("entry ", i)))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return st, s, ids
}

// server serves st with opts and returns its URL.
func server(t *testing.T, st agentsession.Store, opts ...remote.Option) string {
	t.Helper()
	h := remote.NewHandler(st, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		h.Close()
	})
	return srv.URL
}

func dial(t *testing.T, url string, opts ...remote.ClientOption) *remote.Client {
	t.Helper()
	c, err := remote.Dial(url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func get(t *testing.T, url string, header map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func problemOf(t *testing.T, resp *http.Response) remote.Problem {
	t.Helper()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/problem+json") {
		t.Fatalf("status %d with content type %q, want a problem", resp.StatusCode, ct)
	}
	var p remote.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if p.Status != resp.StatusCode {
		t.Errorf("problem status %d on a %d response", p.Status, resp.StatusCode)
	}
	return p
}

// Every request is authenticated: with no authenticator nothing is
// served, a request without a token is `auth`, and one whose principal
// lacks the read scope is `scope`; the capabilities need a principal and
// no scope.
func TestAuth(t *testing.T) {
	st, s, _ := seeded(t, 1)
	open := server(t, st)
	if p := problemOf(t, get(t, open+"/v1/", nil)); p.Code != remote.CodeAuth {
		t.Errorf("no authenticator: %s, want auth", p.Code)
	}
	url := server(t, st, remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
		"reader": {Name: "r", Scopes: []remote.Scope{remote.ScopeRead}},
		"nobody": {Name: "n"},
	})))
	tests := []struct {
		name, token, path, code string
	}{
		{"no token", "", "/v1/sessions/" + s.ID(), remote.CodeAuth},
		{"wrong token", "guess", "/v1/sessions/" + s.ID(), remote.CodeAuth},
		{"no read scope", "nobody", "/v1/sessions/" + s.ID(), remote.CodeScope},
		{"no read scope, follow", "nobody", "/v1/sessions/" + s.ID() + "/follow", remote.CodeScope},
		{"no read scope, refs", "nobody", "/v1/refs", remote.CodeScope},
		{"capabilities need no scope", "nobody", "/v1/", ""},
		{"read", "reader", "/v1/sessions/" + s.ID(), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{}
			if tc.token != "" {
				h["Authorization"] = "Bearer " + tc.token
			}
			resp := get(t, url+tc.path, h)
			if tc.code == "" {
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status %d, want 200", resp.StatusCode)
				}
				return
			}
			if p := problemOf(t, resp); p.Code != tc.code {
				t.Errorf("code %s, want %s", p.Code, tc.code)
			}
		})
	}
	// The client's errors match the sentinels.
	if _, err := dial(t, url, withToken("nobody", nil)).Read(context.Background(), s.ID()); !errors.Is(err, remote.ErrForbidden) {
		t.Errorf("Read without the scope: %v, want ErrForbidden", err)
	}
	if _, err := dial(t, url).Capabilities(context.Background()); !errors.Is(err, remote.ErrUnauthenticated) {
		t.Errorf("Capabilities without a token: %v, want ErrUnauthenticated", err)
	}
	if _, err := dial(t, url, withToken("reader", nil)).Read(context.Background(), s.ID()); err != nil {
		t.Errorf("Read with the scope: %v", err)
	}
}

// noRefs is a store that keeps no refs: the memory store's sessions,
// reads and follows, and nothing else of it.
type noRefs struct{ m *agentsession.MemoryStore }

func (s noRefs) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	return s.m.Create(ctx, h)
}
func (s noRefs) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	return s.m.Open(ctx, id)
}
func (s noRefs) Append(ctx context.Context, id string, e agentsession.Entry) (string, error) {
	return s.m.Append(ctx, id, e)
}
func (s noRefs) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return s.m.List(ctx, f)
}
func (s noRefs) Delete(ctx context.Context, id string) error { return s.m.Delete(ctx, id) }
func (s noRefs) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	return s.m.Read(ctx, id)
}
func (s noRefs) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return s.m.Follow(ctx, id, from)
}

// The capabilities say what the store supports through this server, and
// a feature it lacks is 501 `unsupported`.
func TestCapabilitiesDifferByStore(t *testing.T) {
	ctx := context.Background()
	withRefs := dial(t, server(t, agentsession.NewMemoryStore(), remote.WithInsecureNoAuth()))
	without := server(t, noRefs{agentsession.NewMemoryStore()}, remote.WithInsecureNoAuth())
	a, err := withRefs.Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	b, err := dial(t, without).Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !a.Features.Refs || b.Features.Refs {
		t.Errorf("refs: %v over a store that keeps them, %v over one that does not", a.Features.Refs, b.Features.Refs)
	}
	for _, caps := range []remote.Capabilities{a, b} {
		f := caps.Features
		// Over a memory store: leases, and the shallow path, which the
		// server reads from the session; no log of records, no committed
		// view, no objects or exchange; a ref followed where refs are kept.
		if f.Objects || f.Exchange || f.Marks || f.Commit || f.CommittedView || f.Log || !f.Shallow || !f.Leases || f.RefFollow != f.Refs {
			t.Errorf("features %+v: over a memory store, leases, shallow and refs as kept", f)
		}
		if caps.Limits.SnapshotBytes <= 0 {
			t.Errorf("no snapshot bound published: %+v", caps.Limits)
		}
		if caps.Reads != agentsession.Format || caps.Limits.FollowBuffer <= 0 {
			t.Errorf("capabilities %+v", caps)
		}
	}
	for _, path := range []string{"/v1/refs", "/v1/refs/main"} {
		if p := problemOf(t, get(t, without+path, nil)); p.Code != remote.CodeUnsupported {
			t.Errorf("%s over a store without refs: %s, want unsupported", path, p.Code)
		}
	}
	if _, err := dial(t, without).ResolveRef(ctx, "main"); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("ResolveRef over a store without refs: %v, want ErrUnsupported", err)
	}
}

// What this server does not serve, or its store lacks, is 501
// `unsupported`.
func TestUnservedIsUnsupported(t *testing.T) {
	st, s, _ := seeded(t, 1)
	url := server(t, st, remote.WithInsecureNoAuth())
	if err := st.UpdateRef(context.Background(), "main", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID()}, ""); err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct{ method, path string }{
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/record"},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/commit"},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/entries?commit=true"},
		{http.MethodGet, "/v1/sessions/" + s.ID() + "/log"},
		{http.MethodGet, "/v1/sessions/" + s.ID() + "/log?durable=true"},
		{http.MethodGet, "/v1/sessions/" + s.ID() + "/follow?durable=true"},
		{http.MethodGet, "/v1/objects/entries/sha256:00"},
		{http.MethodPost, "/v1/objects/missing"},
	} {
		req, _ := http.NewRequest(r.method, url+r.path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		p := problemOf(t, resp)
		resp.Body.Close()
		if p.Code != remote.CodeUnsupported {
			t.Errorf("%s %s: %s, want unsupported", r.method, r.path, p.Code)
		}
	}
}

// The client reads and follows a session another goroutine appends to,
// and sees every entry once and in order; a follow resumed from the
// cursor it stopped at goes on with no gap and no repeat.
func TestFollowSeesEveryAppendAcrossAResume(t *testing.T) {
	st, s, ids := seeded(t, 3)
	c := dial(t, server(t, st, remote.WithInsecureNoAuth()))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	want := append([]string(nil), ids...)
	const more = 40
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range more {
			id, err := st.Append(ctx, s.ID(), user(fmt.Sprint("more ", i)))
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			want = append(want, id)
			mu.Unlock()
			if i%7 == 0 {
				time.Sleep(time.Millisecond)
			}
		}
	}()

	var got []string
	var cursor agentsession.Cursor
	follow := func(from agentsession.Cursor, upTo int) {
		t.Helper()
		for ch, err := range c.Follow(ctx, s.ID(), from) {
			if err != nil {
				t.Fatal(err)
			}
			switch ch.Kind {
			case agentsession.Snapshot:
				if from != "" {
					t.Fatalf("a resumed follow began with a snapshot")
				}
				for _, e := range ch.Session.Entries() {
					got = append(got, e.Base().ID)
				}
			case agentsession.Appended:
				got = append(got, ch.ID)
				if p := ch.Session.Path(ch.ID); len(p) == 0 {
					t.Fatalf("the session after %s does not hold it", ch.ID)
				}
			case agentsession.Reset:
				t.Fatalf("a reset at %d entries", len(got))
			}
			cursor = ch.Cursor
			if len(got) >= upTo {
				return
			}
		}
	}
	follow("", 3+more/2)
	follow(cursor, 3+more)
	<-done
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("followed %d entries, want the %d appended, in order:\n got %v\nwant %v", len(got), len(want), got, want)
	}
	// What the client holds reads as the store does.
	read, err := c.Read(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(read.Entries()); n != len(want) || read.Leaf() != want[len(want)-1] {
		t.Errorf("Read holds %d entries at leaf %s, want %d at %s", n, read.Leaf(), len(want), want[len(want)-1])
	}
}

// A reader slow enough to fall behind a follow with a buffer of one is
// cut with `overflow` and resumes from its last event by itself: the
// caller sees every entry, in order. (The cut itself is
// TestAClientThatFallsBehindIsCut's.)
func TestAFollowThatFallsBehindResumes(t *testing.T) {
	st, s, ids := seeded(t, 1)
	url := server(t, st, remote.WithInsecureNoAuth(), remote.WithFollowBuffer(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const more = 200
	want := append([]string(nil), ids...)
	late := make(chan []string, 1)
	var got []string
	for ch, err := range dial(t, url).Follow(ctx, s.ID(), "") {
		if err != nil {
			t.Fatal(err)
		}
		switch ch.Kind {
		case agentsession.Snapshot:
			for _, e := range ch.Session.Entries() {
				got = append(got, e.Base().ID)
			}
			go func() {
				var appended []string
				defer func() { late <- appended }()
				for i := range more {
					id, err := st.Append(ctx, s.ID(), user(fmt.Sprint("late ", i)))
					if err != nil {
						return
					}
					appended = append(appended, id)
				}
			}()
		case agentsession.Appended:
			got = append(got, ch.ID)
			if len(got)%10 == 0 {
				time.Sleep(5 * time.Millisecond)
			}
		}
		if len(got) == len(ids)+more {
			break
		}
	}
	want = append(want, <-late...)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("followed %d entries, want %d, in order", len(got), len(want))
	}
}

// The path carries each envelope inline, so the chain verifies from the
// response alone: the entry is the hash of its envelope, and the
// envelope names the hash of its content. contents=none leaves the
// contents out.
func TestThePathVerifiesFromTheResponse(t *testing.T) {
	st, s, ids := seeded(t, 3)
	url := server(t, st, remote.WithInsecureNoAuth())
	for _, contents := range []string{"all", "none"} {
		resp := get(t, url+"/v1/sessions/"+s.ID()+"/path?contents="+contents, nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
		sc := bufio.NewScanner(resp.Body)
		var entries []string
		parent := ""
		ended := false
		for i := 0; sc.Scan(); i++ {
			var l struct {
				Entry    string          `json:"entry"`
				Envelope json.RawMessage `json:"envelope"`
				Content  json.RawMessage `json:"content"`
				Head     *string         `json:"head"`
				Cursor   string          `json:"cursor"`
				End      bool            `json:"end"`
			}
			if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
				t.Fatal(err)
			}
			if l.End {
				ended = true
				break
			}
			if i == 0 && (l.Head == nil || *l.Head != ids[2] || l.Cursor == "") {
				t.Errorf("first line: head %v, cursor %q", l.Head, l.Cursor)
			}
			id, err := agentsession.HashRequestJSON(l.Envelope)
			if err != nil || id != l.Entry {
				t.Errorf("envelope hashes to %s, entry %s (%v)", id, l.Entry, err)
			}
			var env struct {
				Parent  *string `json:"parent"`
				Content string  `json:"content"`
			}
			if err := json.Unmarshal(l.Envelope, &env); err != nil {
				t.Fatal(err)
			}
			if got := headOrNone(env.Parent); got != parent {
				t.Errorf("entry %s has parent %q, want %q", l.Entry, got, parent)
			}
			switch contents {
			case "all":
				h, err := agentsession.HashRequestJSON(l.Content)
				if err != nil || h != env.Content {
					t.Errorf("content hashes to %s, envelope names %s (%v)", h, env.Content, err)
				}
			case "none":
				if string(l.Content) != "null" {
					t.Errorf("contents=none carries %s", l.Content)
				}
			}
			entries = append(entries, l.Entry)
			parent = l.Entry
		}
		if !ended {
			t.Error("the path has no end frame")
		}
		if strings.Join(entries, ",") != strings.Join(ids, ",") {
			t.Errorf("contents=%s: path %v, want %v", contents, entries, ids)
		}
	}
	if p := problemOf(t, get(t, url+"/v1/sessions/"+s.ID()+"/path?leaf=sha256:nope", nil)); p.Code != remote.CodeNoEntry {
		t.Errorf("a leaf the session does not hold: %s, want no_entry", p.Code)
	}
}

// A session's state, and its incarnation on every response about it; an
// If-Incarnation naming another is `session_replaced`.
func TestSessionStateAndIncarnation(t *testing.T) {
	st, s, ids := seeded(t, 2)
	url := server(t, st, remote.WithInsecureNoAuth())
	c := dial(t, url)
	info, err := c.Session(context.Background(), s.ID())
	if err != nil {
		t.Fatal(err)
	}
	inc, _ := agentsession.HeaderIdent(s.Header())
	if info.Header.ID != s.ID() || info.Head != ids[1] || info.Length != 2 || info.Incarnation != inc || info.Cursor == "" {
		t.Errorf("state %+v", info)
	}
	for _, path := range []string{"", "/path", "/follow"} {
		resp := get(t, url+"/v1/sessions/"+s.ID()+path, map[string]string{"If-Incarnation": "another"})
		if p := problemOf(t, resp); p.Code != remote.CodeSessionReplaced || p.Incarnation != inc {
			t.Errorf("%q with another incarnation: %+v", path, p)
		}
		if got := get(t, url+"/v1/sessions/"+s.ID()+path, map[string]string{"If-Incarnation": inc}); got.StatusCode != http.StatusOK || got.Header.Get("Incarnation") != inc {
			t.Errorf("%q with its incarnation: status %d, incarnation %q", path, got.StatusCode, got.Header.Get("Incarnation"))
		}
	}
	if _, err := c.Session(context.Background(), "no-such"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a missing session: %v, want ErrNoSession", err)
	}
}

// A store fault tells the client that and no more: its text can name
// the store's files.
func TestAStoreFaultSaysNothingOfTheStore(t *testing.T) {
	var logged []error
	url := server(t, failing{agentsession.NewMemoryStore()}, remote.WithInsecureNoAuth(), remote.WithErrorLog(func(err error) { logged = append(logged, err) }))
	resp := get(t, url+"/v1/sessions", nil)
	p := problemOf(t, resp)
	if p.Code != remote.CodeStoreFault || strings.Contains(p.Title+p.Detail, "/secret") {
		t.Errorf("problem %+v", p)
	}
	if len(logged) != 1 || !strings.Contains(logged[0].Error(), "/secret") {
		t.Errorf("logged %v, want the store's error", logged)
	}
}

func headOrNone(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

type failing struct{ *agentsession.MemoryStore }

func (failing) List(context.Context, agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		yield(agentsession.Summary{}, errors.New("open /secret/sessions: permission denied"))
	}
}

// A resume from an event ID that is none of this server's, or names a
// place the log no longer holds, is answered with a reset carrying the
// session afresh, never a refusal or a silent skip.
func TestAForeignCursorIsAReset(t *testing.T) {
	stores := map[string]func(t *testing.T) agentsession.Store{
		"memory": func(*testing.T) agentsession.Store { return agentsession.NewMemoryStore() },
		"jsonl": func(t *testing.T) agentsession.Store {
			st, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		},
		"cas": func(t *testing.T) agentsession.Store {
			st, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		},
	}
	for name, open := range stores {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			st := open(t)
			s, err := st.Create(ctx, agentsession.Header{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.Append(ctx, s.ID(), user("a")); err != nil {
				t.Fatal(err)
			}
			url := server(t, st, remote.WithInsecureNoAuth())
			for _, id := range []string{"not-an-event", "Zm9vOmJhcg.0"} {
				ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
				req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url+"/v1/sessions/"+s.ID()+"/follow", nil)
				req.Header.Set("Last-Event-ID", id)
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				sc := bufio.NewScanner(resp.Body)
				sc.Buffer(nil, 1<<20)
				first := ""
				for sc.Scan() {
					if v, ok := strings.CutPrefix(sc.Text(), "event: "); ok {
						first = v
						break
					}
				}
				resp.Body.Close()
				cancel()
				if resp.StatusCode != http.StatusOK || first != "reset" {
					t.Errorf("resume from %q: status %d, first event %q, want a reset", id, resp.StatusCode, first)
				}
			}
		})
	}
}

// A Read, a session's state and a fresh follow give what the store
// holds when they are asked, even while the session's shared follower
// runs behind it. Appends through the session Open returns ring no
// follower of the memory store's, which finds them only by its slow
// look, so the follower a running follow keeps is behind each of them.
func TestAReadRightAfterAnAppendSeesIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	st, s, _ := seeded(t, 1)
	c := dial(t, server(t, st, remote.WithInsecureNoAuth()))
	// A follow kept running keeps the session's follower alive.
	go func() {
		for _, err := range c.Follow(ctx, s.ID(), "") {
			if err != nil {
				return
			}
		}
	}()
	live, err := st.Open(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		id, err := live.Append(user(fmt.Sprint("quiet ", i)))
		if err != nil {
			t.Fatal(err)
		}
		read, err := c.Read(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := read.Entry(id); !ok || read.Leaf() != id {
			t.Fatalf("append %d: Read holds %d entries at leaf %s, not the append %s the store holds", i, len(read.Entries()), read.Leaf(), id)
		}
		info, err := c.Session(ctx, s.ID())
		if err != nil {
			t.Fatal(err)
		}
		if info.Length != i+2 || info.Head != id {
			t.Fatalf("append %d: state %d entries at %s, want %d at %s", i, info.Length, info.Head, i+2, id)
		}
	}
}
