package agentsession

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHashFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got, err := HashFile(filepath.Join(dir, "a.txt"))
	if err != nil || got != want {
		t.Errorf("HashFile = %q, %v", got, err)
	}
	if HashBytes([]byte("hello")) != want {
		t.Error("HashBytes")
	}
	m, err := HashFiles(dir, "a.txt")
	if err != nil || m["a.txt"] != want {
		t.Errorf("HashFiles = %v, %v", m, err)
	}
	if _, err := HashFiles(dir, "missing"); err == nil {
		t.Error("HashFiles(missing) succeeded")
	}
	if _, err := HashFile(dir); err == nil {
		t.Error("HashFile(dir) succeeded")
	}
}

func TestEnvEntryBuilders(t *testing.T) {
	e := NewEnvEntry("/p")
	e.AddRead("a", "sha256:1")
	e.AddWritten("b", "sha256:2")
	e.AddWritten("c", "sha256:3")
	e.AddTool("go", "1.25")
	want := &EnvEntry{CWD: "/p", Files: &FileHashes{Read: map[string]string{"a": "sha256:1"}, Written: map[string]string{"b": "sha256:2", "c": "sha256:3"}}, Tools: map[string]string{"go": "1.25"}}
	if !reflect.DeepEqual(e, want) {
		t.Errorf("env = %+v", e)
	}
	// Not a git checkout: VCS stays unset without error.
	if err := e.SetGit(t.TempDir(), false); err != nil || e.VCS != nil {
		t.Errorf("SetGit(non-git) = %v, vcs %+v", err, e.VCS)
	}
}

