package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// exchanger is a store that takes part in RFC 0002's exchange over the
// wire: it holds objects by hash in RFC 0003's spaces, admits a pushed
// closure, and keeps a record mark per session. The cas store is one.
type exchanger interface {
	Object(ctx context.Context, space, hash string) ([]byte, error)
	HasObject(ctx context.Context, space, hash string) (bool, error)
	PutObject(ctx context.Context, space, hash string, data []byte) error
	Receive(ctx context.Context, c cas.Closure, o cas.ReceiveOptions) (cas.Exchange, error)
	MarkOf(ctx context.Context, id string) (mark, head string, err error)
	DeclareRecordAt(ctx context.Context, id, head string) error
	HandOver(ctx context.Context, id, head string) error
}

// Objects are object hashes by RFC 0003's space: the body of a missing
// request and its answer, and a missing_object problem's member.
type Objects struct {
	Entries  []string `json:"entries"`
	Contents []string `json:"contents"`
	Media    []string `json:"media"`
}

func (o *Objects) empty() bool {
	return o == nil || len(o.Entries)+len(o.Contents)+len(o.Media) == 0
}

// each calls fn with every hash and its space.
func (o *Objects) each(fn func(space, hash string)) {
	for _, h := range o.Entries {
		fn(cas.SpaceEntries, h)
	}
	for _, h := range o.Contents {
		fn(cas.SpaceContents, h)
	}
	for _, h := range o.Media {
		fn(cas.SpaceMedia, h)
	}
}

// add puts a hash in its space's list.
func (o *Objects) add(space, hash string) {
	switch space {
	case cas.SpaceEntries:
		o.Entries = append(o.Entries, hash)
	case cas.SpaceContents:
		o.Contents = append(o.Contents, hash)
	case cas.SpaceMedia:
		o.Media = append(o.Media, hash)
	}
}

// objKey names an object in its space.
type objKey struct{ space, hash string }

func validSpace(s string) bool {
	return s == cas.SpaceEntries || s == cas.SpaceContents || s == cas.SpaceMedia
}

// uploads are the objects each principal uploaded for a push, which
// nothing else reaches until a push names them: the uploader may name
// them in a push, and is told it holds them, for the grace the
// capabilities publish.
type uploads struct {
	mu sync.Mutex
	by map[string]map[objKey]time.Time
}

func (u *uploads) put(who string, k objKey) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.by == nil {
		u.by = map[string]map[objKey]time.Time{}
	}
	if u.by[who] == nil {
		u.by[who] = map[objKey]time.Time{}
	}
	u.by[who][k] = time.Now()
}

// has reports whether who uploaded k within grace, and forgets what
// grace has passed for.
func (u *uploads) has(who string, k objKey, grace time.Duration) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	at, ok := u.by[who][k]
	if ok && time.Since(at) > grace {
		delete(u.by[who], k)
		return false
	}
	return ok
}

// reach is which objects the store's sessions hold: an own or prefix
// entry's envelope, the content it names, and the media blobs that
// content names. It is rebuilt when the store's listing changes, which
// an append, a create, a push or a delete does.
type reach struct {
	mu  sync.Mutex
	fp  string
	set map[objKey]bool
}

// reachable reports which of keys some session the caller may read
// holds, or the caller uploaded for a push. Read scope reads every
// session of the store, so a reachable object is one any session
// holds. The answer is built by reading the store's sessions, which a
// server for many sessions would keep an index for instead.
func (h *Handler) reachable(ctx context.Context, who string, keys []objKey) (map[objKey]bool, error) {
	out := map[objKey]bool{}
	var rest []objKey
	for _, k := range keys {
		if h.uploads.has(who, k, h.uploadGrace) {
			out[k] = true
		} else {
			rest = append(rest, k)
		}
	}
	if len(rest) == 0 {
		return out, nil
	}
	set, err := h.reachSet(ctx)
	if err != nil {
		return nil, err
	}
	for _, k := range rest {
		if set[k] {
			out[k] = true
		}
	}
	return out, nil
}

