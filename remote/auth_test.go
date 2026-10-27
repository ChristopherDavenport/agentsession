package remote_test

import (
	"context"
	"errors"
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
