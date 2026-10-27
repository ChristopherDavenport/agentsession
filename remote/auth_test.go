package remote_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// tokenAuth is the tests' authenticator, standing in for a program's: a
// request whose Authorization header is "Bearer <token>" for a token in
// tokens is that token's principal, and any other is unauthenticated.
func tokenAuth(tokens map[string]remote.Principal) remote.Authenticator {
	return func(r *http.Request) (remote.Principal, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, known := tokens[tok]
		if !ok || tok == "" || !known {
			return remote.Principal{}, remote.ErrUnauthenticated
		}
		return p, nil
	}
}

// bearerTransport adds token to every request as a program's transport
// would, over next, or the default transport when next is nil.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// withToken is the client option that sends token through
// [bearerTransport] over next.
func withToken(token string, next http.RoundTripper) remote.ClientOption {
	return remote.WithHTTPClient(&http.Client{Transport: bearerTransport{token, next}})
}

// owners is the tests' authorizer, standing in for a program's: a
// session is its owner's, and a session it does not name is anyone's.
func owners(of map[string]string) remote.Authorizer {
	return func(_ context.Context, p remote.Principal, _ remote.Scope, session string) error {
		if o, ok := of[session]; ok && o != p.Name {
			return errors.New("not the owner")
		}
		return nil
	}
}

// Every request is authorized against the session it names, a follow
// resumed from another principal's event ID included: an event ID is
// not a capability (RFC 0003, Security considerations). A refused
// session reads as one the store does not hold, a listing leaves it
// out, and its objects are not reachable through it.
func TestAuthorizeEveryRequest(t *testing.T) {
	ctx := context.Background()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	mine, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := st.Append(ctx, mine.ID(), user("alice's secret"))
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	read := []remote.Scope{remote.ScopeRead}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"alice": {Name: "alice", Scopes: read},
			"bob":   {Name: "bob", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
		})),
		remote.WithAuthorize(owners(map[string]string{mine.ID(): "alice", theirs.ID(): "bob"})),
	)
	alice := map[string]string{"Authorization": "Bearer alice"}
	evs := rawFollow(t, url+"/v1/sessions/"+mine.ID()+"/follow", alice, 1)
	if len(evs) != 1 || evs[0].id == "" {
		t.Fatalf("alice's follow: %+v", evs)
	}
	eventID := evs[0].id

	sess := url + "/v1/sessions/" + mine.ID()
	for _, tc := range []struct {
		name, url string
		header    map[string]string
	}{
		{"state", sess, nil},
		{"path", sess + "/path", nil},
		{"follow", sess + "/follow", nil},
		{"follow resumed from alice's event", sess + "/follow", map[string]string{"Last-Event-ID": eventID}},
		{"follow after alice's event", sess + "/follow?after=" + eventID, nil},
		{"durable follow resumed", sess + "/follow?durable=true", map[string]string{"Last-Event-ID": eventID}},
		{"log", sess + "/log", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := map[string]string{"Authorization": "Bearer bob"}
			for k, v := range tc.header {
				h[k] = v
			}
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, tc.url, nil)
			for k, v := range h {
				req.Header.Set(k, v)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("bob was served alice's session")
			}
			if p := problemOf(t, resp); p.Code != remote.CodeNoSession {
				t.Errorf("code %s, want no_session", p.Code)
			}
		})
	}

	// The write side is authorized the same way: a lease on alice's
	// session is refused although bob holds the write scope.
	req, _ := http.NewRequest(http.MethodPost, sess+"/lease", nil)
	req.Header.Set("Authorization", "Bearer bob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if p := problemOf(t, resp); p.Code != remote.CodeNoSession {
		t.Errorf("bob's lease: %s, want no_session", p.Code)
	}

	// Nor can bob fork alice's session at her entry, named or not.
	for _, hdr := range []string{
		`{"type":"session","base":"` + entry + `"}`,
		`{"type":"session","base":"` + entry + `","parent_session":"` + mine.ID() + `"}`,
	} {
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/sessions", strings.NewReader(hdr))
		req.Header.Set("Authorization", "Bearer bob")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if p := problemOf(t, resp); p.Code != remote.CodeNoEntry && p.Code != remote.CodeNoSession {
			t.Errorf("bob's fork %s: %+v, want no_entry or no_session", hdr, p)
		}
	}

	// The listing leaves alice's session out of bob's.
	var listed []string
	for sum, err := range dial(t, url, withToken("bob", nil)).List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		listed = append(listed, sum.Header.ID)
	}
	if len(listed) != 1 || listed[0] != theirs.ID() {
		t.Errorf("bob lists %v, want only %s", listed, theirs.ID())
	}
	// Alice's entry is no object of bob's.
	obj := url + "/v1/objects/" + cas.SpaceEntries + "/" + entry
	if p := problemOf(t, get(t, obj, map[string]string{"Authorization": "Bearer bob"})); p.Code != remote.CodeNoObject {
		t.Errorf("bob's object read: %s, want no_object", p.Code)
	}
	if resp := get(t, obj, alice); resp.StatusCode != http.StatusOK {
		t.Errorf("alice's object read: status %d", resp.StatusCode)
	}
	// Alice still reads her own, fresh and resumed.
	if evs := rawFollow(t, sess+"/follow", map[string]string{"Authorization": "Bearer alice", "Last-Event-ID": eventID}, 0); evs != nil {
		t.Errorf("alice's resume: %+v", evs)
	}
}

