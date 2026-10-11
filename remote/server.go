package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
)

// Handler serves a store over RFC 0003: reading, following and refs,
// and writing under leases. Close stops the followers it runs and ends
// its leases.
type Handler struct {
	store    agentsession.Store
	reader   agentsession.Reader
	follower agentsession.Follower
	refs     agentsession.RefStore

	auth      Authenticator
	insecure  bool
	buffer    int
	replay    int
	linger    time.Duration
	heartbeat time.Duration
	// catchUpWait bounds how long a read waits for the session's
	// follower to catch up with the store.
	catchUpWait time.Duration
	logf        func(error)

	mux    *http.ServeMux
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	hubs   map[string]*hub

	// The write side: each session ID's lease and token, the lease
	// times, and the answers kept for an Idempotency-Key.
	wmu         sync.Mutex
	held        map[string]*held
	leaseTTL    time.Duration
	maxLeaseTTL time.Duration
	idem        idempotent

	// Exchange: the store's objects and marks where it has them, the
	// objects each principal uploaded for a push and how long they are
	// kept for it, and which objects the store's sessions reach.
	ex          exchanger
	uploads     uploads
	uploadGrace time.Duration
	reach       reach
}

// Option configures a [Handler].
type Option func(*Handler)

// WithAuthenticator sets how a request's principal is resolved. Without
// it, or [WithInsecureNoAuth], the handler refuses every request.
func WithAuthenticator(a Authenticator) Option { return func(h *Handler) { h.auth = a } }

// WithInsecureNoAuth serves every request as a principal with every
// scope, for tests and for a socket only the host's user can reach.
// Everything a session holds is readable by whoever can connect.
func WithInsecureNoAuth() Option { return func(h *Handler) { h.insecure = true } }

// WithFollowBuffer sets how many events a follow may fall behind before
// it is cut with `overflow` (default 256).
func WithFollowBuffer(n int) Option {
	return func(h *Handler) {
		if n > 0 {
			h.buffer = n
		}
	}
}

// WithFollowReplay sets how many recent events of a session the handler
// keeps, so a client that resumes from one of them is served without
// loading the session again (default 1024).
func WithFollowReplay(n int) Option {
	return func(h *Handler) {
		if n > 0 {
			h.replay = n
		}
	}
}

// WithLinger sets how long a session's follower outlives its last
// client, so reads close together share one load (default 2s).
func WithLinger(d time.Duration) Option {
	return func(h *Handler) {
		if d >= 0 {
			h.linger = d
		}
	}
}

// WithHeartbeat sets how often an idle follow is sent a comment, so
// intermediaries do not close it (default 15s).
func WithHeartbeat(d time.Duration) Option {
	return func(h *Handler) {
		if d > 0 {
			h.heartbeat = d
		}
	}
}

// WithLeaseLimits sets a lease's time to live when an acquire names none
// (default 30s), and the most an acquire or renew may ask for (default
// 5m). A lease not renewed within its time ends.
func WithLeaseLimits(def, max time.Duration) Option {
	return func(h *Handler) {
		if def > 0 {
			h.leaseTTL = def
		}
		if max > 0 {
			h.maxLeaseTTL = max
		}
		if h.leaseTTL > h.maxLeaseTTL {
			h.leaseTTL = h.maxLeaseTTL
		}
	}
}

// WithUploadGrace sets how long an object uploaded for a push stays
// usable by its uploader's push (default 1h). It is published in the
// capabilities, and it is the least grace a Sweep of the store must give
// unreferenced objects, since an upload is one until a push names it.
func WithUploadGrace(d time.Duration) Option {
	return func(h *Handler) {
		if d > 0 {
			h.uploadGrace = d
		}
	}
}

// WithErrorLog receives the errors the handler answers as
// `store_fault`, whose text a client is not sent.
func WithErrorLog(fn func(error)) Option { return func(h *Handler) { h.logf = fn } }

