package remote

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// event is one follow event, encoded once and sent to every client.
type event struct {
	name string
	id   string
	data []byte
	// v is the decoded payload, which the hub applies to its replica:
	// *snapshotData, *appendedData or *headData.
	v any
	// cut is set on a snapshot or reset a client cannot be sent, past the
	// snapshot bound of a server that serves no objects: the client's
	// follow ends with it.
	cut *Problem
}

// encoder turns a store's follow into events. It drops the Head change
// the library yields after an Appended at the same cursor, since the
// appended event carries the head after it, and numbers the events at
// one cursor.
type encoder struct {
	// seq is the number of records the log holds, -1 until a change
	// says.
	seq        int
	lastAppend agentsession.Cursor
	appended   bool
	last       agentsession.Cursor
	n          int
	started    bool
	// bound is what a client is sent of a snapshot or reset, under the
	// server's snapshot limit; nil sends it whole.
	bound func(*snapshotData) (*snapshotData, *Problem)
}

func newEncoder() *encoder { return &encoder{seq: -1} }

// encode returns the event for c, nil for a change that adds nothing to
// what the client was sent. c.Session is read here, before the follow's
// next step extends it.
func (e *encoder) encode(c agentsession.Change) (*event, error) {
	ev := &event{}
	switch c.Kind {
	case agentsession.Snapshot, agentsession.Reset:
		d, err := snapshotOf(c.Session)
		if err != nil {
			return nil, err
		}
		e.seq = len(d.Records)
		ev.name, ev.v = eventSnapshot, d
		if c.Kind == agentsession.Reset {
			ev.name = eventReset
		}
		e.appended = false
	case agentsession.Appended:
		line, err := agentsession.MarshalEntry(c.Entry)
		if err == nil {
			line, err = jcs.Transform(line)
		}
		if err != nil {
			return nil, err
		}
		if e.seq < 0 {
			// A resumed follow starts past its cursor, with the session
			// up to it, this entry included.
			e.seq = len(c.Session.Entries()) - 1
		}
		e.seq++
		head := c.Session.Leaf()
		ev.name, ev.v = eventAppended, &appendedData{
			Seq: e.seq, Entry: c.ID, Line: line, Outcome: outcomeOf(c.Entry, c.ID, head), Head: headOut(head),
		}
		e.lastAppend, e.appended = c.Cursor, true
	case agentsession.Head:
		if e.appended && c.Cursor == e.lastAppend {
			return nil, nil
		}
		ev.name, ev.v = eventHead, &headData{Head: headOut(c.Leaf)}
		e.appended = false
	default:
		return nil, nil
	}
	send := ev.v
	if d, ok := ev.v.(*snapshotData); ok && e.bound != nil {
		var cut *Problem
		if send, cut = e.bound(d); cut != nil {
			ev.cut, send = cut, nil
		}
	}
	if send != nil {
		data, err := json.Marshal(send)
		if err != nil {
			return nil, err
		}
		ev.data = data
	}
	if e.started && c.Cursor == e.last {
		e.n++
	} else {
		e.n = 0
	}
	e.started, e.last = true, c.Cursor
	ev.id = eventID(c.Cursor, e.n)
	return ev, nil
}

// outcomeOf names what an append did, from the head after it: a leaf
// label moved the head to its target or did not, and any other entry
// continued the head or was added beside it.
func outcomeOf(e agentsession.Entry, id, head string) string {
	if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
		if head == l.Target {
			return "leaf_moved"
		}
		return "leaf_not_moved"
	}
	if head == id {
		return "continued"
	}
	return "branched"
}

// errorEvent is the event that ends a follow with p.
func errorEvent(p *Problem) *event {
	data, _ := json.Marshal(p)
	return &event{name: eventError, data: data}
}

