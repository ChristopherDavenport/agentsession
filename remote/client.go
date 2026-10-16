package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Client is a store a [Handler] serves, over the wire. It implements
// [agentsession.Store], [agentsession.Writer], [agentsession.Reader],
// [agentsession.Follower] and [agentsession.RefStore].
//
// It writes under leases, RFC 0003's form of a store's hold: Create and
// Open take one on the session, which the client renews until Release,
// Delete or Close ends it, and every write carries its fencing token.
// The session Create and Open return is the client's own, kept up to
// date with the client's appends and not shared with the server, as a
// local store's session is shared with that store: moving its leaf
// (Session.Branch, ResetLeaf) steers the client's next append, and that
// append moves the server's head with it, by RFC 0003's compare-and-swap,
// so the record's head is the writer's leaf.
type Client struct {
	base  *url.URL
	hc    *http.Client
	token string

	holder   string
	leaseTTL time.Duration
	// redactedOnly says the client sends only redacted sessions.
	redactedOnly bool
	holdMu       sync.Mutex
	holds        map[string]*holding

	mu sync.Mutex
	// last is each session's replica where its last follow stopped, so a
	// follow resumed from that cursor goes on from it.
	last map[string]resumable
	// cache is the objects the client read, by hash.
	cache objectCache
}

type resumable struct {
	cursor agentsession.Cursor
	rep    *replica
}

var (
	_ agentsession.Store    = (*Client)(nil)
	_ agentsession.Writer   = (*Client)(nil)
	_ agentsession.Reader   = (*Client)(nil)
	_ agentsession.Follower = (*Client)(nil)
	_ agentsession.RefStore = (*Client)(nil)
)

// ClientOption configures [Dial].
type ClientOption func(*Client)

// WithHTTPClient sets the HTTP client requests go through.
func WithHTTPClient(hc *http.Client) ClientOption { return func(c *Client) { c.hc = hc } }

// WithBearerToken sends token as every request's bearer token.
func WithBearerToken(token string) ClientOption { return func(c *Client) { c.token = token } }

// WithHolder labels the leases the client takes, which a refused
// acquire shows another writer. It is a label, not an identity.
func WithHolder(label string) ClientOption { return func(c *Client) { c.holder = label } }

// WithLeaseTTL sets the time to live the client asks for its leases
// (default 30s); it renews each at a third of it.
func WithLeaseTTL(d time.Duration) ClientOption {
	return func(c *Client) {
		if d > 0 {
			c.leaseTTL = d
		}
	}
}

// WithRedactedOnly makes the client send only redacted sessions, as a
// publisher's client of a store that holds only redacted ones: Create
// and [Push] refuse a session whose header does not carry redacted with
// [agentsession.ErrNotRedacted] before anything is sent, a fork of a
// redacted session among them. The server's store refuses one too when
// it is set to hold only redacted sessions, answering not_redacted; the
// option makes the client fail closed against a server that is not.
func WithRedactedOnly() ClientOption { return func(c *Client) { c.redactedOnly = true } }

// Dial returns a client of the server at baseURL, the URL the /v1
// routes are under. It does not contact the server.
func Dial(baseURL string, opts ...ClientOption) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("remote: %q is not an http or https URL", baseURL)
	}
	c := &Client{base: u, hc: http.DefaultClient, last: map[string]resumable{}, holds: map[string]*holding{},
		holder: "remote-client", leaseTTL: 30 * time.Second}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// get sends a GET for path under /v1 and returns the response, or the
// problem a status other than 200 carries.
func (c *Client) get(ctx context.Context, path string, q url.Values, header http.Header) (*http.Response, error) {
	return c.do(ctx, http.MethodGet, path, q, header, nil)
}

// do sends a request for path under /v1 and returns the response, or
// the problem a status outside 2xx carries. A body is JSON unless the
// header names another type.
func (c *Client) do(ctx context.Context, method, path string, q url.Values, header http.Header, body []byte) (*http.Response, error) {
	u := *c.base
	u.Path += Version + path
	u.RawQuery = q.Encode()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("remote: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		return nil, problemFrom(resp)
	}
	return resp, nil
}

