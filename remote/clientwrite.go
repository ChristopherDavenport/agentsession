package remote

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// holding is a session the client holds under a lease: the lease, the
// client's own session, and the server's head as last reported. Its
// lock orders the client's writes to the session.
type holding struct {
	mu      sync.Mutex
	id      string
	leaseID string
	token   uint64
	// deadline is the client's own: the time it sent the acquire or last
	// renew, plus the lease's time, less a margin. Past it the client
	// writes nothing, whatever the server would say.
	deadline time.Time
	sess     *agentsession.Session
	// head is the server's head, which the client's appends and head
	// moves keep in step with: nobody else writes under the lease.
	head string
	stop chan struct{}
	done bool
}

// leaseMargin is how long before a lease's end the client stops writing
// under it: the server's clock and the network's delay are not the
// client's.
func (c *Client) leaseMargin() time.Duration { return c.leaseTTL / 5 }

// hold returns the client's holding of the session with its lock held,
// taking a lease when the client has none. The caller unlocks it.
func (c *Client) hold(ctx context.Context, id string) (*holding, error) {
	c.holdMu.Lock()
	hd, ok := c.holds[id]
	if !ok {
		hd = &holding{id: id}
		c.holds[id] = hd
	}
	c.holdMu.Unlock()
	hd.mu.Lock()
	if hd.leaseID != "" && !hd.done {
		return hd, nil
	}
	if err := c.acquire(ctx, hd); err != nil {
		c.drop(hd)
		hd.mu.Unlock()
		return nil, err
	}
	return hd, nil
}

// drop forgets hd, unless another holding took its place. The caller
// holds hd.mu.
func (c *Client) drop(hd *holding) {
	hd.done = true
	if hd.stop != nil {
		close(hd.stop)
		hd.stop = nil
	}
	c.holdMu.Lock()
	if c.holds[hd.id] == hd {
		delete(c.holds, hd.id)
	}
	c.holdMu.Unlock()
}

// acquire takes a lease on hd's session. The caller holds hd.mu.
func (c *Client) acquire(ctx context.Context, hd *holding) error {
	body, _ := json.Marshal(leaseRequest{Holder: c.holder, TTLms: c.leaseTTL.Milliseconds()})
	sent := time.Now()
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(hd.id)+"/lease", nil, nil, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var l leaseOut
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return fmt.Errorf("remote: lease on %s: %w", hd.id, err)
	}
	c.leased(hd, &l, sent)
	return nil
}

// leased takes a granted lease into hd and starts renewing it. The
// caller holds hd.mu.
func (c *Client) leased(hd *holding, l *leaseOut, sent time.Time) {
	hd.leaseID, hd.token, hd.head, hd.done = l.LeaseID, l.Token, headIn(l.Head), false
	hd.deadline = sent.Add(c.leaseTTL - c.leaseMargin())
	hd.stop = make(chan struct{})
	go c.renew(hd, hd.leaseID, hd.stop)
}

// renew keeps hd's lease until stop closes or the server says it is
// lost.
func (c *Client) renew(hd *holding, leaseID string, stop chan struct{}) {
	t := time.NewTicker(c.leaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		body, _ := json.Marshal(leaseRequest{TTLms: c.leaseTTL.Milliseconds()})
		sent := time.Now()
		ctx, cancel := context.WithTimeout(context.Background(), c.leaseTTL/3)
		resp, err := c.do(ctx, http.MethodPut, "/sessions/"+url.PathEscape(hd.id)+"/lease", nil, http.Header{"Lease-Id": {leaseID}}, body)
		cancel()
		if err != nil {
			if errors.Is(err, ErrLeaseLost) {
				return
			}
			continue // the next tick tries again, until the deadline passes
		}
		resp.Body.Close()
		hd.mu.Lock()
		if hd.leaseID == leaseID && !hd.done {
			hd.deadline = sent.Add(c.leaseTTL - c.leaseMargin())
		}
		hd.mu.Unlock()
	}
}

// fence is the header a write under hd's lease carries, or an error
// when the client's own deadline for the lease has passed. The caller
// holds hd.mu.
func (c *Client) fence(hd *holding) (http.Header, error) {
	if time.Now().After(hd.deadline) {
		return nil, fmt.Errorf("%w: the lease on %s passed its deadline unrenewed", ErrLeaseLost, hd.id)
	}
	return http.Header{"Lease-Token": {strconv.FormatUint(hd.token, 10)}, "Session-Format": {agentsession.Format}}, nil
}

