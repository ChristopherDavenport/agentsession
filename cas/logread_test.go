package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/followtest"
	"github.com/ChristopherDavenport/openresponses"
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

// A log line can hold more than one record. When the follower reads one
// whose second append's objects are not there yet, it yields neither
// append until they land, then both, in order: before, it yielded the
// first with the line's cursor and went on past the line, so it, and a
// follower resuming from the cursor it was given, never saw the second.
func TestTheFollowerWaitsForAWholeLine(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st, err := Open(t.TempDir(), WithFollowInterval(5*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	mustAppend(t, st, "s", item("one"))

	changes := make(chan agentsession.Change, 16)
	go func() {
		for c, err := range st.Follow(ctx, "s", "") {
			if err != nil {
				return
			}
			changes <- c
		}
	}()
	snap := <-changes
	if snap.Kind != agentsession.Snapshot {
		t.Fatalf("the first change is %v", snap.Kind)
	}

	// Two appends prepared on a copy of the session, the first's objects
	// stored and the second's not, their records written as one line.
	sess, err := st.Read(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	e1, e2 := item("two"), item("three")
	for _, e := range []agentsession.Entry{e1, e2} {
		if _, err := sess.Commit(e); err != nil {
			t.Fatal(err)
		}
	}
	size1, _, err := st.storeEntry(e1, true, newPendSet())
	if err != nil {
		t.Fatal(err)
	}
	r1, err := logRecord{Op: opAppend, Session: "s", Entry: e1.Base().ID, Head: e1.Base().ID, Seq: 2, Size: size1}.encode()
	if err != nil {
		t.Fatal(err)
	}
	r2, err := logRecord{Op: opAppend, Session: "s", Entry: e2.Base().ID, Head: e2.Base().ID, Seq: 3, Size: 1}.encode()
	if err != nil {
		t.Fatal(err)
	}
	dir, err := st.sessionDir("s")
	if err != nil {
		t.Fatal(err)
	}
	f, err := openLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	line := append(bytes.TrimSuffix(r1, []byte{'\n'}), r2...)
	if _, err := f.Write(line); err != nil {
		t.Fatal(err)
	}
	f.Close()
	st.hub.Notify("s")

	select {
	case c := <-changes:
		t.Fatalf("a change before the line's objects are all there: %v %s", c.Kind, c.ID)
	case <-time.After(300 * time.Millisecond):
	}

	if _, _, err := st.storeEntry(e2, true, newPendSet()); err != nil {
		t.Fatal(err)
	}
	st.hub.Notify("s")
	var got []string
	var cursor agentsession.Cursor
	for len(got) < 2 {
		select {
		case c := <-changes:
			if c.Kind == agentsession.Appended {
				got = append(got, c.ID)
				cursor = c.Cursor
			}
		case <-ctx.Done():
			t.Fatalf("the follower yielded %v, never both appends", got)
		}
	}
	if got[0] != e1.Base().ID || got[1] != e2.Base().ID {
		t.Errorf("the follower yielded %v, want %s then %s", got, e1.Base().ID, e2.Base().ID)
	}

	// A follower resuming from the cursor before the line sees both; one
	// resuming from the line's cursor, given with the second, sees none
	// again.
	resumed := func(from agentsession.Cursor) []string {
		t.Helper()
		rctx, rcancel := context.WithTimeout(ctx, 500*time.Millisecond)
		defer rcancel()
		var ids []string
		for c, err := range st.Follow(rctx, "s", from) {
			if err != nil {
				t.Fatal(err)
			}
			if c.Kind == agentsession.Appended {
				ids = append(ids, c.ID)
			}
		}
		return ids
	}
	if ids := resumed(snap.Cursor); len(ids) != 2 || ids[1] != e2.Base().ID {
		t.Errorf("resumed before the line: %v, want both appends", ids)
	}
	if ids := resumed(cursor); len(ids) != 0 {
		t.Errorf("resumed from the line's cursor: %v, want nothing", ids)
	}
}

// TestGatedEntryReadAndFollowed: a session whose header names 0.12 and
// one of whose entries holds redacted, which 0.12 does not define, is
// read by ReadLog, durable or not, and by a follow that meets the entry
// in the log's tail, with the entry not redacted and the session one
// that may not be raised.
func TestGatedEntryReadAndFollowed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	if _, err := st.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("first"))); err != nil {
		t.Fatal(err)
	}
	before, err := st.LogCursor(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	e := agentsession.NewItemEntry(openresponses.UserText("hi"))
	if err := agentsession.SetRedacted(e, []agentsession.Span{{From: 1, To: 1}}); err != nil {
		t.Fatal(err)
	}
	eid, err := st.Append(ctx, id, e)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	lowerHeader(t, root, id, "agentsession/0.12")
	st, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	gated := func(what string, e agentsession.Entry, sess *agentsession.Session) {
		t.Helper()
		if spans, ok := agentsession.RedactedOf(e); ok {
			t.Errorf("%s: the entry reads as redacted %v under 0.12", what, spans)
		}
		if err := sess.RaiseRefused(agentsession.Format); !errors.Is(err, agentsession.ErrNoRaise) {
			t.Errorf("%s: RaiseRefused = %v, want ErrNoRaise", what, err)
		}
		if f := sess.Header().Format; f != "agentsession/0.12" {
			t.Errorf("%s: the session is at %s", what, f)
		}
	}
	for _, durable := range []bool{false, true} {
		lg, err := st.ReadLog(ctx, id, "", durable)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, r := range lg.Records {
			if r.Entry != nil && r.Entry.Base().ID == eid {
				found = true
				gated(fmt.Sprintf("ReadLog durable=%v", durable), r.Entry, lg.Session)
			}
		}
		if !found {
			t.Errorf("ReadLog durable=%v: no record of %s", durable, eid)
		}
		lg, err = st.ReadLog(ctx, id, before, durable)
		if err != nil {
			t.Fatal(err)
		}
		if len(lg.Records) != 1 || lg.Reset {
			t.Fatalf("ReadLog from the cursor before the entry: %d records, reset %v", len(lg.Records), lg.Reset)
		}
		gated(fmt.Sprintf("ReadLog from a cursor, durable=%v", durable), lg.Records[0].Entry, lg.Session)
	}
	w := followtest.Start(t, st, id, before)
	defer w.Stop()
	c := w.NextKind(agentsession.Appended)
	if c.ID != eid {
		t.Fatalf("the follow appended %s, want %s", c.ID, eid)
	}
	gated("Follow", c.Entry, c.Session)
}