// NewHandler serves store. It reads sessions through the store's
// [agentsession.Reader] and follows them through its
// [agentsession.Follower], so a store without both serves neither, and
// refs through its [agentsession.RefStore] when it keeps them.
func NewHandler(store agentsession.Store, opts ...Option) *Handler {
	h := &Handler{
		store:       store,
		buffer:      256,
		replay:      1024,
		linger:      2 * time.Second,
		heartbeat:   15 * time.Second,
		hubs:        map[string]*hub{},
		catchUpWait: 30 * time.Second,
		held:        map[string]*held{},
		leaseTTL:    30 * time.Second,
		maxLeaseTTL: 5 * time.Minute,
		uploadGrace: time.Hour,
	}
	h.ex, _ = store.(exchanger)
	h.reader, _ = store.(agentsession.Reader)
	h.follower, _ = store.(agentsession.Follower)
	h.refs, _ = store.(agentsession.RefStore)
	for _, o := range opts {
		o(h)
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/{$}", h.guard("", h.capabilities))
	mux.HandleFunc("GET /v1/sessions", h.guard(ScopeRead, h.list))
	mux.HandleFunc("GET /v1/sessions/{id}", h.guard(ScopeRead, h.session))
	mux.HandleFunc("GET /v1/sessions/{id}/path", h.guard(ScopeRead, h.path))
	mux.HandleFunc("GET /v1/sessions/{id}/follow", h.guard(ScopeRead, h.follow))
	mux.HandleFunc("GET /v1/refs", h.guard(ScopeRead, h.listRefs))
	mux.HandleFunc("GET /v1/refs/{name...}", h.guard(ScopeRead, h.ref))
	mux.HandleFunc("POST /v1/sessions", h.guard(ScopeWrite, h.create))
	mux.HandleFunc("DELETE /v1/sessions/{id}", h.guard(ScopeWrite, h.deleteSession))
	mux.HandleFunc("POST /v1/sessions/{id}/lease", h.guard(ScopeWrite, h.leaseAcquire))
	mux.HandleFunc("PUT /v1/sessions/{id}/lease", h.guard(ScopeWrite, h.leaseRenew))
	mux.HandleFunc("DELETE /v1/sessions/{id}/lease", h.guard(ScopeWrite, h.leaseRelease))
	mux.HandleFunc("POST /v1/sessions/{id}/lease/break", h.guard(ScopeAdmin, h.leaseBreak))
	mux.HandleFunc("POST /v1/sessions/{id}/entries", h.guard(ScopeWrite, h.appendEntries))
	mux.HandleFunc("POST /v1/sessions/{id}/commit", h.guard(ScopeWrite, h.commit))
	mux.HandleFunc("PUT /v1/sessions/{id}/head", h.guard(ScopeWrite, h.setHead))
	mux.HandleFunc("PUT /v1/refs/{name...}", h.guard(ScopeRefs, h.updateRef))
	mux.HandleFunc("GET /v1/objects/{space}/{hash}", h.guard(ScopeRead, h.objectGet))
	mux.HandleFunc("PUT /v1/objects/{space}/{hash}", h.guard(ScopeExchange, h.objectPut))
	mux.HandleFunc("POST /v1/objects/missing", h.guard(ScopeExchange, h.missing))
	mux.HandleFunc("POST /v1/sessions/{id}/push", h.guard(ScopeExchange, h.push))
	mux.HandleFunc("POST /v1/sessions/{id}/record", h.guard(ScopeExchange, h.record))
	mux.HandleFunc("POST /v1/sessions/{id}/handover", h.guard(ScopeWrite, h.handover))
	mux.HandleFunc("/v1/", h.guard("", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, problem(CodeUnsupported, "not served", r.Method+" "+r.URL.Path+" is not in what this server serves"))
	}))
	h.mux = mux
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

// Close stops every follower the handler runs, so the follows it serves
// end, and ends every lease it granted, letting the store's holds go.
func (h *Handler) Close() error {
	h.cancel()
	h.wmu.Lock()
	all := make([]*held, 0, len(h.held))
	for _, st := range h.held {
		all = append(all, st)
	}
	h.wmu.Unlock()
	for _, st := range all {
		st.mu.Lock()
		if st.lease != nil {
			h.end(st)
		}
		st.mu.Unlock()
	}
	return nil
}

