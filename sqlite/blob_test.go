package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentsession/sqlite"
	"github.com/ChristopherDavenport/openresponses"
)

// bearer adds a token to each request, as a program's transport would.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// RFC 0003's rule that naming a blob grants nothing, over a server on
// this store: mallory's append naming alice's blob is missing_object,
// as one naming a blob nobody holds is, and does not let him read it.
func TestNamingABlobGrantsNothingOverSQLite(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h, err := st.PutBlob(ctx, []byte("alice's picture"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "alices", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+h))); err != nil {
		t.Fatal(err)
	}
	srv, m := mallorysServer(t, st, map[string]string{"alices": "alice", "mallorys": "mallory"})
	if _, err := m.Create(ctx, agentsession.Header{ID: "mallorys", Media: agentsession.MediaSidecar}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{h, agentsession.HashBytes([]byte("a blob nobody holds"))} {
		_, err := m.Append(ctx, "mallorys", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+hash)))
		if !errors.Is(err, remote.ErrMissingObject) {
			t.Errorf("an append naming %s: %v, want ErrMissingObject", hash, err)
		}
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/objects/media/"+h, nil)
	req.Header.Set("Authorization", "Bearer mallory")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("mallory reads alice's blob: %d, want 404", resp.StatusCode)
	}
}

// mallorysServer serves st to principals named by their bearer token,
// with read, write and exchange scope, each session in owner its
// owner's alone, and dials it as mallory.
func mallorysServer(t *testing.T, st *sqlite.Store, owner map[string]string) (*httptest.Server, *remote.Client) {
	t.Helper()
	return mallorysServerAuthorized(t, st, func(_ context.Context, p remote.Principal, _ remote.Scope, session string) error {
		if o, ok := owner[session]; ok && o != p.Name {
			return errors.New("not the owner")
		}
		return nil
	})
}

// mallorysServerAuthorized is mallorysServer with the authorizer given.
func mallorysServerAuthorized(t *testing.T, st *sqlite.Store, authorize remote.Authorizer) (*httptest.Server, *remote.Client) {
	t.Helper()
	handler := remote.NewHandler(st,
		remote.WithAuthenticator(func(r *http.Request) (remote.Principal, error) {
			name, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || name == "" {
				return remote.Principal{}, remote.ErrUnauthenticated
			}
			return remote.Principal{Name: name, Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite, remote.ScopeExchange}}, nil
		}),
		remote.WithAuthorize(authorize),
	)
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		handler.Close()
	})
	m, err := remote.Dial(srv.URL, remote.WithHTTPClient(&http.Client{Transport: bearer("mallory")}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return srv, m
}

// An import and a create over a server on this store find a fork's
// origin only among the sessions the caller may read: mallory's import
// of a fork of alice's entry in another media is taken, as for an entry
// nobody holds, and a fork he creates from it names none of hers.
func TestForkOriginUnreadableOverSQLite(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices"}); err != nil {
		t.Fatal(err)
	}
	x, err := st.Append(ctx, "alices", agentsession.NewItemEntry(openresponses.UserText("alice's base")))
	if err != nil {
		t.Fatal(err)
	}
	alices, err := st.Read(ctx, "alices")
	if err != nil {
		t.Fatal(err)
	}
	_, m := mallorysServer(t, st, map[string]string{"alices": "alice"})
	f, err := agentsession.Fork(alices, x, agentsession.Header{ID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(buf.String(), `"parent_session":"alices",`, `"media":"sidecar",`, 1)
	if !strings.Contains(file, `"media":"sidecar"`) {
		t.Fatalf("setup: the header did not change: %s", file)
	}
	if _, err := m.Import(ctx, strings.NewReader(file), true); err != nil {
		t.Errorf("an import of a fork of alice's base in another media: %v, want it taken", err)
	}
	g, err := m.Create(ctx, agentsession.Header{ID: "m2", Base: x})
	if err != nil {
		t.Fatalf("a fork of the base mallory holds on his prefix: %v", err)
	}
	if g.Header().ParentSession == "alices" {
		t.Error("mallory's fork names alice's session as its parent")
	}
}

// An authorizer that reads this store, a session's owner from its
// header, is asked while a create or an import looks for a fork's
// origin, and the store holds none of its locks then: both complete
// rather than deadlock.
func TestAuthorizerReadsTheStoreOverSQLite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.Create(ctx, agentsession.Header{ID: "alices", CWD: "/home/alice"}); err != nil {
		t.Fatal(err)
	}
	x, err := st.Append(ctx, "alices", agentsession.NewItemEntry(openresponses.UserText("alice's base")))
	if err != nil {
		t.Fatal(err)
	}
	alices, err := st.Read(ctx, "alices")
	if err != nil {
		t.Fatal(err)
	}
	_, m := mallorysServerAuthorized(t, st, func(ctx context.Context, p remote.Principal, _ remote.Scope, session string) error {
		s, err := st.Open(ctx, session)
		if errors.Is(err, agentsession.ErrNoSession) {
			return nil
		}
		if err != nil {
			return err
		}
		if o, ok := strings.CutPrefix(s.Header().CWD, "/home/"); ok && o != p.Name {
			return errors.New("not the owner")
		}
		return nil
	})
	f, err := agentsession.Fork(alices, x, agentsession.Header{ID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, f); err != nil {
		t.Fatal(err)
	}
	file := strings.Replace(buf.String(), `"parent_session":"alices",`, ``, 1)
	if _, err := m.Import(ctx, strings.NewReader(file), true); err != nil {
		t.Fatalf("an import of a fork: %v", err)
	}
	if _, err := m.Create(ctx, agentsession.Header{ID: "m2", Base: x}); err != nil {
		t.Fatalf("a create of a fork: %v", err)
	}
}