// problemFrom reads the problem a response carries, or makes one of its
// status.
func problemFrom(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	p := &Problem{}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") && json.Unmarshal(data, p) == nil && p.Code != "" {
		return p
	}
	return &Problem{Status: resp.StatusCode, Code: CodeStoreFault, Title: resp.Status}
}

// Capabilities asks the server what it serves.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var caps Capabilities
	resp, err := c.get(ctx, "/", nil, nil)
	if err != nil {
		return caps, err
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		return caps, fmt.Errorf("remote: capabilities: %w", err)
	}
	return caps, nil
}

// lines reads an NDJSON listing, yielding each line until the end
// frame. A listing that stops without one, or ends with an error frame,
// yields the error.
func (c *Client) lines(ctx context.Context, path string, q url.Values) iter.Seq2[json.RawMessage, error] {
	return c.linesTo(ctx, path, q, nil)
}

// linesTo is lines, keeping the end frame in end when end is not nil.
func (c *Client) linesTo(ctx context.Context, path string, q url.Values, end *json.RawMessage) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		resp, err := c.get(ctx, path, q, nil)
		if err != nil {
			yield(nil, err)
			return
		}
		defer resp.Body.Close()
		br := bufio.NewReader(resp.Body)
		for {
			line, err := br.ReadBytes('\n')
			line = bytes.TrimSpace(line)
			if len(line) > 0 {
				var frame struct {
					End   bool     `json:"end"`
					Error *Problem `json:"error"`
				}
				if jerr := json.Unmarshal(line, &frame); jerr != nil {
					yield(nil, fmt.Errorf("remote: %s: %w", path, jerr))
					return
				}
				switch {
				case frame.Error != nil:
					yield(nil, frame.Error)
					return
				case frame.End:
					if end != nil {
						*end = append(json.RawMessage(nil), line...)
					}
					return
				}
				if !yield(json.RawMessage(line), nil) {
					return
				}
			}
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				yield(nil, fmt.Errorf("%w: %s", ErrCut, path))
				return
			}
		}
	}
}

// List lists the sessions matching f, newest first, as the store's List
// does. A summary carries no path.
func (c *Client) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	q := url.Values{}
	set := func(k, v string) {
		if v != "" {
			q.Set(k, v)
		}
	}
	set("cwd", f.CWD)
	set("parent_session", f.ParentSession)
	set("harness", f.Harness)
	for k, v := range map[string]bool{"top_level": f.TopLevel, "with_names": f.WithNames, "current": f.Current} {
		if v {
			q.Set(k, "true")
		}
	}
	if !f.After.IsZero() {
		q.Set("after", f.After.Format(time.RFC3339Nano))
	}
	if !f.Before.IsZero() {
		q.Set("before", f.Before.Format(time.RFC3339Nano))
	}
	if f.Limit > 0 {
		q.Set("limit", strconv.Itoa(f.Limit))
	}
	for k, v := range f.Extra {
		q.Add("extra", k+":"+string(v))
	}
	return func(yield func(agentsession.Summary, error) bool) {
		for line, err := range c.lines(ctx, "/sessions", q) {
			if err != nil {
				yield(agentsession.Summary{}, err)
				return
			}
			var l summaryLine
			if err := json.Unmarshal(line, &l); err != nil {
				yield(agentsession.Summary{}, fmt.Errorf("remote: list: %w", err))
				return
			}
			s := agentsession.Summary{Header: l.Header, Name: l.Name, SupersededBy: l.SupersededBy, Size: l.Size}
			if l.Modified != nil {
				s.Modified = *l.Modified
			}
			if !yield(s, nil) {
				return
			}
		}
	}
}

// SessionInfo is a session's state as the server holds it: not a read
// of its entries.
type SessionInfo struct {
	Header      agentsession.Header
	Head        string
	Length      int
	Incarnation string
	// Cursor is where a follow from this state resumes.
	Cursor agentsession.Cursor
	// Mark is the server's record mark for the session, record or
	// mirror, where its store keeps one, and "" where it does not.
	Mark string
}

