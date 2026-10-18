package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/openresponses"
)

// TestRedactedCodes: a redacted-only store's refusal travels as RFC
// 0003's not_redacted (403), and matches the library's sentinel on the
// client, for a create and for a native append.
func TestRedactedCodes(t *testing.T) {
	ctx := context.Background()
	c := serve(t, agentsession.NewMemoryStore(agentsession.MemoryRedactedOnly()), nil)
	check := func(what string, err error) {
		t.Helper()
		var p *remote.Problem
		if !errors.As(err, &p) || p.Code != remote.CodeNotRedacted || p.Status != http.StatusForbidden || !errors.Is(err, agentsession.ErrNotRedacted) {
			t.Errorf("%s: %v, want not_redacted 403 matching ErrNotRedacted", what, err)
		}
	}
	_, err := c.Create(ctx, agentsession.Header{})
	check("create of an original", err)
	if _, err := c.Create(ctx, agentsession.Header{ID: "r", Redacted: true}); err != nil {
		t.Fatalf("create of a redacted session: %v", err)
	}
	_, err = c.Append(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("native")))
	check("a native append", err)

	// A created header RFC 0001 calls malformed is bad_entry, its reason
	// the redacted_from rule.
	_, err = c.Create(ctx, agentsession.Header{ID: "self", Redacted: true, RedactedFrom: &agentsession.RedactedFrom{Session: "self"}})
	var p *remote.Problem
	if !errors.As(err, &p) || p.Code != remote.CodeBadEntry || p.Status != http.StatusUnprocessableEntity || !errors.Is(err, agentsession.ErrRedactedFrom) {
		t.Errorf("a create naming itself in redacted_from: %v, want bad_entry 422 matching ErrRedactedFrom", err)
	}
}

// TestClientRedactedOnly: a client set to send only redacted entries
// refuses, before it sends anything, to create a session whose header
// lacks redacted, to append or write an entry without it, and to push a
// session holding either, so it fails closed against a server whose
// store is not so set. Its server fails the test on any request.
func TestClientRedactedOnly(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the client sent %s %s", r.Method, r.URL.Path)
		http.Error(w, "no", http.StatusTeapot)
	}))
	defer srv.Close()
	c, err := remote.Dial(srv.URL, remote.WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "o"}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Create of an original: %v, want ErrNotRedacted", err)
	}
	local, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := local.Create(ctx, agentsession.Header{ID: "o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append(ctx, "o", agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, local, c, "o", cas.PushOptions{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Push of an original: %v, want ErrNotRedacted", err)
	}
	// A native entry: refused by Append and Write, and a redacted session
	// resumed with one is refused by Push.
	if _, err := c.Append(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("native"))); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Append of a native entry: %v, want ErrNotRedacted", err)
	}
	if _, err := c.Write(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("native"))); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Write of a native entry: %v, want ErrNotRedacted", err)
	}
	if _, err := local.Create(ctx, agentsession.Header{ID: "r", Redacted: true}); err != nil {
		t.Fatal(err)
	}
	marked := agentsession.NewItemEntry(openresponses.UserText("marked"))
	if err := agentsession.SetRedacted(marked, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append(ctx, "r", marked); err != nil {
		t.Fatal(err)
	}
	if _, err := local.Append(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("resumed"))); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, local, c, "r", cas.PushOptions{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Push of a session resumed natively: %v, want ErrNotRedacted", err)
	}
}

