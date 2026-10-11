package remote

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
)

// Version is the path prefix every route is under.
const Version = "/v1"

// Capabilities are what a server answers at GET /v1/: the format it
// reads and, for each optional feature of RFC 0003, whether its store
// supports it through this server.
type Capabilities struct {
	// Reads is the newest format the server reads, as a header names it.
	// It reads every earlier minor of that major too, as the library
	// does.
	Reads    string   `json:"reads"`
	Features Features `json:"features"`
	Limits   Limits   `json:"limits"`
}

// Features are RFC 0003's optional features. A request for one the
// server does not support is 501 `unsupported`.
type Features struct {
	Objects       bool `json:"objects"`
	Shallow       bool `json:"shallow"`
	Exchange      bool `json:"exchange"`
	Marks         bool `json:"marks"`
	Commit        bool `json:"commit"`
	CommittedView bool `json:"committed_view"`
	Refs          bool `json:"refs"`
	RefFollow     bool `json:"ref_follow"`
	Leases        bool `json:"leases"`
}

// Limits are the server's bounds a client can plan around.
type Limits struct {
	// FollowBuffer is how many events a follow may fall behind before
	// the server cuts it with `overflow`.
	FollowBuffer int `json:"follow_buffer"`
	// FollowReplay is how many recent events a resume can be served from
	// without loading the session again.
	FollowReplay int `json:"follow_replay"`
}

// The error codes of RFC 0003's Errors table this slice uses, and
// `overflow`, which RFC 0003's Following adds for a follow the server
// cut because it fell behind.
const (
	CodeNoSession       = "no_session"
	CodeSessionReplaced = "session_replaced"
	CodeNoEntry         = "no_entry"
	CodeNoRef           = "no_ref"
	CodeRefDangling     = "ref_dangling"
	CodeRefName         = "ref_name"
	CodeSessionLocked   = "session_locked"
	CodeFormat          = "format"
	CodeReadOnly        = "read_only"
	CodeUnsupported     = "unsupported"
	CodeStoreFault      = "store_fault"
	CodeAuth            = "auth"
	CodeScope           = "scope"
	CodeOverflow        = "overflow"
	CodeBadRequest      = "bad_request"
)

// Errors a [Client] call returns, matched with errors.Is, beside the
// library's own sentinels a problem carries (agentsession.ErrNoSession
// for `no_session`, and so on).
var (
	// ErrUnsupported is a feature the server's store lacks, or one this
	// slice of the protocol does not serve.
	ErrUnsupported = errors.New("remote: the server does not support this")
	// ErrUnauthenticated is a request no authenticator accepted.
	ErrUnauthenticated = errors.New("remote: unauthenticated")
	// ErrForbidden is a request from a principal without the scope it
	// needs.
	ErrForbidden = errors.New("remote: the principal lacks the scope this needs")
	// ErrSessionReplaced is a session another was created again under
	// the ID of, refused for an If-Incarnation that names the old one.
	ErrSessionReplaced = errors.New("remote: the session was replaced")
	// ErrOverflow is a follow the server cut because the client fell
	// behind. A Client resumes from its cursor by itself.
	ErrOverflow = errors.New("remote: the follow fell behind and was cut")
	// ErrCut is a stream that ended without its terminal frame: the
	// connection was lost or the server stopped, and what was read is
	// not the whole answer.
	ErrCut = errors.New("remote: the stream was cut")
	// ErrBadRequest is a request the server could not parse.
	ErrBadRequest = errors.New("remote: bad request")
)

// codes maps each code to the HTTP status it is sent with and the error
// it matches on the client.
var codes = map[string]struct {
	status int
	err    error
}{
	CodeNoSession:       {http.StatusNotFound, agentsession.ErrNoSession},
	CodeSessionReplaced: {http.StatusConflict, ErrSessionReplaced},
	CodeNoEntry:         {http.StatusNotFound, agentsession.ErrNoEntry},
	CodeNoRef:           {http.StatusNotFound, agentsession.ErrNoRef},
	// A dangling ref is the library's ResolveRef error, which wraps
	// ErrNoSession beside the target.
	CodeRefDangling:   {http.StatusConflict, agentsession.ErrNoSession},
	CodeRefName:       {http.StatusBadRequest, agentsession.ErrRefName},
	CodeSessionLocked: {http.StatusConflict, agentsession.ErrSessionLocked},
	CodeFormat:        {http.StatusUnprocessableEntity, agentsession.ErrUnsupportedFormat},
	CodeReadOnly:      {http.StatusForbidden, agentsession.ErrReadOnly},
	CodeUnsupported:   {http.StatusNotImplemented, ErrUnsupported},
	CodeStoreFault:    {http.StatusInternalServerError, nil},
	CodeAuth:          {http.StatusUnauthorized, ErrUnauthenticated},
	CodeScope:         {http.StatusForbidden, ErrForbidden},
	CodeOverflow:      {http.StatusServiceUnavailable, ErrOverflow},
	CodeBadRequest:    {http.StatusBadRequest, ErrBadRequest},
}