// Capabilities are what this handler serves of its store.
func (h *Handler) Capabilities() Capabilities {
	_, commit := h.store.(syncer)
	exchange := h.ex != nil && h.reader != nil
	c := Capabilities{
		Reads: agentsession.Format,
		Features: Features{
			Refs:     h.refs != nil,
			Leases:   true,
			Commit:   commit,
			Objects:  exchange,
			Exchange: exchange,
			Marks:    h.ex != nil,
		},
		Limits: Limits{FollowBuffer: h.buffer, FollowReplay: h.replay},
	}
	if exchange {
		c.Limits.UploadGraceMS = h.uploadGrace.Milliseconds()
	}
	return c
}

func (h *Handler) logError(err error) {
	if h.logf != nil && err != nil {
		h.logf(err)
	}
}

// guard authenticates r, checks the principal holds scope (none for the
// empty scope) and puts the principal on the request's context.
func (h *Handler) guard(scope Scope, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var p Principal
		switch {
		case h.insecure:
			p = Principal{Name: "insecure", Scopes: AllScopes}
		case h.auth == nil:
			writeProblem(w, problem(CodeAuth, "no authenticator is configured", ""))
			return
		default:
			var err error
			if p, err = h.auth(r); err != nil {
				writeProblem(w, problem(CodeAuth, "unauthenticated", ""))
				return
			}
		}
		if scope != "" && !p.Has(scope) {
			writeProblem(w, problem(CodeScope, "the principal lacks the scope this needs", string(scope)))
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, p)))
	}
}

func writeProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	if p.Incarnation != "" {
		w.Header().Set("Incarnation", p.Incarnation)
	}
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fail answers err as a problem, logging a store fault.
func (h *Handler) fail(w http.ResponseWriter, err error) {
	p := problemOf(err)
	if p.Code == CodeStoreFault {
		h.logError(err)
	}
	writeProblem(w, p)
}

func (h *Handler) capabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, h.Capabilities())
}

// ndjson streams the lines of a listing, then the end frame, or an
// error frame when the listing cannot be completed.
type ndjson struct {
	w  *bufio.Writer
	rw http.ResponseWriter
}

func startNDJSON(w http.ResponseWriter) *ndjson {
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	return &ndjson{w: bufio.NewWriter(w), rw: w}
}

func (n *ndjson) line(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	n.w.Write(data)
	return n.w.WriteByte('\n')
}

func (n *ndjson) end(extra map[string]any) {
	m := map[string]any{"end": true}
	for k, v := range extra {
		m[k] = v
	}
	_ = n.line(m)
	n.flush()
}

func (n *ndjson) fail(h *Handler, err error) {
	p := problemOf(err)
	if p.Code == CodeStoreFault {
		h.logError(err)
	}
	_ = n.line(map[string]any{"error": p})
	n.flush()
}

func (n *ndjson) flush() {
	_ = n.w.Flush()
	if f, ok := n.rw.(http.Flusher); ok {
		f.Flush()
	}
}

// summaryLine is a session in a listing: never a file path.
type summaryLine struct {
	Header       agentsession.Header `json:"header"`
	Incarnation  string              `json:"incarnation"`
	Name         string              `json:"name,omitempty"`
	SupersededBy string              `json:"superseded_by,omitempty"`
	Size         int64               `json:"size,omitempty"`
	Modified     *time.Time          `json:"modified,omitempty"`
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	f, err := filterOf(r)
	if err != nil {
		writeProblem(w, problem(CodeBadRequest, "bad filter", err.Error()))
		return
	}
	next, stop := iter.Pull2(h.store.List(r.Context(), f))
	defer stop()
	sum, err, ok := next()
	if ok && err != nil {
		h.fail(w, err)
		return
	}
	out := startNDJSON(w)
	for ok {
		if err != nil {
			out.fail(h, err)
			return
		}
		l := summaryLine{Header: sum.Header, Incarnation: incarnation(sum.Header), Name: sum.Name, SupersededBy: sum.SupersededBy, Size: sum.Size}
		if !sum.Modified.IsZero() {
			m := sum.Modified
			l.Modified = &m
		}
		if err := out.line(l); err != nil {
			out.fail(h, err)
			return
		}
		sum, err, ok = next()
	}
	out.end(nil)
}