// reachSet is the objects the store's sessions hold, rebuilt when the
// listing's IDs, sizes or modified times have changed.
func (h *Handler) reachSet(ctx context.Context) (map[objKey]bool, error) {
	var b strings.Builder
	var ids []string
	for s, err := range h.store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&b, "%s %d %d\n", s.Header.ID, s.Size, s.Modified.UnixNano())
		ids = append(ids, s.Header.ID)
	}
	fp := b.String()
	h.reach.mu.Lock()
	defer h.reach.mu.Unlock()
	if h.reach.set != nil && h.reach.fp == fp {
		return h.reach.set, nil
	}
	set := map[objKey]bool{}
	for _, id := range ids {
		sess, err := h.reader.Read(ctx, id)
		if errors.Is(err, agentsession.ErrNoSession) {
			continue // deleted since the listing
		}
		if err != nil {
			return nil, err
		}
		for _, e := range sess.Entries() {
			set[objKey{cas.SpaceEntries, e.Base().ID}] = true
			set[objKey{cas.SpaceContents, e.Base().ContentHash()}] = true
			line, err := agentsession.MarshalEntry(e)
			if err != nil {
				return nil, err
			}
			for _, m := range cas.BlobsNamedBy(line) {
				set[objKey{cas.SpaceMedia, m}] = true
			}
		}
	}
	h.reach.fp, h.reach.set = fp, set
	return set, nil
}

func (h *Handler) principal(r *http.Request) Principal {
	p, _ := PrincipalFrom(r.Context())
	return p
}

// objectGet serves an object's canonical bytes, to a caller it is
// reachable by; HEAD answers the same without the body.
func (h *Handler) objectGet(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no objects", ""))
		return
	}
	k := objKey{r.PathValue("space"), r.PathValue("hash")}
	if !validSpace(k.space) || !agentsession.ValidHash(k.hash) {
		h.writeProblem(w, problem(CodeNoObject, "no such object", ""))
		return
	}
	ok, err := h.reachable(r.Context(), h.principal(r).Name, []objKey{k})
	if err != nil {
		h.fail(w, err)
		return
	}
	if !ok[k] {
		h.writeProblem(w, problem(CodeNoObject, "no such object", ""))
		return
	}
	data, err := h.ex.Object(r.Context(), k.space, k.hash)
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

// objectPut takes an object a push will name, checked against its hash.
// One the store holds already is a write that succeeds and says nothing
// of it.
func (h *Handler) objectPut(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no objects", ""))
		return
	}
	k := objKey{r.PathValue("space"), r.PathValue("hash")}
	if !validSpace(k.space) {
		h.writeProblem(w, problem(CodeBadRequest, "no such object space", k.space))
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		h.fail(w, problem(CodeBadRequest, "the body could not be read", err.Error()))
		return
	}
	if len(data) > maxBody {
		h.fail(w, problem(CodeBadRequest, "the object is larger than the server takes", ""))
		return
	}
	if err := h.ex.PutObject(r.Context(), k.space, k.hash, data); err != nil {
		h.fail(w, err)
		return
	}
	h.uploads.put(h.principal(r).Name, k)
	w.WriteHeader(http.StatusNoContent)
}

// missing answers which of the hashes the caller names it cannot reach:
// RFC 0002's negotiation, per caller.
func (h *Handler) missing(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no objects", ""))
		return
	}
	var req Objects
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	var keys []objKey
	req.each(func(space, hash string) { keys = append(keys, objKey{space, hash}) })
	ok, err := h.reachable(r.Context(), h.principal(r).Name, keys)
	if err != nil {
		h.fail(w, err)
		return
	}
	out := Objects{Entries: []string{}, Contents: []string{}, Media: []string{}}
	for _, k := range keys {
		if !ok[k] {
			out.add(k.space, k.hash)
		}
	}
	writeJSON(w, out)
}

// pushRequest is a push: the session's header, its own entries in log
// order and its prefix root first, by hash, the head step, the mark
// the receiver ends with, and the refs it carries.
type pushRequest struct {
	Header  agentsession.Header `json:"header"`
	Entries []string            `json:"entries"`
	Prefix  []string            `json:"prefix"`
	Head    pushHeadStep        `json:"head"`
	Mark    string              `json:"mark"`
	Refs    []PushRef           `json:"refs"`
}

type pushHeadStep struct {
	Expected *string `json:"expected"`
	To       *string `json:"to"`
	Force    bool    `json:"force"`
}

// PushRef is a ref a push carries: the receiver's ref of that name is
// moved from Expected to Next by compare-and-swap once the entries and
// the head are done. Next names the pushed session.
type PushRef struct {
	Name     string  `json:"name"`
	Expected *Target `json:"expected"`
	Next     *Target `json:"next"`
}

// PushResult is what a push did at the server.
type PushResult struct {
	// Admitted is how many of the session's own entries the server took
	// in; the rest it held. Created is set when it lacked the session.
	Admitted int  `json:"admitted"`
	Created  bool `json:"created"`
	// Head is the head step.
	Head PushHead `json:"head"`
	// Mark is the server's mark for the session afterwards.
	Mark string `json:"mark"`
	// Refs is what became of each ref, in order; nil when a handover
	// failed and refs were not attempted.
	Refs []PushRefResult `json:"refs"`
}

