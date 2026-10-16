package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
	"github.com/ChristopherDavenport/openresponses"
)

// Format is the session format version this package writes.
const Format = "agentsession/0.13"

// FormatMajor is the major version of the format this package reads.
// Any minor version of it is accepted; files are migrated in memory.
const FormatMajor = 0

// Payload is the payload profile this package writes: Open Responses
// items at the specification version openresponses targets.
const Payload = "openresponses/" + openresponses.SpecVersion

// Media storage modes named by the header.
const (
	MediaInline  = "inline"
	MediaSidecar = "sidecar"
)

// mediaForm is the media mode a header's media member means: absent is
// inline.
func mediaForm(m string) string {
	if m == "" {
		return MediaInline
	}
	return m
}

// ErrUnsupportedFormat is returned when a header names a format this
// package cannot read.
var ErrUnsupportedFormat = errors.New("agentsession: unsupported format")

// Header is the first line of a session file. It is not part of the
// tree. Unknown fields decode into Extra and are written back, as the
// format requires of any tool that rewrites a file.
type Header struct {
	Format    string    `json:"format"`
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Payload   string    `json:"payload"`
	Harness   *Harness  `json:"harness,omitempty"`
	// Records lists the record entry types this writer writes whenever
	// their event occurs, so a reader may take their absence on a path
	// as the event not having happened. Empty means no such promise,
	// which is what a converter over a native log without them sets.
	Records       []string `json:"records,omitempty"`
	CWD           string   `json:"cwd,omitempty"`
	ParentSession string   `json:"parent_session,omitempty"`
	// SpawnedBy is, for a subsession, the call_id of the parent's
	// function call that spawned it.
	SpawnedBy string `json:"spawned_by,omitempty"`
	// Base is the hash of the entry this session continues from, in
	// the session ParentSession names, or empty for a session that
	// starts fresh. A file with a base opens with the path to it, the
	// prefix, before any entry the session appended itself, and every
	// own entry hangs from the base. A base is never a leaf label.
	Base  string `json:"base,omitempty"`
	Media string `json:"media,omitempty"`
	// Redacted marks a redacted session: one a redactor made from
	// another, its original, by changing what was written after it was
	// written. It is a session of its own, with an ID of its own, whose
	// hashes and references the redactor recomputed over what it wrote,
	// so the file walks and verifies against itself and not against the
	// original. It is written whole and never appended to.
	Redacted bool `json:"redacted,omitempty"`
	// RedactedFrom, on a redacted session, names its original: see
	// [RedactedFrom]. It is defined from format 0.13 and is refused
	// without Redacted, with [ErrRedactedFrom]. A redacted_from member
	// in a file that declares an earlier minor, which was free to hold
	// one in any form, or one in a later file that does not decode as
	// the object, is kept in Extra as written and RedactedFrom is nil.
	RedactedFrom *RedactedFrom `json:"-"`

	// Extra holds header fields this package does not define.
	Extra map[string]json.RawMessage `json:"-"`
}

// HasRecord reports whether the header promises that entries of type
// typ are written whenever their event occurs, so their absence means
// the event did not happen.
func (h Header) HasRecord(typ string) bool {
	for _, t := range h.Records {
		if t == typ {
			return true
		}
	}
	return false
}

// AllRecords are the three record entry types a harness that runs the
// loop itself can promise: run, dispatch and decision. A harness that
// also accepts inputs while a run is in flight adds [TypeQueued],
// which promises that every such input is recorded as a queued entry
// before it is acted on, so a reader may take the absence of one as
// nothing having been queued.
var AllRecords = []string{TypeRun, TypeDispatch, TypeDecision}

// Harness names the writer of a session.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// RedactedFrom is a redacted session's link to its original, the
// header's redacted_from member. Session is the original's ID and
// Entries how many entries the original held, prefix included and in
// file order, when the redaction was made: sessions are append-only, so
// the count pins the version redacted, whatever of it the redaction
// kept, and unlike the hash of its last entry it confirms no guessed
// value. Profile names the redaction, its kind and its rules at a
// version, so two redactions of one original are told apart; a
// redaction that writes one derives the session's ID with
// [RedactedSessionID]. Aligned says the redaction wrote one entry for
// each of the original's, in file order, each made from the one in its
// place with only its body changed, so the n-th entry maps back to the
// original's n-th by position. Extra holds the members this package
// does not define, the namespaced ones a redaction records about itself
// among them, and a profile that is not a non-empty string or an
// aligned that is not true, each kept as written.
type RedactedFrom struct {
	Session string `json:"session"`
	Entries int    `json:"entries"`
	Profile string `json:"profile,omitempty"`
	Aligned bool   `json:"aligned,omitempty"`

	// Extra holds redacted_from members this package does not define.
	Extra map[string]json.RawMessage `json:"-"`
}

