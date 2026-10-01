package agentsession

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVCSMembers: a namespaced member set inside vcs reaches the file
// inside vcs, reads back as written, keeps the entry's id across a
// rewrite, and a change to it is not a substitution (#177).
func TestVCSMembers(t *testing.T) {
	treeEnv := func(tree string) *EnvEntry {
		e := &EnvEntry{CWD: "/w", VCS: &VCS{System: "git", Revision: "ab", Dirty: true}}
		e.SetWorkspace(WorkspaceLocal, "")
		if err := e.VCS.SetMember("cline:tree", tree); err != nil {
			t.Fatal(err)
		}
		return e
	}
	s := New(Header{})
	first, second := treeEnv("sha1:t1"), treeEnv("sha1:t2")
	for _, e := range []Entry{first, second} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	var buf bytes.Buffer
	if err := Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"vcs":{"cline:tree":"sha1:t1","dirty":true,"revision":"ab","system":"git"}`) {
		t.Errorf("vcs written as %s", buf.String())
	}
	back, err := Read(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []*EnvEntry{first, second} {
		got, ok := back.Entry(want.ID)
		if !ok {
			t.Fatalf("entry %d: id %s did not read back", i, want.ID)
		}
		v := got.(*EnvEntry).VCS
		if v.System != "git" || v.Revision != "ab" || !v.Dirty || string(v.Unknown["cline:tree"]) != string(want.VCS.Unknown["cline:tree"]) {
			t.Errorf("entry %d: vcs = %+v", i, v)
		}
	}
	var again bytes.Buffer
	if err := Write(&again, back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.Bytes(), buf.Bytes()) {
		t.Errorf("rewrite changed the file\nfirst  %s\nsecond %s", buf.Bytes(), again.Bytes())
	}
	// The tree moved and the workspace did not: no substitution.
	if !SameWorkspace(first.Workspace, second.Workspace) {
		t.Error("a change inside vcs reads as a substitution")
	}

	// SetGit keeps a member set before it, and one read from a file.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	sha := "0123456789abcdef0123456789abcdef01234567"
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte(sha+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	set := &EnvEntry{CWD: dir, VCS: &VCS{}}
	if err := set.VCS.SetMember("cline:tree", "sha1:t3"); err != nil {
		t.Fatal(err)
	}
	read, _ := back.Entry(first.ID)
	for name, e := range map[string]*EnvEntry{"set": set, "read": read.(*EnvEntry)} {
		want := string(e.VCS.Unknown["cline:tree"])
		if err := e.SetGit(dir, false); err != nil {
			t.Fatal(err)
		}
		if e.VCS.Revision != sha || e.VCS.Dirty || string(e.VCS.Unknown["cline:tree"]) != want || want == "" {
			t.Errorf("%s: after SetGit vcs = %+v, want cline:tree %s", name, e.VCS, want)
		}
	}

	var v VCS
	for _, key := range []string{"system", "revision", "dirty"} {
		if err := v.SetMember(key, "x"); err == nil {
			t.Errorf("SetMember took %q, a member the format defines", key)
		}
	}
	bad := VCS{System: "git", Unknown: map[string]json.RawMessage{"revision": json.RawMessage(`"x"`)}}
	if _, err := json.Marshal(bad); err == nil {
		t.Error("a defined member in Unknown encoded")
	}
}
