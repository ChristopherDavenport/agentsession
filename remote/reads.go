package remote

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// logReader is a store that reads a session's log record by record, a
// cursor after each, and reads only what is committed when asked: of
// the reference stores, cas.
type logReader interface {
	ReadLog(ctx context.Context, id string, after agentsession.Cursor, durable bool) (cas.Log, error)
	LogCursor(ctx context.Context, id string) (agentsession.Cursor, error)
}

// logLine is one record of a log read: an append, with the entry, or a
// head move, without. The first line, or the end frame of an empty
// read, also carries the session's header, incarnation and mark, and
// reset when the cursor asked for named no place in the log.
type logLine struct {
	Seq     int     `json:"seq"`
	Entry   string  `json:"entry,omitempty"`
	Head    *string `json:"head"`
	Outcome string  `json:"outcome,omitempty"`
	Durable bool    `json:"durable"`
	Cursor  string  `json:"cursor"`

	Header      json.RawMessage `json:"header,omitempty"`
	Incarnation string          `json:"incarnation,omitempty"`
	Mark        string          `json:"mark,omitempty"`
	Reset       bool            `json:"reset,omitempty"`
}

// wireCursor is a store cursor as the wire carries it, the event ID of
// the first event at it.
func wireCursor(c agentsession.Cursor) string { return eventID(c, 0) }

// storeCursor reads a cursor a client sent, an event ID or a log line's,
// as the store's. One that is no cursor of this server's names no place
// in the log, which the store answers with a reset.
func storeCursor(s string) agentsession.Cursor {
	if s == "" {
		return ""
	}
	c, _, err := cursorOf(s)
	if err != nil {
		return agentsession.Cursor("\x00" + s)
	}
	return c
}

func (h *Handler) log(w http.ResponseWriter, r *http.Request) {
	if h.lr == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no log of records to read", ""))
		return
	}
	durable := false
	switch r.URL.Query().Get("durable") {
	case "", "false":
	case "true":
		durable = true
	default:
		h.writeProblem(w, problem(CodeBadRequest, "durable is true or false", ""))
		return
	}
	id := r.PathValue("id")
	lg, err := h.lr.ReadLog(r.Context(), id, storeCursor(r.URL.Query().Get("after")), durable)
	if err != nil {
		h.fail(w, err)
		return
	}
	inc := incarnation(lg.Header)
	if err := checkIncarnation(r, inc); err != nil {
		h.fail(w, err)
		return
	}
	header, err := json.Marshal(lg.Header)
	if err != nil {
		h.fail(w, err)
		return
	}
	mark := ""
	if h.ex != nil {
		mark = lg.Mark
	}
	w.Header().Set("Incarnation", inc)
	out := startNDJSON(w)
	lines := logLines(lg)
	for i, l := range lines {
		if i == 0 {
			l.Header, l.Incarnation, l.Mark, l.Reset = header, inc, mark, lg.Reset
		}
		if err := out.line(l); err != nil {
			out.fail(h, err)
			return
		}
	}
	end := map[string]any{"cursor": wireCursor(lg.Cursor), "head": headOut(lg.Session.Leaf())}
	if len(lines) == 0 {
		end["header"], end["incarnation"] = json.RawMessage(header), inc
		if mark != "" {
			end["mark"] = mark
		}
		if lg.Reset {
			end["reset"] = true
		}
	}
	out.end(end)
}

// logLines are a log read's records as the wire carries them. A head
// move's seq is the number of entries before it. Records that share a
// cursor, a log line holding more than one, are numbered as follow
// events at one cursor are.
func logLines(lg cas.Log) []logLine {
	out := make([]logLine, 0, len(lg.Records))
	// The entries before the first record: the session at the read's end
	// less the entries the read appended.
	seq := len(lg.Session.Entries())
	for _, rec := range lg.Records {
		if rec.Entry != nil {
			seq--
		}
	}
	var last agentsession.Cursor
	n := 0
	for _, rec := range lg.Records {
		if rec.Cursor == last {
			n++
		} else {
			n = 0
		}
		last = rec.Cursor
		l := logLine{Head: headOut(rec.Head), Durable: rec.Durable, Cursor: eventID(rec.Cursor, n)}
		if rec.Entry != nil {
			id := rec.Entry.Base().ID
			seq = rec.Seq
			l.Entry, l.Outcome = id, outcomeOf(rec.Entry, id, rec.Head)
		}
		l.Seq = seq
		out = append(out, l)
	}
	return out
}

