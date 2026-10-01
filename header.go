package agentsession

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
	"github.com/ChristopherDavenport/openresponses"
)

// Format is the session format version this package writes.
const Format = "agentsession/0.10"

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
	// Redacted is true when bodies were changed after they were written,
	// as export redaction does. The redactor recomputes every hash and
	// reference over the redacted bodies, so the file walks and
	// verifies against itself and not against the original.
	Redacted bool `json:"redacted,omitempty"`

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

// headerType is the "type" of the header line.
const headerType = "session"

var headerKeys = jsonx.Keys[Header]()

// MarshalJSON emits the header with its type discriminator first and
// Extra flattened into the object.
func (h Header) MarshalJSON() ([]byte, error) {
	type plain Header
	body, err := jsonx.MarshalNoEscape(plain(h))
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
	return nil
}

// Validate checks the required header fields and that the format is one
// this package reads.
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
// 0.6 to 0.9 only add optional members and elements and 0.10 only lets
// an answer follow a dispatch on another branch, but for two rules 0.8
// reads differently: instructions_omitted
// stays in force until a later config changes it, and a run whose
// pending call is held after its dispatch ends input_required, not
// aborted. 0.9 lets instructions_omitted name a run of the list in
// force by keep, which no 0.8 omitted part could be. 0.10 also marks a
// file written under 0.9's rules as they stood when they stopped
// changing.
const FormatMinor = 10

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