// filterOf reads a list's filter from its query, RFC 0003's Sessions.
func filterOf(r *http.Request) (agentsession.ListFilter, error) {
	q := r.URL.Query()
	f := agentsession.ListFilter{CWD: q.Get("cwd"), ParentSession: q.Get("parent_session"), Harness: q.Get("harness")}
	var err error
	flag := func(name string) bool {
		v := q.Get(name)
		if v == "" || err != nil {
			return false
		}
		b, e := strconv.ParseBool(v)
		if e != nil {
			err = fmt.Errorf("%s: %w", name, e)
		}
		return b
	}
	f.TopLevel, f.WithNames, f.Current = flag("top_level"), flag("with_names"), flag("current")
	for _, b := range []struct {
		name string
		to   *time.Time
	}{{"after", &f.After}, {"before", &f.Before}} {
		if v := q.Get(b.name); v != "" && err == nil {
			*b.to, err = time.Parse(time.RFC3339Nano, v)
		}
	}
	if v := q.Get("limit"); v != "" && err == nil {
		if f.Limit, err = strconv.Atoi(v); err == nil && f.Limit < 0 {
			err = errors.New("limit is negative")
		}
	}
	for _, x := range q["extra"] {
		name, val, ok := splitExtra(x)
		if !ok {
			return f, fmt.Errorf("extra %q is not name:json", x)
		}
		if f.Extra == nil {
			f.Extra = map[string]json.RawMessage{}
		}
		f.Extra[name] = json.RawMessage(val)
	}
	return f, err
}

// splitExtra splits an extra filter, <name>:<JSON value>. A member's
// name is namespaced with colons of its own, so the value starts after
// the first colon the rest of which is JSON: a colon outside a string
// or an object is never JSON, so no part of the name passes for one.
func splitExtra(x string) (name, val string, ok bool) {
	for i := 0; i < len(x); i++ {
		if x[i] == ':' && i > 0 && json.Valid([]byte(x[i+1:])) {
			return x[:i], x[i+1:], true
		}
	}
	return "", "", false
}

// following returns the session's hub with its first event in, or
// answers the request and returns nil.
func (h *Handler) following(w http.ResponseWriter, r *http.Request) *hub {
	if h.reader == nil || h.follower == nil {
		writeProblem(w, problem(CodeUnsupported, "the store cannot be read without a hold", ""))
		return nil
	}
	hb := h.acquire(r.PathValue("id"))
	if err := hb.wait(r.Context()); err != nil {
		h.release(hb)
		return nil
	}
	return hb
}

// current returns the session's hub caught up to what the store holds
// now, or answers the request and returns nil: what a read of the
// session's state, its path or a fresh follow starts from. The store is
// read first, so a session it does not hold is answered without a
// follower.
func (h *Handler) current(w http.ResponseWriter, r *http.Request) *hub {
	if h.reader == nil || h.follower == nil {
		writeProblem(w, problem(CodeUnsupported, "the store cannot be read without a hold", ""))
		return nil
	}
	held, err := h.reader.Read(r.Context(), r.PathValue("id"))
	if err != nil {
		if r.Context().Err() == nil {
			h.fail(w, err)
		}
		return nil
	}
	hb := h.following(w, r)
	if hb == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), h.catchUpWait)
	defer cancel()
	if err := hb.catchUp(ctx, held); err != nil {
		h.release(hb)
		if r.Context().Err() != nil {
			return nil
		}
		if errors.Is(err, context.DeadlineExceeded) {
			err = fmt.Errorf("remote: the follower of %s did not catch up with the store in %s", hb.id, h.catchUpWait)
		}
		h.fail(w, err)
		return nil
	}
	return hb
}

// checkIncarnation refuses a request whose If-Incarnation names another
// session than the one under the ID.
func checkIncarnation(r *http.Request, inc string) error {
	if want := r.Header.Get("If-Incarnation"); want != "" && want != inc {
		p := problem(CodeSessionReplaced, "the session was replaced", "")
		p.Incarnation = inc
		return p
	}
	return nil
}

// sessionInfo is a session's state: not a read of its entries.
type sessionInfo struct {
	Header      agentsession.Header `json:"header"`
	Head        *string             `json:"head"`
	Length      int                 `json:"length"`
	Incarnation string              `json:"incarnation"`
	Cursor      string              `json:"cursor"`
	// Mark is the server's record mark for the session, record or
	// mirror, where its store keeps one.
	Mark string `json:"mark,omitempty"`
}

