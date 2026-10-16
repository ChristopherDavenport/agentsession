package remote_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentsession/storetest"
)

// wire is the store the suite sees: a client of a server over a store,
// as another process would reach it. Every call of the suite, its writes
// included, goes over the wire. Close takes the whole of it down, the
// client's leases, the server and the store, as a process exiting would.
type wire struct {
	*remote.Client
	backing agentsession.Store
	url     string
	once    sync.Once
	down    func()
}

func (w *wire) Close() error {
	w.once.Do(w.down)
	return nil
}

// serve wraps st, which closer closes, in a server and returns a client
// of it.
func serve(t *testing.T, st agentsession.Store, closer func() error, opts ...remote.Option) *wire {
	t.Helper()
	h := remote.NewHandler(st, append([]remote.Option{remote.WithInsecureNoAuth(), remote.WithLinger(0)}, opts...)...)
	srv := httptest.NewServer(h)
	c, err := remote.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{Client: c, backing: st, url: srv.URL}
	w.down = func() {
		c.Close()
		srv.CloseClientConnections()
		srv.Close()
		h.Close()
		if closer != nil {
			closer()
		}
	}
	t.Cleanup(func() { w.Close() })
	return w
}

func TestConformanceOverMemory(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New:    func(t *testing.T) agentsession.Store { return serve(t, agentsession.NewMemoryStore(), nil) },
		Import: importBehind,
		RedactedOnly: func(t *testing.T) agentsession.Store {
			return serve(t, agentsession.NewMemoryStore(agentsession.MemoryRedactedOnly()), nil)
		},
	})
}

func TestConformanceOverJSONL(t *testing.T) {
	open := func(t *testing.T, root string, opts ...jsonl.Option) *wire {
		st, err := jsonl.Open(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return serve(t, st, st.Close)
	}
	root := func(s agentsession.Store) string { return s.(*wire).backing.(*jsonl.Store).Root() }
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return open(t, t.TempDir()) },
		Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
			s.(*wire).Close()
			return open(t, root(s))
		},
		Second: func(t *testing.T, s agentsession.Store) agentsession.Store { return open(t, root(s)) },
		ReadOnly: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return open(t, root(s), jsonl.WithReadOnly())
		},
		Import: importBehind,
		RedactedOnly: func(t *testing.T) agentsession.Store {
			return open(t, t.TempDir(), jsonl.WithRedactedOnly())
		},
	})
}

func TestConformanceOverCAS(t *testing.T) {
	open := func(t *testing.T, root string, opts ...cas.Option) *wire {
		st, err := cas.Open(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		return serve(t, st, st.Close)
	}
	root := func(s agentsession.Store) string { return s.(*wire).backing.(*cas.Store).Root() }
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return open(t, t.TempDir()) },
		Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
			s.(*wire).Close()
			return open(t, root(s))
		},
		Second: func(t *testing.T, s agentsession.Store) agentsession.Store { return open(t, root(s)) },
		ReadOnly: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return open(t, root(s), cas.WithReadOnly())
		},
		Import: importBehind,
		Push: func(t *testing.T, s agentsession.Store, file []byte) error {
			return pushFile(t, s.(*wire).Client, file)
		},
		RedactedOnly: func(t *testing.T) agentsession.Store {
			return open(t, t.TempDir(), cas.WithRedactedOnly())
		},
	})
}

// importBehind imports a session file into the store behind the
// server, as the record: the server has no import, so a session file
// reaches it over the wire only by push.
func importBehind(t *testing.T, s agentsession.Store, file []byte) error {
	_, err := s.(*wire).backing.(agentsession.Importer).Import(context.Background(), bytes.NewReader(file), true)
	return err
}

// pushFile pushes the session file holds to the server c reaches, from
// a local store that imported the file as the record.
func pushFile(t *testing.T, c *remote.Client, file []byte) error {
	t.Helper()
	ctx := context.Background()
	local, err := cas.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	s, err := local.Import(ctx, bytes.NewReader(file), true)
	if err != nil {
		t.Fatal(err)
	}
	_, err = remote.Push(ctx, local, c, s.ID(), cas.PushOptions{})
	return err
}
