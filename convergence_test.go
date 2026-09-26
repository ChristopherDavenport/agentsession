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
// would part company here. The fork fixture carries the member on its
// root, where it records where the session came from rather than what
// it merged, and the same must hold there.
func TestConvergenceIsNotContext(t *testing.T) {
	for _, name := range []string{"converge", "fork"} {
		t.Run(name, func(t *testing.T) { convergenceIsNotContext(t, name) })
	}
}

func convergenceIsNotContext(t *testing.T, name string) {
	path := filepath.Join("testdata", "sessions", name+".jsonl")
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

// requestHashAt hashes the request the context algorithm builds at id.
func requestHashAt(t *testing.T, s *Session, id string) string {
	t.Helper()
	ctx, err := s.ContextAt(id)
	if err != nil {
		t.Fatal(err)
	}
	req, err := ctx.Request()
	if err != nil {
		t.Fatal(err)
	}
	h, err := RequestHash(req)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestForkIsMaterialised is the claim the format makes of a root that
// carries parents: the session continues the context the root names,
// and that context is in this file. The fork fixture is basic forked at
// its tool output. Its opening entries are the origin's path to that
// entry under the same IDs, so the reference can be located; every
// response the fork copied verifies in the fork as it did in the origin;
// and the fork's first model call was sent the request the origin's was
// at that point, which is the prefix a provider caches and the reason to
// fork rather than restate. The two sessions part company only in what
// the model answered.
func TestForkIsMaterialised(t *testing.T) {
	origin := loadFixture(t, "basic")
	fork := loadFixture(t, "fork")

	if got := fork.Header().ParentSession; got != origin.ID() {
		t.Errorf("parent_session = %s, want the origin %s", got, origin.ID())
	}
	roots := fork.Roots()
	if len(roots) != 1 {
		t.Fatalf("fork has %d roots, want 1", len(roots))
	}
	root, _ := fork.Entry(roots[0])
	want := []EntryRef{{Session: origin.ID(), Entry: "i0000004"}}
	if got := root.Base().Parents; !reflect.DeepEqual(got, want) {
		t.Fatalf("root parents = %+v, want %+v", got, want)
	}
	at := want[0].Entry

	// The copied path is the origin's path to the named entry, under the
	// same IDs, and rebuilds the same request there.
	op, fp := origin.Path(at), fork.Path(at)
	if len(fp) == 0 {
		t.Fatalf("the fork has no entry %s, so the reference cannot be located", at)
	}
	if len(op) != len(fp) {
		t.Fatalf("origin path has %d entries, fork path %d", len(op), len(fp))
	}
	for i := range op {
		if op[i].Base().ID != fp[i].Base().ID || op[i].EntryType() != fp[i].EntryType() {
			t.Errorf("path[%d]: origin %s %s, fork %s %s", i, op[i].EntryType(), op[i].Base().ID, fp[i].EntryType(), fp[i].Base().ID)
		}
	}
	if ho, hf := requestHashAt(t, origin, at), requestHashAt(t, fork, at); ho != hf {
		t.Errorf("request at %s: origin %s, fork %s", at, ho, hf)
	}

	// What the fork copied still verifies where it was copied to.
	for _, e := range fp {
		if r, ok := e.(*ResponseEntry); ok {
			if err := fork.Verify(r.ID); err != nil {
				t.Errorf("copied response %s: %v", r.ID, err)
			}
		}
	}

	// The fork's own first model call was sent the same request the
	// origin's was, and answered differently: same prefix, intentional
	// divergence.
	const first = "r0000002"
	if err := fork.Verify(first); err != nil {
		t.Fatalf("the fork's first response: %v", err)
	}
	oe, _ := origin.Entry(first)
	fe, _ := fork.Entry(first)
	if o, f := oe.(*ResponseEntry).RequestHash, fe.(*ResponseEntry).RequestHash; o != f {
		t.Errorf("first request after the fork: origin %s, fork %s", o, f)
	}
	oc, err := origin.RequestContext(first)
	if err != nil {
		t.Fatal(err)
	}
	fc, err := fork.RequestContext(first)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(oc.Items, fc.Items) {
		t.Error("the request context of the first response differs between origin and fork")
	}
	oi, _ := origin.Entry("i0000005")
	fi, _ := fork.Entry("i0000005")
	ob, _ := MarshalEntry(oi)
	fb, _ := MarshalEntry(fi)
	if bytes.Equal(ob, fb) {
		t.Error("the fork's answer is the origin's, so the fixture shows no divergence")
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

// TestItemReferenceIsNotWritten covers the ingress rule's one tightened
// case. A writer must not name an item in the provider's store instead
// of carrying what the model saw — the same dependency the canonical
// request already refuses with store: false and no
// previous_response_id. A reader still accepts one, because such a file
// is incomplete rather than malformed, and refusing it would lose the
// parts that are intact.
func TestItemReferenceIsNotWritten(t *testing.T) {
	s := New(Header{ID: "01995b2a-0000-7000-8000-000000000010"})
	if _, err := s.Append(NewItemEntry(&openresponses.ItemReference{ID: "msg_9"})); err == nil {
		t.Fatal("Append accepted an item_reference")
	} else if !strings.Contains(err.Error(), "item_reference") {
		t.Errorf("error %q does not name the member", err)
	}
	if s.Len() != 0 {
		t.Errorf("the refused entry reached the tree: %d entries", s.Len())
	}

	// The same item inside a file reads, and round-trips byte for byte.
	const line = `{"type":"item","id":"i1","parent":null,"ts":"2026-09-25T09:00:00Z","item":{"type":"item_reference","id":"msg_9"}}`
	file := `{"type":"session","format":"agentsession/0.4","id":"01995b2a-0000-7000-8000-000000000011","created_at":"2026-09-25T09:00:00Z","payload":"openresponses/2026-04-24"}` + "\n" + line + "\n"
	read, err := Read(strings.NewReader(file))
	if err != nil {
		t.Fatalf("Read refused a file carrying an item_reference: %v", err)
	}
	e, ok := read.Entry("i1")
	if !ok {
		t.Fatal("the item_reference entry is missing")
	}
	if _, ok := e.(*ItemEntry).Item.(*openresponses.ItemReference); !ok {
		t.Errorf("item = %T, want *openresponses.ItemReference", e.(*ItemEntry).Item)
	}
	var buf bytes.Buffer
	if err := Write(&buf, read); err != nil {
		t.Fatal(err)
	}
	if buf.String() != file {
		t.Errorf("round trip changed the file\nwant: %s\ngot:  %s", file, buf.String())
	}
}
