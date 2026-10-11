package remote_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/remote"
	"github.com/ChristopherDavenport/openresponses"
)

// sseEvent is one event of a raw follow, for the tests that read the
// wire itself.
type sseEvent struct{ name, id, data string }

// rawFollow reads the first n events of a follow, with the request
// shaped by header and query.
func rawFollow(t *testing.T, url string, header map[string]string, n int) []sseEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("follow: status %d", resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 16<<20)
	var out []sseEvent
	var ev sseEvent
	for len(out) < n && sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if ev.name != "" {
				out = append(out, ev)
			}
			ev = sseEvent{}
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "id: "):
			ev.id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

// The log read is the session's records in log order, each with its
// place, the head after it, what it did and a cursor; from a cursor it
// is the records after it, and a follow resumes from a log read's
// cursor. A store that keeps no log of records does not serve it.
func TestTheLogReadIsTheSessionsLog(t *testing.T) {
	ctx := context.Background()
	st := casStore(t)
	mustCreate(t, st, agentsession.Header{ID: "s"})
	a := mustAppend(t, st, "s", user("one"))
	b := mustAppend(t, st, "s", user("two"))
	if err := st.SetHead(ctx, "s", b, a); err != nil {
		t.Fatal(err)
	}
	beside := mustAppend(t, st, "s", user("beside"))
	url := server(t, st, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)

	lg, err := c.Log(ctx, "s", "", false)
	if err != nil {
		t.Fatal(err)
	}
	type rec struct {
		seq             int
		entry, head     string
		outcome         string
		durable, cursor bool
	}
	var got []rec
	for _, r := range lg.Records {
		got = append(got, rec{r.Seq, r.Entry, r.Head, r.Outcome, r.Durable, r.Cursor != ""})
	}
	want := []rec{
		{1, a, a, "continued", true, true},
		{2, b, b, "continued", true, true},
		{2, "", a, "", true, true},
		{3, beside, beside, "continued", true, true},
	}
	if !slices.Equal(got, want) {
		t.Errorf("records\n%+v\nwant\n%+v", got, want)
	}
	if lg.Header.ID != "s" || lg.Incarnation == "" || lg.Mark != cas.MarkRecord || lg.Head != beside || lg.Cursor == "" || lg.Reset {
		t.Errorf("log: header %s, incarnation %q, mark %q, head %s, cursor %q, reset %v", lg.Header.ID, lg.Incarnation, lg.Mark, lg.Head, lg.Cursor, lg.Reset)
	}
	rest, err := c.Log(ctx, "s", lg.Records[1].Cursor, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Records) != 2 || rest.Records[0].Entry != "" || rest.Records[1].Entry != beside {
		t.Errorf("after the second record: %+v", rest.Records)
	}
	empty, err := c.Log(ctx, "s", lg.Cursor, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Records) != 0 || empty.Cursor != lg.Cursor || empty.Head != beside || empty.Header.ID != "s" {
		t.Errorf("from the end: %+v", empty)
	}
	stale, err := c.Log(ctx, "s", "not-a-cursor", false)
	if err != nil || !stale.Reset || len(stale.Records) != 4 {
		t.Errorf("from a cursor that is none of the server's: reset %v, %d records (%v)", stale.Reset, len(stale.Records), err)
	}

	// A follow resumes from where the log read ended.
	more := mustAppend(t, st, "s", user("more"))
	evs := rawFollow(t, url+"/v1/sessions/s/follow", map[string]string{"Last-Event-ID": string(lg.Cursor)}, 1)
	if len(evs) != 1 || evs[0].name != "appended" || !strings.Contains(evs[0].data, more) {
		t.Errorf("a follow from the log's cursor: %+v, want the append after it", evs)
	}

	mem := dial(t, server(t, agentsession.NewMemoryStore(), remote.WithInsecureNoAuth()))
	if _, err := mem.Log(ctx, "s", "", false); !errors.Is(err, remote.ErrUnsupported) {
		t.Errorf("a log read over a memory store: %v, want unsupported", err)
	}
}

