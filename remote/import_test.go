package remote_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
)

// fileOfSession is a session's RFC 0001 file.
func fileOfSession(t *testing.T, s *agentsession.Session) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestImportEndpoint is #241: a server over a store with no exchange
// takes a session file whole, with exchange scope, authorized for the
// session it creates; it refuses what a local import refuses, and a
// sidecar session whose blob it cannot find for the caller with
// missing_object, admitting nothing, until the blob is uploaded.
func TestImportEndpoint(t *testing.T) {
	ctx := context.Background()
	st := agentsession.NewMemoryStore()
	url := server(t, st,
		remote.WithAuthenticator(tokenAuth(map[string]remote.Principal{
			"x": {Name: "x", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeExchange}},
			"w": {Name: "w", Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}},
		})),
		remote.WithAuthorize(owners(map[string]string{"theirs": "someone else"})),
	)
	x := dial(t, url, withToken("x", nil))
	w := dial(t, url, withToken("w", nil))

	sess := agentsession.New(agentsession.Header{ID: "imp"})
	for _, text := range []string{"one", "two"} {
		if _, err := sess.Append(user(text)); err != nil {
			t.Fatal(err)
		}
	}
	file := fileOfSession(t, sess)
	if _, err := w.Import(ctx, bytes.NewReader(file), true); !errors.Is(err, remote.ErrForbidden) {
		t.Errorf("an import with write scope alone: %v, want ErrForbidden", err)
	}
	got, err := x.Import(ctx, bytes.NewReader(file), true)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if got.Len() != 2 || got.Leaf() != sess.Leaf() {
		t.Errorf("imported %d entries at %s, want 2 at %s", got.Len(), got.Leaf(), sess.Leaf())
	}
	if _, err := st.Open(ctx, "imp"); err != nil {
		t.Errorf("the store behind the server: %v", err)
	}
	if _, err := x.Import(ctx, bytes.NewReader(file), true); !errors.Is(err, agentsession.ErrSessionExists) {
		t.Errorf("a second import: %v, want ErrSessionExists", err)
	}
	theirs := agentsession.New(agentsession.Header{ID: "theirs"})
	if _, err := x.Import(ctx, bytes.NewReader(fileOfSession(t, theirs)), true); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("an import of a session the caller may not write: %v, want ErrNoSession", err)
	}
	bad := bytes.Replace(file, []byte(`"one"`), []byte(`"0ne"`), 1)
	bad = bytes.Replace(bad, []byte(`"imp"`), []byte(`"imp-bad"`), 1)
	if _, err := x.Import(ctx, bytes.NewReader(bad), true); !errors.Is(err, agentsession.ErrBadID) {
		t.Errorf("an import of a line that does not verify: %v, want ErrBadID", err)
	}

	blob := []byte("image bytes")
	side := agentsession.New(agentsession.Header{ID: "side", Media: agentsession.MediaSidecar})
	if _, err := side.Append(user("see sidecar:" + agentsession.HashBytes(blob))); err != nil {
		t.Fatal(err)
	}
	sf := fileOfSession(t, side)
	_, err = x.Import(ctx, bytes.NewReader(sf), true)
	var p *remote.Problem
	if !errors.As(err, &p) || p.Code != remote.CodeMissingObject || p.Missing == nil || len(p.Missing.Media) != 1 {
		t.Fatalf("a sidecar import whose blob the server lacks: %v, want missing_object naming it", err)
	}
	if _, err := st.Open(ctx, "side"); err == nil {
		t.Error("a refused import left its session")
	}
	dir := fstest.MapFS{strings.TrimPrefix(agentsession.HashBytes(blob), agentsession.HashPrefix): {Data: blob}}
	if _, _, err := x.ImportMedia(ctx, bytes.NewReader(sf), dir, true); err != nil {
		t.Fatalf("a sidecar import with its blob: %v", err)
	}
	if data, err := st.Blob(ctx, agentsession.HashBytes(blob)); err != nil || !bytes.Equal(data, blob) {
		t.Errorf("the imported blob: %q, %v", data, err)
	}
}

// TestPushCheck is #244: a push to a store that holds only redacted
// entries is refused at its pre-flight, before any object is uploaded,
// and a push posted without one meets the same refusal before the
// server looks for an object.
func TestPushCheck(t *testing.T) {
	ctx := context.Background()
	recv, err := cas.Open(t.TempDir(), cas.WithRedactedOnly())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { recv.Close() })
	url := server(t, recv, remote.WithInsecureNoAuth())
	c := dial(t, url)

	local, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { local.Close() })
	if _, err := local.Create(ctx, agentsession.Header{ID: "orig"}); err != nil {
		t.Fatal(err)
	}
	e, err := local.Append(ctx, "orig", user("an original"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Push(ctx, local, c, "orig", cas.PushOptions{}); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Fatalf("a push of an original: %v, want ErrNotRedacted", err)
	}
	if ok, _ := recv.HasObject(ctx, cas.SpaceEntries, e); ok {
		t.Error("a push refused at its pre-flight uploaded the entry")
	}
	h, _ := local.Open(ctx, "orig")
	if err := c.CheckPush(ctx, h.Header(), []string{e}, nil); !errors.Is(err, agentsession.ErrNotRedacted) {
		t.Errorf("CheckPush: %v, want ErrNotRedacted", err)
	}
	// The push itself, posted with an entry the server lacks, refuses as
	// the pre-flight did and not with missing_object.
	hdr, err := json.Marshal(h.Header())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"header":` + string(hdr) + `,"entries":["` + e + `"],"prefix":[],"head":{"expected":null,"to":"` + e + `"},"mark":"mirror"}`
	resp, err := http.Post(url+"/v1/sessions/orig/push", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("a posted push of an original: %d, want 403 not_redacted", resp.StatusCode)
	}
	// A prefix that does not end at the base is base_rule.
	fork := h.Header()
	fork.ID, fork.Base, fork.Redacted = "f", e, true
	if err := c.CheckPush(ctx, fork, nil, nil); !errors.Is(err, agentsession.ErrBaseRule) {
		t.Errorf("CheckPush of a fork with no prefix: %v, want ErrBaseRule", err)
	}
}