// Session returns the session's state.
func (c *Client) Session(ctx context.Context, id string) (SessionInfo, error) {
	resp, err := c.get(ctx, "/sessions/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return SessionInfo{}, err
	}
	defer resp.Body.Close()
	var info sessionInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return SessionInfo{}, fmt.Errorf("remote: session %s: %w", id, err)
	}
	return SessionInfo{Header: info.Header, Head: headIn(info.Head), Length: info.Length, Incarnation: info.Incarnation, Cursor: agentsession.Cursor(info.Cursor), Mark: info.Mark}, nil
}

// Read implements [agentsession.Reader]: the whole session, as a
// follow's snapshot carries it, since this slice serves no objects to
// read it by. The session is the caller's own.
func (c *Client) Read(ctx context.Context, id string) (*agentsession.Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for ch, err := range c.stream(ctx, id, "", nil, false, false) {
		if err != nil {
			return nil, err
		}
		return ch.Session, nil
	}
	return nil, fmt.Errorf("%w: follow of %s", ErrCut, id)
}

// Follow implements [agentsession.Follower] over the server's follow.
//
// Every entry is verified by its hash before it is yielded. A follow the
// server cuts because it fell behind resumes from its last event by
// itself; one cut otherwise ends with [ErrCut], and the caller resumes
// from the last change's cursor. This client can resume only from a
// cursor of a follow it served: it holds the session there. For any
// other cursor it follows afresh and yields the session as a
// [agentsession.Reset], which the contract allows for a cursor it cannot
// resume from, and which a consumer handles by dropping what it derived
// before.
func (c *Client) Follow(ctx context.Context, id string, from agentsession.Cursor) iter.Seq2[agentsession.Change, error] {
	return c.follow(ctx, id, from, false)
}

// follow is Follow, or FollowCommitted with durable.
func (c *Client) follow(ctx context.Context, id string, from agentsession.Cursor, durable bool) iter.Seq2[agentsession.Change, error] {
	return func(yield func(agentsession.Change, error) bool) {
		var rep *replica
		reset := false
		if from != "" {
			c.mu.Lock()
			if r, ok := c.last[id]; ok && r.cursor == from {
				// A copy: the session the earlier follow yielded is the
				// caller's to keep.
				if d, err := snapshotOf(r.rep.sess); err == nil {
					if cp, err := newReplica(d); err == nil {
						cp.seq = r.rep.seq
						rep = cp
					}
				}
			}
			c.mu.Unlock()
			if rep == nil {
				from, reset = "", true
			}
		}
		for ch, err := range c.stream(ctx, id, from, rep, reset, durable) {
			if !yield(ch, err) || err != nil {
				return
			}
		}
	}
}

// stream follows id from the event ID from, with rep the session there,
// or from the start when from is empty. reset reports the first
// snapshot as a Reset: a fresh start answering a cursor the caller gave.
// durable follows only what is committed.
func (c *Client) stream(ctx context.Context, id string, from agentsession.Cursor, rep *replica, reset, durable bool) iter.Seq2[agentsession.Change, error] {
	return func(yield func(agentsession.Change, error) bool) {
		resumed := from != "" || reset
		last := string(from)
		defer func() {
			if rep != nil && last != "" {
				c.mu.Lock()
				c.last[id] = resumable{cursor: agentsession.Cursor(last), rep: rep}
				c.mu.Unlock()
			}
		}()
		for {
			h := http.Header{}
			if last != "" {
				h.Set("Last-Event-ID", last)
			}
			var q url.Values
			if durable {
				q = url.Values{"durable": {"true"}}
			}
			resp, err := c.get(ctx, "/sessions/"+url.PathEscape(id)+"/follow", q, h)
			if err != nil {
				if ctx.Err() == nil {
					yield(agentsession.Change{}, err)
				}
				return
			}
			again, ok := c.read(ctx, resp, id, &rep, &last, resumed, yield)
			resp.Body.Close()
			if !ok || !again {
				return
			}
			resumed = true
		}
	}
}

