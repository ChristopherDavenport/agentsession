package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/openresponses"
)

// fixtures022 are the 0.22 conformance fixtures, each built natively by
// its function, as for fixtures021.
var fixtures022 = map[string]func(t *testing.T) *Session{
	"checkouts": checkoutsFixture,
}

// writeFixtures022 writes the 0.22 fixtures.
func writeFixtures022(t *testing.T) {
	t.Helper()
	for name, build := range fixtures022 {
		var buf bytes.Buffer
		if err := Write(&buf, build(t)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "sessions", name+".jsonl"), buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	revMain    = "4c61d09aa1b2c3d4e5f60718293a4b5c6d7e8f90"
	revFix     = "9a1f0c3e5b7d9f1a3c5e7092b4d6f8a0c2e4f6a8"
	revLib     = "1b2c3d4e5f60718293a4b5c6d7e8f904c61d09a"
	checkoutWS = "laptop"
)

// checkoutsFixture builds the session checkouts.jsonl holds (RFC 0001,
// env, since 0.22): an env whose checkouts list "." beside the
// top-level vcs, on main tracking origin/main; after a turn that made a
// worktree under cwd and read a checkout outside it, an env listing
// the three; after a turn that committed in the worktree and pushed it,
// an env whose worktree moved to a new revision, dirty no more and with
// an upstream; and last an env whose list is empty, every checkout gone.
func checkoutsFixture(t *testing.T) *Session {
	t.Helper()
	s, must, say := fixtureSession(t, Header{ID: "01995b2a-0000-7000-8000-0000000000c5", CWD: "/home/u/work"}, "2026-11-26T10:00:00Z")
	must(&ConfigEntry{Model: "gpt-5", Instructions: ptr("Work in the repository.")})
	root := func() Checkout {
		return Checkout{Path: ".", Repo: ".git", VCS: &VCS{System: "git", Revision: revMain}, Branch: "main",
			Upstream: &Upstream{Name: "origin/main", Revision: revMain}}
	}
	env := func(list ...Checkout) *EnvEntry {
		e := NewEnvEntry("/home/u/work")
		e.VCS = &VCS{System: "git", Revision: revMain}
		e.SetWorkspace(WorkspaceLocal, "").Host = checkoutWS
		e.Checkouts = append([]Checkout{}, list...)
		return e
	}
	lib := Checkout{Path: "/srv/shared/lib", Repo: "/srv/shared/lib/.git", VCS: &VCS{System: "git", Revision: revLib}, Branch: "main"}
	must(env(root()))
	must(NewItemEntry(openresponses.UserText("Fix the parser in a worktree.")))
	say("resp_1", "Made .worktrees/fix and read the shared library.")
	must(env(root(),
		Checkout{Path: ".worktrees/fix", Repo: ".git", VCS: &VCS{System: "git", Revision: revMain, Dirty: true}, Branch: "fix"},
		lib))
	must(NewItemEntry(openresponses.UserText("Commit and push it.")))
	say("resp_2", "Committed and pushed fix.")
	must(env(root(),
		Checkout{Path: ".worktrees/fix", Repo: ".git", VCS: &VCS{System: "git", Revision: revFix}, Branch: "fix",
			Upstream: &Upstream{Name: "origin/fix", Revision: revFix}},
		lib))
	must(NewItemEntry(openresponses.UserText("Clean up.")))
	say("resp_3", "Removed the worktree.")
	must(env())
	return s
}

// TestCheckoutsFixture reads the checkouts fixture: every env keeps its
// list as written, "." beside the top-level vcs verifies, the last
// list is empty and not absent, and folding CheckoutChanges over the
// path follows the worktree from creation through its commit and push
// to its removal.
func TestCheckoutsFixture(t *testing.T) {
	checkBuilt(t, "checkouts")
	s := loadFixture(t, "checkouts")
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Fatalf("VerifyRecords: %v", err)
	}
	var envs []*EnvEntry
	for _, e := range s.Path(s.Leaf()) {
		if v, ok := e.(*EnvEntry); ok {
			envs = append(envs, v)
		}
	}
	if len(envs) != 4 {
		t.Fatalf("%d env entries", len(envs))
	}
	if last := envs[3]; last.Checkouts == nil || len(last.Checkouts) != 0 {
		t.Errorf("the last env's checkouts are %#v, want an empty list", last.Checkouts)
	}
	if c := envs[1].Checkout("/srv/shared/lib"); c == nil || c.Repo != "/srv/shared/lib/.git" {
		t.Errorf("the checkout outside cwd is %+v", c)
	}
	var got []string
	var before *EnvEntry
	for _, e := range envs {
		for _, c := range CheckoutChanges(before, e) {
			got = append(got, c.Path+" "+string(c.Kind))
		}
		before = e
	}
	want := []string{
		". added",
		".worktrees/fix added", "/srv/shared/lib added",
		".worktrees/fix revision", ".worktrees/fix upstream", ".worktrees/fix dirty",
		". removed", ".worktrees/fix removed", "/srv/shared/lib removed",
	}
	if strings.Join(got, "; ") != strings.Join(want, "; ") {
		t.Errorf("changes\n got %s\nwant %s", strings.Join(got, "; "), strings.Join(want, "; "))
	}
}

