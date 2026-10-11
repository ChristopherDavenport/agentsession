package remote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// maxBody bounds a write's request body.
const maxBody = 64 << 20

// syncer is a store with a commit of its own (RFC 0002's): the cas
// store's Sync.
type syncer interface {
	Sync(ctx context.Context) error
}

// headSetter is a store that keeps a durable head and moves it by
// compare-and-swap: the cas store's SetHead.
type headSetter interface {
	SetHead(ctx context.Context, sessionID, expected, to string) error
}

// leaseRequest is an acquire's body.
type leaseRequest struct {
	Holder string `json:"holder"`
	TTLms  int64  `json:"ttl_ms"`
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return problem(CodeBadRequest, "the body does not parse", err.Error())
	}
	return nil
}

func (h *Handler) leaseAcquire(w http.ResponseWriter, r *http.Request) {
	var req leaseRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	l, err := h.take(r.Context(), st, req.Holder, h.ttlOf(req.TTLms))
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Incarnation", incarnation(st.sess.Header()))
	writeJSON(w, leaseOf(l, st.sess))
}

func (h *Handler) leaseRenew(w http.ResponseWriter, r *http.Request) {
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	l := st.lease
	if !l.current() || r.Header.Get("Lease-ID") != l.id {
		h.fail(w, problem(CodeLeaseLost, "the lease is not the session's current one", ""))
		return
	}
	var req leaseRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	ttl := h.ttlOf(req.TTLms)
	l.expires = time.Now().Add(ttl)
	l.timer.Reset(ttl)
	writeJSON(w, leaseOf(l, st.sess))
}

func (h *Handler) leaseRelease(w http.ResponseWriter, r *http.Request) {
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease == nil || r.Header.Get("Lease-ID") != st.lease.id {
		h.fail(w, problem(CodeLeaseLost, "the lease is not the session's current one", ""))
		return
	}
	h.end(st)
	w.WriteHeader(http.StatusNoContent)
}

// leaseBreak ends whatever lease the session has, an admin's operation.
// The token is advanced, so the broken holder's next write is refused
// and the next lease's token is greater than any it had.
func (h *Handler) leaseBreak(w http.ResponseWriter, r *http.Request) {
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.lease != nil {
		h.end(st)
	}
	h.nextToken(st)
	w.WriteHeader(http.StatusNoContent)
}

