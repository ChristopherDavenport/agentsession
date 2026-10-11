package remote_test

import (
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
	w := &wire{Client: c, backing: st}
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
		New: func(t *testing.T) agentsession.Store { return serve(t, agentsession.NewMemoryStore(), nil) },
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
	})
}
