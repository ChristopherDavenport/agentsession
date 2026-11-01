package sqlite_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
	owner := map[string]string{"alices": "alice", "mallorys": "mallory"}
	handler := remote.NewHandler(st,
		remote.WithAuthenticator(func(r *http.Request) (remote.Principal, error) {
			name, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || name == "" {
				return remote.Principal{}, remote.ErrUnauthenticated
			}
			return remote.Principal{Name: name, Scopes: []remote.Scope{remote.ScopeRead, remote.ScopeWrite}}, nil
		}),
		remote.WithAuthorize(func(_ context.Context, p remote.Principal, _ remote.Scope, session string) error {
			if o, ok := owner[session]; ok && o != p.Name {
				return errors.New("not the owner")
			}
			return nil
		}),
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
