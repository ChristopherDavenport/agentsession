package remote_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// logged sends the server's store faults to the test's log.
func logged(t *testing.T) remote.Option {
	return remote.WithErrorLog(func(err error) { t.Log("store fault:", err) })
}

// casStore is a cas store in a temporary directory, closed with the
// test.
func casStore(t *testing.T) *cas.Store {
	t.Helper()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustAppend(t *testing.T, st agentsession.Store, id string, e agentsession.Entry) string {
	t.Helper()
	got, err := st.Append(context.Background(), id, e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func mustCreate(t *testing.T, st agentsession.Store, h agentsession.Header) {
	t.Helper()
	if _, err := st.Create(context.Background(), h); err != nil {
		t.Fatal(err)
	}
}

// counting counts the requests a client sends, by method and the first
// segment of their path after /v1.
type counting struct {
	mu   sync.Mutex
	hits map[string]int
}

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/")
	first, _, _ := strings.Cut(rest, "/")
	c.mu.Lock()
	if c.hits == nil {
		c.hits = map[string]int{}
	}
	c.hits[r.Method+" "+first]++
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}

func (c *counting) take(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.hits[key]
	delete(c.hits, key)
	return n
}

// held is what a receiver holds of a session: its entries' IDs in log
// order, its head and its mark.
type heldSession struct {
	entries    []string
	head, mark string
}

func holding(t *testing.T, st *cas.Store, id string) heldSession {
	t.Helper()
	s, err := st.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var h heldSession
	for _, e := range s.Entries() {
		h.entries = append(h.entries, e.Base().ID)
	}
	if h.mark, h.head, err = st.MarkOf(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return h
}

// same reports whether two exchanges did the same, apart from the
// refs, which sameRefs compares.
func sameExchange(a, b cas.Exchange) bool {
	return a.Created == b.Created && a.Admitted == b.Admitted && a.Head == b.Head && a.HeadMoved == b.HeadMoved &&
		a.Forced == b.Forced && a.Why == b.Why && a.Handover == b.Handover && len(a.Refs) == len(b.Refs)
}

// A push through the server does what RFC 0002's push between two stores
// in one process does: the same exchange reported, the same log, head and
// mark at the receiver, for a new session with a ref, the same session
// pushed again after appends, a fork with its prefix, a session naming a
// media blob, a push whose expected head is wrong, and a handover.
func TestAPushThroughTheServerIsRFC0002sPush(t *testing.T) {
	ctx := context.Background()
	local, inproc, served := casStore(t), casStore(t), casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	both := func(name, id string, opts cas.PushOptions) {
		t.Helper()
		want, werr := local.Push(ctx, inproc, id, opts)
		got, gerr := remote.Push(ctx, local, c, id, opts)
		if (werr == nil) != (gerr == nil) {
			t.Fatalf("%s: in process %v, through the server %v", name, werr, gerr)
		}
		if !sameExchange(got, want) {
			t.Errorf("%s: through the server %+v, in process %+v", name, got, want)
		}
		for i := range want.Refs {
			if got.Refs[i].Moved != want.Refs[i].Moved || got.Refs[i].Target != want.Refs[i].Target {
				t.Errorf("%s: ref %d through the server %+v, in process %+v", name, i, got.Refs[i], want.Refs[i])
			}
		}
		if g, w := holding(t, served, id), holding(t, inproc, id); !slices.Equal(g.entries, w.entries) || g.head != w.head || g.mark != w.mark {
			t.Errorf("%s: the server holds %+v, the in-process receiver %+v", name, g, w)
		}
	}

	mustCreate(t, local, agentsession.Header{ID: "o"})
	ids := []string{mustAppend(t, local, "o", user("one")), mustAppend(t, local, "o", user("two")), mustAppend(t, local, "o", user("three"))}
	if err := local.UpdateRef(ctx, "main", agentsession.RefTarget{}, agentsession.RefTarget{Session: "o"}, "test"); err != nil {
		t.Fatal(err)
	}
	both("new session", "o", cas.PushOptions{Refs: []cas.RefPush{{Name: "main"}}})
	if r, err := served.ResolveRef(ctx, "main"); err != nil || r.Session != "o" {
		t.Errorf("the pushed ref at the server: %+v, %v", r, err)
	}

	head := ids[2]
	ids = append(ids, mustAppend(t, local, "o", user("four")), mustAppend(t, local, "o", user("five")))
	both("appended", "o", cas.PushOptions{Expected: head})
	both("pushed again", "o", cas.PushOptions{Expected: ids[4]})
	mustAppend(t, local, "o", user("six"))
	both("stale expected", "o", cas.PushOptions{Expected: ids[1]})

	mustCreate(t, local, agentsession.Header{ID: "f", Base: ids[1]})
	mustAppend(t, local, "f", user("fork"))
	both("fork", "f", cas.PushOptions{Expected: ids[1]})

	blob, err := local.PutBlob(ctx, []byte("image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	mustCreate(t, local, agentsession.Header{ID: "m", Media: agentsession.MediaSidecar})
	mustAppend(t, local, "m", user("see sidecar:"+blob))
	both("media", "m", cas.PushOptions{})
	if data, err := served.Blob(ctx, blob); err != nil || string(data) != "image bytes" {
		t.Errorf("the blob at the server: %q, %v", data, err)
	}

	// A handover makes the receiver the record and the sender a mirror.
	// The sender can hand a session over once, so each receiver gets its
	// own session and the two are compared by what was done.
	for _, id := range []string{"h1", "h2"} {
		mustCreate(t, local, agentsession.Header{ID: id})
		mustAppend(t, local, id, user("handed over"))
	}
	want, err := local.Push(ctx, inproc, "h1", cas.PushOptions{Handover: true})
	if err != nil {
		t.Fatal(err)
	}
	got, err := remote.Push(ctx, local, c, "h2", cas.PushOptions{Handover: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Created != want.Created || got.Admitted != want.Admitted || got.HeadMoved != want.HeadMoved || !got.Handover || !want.Handover {
		t.Errorf("handover: through the server %+v, in process %+v", got, want)
	}
	for _, s := range []struct {
		st   *cas.Store
		id   string
		mark string
	}{{inproc, "h1", cas.MarkRecord}, {served, "h2", cas.MarkRecord}, {local, "h1", cas.MarkMirror}, {local, "h2", cas.MarkMirror}} {
		if m, _, err := s.st.MarkOf(ctx, s.id); err != nil || m != s.mark {
			t.Errorf("%s's mark: %q, %v; want %s", s.id, m, err, s.mark)
		}
	}
	// A mirror pushes nothing.
	if _, err := remote.Push(ctx, local, c, "h2", cas.PushOptions{}); !errors.Is(err, cas.ErrNotRecord) {
		t.Errorf("a push from a mirror: %v, want ErrNotRecord", err)
	}
}

// A push uploads only the objects the server cannot already show the
// caller: all of a new session's, none when nothing changed, and an
// appended entry's alone after it.
func TestAPushSendsOnlyWhatTheServerLacks(t *testing.T) {
	ctx := context.Background()
	local, served := casStore(t), casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	tr := &counting{}
	c := dial(t, url, remote.WithHTTPClient(&http.Client{Transport: tr}))
	mustCreate(t, local, agentsession.Header{ID: "o"})
	for _, text := range []string{"one", "two", "three"} {
		mustAppend(t, local, "o", user(text))
	}
	x, err := remote.Push(ctx, local, c, "o", cas.PushOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := tr.take("PUT objects"); n != 6 {
		t.Errorf("the first push uploaded %d objects, want 6: three envelopes and three contents", n)
	}
	if _, err := remote.Push(ctx, local, c, "o", cas.PushOptions{Expected: x.Head}); err != nil {
		t.Fatal(err)
	}
	if n := tr.take("PUT objects"); n != 0 {
		t.Errorf("a push of what the server holds uploaded %d objects", n)
	}
	mustAppend(t, local, "o", user("four"))
	if _, err := remote.Push(ctx, local, c, "o", cas.PushOptions{Expected: x.Head}); err != nil {
		t.Fatal(err)
	}
	if n := tr.take("PUT objects"); n != 2 {
		t.Errorf("a push of one new entry uploaded %d objects, want its envelope and content", n)
	}
	// An entry whose content the server holds sends its envelope alone.
	mustAppend(t, local, "o", user("one"))
	x, err = remote.Push(ctx, local, c, "o", cas.PushOptions{Expected: holding(t, served, "o").head})
	if err != nil {
		t.Fatal(err)
	}
	if n := tr.take("PUT objects"); n != 1 {
		t.Errorf("a push of an entry repeating a held content uploaded %d objects, want 1", n)
	}
	if !x.HeadMoved {
		t.Errorf("the head did not move: %+v", x)
	}
}

// An object whose bytes do not hash to its name is refused, and so is
// a push naming an object the server cannot find for the caller; a
// client checks an object it reads against its hash.
func TestAnObjectIsCheckedAgainstItsHash(t *testing.T) {
	ctx := context.Background()
	served := casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	good := agentsession.HashBytes([]byte(`{"text":"a"}`))
	err := c.PutObject(ctx, cas.SpaceContents, good, []byte(`{"text":"b"}`))
	if !errors.Is(err, cas.ErrBadObject) {
		t.Errorf("a tampered upload: %v, want bad_object", err)
	}
	var p *remote.Problem
	if errors.As(err, &p) && (p.Code != remote.CodeBadObject || p.Status != http.StatusUnprocessableEntity) {
		t.Errorf("a tampered upload: %+v", p)
	}
	if held, err := served.HasObject(ctx, cas.SpaceContents, good); err != nil || held {
		t.Errorf("a tampered upload was stored: %v, %v", held, err)
	}

	// A push naming an entry it never uploaded is missing_object, with
	// the hash, and admits nothing.
	local := casStore(t)
	mustCreate(t, local, agentsession.Header{ID: "o"})
	id := mustAppend(t, local, "o", user("one"))
	body := `{"header":{"type":"session","id":"o","format":"` + agentsession.Format + `","created_at":"2026-10-16T00:00:00Z","payload":"` + agentsession.Payload + `"},"entries":["` + id + `"],"prefix":[],"head":{"expected":null,"to":"` + id + `"},"mark":"mirror"}`
	resp := send(t, http.MethodPost, url+"/v1/sessions/o/push", nil, body)
	p2 := problemOf(t, resp)
	if p2.Code != remote.CodeMissingObject || p2.Missing == nil || !slices.Contains(p2.Missing.Entries, id) {
		t.Errorf("a push naming an object never uploaded: %+v", p2)
	}
	if _, err := served.Read(ctx, "o"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a refused push left a session: %v", err)
	}
}

// A handover whose head would not move is refused with head_moved: before
// anything lands for a session the server lacks, and after the entries
// land, the mark unchanged, for one it holds. Declaring the record is a
// compare-and-swap on the head, and a mirror cannot be leased until it
// is the record.
func TestALostHeadIsRefusedWithHeadMoved(t *testing.T) {
	ctx := context.Background()
	local, served := casStore(t), casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	mustCreate(t, local, agentsession.Header{ID: "o"})
	first := mustAppend(t, local, "o", user("one"))

	if _, err := remote.Push(ctx, local, c, "o", cas.PushOptions{Expected: first, Handover: true}); !errors.Is(err, remote.ErrHeadMoved) {
		t.Errorf("a handover of a new session from the wrong head: %v, want head_moved", err)
	}
	if _, err := served.Read(ctx, "o"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("a refused handover of a new session left it: %v", err)
	}
	if _, err := remote.Push(ctx, local, c, "o", cas.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	second := mustAppend(t, local, "o", user("two"))
	x, err := remote.Push(ctx, local, c, "o", cas.PushOptions{Expected: second, Handover: true})
	var p *remote.Problem
	if !errors.As(err, &p) || p.Code != remote.CodeHeadMoved || headOrNone(p.Head) != first {
		t.Fatalf("a handover from the wrong head: %v (%+v)", err, p)
	}
	if x.Admitted != 1 || x.HeadMoved || x.Handover {
		t.Errorf("a failed handover's exchange: %+v, want the entry admitted and nothing else", x)
	}
	if h := holding(t, served, "o"); h.mark != cas.MarkMirror || h.head != first || len(h.entries) != 2 {
		t.Errorf("after a failed handover the server holds %+v", h)
	}
	if m, _, _ := local.MarkOf(ctx, "o"); m != cas.MarkRecord {
		t.Errorf("after a failed handover the sender's mark is %s", m)
	}

	// The server holds a mirror, which cannot be leased.
	resp := send(t, http.MethodPost, url+"/v1/sessions/o/lease", nil, `{"holder":"x","ttl_ms":1000}`)
	if p := problemOf(t, resp); p.Code != remote.CodeMirror {
		t.Errorf("a lease on a mirror: %s, want mirror", p.Code)
	}
	if err := c.DeclareRecord(ctx, "o", "sole_copy", second); !errors.Is(err, remote.ErrHeadMoved) {
		t.Errorf("declaring the record from the wrong head: %v, want head_moved", err)
	} else if errors.As(err, &p); headOrNone(p.Head) != first {
		t.Errorf("declare-record's head_moved names %s, want %s", headOrNone(p.Head), first)
	}
	if err := c.DeclareRecord(ctx, "o", "sole_copy", first); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := served.MarkOf(ctx, "o"); m != cas.MarkRecord {
		t.Errorf("after declare-record the server's mark is %s", m)
	}
	l := leaseOn(t, url, "o", "replacement", time.Minute)
	resp = send(t, http.MethodPost, url+"/v1/sessions/o/entries", map[string]string{"Lease-Token": l.Token, "Session-Format": agentsession.Format}, `{"type":"label","label":"after"}`+"\n")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("an append under the replacement's lease: %d %+v", resp.StatusCode, problemOf(t, resp))
	}
}

// A handover to the client makes the server a mirror, under the lease,
// which ends with it, when its head is the one named.
func TestAHandoverToTheClientMakesTheServerAMirror(t *testing.T) {
	ctx := context.Background()
	served := casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	head, err := c.Append(ctx, "s", user("one"))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.HandOver(ctx, "s", "sha256:"+strings.Repeat("0", 64)); !errors.Is(err, remote.ErrHeadMoved) {
		t.Errorf("a handover from the wrong head: %v, want head_moved", err)
	}
	if err := c.HandOver(ctx, "s", head); err != nil {
		t.Fatal(err)
	}
	info, err := c.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if info.Mark != cas.MarkMirror {
		t.Errorf("after a handover the server's mark is %q", info.Mark)
	}
	if _, err := c.Append(ctx, "s", user("two")); !errors.Is(err, remote.ErrMirror) {
		t.Errorf("an append after the handover: %v, want mirror", err)
	}
}

// Objects and exchange have their scopes: reading an object needs read,
// uploading, negotiating, pushing and declaring the record need
// exchange, a push's refs need refs as well, and a handover is a write.
func TestExchangeNeedsItsScopes(t *testing.T) {
	ctx := context.Background()
	local, served := casStore(t), casStore(t)
	auth := tokenAuth(map[string]remote.Principal{
		"reader":   {Name: "reader", Scopes: []remote.Scope{remote.ScopeRead}},
		"exchange": {Name: "exchange", Scopes: []remote.Scope{remote.ScopeExchange}},
		"both":     {Name: "both", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeExchange}},
		"refs":     {Name: "refs", Scopes: []remote.Scope{remote.ScopeExchange, remote.ScopeRefs}},
	})
	url := server(t, served, remote.WithAuthenticator(auth))
	mustCreate(t, local, agentsession.Header{ID: "o"})
	id := mustAppend(t, local, "o", user("one"))
	if err := local.UpdateRef(ctx, "main", agentsession.RefTarget{}, agentsession.RefTarget{Session: "o"}, "test"); err != nil {
		t.Fatal(err)
	}
	as := func(token string) *remote.Client { return dial(t, url, withToken(token, nil)) }
	scoped := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, remote.ErrForbidden) {
			t.Errorf("%s: %v, want scope", what, err)
		}
	}
	scoped("a reader's push", func() error { _, err := remote.Push(ctx, local, as("reader"), "o", cas.PushOptions{}); return err }())
	scoped("a reader's upload", as("reader").PutObject(ctx, cas.SpaceContents, agentsession.HashBytes([]byte("{}")), []byte("{}")))
	scoped("a reader's declare-record", as("reader").DeclareRecord(ctx, "o", "sole_copy", id))
	scoped("a push's refs without refs", func() error {
		_, err := remote.Push(ctx, local, as("exchange"), "o", cas.PushOptions{Refs: []cas.RefPush{{Name: "main"}}})
		return err
	}())
	if _, err := remote.Push(ctx, local, as("refs"), "o", cas.PushOptions{Refs: []cas.RefPush{{Name: "main"}}}); err != nil {
		t.Fatalf("a push with exchange and refs: %v", err)
	}
	_, err := as("exchange").Object(ctx, cas.SpaceEntries, id)
	scoped("reading an object without read", err)
	if _, err := as("both").Object(ctx, cas.SpaceEntries, id); err != nil {
		t.Errorf("reading a held object with read: %v", err)
	}
	scoped("a handover without write", as("both").HandOver(ctx, "o", id))
}

// The server reports an object held, and serves it, only to a caller a
// session reaches it for, or the caller that uploaded it: a hash is not
// a capability. The client fetches an object once.
func TestAnObjectIsServedOnlyToWhomItIsReachable(t *testing.T) {
	ctx := context.Background()
	local, served := casStore(t), casStore(t)
	auth := tokenAuth(map[string]remote.Principal{
		"a": {Name: "a", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeExchange}},
		"b": {Name: "b", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeExchange}},
	})
	url := server(t, served, remote.WithAuthenticator(auth))
	tr := &counting{}
	a := dial(t, url, withToken("a", tr))
	b := dial(t, url, withToken("b", nil))

	loose := []byte(`{"text":"uploaded, pushed nowhere"}`)
	hash := agentsession.HashBytes(loose)
	if err := a.PutObject(ctx, cas.SpaceContents, hash, loose); err != nil {
		t.Fatal(err)
	}
	ask := remote.Objects{Contents: []string{hash}}
	if m, err := a.Missing(ctx, ask); err != nil || len(m.Contents) != 0 {
		t.Errorf("the uploader's missing: %+v, %v; want it held", m, err)
	}
	if m, err := b.Missing(ctx, ask); err != nil || !slices.Equal(m.Contents, []string{hash}) {
		t.Errorf("another caller's missing: %+v, %v; want it missing", m, err)
	}
	if _, err := b.Object(ctx, cas.SpaceContents, hash); !errors.Is(err, cas.ErrNoObject) {
		t.Errorf("another caller's read of an upload: %v, want no_object", err)
	}

	mustCreate(t, local, agentsession.Header{ID: "o"})
	id := mustAppend(t, local, "o", user("one"))
	if _, err := remote.Push(ctx, local, a, "o", cas.PushOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Object(ctx, cas.SpaceEntries, id); err != nil {
		t.Errorf("an entry a session holds, read by another caller: %v", err)
	}
	tr.take("GET objects")
	for range 2 {
		if _, err := a.Object(ctx, cas.SpaceEntries, id); err != nil {
			t.Fatal(err)
		}
	}
	if n := tr.take("GET objects"); n != 1 {
		t.Errorf("two reads of one object fetched it %d times", n)
	}
}

// A store without exchange answers every exchange route with
// unsupported, and the capabilities say so; a cas store's say it has
// objects, exchange and marks.
func TestExchangeIsUnsupportedWithoutAStoreThatHasIt(t *testing.T) {
	ctx := context.Background()
	st, s, ids := seeded(t, 1)
	url := server(t, st, remote.WithInsecureNoAuth())
	for _, r := range []struct{ method, path, body string }{
		{http.MethodPut, "/v1/objects/contents/" + agentsession.HashBytes([]byte("{}")), "{}"},
		{http.MethodPost, "/v1/objects/missing", `{"entries":["` + ids[0] + `"]}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/push", `{}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/record", `{"basis":"sole_copy"}`},
		{http.MethodPost, "/v1/sessions/" + s.ID() + "/handover", `{"to":"client"}`},
		{http.MethodGet, "/v1/objects/entries/" + ids[0], ""},
	} {
		if p := problemOf(t, send(t, r.method, url+r.path, nil, r.body)); p.Code != remote.CodeUnsupported {
			t.Errorf("%s %s over a memory store: %s, want unsupported", r.method, r.path, p.Code)
		}
	}
	caps, err := dial(t, server(t, casStore(t), remote.WithInsecureNoAuth())).Capabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f := caps.Features; !f.Objects || !f.Exchange || !f.Marks || caps.Limits.UploadGraceMS <= 0 {
		t.Errorf("a cas store's capabilities: %+v", caps)
	}
}
