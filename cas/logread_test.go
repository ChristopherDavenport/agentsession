package cas

import (
	"context"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// The log read lists the session's records in log order, an append with
// its entry, its place and the head after it, a head move with the head,
// each with the cursor the store's follower gives after it, so a read
// resumes from a follow's cursor and a follow from a read's.
func TestReadLogIsTheFollowersLog(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids := fill(t, st, "s", 3)
	if err := st.SetHead(ctx, "s", ids[2], ids[0]); err != nil {
		t.Fatal(err)
	}
	branch := mustAppend(t, st, "s", item("beside"))

	log, err := st.ReadLog(ctx, "s", "", false)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		entry string
		seq   int
		head  string
	}
	var got []rec
	for _, r := range log.Records {
		id := ""
		if r.Entry != nil {
			id = r.Entry.Base().ID
		}
		got = append(got, rec{id, r.Seq, r.Head})
		if !r.Durable {
			t.Errorf("record %+v is not durable under the default sync policy", r)
		}
	}
	want := []rec{{ids[0], 1, ids[0]}, {ids[1], 2, ids[1]}, {ids[2], 3, ids[2]}, {"", 0, ids[0]}, {branch, 4, branch}}
	if len(got) != len(want) {
		t.Fatalf("records %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if log.Session.Leaf() != branch || log.Mark != MarkRecord || log.Header.ID != "s" || log.Reset {
		t.Errorf("log: leaf %s, mark %q, header %s, reset %v", log.Session.Leaf(), log.Mark, log.Header.ID, log.Reset)
	}

	// The follower's cursors are the read's.
	var cursors []agentsession.Cursor
	fctx, cancel := context.WithCancel(ctx)
	for c, err := range st.Follow(fctx, "s", log.Records[0].Cursor) {
		if err != nil {
			t.Fatal(err)
		}
		if c.Kind == agentsession.Appended || c.Kind == agentsession.Head {
			if len(cursors) == 0 || cursors[len(cursors)-1] != c.Cursor {
				cursors = append(cursors, c.Cursor)
			}
		}
		if len(cursors) == len(log.Records)-1 {
			break
		}
	}
	cancel()
	for i, c := range cursors {
		if c != log.Records[i+1].Cursor {
			t.Errorf("the follower's cursor %d is %s, the read's %s", i, c, log.Records[i+1].Cursor)
		}
	}
	if end, err := st.LogCursor(ctx, "s"); err != nil || end != log.Cursor {
		t.Errorf("LogCursor %s (%v), want the read's end %s", end, err, log.Cursor)
	}

	// From a cursor, the records after it.
	after, err := st.ReadLog(ctx, "s", log.Records[2].Cursor, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Records) != 2 || after.Records[0].Entry != nil || after.Records[1].Entry.Base().ID != branch || after.Reset {
		t.Errorf("after the third record: %+v", after.Records)
	}
	if end, err := st.ReadLog(ctx, "s", log.Cursor, false); err != nil || len(end.Records) != 0 || end.Cursor != log.Cursor {
		t.Errorf("from the end: %d records, cursor %s (%v)", len(end.Records), end.Cursor, err)
	}
}

// A cursor from before the log was replaced reads the whole log again,
// marked as a reset, never a silent skip.
func TestReadLogResetsAStaleCursor(t *testing.T) {
	ctx := context.Background()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fill(t, st, "s", 2)
	log, err := st.ReadLog(ctx, "s", "", false)
	if err != nil {
		t.Fatal(err)
	}
	stale := agentsession.Cursor("999:1.1:-:x:0.0")
	got, err := st.ReadLog(ctx, "s", stale, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Reset || len(got.Records) != len(log.Records) {
		t.Errorf("a stale cursor: reset %v, %d records, want a reset with %d", got.Reset, len(got.Records), len(log.Records))
	}
	if _, err := st.ReadLog(ctx, "none", "", false); !errors.Is(err, agentsession.ErrNoSession) {
		t.Errorf("an unknown session: %v", err)
	}
}

// Under a lazy sync policy an append is accepted before it is committed.
// The log read says so; a durable read commits the session this store
// holds first, so its writer's appends are served; and a store that
// cannot commit another process's working state, read-only here, serves
// only what a commit covers.
func TestReadLogDurable(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	w, err := Open(root, WithSync(SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	first := mustAppend(t, w, "s", item("one"))
	if err := w.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	second := mustAppend(t, w, "s", item("two"))

	log, err := w.ReadLog(ctx, "s", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Records) != 2 || !log.Records[0].Durable || log.Records[1].Durable {
		t.Fatalf("before a commit: %+v, want the first durable and the second not", log.Records)
	}

	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	committed, err := ro.ReadLog(ctx, "s", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Records) != 1 || committed.Records[0].Entry.Base().ID != first || committed.Session.Leaf() != first {
		t.Errorf("a read-only durable read: %+v, leaf %s, want the committed first append alone", committed.Records, committed.Session.Leaf())
	}
	if committed.Cursor != committed.Records[0].Cursor && committed.Cursor == log.Cursor {
		t.Errorf("a read-only durable read ends at %s, past what is committed", committed.Cursor)
	}

	served, err := w.ReadLog(ctx, "s", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(served.Records) != 2 || !served.Records[1].Durable || served.Records[1].Entry.Base().ID != second {
		t.Errorf("the holder's durable read: %+v, want both, committed", served.Records)
	}
	// The holder committed: another reader now sees both committed.
	after, err := ro.ReadLog(ctx, "s", committed.Cursor, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Records) != 1 || after.Records[0].Entry.Base().ID != second || !after.Records[0].Durable {
		t.Errorf("after the holder's commit: %+v", after.Records)
	}
}

// A fetch admits a session read from elsewhere as Fetch does: a store
// that lacked it holds it as a mirror, and the head moves on a mirror
// only to a head that descends from its own.
func TestReceiveFetch(t *testing.T) {
	ctx := context.Background()
	rec, mir := twoStores(t)
	ids := fill(t, rec, "s", 2)
	c, err := rec.Closure(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	x, err := mir.Receive(ctx, c, ReceiveOptions{Fetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !x.Created || x.Head != ids[1] {
		t.Errorf("a fetch into a store that lacked the session: %+v", x)
	}
	if m, _ := mir.Mark(ctx, "s"); m != MarkMirror {
		t.Errorf("the fetcher's mark is %s, want mirror", m)
	}
	third := mustAppend(t, rec, "s", item("three"))
	if c, err = rec.Closure(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if x, err = mir.Receive(ctx, c, ReceiveOptions{Fetch: true}); err != nil || x.Admitted != 1 || !x.HeadMoved || x.Head != third {
		t.Errorf("a second fetch: %+v (%v)", x, err)
	}
}
