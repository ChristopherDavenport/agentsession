package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

func refTarget(id string) agentsession.RefTarget { return agentsession.RefTarget{Session: id} }

func newRefSession(t *testing.T, st *Store) string {
	t.Helper()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

func logOf(t *testing.T, st *Store, name string) []agentsession.RefUpdate {
	t.Helper()
	var out []agentsession.RefUpdate
	for u, err := range st.RefLog(context.Background(), name) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

// appendRaw adds bytes to a ref's log, as a crashed writer leaves them.
func appendRaw(t *testing.T, st *Store, name string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(st.refLogPath(name)), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(st.refLogPath(name), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func refFileHolds(t *testing.T, st *Store, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(st.root, refsDir, refFile(name)))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// TestRefCrashRecordWithoutRename is a crash after the log record and
// before the rename: the ref file lags its log. The record is the
// update's commit, so a read of the ref sees it, and the next update
// under the lock finishes the rename.
func TestRefCrashRecordWithoutRename(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b, c := newRefSession(t, st), newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, refTarget(a), "one"); err != nil {
		t.Fatal(err)
	}
	rec, err := newRefRecord("r", refTarget(a), refTarget(b), time.Now(), "crashed").encode()
	if err != nil {
		t.Fatal(err)
	}
	appendRaw(t, st, "r", rec)
	if got := refFileHolds(t, st, "r"); got != a {
		t.Fatalf("setup: the file holds %q, want %s", got, a)
	}

	// Another store on the root, a reader, sees the recorded target.
	ro, err := Open(st.root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := ro.ResolveRef(ctx, "r"); err != nil || got != refTarget(b) {
		t.Errorf("read-only ResolveRef = %v, %v; want %s, the recorded target", got, err, b)
	}
	if got := refFileHolds(t, st, "r"); got != a {
		t.Errorf("a read wrote the ref file: %q", got)
	}
	// The expected value is the recorded one.
	var moved *agentsession.RefMovedError
	if err := st.UpdateRef(ctx, "r", refTarget(a), refTarget(c), ""); !errors.As(err, &moved) || moved.Current != refTarget(b) {
		t.Fatalf("update from the stale target: %v, want ErrRefMoved holding %s", err, b)
	}
	if got := refFileHolds(t, st, "r"); got != b {
		t.Errorf("the refused update did not finish the rename: file holds %q, want %s", got, b)
	}
	if err := st.UpdateRef(ctx, "r", refTarget(b), refTarget(c), "after"); err != nil {
		t.Fatal(err)
	}
	log := logOf(t, st, "r")
	if len(log) != 3 || log[0].New != refTarget(c) || log[1].New != refTarget(b) || log[2].New != refTarget(a) {
		t.Errorf("log: %+v, want three updates, each once", log)
	}
}

// TestRefCrashRenameWithoutRecord is the other order: the file changed
// and no record says so, as after a crash with no write-ahead, or an
// edit by hand. The next update logs it.
func TestRefCrashRenameWithoutRecord(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b, c := newRefSession(t, st), newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, refTarget(a), "one"); err != nil {
		t.Fatal(err)
	}
	if err := st.placeRef("r", refTarget(b)); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveRef(ctx, "r"); err != nil || got != refTarget(b) {
		t.Fatalf("ResolveRef = %v, %v; want the file's %s", got, err, b)
	}
	if err := st.UpdateRef(ctx, "r", refTarget(b), refTarget(c), "after"); err != nil {
		t.Fatal(err)
	}
	log := logOf(t, st, "r")
	if len(log) != 3 {
		t.Fatalf("log: %+v, want create, the recovered move and the update", log)
	}
	if rec := log[1]; rec.Old != refTarget(a) || rec.New != refTarget(b) || !strings.HasPrefix(rec.Reason, "recovered") {
		t.Errorf("recovery record %+v, want a to b, recovered", rec)
	}
	// A ref created and never logged is logged too.
	if err := st.placeRef("fresh", refTarget(a)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "fresh", refTarget(a), refTarget(c), ""); err != nil {
		t.Fatal(err)
	}
	if log := logOf(t, st, "fresh"); len(log) != 2 || log[1].Old != (agentsession.RefTarget{}) || log[1].New != refTarget(a) {
		t.Errorf("log of a ref created with no record: %+v", log)
	}
}

// TestRefCrashCutRecord is a crash in the middle of the record: the
// last line has no newline. It is not damage and not an update; the
// next update cuts it.
func TestRefCrashCutRecord(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b := newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, refTarget(a), "one"); err != nil {
		t.Fatal(err)
	}
	rec, _ := newRefRecord("r", refTarget(a), refTarget(b), time.Now(), "").encode()
	appendRaw(t, st, "r", rec[:len(rec)/2])
	if got := logOf(t, st, "r"); len(got) != 1 {
		t.Fatalf("a cut record is read as an update: %+v", got)
	}
	if got, err := st.ResolveRef(ctx, "r"); err != nil || got != refTarget(a) {
		t.Fatalf("ResolveRef = %v, %v; want %s", got, err, a)
	}
	if err := st.UpdateRef(ctx, "r", refTarget(a), refTarget(b), "two"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(st.refLogPath("r"))
	if !bytes.HasSuffix(data, []byte("\n")) || bytes.Count(data, []byte("\n")) != 2 {
		t.Errorf("log after the update has %d lines, want 2, whole: %q", bytes.Count(data, []byte("\n")), data)
	}
	// The new record is its own line, not glued to the cut bytes.
	if got := logOf(t, st, "r"); len(got) != 2 || got[0].Reason != "two" {
		t.Errorf("log after the update: %+v, want two updates, the newest \"two\"", got)
	}
}

// TestRefLogDamage: a whole line that fails its checksum is damage,
// reported by the log and refused by the update, never skipped.
func TestRefLogDamage(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a, b := newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, refTarget(a), "one"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "r", refTarget(a), refTarget(b), "two"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(st.refLogPath("r"))
	data = bytes.Replace(data, []byte(`"reason":"two"`), []byte(`"reason":"twp"`), 1)
	if err := os.WriteFile(st.refLogPath("r"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	var saw error
	for _, err := range st.RefLog(ctx, "r") {
		saw = err
	}
	if !errors.Is(saw, ErrCorrupt) {
		t.Errorf("RefLog over a damaged record: %v, want ErrCorrupt", saw)
	}
	if err := st.UpdateRef(ctx, "r", refTarget(b), agentsession.RefTarget{}, ""); !errors.Is(err, ErrCorrupt) {
		t.Errorf("UpdateRef over a damaged last record: %v, want ErrCorrupt", err)
	}
	if got := refFileHolds(t, st, "r"); got != b {
		t.Errorf("a refused update changed the ref to %q", got)
	}
}

// TestRefSweepKeepsPin: a sweep keeps the entry a ref pins, and the
// path above it, after the session that wrote it is deleted, and
// removes the entry once the ref lets it go.
func TestRefSweepKeepsPin(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	root := mustAppend(t, st, s.ID(), agentsession.NewItemEntry(openresponses.UserText("root")))
	pin := mustAppend(t, st, s.ID(), agentsession.NewItemEntry(openresponses.UserText("pinned")))
	tip := mustAppend(t, st, s.ID(), agentsession.NewItemEntry(openresponses.UserText("past the pin")))
	keeper := newRefSession(t, st)
	if err := st.UpdateRef(ctx, "baseline", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID(), Entry: pin}, "pin"); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root, pin} {
		if ok, err := st.present(id); err != nil || !ok {
			t.Errorf("entry %s is gone after a sweep, though the ref pins it (%v)", id, err)
		}
	}
	if ok, _ := st.present(tip); ok {
		t.Errorf("the entry past the pin survived: nothing holds it")
	}
	// The ref moves off the pin; the next sweep takes what it kept.
	if err := st.UpdateRef(ctx, "baseline", agentsession.RefTarget{Session: s.ID(), Entry: pin}, refTarget(keeper), "unpin"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Sweep(ctx, 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root, pin} {
		if ok, _ := st.present(id); ok {
			t.Errorf("entry %s survived a sweep with nothing holding it", id)
		}
	}
}

// TestRefsTwoStoresOneName is #129's scenario: two harnesses, each with
// a store on one root, find the session of one name. The first gets
// it; the second is told it is held, and when the first lets go, gets
// the same session and creates none.
func TestRefsTwoStoresOneName(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	a, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	sa, err := agentsession.SessionFor(ctx, a, "handoff7", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agentsession.SessionFor(ctx, b, "handoff7", agentsession.Header{}); !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Fatalf("second store: %v, want ErrSessionLocked, the session being held", err)
	}
	if err := a.Release(sa.ID()); err != nil {
		t.Fatal(err)
	}
	sb, err := agentsession.SessionFor(ctx, b, "handoff7", agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if sb.ID() != sa.ID() {
		t.Errorf("the stores hold sessions %s and %s for one name", sa.ID(), sb.ID())
	}
	n := 0
	for _, err := range a.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d sessions for one name, want 1", n)
	}
}

// refRaceEnv names the helper process of TestRefsProcessRace.
const refRaceEnv = "CAS_REFS_RACE_ROOT"

// TestRefsRaceHelper is the body of one process of TestRefsProcessRace;
// it does nothing in a normal run.
func TestRefsRaceHelper(t *testing.T) {
	root := os.Getenv(refRaceEnv)
	if root == "" {
		t.Skip("helper process of TestRefsProcessRace")
	}
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for { // the start gate: every process begins together
		if _, err := os.Stat(filepath.Join(root, "go")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s, err := agentsession.SessionFor(context.Background(), st, "race/name", agentsession.Header{})
	switch {
	case err == nil:
		fmt.Printf("RESULT session %s\n", s.ID())
		time.Sleep(200 * time.Millisecond) // hold it while the others try
	case errors.Is(err, agentsession.ErrSessionLocked):
		fmt.Println("RESULT locked")
	default:
		fmt.Printf("RESULT error %v\n", err)
	}
}

// TestRefsProcessRace runs SessionFor for one name in several
// processes on one store at once: one session is created, one ref
// names it, and every process that got a session got that one.
func TestRefsProcessRace(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	const procs = 8
	for round := range 3 {
		root := t.TempDir()
		st, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		var cmds []*exec.Cmd
		var outs []*bytes.Buffer
		for range procs {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRefsRaceHelper$", "-test.v")
			cmd.Env = append(os.Environ(), refRaceEnv+"="+root)
			out := &bytes.Buffer{}
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			cmds, outs = append(cmds, cmd), append(outs, out)
		}
		time.Sleep(300 * time.Millisecond) // every helper has opened the store
		if err := os.WriteFile(filepath.Join(root, "go"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for i, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("round %d: process %d: %v\n%s", round, i, err, outs[i])
			}
		}
		ids := map[string]int{}
		for i, out := range outs {
			var res string
			for line := range strings.SplitSeq(out.String(), "\n") {
				if rest, ok := strings.CutPrefix(line, "RESULT "); ok {
					res = rest
				}
			}
			switch {
			case strings.HasPrefix(res, "session "):
				ids[strings.TrimPrefix(res, "session ")]++
			case res == "locked":
			default:
				t.Errorf("round %d: process %d: %q\n%s", round, i, res, out)
			}
		}
		check, err := Open(root)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := check.ResolveRef(context.Background(), "race/name")
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(ids) > 1 || (len(ids) == 1 && ids[ref.Session] == 0) {
			t.Errorf("round %d: processes got sessions %v, the ref names %s", round, ids, ref.Session)
		}
		n := 0
		for sum, err := range check.List(context.Background(), agentsession.ListFilter{}) {
			if err != nil {
				t.Fatal(err)
			}
			n++
			if sum.Header.ID != ref.Session {
				t.Errorf("round %d: session %s left behind by a loser", round, sum.Header.ID)
			}
		}
		if n != 1 {
			t.Errorf("round %d: %d sessions, want 1", round, n)
		}
		check.Close()
	}
}

// holdAt makes the update of name stop at stage until release is
// called, and says when one has.
func holdAt(t *testing.T, name, stage string) (reached <-chan struct{}, release func()) {
	t.Helper()
	r, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	old := refStep
	refStep = func(n, s string) {
		if n == name && s == stage {
			once.Do(func() { close(r); <-gate })
		}
	}
	t.Cleanup(func() { refStep = old })
	return r, func() { close(gate) }
}

// TestRefUpdatesTakeTurns holds one update between its compare and its
// write while a second arrives for the same ref: the second waits for
// the lock, and then loses to the first, whichever the interleaving.
func TestRefUpdatesTakeTurns(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	other, err := Open(st.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	a, b, c := newRefSession(t, st), newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, refTarget(a), "create"); err != nil {
		t.Fatal(err)
	}
	// Moves, not creations, which take the creation lock besides.
	reached, release := holdAt(t, "r", "checked")
	first := make(chan error, 1)
	go func() { first <- st.UpdateRef(ctx, "r", refTarget(a), refTarget(b), "first") }()
	<-reached
	second := make(chan error, 1)
	go func() { second <- other.UpdateRef(ctx, "r", refTarget(a), refTarget(c), "second") }()
	select {
	case err := <-second:
		t.Fatalf("a second update finished while the first held the ref: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	var moved *agentsession.RefMovedError
	if err := <-second; !errors.As(err, &moved) || moved.Current != refTarget(b) {
		t.Errorf("the second update: %v, want ErrRefMoved holding %s", err, b)
	}
}

// TestRefCreationsTakeTurns holds the creation of a ref while the
// creation of one under it arrives: the names are checked against each
// other, so the second waits and is then refused.
func TestRefCreationsTakeTurns(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := newRefSession(t, st)
	reached, release := holdAt(t, "p", "checked")
	first := make(chan error, 1)
	go func() { first <- st.UpdateRef(ctx, "p", agentsession.RefTarget{}, refTarget(s), "") }()
	<-reached
	second := make(chan error, 1)
	go func() { second <- st.UpdateRef(ctx, "p/q", agentsession.RefTarget{}, refTarget(s), "") }()
	select {
	case err := <-second:
		t.Fatalf("a creation finished while another, whose name it conflicts with, held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; !errors.Is(err, agentsession.ErrRefName) {
		t.Errorf("the creation under a ref: %v, want ErrRefName", err)
	}
}