// hub is the one follower of a session every client following it
// shares. It keeps a replica of the session built from the events it
// sends, which is what a client starting from nothing is sent as its
// snapshot, and the recent events, which a client resuming from one of
// them is sent again.
type hub struct {
	h      *Handler
	id     string
	cancel context.CancelFunc
	ready  chan struct{}
	// refs counts the clients and reads using the hub; it is the
	// Handler's to change, under its lock.
	refs int

	mu   sync.Mutex
	rep  *replica
	inc  string
	last string
	ring []*event
	subs map[*sub]struct{}
	// end is the problem the follow ended with; done says it ended.
	end  *Problem
	done bool
	// changed is closed, and replaced, at each event and at the end, for
	// a caller waiting for the replica to catch up.
	changed chan struct{}
}

// sub is one client's place in a hub.
type sub struct {
	ch chan *event
	// err is the problem that ended the client's stream, set before ch
	// is closed.
	err *Problem
}

// run follows the session and fans its events out until the follow
// ends or the hub is stopped.
func (hb *hub) run(ctx context.Context) {
	enc := newEncoder()
	enc.bound = hb.h.bound
	for c, err := range hb.h.follower.Follow(ctx, hb.id, "") {
		if err != nil {
			hb.finish(problemOf(err))
			return
		}
		ev, err := enc.encode(c)
		if err != nil {
			hb.finish(problemOf(err))
			return
		}
		if ev == nil {
			continue
		}
		if err := hb.publish(ev); err != nil {
			hb.h.logError(err)
			hb.finish(problem(CodeStoreFault, "the follow could not be served", ""))
			return
		}
	}
	hb.finish(nil)
}

// publish applies ev to the replica and sends it to every client. A
// client whose buffer is full is cut with `overflow`.
func (hb *hub) publish(ev *event) error {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	switch d := ev.v.(type) {
	case *snapshotData:
		r, err := newReplica(d)
		if err != nil {
			return err
		}
		hb.rep, hb.inc = r, d.Incarnation
	case *appendedData:
		if hb.rep == nil {
			return errors.New("remote: an append before the snapshot")
		}
		if _, _, err := hb.rep.appended(d); err != nil {
			return err
		}
	case *headData:
		if hb.rep == nil {
			return errors.New("remote: a head before the snapshot")
		}
		if err := hb.rep.place(headIn(d.Head)); err != nil {
			return err
		}
	}
	hb.last = ev.id
	hb.ring = append(hb.ring, ev)
	if n := len(hb.ring) - hb.h.replay; n > 0 {
		clear(hb.ring[:n])
		hb.ring = hb.ring[n:]
	}
	for s := range hb.subs {
		if ev.cut != nil {
			// A reset past the bound the client cannot be sent: its
			// follow ends, and it reads the session another way.
			s.err = ev.cut
			close(s.ch)
			delete(hb.subs, s)
			continue
		}
		select {
		case s.ch <- ev:
		default:
			s.err = problem(CodeOverflow, "the follow fell behind", "resume from the last event read")
			close(s.ch)
			delete(hb.subs, s)
		}
	}
	select {
	case <-hb.ready:
	default:
		close(hb.ready)
	}
	close(hb.changed)
	hb.changed = make(chan struct{})
	return nil
}

// finish ends the hub: every client is sent p, or, for nil, its stream
// ends.
func (hb *hub) finish(p *Problem) {
	hb.mu.Lock()
	hb.done, hb.end = true, p
	for s := range hb.subs {
		s.err = p
		close(s.ch)
		delete(hb.subs, s)
	}
	select {
	case <-hb.ready:
	default:
		close(hb.ready)
	}
	close(hb.changed)
	hb.changed = make(chan struct{})
	hb.mu.Unlock()
	hb.h.forget(hb)
}

