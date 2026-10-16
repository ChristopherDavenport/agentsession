package remote_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/openresponses"
)

// TestRedactedCodes: the two refusals travel as RFC 0003's redacted
// (409) and not_redacted (403), and match the library's sentinels on
// the client.
func TestRedactedCodes(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name   string
		st     agentsession.Store
		h      agentsession.Header
		code   string
		status int
		want   error
	}{
		{"redacted", agentsession.NewMemoryStore(), agentsession.Header{Redacted: true}, remote.CodeRedacted, http.StatusConflict, agentsession.ErrRedacted},
		{"not_redacted", agentsession.NewMemoryStore(agentsession.MemoryRedactedOnly()), agentsession.Header{}, remote.CodeNotRedacted, http.StatusForbidden, agentsession.ErrNotRedacted},
	} {
		c := serve(t, tt.st, nil)
		_, err := c.Create(ctx, tt.h)
		var p *remote.Problem
		if !errors.As(err, &p) || p.Code != tt.code || p.Status != tt.status || !errors.Is(err, tt.want) {
			t.Errorf("%s: %v, want %s %d matching %v", tt.name, err, tt.code, tt.status, tt.want)
		}
	}
}

// TestClientRedactedOnly: a client set to send only redacted sessions
// refuses to create or push any other before it sends anything, so it
// fails closed against a server whose store is not so set. Its server
// is one nothing listens at: a request sent would fail otherwise.
func TestClientRedactedOnly(t *testing.T) {
	ctx := context.Background()
	c, err := remote.Dial("http://127.0.0.1:9", remote.WithRedactedOnly())
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
	h.RedactedFrom = &agentsession.RedactedFrom{Session: "o", Entries: 0}
	hdr, err := json.Marshal(h)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"header":` + string(hdr) + `,"entries":[],"prefix":[],"head":{"expected":null,"to":null,"force":false},"mark":"mirror","refs":[]}`
	resp, err := http.Post(w.url+"/v1/sessions/x/push", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var p remote.Problem
	if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusUnprocessableEntity || p.Code != remote.CodeBadEntry || !errors.Is(&p, agentsession.ErrRedactedFrom) {
		t.Errorf("push of a malformed header: %d %+v, want 422 bad_entry matching ErrRedactedFrom", resp.StatusCode, p)
	}
	if _, err := backing.Read(context.Background(), "x"); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("the refused push left the session: %v", err)
	}
}