// envFile is a file of the given format holding one env entry with
// the given members beside cwd.
func envFile(t *testing.T, format, members string) string {
	t.Helper()
	header := `{"type":"session","format":"` + format + `","id":"x","created_at":"2026-11-26T10:00:00Z","payload":"openresponses/2026-04-24"}`
	line := reparent(t, `{"type":"env","id":"x","parent":null,"ts":"2026-11-26T10:00:01Z","cwd":"/home/u/work"`+members+`}`, "")
	return header + "\n" + line + "\n"
}

// readEnv reads envFile's file and returns the session and its env.
func readEnv(t *testing.T, format, members string) (*Session, *EnvEntry) {
	t.Helper()
	s, err := Read(strings.NewReader(envFile(t, format, members)))
	if err != nil {
		t.Fatalf("%s %s: %v", format, members, err)
	}
	return s, s.Entries()[0].(*EnvEntry)
}

// TestCheckoutsForms: a checkouts member is taken where 0.22 defines
// it, a list of checkouts in the format's form, in a file of any
// minor, with absent told from empty; any other form is kept as
// written, and VerifyRecords reports it in a file declaring 0.22 and
// not in one declaring 0.21, which was free to write it.
func TestCheckoutsForms(t *testing.T) {
	for _, tt := range []struct {
		name, member string
		typed        bool
	}{
		{"empty", `[]`, true},
		{"dot", `[{"path":"."}]`, true},
		{"relative", `[{"path":"a/b","repo":".git","branch":"x"}]`, true},
		{"absolute", `[{"path":"/srv/lib"}]`, true},
		{"windows", `[{"path":"C:\\src\\lib"}]`, true},
		{"undefined members", `[{"path":"sub","acme:tree":"t1","vcs":{"system":"git","acme:x":1},"upstream":{"name":"o/m","acme:y":true}}]`, true},
		{"no path", `[{"repo":".git"}]`, false},
		{"empty path", `[{"path":""}]`, false},
		{"climbs", `[{"path":"../x"}]`, false},
		{"dot segment", `[{"path":"./x"}]`, false},
		{"empty segment", `[{"path":"a//b"}]`, false},
		{"trailing slash", `[{"path":"a/"}]`, false},
		{"bad repo", `[{"path":".","repo":"../.git"}]`, false},
		{"twice", `[{"path":"a"},{"path":"a"}]`, false},
		{"path a number", `[{"path":1}]`, false},
		{"vcs null", `[{"path":".","vcs":null}]`, false},
		{"upstream a string", `[{"path":".","upstream":"origin/main"}]`, false},
		{"branch null", `[{"path":".","branch":null}]`, false},
		{"an object", `{"path":"."}`, false},
		{"null", `null`, false},
	} {
		for _, format := range []string{"agentsession/0.21", Format} {
			s, env := readEnv(t, format, `,"checkouts":`+tt.member)
			_, kept := env.Unknown["checkouts"]
			if kept == tt.typed || (env.Checkouts != nil) != tt.typed {
				t.Errorf("%s at %s: kept %v, typed %#v, want typed %v", tt.name, format, kept, env.Checkouts, tt.typed)
			}
			back, err := MarshalEntry(env)
			if err != nil {
				t.Fatalf("%s at %s: %v", tt.name, format, err)
			}
			if !sameJSON(t, members(t, back)["checkouts"], []byte(tt.member)) {
				t.Errorf("%s at %s: written back as %s", tt.name, format, back)
			}
			err = s.VerifyRecords(s.Leaf())
			if wantReport := !tt.typed && format == Format; wantReport != errors.Is(err, ErrCheckoutsForm) || !wantReport && err != nil {
				t.Errorf("%s at %s: VerifyRecords = %v", tt.name, format, err)
			}
		}
	}
	_, env := readEnv(t, Format, ``)
	if env.Checkouts != nil {
		t.Errorf("no member read as %#v, want nil", env.Checkouts)
	}
	back, err := MarshalEntry(env)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := members(t, back)["checkouts"]; ok {
		t.Errorf("no member written back as %s", back)
	}
}