func (h *Handler) session(w http.ResponseWriter, r *http.Request) {
	hb := h.current(w, r)
	if hb == nil {
		return
	}
	defer h.release(hb)
	var info sessionInfo
	err := hb.view(func(rep *replica, inc, last string) error {
		if err := checkIncarnation(r, inc); err != nil {
			return err
		}
		info = sessionInfo{Header: rep.sess.Header(), Head: headOut(rep.sess.Leaf()), Length: rep.seq, Incarnation: inc, Cursor: last}
		return nil
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	info.Mark = h.markOf(r.Context(), r.PathValue("id"))
	w.Header().Set("Incarnation", info.Incarnation)
	writeJSON(w, info)
}

// pathLine is one entry of a path: its envelope inline, so the chain
// verifies from the response alone, and its content when asked for.
type pathLine struct {
	Entry       string          `json:"entry"`
	Envelope    json.RawMessage `json:"envelope"`
	Content     json.RawMessage `json:"content"`
	Head        *string         `json:"head,omitempty"`
	Cursor      string          `json:"cursor,omitempty"`
	Incarnation string          `json:"incarnation,omitempty"`
	Contents    string          `json:"contents,omitempty"`
}

func (h *Handler) path(w http.ResponseWriter, r *http.Request) {
	contents := r.URL.Query().Get("contents")
	switch contents {
	case "":
		contents = "all"
	case "all", "none":
	case "since-fold":
		writeProblem(w, problem(CodeUnsupported, "shallow reads are not served", ""))
		return
	default:
		writeProblem(w, problem(CodeBadRequest, "contents is all, since-fold or none", ""))
		return
	}
	hb := h.current(w, r)
	if hb == nil {
		return
	}
	defer h.release(hb)
	var lines []pathLine
	var inc string
	err := hb.view(func(rep *replica, in, last string) error {
		if err := checkIncarnation(r, in); err != nil {
			return err
		}
		inc = in
		leaf := r.URL.Query().Get("leaf")
		if leaf == "" {
			leaf = rep.sess.Leaf()
		}
		var path []agentsession.Entry
		if leaf != "" {
			if path = rep.sess.Path(leaf); path == nil {
				return problem(CodeNoEntry, "no such entry", leaf)
			}
		}
		for i, e := range path {
			l, err := pathLineOf(e, contents == "all")
			if err != nil {
				return err
			}
			if i == 0 {
				l.Head, l.Cursor, l.Incarnation, l.Contents = headOut(rep.sess.Leaf()), last, in, contents
			}
			lines = append(lines, l)
		}
		if len(path) == 0 {
			lines = append(lines, pathLine{Head: headOut(rep.sess.Leaf()), Cursor: last, Incarnation: in, Contents: contents})
		}
		return nil
	})
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Incarnation", inc)
	out := startNDJSON(w)
	for _, l := range lines {
		if l.Entry == "" {
			// An empty path: the state it was read at goes in the end
			// frame.
			out.end(map[string]any{"head": l.Head, "cursor": l.Cursor, "incarnation": l.Incarnation, "contents": l.Contents})
			return
		}
		if err := out.line(l); err != nil {
			out.fail(h, err)
			return
		}
	}
	out.end(nil)
}

// pathLineOf splits an entry's line into its envelope, the members the
// ID is the hash of, and its content, the rest.
func pathLineOf(e agentsession.Entry, withContent bool) (pathLine, error) {
	line, err := agentsession.MarshalEntry(e)
	if err != nil {
		return pathLine{}, err
	}
	id, content, err := agentsession.EntryHashes(line)
	if err != nil {
		return pathLine{}, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(line, &all); err != nil {
		return pathLine{}, err
	}
	env := map[string]json.RawMessage{"content": json.RawMessage(strconv.Quote(content))}
	body := map[string]json.RawMessage{}
	for k, v := range all {
		switch k {
		case "id", "content":
		case "type", "parent", "ts":
			env[k] = v
		case "parents":
			var refs []json.RawMessage
			if json.Unmarshal(v, &refs) == nil && len(refs) > 0 {
				env[k] = v
			}
		default:
			body[k] = v
		}
	}
	if env["parent"] == nil {
		env["parent"] = json.RawMessage("null")
	}
	l := pathLine{Entry: id}
	if l.Envelope, err = canonical(env); err != nil {
		return pathLine{}, err
	}
	if withContent {
		if l.Content, err = canonical(body); err != nil {
			return pathLine{}, err
		}
	} else {
		l.Content = json.RawMessage("null")
	}
	return l, nil
}

func canonical(v any) (json.RawMessage, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(data)
}

func (h *Handler) follow(w http.ResponseWriter, r *http.Request) {
	from := r.Header.Get("Last-Event-ID")
	if from == "" {
		from = r.URL.Query().Get("after")
	}
	// A resume names an event; one that names none of this server's
	// still names no place in the log, which the store answers with a
	// reset, never a refusal.
	var resume agentsession.Cursor
	if from != "" {
		c, n, err := cursorOf(from)
		if err != nil {
			c, n = agentsession.Cursor("\x00"+from), 0
		}
		resume, from = c, eventID(c, n)
	}
	if r.URL.Query().Get("durable") == "true" {
		writeProblem(w, problem(CodeUnsupported, "the store has no committed view", ""))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeProblem(w, problem(CodeStoreFault, "the connection cannot stream", ""))
		return
	}
	// A fresh follow starts from what the store holds now; a resume
	// from the events after its cursor.
	var hb *hub
	if from == "" {
		hb = h.current(w, r)
	} else {
		hb = h.following(w, r)
	}
	if hb == nil {
		return
	}
	var inc string
	if err := hb.view(func(_ *replica, in, _ string) error { inc = in; return checkIncarnation(r, in) }); err != nil {
		h.release(hb)
		h.fail(w, err)
		return
	}
	s, first, ok, err := hb.attach(from)
	if err != nil {
		h.release(hb)
		h.fail(w, err)
		return
	}
	if !ok {
		// A cursor the shared follower no longer holds: a follower of
		// this client's own, from the store, which answers a cursor it
		// cannot resume from with a reset.
		h.release(hb)
		h.followFrom(w, r, flusher, resume)
		return
	}
	defer h.release(hb)
	defer hb.detach(s)
	sse := startSSE(w, flusher, inc)
	for _, ev := range first {
		sse.send(ev)
	}
	sse.flush()
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-s.ch:
			if !ok {
				if s.err != nil {
					sse.send(errorEvent(s.err))
				}
				sse.flush()
				return
			}
			sse.send(ev)
			// Send what else is waiting before flushing.
			for more := true; more; {
				select {
				case ev, ok := <-s.ch:
					if !ok {
						if s.err != nil {
							sse.send(errorEvent(s.err))
						}
						sse.flush()
						return
					}
					sse.send(ev)
				default:
					more = false
				}
			}
			sse.flush()
		case <-tick.C:
			sse.comment()
		case <-r.Context().Done():
			return
		}
	}
}

// followFrom serves a follow from a cursor the shared follower does not
// hold, through a follower of its own. A resumed follow sends nothing
// until the session changes, so the stream starts at once and the
// follow runs beside it; a failure is an error event.
func (h *Handler) followFrom(w http.ResponseWriter, r *http.Request, flusher http.Flusher, c agentsession.Cursor) {
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	evs := make(chan *event)
	go func() {
		defer close(evs)
		send := func(ev *event) bool {
			select {
			case evs <- ev:
				return true
			case <-ctx.Done():
				return false
			}
		}
		enc := newEncoder()
		checked := false
		for change, err := range h.follower.Follow(ctx, r.PathValue("id"), c) {
			if err != nil {
				p := problemOf(err)
				if p.Code == CodeStoreFault {
					h.logError(err)
				}
				send(errorEvent(p))
				return
			}
			if !checked {
				checked = true
				if err := checkIncarnation(r, incarnation(change.Session.Header())); err != nil {
					send(errorEvent(problemOf(err)))
					return
				}
			}
			ev, err := enc.encode(change)
			if err != nil {
				h.logError(err)
				send(errorEvent(problem(CodeStoreFault, "the follow could not be served", "")))
				return
			}
			if ev != nil && !send(ev) {
				return
			}
		}
	}()
	sse := startSSE(w, flusher, "")
	sse.flush()
	tick := time.NewTicker(h.heartbeat)
	defer tick.Stop()
	for {
		select {
		case ev, ok := <-evs:
			if !ok {
				return
			}
			sse.send(ev)
			sse.flush()
		case <-tick.C:
			sse.comment()
		case <-r.Context().Done():
			return
		}
	}
}

// sse writes server-sent events.
type sse struct {
	w *bufio.Writer
	f http.Flusher
}

func startSSE(w http.ResponseWriter, f http.Flusher, inc string) *sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	if inc != "" {
		w.Header().Set("Incarnation", inc)
	}
	w.WriteHeader(http.StatusOK)
	return &sse{w: bufio.NewWriter(w), f: f}
}