// A durable read serves only what is committed: the server commits the
// session whose writer it holds first, and one that cannot commit
// another process's working state serves what a commit covers. A
// commit says the cursor through which the log is committed.
func TestADurableReadServesOnlyWhatIsCommitted(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	lazy, err := cas.Open(root, cas.WithSync(cas.SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Close()
	url := server(t, lazy, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	first, err := c.Append(ctx, "s", user("one"))
	if err != nil {
		t.Fatal(err)
	}
	lg, err := c.Log(ctx, "s", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(lg.Records) != 1 || lg.Records[0].Durable {
		t.Fatalf("a lazy append read: %+v, want it not durable", lg.Records)
	}

	// Another process's view of the store, read-only: it cannot commit
	// the writer's working state, and serves what is committed: nothing.
	ro, err := cas.Open(root, cas.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	other := dial(t, server(t, ro, remote.WithInsecureNoAuth(), logged(t)))
	before, err := other.Log(ctx, "s", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.Records) != 0 {
		t.Errorf("a durable read by another process before a commit: %+v", before.Records)
	}

	committed, err := c.Log(ctx, "s", "", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(committed.Records) != 1 || !committed.Records[0].Durable || committed.Head != first {
		t.Errorf("the holder's durable read: %+v, head %s", committed.Records, committed.Head)
	}
	after, err := other.Log(ctx, "s", "", true)
	if err != nil || len(after.Records) != 1 || after.Records[0].Entry != first {
		t.Errorf("after the holder's durable read committed it: %+v (%v)", after.Records, err)
	}

	// A commit answers the cursor through which the log is committed.
	second, err := c.Append(ctx, "s", user("two"))
	if err != nil {
		t.Fatal(err)
	}
	token := leaseToken(t, url, "s", c)
	req, _ := http.NewRequest(http.MethodPost, url+"/v1/sessions/s/commit", nil)
	req.Header.Set("Lease-Token", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Durable bool   `json:"durable"`
		Cursor  string `json:"cursor"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !out.Durable || out.Cursor == "" {
		t.Fatalf("commit: status %d, %+v", resp.StatusCode, out)
	}
	end, err := other.Log(ctx, "s", agentsession.Cursor(out.Cursor), true)
	if err != nil || len(end.Records) != 0 {
		t.Errorf("a durable read after commit's cursor: %+v (%v), want nothing after it", end.Records, err)
	}
	all, err := other.Log(ctx, "s", "", true)
	if err != nil || len(all.Records) != 2 || all.Records[1].Entry != second {
		t.Errorf("everything through commit's cursor: %+v (%v)", all.Records, err)
	}
}

// leaseToken is the token of the lease the client holds on id, read by
// a renew through the client's own lease ID: tests that need to send a
// fenced request of their own.
func leaseToken(t *testing.T, url, id string, c *remote.Client) string {
	t.Helper()
	// A lease of the test's own: the client's is released first, so the
	// session is free to take.
	if err := c.Release(id); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(url+"/v1/sessions/"+id+"/lease", "application/json", strings.NewReader(`{"holder":"test","ttl_ms":60000}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var l struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil || l.Token == "" {
		t.Fatalf("lease: status %d (%v)", resp.StatusCode, err)
	}
	return l.Token
}

// A durable follow sends only committed records, each marked durable,
// from a snapshot of the committed view; the server commits what its
// writer accepted before it sends it.
func TestADurableFollowSendsOnlyCommittedRecords(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	root := t.TempDir()
	lazy, err := cas.Open(root, cas.WithSync(cas.SyncNever))
	if err != nil {
		t.Fatal(err)
	}
	defer lazy.Close()
	url := server(t, lazy, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	if _, err := c.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	first, err := c.Append(ctx, "s", user("one"))
	if err != nil {
		t.Fatal(err)
	}
	evs := rawFollow(t, url+"/v1/sessions/s/follow?durable=true", nil, 1)
	if len(evs) != 1 || evs[0].name != "snapshot" || !strings.Contains(evs[0].data, first) {
		t.Fatalf("a durable follow's first event: %+v", evs)
	}

	reader := dial(t, url)
	seen := make(chan agentsession.Change, 8)
	go func() {
		for ch, err := range reader.FollowCommitted(ctx, "s", "") {
			if err != nil {
				return
			}
			seen <- ch
		}
	}()
	if ch := <-seen; ch.Kind != agentsession.Snapshot || ch.Session.Leaf() != first {
		t.Fatalf("the first change: %v, leaf %s", ch.Kind, ch.Session.Leaf())
	}
	second, err := c.Append(ctx, "s", user("two"))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case ch := <-seen:
		if ch.Kind != agentsession.Appended || ch.ID != second {
			t.Errorf("the next change: %v %s, want the second append", ch.Kind, ch.ID)
		}
	case <-ctx.Done():
		t.Fatal("the durable follow never sent the second append")
	}
	// What it sent was committed: another process reads it as such.
	ro, err := cas.Open(root, cas.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	lg, err := ro.ReadLog(ctx, "s", "", true)
	if err != nil || len(lg.Records) != 2 {
		t.Errorf("committed after the durable follow sent it: %d records (%v)", len(lg.Records), err)
	}

	mem := server(t, agentsession.NewMemoryStore(), remote.WithInsecureNoAuth())
	if p := problemOf(t, get(t, mem+"/v1/sessions/s/follow?durable=true", nil)); p.Code != remote.CodeUnsupported {
		t.Errorf("a durable follow over a memory store: %s, want unsupported", p.Code)
	}
}

// A shallow path carries every envelope, and the contents the newest
// fold kept: the compaction's, every entry's from its first_kept on,
// and those above it that are not items. A fold without a config
// checkpoint is answered whole.
func TestAShallowPathCarriesWhatTheFoldKept(t *testing.T) {
	ctx := context.Background()
	st, s, ids := seeded(t, 3)
	label := mustAppend(t, st, s.ID(), agentsession.NewLabelEntry(ids[0], "start"))
	fold := mustAppend(t, st, s.ID(), &agentsession.CompactionEntry{FirstKept: ids[2], Summary: openresponses.UserText("summary"), Config: agentsession.Settings{Model: "m"}})
	after := mustAppend(t, st, s.ID(), user("after"))
	c := dial(t, server(t, st, remote.WithInsecureNoAuth()))

	p, err := c.Path(ctx, s.ID(), "", "since-fold")
	if err != nil {
		t.Fatal(err)
	}
	if p.Contents != "since-fold" || p.Head != after || len(p.Entries) != 6 {
		t.Fatalf("path: contents %q, head %s, %d entries", p.Contents, p.Head, len(p.Entries))
	}
	if lacks := p.Lacks(); !slices.Equal(lacks, []string{ids[0], ids[1]}) {
		t.Errorf("lacks %v, want the two items above first_kept", lacks)
	}
	for _, e := range p.Entries {
		switch e.ID {
		case label, fold, ids[2], after:
			if e.Entry == nil {
				t.Errorf("entry %s came without its content", e.ID)
			}
		}
	}
	whole, err := c.Path(ctx, s.ID(), "", "all")
	if err != nil || len(whole.Lacks()) != 0 || whole.Contents != "all" {
		t.Errorf("an all path: lacks %v, contents %q (%v)", whole.Lacks(), whole.Contents, err)
	}
	none, err := c.Path(ctx, s.ID(), "", "none")
	if err != nil || len(none.Lacks()) != 6 {
		t.Errorf("a none path: lacks %d (%v)", len(none.Lacks()), err)
	}

	// No checkpoint: the server cannot make it shallow, and says so.
	st2, s2, ids2 := seeded(t, 2)
	mustAppend(t, st2, s2.ID(), &agentsession.CompactionEntry{FirstKept: ids2[1], Summary: openresponses.UserText("summary")})
	c2 := dial(t, server(t, st2, remote.WithInsecureNoAuth()))
	p2, err := c2.Path(ctx, s2.ID(), "", "since-fold")
	if err != nil || p2.Contents != "all" || len(p2.Lacks()) != 0 {
		t.Errorf("a fold without a checkpoint: contents %q, lacks %v (%v)", p2.Contents, p2.Lacks(), err)
	}
}

// A followed ref sends its target on connect and again when the target
// or its state changes: a move, its session deleted (dangling), the ref
// set to none.
func TestFollowingARef(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	st := agentsession.NewMemoryStore()
	one, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateRef(ctx, "main", agentsession.RefTarget{}, agentsession.RefTarget{Session: one.ID()}, ""); err != nil {
		t.Fatal(err)
	}
	c := dial(t, server(t, st, remote.WithInsecureNoAuth(), remote.WithRefPoll(20*time.Millisecond)))
	states := make(chan remote.RefState, 8)
	go func() {
		for s, err := range c.FollowRef(ctx, "main") {
			if err != nil {
				return
			}
			states <- s
		}
	}()
	next := func(what string) remote.RefState {
		t.Helper()
		select {
		case s := <-states:
			return s
		case <-ctx.Done():
			t.Fatalf("%s: no event", what)
		}
		return remote.RefState{}
	}
	if s := next("connect"); s.Target.Session != one.ID() || s.Dangling {
		t.Errorf("on connect: %+v", s)
	}
	if err := st.UpdateRef(ctx, "main", agentsession.RefTarget{Session: one.ID()}, agentsession.RefTarget{Session: two.ID()}, ""); err != nil {
		t.Fatal(err)
	}
	if s := next("move"); s.Target.Session != two.ID() || s.Dangling {
		t.Errorf("after a move: %+v", s)
	}
	if err := st.Delete(ctx, two.ID()); err != nil {
		t.Fatal(err)
	}
	if s := next("delete"); s.Target.Session != two.ID() || !s.Dangling {
		t.Errorf("after its session went: %+v", s)
	}

	for _, err := range c.FollowRef(ctx, "absent") {
		if !errors.Is(err, agentsession.ErrNoRef) {
			t.Errorf("following a ref that is not there: %v, want no_ref", err)
		}
		break
	}
}

// Past the snapshot bound, a server that serves objects sends the rest
// of the records without their lines, which the client reads as
// objects; one that serves none refuses the snapshot.
func TestASnapshotPastTheBound(t *testing.T) {
	ctx := context.Background()
	st := casStore(t)
	mustCreate(t, st, agentsession.Header{ID: "s"})
	var ids []string
	for i := range 4 {
		ids = append(ids, mustAppend(t, st, "s", user(strings.Repeat("x", 200)+string(rune('a'+i)))))
	}
	url := server(t, st, remote.WithInsecureNoAuth(), remote.WithSnapshotLimit(600), logged(t))
	evs := rawFollow(t, url+"/v1/sessions/s/follow", nil, 1)
	if len(evs) != 1 || evs[0].name != "snapshot" {
		t.Fatalf("first event: %+v", evs)
	}
	var d struct {
		Records []struct {
			Seq   int             `json:"seq"`
			Entry string          `json:"entry"`
			Line  json.RawMessage `json:"line"`
		} `json:"records"`
	}
	if err := json.Unmarshal([]byte(evs[0].data), &d); err != nil {
		t.Fatal(err)
	}
	inline := 0
	for _, r := range d.Records {
		if len(r.Line) > 0 {
			inline++
		}
	}
	if len(d.Records) != 4 || inline == 0 || inline == 4 {
		t.Errorf("a snapshot past the bound: %d records, %d inline, want some but not all", len(d.Records), inline)
	}
	s, err := dial(t, url).Read(ctx, "s")
	if err != nil {
		t.Fatal(err)
	}
	if s.Len() != 4 || s.Leaf() != ids[3] {
		t.Errorf("the client's read past the bound: %d entries, leaf %s", s.Len(), s.Leaf())
	}

	mem, ms, _ := seeded(t, 4)
	small := dial(t, server(t, mem, remote.WithInsecureNoAuth(), remote.WithSnapshotLimit(100)))
	if _, err := small.Read(ctx, ms.ID()); !errors.Is(err, remote.ErrTooLarge) {
		t.Errorf("a read past the bound of a server without objects: %v, want too_large", err)
	}
}

// A session handed to the server can be taken back: the client fetches
// what the server's writers added, the server hands the record over and
// is a mirror, and the local store is the record again and writes on.
func TestTakingASessionBack(t *testing.T) {
	ctx := context.Background()
	local, served := casStore(t), casStore(t)
	url := server(t, served, remote.WithInsecureNoAuth(), logged(t))
	c := dial(t, url)
	defer c.Close()
	mustCreate(t, local, agentsession.Header{ID: "s"})
	mustAppend(t, local, "s", user("local one"))
	if _, err := remote.Push(ctx, local, c, "s", cas.PushOptions{Handover: true}); err != nil {
		t.Fatal(err)
	}
	if m, _ := local.Mark(ctx, "s"); m != cas.MarkMirror {
		t.Fatalf("after the handover push the local mark is %s", m)
	}
	if _, err := c.Open(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	there, err := c.Append(ctx, "s", user("written at the server"))
	if err != nil {
		t.Fatal(err)
	}

	x, err := remote.TakeBack(ctx, local, c, "s")
	if err != nil {
		t.Fatal(err)
	}
	if !x.Handover || x.Admitted != 1 || x.Head != there {
		t.Errorf("take back: %+v", x)
	}
	if m, _ := local.Mark(ctx, "s"); m != cas.MarkRecord {
		t.Errorf("after taking it back the local mark is %s", m)
	}
	if info, err := c.Session(ctx, "s"); err != nil || info.Mark != cas.MarkMirror {
		t.Errorf("after taking it back the server's mark is %q (%v)", info.Mark, err)
	}
	next := mustAppend(t, local, "s", user("local again"))
	s, err := local.Read(ctx, "s")
	if err != nil || s.Leaf() != next || s.Len() != 3 {
		t.Errorf("the local record: leaf %s, %d entries (%v)", s.Leaf(), s.Len(), err)
	}
	if _, err := c.Append(ctx, "s", user("too late")); !errors.Is(err, remote.ErrMirror) && !errors.Is(err, remote.ErrLeaseLost) {
		t.Errorf("an append at the server after it was taken back: %v, want mirror", err)
	}
}
