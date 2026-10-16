package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