// PushHead is a push's head step: whether the head moved, whether a
// force moved one that was not the expected head, the head now, and why
// it did not move.
type PushHead struct {
	Moved   bool    `json:"moved"`
	Forced  bool    `json:"forced"`
	Current *string `json:"current"`
	Why     string  `json:"why,omitempty"`
}

// PushRefResult is what became of one pushed ref.
type PushRefResult struct {
	Name    string  `json:"name"`
	Moved   bool    `json:"moved"`
	Current *Target `json:"current"`
	Why     string  `json:"why,omitempty"`
}

func resultOf(x cas.Exchange, mark string) *PushResult {
	return &PushResult{Admitted: x.Admitted, Created: x.Created, Mark: mark,
		Head: PushHead{Moved: x.HeadMoved, Forced: x.Forced, Current: headOut(x.Head), Why: x.Why}}
}

// push admits a session's closure from the record, RFC 0002's push over
// the wire. It is not leased: it takes the session's per-session lock,
// so it is ordered against a lease holder's writes, and presents no
// token.
func (h *Handler) push(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store does not exchange", ""))
		return
	}
	id := r.PathValue("id")
	var req pushRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	switch {
	case req.Header.ID != id:
		h.fail(w, problem(CodeBadRequest, "the header names another session", req.Header.ID))
		return
	case req.Mark != cas.MarkMirror && req.Mark != cas.MarkRecord:
		h.fail(w, problem(CodeBadRequest, "mark is mirror or record", req.Mark))
		return
	}
	// The header is held to the rules an import holds a file's to,
	// before any object is looked for: a format the server does not read
	// is format, and a header RFC 0001 calls malformed is bad_entry, as
	// RFC 0003's row has it, with the reason redacted_from for that
	// member's rule.
	if err := req.Header.Validate(); err != nil {
		p := problemOf(err)
		if p.Code == CodeStoreFault {
			p = problem(CodeBadEntry, "the header is malformed", err.Error())
			p.Reason = "header"
		}
		h.fail(w, p)
		return
	}
	who := h.principal(r)
	if len(req.Refs) > 0 && !who.Has(ScopeRefs) {
		h.writeProblem(w, problem(CodeScope, "a push's refs need the refs scope", string(ScopeRefs)))
		return
	}
	for _, ref := range req.Refs {
		if err := agentsession.ValidRefName(ref.Name); err != nil {
			h.fail(w, problem(CodeRefName, "invalid ref name", err.Error()))
			return
		}
		if ref.Next == nil || ref.Next.Session != id {
			h.fail(w, problem(CodeBadRefTarget, "a pushed ref names the pushed session", ref.Name))
			return
		}
	}
	if h.refs == nil && len(req.Refs) > 0 {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no refs", ""))
		return
	}
	st := h.state(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	c, missing, err := h.closure(r.Context(), who.Name, req)
	if err != nil {
		h.fail(w, err)
		return
	}
	if !missing.empty() {
		p := problem(CodeMissingObject, "the push names objects the server cannot find", "")
		p.Missing = missing
		h.fail(w, p)
		return
	}
	x, err := h.ex.Receive(r.Context(), c, cas.ReceiveOptions{Expected: headIn(req.Head.Expected), Force: req.Head.Force, Handover: req.Mark == cas.MarkRecord})
	defer h.settle(r.Context(), st)
	mark, _, _ := h.ex.MarkOf(r.Context(), id)
	if errors.Is(err, cas.ErrHeadMoved) {
		p := problem(CodeHeadMoved, "a handover whose head does not move is refused", "")
		p.Head = headOut(x.Head)
		if x.Admitted > 0 || x.Head != "" {
			p.Push = resultOf(x, mark)
		}
		h.fail(w, p)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	res := resultOf(x, mark)
	res.Refs = []PushRefResult{}
	for _, ref := range req.Refs {
		rr, err := h.pushRef(r.Context(), ref)
		if err != nil {
			// Committed, and a ref could not be moved for a reason the
			// pusher cannot fix: a store fault.
			h.fail(w, err)
			return
		}
		res.Refs = append(res.Refs, rr)
	}
	w.Header().Set("Incarnation", incarnation(req.Header))
	writeJSON(w, res)
}

// pushRef moves one pushed ref by compare-and-swap. A refusal the ref's
// name, expected value or pinned entry explains is the result's; a store
// fault is an error.
func (h *Handler) pushRef(ctx context.Context, ref PushRef) (PushRefResult, error) {
	out := PushRefResult{Name: ref.Name}
	err := h.refs.UpdateRef(ctx, ref.Name, ref.Expected.lib(), ref.Next.lib(), "exchange: push")
	var moved *agentsession.RefMovedError
	switch {
	case err == nil:
		out.Moved = true
	case errors.As(err, &moved):
		out.Current = targetOut(moved.Current)
		out.Why = fmt.Sprintf("the server's ref holds %s, not the expected %s", orNone(moved.Current.String()), orNone(ref.Expected.lib().String()))
	case errors.Is(err, agentsession.ErrRefMoved):
		out.Why = err.Error()
	case errors.Is(err, agentsession.ErrRefName), errors.Is(err, agentsession.ErrNoEntry), errors.Is(err, agentsession.ErrNoSession):
		out.Why = err.Error()
	default:
		return out, err
	}
	return out, nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// closure builds the closure a push names from the objects the caller
// may use: those a session it may read holds, and those it uploaded.
// The objects it cannot find are returned, by space; nothing is
// admitted while any is missing.
func (h *Handler) closure(ctx context.Context, who string, req pushRequest) (cas.Closure, *Objects, error) {
	missing := &Objects{}
	c := cas.Closure{Header: req.Header, Head: headIn(req.Head.To), Blobs: map[string][]byte{}}
	envs := map[string][]byte{}
	var keys []objKey
	for _, id := range append(append([]string(nil), req.Prefix...), req.Entries...) {
		keys = append(keys, objKey{cas.SpaceEntries, id})
	}
	ok, err := h.reachable(ctx, who, keys)
	if err != nil {
		return c, nil, err
	}
	var contents []objKey
	for _, k := range keys {
		if !ok[k] {
			missing.add(k.space, k.hash)
			continue
		}
		env, err := h.objectFor(ctx, k)
		if err != nil {
			return c, nil, err
		}
		if env == nil {
			missing.add(k.space, k.hash)
			continue
		}
		ch, okc := cas.ContentOf(env)
		if !okc {
			return c, nil, problem(CodeBadEntry, "an envelope names no content", k.hash)
		}
		envs[k.hash] = env
		contents = append(contents, objKey{cas.SpaceContents, ch})
	}
	if ok, err = h.reachable(ctx, who, contents); err != nil {
		return c, nil, err
	}
	bodies := map[string][]byte{}
	var media []objKey
	for _, k := range contents {
		if _, done := bodies[k.hash]; done {
			continue
		}
		body, err := h.objectIf(ctx, k, ok[k])
		if err != nil {
			return c, nil, err
		}
		if body == nil {
			missing.add(k.space, k.hash)
			bodies[k.hash] = nil
			continue
		}
		bodies[k.hash] = body
		for _, m := range cas.BlobsNamedBy(body) {
			media = append(media, objKey{cas.SpaceMedia, m})
		}
	}
	if ok, err = h.reachable(ctx, who, media); err != nil {
		return c, nil, err
	}
	for _, k := range media {
		if _, done := c.Blobs[k.hash]; done {
			continue
		}
		blob, err := h.objectIf(ctx, k, ok[k])
		if err != nil {
			return c, nil, err
		}
		if blob == nil {
			missing.add(k.space, k.hash)
			continue
		}
		c.Blobs[k.hash] = blob
	}
	if !missing.empty() {
		return c, missing, nil
	}
	// The closure is read as a session file is: the header, then each
	// line, the prefix root first and the own entries in log order, so
	// every entry is hashed and decoded as the format reads it.
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, agentsession.New(req.Header)); err != nil {
		return c, nil, problem(CodeBadRequest, "the header does not encode", err.Error())
	}
	for _, id := range append(append([]string(nil), req.Prefix...), req.Entries...) {
		env := envs[id]
		ch, _ := cas.ContentOf(env)
		line, err := cas.JoinLine(id, env, bodies[ch])
		if err != nil {
			return c, nil, err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	sess, err := agentsession.Read(&buf)
	if err != nil {
		return c, nil, err
	}
	if sess.Truncated() != nil {
		return c, nil, problem(CodeBadEntry, "a line of the closure does not parse", "")
	}
	pick := func(ids []string) ([]agentsession.Entry, error) {
		out := make([]agentsession.Entry, 0, len(ids))
		for _, id := range ids {
			e, ok := sess.Entry(id)
			if !ok {
				return nil, problem(CodeBadEntry, "the closure names an entry twice or out of place", id)
			}
			out = append(out, e)
		}
		return out, nil
	}
	if c.Prefix, err = pick(req.Prefix); err != nil {
		return c, nil, err
	}
	if c.Own, err = pick(req.Entries); err != nil {
		return c, nil, err
	}
	return c, nil, nil
}

// objectFor reads an object the caller may use, nil when the store does
// not hold it after all (swept since it was uploaded).
func (h *Handler) objectFor(ctx context.Context, k objKey) ([]byte, error) {
	data, err := h.ex.Object(ctx, k.space, k.hash)
	if errors.Is(err, cas.ErrNoObject) {
		return nil, nil
	}
	return data, err
}

// objectIf is objectFor for an object the caller may use, and nil for
// one it may not.
func (h *Handler) objectIf(ctx context.Context, k objKey, may bool) ([]byte, error) {
	if !may {
		return nil, nil
	}
	return h.objectFor(ctx, k)
}

// settle leaves the store's hold as the lease table says, after an
// exchange that took it: the leased session is the store's again, as
// the push rebuilt it, and an unleased one is let go, so another process
// may open it. The caller holds st.mu.
func (h *Handler) settle(ctx context.Context, st *held) {
	if st.lease != nil {
		if sess, err := h.store.Open(ctx, st.id); err == nil {
			st.sess = sess
		} else {
			h.logError(err)
		}
		return
	}
	h.releaseHold(st.id)
}

// recordRequest declares this server the record for a session it holds
// as a mirror.
type recordRequest struct {
	Basis string  `json:"basis"`
	Head  *string `json:"head"`
}

// markResponse is a mark change's answer.
type markResponse struct {
	Mark string  `json:"mark"`
	Head *string `json:"head"`
}

// record is RFC 0002's declare-record, guarded by the head the caller
// believes the server holds. A mirror cannot be leased, so it presents
// no token; it takes the per-session lock.
func (h *Handler) record(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no record mark", ""))
		return
	}
	var req recordRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	if req.Basis != "record_deleted" && req.Basis != "sole_copy" {
		h.fail(w, problem(CodeBadRequest, "basis is record_deleted or sole_copy", req.Basis))
		return
	}
	id := r.PathValue("id")
	st := h.state(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	err := h.ex.DeclareRecordAt(r.Context(), id, headIn(req.Head))
	defer h.settle(r.Context(), st)
	if err != nil {
		h.fail(w, h.headProblem(r.Context(), id, err))
		return
	}
	writeJSON(w, markResponse{Mark: cas.MarkRecord, Head: req.Head})
}

// headProblem is the problem for a mark change's error, with the head on
// head_moved.
func (h *Handler) headProblem(ctx context.Context, id string, err error) error {
	if !errors.Is(err, cas.ErrHeadMoved) {
		return err
	}
	p := problem(CodeHeadMoved, "the head is not the expected entry", "")
	if _, head, herr := h.ex.MarkOf(ctx, id); herr == nil {
		p.Head = headOut(head)
	}
	return p
}

// handoverRequest hands the session to the client.
type handoverRequest struct {
	To   string  `json:"to"`
	Head *string `json:"head"`
}

// handover makes this server a mirror of a session it is the record
// for, when its head is the one the client fetched: the server's side
// of handing the session back to a client store. A mark change is a
// write, so it carries the lease's token; the lease ends with it, since
// a mirror cannot be leased.
func (h *Handler) handover(w http.ResponseWriter, r *http.Request) {
	if h.ex == nil {
		h.writeProblem(w, problem(CodeUnsupported, "the store keeps no record mark", ""))
		return
	}
	var req handoverRequest
	if err := readJSON(r, &req); err != nil {
		h.fail(w, err)
		return
	}
	if req.To != "client" {
		h.fail(w, problem(CodeBadRequest, "a handover is to the client", req.To))
		return
	}
	id := r.PathValue("id")
	st := h.state(id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := fenced(r.Header, st); err != nil {
		h.fail(w, err)
		return
	}
	if err := h.ex.HandOver(r.Context(), id, headIn(req.Head)); err != nil {
		h.fail(w, h.headProblem(r.Context(), id, err))
		return
	}
	h.end(st)
	writeJSON(w, markResponse{Mark: cas.MarkMirror, Head: req.Head})
}

// markOf is the session's record mark where the store keeps one, "" where
// it does not.
func (h *Handler) markOf(ctx context.Context, id string) string {
	if h.ex == nil {
		return ""
	}
	m, _, err := h.ex.MarkOf(ctx, id)
	if err != nil {
		return ""
	}
	return m
}