// wait waits for the hub's first event, or its end.
func (hb *hub) wait(ctx context.Context) error {
	select {
	case <-hb.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// catchUp waits until the replica holds every entry of held, the session
// as the store held it when a caller asked. The follower runs behind the
// store: it learns of an append when the store wakes it, or when it next
// looks, while a reader is owed what the store holds when it reads. The
// log is in append order and the replica takes it in that order, so
// holding held's newest entry is holding them all.
func (hb *hub) catchUp(ctx context.Context, held *agentsession.Session) error {
	entries := held.Entries()
	if len(entries) == 0 {
		return nil
	}
	newest := entries[len(entries)-1].Base().ID
	for {
		hb.mu.Lock()
		if hb.done {
			end := hb.end
			hb.mu.Unlock()
			if end != nil {
				return end
			}
			return problem(CodeStoreFault, "the follow ended", "")
		}
		if hb.rep != nil {
			if _, ok := hb.rep.sess.Entry(newest); ok {
				hb.mu.Unlock()
				return nil
			}
		}
		ch := hb.changed
		hb.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// view runs fn on the replica, or returns the problem the follow ended
// with before it had one.
func (hb *hub) view(fn func(r *replica, inc, last string) error) error {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.rep == nil || hb.done && hb.end != nil {
		if hb.end != nil {
			return hb.end
		}
		return problem(CodeStoreFault, "the follow ended", "")
	}
	return fn(hb.rep, hb.inc, hb.last)
}

// attach registers a client. With from empty it is sent a snapshot of
// the replica first; with an event ID the hub still holds it is sent the
// events after it. ok is false for an ID the hub does not hold, which
// the client is served from a follower of its own.
func (hb *hub) attach(from string) (s *sub, first []*event, ok bool, err error) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if hb.done || hb.rep == nil {
		if hb.end != nil {
			return nil, nil, false, hb.end
		}
		return nil, nil, false, problem(CodeStoreFault, "the follow ended", "")
	}
	if from == "" {
		d, err := snapshotOf(hb.rep.sess)
		if err != nil {
			return nil, nil, false, err
		}
		ev, cut, err := hb.h.snapshotEvent(d, false)
		if err != nil {
			return nil, nil, false, err
		}
		if cut != nil {
			return nil, nil, false, cut
		}
		ev.id, ev.v = hb.last, nil
		first = []*event{ev}
	} else {
		i := -1
		for j, ev := range hb.ring {
			if ev.id == from {
				i = j
			}
		}
		if i < 0 {
			return nil, nil, false, nil
		}
		for _, ev := range hb.ring[i+1:] {
			if ev.cut != nil {
				// A reset after the cursor that the client cannot be
				// sent: its follow ends as a live one's did.
				return nil, nil, false, ev.cut
			}
		}
		first = append(first, hb.ring[i+1:]...)
	}
	s = &sub{ch: make(chan *event, hb.h.buffer)}
	hb.subs[s] = struct{}{}
	return s, first, true, nil
}

// changes returns the channel the hub closes at its next event, and
// done when the hub's follow has ended and no event will come.
func (hb *hub) changes() (ch <-chan struct{}, done bool) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	return hb.changed, hb.done
}

// detach removes a client that went away.
func (hb *hub) detach(s *sub) {
	hb.mu.Lock()
	defer hb.mu.Unlock()
	if _, ok := hb.subs[s]; ok {
		delete(hb.subs, s)
		close(s.ch)
	}
}

// acquire returns the session's hub, starting one when none runs, and
// counts the caller as using it until release.
func (h *Handler) acquire(id string) *hub {
	h.mu.Lock()
	defer h.mu.Unlock()
	hb := h.hubs[id]
	if hb == nil {
		ctx, cancel := context.WithCancel(h.ctx)
		hb = &hub{h: h, id: id, cancel: cancel, ready: make(chan struct{}), subs: map[*sub]struct{}{}, changed: make(chan struct{})}
		h.hubs[id] = hb
		go hb.run(ctx)
	}
	hb.refs++
	return hb
}

// release stops counting a caller. A hub nobody uses is stopped after
// the linger, so reads close together share one load.
func (h *Handler) release(hb *hub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	hb.refs--
	if hb.refs > 0 {
		return
	}
	time.AfterFunc(h.linger, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if hb.refs == 0 && h.hubs[hb.id] == hb {
			delete(h.hubs, hb.id)
			hb.cancel()
		}
	})
}

// forget removes a hub whose follow ended, so the next client starts
// another.
func (h *Handler) forget(hb *hub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hubs[hb.id] == hb {
		delete(h.hubs, hb.id)
	}
	hb.cancel()
}
