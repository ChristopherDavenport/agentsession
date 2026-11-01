package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// send makes a request with a JSON body and the headers given.
func send(t *testing.T, method, url string, header map[string]string, body string) *http.Response {
	t.Helper()
	var rd *bytes.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rd)
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

type grantedLease struct {
	LeaseID string    `json:"lease_id"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// leaseOn takes a lease on the session over the wire, failing the test
// on a refusal.
func leaseOn(t *testing.T, url, id, holder string, ttl time.Duration) grantedLease {
	t.Helper()
	resp := send(t, http.MethodPost, url+"/v1/sessions/"+id+"/lease", nil, `{"holder":"`+holder+`","ttl_ms":`+jsonInt(ttl.Milliseconds())+`}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("acquire: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
	var l grantedLease
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		t.Fatal(err)
	}
	return l
}

func mustUint(t *testing.T, s string) uint64 {
	t.Helper()
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// line is an entry's line, as a client that leaves it incomplete sends
// it: no id, parent or ts, which the server fills.
func line(text string) string {
	b, err := agentsession.MarshalEntry(user(text))
	if err != nil {
		panic(err)
	}
	return string(b) + "\n"
}

// An append retried under the same Idempotency-Key lands once: the
// server fills an incomplete line differently each time, so without the
// key a retry would be another entry.
func TestARetriedAppendWithTheSameKeyLandsOnce(t *testing.T) {
	st := agentsession.NewMemoryStore()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	url := server(t, st, remote.WithInsecureNoAuth())
	l := leaseOn(t, url, s.ID(), "w", time.Minute)
	header := map[string]string{"Lease-Token": l.Token, "Idempotency-Key": "k1", "Content-Type": "application/x-ndjson"}
	var ids []string
	for range 2 {
		resp := send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/entries", header, line("once"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("append: %d %+v", resp.StatusCode, problemOf(t, resp))
		}
		var ar struct {
			Results []struct{ ID, Outcome string }
		}
		if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ar.Results[0].ID)
	}
	if ids[0] != ids[1] || s.Len() != 1 {
		t.Errorf("a retry under one key: ids %v, %d entries; want one entry", ids, s.Len())
	}
	// Another key is another append.
	header["Idempotency-Key"] = "k2"
	if resp := send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/entries", header, line("once")); resp.StatusCode != http.StatusOK {
		t.Fatalf("append under another key: %d", resp.StatusCode)
	}
	if s.Len() != 2 {
		t.Errorf("another key's append: %d entries, want 2", s.Len())
	}
}

// A writer whose lease expired and was taken is refused by its token,
// lease_lost, however late its write arrives; the new holder's token is
// greater.
func TestAWriterWhoseLeaseWasTakenIsFenced(t *testing.T) {
	st := agentsession.NewMemoryStore()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	url := server(t, st, remote.WithInsecureNoAuth(), remote.WithLeaseLimits(time.Second, time.Minute))
	old := leaseOn(t, url, s.ID(), "old", 50*time.Millisecond)
	// Held: another acquire is refused, and told who holds it.
	resp := send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/lease", nil, `{"holder":"new"}`)
	if p := problemOf(t, resp); p.Code != remote.CodeSessionLocked || p.Holder != "old" {
		t.Fatalf("acquire of a held session: %+v, want session_locked naming old", p)
	}
	time.Sleep(100 * time.Millisecond)
	taken := leaseOn(t, url, s.ID(), "new", time.Minute)
	if a, b := mustUint(t, old.Token), mustUint(t, taken.Token); b <= a {
		t.Errorf("the new token %d is not greater than the old %d", b, a)
	}
	write := func(token string) *http.Response {
		return send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/entries", map[string]string{"Lease-Token": token}, line("x"))
	}
	if p := problemOf(t, write(old.Token)); p.Code != remote.CodeLeaseLost {
		t.Errorf("a write under the taken lease: %+v, want lease_lost", p)
	}
	if resp := write(taken.Token); resp.StatusCode != http.StatusOK {
		t.Errorf("a write under the new lease: %d", resp.StatusCode)
	}
	if p := problemOf(t, write("")); p.Code != remote.CodeLeaseLost {
		t.Errorf("a write with no token: %+v, want lease_lost", p)
	}
	if s.Len() != 1 {
		t.Errorf("session holds %d entries, want the new holder's one", s.Len())
	}
}