func (s *sse) send(ev *event) {
	s.w.WriteString("event: " + ev.name + "\n")
	if ev.id != "" {
		s.w.WriteString("id: " + ev.id + "\n")
	}
	s.w.WriteString("data: ")
	s.w.Write(ev.data)
	s.w.WriteString("\n\n")
}

func (s *sse) comment() {
	s.w.WriteString(": keep-alive\n\n")
	s.flush()
}

func (s *sse) flush() {
	_ = s.w.Flush()
	s.f.Flush()
}

// refLine is a ref in a listing or a resolve.
type refLine struct {
	Name   string  `json:"name"`
	Target *Target `json:"target"`
}

// refUpdateLine is one update in a ref's log.
type refUpdateLine struct {
	Name   string    `json:"name"`
	Old    *Target   `json:"old"`
	New    *Target   `json:"new"`
	Time   time.Time `json:"time"`
	Reason string    `json:"reason,omitempty"`
}

func (h *Handler) listRefs(w http.ResponseWriter, r *http.Request) {
	if h.refs == nil {
		writeProblem(w, problem(CodeUnsupported, "the store keeps no refs", ""))
		return
	}
	prefix := r.URL.Query().Get("prefix")
	if prefix != "" {
		if err := agentsession.ValidRefPrefix(prefix); err != nil {
			writeProblem(w, problem(CodeRefName, "invalid ref prefix", err.Error()))
			return
		}
	}
	streamNDJSON(h, w, h.refs.ListRefs(r.Context(), prefix), func(ref agentsession.Ref) any {
		return refLine{Name: ref.Name, Target: targetOut(ref.Target)}
	})
}

