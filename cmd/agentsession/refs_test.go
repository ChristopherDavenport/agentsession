package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

// refsStore makes a cas store with two sessions, two refs and a pin.
func refsStore(t *testing.T) (root, a, b, pin string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b = "sess-a", "sess-b"
	for _, id := range []string{a, b} {
		if _, err := st.Create(ctx, agentsession.Header{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Append(ctx, a, say("first session says hello")); err != nil {
		t.Fatal(err)
	}
	if pin, err = st.Append(ctx, b, say("second session pins this")); err != nil {
		t.Fatal(err)
	}
	for _, u := range []struct {
		name         string
		old, current agentsession.RefTarget
		reason       string
	}{
		{"chat/one", agentsession.RefTarget{}, agentsession.RefTarget{Session: a}, "created"},
		{"chat/one", agentsession.RefTarget{Session: a}, agentsession.RefTarget{Session: b}, "continued"},
		{"chat/two", agentsession.RefTarget{}, agentsession.RefTarget{Session: a}, "created"},
		{"base", agentsession.RefTarget{}, agentsession.RefTarget{Session: b, Entry: pin}, "baseline"},
	} {
		if err := st.UpdateRef(ctx, u.name, u.old, u.current, u.reason); err != nil {
			t.Fatal(err)
		}
	}
	return root, a, b, pin
}

// row reports whether some line of out has exactly these fields, so a
// test does not depend on how a table pads its columns.
func row(out string, fields ...string) bool {
	for _, line := range strings.Split(out, "\n") {
		if got := strings.Fields(line); len(got) == len(fields) && strings.Join(got, "\x00") == strings.Join(fields, "\x00") {
			return true
		}
	}
	return false
}

func runCmd(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestRefsList(t *testing.T) {
	root, a, b, pin := refsStore(t)
	code, out, errOut := runCmd("refs", root)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	for _, want := range [][]string{
		{"NAME", "SESSION", "ENTRY", "STATE"},
		{"base", b, shortID(pin), "ok"},
		{"chat/one", b, "-", "ok"},
		{"chat/two", a, "-", "ok"},
	} {
		if !row(out, want...) {
			t.Errorf("refs output has no row %v:\n%s", want, out)
		}
	}
	// In name order.
	if strings.Index(out, "base") > strings.Index(out, "chat/one") || strings.Index(out, "chat/one") > strings.Index(out, "chat/two") {
		t.Errorf("refs not in name order:\n%s", out)
	}
	code, out, _ = runCmd("refs", root, "chat/")
	if code != 0 || strings.Contains(out, "base") || !strings.Contains(out, "chat/two") {
		t.Errorf("refs with a prefix (exit %d):\n%s", code, out)
	}
	// A dangling ref is said to be.
	st, err := cas.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	st.Close()
	_, out, _ = runCmd("refs", root, "chat/two")
	if !strings.Contains(out, "dangling") {
		t.Errorf("a ref to a deleted session is not marked:\n%s", out)
	}
	if code, _, errOut := runCmd("refs"); code != 2 || !strings.Contains(errOut, "usage") {
		t.Errorf("refs with no root: exit %d, %s", code, errOut)
	}
	if code, _, _ := runCmd("refs", root+"/nowhere"); code != 1 {
		t.Errorf("refs of a missing root: exit %d, want 1", code)
	}
}

func TestRefShow(t *testing.T) {
	root, a, b, pin := refsStore(t)
	code, out, errOut := runCmd("ref", root, "chat/one")
	if code != 0 || !row(out, "session", b) || !row(out, "state", "ok") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if strings.Contains(out, "TIME") {
		t.Errorf("ref without -log printed the log:\n%s", out)
	}
	_, out, _ = runCmd("ref", root, "base")
	if !row(out, "entry", pin) {
		t.Errorf("a pinned ref does not show its entry:\n%s", out)
	}
	code, out, _ = runCmd("ref", root, "chat/one", "--log")
	if code != 0 {
		t.Fatalf("ref --log: exit %d", code)
	}
	// Newest first: the move to b before the creation at a.
	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 4 && f[0] != "TIME" {
			rows = append(rows, strings.Join(f[1:], " "))
		}
	}
	if want := []string{a + " " + b + " continued", "- " + a + " created"}; strings.Join(rows, "|") != strings.Join(want, "|") {
		t.Errorf("log rows %q, want %q, newest first:\n%s", rows, want, out)
	}
	if code, _, errOut := runCmd("ref", root, "no/such"); code != 1 || !strings.Contains(errOut, "no such ref") {
		t.Errorf("a missing ref: exit %d, %s", code, errOut)
	}
	if code, _, _ := runCmd("ref", root); code != 2 {
		t.Errorf("ref with no name: exit %d, want 2", code)
	}
	// A deleted ref still has a log to read.
	st, _ := cas.Open(root)
	if err := st.UpdateRef(context.Background(), "chat/two", agentsession.RefTarget{Session: a}, agentsession.RefTarget{}, "retired"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	code, out, _ = runCmd("ref", root, "chat/two", "-log")
	if code != 0 || !strings.Contains(out, "retired") || !row(out, "state", "none") {
		t.Errorf("log of a deleted ref (exit %d):\n%s", code, out)
	}
}

// TestShowRef: show takes ref:<name> where it takes a session id, and
// reads the session the ref points to.
func TestShowRef(t *testing.T) {
	root, _, b, _ := refsStore(t)
	code, out, errOut := runCmd("show", root, "ref:chat/one")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if !row(out, "session", b) || !strings.Contains(out, "second session pins this") {
		t.Errorf("show ref:chat/one did not show session %s:\n%s", b, out)
	}
	if !strings.Contains(errOut, "ref:chat/one is session "+b) {
		t.Errorf("stderr does not say which session the ref named: %s", errOut)
	}
	// verify and export take a session too.
	if code, _, errOut := runCmd("verify", root, "ref:chat/one"); code != 0 {
		t.Errorf("verify ref:chat/one: exit %d: %s", code, errOut)
	}
	if code, _, errOut := runCmd("show", root, "ref:no/such"); code != 1 || !strings.Contains(errOut, "no such ref") {
		t.Errorf("show of a missing ref: exit %d: %s", code, errOut)
	}
	if code, _, errOut := runCmd("show", root, "ref:bad name"); code != 1 || !strings.Contains(errOut, "invalid ref name") {
		t.Errorf("show of a bad ref name: exit %d: %s", code, errOut)
	}
}

func TestRefsJSONL(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := jsonl.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{ID: "j1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "chat/x", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID()}, "made"); err != nil {
		t.Fatal(err)
	}
	code, out, errOut := runCmd("refs", root)
	if code != 0 || !row(out, "chat/x", "j1", "-", "ok") {
		t.Fatalf("exit %d:\n%s%s", code, out, errOut)
	}
	if code, out, _ := runCmd("ref", root, "chat/x", "-log"); code != 0 || !strings.Contains(out, "made") {
		t.Errorf("ref -log of a jsonl store (exit %d):\n%s", code, out)
	}
}
