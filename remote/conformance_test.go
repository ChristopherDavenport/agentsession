package remote_test

import (
	"bytes"
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

// lowerHeaderFile rewrites the format the header line at the start of
// the file at path names, as a writer of that earlier minor would have
// left it.
func lowerHeaderFile(t *testing.T, path, format string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cur := `"format":"` + agentsession.Format + `"`
	if !bytes.Contains(data, []byte(cur)) {
		t.Fatalf("%s holds no %s", path, cur)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte(cur), []byte(`"format":"`+format+`"`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestConformanceOverMemory(t *testing.T) {
	storetest.Run(t, storetest.Options{
		New:    func(t *testing.T) agentsession.Store { return serve(t, agentsession.NewMemoryStore(), nil) },
		Import: importBehind,
		RedactedOnly: func(t *testing.T) agentsession.Store {
			return serve(t, agentsession.NewMemoryStore(agentsession.MemoryRedactedOnly()), nil)
		},
		// A memory store's storage is the store: the session is lowered
		// into a store of its own, behind a server of its own.
		Lower: func(t *testing.T, s agentsession.Store, id, format string) agentsession.Store {
			w := s.(*wire)
			lowered := lowerMemory(t, w.backing.(*agentsession.MemoryStore), id, format)
			w.Close()
			return serve(t, lowered, nil)
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
		Lower: func(t *testing.T, s agentsession.Store, id, format string) agentsession.Store {
			path, err := s.(*wire).backing.(*jsonl.Store).Path(id)
			if err != nil {
				t.Fatal(err)
			}
			s.(*wire).Close()
			lowerHeaderFile(t, path, format)
			return open(t, root(s))
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
		Lower: func(t *testing.T, s agentsession.Store, id, format string) agentsession.Store {
			s.(*wire).Close()
			lowerHeaderFile(t, filepath.Join(root(s), "sessions", id, "header"), format)
			return open(t, root(s))
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

// lowerMemory is a memory store holding the session id of old, its
// header naming format, as a writer of that earlier minor would have
// left it: the session's file, its format rewritten, imported into a
// store of its own.
func lowerMemory(t *testing.T, old *agentsession.MemoryStore, id, format string) *agentsession.MemoryStore {
	t.Helper()
	ctx := context.Background()
	s, err := old.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	cur := `"format":"` + agentsession.Format + `"`
	if !strings.Contains(buf.String(), cur) {
		t.Fatalf("the header holds no %s", cur)
	}
	st := agentsession.NewMemoryStore()
	if _, err := st.Import(ctx, strings.NewReader(strings.Replace(buf.String(), cur, `"format":"`+format+`"`, 1)), true); err != nil {
		t.Fatal(err)
	}
	return st
}