// followDurable serves a follow of only what is committed: RFC 0003's
// durable=true. Each read commits the session where the server holds
// it, then sends what the committed view gained; the session's shared
// follower says when to look, and a slower poll catches a commit made
// elsewhere.
func (h *Handler) followDurable(w http.ResponseWriter, r *http.Request, flusher http.Flusher, resume agentsession.Cursor) {
	if h.lr == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store has no committed view", ""))
		return
	}
	id := r.PathValue("id")
	ctx := r.Context()
	lg, err := h.lr.ReadLog(ctx, id, resume, true)
	if err != nil {
		h.fail(w, err)
		return
	}
	inc := incarnation(lg.Header)
	if err := checkIncarnation(r, inc); err != nil {
		h.fail(w, err)
		return
	}
	var first *event
	if resume == "" || lg.Reset {
		d, err := snapshotOf(lg.Session)
		if err != nil {
			h.fail(w, err)
			return
		}
		ev, cut, err := h.snapshotEvent(d, resume != "")
		if err != nil {
			h.fail(w, err)
			return
		}
		if cut != nil {
			h.writeProblem(w, cut)
			return
		}
		ev.id = wireCursor(lg.Cursor)
		first = ev
	}
	hb := h.acquire(id)
	defer h.release(hb)
	sse := startSSE(w, flusher, inc)
	if first != nil {
		sse.send(first)
	} else if !h.sendDurable(sse, lg) {
		return
	}
	sse.flush()
	cur := lg.Cursor
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		changed, done := hb.changes()
		if done {
			changed = nil // the shared follower ended: the poll alone wakes
		}
		select {
		case <-changed:
		case <-poll.C:
		case <-tick.C:
			sse.comment()
			continue
		case <-ctx.Done():
			return
		}
		lg, err := h.lr.ReadLog(ctx, id, cur, true)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p := problemOf(err)
			if p.Code == CodeStoreFault {
				h.logError(err)
			}
			sse.send(errorEvent(p))
			sse.flush()
			return
		}
		if lg.Reset {
			d, err := snapshotOf(lg.Session)
			if err != nil {
				h.logError(err)
				sse.send(errorEvent(problem(CodeStoreFault, "the follow could not be served", "")))
				sse.flush()
				return
			}
			ev, cut, err := h.snapshotEvent(d, true)
			if err == nil && cut != nil {
				sse.send(errorEvent(cut))
				sse.flush()
				return
			}
			if err != nil {
				h.logError(err)
				sse.send(errorEvent(problem(CodeStoreFault, "the follow could not be served", "")))
				sse.flush()
				return
			}
			ev.id = wireCursor(lg.Cursor)
			sse.send(ev)
		} else if !h.sendDurable(sse, lg) {
			return
		}
		sse.flush()
		cur = lg.Cursor
	}
}

// sendDurable sends a durable read's records as follow events, each
// marked durable.
func (h *Handler) sendDurable(sse *sse, lg cas.Log) bool {
	yes := true
	for i, l := range logLines(lg) {
		var v any
		name := eventHead
		if rec := lg.Records[i]; rec.Entry != nil {
			line, err := agentsession.MarshalEntry(rec.Entry)
			if err == nil {
				line, err = jcs.Transform(line)
			}
			if err != nil {
				h.logError(err)
				sse.send(errorEvent(problem(CodeStoreFault, "the follow could not be served", "")))
				sse.flush()
				return false
			}
			name = eventAppended
			v = &appendedData{Seq: l.Seq, Entry: l.Entry, Line: line, Outcome: l.Outcome, Head: l.Head, Durable: &yes}
		} else {
			v = &headData{Head: l.Head, Durable: &yes}
		}
		data, err := json.Marshal(v)
		if err != nil {
			h.logError(err)
			return false
		}
		sse.send(&event{name: name, id: l.Cursor, data: data})
	}
	return true
}

// snapshotEvent is the snapshot or reset event d makes for a client,
// bounded by the server's snapshot limit: past it, a server that serves
// objects sends the rest of the records without their lines, and one
// that serves none refuses it, cut being the problem.
func (h *Handler) snapshotEvent(d *snapshotData, reset bool) (ev *event, cut *Problem, err error) {
	send, cut := h.bound(d)
	if cut != nil {
		return nil, cut, nil
	}
	data, err := json.Marshal(send)
	if err != nil {
		return nil, nil, err
	}
	ev = &event{name: eventSnapshot, data: data, v: d}
	if reset {
		ev.name = eventReset
	}
	return ev, nil, nil
}

// bound returns d as a client is sent it under the snapshot limit, or
// the problem that refuses it.
func (h *Handler) bound(d *snapshotData) (*snapshotData, *Problem) {
	var total int64
	for _, r := range d.Records {
		total += int64(len(r.Line))
	}
	if total <= h.snapshotLimit {
		return d, nil
	}
	if h.ex == nil {
		return nil, problem(CodeTooLarge, "the session is past what this server sends in a snapshot", "the store serves no objects to read the rest by")
	}
	out := *d
	out.Records = make([]record, len(d.Records))
	var sent int64
	for i, r := range d.Records {
		out.Records[i] = record{Seq: r.Seq, Entry: r.Entry}
		if sent+int64(len(r.Line)) <= h.snapshotLimit {
			out.Records[i].Line = r.Line
			sent += int64(len(r.Line))
		} else {
			// Past the bound every later record goes without its line,
			// so a client reads one run of objects.
			sent = h.snapshotLimit + 1
		}
	}
	return &out, nil
}

