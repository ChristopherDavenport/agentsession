package spanner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gs "cloud.google.com/go/spanner"
	database "cloud.google.com/go/spanner/admin/database/apiv1"
	"cloud.google.com/go/spanner/admin/database/apiv1/databasepb"
	instance "cloud.google.com/go/spanner/admin/instance/apiv1"
	"cloud.google.com/go/spanner/admin/instance/apiv1/instancepb"
	"google.golang.org/grpc/codes"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/storetest"
	"github.com/ChristopherDavenport/openresponses"
)

// The tests run against the Cloud Spanner emulator: start it with
// `gcloud emulators spanner start` and set
// SPANNER_EMULATOR_HOST=localhost:9010. Without it they skip, except in
// CI, where a missing emulator would otherwise pass unnoticed. Each
// store gets a database of its own in one instance.
const (
	testProject = "agentsession-test"
	instName    = "agentsession"
)

var (
	setupOnce sync.Once
	setupErr  error
	dbCounter atomic.Int64
	runID     = time.Now().UnixNano() % 1_000_000
)

func setup(ctx context.Context) error {
	ic, err := instance.NewInstanceAdminClient(ctx)
	if err != nil {
		return err
	}
	defer ic.Close()
	op, err := ic.CreateInstance(ctx, &instancepb.CreateInstanceRequest{
		Parent:     "projects/" + testProject,
		InstanceId: instName,
		Instance: &instancepb.Instance{
			Config:      "projects/" + testProject + "/instanceConfigs/emulator-config",
			DisplayName: instName,
			NodeCount:   1,
		},
	})
	if gs.ErrCode(err) == codes.AlreadyExists {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = op.Wait(ctx)
	return err
}

// newDatabase creates an empty database with the store's schema and
// returns its name.
func newDatabase(t *testing.T) string {
	t.Helper()
	if os.Getenv("SPANNER_EMULATOR_HOST") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("SPANNER_EMULATOR_HOST is not set in CI")
		}
		t.Skip("SPANNER_EMULATOR_HOST is not set; start the Spanner emulator to run these tests")
	}
	ctx := context.Background()
	setupOnce.Do(func() { setupErr = setup(ctx) })
	if setupErr != nil {
		t.Fatalf("emulator instance: %v", setupErr)
	}
	dc, err := database.NewDatabaseAdminClient(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer dc.Close()
	id := fmt.Sprintf("t%d-%d", runID, dbCounter.Add(1))
	op, err := dc.CreateDatabase(ctx, &databasepb.CreateDatabaseRequest{
		Parent:          "projects/" + testProject + "/instances/" + instName,
		CreateStatement: "CREATE DATABASE `" + id + "`",
		ExtraStatements: DDL,
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := op.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		dc, err := database.NewDatabaseAdminClient(context.Background())
		if err != nil {
			return
		}
		defer dc.Close()
		_ = dc.DropDatabase(context.Background(), &databasepb.DropDatabaseRequest{Database: db.Name})
	})
	return db.Name
}