// createResponse is what a create answers: the session as a snapshot
// carries it, and the lease when one was asked for.
type createResponse struct {
	Session     *snapshotData       `json:"session"`
	Header      agentsession.Header `json:"header"`
	Incarnation string              `json:"incarnation"`
	Lease       *leaseOut           `json:"lease,omitempty"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	key := idemKey(r, "create")
	if key != "" {
		if k, ok := h.idem.get(key, h.maxLeaseTTL); ok {
			replay(w, k)
			return
		}
	}
	var hdr agentsession.Header
	if err := readJSON(r, &hdr); err != nil {
		h.fail(w, err)
		return
	}
	var ttl time.Duration
	if v := r.URL.Query().Get("lease_ttl_ms"); v != "" {
		ms, err := strconv.ParseInt(v, 10, 64)
		if err != nil || ms <= 0 {
			h.fail(w, problem(CodeBadRequest, "lease_ttl_ms is a positive number", ""))
			return
		}
		ttl = h.ttlOf(ms)
	}
	// The ID is made here when the header has none, so the session's
	// lock is held from before it exists.
	complete := hdr.ID != "" && !hdr.CreatedAt.IsZero()
	if hdr.ID == "" {
		hdr.ID = agentsession.NewSessionID()
	}
	st := h.state(hdr.ID)
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := h.store.Create(r.Context(), hdr)
	fresh := err == nil
	if errors.Is(err, agentsession.ErrSessionExists) && complete {
		// A retry of a create whose answer was lost: the same header
		// is the same session.
		sess, err = h.sameSession(r.Context(), hdr)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	resp := createResponse{Header: sess.Header(), Incarnation: incarnation(sess.Header())}
	if resp.Session, err = snapshotOf(sess); err != nil {
		h.fail(w, err)
		return
	}
	if ttl > 0 {
		if st.lease.current() {
			// A retry under the same header while the first create's
			// lease holds: the lease is its holder's, not handed out
			// again.
			p := problem(CodeSessionLocked, "the session is leased", "")
			p.Holder = st.lease.holder
			h.fail(w, p)
			return
		}
		if st.lease != nil {
			h.end(st)
		}
		if !fresh {
			// The retry read the session; the lease holds the store's.
			if sess, err = h.store.Open(r.Context(), hdr.ID); err != nil {
				h.fail(w, err)
				return
			}
		}
		l, err := h.grant(st, sess, r.URL.Query().Get("holder"), ttl)
		if err != nil {
			h.fail(w, err)
			return
		}
		resp.Lease = leaseOf(l, sess)
	} else {
		h.releaseHold(hdr.ID)
	}
	h.answer(w, key, http.StatusCreated, resp, resp.Incarnation)
}

// sameSession returns the session under hdr's ID when its header is
// hdr, compared without the format, as RFC 0003's create retries ask.
func (h *Handler) sameSession(ctx context.Context, hdr agentsession.Header) (*agentsession.Session, error) {
	exists := fmt.Errorf("%w: %s", agentsession.ErrSessionExists, hdr.ID)
	if h.reader == nil {
		return nil, exists
	}
	held, err := h.reader.Read(ctx, hdr.ID)
	if err != nil {
		return nil, exists
	}
	want := hdr
	if want.Payload == "" {
		want.Payload = agentsession.Payload
	}
	a, aerr := agentsession.HeaderIdent(held.Header())
	b, berr := agentsession.HeaderIdent(want)
	if aerr != nil || berr != nil || a != b {
		return nil, exists
	}
	return held, nil
}

// answer writes a JSON response, and remembers it under key for a
// retry.
func (h *Handler) answer(w http.ResponseWriter, key string, status int, v any, inc string) {
	body, err := json.Marshal(v)
	if err != nil {
		h.fail(w, err)
		return
	}
	body = append(body, '\n')
	if key != "" {
		h.idem.put(key, keyed{status: status, body: body}, h.maxLeaseTTL)
	}
	if inc != "" {
		w.Header().Set("Incarnation", inc)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(body)
}

func replay(w http.ResponseWriter, k keyed) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(k.status)
	w.Write(k.body)
}

// idemKey is a request's Idempotency-Key, scoped to its principal and
// what it does.
func idemKey(r *http.Request, what string) string {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		return ""
	}
	p, _ := PrincipalFrom(r.Context())
	return p.Name + "\x00" + what + "\x00" + r.PathValue("id") + "\x00" + k
}

func (h *Handler) deleteSession(w http.ResponseWriter, r *http.Request) {
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fenced(r.Header, st); err != nil {
		h.fail(w, err)
		return
	}
	err := h.store.Delete(r.Context(), st.id)
	if err != nil {
		h.fail(w, err)
		return
	}
	h.end(st)
	w.WriteHeader(http.StatusNoContent)
}

// appended is one applied entry of an append's response.
type appended struct {
	ID         string   `json:"id"`
	Outcome    string   `json:"outcome"`
	Durable    bool     `json:"durable"`
	Unresolved []string `json:"unresolved"`
}

// stopped is where an append's batch stopped: the refused line and why.
type stopped struct {
	Index   int      `json:"index"`
	Problem *Problem `json:"problem"`
}

// appendResponse is an append's answer.
type appendResponse struct {
	Results []appended `json:"results"`
	// Head is the session's head after the batch, null for none.
	Head    *string  `json:"head"`
	Stopped *stopped `json:"stopped,omitempty"`
}

var outcomes = map[agentsession.Outcome]string{
	agentsession.Continued:    "continued",
	agentsession.Branched:     "branched",
	agentsession.Held:         "held",
	agentsession.LeafMoved:    "leaf_moved",
	agentsession.LeafNotMoved: "leaf_not_moved",
}

func outcomeNamed(name string) (agentsession.Outcome, bool) {
	for o, n := range outcomes {
		if n == name {
			return o, true
		}
	}
	return 0, false
}

// checkFormat refuses a write whose Session-Format the server does not
// read, or is earlier than the session's, RFC 0002's rule that an
// append carries its writer's format.
func checkFormat(r *http.Request, sess *agentsession.Session) error {
	f := r.Header.Get("Session-Format")
	if f == "" {
		return nil
	}
	maj, min, err := agentsession.ParseFormat(f)
	if err != nil {
		return err
	}
	ourMaj, ourMin, _ := agentsession.ParseFormat(agentsession.Format)
	if maj != ourMaj || min > ourMin {
		return fmt.Errorf("%w: %s", agentsession.ErrUnsupportedFormat, f)
	}
	if sMaj, sMin, err := agentsession.ParseFormat(sess.Header().Format); err == nil && (maj < sMaj || maj == sMaj && min < sMin) {
		return fmt.Errorf("%w: %s is earlier than the session's %s", agentsession.ErrUnsupportedFormat, f, sess.Header().Format)
	}
	return nil
}

func (h *Handler) appendEntries(w http.ResponseWriter, r *http.Request) {
	commit := r.URL.Query().Get("commit") == "true"
	sy, canCommit := h.store.(syncer)
	if commit && !canCommit {
		h.fail(w, problem(CodeUnsupported, "the store has no commit of its own", ""))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		h.fail(w, problem(CodeBadRequest, "the body could not be read", ""))
		return
	}
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	key := idemKey(r, "append")
	if key != "" {
		if k, ok := h.idem.get(key, h.maxLeaseTTL); ok {
			replay(w, k)
			return
		}
	}
	if err := fenced(r.Header, st); err != nil {
		h.fail(w, err)
		return
	}
	if err := checkFormat(r, st.sess); err != nil {
		h.fail(w, err)
		return
	}
	var resp appendResponse
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64<<10), maxBody)
	i := 0
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		res, err := h.appendLine(r.Context(), st, line)
		if err != nil {
			p := problemOf(err)
			if p.Code == CodeStoreFault {
				h.logError(err)
			}
			if len(resp.Results) == 0 {
				// The first line refused: the request's own refusal.
				h.fail(w, p)
				return
			}
			resp.Stopped = &stopped{Index: i, Problem: p}
			break
		}
		resp.Results = append(resp.Results, res)
		i++
	}
	if commit && len(resp.Results) > 0 {
		if err := sy.Sync(r.Context()); err != nil {
			h.fail(w, err)
			return
		}
		for i := range resp.Results {
			resp.Results[i].Durable = true
		}
	}
	resp.Head = headOut(st.sess.Leaf())
	h.answer(w, key, http.StatusOK, resp, incarnation(st.sess.Header()))
}

// appendLine applies one entry line under st's lease. A complete line,
// its ID set, is taken as written, parent included: a null parent is a
// root, and the session's head is put to none first, as a writer that
// appends a root has reset its leaf.
func (h *Handler) appendLine(ctx context.Context, st *held, line []byte) (appended, error) {
	line, err := fillLine(line, st.sess.Leaf())
	if err != nil {
		p := problem(CodeBadEntry, "the line is not an entry", err.Error())
		p.Reason = "parse"
		return appended{}, p
	}
	e, err := agentsession.UnmarshalEntry(line)
	if err != nil {
		p := problem(CodeBadEntry, "the line is not an entry", err.Error())
		p.Reason = "parse"
		return appended{}, p
	}
	b := e.Base()
	before := st.sess.Leaf()
	if b.ID != "" && b.Parent == "" && before != "" {
		st.sess.ResetLeaf()
	}
	res, err := h.write(ctx, st, e)
	if err != nil {
		if before != "" && st.sess.Leaf() != before {
			_ = st.sess.Branch(before)
		}
		return appended{}, err
	}
	out := appended{ID: res.ID, Outcome: outcomes[res.Outcome], Durable: res.Durable, Unresolved: res.Unresolved}
	if out.Unresolved == nil {
		out.Unresolved = []string{}
	}
	return out, nil
}

// fillLine completes a line a client left incomplete, its id missing or
// empty, as a store fills an entry it is handed (Session.Prepare): the
// time now for a missing or zero ts, the session's head for a missing
// or null parent, and the ID the line then hashes to. Only a complete
// line says a null parent is a root. A complete line is returned as it
// is.
func fillLine(line []byte, head string) ([]byte, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(line, &all); err != nil {
		return nil, err
	}
	if !empty(all["id"]) {
		return line, nil
	}
	if empty(all["ts"]) || string(all["ts"]) == `"0001-01-01T00:00:00Z"` {
		all["ts"], _ = json.Marshal(time.Now().UTC().Format(time.RFC3339Nano))
	}
	if empty(all["parent"]) {
		all["parent"] = json.RawMessage("null")
		if head != "" {
			all["parent"], _ = json.Marshal(head)
		}
	}
	delete(all, "id")
	filled, err := json.Marshal(all)
	if err != nil {
		return nil, err
	}
	id, _, err := agentsession.EntryHashes(filled)
	if err != nil {
		return nil, err
	}
	all["id"], _ = json.Marshal(id)
	return json.Marshal(all)
}

// empty reports whether a member is missing, null or the empty string.
func empty(v json.RawMessage) bool {
	s := string(bytes.TrimSpace(v))
	return s == "" || s == "null" || s == `""`
}

// write appends e through the store, with what the append did: the
// store's own report when it makes one ([agentsession.Writer]), and
// otherwise the outcome the session's Prepare gives under the lock.
func (h *Handler) write(ctx context.Context, st *held, e agentsession.Entry) (agentsession.Result, error) {
	if wr, ok := h.store.(agentsession.Writer); ok {
		return wr.Write(ctx, st.id, e)
	}
	pre, err := st.sess.Prepare(e)
	if err != nil {
		return agentsession.Result{}, err
	}
	id, err := h.store.Append(ctx, st.id, e)
	if err != nil {
		return agentsession.Result{}, err
	}
	pre.ID = id
	return pre, nil
}

func (h *Handler) commit(w http.ResponseWriter, r *http.Request) {
	sy, ok := h.store.(syncer)
	if !ok {
		h.fail(w, problem(CodeUnsupported, "the store has no commit of its own", ""))
		return
	}
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fenced(r.Header, st); err != nil {
		h.fail(w, err)
		return
	}
	if err := sy.Sync(r.Context()); err != nil {
		h.fail(w, err)
		return
	}
	out := map[string]any{"durable": true}
	if h.lr != nil {
		// The cursor through which everything is committed: the log's end,
		// which this commit's sync record closes.
		c, err := h.lr.LogCursor(r.Context(), r.PathValue("id"))
		if err != nil {
			h.fail(w, err)
			return
		}
		out["cursor"] = wireCursor(c)
	}
	writeJSON(w, out)
}

// headRequest is a head compare-and-swap.
type headRequest struct {
	Expected *string `json:"expected"`
	To       *string `json:"to"`
	Durable  bool    `json:"durable"`
}

func (h *Handler) setHead(w http.ResponseWriter, r *http.Request) {
	var req headRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	if req.To == nil || *req.To == "" {
		h.fail(w, problem(CodeHeadTarget, "a head move names its target", ""))
		return
	}
	hs, durable := h.store.(headSetter)
	if req.Durable && !durable {
		h.fail(w, problem(CodeUnsupported, "the store has no commit of its own", ""))
		return
	}
	st := h.state(r.PathValue("id"))
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fenced(r.Header, st); err != nil {
		h.fail(w, err)
		return
	}
	cur, exp, to := st.sess.Leaf(), headIn(req.Expected), *req.To
	if cur != exp {
		p := problem(CodeHeadMoved, "the head is not the expected entry", "")
		p.Head = headOut(cur)
		h.fail(w, p)
		return
	}
	e, ok := st.sess.Entry(to)
	if base := st.sess.Header().Base; !ok || base != "" && to != base && st.sess.Prefix(to) {
		h.fail(w, problem(CodeNoEntry, "the head may rest on the base or an own entry", to))
		return
	}
	if l, isLabel := e.(*agentsession.LabelEntry); isLabel && l.Label != nil && *l.Label == agentsession.LeafLabel {
		h.fail(w, problem(CodeHeadTarget, "the head never rests on a leaf label", to))
		return
	}
	var err error
	if durable {
		err = hs.SetHead(r.Context(), st.id, exp, to)
	} else {
		err = st.sess.Branch(to)
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"head": to, "durable": durable})
}

// refUpdate is a ref compare-and-swap.
type refUpdate struct {
	Expected *Target `json:"expected"`
	Next     *Target `json:"next"`
	Reason   string  `json:"reason"`
}

func (h *Handler) updateRef(w http.ResponseWriter, r *http.Request) {
	if h.refs == nil {
		writeProblem(w, problem(CodeUnsupported, "the store keeps no refs", ""))
		return
	}
	name := r.PathValue("name")
	if err := agentsession.ValidRefName(name); err != nil {
		writeProblem(w, problem(CodeRefName, "invalid ref name", err.Error()))
		return
	}
	var req refUpdate
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	next := req.Next.lib()
	err := h.refs.UpdateRef(r.Context(), name, req.Expected.lib(), next, req.Reason)
	switch {
	case err == nil:
		writeJSON(w, refLine{Name: name, Target: targetOut(next)})
	case errors.Is(err, agentsession.ErrRefMoved):
		p := problem(CodeRefMoved, "the ref is not at the expected target", "")
		var moved *agentsession.RefMovedError
		if errors.As(err, &moved) {
			p.Target = targetOut(moved.Current)
		} else if t, rerr := h.refs.ResolveRef(r.Context(), name); rerr == nil || !t.IsZero() {
			p.Target = targetOut(t)
		}
		h.fail(w, p)
	case errors.Is(err, agentsession.ErrNoSession):
		// A same-target update of a dangling ref is refused as dangling,
		// so a client need not guess which refusal it got.
		if t, rerr := h.refs.ResolveRef(r.Context(), name); rerr != nil && !t.IsZero() && t == next {
			p := problem(CodeRefDangling, "the ref's session is gone", "")
			p.Target = targetOut(t)
			h.fail(w, p)
			return
		}
		h.fail(w, err)
	default:
		h.fail(w, err)
	}
}
