package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/openresponses"
)

// maxSearchBody bounds a search's request body: a query and a filter.
const maxSearchBody = 1 << 20

// searchRequest is a search's body (RFC 0003, Search).
type searchRequest struct {
	Query   string        `json:"query,omitempty"`
	Filter  *searchFilter `json:"filter,omitempty"`
	Exclude []string      `json:"exclude,omitempty"`
	// OmitRedacted is omit_redacted: true leaves out redacted sessions.
	OmitRedacted bool       `json:"omit_redacted,omitempty"`
	Roles        []string   `json:"roles,omitempty"`
	After        *time.Time `json:"after,omitempty"`
	Before       *time.Time `json:"before,omitempty"`
	Limit        int        `json:"limit,omitempty"`
}

// searchFilter is the listing's filter in a search's body, its members
// named as the listing's query parameters are.
type searchFilter struct {
	CWD           string                     `json:"cwd,omitempty"`
	ParentSession string                     `json:"parent_session,omitempty"`
	Harness       string                     `json:"harness,omitempty"`
	Extra         map[string]json.RawMessage `json:"extra,omitempty"`
	TopLevel      bool                       `json:"top_level,omitempty"`
	After         *time.Time                 `json:"after,omitempty"`
	Before        *time.Time                 `json:"before,omitempty"`
	Limit         int                        `json:"limit,omitempty"`
	Current       bool                       `json:"current,omitempty"`
}

// hitLine is a hit in a search's stream.
type hitLine struct {
	Session string    `json:"session"`
	Entry   string    `json:"entry"`
	TS      time.Time `json:"ts"`
	Role    string    `json:"role"`
	Snippet string    `json:"snippet"`
}

func timeOf(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

func timeIn(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// query is the library's query the request asks.
func (req searchRequest) query() (agentsession.Query, error) {
	q := agentsession.Query{Text: req.Query, Exclude: req.Exclude, OmitRedacted: req.OmitRedacted,
		After: timeOf(req.After), Before: timeOf(req.Before), Limit: req.Limit}
	if req.Limit < 0 {
		return q, errors.New("limit is negative")
	}
	for _, r := range req.Roles {
		q.Roles = append(q.Roles, openresponses.Role(r))
	}
	if f := req.Filter; f != nil {
		if f.Limit < 0 {
			return q, errors.New("filter limit is negative")
		}
		for name, v := range f.Extra {
			if !json.Valid(v) {
				return q, fmt.Errorf("filter extra %q is not JSON", name)
			}
		}
		q.Filter = agentsession.ListFilter{CWD: f.CWD, ParentSession: f.ParentSession, Harness: f.Harness, Extra: f.Extra,
			TopLevel: f.TopLevel, After: timeOf(f.After), Before: timeOf(f.Before), Limit: f.Limit, Current: f.Current}
	}
	return q, nil
}

// searchRequestOf is the request that asks q, but for its Readable,
// which the server answers for itself.
func searchRequestOf(q agentsession.Query) searchRequest {
	req := searchRequest{Query: q.Text, Exclude: q.Exclude, OmitRedacted: q.OmitRedacted,
		After: timeIn(q.After), Before: timeIn(q.Before), Limit: q.Limit}
	for _, r := range q.Roles {
		req.Roles = append(req.Roles, string(r))
	}
	f := q.Filter
	if f.CWD != "" || f.ParentSession != "" || f.Harness != "" || len(f.Extra) > 0 || f.TopLevel ||
		!f.After.IsZero() || !f.Before.IsZero() || f.Limit > 0 || f.Current {
		req.Filter = &searchFilter{CWD: f.CWD, ParentSession: f.ParentSession, Harness: f.Harness, Extra: f.Extra,
			TopLevel: f.TopLevel, After: timeIn(f.After), Before: timeIn(f.Before), Limit: f.Limit, Current: f.Current}
	}
	return req
}

// searches reports whether the handler serves search: over a store
// that searches, or one it can scan, which needs its Reader.
func (h *Handler) searches() bool {
	if _, ok := h.store.(agentsession.Searcher); ok {
		return true
	}
	return h.reader != nil
}

// search answers RFC 0003's search: the store's, through its
// [agentsession.Searcher] or by [agentsession.ScanSearch], over the
// sessions the caller may read alone, as NDJSON hits ending with end.
// A member the server does not know is bad_request, never ignored.
func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	if !h.searches() {
		h.writeProblem(w, problem(CodeUnsupported, "the store cannot be searched", ""))
		return
	}
	var req searchRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSearchBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		h.writeProblem(w, problem(CodeBadRequest, "the body does not parse", err.Error()))
		return
	}
	q, err := req.query()
	if err != nil {
		h.writeProblem(w, problem(CodeBadRequest, "bad search", err.Error()))
		return
	}
	ctx := r.Context()
	q.Readable = func(id string) bool { return h.may(ctx, ScopeRead, id) }
	next, stop := iter.Pull2(agentsession.Search(ctx, h.store, q))
	defer stop()
	hit, err, ok := next()
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
		if err := out.line(hitLine{Session: hit.Session, Entry: hit.Entry, TS: hit.Time, Role: string(hit.Role), Snippet: hit.Snippet}); err != nil {
			out.fail(h, err)
			return
		}
		hit, err, ok = next()
	}
	out.end(nil)
}

// Search implements [agentsession.Searcher] through the server's
// search, which answers over the sessions this client may read. A query
// with Readable set, which the server cannot be sent, and a server that
// serves no search (unsupported) are scanned here instead, over List
// and Read, as [agentsession.ScanSearch] scans any store.
func (c *Client) Search(ctx context.Context, q agentsession.Query) iter.Seq2[agentsession.Hit, error] {
	if q.Readable != nil {
		return agentsession.ScanSearch(ctx, c, q)
	}
	return func(yield func(agentsession.Hit, error) bool) {
		body, err := json.Marshal(searchRequestOf(q))
		if err != nil {
			yield(agentsession.Hit{}, fmt.Errorf("remote: search: %w", err))
			return
		}
		first := true
		for line, err := range c.streamLines(ctx, http.MethodPost, "/sessions/search", nil, body, nil) {
			if err != nil {
				var p *Problem
				if first && errors.As(err, &p) && p.Code == CodeUnsupported {
					for h, err := range agentsession.ScanSearch(ctx, c, q) {
						if !yield(h, err) || err != nil {
							return
						}
					}
					return
				}
				yield(agentsession.Hit{}, err)
				return
			}
			first = false
			var l hitLine
			dec := json.NewDecoder(bytes.NewReader(line))
			if err := dec.Decode(&l); err != nil {
				yield(agentsession.Hit{}, fmt.Errorf("remote: search: %w", err))
				return
			}
			if !yield(agentsession.Hit{Session: l.Session, Entry: l.Entry, Time: l.TS, Role: openresponses.Role(l.Role), Snippet: l.Snippet}, nil) {
				return
			}
		}
	}
}
