package agentsession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// parentsMember matches a whole parents member on one line. The
// references it holds are objects of strings, so nothing inside the
// array carries a closing bracket.
var parentsMember = regexp.MustCompile(`,"parents":\[[^\]]*\]`)

// TestConvergenceIsNotContext is the claim that makes parents a minor
// version rather than a major one: a reader that does not understand
// the member rebuilds the same request, byte for byte.
//
// It is measured by stripping every parents member from the fixture and
// rebuilding both sessions at every leaf. A 0.3 reader is exactly a 0.4
// reader given the stripped file, since the member is the only thing
// 0.4 added, so if the walk ever followed a convergence edge the two
// would part company here.
func TestConvergenceIsNotContext(t *testing.T) {
	path := filepath.Join("testdata", "sessions", "converge.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	stripped := parentsMember.ReplaceAll(raw, nil)
	if bytes.Equal(raw, stripped) {
		t.Fatal("the fixture carries no parents member, so this proves nothing")
	}
	if bytes.Contains(stripped, []byte(`"parents"`)) {
		t.Fatal("stripping left a parents member behind")
	}

	with, err := Read(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	without, err := Read(bytes.NewReader(stripped))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(with.Leaves(), without.Leaves()) {
		t.Fatalf("leaves differ: %v and %v", with.Leaves(), without.Leaves())
	}
	for _, leaf := range with.Leaves() {
		a, err := with.ContextAt(leaf)
		if err != nil {
			t.Fatal(err)
		}
		b, err := without.ContextAt(leaf)
		if err != nil {
			t.Fatal(err)
		}
		ra, err := a.Request()
		if err != nil {
			t.Fatal(err)
		}
		rb, err := b.Request()
		if err != nil {
			t.Fatal(err)
		}
		ha, err := RequestHash(ra)
		if err != nil {
			t.Fatal(err)
		}
		hb, err := RequestHash(rb)
		if err != nil {
			t.Fatal(err)
		}
		if ha != hb {
			t.Errorf("leaf %s: request %s with parents, %s without", leaf, ha, hb)
		}
	}
}

// TestConvergenceIsRecorded checks the other half: the member survives
// the read that must not act on it, in the order a writer sorted it
// into.
func TestConvergenceIsRecorded(t *testing.T) {
	s := loadFixture(t, "converge")

	// The output entry names the leaf of the child that answered, which
	// the link entry beside it cannot: a link names a session, and when
	// it is written the child has no point to name yet.
	e, ok := s.Entry("i0000004")
	if !ok {
		t.Fatal("no i0000004")
	}
	want := []EntryRef{{Session: "01995b2a-0000-7000-8000-0000000000c1", Entry: "a0000009"}}
	if got := e.Base().Parents; !reflect.DeepEqual(got, want) {
		t.Errorf("output parents = %+v, want %+v", got, want)
	}

	// The join names a branch in this file and a leaf in another. The
	// one in this file omits the session, and sorts first.
	e, _ = s.Entry("i0000009")
	want = []EntryRef{{Entry: "i0000008"}, {Session: "01995b2a-0000-7000-8000-0000000000c2", Entry: "b0000009"}}
	if got := e.Base().Parents; !reflect.DeepEqual(got, want) {
		t.Errorf("join parents = %+v, want %+v", got, want)
	}

	// The converged branch is not the parent, and is not on the path.
	if p := e.Base().Parent; p != "i0000007" {
		t.Errorf("join parent = %s", p)
	}
	for _, pe := range s.Path("i0000009") {
		if pe.Base().ID == "i0000008" {
			t.Error("the converged branch is on the path to the join")
		}
	}
}

// TestAppendSortsParents checks that a writer's output does not depend
// on the order the workers happened to finish in.
func TestAppendSortsParents(t *testing.T) {
	s := New(Header{ID: "01995b2a-0000-7000-8000-00000000000d"})
	a, err := s.Append(NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Append(NewItemEntry(openresponses.UserText("b")))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(a); err != nil {
		t.Fatal(err)
	}
	join := NewItemEntry(openresponses.UserText("join"))
	join.Parents = []EntryRef{
		{Session: "zzz", Entry: "z1"},
		{Session: "aaa", Entry: "a2"},
		{Session: "aaa", Entry: "a1"},
		{Entry: b},
	}
	if _, err := s.Append(join); err != nil {
		t.Fatal(err)
	}
	want := []EntryRef{
		{Entry: b},
		{Session: "aaa", Entry: "a1"},
		{Session: "aaa", Entry: "a2"},
		{Session: "zzz", Entry: "z1"},
	}
	if got := join.Parents; !reflect.DeepEqual(got, want) {
		t.Errorf("parents = %+v, want %+v", got, want)
	}
	// The join is the leaf; converging b did not move it there.
	if s.Leaf() != join.ID {
		t.Errorf("leaf = %s, want the join %s", s.Leaf(), join.ID)
	}
}

// TestConvergenceRules checks what Append refuses. Every case is a MUST
// in the format's convergence section.
func TestConvergenceRules(t *testing.T) {
	const self = "01995b2a-0000-7000-8000-00000000000e"
	build := func(t *testing.T) (*Session, string, string) {
		t.Helper()
		s := New(Header{ID: self})
		a, err := s.Append(NewItemEntry(openresponses.UserText("a")))
		if err != nil {
			t.Fatal(err)
		}
		b, err := s.Append(NewItemEntry(openresponses.UserText("b")))
		if err != nil {
			t.Fatal(err)
		}
		return s, a, b
	}
	tests := []struct {
		name string
		// refs is built from the two entry IDs a and b; b is the leaf,
		// so b is the parent of whatever the test appends.
		refs func(a, b string) []EntryRef
		want string
	}{
		{"no entry id", func(a, b string) []EntryRef { return []EntryRef{{Session: "other"}} }, "no entry id"},
		{"names the parent", func(a, b string) []EntryRef { return []EntryRef{{Entry: b}} }, "its own parent"},
		{
			"names the parent through this session's own id",
			func(a, b string) []EntryRef { return []EntryRef{{Session: self, Entry: b}} },
			"its own parent",
		},
		{"names one entry twice", func(a, b string) []EntryRef { return []EntryRef{{Entry: a}, {Entry: a}} }, "twice"},
		{
			"names one entry twice, once through this session's own id",
			func(a, b string) []EntryRef { return []EntryRef{{Entry: a}, {Session: self, Entry: a}} },
			"twice",
		},
		{"names an entry that does not exist", func(a, b string) []EntryRef { return []EntryRef{{Entry: "ghost"}} }, "not in this session yet"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, a, b := build(t)
			e := NewItemEntry(openresponses.UserText("join"))
			e.Parents = tt.refs(a, b)
			_, err := s.Append(e)
			if !errors.Is(err, ErrBadConvergence) {
				t.Fatalf("Append = %v, want ErrBadConvergence", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q does not say %q", err, tt.want)
			}
			// A refused append leaves the tree alone.
			if s.Len() != 2 || s.Leaf() != b {
				t.Errorf("after the refusal: %d entries, leaf %s", s.Len(), s.Leaf())
			}
		})
	}

	// A reference into another session is not resolvable here, so only
	// its shape is checked and an unknown session is accepted.
	s, a, _ := build(t)
	e := NewItemEntry(openresponses.UserText("join"))
	e.Parents = []EntryRef{{Entry: a}, {Session: "a-session-this-file-cannot-see", Entry: "x1"}}
	if _, err := s.Append(e); err != nil {
		t.Errorf("Append with a foreign reference: %v", err)
	}
}

// TestReadRejectsForwardConvergence checks the acyclicity rule on read:
// a reference into this file must name an entry that already exists, so
// every edge points at something older. The fixture's join converges an
// entry written after it.
func TestReadRejectsForwardConvergence(t *testing.T) {
	path := filepath.Join("testdata", "sessions", "bad-parents.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Read(bytes.NewReader(raw))
	if !errors.Is(err, ErrBadConvergence) {
		t.Fatalf("Read = %v, want ErrBadConvergence", err)
	}
}

// TestConvergenceOnAnUnknownEntry covers the one path that rebuilds an
// envelope rather than preserving it: an entry of a type this package
// does not define is written back from its raw line, and Append rewrites
// that line to pick up the ID, parent and timestamp it just assigned.
// A rewrite that forgot parents would drop the provenance silently, and
// only for extension types.
func TestConvergenceOnAnUnknownEntry(t *testing.T) {
	s := New(Header{ID: "01995b2a-0000-7000-8000-00000000000f"})
	a, err := s.Append(NewItemEntry(openresponses.UserText("a")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(NewItemEntry(openresponses.UserText("b"))); err != nil {
		t.Fatal(err)
	}
	u := &UnknownEntry{Raw: []byte(`{"type":"acme:join","note":"merged"}`)}
	u.Parents = []EntryRef{{Entry: a}}
	if _, err := s.Append(u); err != nil {
		t.Fatal(err)
	}
	got, err := MarshalEntry(u)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `"parents":[{"entry":"`+a+`"}]`) {
		t.Errorf("the rewritten line lost its parents: %s", got)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	again, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	e, _ := again.Entry(u.ID)
	if got := e.Base().Parents; !reflect.DeepEqual(got, []EntryRef{{Entry: a}}) {
		t.Errorf("parents after a round trip = %+v", got)
	}
}

// TestUnsortedParentsRoundTrip checks that a reader does not improve a
// file it was given. Sorting is a rule a writer is held to; a file that
// arrives out of order is written back as it came, like every other
// thing a reader preserves without understanding why it is so.
func TestUnsortedParentsRoundTrip(t *testing.T) {
	const line = `{"type":"item","id":"i2","parent":"i1","parents":[{"session":"zz","entry":"z"},{"entry":"i1x"}],"ts":"2026-09-25T09:00:00Z","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"j"}]}}`
	e, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	got, err := MarshalEntry(e)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != line {
		t.Errorf("round trip reordered the file\nwant: %s\ngot:  %s", line, got)
	}
}