func TestGitRevision(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef01234567"
	other := "89abcdef0123456789abcdef0123456789abcdef"
	mk := func(t *testing.T, layout map[string]string) string {
		dir := t.TempDir()
		for name, content := range layout {
			path := filepath.Join(dir, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	tests := []struct {
		name   string
		layout map[string]string
		sub    string
		want   string
		err    error
	}{
		{
			name:   "loose ref",
			layout: map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": sha + "\n"},
			want:   sha,
		},
		{
			name:   "from subdirectory",
			layout: map[string]string{".git/HEAD": "ref: refs/heads/main\n", ".git/refs/heads/main": sha + "\n", "sub/dir/x": ""},
			sub:    "sub/dir",
			want:   sha,
		},
		{
			name:   "detached",
			layout: map[string]string{".git/HEAD": sha + "\n"},
			want:   sha,
		},
		{
			name: "packed ref",
			layout: map[string]string{".git/HEAD": "ref: refs/heads/main\n",
				".git/packed-refs": "# pack-refs with: peeled fully-peeled sorted\n" + other + " refs/heads/other\n^" + sha + "\n" + sha + " refs/heads/main\n"},
			want: sha,
		},
		{
			name: "worktree gitdir file",
			layout: map[string]string{".git": "gitdir: worktrees/wt\n", "worktrees/wt/HEAD": "ref: refs/heads/feature\n",
				"worktrees/wt/commondir": "../..\n", "refs/heads/feature": sha + "\n"},
			want: sha,
		},
		{
			name:   "unborn",
			layout: map[string]string{".git/HEAD": "ref: refs/heads/main\n"},
			err:    errors.New("unborn"),
		},
		{
			name:   "not git",
			layout: map[string]string{"x": ""},
			err:    ErrNotGit,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := mk(t, tt.layout)
			got, err := GitRevision(filepath.Join(dir, filepath.FromSlash(tt.sub)))
			if tt.err != nil {
				if err == nil {
					t.Fatalf("GitRevision = %q, want error", got)
				}
				if errors.Is(tt.err, ErrNotGit) && !errors.Is(err, ErrNotGit) {
					t.Errorf("err = %v, want ErrNotGit", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("GitRevision = %q, %v; want %q", got, err, tt.want)
			}
			e := NewEnvEntry(dir)
			if err := e.SetGit(dir, true); err != nil || e.VCS == nil || e.VCS.Revision != tt.want || !e.VCS.Dirty || e.VCS.System != "git" {
				t.Errorf("SetGit: %v, %+v", err, e.VCS)
			}
		})
	}
}

func TestOutcomeAndLinkBuilders(t *testing.T) {
	o := NewOutcomeEntry(OutcomeTest, "r1").WithScore(0.5).WithLabel("3/6").WithDetails(map[string]any{"failed": []string{"x"}})
	if o.Kind != OutcomeTest || o.Target != "r1" || *o.Score != 0.5 || o.Label != "3/6" || string(o.Details) != `{"failed":["x"]}` {
		t.Errorf("outcome = %+v", o)
	}
	bad := NewOutcomeEntry(OutcomeCustom, "").WithDetails(make(chan int))
	if bad.Details != nil {
		t.Error("unencodable details were set")
	}
	l := NewSubsessionLink("s2", "call_1")
	if l.Rel != RelSubsession || l.Session != "s2" || l.CallID != "call_1" {
		t.Errorf("link = %+v", l)
	}
	if l := NewLinkEntry(RelContinuedIn, "s3"); l.Rel != RelContinuedIn || l.CallID != "" {
		t.Errorf("link = %+v", l)
	}
	if l := NewLabelEntry("a", "x"); l.Target != "a" || l.Label == nil || *l.Label != "x" {
		t.Errorf("label = %+v", l)
	}
	if l := NewLabelEntry("a", ""); l.Label != nil {
		t.Errorf("clearing label = %+v", l)
	}
}

// TestSameWorkspace is the substitution rule over the members that
// tell one file system from another (#95): a container restarted from
// the same image, or moved to another host, is another workspace, and
// its host and instance are members of workspace, which the rule
// compares, rather than of the entry, which it does not.
func TestSameWorkspace(t *testing.T) {
	on := func(host, instance string) *Workspace {
		var e EnvEntry
		w := e.SetWorkspace(WorkspaceContainer, "sha256:ab")
		if err := w.SetMember("host", host); err != nil {
			t.Fatal(err)
		}
		if err := w.SetMember("instance", instance); err != nil {
			t.Fatal(err)
		}
		return e.Workspace
	}
	tests := []struct {
		name string
		a, b *Workspace
		same bool
	}{
		{"both absent", nil, nil, true},
		{"one absent", nil, &Workspace{Kind: WorkspaceLocal}, false},
		{"equal", on("h1", "ctr-1"), on("h1", "ctr-1"), true},
		{"another instance", on("h1", "ctr-1"), on("h1", "ctr-2"), false},
		{"another host", on("h1", "ctr-1"), on("h2", "ctr-1"), false},
		{"a member absent", on("h1", "ctr-1"), &Workspace{Kind: WorkspaceContainer, Ref: "sha256:ab"}, false},
		{"members spelled apart", &Workspace{Kind: "local", Unknown: map[string]json.RawMessage{"n": json.RawMessage(`1.0`)}}, &Workspace{Kind: "local", Unknown: map[string]json.RawMessage{"n": json.RawMessage(`1`)}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SameWorkspace(tt.a, tt.b); got != tt.same {
				t.Errorf("SameWorkspace = %v, want %v", got, tt.same)
			}
		})
	}
	// Set through the setter, the members reach the file inside
	// workspace and read back as they were written.
	s := New(Header{})
	env := &EnvEntry{CWD: "/w"}
	env.Workspace = on("h1", "ctr-1")
	if _, err := s.Append(env); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"workspace":{"host":"h1","instance":"ctr-1","kind":"container","ref":"sha256:ab"}`) {
		t.Errorf("workspace written as %s", buf.String())
	}
	back, err := Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !SameWorkspace(back.Entries()[0].(*EnvEntry).Workspace, env.Workspace) {
		t.Error("the workspace did not read back as written")
	}
	var w Workspace
	if err := w.SetMember("kind", "x"); err == nil {
		t.Error("SetMember took a member the format defines")
	}
}

// TestWorkspaceWithoutKind: kind is written only when set, so a
// workspace without one reads and writes back as it was.
func TestWorkspaceWithoutKind(t *testing.T) {
	for _, w := range []string{`{}`, `{"ref":"x"}`, `{"Kind":"container"}`} {
		line := `{"type":"env","parent":null,"ts":"2026-09-17T16:00:01Z","workspace":` + w + `}`
		id, _, err := EntryHashes([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		line = `{"id":"` + id + `",` + line[1:]
		head := `{"type":"session","format":"agentsession/0.6","id":"s","created_at":"2026-09-17T16:00:00Z","payload":"openresponses/2026-04-24"}`
		if _, err := Read(strings.NewReader(head + "\n" + line + "\n")); err != nil {
			t.Errorf("%s: %v", w, err)
		}
	}
}

// TestJudgedByLink: a judged session names its judge by a judged_by
// link, whose target names the entry judged; Judges reads them off a
// path, a link of another relation is no judge, and a target belongs to
// the judged_by relation alone (#145).
func TestJudgedByLink(t *testing.T) {
	l := NewJudgedByLink("judge-1", "r1")
	if l.Rel != RelJudgedBy || l.Session != "judge-1" || l.Target != "r1" || l.CallID != "" {
		t.Errorf("link = %+v", l)
	}
	s := New(Header{})
	a := appendText(t, s, "work")
	for _, e := range []Entry{
		NewJudgedByLink("judge-1", a),
		NewLinkEntry(RelForkOf, "elsewhere"),
		NewJudgedByLink("judge-2", ""),
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	judges, err := s.Judges(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	if len(judges) != 2 || judges[0].Session != "judge-1" || judges[0].Target != a || judges[1].Session != "judge-2" || judges[1].Target != "" {
		t.Errorf("Judges = %+v", judges)
	}
	if len(Judges(s.Path(a))) != 0 {
		t.Error("a path above the links holds a judge")
	}
	if _, err := s.Judges("nowhere"); !errors.Is(err, ErrNoEntry) {
		t.Errorf("Judges of a missing entry = %v, want ErrNoEntry", err)
	}
	if _, err := s.Append(&LinkEntry{Rel: RelForkOf, Session: "x", Target: a}); err == nil {
		t.Error("a target on a fork_of link was appended")
	}
}

// TestLinkTargetOfAnotherRelation: a target member on a link that is not
// judged_by, which a file from before the member was defined may hold,
// is not the member this document defines, and is kept as written.
func TestLinkTargetOfAnotherRelation(t *testing.T) {
	line := `{"type":"link","id":"k","parent":null,"ts":"2026-09-17T12:00:01Z","rel":"fork_of","session":"s2","target":"x"}`
	e, err := UnmarshalEntry([]byte(line))
	if err != nil {
		t.Fatal(err)
	}
	l := e.(*LinkEntry)
	if l.Target != "" || string(l.Unknown["target"]) != `"x"` {
		t.Errorf("target = %q, unknown %v", l.Target, l.Unknown)
	}
	back, err := MarshalEntry(l)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(t, back, []byte(line)) {
		t.Errorf("rewritten as %s", back)
	}
	// The same member that is not a string is kept too, on judged_by.
	e, err = UnmarshalEntry([]byte(strings.Replace(strings.Replace(line, "fork_of", "judged_by", 1), `"x"`, `7`, 1)))
	if err != nil {
		t.Fatal(err)
	}
	if l := e.(*LinkEntry); l.Target != "" || string(l.Unknown["target"]) != `7` {
		t.Errorf("a numeric target: %q, unknown %v", l.Target, l.Unknown)
	}
}
