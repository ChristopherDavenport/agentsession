// Package wake carries the two pieces of waiting a follower shares
// across stores: a per-session broadcast a writer in the same process
// rings, and the interval a follower polls at when nothing rings.
package wake

import (
	"sync"
	"time"
)

// Hub is a broadcast per followed session: a channel closed and
// replaced each time the session changes. A follower takes the channel
// before it reads, so a change that lands between the read and the wait
// still wakes it. A session nobody follows costs nothing: Notify finds
// no entry and returns.
type Hub struct {
	mu sync.Mutex
	m  map[string]*entry
}

type entry struct {
	ch   chan struct{}
	refs int
}

// Watch registers a follower of id. Chan returns the channel to wait on
// and done releases the registration.
func (h *Hub) Watch(id string) (c func() <-chan struct{}, done func()) {
	h.mu.Lock()
	if h.m == nil {
		h.m = map[string]*entry{}
	}
	e := h.m[id]
	if e == nil {
		e = &entry{ch: make(chan struct{})}
		h.m[id] = e
	}
	e.refs++
	h.mu.Unlock()
	c = func() <-chan struct{} {
		h.mu.Lock()
		defer h.mu.Unlock()
		return e.ch
	}
	var once sync.Once
	done = func() {
		once.Do(func() {
			h.mu.Lock()
			defer h.mu.Unlock()
			if e.refs--; e.refs == 0 && h.m[id] == e {
				delete(h.m, id)
			}
		})
	}
	return c, done
}

// Notify wakes every follower of id.
func (h *Hub) Notify(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if e := h.m[id]; e != nil {
		close(e.ch)
		e.ch = make(chan struct{})
	}
}

// Poll is a follower's poll interval: Min while changes keep coming,
// backing off by doubling to Max while nothing does.
type Poll struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns how long to wait before looking again, and lengthens the
// next wait.
func (p *Poll) Next() time.Duration {
	if p.cur < p.Min {
		p.cur = p.Min
	}
	d := p.cur
	if p.cur *= 2; p.cur > p.Max {
		p.cur = p.Max
	}
	return d
}

// Reset starts the backoff over, after a change.
func (p *Poll) Reset() { p.cur = 0 }

// Wait blocks until c is closed, d has passed or done is closed. It
// reports false when done was closed.
func Wait(done <-chan struct{}, c <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-done:
		return false
	case <-c:
	case <-t.C:
	}
	return true
}