// ErrRedactedFrom is returned for a header whose redacted_from the
// format forbids: one on a session that is not redacted, or, built in
// memory, one that names no original or a negative count.
var ErrRedactedFrom = errors.New("agentsession: malformed redacted_from")

// redactedFromMinor is the first minor that defines redacted_from.
const redactedFromMinor = 13

// MarshalJSON emits the object with Extra flattened into it. A member
// of Extra that a typed field also writes is refused, since the object
// would hold it twice.
func (r RedactedFrom) MarshalJSON() ([]byte, error) {
	for k := range r.Extra {
		if k == "session" || k == "entries" || k == "profile" && r.Profile != "" || k == "aligned" && r.Aligned {
			return nil, fmt.Errorf("%w: %s both typed and in Extra", ErrRedactedFrom, k)
		}
	}
	type plain RedactedFrom
	return jsonx.MarshalWithUnknown(plain(r), r.Extra)
}

// UnmarshalJSON decodes the object, matching members by their exact
// names. It is refused unless session is a non-empty string and entries
// a count written as digits; a profile that is not a non-empty string
// and an aligned that is not true stay in Extra with the members this
// package does not define, so the object writes back as it was read.
func (r *RedactedFrom) UnmarshalJSON(data []byte) error {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	if all == nil {
		return fmt.Errorf("%w: not an object", ErrRedactedFrom)
	}
	var v RedactedFrom
	if json.Unmarshal(all["session"], &v.Session) != nil || v.Session == "" {
		return fmt.Errorf("%w: session is not a session id", ErrRedactedFrom)
	}
	n, err := strconv.ParseUint(string(all["entries"]), 10, 0)
	if err != nil || n > math.MaxInt {
		return fmt.Errorf("%w: entries is not a count", ErrRedactedFrom)
	}
	v.Entries = int(n)
	delete(all, "session")
	delete(all, "entries")
	if raw, ok := all["profile"]; ok && json.Unmarshal(raw, &v.Profile) == nil && v.Profile != "" {
		delete(all, "profile")
	} else {
		v.Profile = ""
	}
	if string(all["aligned"]) == "true" {
		v.Aligned = true
		delete(all, "aligned")
	}
	if len(all) > 0 {
		v.Extra = all
	}
	*r = v
	return nil
}

// headerType is the "type" of the header line.
const headerType = "session"

var headerKeys = jsonx.Keys[Header]()

// MarshalJSON emits the header with its type discriminator first and
// Extra flattened into the object.
func (h Header) MarshalJSON() ([]byte, error) {
	type plain Header
	if h.RedactedFrom != nil {
		if _, dup := h.Extra["redacted_from"]; dup {
			return nil, fmt.Errorf("%w: both typed and in Extra", ErrRedactedFrom)
		}
	}
	body, err := jsonx.MarshalNoEscape(struct {
		plain
		RedactedFrom *RedactedFrom `json:"redacted_from,omitempty"`
	}{plain(h), h.RedactedFrom})
	if err != nil {
		return nil, err
	}
	head, err := jsonx.MarshalNoEscape(struct {
		Type string `json:"type"`
	}{headerType})
	if err != nil {
		return nil, err
	}
	return jsonx.JoinObjects(head, body, h.Extra), nil
}

// UnmarshalJSON decodes the header, keeping unknown fields in Extra.
func (h *Header) UnmarshalJSON(data []byte) error {
	type plain Header
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	if typ := jsonx.PeekString(all["type"]); typ != headerType {
		return fmt.Errorf("agentsession: header type %q, want %q", typ, headerType)
	}
	*h = Header(p)
	h.Extra = jsonx.ExtraKeys(all, headerKeys, "type")
	if raw, ok := h.Extra["redacted_from"]; ok && definesRedactedFrom(h.Format) {
		var r RedactedFrom
		if r.UnmarshalJSON(raw) == nil {
			h.RedactedFrom = &r
			delete(h.Extra, "redacted_from")
			if len(h.Extra) == 0 {
				h.Extra = nil
			}
		}
	}
	return nil
}

// definesRedactedFrom reports whether a header of format defines the
// redacted_from member: from 0.13 on. In an earlier file the name was
// free, so the member is one this package does not know.
func definesRedactedFrom(format string) bool {
	major, minor, err := ParseFormat(format)
	return err == nil && (major > 0 || minor >= redactedFromMinor)
}

