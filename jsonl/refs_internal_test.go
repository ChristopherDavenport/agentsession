package jsonl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

func newRefSession(t *testing.T, st *Store) string {
	t.Helper()
	s, err := st.Create(context.Background(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	return s.ID()
}

func rt(id string) agentsession.RefTarget { return agentsession.RefTarget{Session: id} }

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

func appendRaw(t *testing.T, st *Store, data []byte) {
	t.Helper()
	f, err := os.OpenFile(st.refLogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
}

func recLine(t *testing.T, r refRecord) []byte {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func filesRef(t *testing.T, st *Store, name string) string {
	t.Helper()
	doc, err := st.readRefsDoc()
	if err != nil {
		t.Fatal(err)
	}
	return doc.Refs[name].Session
}

// TestRefCrashRecordWithoutFile is a crash between the log record and
// the rename of refs.json: the file lags its log by one record. The
// record is the update's commit, so a read sees it, and the next update
// makes the file say it.
func TestRefCrashRecordWithoutFile(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b, c := newRefSession(t, st), newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt(a), "one"); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, st, recLine(t, refRecord{Seq: 2, Name: "r", Old: refTarget{Session: a}, New: refTarget{Session: b}, At: time.Now().UTC().Format(time.RFC3339Nano), Reason: "crashed"}))
	if got := filesRef(t, st, "r"); got != a {
		t.Fatalf("setup: refs.json holds %q, want %s", got, a)
	}
	ro, err := Open(st.root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ro.ResolveRef(ctx, "r"); err != nil || got != rt(b) {
		t.Errorf("read-only ResolveRef = %v, %v; want %s, the recorded target", got, err, b)
	}
	if got := filesRef(t, st, "r"); got != a {
		t.Errorf("a read wrote refs.json: %q", got)
	}
	var moved *agentsession.RefMovedError
	if err := st.UpdateRef(ctx, "r", rt(a), rt(c), ""); !errors.As(err, &moved) || moved.Current != rt(b) {
		t.Fatalf("update from the stale target: %v, want ErrRefMoved holding %s", err, b)
	}
	if got := filesRef(t, st, "r"); got != b {
		t.Errorf("the refused update did not finish the record: refs.json holds %q, want %s", got, b)
	}
	if err := st.UpdateRef(ctx, "r", rt(b), rt(c), "after"); err != nil {
		t.Fatal(err)
	}
	if log := logOf(t, st, "r"); len(log) != 3 || log[0].New != rt(c) || log[1].New != rt(b) || log[2].New != rt(a) {
		t.Errorf("log: %+v, want three updates, each once", log)
	}
}

// TestRefCutAndDamagedRecords: a last line with no newline is a record
// being written, cut by the next update; a whole line that does not
// read is damage, reported, and the update refused.
func TestRefCutAndDamagedRecords(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, b := newRefSession(t, st), newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt(a), "one"); err != nil {
		t.Fatal(err)
	}
	line := recLine(t, refRecord{Seq: 2, Name: "r", Old: refTarget{Session: a}, New: refTarget{Session: b}})
	appendRaw(t, st, line[:len(line)/2])
	if got := logOf(t, st, "r"); len(got) != 1 {
		t.Fatalf("a cut record is read as an update: %+v", got)
	}
	if got, err := st.ResolveRef(ctx, "r"); err != nil || got != rt(a) {
		t.Fatalf("ResolveRef = %v, %v; want %s", got, err, a)
	}
	if err := st.UpdateRef(ctx, "r", rt(a), rt(b), "two"); err != nil {
		t.Fatal(err)
	}
	if got := logOf(t, st, "r"); len(got) != 2 || got[0].Reason != "two" {
		t.Errorf("log after cutting: %+v, want two updates, the newest \"two\"", got)
	}
	appendRaw(t, st, []byte("{not json}\n"))
	var saw error
	for _, err := range st.RefLog(ctx, "r") {
		saw = err
	}
	if saw == nil {
		t.Error("RefLog skipped a damaged line")
	}
	if err := st.UpdateRef(ctx, "r", rt(b), agentsession.RefTarget{}, ""); err == nil {
		t.Error("UpdateRef went on over a damaged last record")
	}
	if got := filesRef(t, st, "r"); got != b {
		t.Errorf("a refused update changed the ref to %q", got)
	}
}

// TestRefStaleLockTakenOver: the lock of a process on this host that is
// gone is taken over, as a session's is, and the update goes through.
func TestRefStaleLockTakenOver(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newRefSession(t, st)
	host, _ := os.Hostname()
	data, _ := json.Marshal(LockInfo{PID: 2147483000, Host: host, Since: time.Now()})
	if err := os.WriteFile(lockPath(st.refsPath()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	var reported []LockInfo
	st.staleReport = func(l LockInfo) { reported = append(reported, l) }
	done := make(chan error, 1)
	go func() { done <- st.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt(a), "") }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the update waited on a lock whose holder is gone")
	}
	if len(reported) != 1 {
		t.Errorf("stale lock reported %d times, want 1", len(reported))
	}
	// The lock is let go after an update.
	if _, err := os.Stat(lockPath(st.refsPath())); err == nil {
		t.Error("refs.json.lock left behind")
	}
}

func holdAt(t *testing.T, stage string) (reached <-chan struct{}, release func()) {
	t.Helper()
	r, gate := make(chan struct{}), make(chan struct{})
	var once sync.Once
	old := refStep
	refStep = func(s string) {
		if s == stage {
			once.Do(func() { close(r); <-gate })
		}
	}
	t.Cleanup(func() { refStep = old })
	return r, func() { close(gate) }
}

// TestRefUpdatesTakeTurns holds one update between its compare and its
// write while a second, from another store on the root, arrives: it
// waits for the lock, and then loses to the first.
func TestRefUpdatesTakeTurns(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(st.root)
	if err != nil {
		t.Fatal(err)
	}
	a, b := newRefSession(t, st), newRefSession(t, st)
	reached, release := holdAt(t, "checked")
	first := make(chan error, 1)
	go func() { first <- st.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt(a), "first") }()
	<-reached
	second := make(chan error, 1)
	go func() { second <- other.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt(b), "second") }()
	select {
	case err := <-second:
		t.Fatalf("a second update finished while the first held the lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	var moved *agentsession.RefMovedError
	if err := <-second; !errors.As(err, &moved) || moved.Current != rt(a) {
		t.Errorf("the second update: %v, want ErrRefMoved holding %s", err, a)
	}
}

// TestRefLogRecordsIdentity: refs.log carries the identity of the
// session each target named.
func TestRefLogRecordsIdentity(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := newRefSession(t, st)
	if err := st.UpdateRef(context.Background(), "r", agentsession.RefTarget{}, rt(a), ""); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(st.refLogPath())
	if !strings.Contains(string(data), `"ident":"`) {
		t.Errorf("log lacks the identity: %s", data)
	}
}

// TestRefRecordedAdoptionIsSeen: the record of a ref's move to a session
// created again under its ID, with the file one record behind, resolves
// to the new incarnation, and the next update finishes the file.
func TestRefRecordedAdoptionIsSeen(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "reused", CWD: "/first"}); err != nil {
		t.Fatal(err)
	}
	other := newRefSession(t, st)
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, rt("reused"), "set"); err != nil {
		t.Fatal(err)
	}
	st.Release("reused")
	if err := st.Delete(ctx, "reused"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "reused", CWD: "/second"}); err != nil {
		t.Fatal(err)
	}
	doc, _ := st.readRefsDoc()
	fresh, _ := st.incarnation("reused")
	appendRaw(t, st, recLine(t, refRecord{Seq: doc.Seq + 1, Name: "r", Old: doc.Refs["r"], New: refTarget{Session: "reused", Ident: fresh}, At: time.Now().UTC().Format(time.RFC3339Nano), Reason: "adopt"}))
	if got, err := st.ResolveRef(ctx, "r"); err != nil || got != rt("reused") {
		t.Fatalf("ResolveRef = %v, %v; want it resolving", got, err)
	}
	if err := st.UpdateRef(ctx, "r", rt("reused"), rt(other), "move"); err != nil {
		t.Fatal(err)
	}
	if log := logOf(t, st, "r"); len(log) != 3 || log[1].Reason != "adopt" {
		t.Errorf("log %+v, want set, adopt, move once each", log)
	}
}
