package agentsession

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