// refState is a followed ref's target and whether its session is there.
type refState struct {
	Name   string  `json:"name"`
	Target *Target `json:"target"`
	// State is "live" or "dangling" for a ref with a target.
	State string `json:"state,omitempty"`
}

// followRef serves a ref's target as server-sent events: the current
// one on connect, and again each time the target or its state changes.
// The reference stores have no watch on a ref, so it is resolved again
// every refPoll.
func (h *Handler) followRef(w http.ResponseWriter, r *http.Request, name string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		h.writeProblem(w, problem(CodeStoreFault, "the connection cannot stream", ""))
		return
	}
	ctx := r.Context()
	resolve := func() (refState, error) {
		t, err := h.refs.ResolveRef(ctx, name)
		if (err == nil || !t.IsZero()) && !h.mayRead(ctx, t) {
			// A session the follower may not read: the ref is none of
			// its, so a move there reads as a move to none.
			return refState{Name: name}, noRef()
		}
		switch {
		case err == nil:
			return refState{Name: name, Target: targetOut(t), State: "live"}, nil
		case !t.IsZero() && errors.Is(err, agentsession.ErrNoSession):
			return refState{Name: name, Target: targetOut(t), State: "dangling"}, nil
		case errors.Is(err, agentsession.ErrNoRef):
			return refState{Name: name}, err
		}
		return refState{}, err
	}
	cur, err := resolve()
	if err != nil {
		// A ref that is not there to follow is refused as a resolve is.
		h.fail(w, err)
		return
	}
	sse := startSSE(w, flusher, "")
	send := func(s refState) bool {
		data, err := json.Marshal(s)
		if err != nil {
			h.logError(err)
			return false
		}
		sse.send(&event{name: eventTarget, data: data})
		sse.flush()
		return true
	}
	if !send(cur) {
		return
	}
	poll := time.NewTicker(h.refPoll)
	defer poll.Stop()
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case <-poll.C:
		case <-tick.C:
			sse.comment()
			continue
		case <-ctx.Done():
			return
		}
		next, err := resolve()
		if err != nil && !errors.Is(err, agentsession.ErrNoRef) {
			if ctx.Err() != nil {
				return
			}
			p := problemOf(err)
			if p.Code == CodeStoreFault {
				h.logError(err)
			}
			sse.send(errorEvent(p))
			sse.flush()
			return
		}
		if sameRefState(cur, next) {
			continue
		}
		if !send(next) {
			return
		}
		cur = next
	}
}

func sameRefState(a, b refState) bool {
	if a.State != b.State || (a.Target == nil) != (b.Target == nil) {
		return false
	}
	return a.Target == nil || *a.Target == *b.Target
}

// sinceFold is RFC 0003's shallow path: which entries of path, root
// first, keep their content. Contents come for the newest compaction on
// the path, for every entry from the one its first_kept names onward,
// and for every entry above first_kept that is not an item or a branch
// summary. A path with no compaction folds nothing, so every content
// comes. A compaction whose content carries no config checkpoint, or
// whose first_kept is not on the path, is answered as `all`, which
// served says.
func sinceFold(path []agentsession.Entry) (keep func(int) bool, served string) {
	all := func(int) bool { return true }
	fold := -1
	for i := len(path) - 1; i >= 0; i-- {
		if _, ok := path[i].(*agentsession.CompactionEntry); ok {
			fold = i
			break
		}
	}
	if fold < 0 {
		return all, "since-fold"
	}
	c := path[fold].(*agentsession.CompactionEntry)
	if !hasConfigCheckpoint(c) {
		return all, "all"
	}
	kept := -1
	for i := 0; i <= fold; i++ {
		if path[i].Base().ID == c.FirstKept {
			kept = i
			break
		}
	}
	if kept < 0 {
		return all, "all"
	}
	return func(i int) bool {
		if i >= kept {
			return true
		}
		switch path[i].(type) {
		case *agentsession.ItemEntry, *agentsession.BranchSummaryEntry:
			return false
		}
		return true
	}, "since-fold"
}

// hasConfigCheckpoint reports whether a compaction carries RFC 0001's
// config checkpoint. A compaction read without one has empty settings,
// as one with an empty checkpoint does; both are answered as `all`,
// which is never wrong, only less shallow.
func hasConfigCheckpoint(c *agentsession.CompactionEntry) bool {
	data, err := json.Marshal(c.Config)
	return err == nil && string(data) != "{}"
}
