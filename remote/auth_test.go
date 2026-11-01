package remote_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
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

// TestWriteScopeUploadsMedia is #254 over the wire: a writer, with write
// scope and no exchange scope, uploads the sidecar blob its item names,
// to a server over a store that keeps no objects but holds blobs; it
// may not upload any other object; and a server over a store that holds
// no blobs refuses a sidecar session.
func TestWriteScopeUploadsMedia(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	url := server(t, st, remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
		"w": {Name: "w", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
	})))
	c, err := remote.Dial(url, withToken("w", nil))
	if err != nil {
		t.Fatal(err)
	}
	h, err := c.PutBlob(ctx, []byte("image bytes"))
	if err != nil {
		t.Fatalf("a writer's media upload: %v", err)
	}
	if data, err := st.Blob(ctx, h); err != nil || string(data) != "image bytes" {
		t.Errorf("the store's blob: %q, %v", data, err)
	}
	if err := c.PutObject(ctx, cas.SpaceContents, h, []byte("image bytes")); !errors.Is(err, remote.ErrForbidden) {
		t.Errorf("a writer's content upload: %v, want ErrForbidden", err)
	}
	if _, err := c.Create(ctx, agentsession.Header{ID: "side", Media: agentsession.MediaSidecar}); err != nil {
		t.Errorf("a sidecar session over a store that holds blobs: %v", err)
	}

	bare := server(t, noBlobs{agentsession.NewMemoryStore()}, remote.WithInsecureNoAuth())
	bc, err := remote.Dial(bare)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bc.Create(ctx, agentsession.Header{ID: "side", Media: agentsession.MediaSidecar}); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("a sidecar session over a store that holds no blobs: %v, want ErrUnsupported", err)
	}
	if _, err := bc.PutBlob(ctx, []byte("x")); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("a media upload to a store that holds no blobs: %v, want ErrUnsupported", err)
	}
}

// noBlobs is a store with nothing but the Store interface: no blobs.
type noBlobs struct{ agentsession.Store }