// Problem is an error that crossed the wire, as RFC 9457's problem
// details with RFC 0003's stable code. It matches, with errors.Is, the
// sentinel its code names.
type Problem struct {
	Status int    `json:"status"`
	Code   string `json:"code"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Target is a dangling ref's target, so a client can see which
	// session it was.
	Target *Target `json:"target,omitempty"`
	// Incarnation is the session's, on a problem about one.
	Incarnation string `json:"incarnation,omitempty"`
}

func (p *Problem) Error() string {
	s := "remote: " + p.Code
	if p.Title != "" {
		s += ": " + p.Title
	}
	if p.Detail != "" {
		s += ": " + p.Detail
	}
	return s
}

// Unwrap returns the sentinel the code names, nil for a code that names
// none.
func (p *Problem) Unwrap() error { return codes[p.Code].err }

// problem builds the problem a server sends for code.
func problem(code, title, detail string) *Problem {
	st, ok := codes[code]
	if !ok {
		st.status = http.StatusInternalServerError
	}
	return &Problem{Status: st.status, Code: code, Title: title, Detail: detail}
}

// problemOf is the problem for an error the store returned. A store
// fault says nothing more than that: its text can name the store's
// files, which the protocol never shows a client.
func problemOf(err error) *Problem {
	var p *Problem
	if errors.As(err, &p) {
		return p
	}
	switch {
	case errors.Is(err, agentsession.ErrNoSession):
		return problem(CodeNoSession, "no such session", "")
	case errors.Is(err, agentsession.ErrNoEntry):
		return problem(CodeNoEntry, "no such entry", "")
	case errors.Is(err, agentsession.ErrNoRef):
		return problem(CodeNoRef, "no such ref", "")
	case errors.Is(err, agentsession.ErrRefName):
		return problem(CodeRefName, "invalid ref name", "")
	case errors.Is(err, agentsession.ErrReadOnly):
		return problem(CodeReadOnly, "the store is read-only", "")
	case errors.Is(err, agentsession.ErrNoRefs):
		return problem(CodeUnsupported, "the store keeps no refs", "")
	case errors.Is(err, agentsession.ErrSessionLocked):
		return problem(CodeSessionLocked, "the session is held", "")
	case errors.Is(err, agentsession.ErrUnsupportedFormat):
		return problem(CodeFormat, "a format the server does not read", "")
	}
	return problem(CodeStoreFault, "the store failed", "")
}

// Target is a ref's target on the wire.
type Target struct {
	Session string `json:"session"`
	Entry   string `json:"entry"`
}

func targetOut(t agentsession.RefTarget) *Target {
	if t.IsZero() {
		return nil
	}
	return &Target{Session: t.Session, Entry: t.Entry}
}

func (t *Target) lib() agentsession.RefTarget {
	if t == nil {
		return agentsession.RefTarget{}
	}
	return agentsession.RefTarget{Session: t.Session, Entry: t.Entry}
}

// record is one entry of a snapshot: its place in the log, its ID and
// its RFC 0001 line.
type record struct {
	Seq   int             `json:"seq"`
	Entry string          `json:"entry"`
	Line  json.RawMessage `json:"line"`
}

// snapshotData is a snapshot or reset event: the session from the
// start, every entry inlined.
type snapshotData struct {
	Session     string          `json:"session"`
	Header      json.RawMessage `json:"header"`
	Incarnation string          `json:"incarnation"`
	Head        *string         `json:"head"`
	Records     []record        `json:"records"`
}

// appendedData is an appended event: the record, with its line, and
// the head after it.
type appendedData struct {
	Seq     int             `json:"seq"`
	Entry   string          `json:"entry"`
	Line    json.RawMessage `json:"line"`
	Outcome string          `json:"outcome"`
	Head    *string         `json:"head"`
}

// headData is a head event: a head move no append explains.
type headData struct {
	Head *string `json:"head"`
}

func headOut(leaf string) *string {
	if leaf == "" {
		return nil
	}
	return &leaf
}

func headIn(h *string) string {
	if h == nil {
		return ""
	}
	return *h
}

// The follow events of RFC 0003's Following this slice sends.
const (
	eventSnapshot = "snapshot"
	eventAppended = "appended"
	eventHead     = "head"
	eventReset    = "reset"
	eventError    = "error"
)

// eventID is an SSE id: the cursor after the event, a dot, and the
// event's index among those at that cursor. The store's cursor is
// opaque to a client, so it goes on the wire base64url-encoded, which
// keeps it to characters an SSE id and a query carry as they are and
// leaves the last dot the index's.
func eventID(c agentsession.Cursor, n int) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c)) + "." + strconv.Itoa(n)
}

// cursorOf splits an event ID into the store's cursor and the index.
func cursorOf(id string) (agentsession.Cursor, int, error) {
	i := strings.LastIndexByte(id, '.')
	if i < 0 {
		return "", 0, fmt.Errorf("%w: event id %q", ErrBadRequest, id)
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil || n < 0 {
		return "", 0, fmt.Errorf("%w: event id %q", ErrBadRequest, id)
	}
	c, err := base64.RawURLEncoding.DecodeString(id[:i])
	if err != nil {
		return "", 0, fmt.Errorf("%w: event id %q", ErrBadRequest, id)
	}
	return agentsession.Cursor(c), n, nil
}

// incarnation names a session's incarnation, as RFC 0002's refs do.
func incarnation(h agentsession.Header) string {
	inc, err := agentsession.HeaderIdent(h)
	if err != nil {
		return ""
	}
	return inc
}