// TestCheckoutsRoundTrip: a list written in memory, undefined members
// at each level and an empty list among them, writes and reads back as
// it was.
func TestCheckoutsRoundTrip(t *testing.T) {
	c := Checkout{Path: ".worktrees/fix", Repo: ".git", VCS: &VCS{System: "git", Revision: revFix}, Branch: "fix", Upstream: &Upstream{Name: "origin/fix", Revision: revFix}}
	if err := c.SetMember("acme:tree", "t1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Upstream.SetMember("acme:pushed", true); err != nil {
		t.Fatal(err)
	}
	if err := c.SetMember("branch", "x"); err == nil {
		t.Error("SetMember of a defined member succeeded")
	}
	for _, list := range [][]Checkout{{c}, {}} {
		e := NewEnvEntry("/w")
		e.Checkouts = list
		if _, err := New(Header{ID: "w"}).Append(e); err != nil {
			t.Fatal(err)
		}
		data, err := MarshalEntry(e)
		if err != nil {
			t.Fatal(err)
		}
		back, err := UnmarshalEntry(data)
		if err != nil {
			t.Fatal(err)
		}
		got := back.(*EnvEntry).Checkouts
		if got == nil || len(got) != len(list) {
			t.Fatalf("%s read back as %#v", data, got)
		}
		again, err := MarshalEntry(back)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(data, again) {
			t.Errorf("round trip\n got %s\nwant %s", again, data)
		}
	}
	both := NewEnvEntry("/w")
	both.Checkouts = []Checkout{}
	both.Unknown = map[string]json.RawMessage{"checkouts": json.RawMessage(`[]`)}
	if _, err := MarshalEntry(both); err == nil {
		t.Error("checkouts both typed and unknown was written")
	}
}

// TestCheckoutsAppend: a writer is held to the forms, and to the
// checkout at "." carrying the entry's vcs.
func TestCheckoutsAppend(t *testing.T) {
	git := func(rev string) *VCS { return &VCS{System: "git", Revision: rev} }
	for _, tt := range []struct {
		name string
		vcs  *VCS
		list []Checkout
		want error
	}{
		{"agrees", git(revMain), []Checkout{{Path: ".", VCS: git(revMain)}}, nil},
		{"both absent", nil, []Checkout{{Path: "."}}, nil},
		{"no dot", git(revMain), []Checkout{{Path: "sub", VCS: git(revLib)}}, nil},
		{"absolute", nil, []Checkout{{Path: "/srv/lib", Repo: "/srv/lib/.git"}}, nil},
		{"differs", git(revMain), []Checkout{{Path: ".", VCS: git(revFix)}}, ErrCheckoutVCS},
		{"dot unread", git(revMain), []Checkout{{Path: "."}}, ErrCheckoutVCS},
		{"entry unsaid", nil, []Checkout{{Path: ".", VCS: git(revMain)}}, ErrCheckoutVCS},
		{"no path", nil, []Checkout{{Repo: ".git"}}, errors.New("path")},
		{"climbs", nil, []Checkout{{Path: "../x"}}, errors.New("path")},
		{"twice", nil, []Checkout{{Path: "a"}, {Path: "a"}}, errors.New("twice")},
		{"bad repo", nil, []Checkout{{Path: "a", Repo: "a/../b"}}, errors.New("repo")},
	} {
		s := New(Header{ID: "w"})
		e := NewEnvEntry("/home/u/work")
		e.VCS, e.Checkouts = tt.vcs, tt.list
		_, err := s.Append(e)
		switch {
		case tt.want == nil && err != nil:
			t.Errorf("%s: %v", tt.name, err)
		case tt.want == ErrCheckoutVCS && !errors.Is(err, ErrCheckoutVCS):
			t.Errorf("%s: %v, want ErrCheckoutVCS", tt.name, err)
		case tt.want != nil && tt.want != ErrCheckoutVCS && (err == nil || !strings.Contains(err.Error(), tt.want.Error())):
			t.Errorf("%s: %v, want an error naming the %s", tt.name, err, tt.want)
		}
	}
}