func openStore(t *testing.T, db string) *Store {
	t.Helper()
	st, err := Open(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	db := newDatabase(t)
	return openStore(t, db), db
}

func TestConformance(t *testing.T) {
	dbs := map[agentsession.Store]string{}
	var mu sync.Mutex
	storetest.Run(t, storetest.Options{
		New: func(t *testing.T) agentsession.Store {
			st, db := newStore(t)
			mu.Lock()
			dbs[st] = db
			mu.Unlock()
			return st
		},
		Reopen: func(t *testing.T, s agentsession.Store) agentsession.Store {
			mu.Lock()
			db := dbs[s]
			mu.Unlock()
			if err := s.(*Store).Close(); err != nil {
				t.Fatal(err)
			}
			st := openStore(t, db)
			mu.Lock()
			dbs[st] = db
			mu.Unlock()
			return st
		},
	})
}

func mustAppend(t *testing.T, st *Store, id string, e agentsession.Entry) string {
	t.Helper()
	got, err := st.Append(context.Background(), id, e)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func count(t *testing.T, st *Store, table string) int {
	t.Helper()
	var n int64
	err := st.client.Single().Query(context.Background(), gs.Statement{SQL: "SELECT COUNT(*) FROM " + table}).Do(func(r *gs.Row) error {
		return r.Columns(&n)
	})
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// TestObjectsAreShared checks the point of the layout: the same body in
// two sessions is one content with two envelopes, and a fork shares its
// origin's entries through prefix rows rather than copying them.
func TestObjectsAreShared(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	a, _ := st.Create(ctx, agentsession.Header{ID: "a"})
	b, _ := st.Create(ctx, agentsession.Header{ID: "b"})
	e1 := agentsession.NewItemEntry(openresponses.UserText("same tool output"))
	e2 := agentsession.NewItemEntry(openresponses.UserText("same tool output"))
	id1 := mustAppend(t, st, a.ID(), e1)
	id2 := mustAppend(t, st, b.ID(), e2)
	if id1 == id2 {
		t.Fatal("two sessions' entries share an id; ts should differ")
	}
	if got, want := count(t, st, "Contents"), 1; got != want {
		t.Errorf("%d contents, want %d", got, want)
	}
	if got, want := count(t, st, "Entries"), 2; got != want {
		t.Errorf("%d entries, want %d", got, want)
	}
	f, err := st.Create(ctx, agentsession.Header{ID: "f", Base: id1})
	if err != nil {
		t.Fatal(err)
	}
	if got := count(t, st, "Entries"); got != 2 {
		t.Errorf("a fork stored %d entries, want none new", got-2)
	}
	if f.Leaf() != id1 || f.Header().ParentSession != "a" {
		t.Errorf("fork leaf %s parent %q, want %s and a", f.Leaf(), f.Header().ParentSession, id1)
	}
}

// TestTwoWriters is the RFC's concurrency model across two stores on
// one database, which is two processes: both append under the head,
// both succeed, the first continues and the second is a branch, and the
// head stays with the first.
func TestTwoWriters(t *testing.T) {
	ctx := context.Background()
	st1, db := newStore(t)
	st2 := openStore(t, db)
	s1, err := st1.Create(ctx, agentsession.Header{ID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	root := mustAppend(t, st1, "s", agentsession.NewItemEntry(openresponses.UserText("q")))
	s2, err := st2.Open(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s2.Leaf() != root {
		t.Fatalf("second writer's leaf %s, want %s", s2.Leaf(), root)
	}
	r1, err := st1.Write(ctx, "s", agentsession.NewItemEntry(openresponses.AssistantText("first")))
	if err != nil {
		t.Fatal(err)
	}
	r2, err := st2.Write(ctx, "s", agentsession.NewItemEntry(openresponses.AssistantText("second")))
	if err != nil {
		t.Fatal(err)
	}
	if r1.Outcome != agentsession.Continued || r2.Outcome != agentsession.Branched {
		t.Errorf("outcomes %v and %v, want continued and branched", r1.Outcome, r2.Outcome)
	}
	if e, _ := s2.Entry(r2.ID); e.Base().Parent != root {
		t.Errorf("the second writer's entry hangs from %s, want %s", e.Base().Parent, root)
	}
	if head, _ := st1.Head(ctx, "s"); head != r1.ID {
		t.Errorf("head %s, want the first writer's %s", head, r1.ID)
	}
	if s2.Leaf() != r1.ID {
		t.Errorf("second writer's leaf %s after its branch, want the head %s", s2.Leaf(), r1.ID)
	}
	if _, err := st1.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s1.Entry(r2.ID); !ok {
		t.Error("the first writer's session never saw the second writer's entry")
	}
	if s1.Len() != 3 || s2.Len() != 3 {
		t.Errorf("lengths %d and %d, want 3", s1.Len(), s2.Len())
	}
}

// TestConcurrentAppends has many goroutines across two stores append
// to one session at once: every append lands exactly once, in one log
// order both stores agree on.
func TestConcurrentAppends(t *testing.T) {
	ctx := context.Background()
	st1, db := newStore(t)
	st2 := openStore(t, db)
	if _, err := st1.Create(ctx, agentsession.Header{ID: "c"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st1, "c", agentsession.NewItemEntry(openresponses.UserText("go")))
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		st := st1
		if i%2 == 1 {
			st = st2
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.Append(ctx, "c", agentsession.NewItemEntry(openresponses.AssistantText(fmt.Sprintf("w%d", i))))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st3 := openStore(t, db)
	s, err := st3.Open(ctx, "c")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != n+1 {
		t.Errorf("%d entries, want %d", s.Len(), n+1)
	}
	a, _ := st1.Open(ctx, "c")
	b, _ := st2.Open(ctx, "c")
	for i, e := range s.Entries() {
		if a.Entries()[i].Base().ID != e.Base().ID || b.Entries()[i].Base().ID != e.Base().ID {
			t.Fatalf("stores disagree on the log at %d", i)
		}
	}
}

// TestHead covers the compare-and-swap and a leaf moved by Branch.
func TestHead(t *testing.T) {
	ctx := context.Background()
	st, db := newStore(t)
	s, _ := st.Create(ctx, agentsession.Header{ID: "h"})
	a := mustAppend(t, st, "h", agentsession.NewItemEntry(openresponses.UserText("a")))
	b := mustAppend(t, st, "h", agentsession.NewItemEntry(openresponses.AssistantText("b")))
	if err := st.SetHead(ctx, "h", a, a); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("SetHead from a stale head: %v, want ErrHeadMoved", err)
	}
	if err := st.SetHead(ctx, "h", b, a); err != nil {
		t.Fatal(err)
	}
	if s.Leaf() != a {
		t.Errorf("leaf %s after SetHead, want %s", s.Leaf(), a)
	}
	// A leaf moved through Branch is a head move, and races another
	// writer's compare-and-swap.
	other := openStore(t, db)
	if err := s.Branch(b); err != nil {
		t.Fatal(err)
	}
	if err := other.SetHead(ctx, "h", a, a); err != nil {
		t.Fatal(err) // a no-op move, which still counts as the head seen
	}
	r, err := st.Write(ctx, "h", agentsession.NewItemEntry(openresponses.UserText("c")))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != agentsession.Continued {
		t.Errorf("append after Branch: %v, want continued", r.Outcome)
	}
	if e, _ := s.Entry(r.ID); e.Base().Parent != b {
		t.Errorf("append after Branch hangs from %s, want %s", e.Base().Parent, b)
	}
	if err := other.SetHead(ctx, "h", r.ID, a); err != nil {
		t.Fatal(err)
	}
	if err := s.Branch(b); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Write(ctx, "h", agentsession.NewItemEntry(openresponses.UserText("d"))); !errors.Is(err, ErrHeadMoved) {
		t.Errorf("append after a Branch another writer beat: %v, want ErrHeadMoved", err)
	}
	if s.Leaf() != a {
		t.Errorf("leaf %s after ErrHeadMoved, want the other writer's head %s", s.Leaf(), a)
	}
	// A leaf label moves the head to its target, never onto itself.
	r, err = st.Write(ctx, "h", agentsession.NewLabelEntry(b, agentsession.LeafLabel))
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != agentsession.LeafMoved {
		t.Errorf("leaf label: %v, want leaf moved", r.Outcome)
	}
	if head, _ := st.Head(ctx, "h"); head != b {
		t.Errorf("head %s after a leaf label, want %s", head, b)
	}
	if err := st.SetHead(ctx, "h", b, r.ID); err == nil {
		t.Error("SetHead onto a leaf label succeeded")
	}
}

// TestProjectAndImport projects a fork with a moved head, imports it
// into another database, and reads back the same entries and head.
func TestProjectAndImport(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	st.Create(ctx, agentsession.Header{ID: "o"})
	q := mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("q")))
	f, err := st.Create(ctx, agentsession.Header{ID: "f", Base: q})
	if err != nil {
		t.Fatal(err)
	}
	a := mustAppend(t, st, "f", agentsession.NewItemEntry(openresponses.AssistantText("a")))
	mustAppend(t, st, "f", agentsession.NewItemEntry(openresponses.UserText("more")))
	if err := st.SetHead(ctx, "f", f.Leaf(), a); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := st.Project(ctx, &buf, "f"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"synthetic":true`) {
		t.Error("projection with a moved head carries no synthetic marker")
	}
	other, _ := newStore(t)
	got, err := other.Import(ctx, bytes.NewReader(buf.Bytes()), false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Leaf() != a || got.Len() != f.Len() {
		t.Errorf("imported leaf %s len %d, want %s and %d", got.Leaf(), got.Len(), a, f.Len())
	}
	if rec, _ := other.Record(ctx, "f"); rec {
		t.Error("an import is the record without being declared one")
	}
	if _, err := other.Append(ctx, "f", agentsession.NewItemEntry(openresponses.UserText("x"))); !errors.Is(err, ErrMirror) {
		t.Errorf("append to a mirror: %v, want ErrMirror", err)
	}
	if err := other.DeclareRecord(ctx, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Append(ctx, "f", agentsession.NewItemEntry(openresponses.UserText("x"))); err != nil {
		t.Errorf("append after DeclareRecord: %v", err)
	}
	var again bytes.Buffer
	if err := other.Project(ctx, &again, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Import(ctx, bytes.NewReader(buf.Bytes()), false); !errors.Is(err, agentsession.ErrSessionExists) {
		t.Errorf("second import: %v, want ErrSessionExists", err)
	}
}

// TestSweep deletes a session and sweeps: its own entries go at once,
// their contents and a blob no content names once their grace runs out,
// and what a fork's prefix names stays.
func TestSweep(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	st.Create(ctx, agentsession.Header{ID: "o"})
	q := mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.UserText("q")))
	mustAppend(t, st, "o", agentsession.NewItemEntry(openresponses.AssistantText("gone")))
	if _, err := st.Create(ctx, agentsession.Header{ID: "f", Base: q}); err != nil {
		t.Fatal(err)
	}
	blob, err := st.PutBlob(ctx, []byte("image bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, "o"); err != nil {
		t.Fatal(err)
	}
	n, err := st.Sweep(ctx, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 { // the abandoned entry; its content is young
		t.Errorf("swept %d, want 1", n)
	}
	f, err := st.Open(ctx, "f")
	if err != nil {
		t.Fatalf("the fork after its origin was swept: %v", err)
	}
	if _, ok := f.Entry(q); !ok {
		t.Error("the fork's prefix was swept")
	}
	if _, err := st.Blob(ctx, blob); err != nil {
		t.Errorf("a young blob was swept: %v", err)
	}
	if n, err := st.Sweep(ctx, -time.Hour); err != nil || n != 2 {
		t.Errorf("sweep past the grace: %d, %v; want 2, the content and the blob", n, err)
	}
	if _, err := st.Blob(ctx, blob); err == nil {
		t.Error("the blob outlived its grace")
	}
}

// TestSidecarBlobs checks a sidecar blob named by an item survives a
// sweep and is reported when the store lacks it.
func TestSidecarBlobs(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	st.Create(ctx, agentsession.Header{ID: "m", Media: agentsession.MediaSidecar})
	blob, err := st.PutBlob(ctx, []byte("png"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := st.Write(ctx, "m", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+blob)))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unresolved) != 0 {
		t.Errorf("a held blob reported unresolved: %v", r.Unresolved)
	}
	missing := agentsession.HashBytes([]byte("absent"))
	r, err = st.Write(ctx, "m", agentsession.NewItemEntry(openresponses.UserText("see sidecar:"+missing)))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Unresolved) != 1 || r.Unresolved[0] != missing {
		t.Errorf("unresolved %v, want [%s]", r.Unresolved, missing)
	}
	if n, err := st.Sweep(ctx, -time.Hour); err != nil || n != 0 {
		t.Errorf("sweep with every blob named: %d, %v; want 0", n, err)
	}
	dir := t.TempDir()
	if _, err := st.ProjectDir(ctx, dir, "m"); err == nil {
		t.Error("ProjectDir with a blob the store lacks succeeded")
	}
}

// TestRefusals covers what an append and a create must refuse.
func TestRefusals(t *testing.T) {
	ctx := context.Background()
	st, _ := newStore(t)
	s, _ := st.Create(ctx, agentsession.Header{ID: "r"})
	a := mustAppend(t, st, "r", agentsession.NewItemEntry(openresponses.UserText("a")))
	label := mustAppend(t, st, "r", agentsession.NewLabelEntry(a, agentsession.LeafLabel))
	if _, err := st.Create(ctx, agentsession.Header{ID: "x", Base: label}); err == nil {
		t.Error("a fork at a leaf label was created")
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "x", Base: agentsession.HashBytes([]byte("nothing"))}); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("a fork at an entry nobody holds: %v, want ErrNoEntry", err)
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "y", Base: a, Media: agentsession.MediaSidecar}); err == nil {
		t.Error("a fork whose media differs from its origin's was created")
	}
	if _, err := st.Create(ctx, agentsession.Header{ID: "r"}); !errors.Is(err, agentsession.ErrSessionExists) {
		t.Errorf("a second create of one ID: %v, want ErrSessionExists", err)
	}
	marker := agentsession.NewLabelEntry(a, agentsession.LeafLabel)
	marker.Unknown = map[string]json.RawMessage{"synthetic": json.RawMessage("true")}
	if _, err := st.Append(ctx, "r", marker); !errors.Is(err, ErrSynthetic) {
		t.Errorf("a synthetic marker: %v, want ErrSynthetic", err)
	}
	if _, err := s.Append(agentsession.NewItemEntry(openresponses.UserText("behind the store"))); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Append(ctx, "r", agentsession.NewItemEntry(openresponses.UserText("b"))); !errors.Is(err, ErrModified) {
		t.Errorf("append after the session was modified directly: %v, want ErrModified", err)
	}
	if _, err := st.Open(ctx, strings.Repeat("x", 201)); !errors.Is(err, ErrBadName) {
		t.Errorf("an ID longer than the key: %v, want ErrBadName", err)
	}
}