func (h *Handler) ref(w http.ResponseWriter, r *http.Request) {
	if h.refs == nil {
		writeProblem(w, problem(CodeUnsupported, "the store keeps no refs", ""))
		return
	}
	name := r.PathValue("name")
	if err := agentsession.ValidRefName(name); err != nil {
		writeProblem(w, problem(CodeRefName, "invalid ref name", err.Error()))
		return
	}
	switch r.URL.Query().Get("view") {
	case "":
		t, err := h.refs.ResolveRef(r.Context(), name)
		switch {
		case err == nil:
			writeJSON(w, refLine{Name: name, Target: targetOut(t)})
		case !t.IsZero() && errors.Is(err, agentsession.ErrNoSession):
			p := problem(CodeRefDangling, "the ref's session is gone", "")
			p.Target = targetOut(t)
			writeProblem(w, p)
		default:
			h.fail(w, err)
		}
	case "log":
		streamNDJSON(h, w, h.refs.RefLog(r.Context(), name), func(u agentsession.RefUpdate) any {
			return refUpdateLine{Name: u.Name, Old: targetOut(u.Old), New: targetOut(u.New), Time: u.Time, Reason: u.Reason}
		})
	case "follow":
		writeProblem(w, problem(CodeUnsupported, "ref following is not served", ""))
	default:
		writeProblem(w, problem(CodeBadRequest, "view is log or follow", ""))
	}
}

// streamNDJSON writes a listing: a store error before the first line is
// the response's status, one after it an error frame.
func streamNDJSON[T any](h *Handler, w http.ResponseWriter, seq iter.Seq2[T, error], line func(T) any) {
	next, stop := iter.Pull2(seq)
	defer stop()
	v, err, ok := next()
	if ok && err != nil {
		h.fail(w, err)
		return
	}
	out := startNDJSON(w)
	for ok {
		if err != nil {
			out.fail(h, err)
			return
		}
		if err := out.line(line(v)); err != nil {
			out.fail(h, err)
			return
		}
		v, err, ok = next()
	}
	out.end(nil)
}
