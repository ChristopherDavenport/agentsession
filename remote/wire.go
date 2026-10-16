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
	"github.com/ChristopherDavenport/agentsession/cas"
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
	// Log is the log read, record by record with a cursor after each,
	// which a store that keeps a log of records serves (of the
	// reference stores, cas).
	Log bool `json:"log"`
	// DurableMarks is durable on each event of a follow that is not
	// durable=true, and the committed event. This server does not send
	// them: its follow is a store's follower, which does not say; a
	// client that needs committed records follows with durable=true.
	DurableMarks bool `json:"durable_marks"`
}

// Limits are the server's bounds a client can plan around.
type Limits struct {
	// FollowBuffer is how many events a follow may fall behind before
	// the server cuts it with `overflow`.
	FollowBuffer int `json:"follow_buffer"`
	// FollowReplay is how many recent events a resume can be served from
	// without loading the session again.
	FollowReplay int `json:"follow_replay"`
	// UploadGraceMS is how long an object uploaded for a push is kept
	// for its uploader's push before nothing reaches it: the grace a
	// sweep of the store must give unreferenced objects. Zero where the
	// server takes no objects.
	UploadGraceMS int64 `json:"upload_grace_ms,omitempty"`
	// SnapshotBytes is the most entry-line bytes a follow's snapshot or
	// reset carries inline: past it a server that serves objects sends
	// records without their lines, and one that serves none refuses the
	// snapshot with `too_large`.
	SnapshotBytes int64 `json:"snapshot_bytes,omitempty"`
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

	// The write side's.
	CodeSessionExists  = "session_exists"
	CodeHeadMoved      = "head_moved"
	CodeHeadTarget     = "head_target"
	CodeLeaseLost      = "lease_lost"
	CodeMirror         = "mirror"
	CodeCallIDRepeated = "call_id_repeated"
	CodeSynthetic      = "synthetic"
	CodeBadEntry       = "bad_entry"
	CodeBaseRule       = "base_rule"
	CodeBadBase        = "bad_base"
	CodeRefMoved       = "ref_moved"
	CodeRedacted       = "redacted"
	CodeNotRedacted    = "not_redacted"

	// Exchange's.
	CodeNoObject      = "no_object"
	CodeBadObject     = "bad_object"
	CodeMissingObject = "missing_object"
	CodeHeaderDiffers = "header_differs"
	CodeNotRecord     = "not_record"
	CodeBadRefTarget  = "bad_ref_target"

	// The remaining reads': a snapshot past the server's bound from a
	// server that serves no objects.
	CodeTooLarge = "too_large"
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
	// ErrLeaseLost is a write whose lease is no longer the session's
	// current one: it expired, was released or another holder took the
	// session. A writer that wants to go on takes a lease again, reads
	// the log to learn whether its last write landed, and resumes.
	ErrLeaseLost = errors.New("remote: the lease is not the session's current one")
	// ErrHeadMoved is a head compare-and-swap whose expected head is not
	// the session's.
	ErrHeadMoved = errors.New("remote: the head is not the expected entry")
	// ErrHeadTarget is a head move to an entry the head may not rest on:
	// a leaf label, or one that is not the base or an own entry.
	ErrHeadTarget = errors.New("remote: the head may not rest there")
	// ErrMirror is a write to a session the server holds as a mirror.
	ErrMirror = errors.New("remote: the session is a mirror on this server")
	// ErrSynthetic is a leaf label carrying synthetic.
	ErrSynthetic = errors.New("remote: a synthetic leaf label is a projection's")
	// ErrMissingObject is a push naming objects the server cannot find
	// for the caller: the problem's Missing says which. Upload them and
	// push again.
	ErrMissingObject = errors.New("remote: the push names objects the server lacks")
	// ErrBadRefTarget is a pushed ref whose next is not the pushed
	// session.
	ErrBadRefTarget = errors.New("remote: a pushed ref names another session")
	// ErrTooLarge is a snapshot past the server's bound, from a server
	// that serves no objects to read the rest by.
	ErrTooLarge = errors.New("remote: the session is past what the server sends in a snapshot")
	// ErrBadEntry is an entry line the server refused as malformed. A
	// problem whose reason names a library rule matches that rule's
	// sentinel too.
	ErrBadEntry = errors.New("remote: a malformed entry")
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

	CodeSessionExists:  {http.StatusConflict, agentsession.ErrSessionExists},
	CodeHeadMoved:      {http.StatusConflict, ErrHeadMoved},
	CodeHeadTarget:     {http.StatusUnprocessableEntity, ErrHeadTarget},
	CodeLeaseLost:      {http.StatusConflict, ErrLeaseLost},
	CodeMirror:         {http.StatusConflict, ErrMirror},
	CodeCallIDRepeated: {http.StatusConflict, agentsession.ErrCallIDRepeated},
	CodeSynthetic:      {http.StatusUnprocessableEntity, ErrSynthetic},
	CodeBadEntry:       {http.StatusUnprocessableEntity, ErrBadEntry},
	CodeBaseRule:       {http.StatusUnprocessableEntity, agentsession.ErrBaseRule},
	CodeBadBase:        {http.StatusUnprocessableEntity, agentsession.ErrNoEntry},
	CodeRefMoved:       {http.StatusConflict, agentsession.ErrRefMoved},
	CodeRedacted:       {http.StatusConflict, agentsession.ErrRedacted},
	CodeNotRedacted:    {http.StatusForbidden, agentsession.ErrNotRedacted},

	CodeNoObject:      {http.StatusNotFound, cas.ErrNoObject},
	CodeBadObject:     {http.StatusUnprocessableEntity, cas.ErrBadObject},
	CodeMissingObject: {http.StatusConflict, ErrMissingObject},
	CodeHeaderDiffers: {http.StatusConflict, cas.ErrHeaderDiffers},
	CodeNotRecord:     {http.StatusConflict, cas.ErrNotRecord},
	CodeBadRefTarget:  {http.StatusUnprocessableEntity, ErrBadRefTarget},

	CodeTooLarge: {http.StatusRequestEntityTooLarge, ErrTooLarge},
}