// read yields the changes of one follow response. again is set when the
// server cut the follow for falling behind, and the stream resumes from
// the last event.
func (c *Client) read(ctx context.Context, resp *http.Response, id string, rep **replica, last *string, resumed bool, yield func(agentsession.Change, error) bool) (again, ok bool) {
	fail := func(err error) (bool, bool) {
		if ctx.Err() != nil {
			return false, false
		}
		yield(agentsession.Change{}, err)
		return false, false
	}
	for ev, err := range readSSE(resp.Body) {
		if err != nil {
			if ctx.Err() != nil {
				return false, false
			}
			return fail(fmt.Errorf("%w: follow of %s: %v", ErrCut, id, err))
		}
		switch ev.name {
		case eventSnapshot, eventReset:
			var d snapshotData
			if err := json.Unmarshal(ev.data, &d); err != nil {
				return fail(fmt.Errorf("remote: follow of %s: %w", id, err))
			}
			// Records past the server's snapshot bound come without their
			// lines, which are read as objects.
			if err := c.fill(ctx, &d); err != nil {
				return fail(err)
			}
			r, err := newReplica(&d)
			if err != nil {
				return fail(err)
			}
			*rep, *last = r, ev.id
			kind := agentsession.Snapshot
			if ev.name == eventReset || resumed {
				kind = agentsession.Reset
			}
			if !yield(agentsession.Change{Kind: kind, Session: r.sess, Cursor: agentsession.Cursor(ev.id)}, nil) {
				return false, false
			}
		case eventAppended:
			if *rep == nil {
				return fail(fmt.Errorf("remote: follow of %s: an append before the snapshot", id))
			}
			var d appendedData
			if err := json.Unmarshal(ev.data, &d); err != nil {
				return fail(fmt.Errorf("remote: follow of %s: %w", id, err))
			}
			e, before, err := (*rep).appended(&d)
			if err != nil {
				return fail(err)
			}
			*last = ev.id
			cur := agentsession.Cursor(ev.id)
			s := (*rep).sess
			if !yield(agentsession.Change{Kind: agentsession.Appended, Session: s, ID: d.Entry, Entry: e, Cursor: cur}, nil) {
				return false, false
			}
			// An append that moved the head elsewhere than to itself, a
			// leaf label's, is a head change too, as the library's
			// followers report it.
			if l := s.Leaf(); l != d.Entry && l != before {
				if !yield(agentsession.Change{Kind: agentsession.Head, Session: s, Leaf: l, Cursor: cur}, nil) {
					return false, false
				}
			}
		case eventHead:
			if *rep == nil {
				return fail(fmt.Errorf("remote: follow of %s: a head before the snapshot", id))
			}
			var d headData
			if err := json.Unmarshal(ev.data, &d); err != nil {
				return fail(fmt.Errorf("remote: follow of %s: %w", id, err))
			}
			before := (*rep).sess.Leaf()
			if err := (*rep).place(headIn(d.Head)); err != nil {
				return fail(err)
			}
			*last = ev.id
			if l := (*rep).sess.Leaf(); l != before {
				if !yield(agentsession.Change{Kind: agentsession.Head, Session: (*rep).sess, Leaf: l, Cursor: agentsession.Cursor(ev.id)}, nil) {
					return false, false
				}
			}
		case eventError:
			p := &Problem{}
			if err := json.Unmarshal(ev.data, p); err != nil || p.Code == "" {
				return fail(fmt.Errorf("remote: follow of %s: an error event that does not parse", id))
			}
			if p.Code == CodeOverflow && *last != "" {
				return true, true
			}
			return fail(p)
		}
	}
	// A follow has no end frame: a stream that stops was cut.
	return fail(fmt.Errorf("%w: follow of %s", ErrCut, id))
}

