package remote_test

import (
	"context"
	"iter"
	"net/http/httptest"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/agentsession/storetest"
)

// overWire is a store whose writes go to the store itself and whose
// reads go through a remote client of a server over it: the suite's
// read, follow and ref cases run over the wire, and its write cases
// run on the store as before.
type overWire struct {
	agentsession.Store
	c *remote.Client
}

func (w overWire) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	return w.c.Read(ctx, id)
}

func (w overWire) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return w.c.Follow(ctx, id, from)
}

func (w overWire) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return w.c.List(ctx, f)
}

func (w overWire) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	return w.c.ResolveRef(ctx, name)
}

func (w overWire) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	return w.c.ListRefs(ctx, prefix)
}

func (w overWire) RefLog(ctx context.Context, name string) iter.Seq2[agentsession.RefUpdate, error] {
	return w.c.RefLog(ctx, name)
}

// UpdateRef is the store's: the client reads only.
func (w overWire) UpdateRef(ctx context.Context, name string, expected, next agentsession.RefTarget, reason string) error {
	return w.Store.(agentsession.RefStore).UpdateRef(ctx, name, expected, next, reason)
}

// serve wraps st in a server and returns the store the suite sees.
func serve(t *testing.T, st agentsession.Store, opts ...remote.Option) overWire {
	t.Helper()
	h := remote.NewHandler(st, append([]remote.Option{remote.WithInsecureNoAuth(), remote.WithLinger(0)}, opts...)...)
	srv := httptest.NewServer(h)
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		h.Close()
	})
	c, err := remote.Dial(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return overWire{Store: st, c: c}
}

func TestConformanceOverMemory(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return serve(t, agentsession.NewMemoryStore()) },
	})
}

func TestConformanceOverJSONL(t *testing.T) {
	open := func(t *testing.T, root string, opts ...jsonl.Option) *jsonl.Store {
		st, err := jsonl.Open(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return serve(t, open(t, t.TempDir())) },
		Second: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return serve(t, open(t, s.(overWire).Store.(*jsonl.Store).Root()))
		},
		ReadOnly: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return serve(t, open(t, s.(overWire).Store.(*jsonl.Store).Root(), jsonl.WithReadOnly()))
		},
	})
}

func TestConformanceOverCAS(t *testing.T) {
	open := func(t *testing.T, root string, opts ...cas.Option) *cas.Store {
		st, err := cas.Open(root, opts...)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return st
	}
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store { return serve(t, open(t, t.TempDir())) },
		Second: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return serve(t, open(t, s.(overWire).Store.(*cas.Store).Root()))
		},
		ReadOnly: func(t *testing.T, s agentsession.Store) agentsession.Store {
			return serve(t, open(t, s.(overWire).Store.(*cas.Store).Root(), cas.WithReadOnly()))
		},
	})
}