// A lease renews under its ID and outlives its first time; released, it
// is gone at once, and renewing it then is lease_lost. Only the ID
// authorizes either, never the token.
func TestALeaseRenewsAndReleases(t *testing.T) {
	st := agentsession.NewMemoryStore()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	url := server(t, st, remote.WithInsecureNoAuth())
	l := leaseOn(t, url, s.ID(), "w", 150*time.Millisecond)
	lease := url + "/v1/sessions/" + s.ID() + "/lease"
	for range 4 {
		time.Sleep(60 * time.Millisecond)
		if resp := send(t, http.MethodPut, lease, map[string]string{"Lease-ID": l.LeaseID}, `{"ttl_ms":150}`); resp.StatusCode != http.StatusOK {
			t.Fatalf("renew: %d %+v", resp.StatusCode, problemOf(t, resp))
		}
	}
	// 240ms on, past the first 150ms: still held.
	if p := problemOf(t, send(t, http.MethodPost, lease, nil, `{"holder":"other"}`)); p.Code != remote.CodeSessionLocked {
		t.Fatalf("a renewed lease was taken: %+v", p)
	}
	if p := problemOf(t, send(t, http.MethodPut, lease, map[string]string{"Lease-ID": l.Token}, "")); p.Code != remote.CodeLeaseLost {
		t.Errorf("a renew by token: %+v, want lease_lost", p)
	}
	if resp := send(t, http.MethodDelete, lease, map[string]string{"Lease-ID": l.LeaseID}, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	if p := problemOf(t, send(t, http.MethodPut, lease, map[string]string{"Lease-ID": l.LeaseID}, "")); p.Code != remote.CodeLeaseLost {
		t.Errorf("a renew after release: %+v, want lease_lost", p)
	}
	leaseOn(t, url, s.ID(), "other", time.Minute)
}

// A ref compare-and-swap that loses is ref_moved with the target the
// ref holds; the client returns the library's RefMovedError.
func TestARefUpdateThatLosesIsRefMoved(t *testing.T) {
	st := agentsession.NewMemoryStore()
	ctx := context.Background()
	a, _ := st.Create(ctx, agentsession.Header{})
	b, _ := st.Create(ctx, agentsession.Header{})
	url := server(t, st, remote.WithInsecureNoAuth())
	c := dial(t, url)
	if err := c.UpdateRef(ctx, "chat/one", agentsession.RefTarget{}, agentsession.RefTarget{Session: a.ID()}, "first"); err != nil {
		t.Fatal(err)
	}
	resp := send(t, http.MethodPut, url+"/v1/refs/chat/one", nil, `{"expected":null,"next":{"session":"`+b.ID()+`","entry":""}}`)
	if p := problemOf(t, resp); p.Code != remote.CodeRefMoved || p.Target == nil || p.Target.Session != a.ID() {
		t.Errorf("a losing create-if-absent: %+v, want ref_moved holding %s", p, a.ID())
	}
	err := c.UpdateRef(ctx, "chat/one", agentsession.RefTarget{}, agentsession.RefTarget{Session: b.ID()}, "")
	var moved *agentsession.RefMovedError
	if !errors.As(err, &moved) || !errors.Is(err, agentsession.ErrRefMoved) || moved.Current.Session != a.ID() {
		t.Errorf("the client's losing update: %v, want a RefMovedError holding %s", err, a.ID())
	}
}

// Writes need write scope, a ref update refs scope and a lease break
// admin scope; a reader with read scope alone reads.
func TestWritesNeedTheirScopes(t *testing.T) {
	st := agentsession.NewMemoryStore()
	s, _ := st.Create(context.Background(), agentsession.Header{})
	url := server(t, st, remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
		"reader": {Name: "r", Scopes: []remote.Scope{remote.ScopeRead}},
		"writer": {Name: "w", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
	})))
	bearer := func(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }
	for _, tc := range []struct {
		method, path, body string
	}{
		{http.MethodPost, "/v1/sessions", `{}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/lease", `{}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/entries", line("x")},
		{http.MethodPut, "/v1/sessions/" + s.ID() + "/head", `{"to":"x"}`},
		{http.MethodDelete, "/v1/sessions/" + s.ID(), ""},
		{http.MethodPut, "/v1/refs/a", `{"next":null}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/lease/break", ""},
	} {
		if p := problemOf(t, send(t, tc.method, url+tc.path, bearer("reader"), tc.body)); p.Code != remote.CodeScope {
			t.Errorf("%s %s with read scope: %+v, want scope", tc.method, tc.path, p)
		}
	}
	// The writer may create and lease, and not update a ref.
	if resp := send(t, http.MethodPost, url+"/v1/sessions", bearer("writer"), `{"type":"session"}`); resp.StatusCode != http.StatusCreated {
		t.Errorf("a create with write scope: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
	if p := problemOf(t, send(t, http.MethodPut, url+"/v1/refs/a", bearer("writer"), `{"next":null}`)); p.Code != remote.CodeScope {
		t.Errorf("a ref update with write scope alone: %+v, want scope", p)
	}
	if p := problemOf(t, send(t, http.MethodPost, url+"/v1/sessions", nil, `{}`)); p.Code != remote.CodeAuth {
		t.Errorf("a create with no token: %+v, want auth", p)
	}
}

// Two clients write one session in turn: while the first holds it the
// second is refused, session_locked, and once the first releases it the
// second appends after the first's entries.
func TestTwoClientsWriteOneSessionInTurn(t *testing.T) {
	ctx := context.Background()
	url := server(t, agentsession.NewMemoryStore(), remote.WithInsecureNoAuth())
	one, two := dial(t, url, remote.WithHolder("one")), dial(t, url, remote.WithHolder("two"))
	t.Cleanup(func() { one.Close(); two.Close() })
	s, err := one.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := one.Append(ctx, s.ID(), user("from one"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := two.Append(ctx, s.ID(), user("from two")); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Fatalf("the second client's append while the first holds: %v, want ErrSessionLocked", err)
	}
	var p *remote.Problem
	if _, err := two.Open(ctx, s.ID()); !errors.As(err, &p) || p.Holder != "one" {
		t.Errorf("the refusal does not name the holder: %v", err)
	}
	if err := one.Release(s.ID()); err != nil {
		t.Fatal(err)
	}
	second, err := two.Append(ctx, s.ID(), user("from two"))
	if err != nil {
		t.Fatalf("the second client's append after the release: %v", err)
	}
	got, err := two.Read(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := got.Entry(second); got.Len() != 2 || !ok || e.Base().Parent != first {
		t.Errorf("the session holds %d entries, the second under %v; want both, in order", got.Len(), e)
	}
	// The first client's own session no longer follows; it opens again
	// and is refused while two holds.
	if _, err := one.Append(ctx, s.ID(), user("again")); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Errorf("the first client's append after releasing: %v, want ErrSessionLocked", err)
	}
}

// The writer's leaf steers its appends as a local store's shared
// session does, and the server's head follows by compare-and-swap; a
// head move whose expected head is stale is head_moved with the head.
func TestTheWritersLeafMovesTheServersHead(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	url := server(t, st, remote.WithInsecureNoAuth())
	c := dial(t, url)
	t.Cleanup(func() { c.Close() })
	s, err := c.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := c.Append(ctx, s.ID(), user("a"))
	if _, err := c.Append(ctx, s.ID(), user("b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	alt, err := c.Write(ctx, s.ID(), user("alt"))
	if err != nil || alt.Outcome != agentsession.Continued {
		t.Fatalf("an append after a branch: %+v, %v; want continued", alt, err)
	}
	info, err := c.Session(ctx, s.ID())
	if err != nil {
		t.Fatal(err)
	}
	if info.Head != alt.ID || s.Leaf() != alt.ID {
		t.Errorf("the server's head %s, the writer's leaf %s; want both %s", info.Head, s.Leaf(), alt.ID)
	}
	// A root, the writer's leaf reset, lands as a root.
	s.ResetLeaf()
	root, err := c.Write(ctx, s.ID(), user("root"))
	if err != nil {
		t.Fatal(err)
	}
	if roots := s.Roots(); len(roots) != 2 || roots[1] != root.ID {
		t.Errorf("roots %v, want the new root second", roots)
	}
	if err := c.Release(s.ID()); err != nil {
		t.Fatal(err)
	}

	// Over the wire: a head move from a stale expected head.
	l := leaseOn(t, url, s.ID(), "raw", time.Minute)
	resp := send(t, http.MethodPut, url+"/v1/sessions/"+s.ID()+"/head", map[string]string{"Lease-Token": l.Token}, `{"expected":"`+a+`","to":"`+a+`"}`)
	p := problemOf(t, resp)
	if p.Code != remote.CodeHeadMoved || p.Head == nil || *p.Head != root.ID {
		t.Errorf("a head move from a stale head: %+v, want head_moved naming %s", p, root.ID)
	}
	resp = send(t, http.MethodPut, url+"/v1/sessions/"+s.ID()+"/head", map[string]string{"Lease-Token": l.Token}, `{"expected":"`+root.ID+`","to":"sha256:`+strings.Repeat("0", 64)+`"}`)
	if p := problemOf(t, resp); p.Code != remote.CodeNoEntry {
		t.Errorf("a head move to an entry not held: %+v, want no_entry", p)
	}
}

// Over the cas store, which has a commit of its own, the capabilities
// say so, an append asked to commit answers durable, and the head moves
// durably by compare-and-swap.
func TestCommitOverCAS(t *testing.T) {
	ctx := context.Background()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Release(s.ID()); err != nil {
		t.Fatal(err)
	}
	url := server(t, st, remote.WithInsecureNoAuth())
	caps, err := dial(t, url).Capabilities(ctx)
	if err != nil || !caps.Features.Commit || !caps.Features.Leases {
		t.Fatalf("capabilities over cas: %+v, %v; want commit and leases", caps.Features, err)
	}
	l := leaseOn(t, url, s.ID(), "w", time.Minute)
	resp := send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/entries?commit=true", map[string]string{"Lease-Token": l.Token}, line("a")+line("b"))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("append with commit: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
	var ar struct {
		Results []struct {
			ID      string
			Outcome string
			Durable bool
		}
		Head *string
	}
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		t.Fatal(err)
	}
	if len(ar.Results) != 2 || !ar.Results[0].Durable || !ar.Results[1].Durable || ar.Results[1].Outcome != "continued" || ar.Head == nil || *ar.Head != ar.Results[1].ID {
		t.Fatalf("append with commit: %+v", ar)
	}
	first := ar.Results[0].ID
	resp = send(t, http.MethodPut, url+"/v1/sessions/"+s.ID()+"/head", map[string]string{"Lease-Token": l.Token}, `{"expected":"`+*ar.Head+`","to":"`+first+`","durable":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("durable head move: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
	info, err := dial(t, url).Session(ctx, s.ID())
	if err != nil || info.Head != first {
		t.Errorf("the head after the move: %s, %v; want %s", info.Head, err, first)
	}
}

// A second continued_in link naming another successor is refused with a
// code of its own, superseded, so a client tells "another writer
// continued this session" from a store fault.
func TestASecondContinuationIsSuperseded(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), agentsession.NewLinkEntry(agentsession.RelContinuedIn, "first-successor")); err != nil {
		t.Fatal(err)
	}
	url := server(t, st, remote.WithInsecureNoAuth())
	b, err := agentsession.MarshalEntry(agentsession.NewLinkEntry(agentsession.RelContinuedIn, "second-successor"))
	if err != nil {
		t.Fatal(err)
	}
	l := leaseOn(t, url, s.ID(), "w", time.Minute)
	resp := send(t, http.MethodPost, url+"/v1/sessions/"+s.ID()+"/entries", map[string]string{"Lease-Token": l.Token}, string(b)+"\n")
	if p := problemOf(t, resp); p.Code != remote.CodeSuperseded || p.Status != http.StatusConflict {
		t.Errorf("a second continuation: %+v, want 409 superseded", p)
	}
	if resp := send(t, http.MethodDelete, url+"/v1/sessions/"+s.ID()+"/lease", map[string]string{"Lease-Id": l.LeaseID}, ""); resp.StatusCode/100 != 2 {
		t.Fatalf("release: %d", resp.StatusCode)
	}
	if _, err := dial(t, url).Append(ctx, s.ID(), agentsession.NewLinkEntry(agentsession.RelContinuedIn, "third-successor")); !errors.Is(err, agentsession.ErrSuperseded) {
		t.Errorf("through the client: %v, want ErrSuperseded", err)
	}
}

// On the wire null is the universal root: a head move to null rests the
// head on the empty context, and one that names no target is refused.
func TestAHeadMoveToNullIsTheRoot(t *testing.T) {
	ctx := context.Background()
	st, s, ids := seeded(t, 2)
	url := server(t, st, remote.WithInsecureNoAuth())
	l := leaseOn(t, url, s.ID(), "w", time.Minute)
	head := url + "/v1/sessions/" + s.ID() + "/head"
	lt := map[string]string{"Lease-Token": l.Token}
	if p := problemOf(t, send(t, http.MethodPut, head, lt, `{"expected":"`+ids[1]+`","durable":true}`)); p.Code != remote.CodeHeadTarget {
		t.Errorf("a move with no target: %s, want head_target", p.Code)
	}
	resp := send(t, http.MethodPut, head, lt, `{"expected":"`+ids[1]+`","to":null,"durable":true}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a move to null: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
	var out struct {
		Head *string `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || out.Head != nil {
		t.Errorf("a move to null answered head %v, %v; want null", out.Head, err)
	}
	if got, err := st.Read(ctx, s.ID()); err != nil || got.Leaf() != "" {
		t.Errorf("the store's head after a move to null: %v, %v; want the root", got.Leaf(), err)
	}
}