// sseEvent is one server-sent event as read.
type sseEvent struct {
	name, id string
	data     []byte
}

// readSSE reads server-sent events until the body ends, which it
// reports as io.EOF.
func readSSE(body io.Reader) iter.Seq2[sseEvent, error] {
	return func(yield func(sseEvent, error) bool) {
		br := bufio.NewReader(body)
		var ev sseEvent
		var data bytes.Buffer
		for {
			line, err := br.ReadBytes('\n')
			if err != nil {
				yield(sseEvent{}, err)
				return
			}
			line = bytes.TrimRight(line, "\r\n")
			switch {
			case len(line) == 0:
				if ev.name != "" || data.Len() > 0 {
					ev.data = bytes.Clone(data.Bytes())
					if !yield(ev, nil) {
						return
					}
				}
				ev, data = sseEvent{}, bytes.Buffer{}
			case line[0] == ':':
			default:
				field, value, _ := bytes.Cut(line, []byte{':'})
				value = bytes.TrimPrefix(value, []byte{' '})
				switch string(field) {
				case "event":
					ev.name = string(value)
				case "id":
					ev.id = string(value)
				case "data":
					if data.Len() > 0 {
						data.WriteByte('\n')
					}
					data.Write(value)
				}
			}
		}
	}
}

// ResolveRef implements [agentsession.RefStore]. A dangling ref returns
// its target with an error wrapping [agentsession.ErrNoSession], as the
// library's stores do.
func (c *Client) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	// A name is a path, and one the format refuses could be one the
	// server's router cleans into another name: it is refused here.
	if err := agentsession.ValidRefName(name); err != nil {
		return agentsession.RefTarget{}, err
	}
	resp, err := c.get(ctx, "/refs/"+name, nil, nil)
	if err != nil {
		var p *Problem
		if errors.As(err, &p) && p.Code == CodeRefDangling {
			return p.Target.lib(), err
		}
		return agentsession.RefTarget{}, err
	}
	defer resp.Body.Close()
	var l refLine
	if err := json.NewDecoder(resp.Body).Decode(&l); err != nil {
		return agentsession.RefTarget{}, fmt.Errorf("remote: ref %s: %w", name, err)
	}
	return l.Target.lib(), nil
}

// ListRefs implements [agentsession.RefStore].
func (c *Client) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	q := url.Values{}
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	return func(yield func(agentsession.Ref, error) bool) {
		if prefix != "" {
			if err := agentsession.ValidRefPrefix(prefix); err != nil {
				yield(agentsession.Ref{}, err)
				return
			}
		}
		for line, err := range c.lines(ctx, "/refs", q) {
			if err != nil {
				yield(agentsession.Ref{}, err)
				return
			}
			var l refLine
			if err := json.Unmarshal(line, &l); err != nil {
				yield(agentsession.Ref{}, fmt.Errorf("remote: refs: %w", err))
				return
			}
			if !yield(agentsession.Ref{Name: l.Name, Target: l.Target.lib()}, nil) {
				return
			}
		}
	}
}

// RefLog implements [agentsession.RefStore].
func (c *Client) RefLog(ctx context.Context, name string) iter.Seq2[agentsession.RefUpdate, error] {
	return func(yield func(agentsession.RefUpdate, error) bool) {
		if err := agentsession.ValidRefName(name); err != nil {
			yield(agentsession.RefUpdate{}, err)
			return
		}
		for line, err := range c.lines(ctx, "/refs/"+name, url.Values{"view": {"log"}}) {
			if err != nil {
				yield(agentsession.RefUpdate{}, err)
				return
			}
			var l refUpdateLine
			if err := json.Unmarshal(line, &l); err != nil {
				yield(agentsession.RefUpdate{}, fmt.Errorf("remote: ref log: %w", err))
				return
			}
			if !yield(agentsession.RefUpdate{Name: l.Name, Old: l.Old.lib(), New: l.New.lib(), Time: l.Time, Reason: l.Reason}, nil) {
				return
			}
		}
	}
}