// session loads hd's session when the client has not read it yet: the
// server's, with its leaf where the lease started. The caller holds
// hd.mu.
func (c *Client) session(ctx context.Context, hd *holding) error {
	if hd.sess != nil {
		return nil
	}
	sess, err := c.Read(ctx, hd.id)
	if err != nil {
		return err
	}
	if sess.Leaf() != hd.head {
		if hd.head == "" {
			sess.ResetLeaf()
		} else if err := sess.Branch(hd.head); err != nil {
			return fmt.Errorf("remote: open %s: %w", hd.id, err)
		}
	}
	hd.sess = sess
	return nil
}

// Create implements [agentsession.Store]: the session is created under a
// lease the client holds from its first instant, and returned as the
// client's own.
func (c *Client) Create(ctx context.Context, h agentsession.Header) (*agentsession.Session, error) {
	if c.redactedOnly && !h.Redacted {
		return nil, fmt.Errorf("%w: create %s through a client that sends only redacted sessions", agentsession.ErrNotRedacted, h.ID)
	}
	body, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	// A header the server fills is not retry-safe on its own; the key
	// makes a retry of this request the same create.
	var k [16]byte
	_, _ = rand.Read(k[:])
	q := url.Values{"lease_ttl_ms": {strconv.FormatInt(c.leaseTTL.Milliseconds(), 10)}, "holder": {c.holder}}
	sent := time.Now()
	resp, err := c.do(ctx, http.MethodPost, "/sessions", q, http.Header{"Idempotency-Key": {hex.EncodeToString(k[:])}}, body)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var cr createResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("remote: create: %w", err)
	}
	if cr.Session == nil || cr.Lease == nil {
		return nil, fmt.Errorf("remote: create: the server answered without the session or its lease")
	}
	rep, err := newReplica(cr.Session)
	if err != nil {
		return nil, err
	}
	hd := &holding{id: rep.sess.ID(), sess: rep.sess}
	hd.mu.Lock()
	c.leased(hd, cr.Lease, sent)
	hd.mu.Unlock()
	c.holdMu.Lock()
	if old, ok := c.holds[hd.id]; ok && old != hd {
		old.mu.Lock()
		old.done = true
		if old.stop != nil {
			close(old.stop)
			old.stop = nil
		}
		old.mu.Unlock()
	}
	c.holds[hd.id] = hd
	c.holdMu.Unlock()
	return rep.sess, nil
}

// Open implements [agentsession.Store]: it takes a lease on the session,
// unless the client holds one, and returns the client's session, the
// same one each time while the lease holds.
func (c *Client) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	hd, err := c.hold(ctx, id)
	if err != nil {
		return nil, err
	}
	defer hd.mu.Unlock()
	if err := c.session(ctx, hd); err != nil {
		c.releaseHeld(ctx, hd)
		return nil, err
	}
	return hd.sess, nil
}

// Append implements [agentsession.Store].
func (c *Client) Append(ctx context.Context, sessionID string, e agentsession.Entry) (string, error) {
	r, err := c.Write(ctx, sessionID, e)
	return r.ID, err
}

