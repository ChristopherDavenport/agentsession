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
	"slices"
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
	Name    string    `json:"name,omitempty"`
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

// searchMembers and filterMembers are the members a search's body and
// its filter may hold, by their exact names.
var (
	searchMembers = []string{"query", "filter", "exclude", "omit_redacted", "roles", "after", "before", "limit"}
	filterMembers = []string{"cwd", "parent_session", "harness", "extra", "top_level", "after", "before", "limit", "current"}
)

// decodeSearch reads a search's body: one JSON object, or nothing,
// whose members, and its filter's, are named exactly as RFC 0003 names
// them. encoding/json alone would take "Limit" for limit and ignore what
// follows the object.
func decodeSearch(body io.Reader) (searchRequest, error) {
	var req searchRequest
	dec := json.NewDecoder(body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); errors.Is(err, io.EOF) {
		return req, nil
	} else if err != nil {
		return req, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, errors.New("data after the object")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return req, err
	}
	if err := knownMembers(top, searchMembers, ""); err != nil {
		return req, err
	}
	if f, ok := top["filter"]; ok {
		var members map[string]json.RawMessage
		if err := json.Unmarshal(f, &members); err != nil {
			return req, fmt.Errorf("filter: %w", err)
		}
		if err := knownMembers(members, filterMembers, "filter "); err != nil {
			return req, err
		}
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if err := strict.Decode(&req); err != nil {
		return req, err
	}
	return req, nil
}

// knownMembers refuses a member of obj not named in known, compared
// exactly.
func knownMembers(obj map[string]json.RawMessage, known []string, where string) error {
	for name := range obj {
		if !slices.Contains(known, name) {
			return fmt.Errorf("unknown %smember %q", where, name)
		}
	}
	return nil
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
	req, err := decodeSearch(io.LimitReader(r.Body, maxSearchBody))
	if err != nil {
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
		if err := out.line(hitLine{Session: hit.Session, Name: hit.Name, Entry: hit.Entry, TS: hit.Time, Role: string(hit.Role), Snippet: hit.Snippet}); err != nil {
			out.fail(h, err)
			return
		}
		hit, err, ok = next()
	}
	out.end(nil)
}

// Search implements [agentsession.Searcher] through the server's
// search, which answers over the sessions this client may read. Where
// the server cannot answer, a server that serves no search and a query
// with Readable set, which cannot be sent, it answers [ErrUnsupported]
// unless the client was made [WithSearchScan], and then scans here,
// over List and Read, as [agentsession.ScanSearch] scans any store. A
// scan the server cannot serve either, over a store with no Reader,
// fails with the server's answer to the search.
func (c *Client) Search(ctx context.Context, q agentsession.Query) iter.Seq2[agentsession.Hit, error] {
	if q.Readable != nil {
		if !c.searchScan {
			return func(yield func(agentsession.Hit, error) bool) {
				yield(agentsession.Hit{}, fmt.Errorf("%w: a search carrying Readable is scanned only by a client made WithSearchScan", ErrUnsupported))
			}
		}
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
				if first && c.searchScan && errors.As(err, &p) && p.Code == CodeUnsupported {
					c.scan(ctx, q, p, yield)
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
			if !yield(agentsession.Hit{Session: l.Session, Name: l.Name, Entry: l.Entry, Time: l.TS, Role: openresponses.Role(l.Role), Snippet: l.Snippet}, nil) {
				return
			}
		}
	}
}

// scan answers q by a scan here, for a server that answered its search
// with refused: a scan the server cannot serve either fails with
// refused, the server's answer to the search, and not with the read
// that found it out.
func (c *Client) scan(ctx context.Context, q agentsession.Query, refused *Problem, yield func(agentsession.Hit, error) bool) {
	for h, err := range agentsession.ScanSearch(ctx, c, q) {
		if errors.Is(err, ErrUnsupported) {
			err = refused
		}
		if !yield(h, err) || err != nil {
			return
		}
	}
}