// A ref reaches the session it names, so reading it is authorized
// against that session as every other read is: a ref to a session the
// caller may not read is no ref of theirs, in a resolve, a follow, a
// log and a listing, and a ref moved onto such a session reads, to a
// follower, as moved to none.
func TestAuthorizeRefReads(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	mine, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "alice/talk", agentsession.RefTarget{}, agentsession.RefTarget{Session: mine.ID()}, "created"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "bob/talk", agentsession.RefTarget{}, agentsession.RefTarget{Session: theirs.ID()}, "created"); err != nil {
		t.Fatal(err)
	}
	read := []remote.Scope{remote.ScopeRead}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"alice": {Name: "alice", Scopes: read},
			"bob":   {Name: "bob", Scopes: read},
		})),
		remote.WithAuthorize(owners(map[string]string{mine.ID(): "alice", theirs.ID(): "bob"})),
		remote.WithRefPoll(10*time.Millisecond),
	)
	bob := map[string]string{"Authorization": "Bearer bob"}
	for _, path := range []string{"/v1/refs/alice/talk", "/v1/refs/alice/talk?view=log", "/v1/refs/alice/talk?view=follow"} {
		if p := problemOf(t, get(t, url+path, bob)); p.Code != remote.CodeNoRef {
			t.Errorf("bob reads %s: %s, want no_ref", path, p.Code)
		}
	}
	var names []string
	for ref, err := range dial(t, url, withToken("bob", nil)).ListRefs(ctx, "") {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, ref.Name)
	}
	if len(names) != 1 || names[0] != "bob/talk" {
		t.Errorf("bob lists refs %v, want only bob/talk", names)
	}
	// Bob follows his ref; alice's session is not his to see, so a move
	// of it there reads as a move to none.
	fctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(fctx, http.MethodGet, url+"/v1/refs/bob/talk?view=follow", nil)
	req.Header.Set("Authorization", "Bearer bob")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	var datas []string
	moved := false
	for sc.Scan() && len(datas) < 2 {
		if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			datas = append(datas, d)
			if !moved {
				moved = true
				if err := st.UpdateRef(ctx, "bob/talk", agentsession.RefTarget{Session: theirs.ID()}, agentsession.RefTarget{Session: mine.ID()}, "moved"); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if len(datas) != 2 || !strings.Contains(datas[0], theirs.ID()) || strings.Contains(datas[1], mine.ID()) || !strings.Contains(datas[1], `"target":null`) {
		t.Errorf("bob's follow: %q, want his session and then no target", datas)
	}
}

// A ref write reaches the session the ref names now as well as the one
// it moves to: a principal that may not read the ref's target gets
// no_ref, never the target in a refusal, and cannot move the ref away
// with a target it learnt elsewhere.
func TestAuthorizeRefWrites(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	alices, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	bobs, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "team/talk", agentsession.RefTarget{}, agentsession.RefTarget{Session: alices.ID()}, "created"); err != nil {
		t.Fatal(err)
	}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"bob": {Name: "bob", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeRefs}},
		})),
		remote.WithAuthorize(owners(map[string]string{alices.ID(): "alice", bobs.ID(): "bob"})),
	)
	for _, expected := range []string{`null`, `{"session":"` + alices.ID() + `"}`} {
		req, _ := http.NewRequest(http.MethodPut, url+"/v1/refs/team/talk", strings.NewReader(`{"expected":`+expected+`,"next":{"session":"`+bobs.ID()+`"}}`))
		req.Header.Set("Authorization", "Bearer bob")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), remote.CodeNoRef) || strings.Contains(string(body), alices.ID()) {
			t.Errorf("bob's update from %s: %d %s, want no_ref naming nothing of alice's", expected, resp.StatusCode, body)
		}
	}
	if got, err := st.ResolveRef(ctx, "team/talk"); err != nil || got.Session != alices.ID() {
		t.Errorf("alice's ref is at %v, %v; want still %s", got, err, alices.ID())
	}
}