// Write implements [agentsession.Writer]. The entry is prepared on the
// client's session, so it goes as a complete line, the same entry on a
// retry; the server's head is first moved to the client's leaf when the
// two differ, so the append lands where the writer's session says; and
// the client's session takes the entry once the server has.
func (c *Client) Write(ctx context.Context, sessionID string, e agentsession.Entry) (agentsession.Result, error) {
	hd, err := c.hold(ctx, sessionID)
	if err != nil {
		return agentsession.Result{}, err
	}
	defer hd.mu.Unlock()
	if err := c.session(ctx, hd); err != nil {
		return agentsession.Result{}, err
	}
	pre, err := hd.sess.Prepare(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	if pre.Outcome == agentsession.Held {
		return pre, nil
	}
	line, err := agentsession.MarshalEntry(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	header, err := c.fence(hd)
	if err != nil {
		return agentsession.Result{}, err
	}
	if leaf := hd.sess.Leaf(); leaf != "" && leaf != hd.head {
		if err := c.moveHead(ctx, hd, header, leaf); err != nil {
			return agentsession.Result{}, err
		}
	}
	header.Set("Content-Type", "application/x-ndjson")
	resp, err := c.do(ctx, http.MethodPost, "/sessions/"+url.PathEscape(sessionID)+"/entries", nil, header, append(line, '\n'))
	if err != nil {
		return agentsession.Result{}, err
	}
	defer resp.Body.Close()
	var ar appendResponse
	if err := json.NewDecoder(resp.Body).Decode(&ar); err != nil {
		return agentsession.Result{}, fmt.Errorf("remote: append to %s: %w", sessionID, err)
	}
	if ar.Stopped != nil {
		return agentsession.Result{}, ar.Stopped.Problem
	}
	if len(ar.Results) != 1 {
		return agentsession.Result{}, fmt.Errorf("remote: append to %s: %d results for one entry", sessionID, len(ar.Results))
	}
	got := ar.Results[0]
	hd.head = headIn(ar.Head)
	res := agentsession.Result{ID: got.ID, Durable: got.Durable}
	if len(got.Unresolved) > 0 {
		res.Unresolved = got.Unresolved
	}
	res.Outcome, _ = outcomeNamed(got.Outcome)
	// The client's session takes an entry of its own, as a store keeps
	// one: the caller's entry is the caller's.
	own, err := agentsession.UnmarshalEntry(line)
	if err == nil {
		_, err = hd.sess.Commit(own)
	}
	if err != nil || got.ID != pre.ID {
		// The server has the entry and this session could not follow:
		// the caller opens the session again.
		res.Reopen = true
		hd.sess = nil
	}
	return res, nil
}

// moveHead moves the server's head from where the client last saw it to
// to, by compare-and-swap. The caller holds hd.mu.
func (c *Client) moveHead(ctx context.Context, hd *holding, header http.Header, to string) error {
	body, _ := json.Marshal(headRequest{Expected: headOut(hd.head), To: &to})
	resp, err := c.do(ctx, http.MethodPut, "/sessions/"+url.PathEscape(hd.id)+"/head", nil, header.Clone(), body)
	if err != nil {
		return err
	}
	resp.Body.Close()
	hd.head = to
	return nil
}

// Delete implements [agentsession.Store]: under the client's lease,
// taking one when it holds none, and the lease ends with the session.
func (c *Client) Delete(ctx context.Context, id string) error {
	hd, err := c.hold(ctx, id)
	if err != nil {
		return err
	}
	defer hd.mu.Unlock()
	header, err := c.fence(hd)
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(id), nil, header, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	c.drop(hd)
	return nil
}

// Release ends the client's lease on the session, so another writer can
// take it. The session Open returned no longer follows the server.
func (c *Client) Release(id string) error {
	c.holdMu.Lock()
	hd, ok := c.holds[id]
	c.holdMu.Unlock()
	if !ok {
		return nil
	}
	hd.mu.Lock()
	defer hd.mu.Unlock()
	return c.releaseHeld(context.Background(), hd)
}

// releaseHeld ends hd's lease. The caller holds hd.mu.
func (c *Client) releaseHeld(ctx context.Context, hd *holding) error {
	leaseID := hd.leaseID
	c.drop(hd)
	if leaseID == "" {
		return nil
	}
	resp, err := c.do(ctx, http.MethodDelete, "/sessions/"+url.PathEscape(hd.id)+"/lease", nil, http.Header{"Lease-Id": {leaseID}}, nil)
	if err != nil {
		if errors.Is(err, ErrLeaseLost) {
			return nil // it had ended already
		}
		return err
	}
	resp.Body.Close()
	return nil
}

// Close ends every lease the client holds.
func (c *Client) Close() error {
	c.holdMu.Lock()
	all := make([]*holding, 0, len(c.holds))
	for _, hd := range c.holds {
		all = append(all, hd)
	}
	c.holdMu.Unlock()
	var first error
	for _, hd := range all {
		hd.mu.Lock()
		if err := c.releaseHeld(context.Background(), hd); err != nil && first == nil {
			first = err
		}
		hd.mu.Unlock()
	}
	return first
}

// UpdateRef implements [agentsession.RefStore]: RFC 0003's ref
// compare-and-swap, which needs refs scope and no lease.
func (c *Client) UpdateRef(ctx context.Context, name string, expected, next agentsession.RefTarget, reason string) error {
	if err := agentsession.ValidRefName(name); err != nil {
		return err
	}
	body, err := json.Marshal(refUpdate{Expected: targetOut(expected), Next: targetOut(next), Reason: reason})
	if err != nil {
		return err
	}
	resp, err := c.do(ctx, http.MethodPut, "/refs/"+name, nil, nil, body)
	if err != nil {
		var p *Problem
		if errors.As(err, &p) && p.Code == CodeRefMoved {
			// The library's form, with the target the ref holds.
			return &agentsession.RefMovedError{Name: name, Current: p.Target.lib()}
		}
		return err
	}
	resp.Body.Close()
	return nil
}
