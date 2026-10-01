package agentsession

import (
	"bytes"
	"encoding/json"
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