// reasons are the library rules a bad_entry or bad_base problem's
// reason names, so a client matches the library's sentinel as a local
// store's caller would.
var reasons = map[string]error{
	"bad_id":                agentsession.ErrBadID,
	"reserved_member":       agentsession.ErrReservedMember,
	"bad_convergence":       agentsession.ErrBadConvergence,
	"bad_target":            agentsession.ErrBadTarget,
	"call_id_empty":         agentsession.ErrCallIDEmpty,
	"call_rejected":         agentsession.ErrCallRejected,
	"call_answered":         agentsession.ErrCallAnswered,
	"call_completed":        agentsession.ErrCallCompleted,
	"source_mismatch":       agentsession.ErrSourceMismatch,
	"reject_dispatched":     agentsession.ErrRejectDispatched,
	"answer_not_dispatched": agentsession.ErrAnswerNotDispatched,
	"reason_mismatch":       agentsession.ErrReasonMismatch,
	"link_mismatch":         agentsession.ErrLinkMismatch,
	"bad_normalisation":     agentsession.ErrBadNormalisation,
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
	// Reason names the library rule a bad_entry or bad_base broke.
	Reason string `json:"reason,omitempty"`
	// Head is the session's head on a head_moved refusal, "" for none.
	Head *string `json:"head,omitempty"`
	// Holder is the label of the lease holder on a session_locked
	// refusal, for a caller with write scope.
	Holder string `json:"holder,omitempty"`
	// Missing is the objects a push named that the server could not
	// find for the caller, on missing_object.
	Missing *Objects `json:"missing,omitempty"`
	// Push is what a push did before it was refused, on a handover's
	// head_moved after its entries were admitted: the push's response,
	// its refs not attempted.
	Push *PushResult `json:"push,omitempty"`
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

// Unwrap returns the sentinels the code and the reason name: the
// code's, and the library rule's for a reason that names one.
func (p *Problem) Unwrap() []error {
	var out []error
	if err := codes[p.Code].err; err != nil {
		out = append(out, err)
	}
	if err := reasons[p.Reason]; err != nil {
		out = append(out, err)
	}
	return out
}

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
	case errors.Is(err, agentsession.ErrRedacted):
		return problem(CodeRedacted, "a redacted session is written whole and never created empty or appended to", err.Error())
	case errors.Is(err, agentsession.ErrNotRedacted):
		return problem(CodeNotRedacted, "the store holds only redacted sessions", "")
	case errors.Is(err, agentsession.ErrNoSession):
		return problem(CodeNoSession, "no such session", "")
	case errors.Is(err, agentsession.ErrBaseRule):
		return problem(CodeBaseRule, "the parent breaks the session's parent rule", err.Error())
	case errors.Is(err, agentsession.ErrBadConvergence):
		p := problem(CodeBadBase, "the fork's prefix converges an entry off its path", err.Error())
		p.Reason = "bad_convergence"
		return p
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
	case errors.Is(err, agentsession.ErrSessionExists):
		return problem(CodeSessionExists, "a session is held under this ID", "")
	case errors.Is(err, agentsession.ErrRefMoved):
		return problem(CodeRefMoved, "the ref is not at the expected target", "")
	case errors.Is(err, agentsession.ErrCallIDRepeated):
		return problem(CodeCallIDRepeated, "the entry repeats a call_id", err.Error())
	case errors.Is(err, cas.ErrHeadMoved):
		return problem(CodeHeadMoved, "the head is not the expected entry", "")
	case errors.Is(err, cas.ErrMirror):
		return problem(CodeMirror, "the session is a mirror here", "")
	case errors.Is(err, cas.ErrHeadTarget):
		return problem(CodeHeadTarget, "the head never rests on a leaf label", "")
	case errors.Is(err, cas.ErrNoObject):
		return problem(CodeNoObject, "no such object", "")
	case errors.Is(err, cas.ErrBadObject):
		return problem(CodeBadObject, "the bytes do not hash to the name", "")
	case errors.Is(err, cas.ErrHeaderDiffers):
		return problem(CodeHeaderDiffers, "the server holds another session under this ID", "")
	case errors.Is(err, cas.ErrNotRecord):
		return problem(CodeNotRecord, "only the record may push a session", "")
	case errors.Is(err, cas.ErrSynthetic):
		return problem(CodeSynthetic, "a synthetic leaf label is a projection's", "")
	}
	for reason, sentinel := range reasons {
		if errors.Is(err, sentinel) {
			p := problem(CodeBadEntry, "the entry breaks a rule of the format", err.Error())
			p.Reason = reason
			return p
		}
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
	Line  json.RawMessage `json:"line,omitempty"`
}

// snapshotData is a snapshot or reset event: the session from the
// start, every entry inlined up to the server's snapshot bound, and past
// it, from a server that serves objects, the records alone.
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
	// Durable is set on a durable follow, whose every record is
	// committed, and omitted otherwise.
	Durable *bool `json:"durable,omitempty"`
}

// headData is a head event: a head move no append explains.
type headData struct {
	Head    *string `json:"head"`
	Durable *bool   `json:"durable,omitempty"`
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
	// eventTarget is a followed ref's target and state.
	eventTarget = "target"
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