// TestCheckoutVCSReported: a file whose checkout at "." carries a vcs
// other than the entry's reads, the entry's vcs standing for the tree
// at cwd, and VerifyRecords reports it.
func TestCheckoutVCSReported(t *testing.T) {
	s, env := readEnv(t, Format, `,"vcs":{"system":"git","revision":"`+revMain+`"},"checkouts":[{"path":".","vcs":{"system":"git","revision":"`+revFix+`"}}]`)
	if env.VCS.Revision != revMain || env.Checkout(".").VCS.Revision != revFix {
		t.Errorf("read as %+v", env)
	}
	if err := s.VerifyRecords(s.Leaf()); !errors.Is(err, ErrCheckoutVCS) {
		t.Errorf("VerifyRecords = %v, want ErrCheckoutVCS", err)
	}
	// A 0.21 writer had no rule for a member its minor did not define.
	s, _ = readEnv(t, "agentsession/0.21", `,"vcs":{"system":"git","revision":"`+revMain+`"},"checkouts":[{"path":".","vcs":{"system":"git","revision":"`+revFix+`"}}]`)
	if err := s.VerifyRecords(s.Leaf()); err != nil {
		t.Errorf("at 0.21, VerifyRecords = %v", err)
	}
}

// TestCheckoutChanges names each kind of move, a nil entry and one
// with no member as listing nothing, and an undefined member's change
// as other, compared in canonical form.
func TestCheckoutChanges(t *testing.T) {
	base := func() Checkout {
		return Checkout{Path: "a", Repo: ".git", VCS: &VCS{System: "git", Revision: revMain}, Branch: "main", Upstream: &Upstream{Name: "origin/main", Revision: revMain}}
	}
	with := func(list ...Checkout) *EnvEntry { return &EnvEntry{Checkouts: list} }
	kinds := func(cs []CheckoutChange) string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Path+" "+string(c.Kind))
		}
		return strings.Join(out, "; ")
	}
	mod := func(f func(*Checkout)) Checkout { c := base(); f(&c); return c }
	member := func(key string, raw string) Checkout {
		c := base()
		c.Unknown = map[string]json.RawMessage{key: json.RawMessage(raw)}
		return c
	}
	for _, tt := range []struct {
		name          string
		before, after *EnvEntry
		want          string
	}{
		{"same", with(base()), with(base()), ""},
		{"from nil", nil, with(base()), "a added"},
		{"to nil", with(base()), nil, "a removed"},
		{"from no member", &EnvEntry{}, with(base()), "a added"},
		{"revision", with(base()), with(mod(func(c *Checkout) { c.VCS.Revision = revFix })), "a revision"},
		{"branch", with(base()), with(mod(func(c *Checkout) { c.Branch = "" })), "a branch"},
		{"upstream gone", with(base()), with(mod(func(c *Checkout) { c.Upstream = nil })), "a upstream"},
		{"dirty", with(base()), with(mod(func(c *Checkout) { c.VCS.Dirty = true })), "a dirty"},
		{"repo", with(base()), with(mod(func(c *Checkout) { c.Repo = "other/.git" })), "a other"},
		{"unread", with(base()), with(mod(func(c *Checkout) { c.VCS = nil })), "a revision; a other"},
		{"undefined member", with(member("acme:tree", `"t1"`)), with(member("acme:tree", `"t2"`)), "a other"},
		{"undefined member, respelled", with(member("acme:tree", `{"b":1,"a":2}`)), with(member("acme:tree", `{"a":2, "b":1}`)), ""},
		{"order", with(base(), Checkout{Path: "b"}), with(Checkout{Path: "c"}, mod(func(c *Checkout) { c.Branch = "x" })), "c added; a branch; b removed"},
	} {
		if got := kinds(CheckoutChanges(tt.before, tt.after)); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
		}
	}
}

// TestCheckoutsRedaction: a rewrite reaches a checkout's strings as
// body text, its paths among them, and leaves the format's member
// names as they are.
func TestCheckoutsRedaction(t *testing.T) {
	e := NewEnvEntry("/home/u/work")
	e.Checkouts = []Checkout{{Path: "/home/u/lib", Repo: "/home/u/lib/.git", VCS: &VCS{System: "git", Revision: revLib}, Branch: "u/fix",
		Upstream: &Upstream{Name: "origin/u/fix", Revision: revLib}}}
	home := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "/home/u", "~"), "u/fix", "x/fix")
	}
	out, err := RewriteStrings(e, home)
	if err != nil {
		t.Fatal(err)
	}
	r := out.(*EnvEntry)
	if r.CWD != "~/work" {
		t.Errorf("cwd %q", r.CWD)
	}
	c := r.Checkout("~/lib")
	if c == nil || c.Repo != "~/lib/.git" || c.Branch != "x/fix" || c.Upstream == nil || c.Upstream.Name != "origin/x/fix" {
		t.Fatalf("rewritten checkouts %+v", r.Checkouts)
	}
	if len(c.Unknown) != 0 || len(c.Upstream.Unknown) != 0 {
		t.Errorf("a member name was rewritten: %+v %+v", c.Unknown, c.Upstream.Unknown)
	}
}
