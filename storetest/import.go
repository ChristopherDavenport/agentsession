package storetest

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// testImport takes session files in whole, for a store with an import,
// as RFC 0002's import paragraph has it: a session and a fork are held
// as their files have them, in file order, and take appends; and a
// session ID the store holds, a redacted_from without redacted or naming
// the session itself, a line that does not verify, a last line cut
// short and a fork whose media differs from its origin's are refused,
// with nothing left of the attempt.
func testImport(t *testing.T, opts Options) {
	ctx := context.Background()
	st := opts.New(t)
	take := importer(opts, st)
	hooks(t, take != nil)

	orig := agentsession.New(agentsession.Header{ID: "imp", CWD: "/w"})
	for _, text := range []string{"hello", "world", "again"} {
		if _, err := orig.Append(agentsession.NewItemEntry(openresponses.UserText(text))); err != nil {
			t.Fatal(err)
		}
	}
	file := fileOf(t, orig)
	if err := take(t, st, file); err != nil {
		t.Fatalf("import: %v", err)
	}
	got := readBack(t, st, "imp")
	if !reflect.DeepEqual(entryIDs(got), entryIDs(orig)) || got.Leaf() != orig.Leaf() {
		t.Errorf("imported %v at %s, want %v at %s", entryIDs(got), got.Leaf(), entryIDs(orig), orig.Leaf())
	}
	if _, err := st.Append(ctx, "imp", user("after the import")); err != nil {
		t.Errorf("append to an imported session: %v", err)
	}
	wantErr(t, "import of a session the store holds", take(t, st, file), agentsession.ErrSessionExists)

	fork, err := agentsession.Fork(orig, entryIDs(orig)[1], agentsession.Header{ID: "imp-fork"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fork.Append(user("forked")); err != nil {
		t.Fatal(err)
	}
	if err := take(t, st, fileOf(t, fork)); err != nil {
		t.Fatalf("import of a fork: %v", err)
	}
	gf := readBack(t, st, "imp-fork")
	if g, w := entryIDs(gf), entryIDs(fork); !reflect.DeepEqual(g, w) || gf.Header().Base != fork.Header().Base || !gf.Prefix(w[0]) || gf.Prefix(w[len(w)-1]) {
		t.Errorf("imported fork %v base %s, want %v base %s with its prefix", g, gf.Header().Base, w, fork.Header().Base)
	}

	header, rest, _ := strings.Cut(string(fileOf(t, renamed(t, orig, "imp-refused"))), "\n")
	refused := map[string]struct {
		file string
		want error
	}{
		"redacted_from without redacted": {strings.Replace(header, `{`, `{"redacted_from":{"entries":3,"session":"o"},`, 1) + "\n" + rest, agentsession.ErrRedactedFrom},
		"redacted_from naming itself":    {strings.Replace(header, `{`, `{"redacted":true,"redacted_from":{"entries":3,"session":"imp-refused"},`, 1) + "\n" + rest, agentsession.ErrRedactedFrom},
		"a line that does not verify":    {header + "\n" + strings.Replace(rest, "world", "w0rld", 1), nil},
		"a last line cut short":          {header + "\n" + rest[:len(rest)-10], nil},
		"media other than the origin's":  {strings.Replace(string(fileOf(t, renamedFork(t, fork, "imp-media"))), `{`, `{"media":"sidecar",`, 1), nil},
	}
	for name, tt := range refused {
		err := take(t, st, []byte(tt.file))
		if err == nil || tt.want != nil && !errors.Is(err, tt.want) {
			t.Errorf("import of %s: %v, want a refusal (%v)", name, err, tt.want)
		}
	}
	for _, id := range []string{"imp-refused", "imp-media"} {
		if _, err := st.Open(ctx, id); !errors.Is(err, agentsession.ErrNoSession) {
			t.Errorf("a refused import left %s: %v", id, err)
		}
	}

	if opts.Reopen != nil {
		again := opts.Reopen(t, st)
		if got := readBack(t, again, "imp-fork"); !reflect.DeepEqual(entryIDs(got), entryIDs(fork)) {
			t.Errorf("after a reopen the fork holds %v", entryIDs(got))
		}
	}
}

// renamed is s under another ID, its entries as they are: the header is
// not hashed.
func renamed(t *testing.T, s *agentsession.Session, id string) *agentsession.Session {
	t.Helper()
	h := s.Header()
	h.ID = id
	out := agentsession.New(h)
	for _, e := range s.Entries() {
		if _, err := out.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// renamedFork is a fork's file under another ID, read back so its prefix
// stays a prefix.
func renamedFork(t *testing.T, fork *agentsession.Session, id string) *agentsession.Session {
	t.Helper()
	file := strings.Replace(string(fileOf(t, fork)), `"id":"`+fork.ID()+`"`, `"id":"`+id+`"`, 1)
	s, err := agentsession.Read(bytes.NewReader([]byte(file)))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