// Validate checks the required header fields, that the format is one
// this package reads, and that a redacted_from names an original and
// is on a redacted session, as [ErrRedactedFrom] says.
func (h Header) Validate() error {
	major, _, err := ParseFormat(h.Format)
	if err != nil {
		return err
	}
	if major != FormatMajor {
		return fmt.Errorf("%w: %s", ErrUnsupportedFormat, h.Format)
	}
	if minor, _ := func() (int, error) { _, m, e := ParseFormat(h.Format); return m, e }(); major == 0 && minor > FormatMinor {
		// The 0.x series is exempt from the rule that a reader reads
		// every minor of its major: a later 0.x may have changed the
		// envelope, and a reader of 0.x supports the minors it names.
		return fmt.Errorf("%w: %s is later than this reader's %s", ErrUnsupportedFormat, h.Format, Format)
	}
	if h.ID == "" {
		return errors.New("agentsession: header id is required")
	}
	if h.CreatedAt.IsZero() {
		return errors.New("agentsession: header created_at is required")
	}
	if h.Payload == "" {
		return errors.New("agentsession: header payload is required")
	}
	if !strings.HasPrefix(h.Payload, "openresponses/") {
		return fmt.Errorf("agentsession: unsupported payload profile %q", h.Payload)
	}
	if r := h.RedactedFrom; r != nil {
		switch {
		case !h.Redacted:
			return fmt.Errorf("%w: on a session that is not redacted", ErrRedactedFrom)
		case r.Session == "":
			return fmt.Errorf("%w: names no session", ErrRedactedFrom)
		case r.Entries < 0:
			return fmt.Errorf("%w: entries %d", ErrRedactedFrom, r.Entries)
		}
	}
	return nil
}

// ParseFormat splits a format string such as "agentsession/0.1" into its
// major and minor versions.
func ParseFormat(format string) (major, minor int, err error) {
	name, version, ok := strings.Cut(format, "/")
	if !ok || name != "agentsession" {
		return 0, 0, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	maj, min, ok := strings.Cut(version, ".")
	if !ok {
		return 0, 0, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	if major, err = strconv.Atoi(maj); err != nil {
		return 0, 0, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	if minor, err = strconv.Atoi(min); err != nil {
		return 0, 0, fmt.Errorf("%w: %q", ErrUnsupportedFormat, format)
	}
	return major, minor, nil
}

// fill sets the defaults a new session needs: a UUIDv7 ID, the current
// time, this package's format and payload profile.
func (h *Header) fill(now time.Time) {
	if h.Format == "" {
		h.Format = Format
	}
	if h.Payload == "" {
		h.Payload = Payload
	}
	if h.ID == "" {
		h.ID = NewSessionID()
	}
	if h.CreatedAt.IsZero() {
		h.CreatedAt = now.Round(0) // drop the monotonic reading; it never reaches disk
	}
}

// FormatMinor is the minor version this package writes. A file of an
// earlier minor is migrated in memory on read: from 0.5 an entry's id
// is its envelope hash, so every earlier entry is rehashed and keeps
// its old id in legacy_id. A 0.5 to 0.9 file reads as it stands, since
// 0.6 to 0.9 only add optional members and elements and 0.10 lets a
// dispatch elsewhere in the session stand for one on the path, for the
// answer rule and against the call having never started, which reads a
// 0.9 file's rebased call differently too; and for two rules 0.8 reads
// differently: instructions_omitted stays in force until a later config
// changes it, and a run whose pending call is held after its dispatch
// ends input_required, not aborted. 0.9 lets instructions_omitted name
// a run of the list in force by keep, which no 0.8 omitted part could
// be. 0.10 also marks a file written under 0.9's rules as they stood
// when they stopped changing. 0.11 accepts a run written resume over a
// segment that holds nothing, in a 0.9 or 0.10 file too, and adds the
// of member of an omitted keep, the omit setting, the judged_by link
// relation and the target member of a link; its writer names an
// instructions part out of force by its hash. 0.12 adds the peer link
// relation, the task, context and url members of a link that names a
// callee in another system, and its cancel member. 0.13 adds the
// redacted_from header member and makes a redacted session one of its
// own; a 0.12 file is a 0.13 file without the member, and one it holds
// there is kept in Extra as written.
const FormatMinor = 13

// hashedMinor is the first minor whose entry ids are envelope hashes.
// A file below it is rehashed on read.
const hashedMinor = 5

// migrate brings a header of an earlier minor version up to the current
// one in memory. The 0.x series is exempt from the rule that a minor
// version changes nothing a reader depends on, so a reader reads a
// file before 0.5 by rewriting it; the entries are rewritten by Read,
// and the header takes the current format, as a 0.5 header does with
// nothing rewritten.
func migrate(h *Header) error {
	major, _, err := ParseFormat(h.Format)
	if err != nil {
		return err
	}
	if major != FormatMajor {
		return fmt.Errorf("%w: %s", ErrUnsupportedFormat, h.Format)
	}
	if _, minor, _ := ParseFormat(h.Format); minor < FormatMinor {
		h.Format = Format
	}
	return nil
}