// TestPushRevealsNoUnreadableBase: a push's pre-flight and the push
// itself authorize a fork's origin and base for reading before the
// store looks at them, so a base held only in a session the caller may
// not read answers as one the server does not hold: no bad_base for an
// origin whose media differs, where a hash nobody holds answers 204.
func TestPushRevealsNoUnreadableBase(t *testing.T) {
	ctx := context.Background()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	base, err := st.Append(ctx, "alices", user("alice's secret"))
	if err != nil {
		t.Fatal(err)
	}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"mallory": {Name: "mallory", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeExchange}},
		})),
		remote.WithAuthorize(owners(map[string]string{"alices": "alice"})),
	)
	c := dial(t, url, withToken("mallory", nil))
	unheld := agentsession.HashBytes([]byte("no entry"))
	for _, tc := range []struct{ name, base, parent string }{
		{"a hash nobody holds", unheld, ""},
		{"alice's entry, her session named", base, "alices"},
		{"alice's entry, found by hash", base, ""},
	} {
		fork := agentsession.New(agentsession.Header{ID: "m", Base: tc.base, ParentSession: tc.parent}).Header()
		if err := c.CheckPush(ctx, fork, nil, []string{tc.base}); err != nil {
			t.Errorf("CheckPush of a fork of %s: %v, want 204", tc.name, err)
		}
	}
	// The push itself, with the prefix the caller uploaded, answers as
	// for an origin the server does not hold.
	for _, sp := range []string{cas.SpaceEntries, cas.SpaceContents} {
		hash := base
		if sp == cas.SpaceContents {
			env, _ := st.Object(ctx, cas.SpaceEntries, base)
			hash, _ = cas.ContentOf(env)
		}
		data, err := st.Object(ctx, sp, hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.PutObject(ctx, sp, hash, data); err != nil {
			t.Fatal(err)
		}
	}
	hdr, err := json.Marshal(agentsession.New(agentsession.Header{ID: "m", Base: base, ParentSession: "alices"}).Header())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"header":` + string(hdr) + `,"entries":[],"prefix":["` + base + `"],"head":{"expected":null,"to":"` + base + `"},"mark":"mirror"}`
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/sessions/m/push", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer mallory")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("a push of a fork of alice's entry: %d %s, want 200 as for an origin not held", resp.StatusCode, b)
	}
}

// TestNamingABlobGrantsNothing is RFC 0003's rule that a hash is a name,
// not a capability, for sidecar media, over each store that holds
// blobs: mallory names alice's blob in an append, in an import of a
// file with no sidecar header and in a push, and each is refused with
// missing_object exactly as naming a blob nobody holds is, and his
// session reaches nothing by it. A blob he uploaded himself he may
// name, and alice still reads hers.
func TestNamingABlobGrantsNothing(t *testing.T) {
	stores := []struct {
		name string
		open func(t *testing.T) agentsession.Store
	}{
		{"memory", func(t *testing.T) agentsession.Store { return agentsession.NewMemoryStore() }},
		{"jsonl", func(t *testing.T) agentsession.Store {
			st, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		}},
		{"cas", func(t *testing.T) agentsession.Store {
			st, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) { testNamingABlobGrantsNothing(t, tc.open(t)) })
	}
}

func testNamingABlobGrantsNothing(t *testing.T, st agentsession.Store) {
	ctx := context.Background()
	secret := []byte("alice's picture")
	h, err := st.(agentsession.BlobWriter).PutBlob(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "alices", user("see sidecar:"+h)); err != nil {
		t.Fatal(err)
	}
	all := []remote.Scope{remote.ScopeRead, remote.ScopeWrite, remote.ScopeExchange}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"alice":   {Name: "alice", Scopes: all},
			"mallory": {Name: "mallory", Scopes: all},
		})),
		remote.WithAuthorize(owners(map[string]string{"alices": "alice", "mallorys": "mallory", "imported": "mallory", "plain": "mallory", "pushed": "mallory"})),
	)
	m := dial(t, url, withToken("mallory", nil))
	mallory := map[string]string{"Authorization": "Bearer mallory"}
	media := url + "/v1/objects/" + cas.SpaceMedia + "/"
	unreached := func(when string) {
		t.Helper()
		if p := problemOf(t, get(t, media+h, mallory)); p.Code != remote.CodeNoObject {
			t.Errorf("%s: mallory reads alice's blob: %s, want no_object", when, p.Code)
		}
	}
	absent := agentsession.HashBytes([]byte("a blob nobody holds"))
	// Each refusal is the one for a blob nobody holds.
	same := func(what string, try func(hash string) error) {
		t.Helper()
		for _, hash := range []string{h, absent} {
			if err := try(hash); !errors.Is(err, remote.ErrMissingObject) || !errors.Is(err, agentsession.ErrNoBlob) {
				t.Errorf("%s naming %s: %v, want ErrMissingObject and ErrNoBlob", what, hash, err)
			}
		}
		unreached("after " + what)
	}
	unreached("at the start")

	if _, err := m.Create(ctx, agentsession.Header{ID: "mallorys", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	same("an append", func(hash string) error {
		_, err := m.Append(ctx, "mallorys", user("see sidecar:"+hash))
		return err
	})
	same("an import with no sidecar header", func(hash string) error {
		s := agentsession.New(agentsession.Header{ID: "plain"})
		if _, err := s.Append(user("see sidecar:" + hash)); err != nil {
			t.Fatal(err)
		}
		_, err := m.Import(ctx, bytes.NewReader(fileOfSession(t, s)), true)
		return err
	})
	same("a sidecar import", func(hash string) error {
		s := agentsession.New(agentsession.Header{ID: "imported", Media: agentsession.MediaSidecar})
		if _, err := s.Append(user("see sidecar:" + hash)); err != nil {
			t.Fatal(err)
		}
		_, _, err := m.ImportMedia(ctx, bytes.NewReader(fileOfSession(t, s)), fstest.MapFS{}, true)
		return err
	})
	if _, ok := st.(*cas.Store); ok {
		local, err := cas.Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { local.Close() })
		if _, err := local.Create(ctx, agentsession.Header{ID: "pushed", Media: agentsession.MediaSidecar}); err != nil {
			t.Fatal(err)
		}
		e, err := local.Append(ctx, "pushed", user("see sidecar:"+h))
		if err != nil {
			t.Fatal(err)
		}
		// Mallory holds the entry and not the blob: he uploads what he
		// has and pushes.
		env, _ := local.Object(ctx, cas.SpaceEntries, e)
		ch, _ := cas.ContentOf(env)
		body, _ := local.Object(ctx, cas.SpaceContents, ch)
		if err := m.PutObject(ctx, cas.SpaceEntries, e, env); err != nil {
			t.Fatal(err)
		}
		if err := m.PutObject(ctx, cas.SpaceContents, ch, body); err != nil {
			t.Fatal(err)
		}
		pushed, _ := local.Open(ctx, "pushed")
		hdr, _ := json.Marshal(pushed.Header())
		req, _ := http.NewRequest(http.MethodPost, url+"/v1/sessions/pushed/push", strings.NewReader(`{"header":`+string(hdr)+`,"entries":["`+e+`"],"prefix":[],"head":{"expected":null,"to":"`+e+`"},"mark":"mirror"}`))
		req.Header.Set("Authorization", "Bearer mallory")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if p := problemOf(t, resp); p.Code != remote.CodeMissingObject || p.Missing == nil || len(p.Missing.Media) != 1 || p.Missing.Media[0] != h {
			t.Errorf("a push naming alice's blob: %+v, want missing_object naming it", p)
		}
		resp.Body.Close()
		unreached("after a push")
	}

	// A blob mallory uploads he may name, and read; alice reads hers.
	mine, err := m.PutBlob(ctx, []byte("mallory's picture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Append(ctx, "mallorys", user("see sidecar:"+mine)); err != nil {
		t.Errorf("an append naming mallory's own upload: %v", err)
	}
	if resp := get(t, media+mine, mallory); resp.StatusCode != http.StatusOK {
		t.Errorf("mallory reads his own blob: %d", resp.StatusCode)
	}
	if resp := get(t, media+h, map[string]string{"Authorization": "Bearer alice"}); resp.StatusCode != http.StatusOK {
		t.Errorf("alice reads her blob: %d", resp.StatusCode)
	}
}

// TestUnnamedUploadsSwept: a writer's media upload that no session comes
// to name is removed once older than the server's upload grace, over a
// store that keeps no objects but holds blobs, as RFC 0003's Objects
// has every upload collected; one a session names stays. Without it a
// principal with write scope could fill the store with blobs nothing
// names.
func TestUnnamedUploadsSwept(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(t *testing.T) agentsession.Store
	}{
		{"memory", func(t *testing.T) agentsession.Store { return agentsession.NewMemoryStore() }},
		{"jsonl", func(t *testing.T) agentsession.Store {
			st, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			st := tc.open(t)
			grace := 200 * time.Millisecond
			url := server(t, st,
				remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
					"w": {Name: "w", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
				})),
				remote.WithUploadGrace(grace),
			)
			c := dial(t, url, withToken("w", nil))
			if _, err := c.Create(ctx, agentsession.Header{ID: "s", Media: agentsession.MediaSidecar}); err != nil {
				t.Fatal(err)
			}
			named, err := c.PutBlob(ctx, []byte("a picture the session shows"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Append(ctx, "s", user("see sidecar:"+named)); err != nil {
				t.Fatal(err)
			}
			stray, err := c.PutBlob(ctx, []byte("a blob nothing names"))
			if err != nil {
				t.Fatal(err)
			}
			time.Sleep(grace + 100*time.Millisecond)
			late, err := c.PutBlob(ctx, []byte("an upload after the grace"))
			if err != nil {
				t.Fatal(err)
			}
			br := st.(agentsession.BlobReader)
			if _, err := br.Blob(ctx, stray); !errors.Is(err, agentsession.ErrNoBlob) {
				t.Errorf("an upload nothing names, past the grace: %v, want ErrNoBlob", err)
			}
			for _, h := range []string{named, late} {
				if _, err := br.Blob(ctx, h); err != nil {
					t.Errorf("blob %s: %v", h, err)
				}
			}
		})
	}
}

// TestForkOriginUnreadable: an import and a create find a fork's origin
// only among the sessions the caller may read, over every store, so a
// base held only in another's session answers as a base nobody holds:
// mallory's import of a fork of alice's entry in another media is
// taken, as one of an entry nobody holds is, and a fork he creates from
// it names none of alice's sessions. A push is held to the media of
// the origin he may read, the first session holding the base that he
// may read, so his own fork of her entry cannot be forked again in
// another media, whether or not the push names it.
func TestForkOriginUnreadable(t *testing.T) {
	stores := []struct {
		name string
		open func(t *testing.T) agentsession.Store
	}{
		{"memory", func(t *testing.T) agentsession.Store { return agentsession.NewMemoryStore() }},
		{"jsonl", func(t *testing.T) agentsession.Store {
			st, err := jsonl.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		}},
		{"cas", func(t *testing.T) agentsession.Store {
			st, err := cas.Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			return st
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) { testForkOriginUnreadable(t, tc.open(t)) })
	}
}

// forkFile is the file of a fork of origin at base with the given ID,
// naming no parent_session, in the media given.
func forkFile(t *testing.T, origin *agentsession.Session, base, id, media string) []byte {
	t.Helper()
	f, err := agentsession.Fork(origin, base, agentsession.Header{ID: id})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Append(user("on " + id)); err != nil {
		t.Fatal(err)
	}
	head, rest, _ := bytes.Cut(fileOfSession(t, f), []byte("\n"))
	var hdr map[string]any
	if err := json.Unmarshal(head, &hdr); err != nil {
		t.Fatal(err)
	}
	delete(hdr, "parent_session")
	hdr["media"] = media
	head, err = json.Marshal(hdr)
	if err != nil {
		t.Fatal(err)
	}
	return append(append(head, '\n'), rest...)
}

func testForkOriginUnreadable(t *testing.T, st agentsession.Store) {
	ctx := context.Background()
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices"}); err != nil {
		t.Fatal(err)
	}
	x, err := st.Append(ctx, "alices", user("alice's base"))
	if err != nil {
		t.Fatal(err)
	}
	alices, err := st.(agentsession.Reader).Read(ctx, "alices")
	if err != nil {
		t.Fatal(err)
	}
	nobodys := agentsession.New(agentsession.Header{ID: "nobodys"})
	y, err := nobodys.Append(user("a base nobody holds"))
	if err != nil {
		t.Fatal(err)
	}
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"mallory": {Name: "mallory", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite, remote.ScopeExchange}},
		})),
		remote.WithAuthorize(owners(map[string]string{"alices": "alice"})),
	)
	m := dial(t, url, withToken("mallory", nil))
	for _, tc := range []struct {
		what   string
		origin *agentsession.Session
		base   string
		id     string
	}{
		{"a base nobody holds", nobodys, y, "m0"},
		{"alice's base", alices, x, "m1"},
	} {
		_, err := m.Import(ctx, bytes.NewReader(forkFile(t, tc.origin, tc.base, tc.id, agentsession.MediaSidecar)), true)
		if err != nil {
			t.Errorf("an import of a fork of %s in another media: %v, want it taken", tc.what, err)
		}
	}
	// A fork created at alice's entry, which mallory reaches through his
	// own m1, names his origin and not hers.
	f, err := m.Create(ctx, agentsession.Header{ID: "m2", Base: x})
	if err != nil {
		t.Fatalf("a fork of the base mallory holds on his prefix: %v", err)
	}
	if p := f.Header().ParentSession; p == "alices" {
		t.Errorf("mallory's fork names alice's session as its parent")
	}
	if _, ok := st.(*cas.Store); !ok {
		return
	}
	// m1 holds alice's entry on its prefix and is sidecar; a fork of it
	// in another media is refused, whether or not it names m1, as it
	// would be if alice's session were not there.
	for _, parent := range []string{"m1", ""} {
		fork := agentsession.New(agentsession.Header{ID: "m3", Base: x, ParentSession: parent}).Header()
		if err := m.CheckPush(ctx, fork, nil, []string{x}); !errors.Is(err, agentsession.ErrMediaDiffers) {
			t.Errorf("CheckPush of an inline fork of m1's prefix, parent %q: %v, want ErrMediaDiffers", parent, err)
		}
	}
}
