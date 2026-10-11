package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// held is one session ID's write state on the server: its lease, the
// last fencing token issued under the ID, and the store's session while
// the lease holds it. Its lock is RFC 0003's per-session lock: acquire,
// renew, release, expiry and every write under a lease take it, so a
// write that passed the token check lands before any newer lease is
// granted.
type held struct {
	mu    sync.Mutex
	id    string
	lease *lease
	// token is the last token issued under the ID. It is kept when the
	// lease ends and when the session is deleted, so a token never
	// repeats under an ID while the handler runs.
	token uint64
	// sess is the store's session, shared with the store, while a lease
	// holds it: the session the writer's appends go to.
	sess *agentsession.Session
}

// lease is a granted lease.
type lease struct {
	id      string
	token   uint64
	holder  string
	expires time.Time
	timer   *time.Timer
}

// current reports whether l is unexpired, on the server's monotonic
// clock.
func (l *lease) current() bool { return l != nil && time.Now().Before(l.expires) }

// state returns the write state of the session ID, made on first use.
// It is never removed: its token outlives the lease and the session.
func (h *Handler) state(id string) *held {
	h.wmu.Lock()
	defer h.wmu.Unlock()
	st, ok := h.held[id]
	if !ok {
		st = &held{id: id}
		h.held[id] = st
	}
	return st
}

// nextToken issues the next fencing token under st's ID: greater than
// every token issued under it before. A token is the wall clock's
// nanoseconds, or one more than the last token when the clock has not
// moved past it, so tokens keep increasing across a restart of the
// server as long as the clock does not step back past the last token
// issued, which the reference stores, keeping no lease state of their
// own, cannot promise otherwise. The caller holds st.mu.
func (h *Handler) nextToken(st *held) uint64 {
	t := uint64(time.Now().UnixNano())
	if t <= st.token {
		t = st.token + 1
	}
	st.token = t
	return t
}

// ttlOf bounds a requested time to live: the handler's default for
// none, its maximum for more.
func (h *Handler) ttlOf(ms int64) time.Duration {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d <= 0:
		return h.leaseTTL
	case d > h.maxLeaseTTL:
		return h.maxLeaseTTL
	}
	return d
}

// grant gives st a lease on sess, the store's session the caller has
// just opened or created and so holds. The caller holds st.mu.
func (h *Handler) grant(st *held, sess *agentsession.Session, holder string, ttl time.Duration) (*lease, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	l := &lease{id: hex.EncodeToString(b[:]), token: h.nextToken(st), holder: holder, expires: time.Now().Add(ttl)}
	l.timer = time.AfterFunc(ttl, func() { h.expire(st, l) })
	st.lease, st.sess = l, sess
	return l, nil
}

// expire ends l when its time is up and nobody renewed it.
func (h *Handler) expire(st *held, l *lease) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease == l && !l.current() {
		h.end(st)
	}
}

// end ends st's lease and lets the store's hold on the session go, so
// another process, or another lease, can take it. The caller holds
// st.mu.
func (h *Handler) end(st *held) {
	if st.lease != nil {
		st.lease.timer.Stop()
	}
	st.lease, st.sess = nil, nil
	h.releaseHold(st.id)
}

// releaseHold lets the store's hold on a session go, for a store that
// keeps one.
func (h *Handler) releaseHold(id string) {
	if r, ok := h.store.(interface{ Release(string) error }); ok {
		if err := r.Release(id); err != nil {
			h.logError(err)
		}
	}
}

// take takes a lease on the session for holder: the store's hold
// first, so a session another process holds, or one this server has
// leased, is refused. The caller holds st.mu.
func (h *Handler) take(ctx context.Context, st *held, holder string, ttl time.Duration) (*lease, error) {
	if st.lease.current() {
		p := problem(CodeSessionLocked, "the session is leased", "")
		p.Holder = st.lease.holder
		return nil, p
	}
	if st.lease != nil {
		// Expired and not yet ended: a takeover. The reference stores
		// keep no working state beyond what an append made durable by
		// their policy, so there is nothing to commit first.
		h.end(st)
	}
	if h.ex != nil {
		// A mirror refuses local writes and takes only exchange, so it
		// cannot be leased.
		if m, _, err := h.ex.MarkOf(ctx, st.id); err == nil && m == cas.MarkMirror {
			return nil, problem(CodeMirror, "a mirror cannot be leased", "")
		}
	}
	sess, err := h.store.Open(ctx, st.id)
	if err != nil {
		return nil, err
	}
	return h.grant(st, sess, holder, ttl)
}

// fenced checks the request's Lease-Token is the session's current,
// unexpired lease. The caller holds st.mu.
func fenced(r interface{ Get(string) string }, st *held) error {
	t, err := strconv.ParseUint(r.Get("Lease-Token"), 10, 64)
	if err != nil || !st.lease.current() || st.lease.token != t {
		return problem(CodeLeaseLost, "the lease is not the session's current one", "")
	}
	return nil
}

// leaseOut is a lease on the wire. The token is a decimal string, since
// it can pass 2^53, where a JSON number loses digits in some readers.
type leaseOut struct {
	LeaseID string    `json:"lease_id"`
	Token   uint64    `json:"token,string"`
	Expires time.Time `json:"expires"`
	// Head is the session's head as the lease starts, null for none: a
	// writer's session starts there.
	Head *string `json:"head"`
}

func leaseOf(l *lease, sess *agentsession.Session) *leaseOut {
	// Expires is advisory: the client keeps its own deadline.
	out := &leaseOut{LeaseID: l.id, Token: l.token, Expires: time.Now().Add(time.Until(l.expires)).UTC()}
	if sess != nil {
		out.Head = headOut(sess.Leaf())
	}
	return out
}

// idempotent remembers the outcome of a request sent with an
// Idempotency-Key, so a retry of it is answered the same.
type idempotent struct {
	mu   sync.Mutex
	seen map[string]keyed
}

type keyed struct {
	status int
	body   []byte
	at     time.Time
}

// get returns what the request under key was answered, within keep.
func (c *idempotent) get(key string, keep time.Duration) (keyed, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k, ok := c.seen[key]
	if !ok || time.Since(k.at) > keep {
		return keyed{}, false
	}
	return k, true
}

// put remembers an answer, dropping those older than keep.
func (c *idempotent) put(key string, k keyed, keep time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seen == nil {
		c.seen = map[string]keyed{}
	}
	for old, v := range c.seen {
		if time.Since(v.at) > keep {
			delete(c.seen, old)
		}
	}
	k.at = time.Now()
	c.seen[key] = k
}