// TestFetchIntoRedactedOnly: a fetch from a server is admitted by the
// local store's rules, so one that holds only redacted sessions takes
// no original from it.
func TestFetchIntoRedactedOnly(t *testing.T) {
	ctx := context.Background()
	backing, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := serve(t, backing, backing.Close)
	if _, err := w.Create(ctx, agentsession.Header{ID: "o"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Append(ctx, "o", agentsession.NewItemEntry(openresponses.UserText("hi"))); err != nil {
		t.Fatal(err)
	}
	local, err := cas.Open(t.TempDir(), cas.WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	if _, err := remote.Fetch(ctx, local, w.Client, "o"); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("Fetch of an original: %v, want ErrNotRedacted", err)
	}
}

// TestPushValidatesHeader: a pushed header is held to the rules an
// import holds a file's to, before any object is looked for: a
// redacted_from without redacted is bad_entry with the reason
// redacted_from, which matches ErrRedactedFrom on the client, and
// nothing lands.
func TestPushValidatesHeader(t *testing.T) {
	backing, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := serve(t, backing, backing.Close)
	h := agentsession.New(agentsession.Header{ID: "x"}).Header()
	h.RedactedFrom = &agentsession.RedactedFrom{Session: "o"}
	hdr, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	push := func(hdr string) (int, remote.Problem) {
		body := `{"header":` + hdr + `,"entries":[],"prefix":[],"head":{"expected":null,"to":null,"force":false},"mark":"mirror","refs":[]}`
		resp, err := http.Post(w.url+"/v1/sessions/x/push", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var p remote.Problem
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, p
	}
	if status, p := push(string(hdr)); status != http.StatusUnprocessableEntity || p.Code != remote.CodeBadEntry || !errors.Is(&p, agentsession.ErrRedactedFrom) {
		t.Errorf("push of a redacted_from without redacted: %d %+v, want 422 bad_entry matching ErrRedactedFrom", status, p)
	}
	// A header missing what every header carries is malformed too.
	if status, p := push(`{"type":"session","id":"x","format":"` + agentsession.Format + `"}`); status != http.StatusUnprocessableEntity || p.Code != remote.CodeBadEntry {
		t.Errorf("push of a header with no created_at: %d %+v, want 422 bad_entry", status, p)
	}
	if _, err := backing.Read(context.Background(), "x"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("the refused push left the session: %v", err)
	}
}

// A durable follow of a 0.12 redacted session that a redactor resumes:
// the first append raises the header and carries redacted, which 0.13
// defines. The replica is built from the server's session, whose header
// Read raised in memory, so it reads the entry as redacted and holds
// the store's format. (The non-durable follow is the conformance
// suite's RaiseFollowed case.)
func TestADurableFollowReadsARaisedEntry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	url := server(t, st, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "s", Format: "agentsession/0.12", Redacted: true}); err != nil {
		t.Fatal(err)
	}
	reader := dial(t, url)
	seen := make(chan agentsession.Change, 8)
	go func() {
		for ch, err := range reader.FollowCommitted(ctx, "s", "") {
			if err != nil {
				return
			}
			seen <- ch
		}
	}()
	if ch := <-seen; ch.Kind != agentsession.Snapshot {
		t.Fatalf("the first change: %v", ch.Kind)
	}
	e := agentsession.NewItemEntry(openresponses.UserText("[redacted]"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 5, To: 5}}); err != nil {
		t.Fatal(err)
	}
	id, err := c.Append(ctx, "s", e)
	if err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case ch := <-seen:
			got, ok := ch.Session.Entry(id)
			if !ok {
				continue
			}
			if spans, ok := agentsession.RedactedOf(got); !ok || len(spans) != 1 || spans[0] != (agentsession.Span{From: 5, To: 5}) {
				t.Errorf("at the %v change the entry reads as redacted %v %v, want [5]", ch.Kind, spans, ok)
			}
			if f := ch.Session.Header().Format; f != agentsession.Format {
				t.Errorf("at the %v change the replica is at %s, want %s", ch.Kind, f, agentsession.Format)
			}
			return
		case <-ctx.Done():
			t.Fatal("the follow never showed the append")
		}
	}
}

// A path read decodes each entry from its line alone, as UnmarshalEntry
// does, since the read carries no header: in a 0.12 session an entry
// holding redacted reads as redacted there, and GateEntry with the
// format of the session's header reads it as that minor does.
func TestAPathReadIsGatedByTheCaller(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, agentsession.Header{ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	e := agentsession.NewItemEntry(openresponses.UserText("hi"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, s.ID(), e); err != nil {
		t.Fatal(err)
	}
	st.Close()
	lowerHeaderFile(t, filepath.Join(root, "sessions", "s", "header"), "agentsession/0.12")
	st, err = cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	url := server(t, st, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	p, err := c.Path(ctx, "s", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 1 {
		t.Fatalf("%d entries", len(p.Entries))
	}
	pe := p.Entries[0].Entry
	if _, ok := agentsession.RedactedOf(pe); !ok {
		t.Error("the path entry was gated, though the read carries no header")
	}
	info, err := c.Session(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if info.Header.Format != "agentsession/0.12" {
		t.Fatalf("the session is at %s", info.Header.Format)
	}
	agentsession.GateEntry(info.Header.Format, pe)
	if spans, ok := agentsession.RedactedOf(pe); ok {
		t.Errorf("after GateEntry the entry reads as redacted %v", spans)
	}
}

// TestClientRedactedOnlyHeaderless: a client set to send only redacted
// entries refuses a marked entry to a session whose header does not
// carry redacted once it has the lease and the session, before any
// write: its server grants the lease and serves the session, and fails
// the test on an append or any other write.
func TestClientRedactedOnlyHeaderless(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	if _, err := st.Create(ctx, agentsession.Header{ID: "o"}); err != nil {
		t.Fatal(err)
	}
	h := remote.NewHandler(st, remote.WithInsecureNoAuth(), remote.WithLinger(0))
	defer h.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write := r.Method != http.MethodGet && !strings.HasSuffix(r.URL.Path, "/lease")
		if write {
			t.Errorf("the client sent %s %s", r.Method, r.URL.Path)
			http.Error(w, "no", http.StatusTeapot)
			return
		}
		h.ServeHTTP(w, r)
	}))
	defer srv.Close()
	c, err := remote.Dial(srv.URL, remote.WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	marked := agentsession.NewItemEntry(openresponses.UserText("marked"))
	if err := agentsession.SetRedacted(marked, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write(ctx, "o", marked); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("a marked entry to a session without redacted: %v, want ErrNotRedacted", err)
	}
	if s, err := st.Read(ctx, "o"); err != nil || s.Len() != 0 {
		t.Errorf("the server's session after the refusal: %v", err)
	}
}
