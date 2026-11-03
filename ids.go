package agentsession

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/agentsession/internal/jsonx"
)

// NewSessionID returns a UUIDv7 in its canonical text form. The
// leading 48 bits are the current Unix time in milliseconds, so IDs sort
// by creation time; the rest is random.
func NewSessionID() string {
	return uuidv7(time.Now())
}

func uuidv7(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	binary.BigEndian.PutUint64(b[:8], ms<<16)
	rand.Read(b[6:]) // never fails since Go 1.24
	b[6] = 0x70 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return formatUUID(b)
}

// NewEntryID returns a short random entry ID: eight hexadecimal
// characters. Session.Append regenerates on the rare collision within a
// file, so callers need not check.
func NewEntryID() string {
	var b [4]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// newUniqueEntryID returns an ID not present in taken, widening after
// repeated collisions so a pathological file still terminates.
func newUniqueEntryID(taken func(string) bool) string {
	for i := 0; i < 8; i++ {
		id := NewEntryID()
		if !taken(id) {
			return id
		}
	}
	var b [16]byte
	rand.Read(b[:])
	id := hex.EncodeToString(b[:])
	if taken(id) {
		panic(fmt.Sprintf("agentsession: entry ID %s already taken", id))
	}
	return id
}

// SubsessionID derives the session ID the format recommends for a
// subsession: a UUIDv5 under the nil namespace over
// "<parent session id>/<call_id>", so a reader can compute the child's
// ID from the parent's link or function call alone, before or after
// the child exists. A second child for the same call appends a new
// root to the existing child session rather than minting a second ID.
func SubsessionID(parentSessionID, callID string) string {
	return uuidv5(parentSessionID + "/" + callID)
}

// SpawnedSessionID derives the session ID the format recommends for a
// child the host started, by a subsession link carrying spawnID (RFC
// 0001, link, since 0.19): a UUIDv5 under the nil namespace over the
// name ["spawn","<parent session id>","<spawn_id>"], a JSON array in
// the canonical form the entry hash uses, which no name [SubsessionID]
// hashes equals, since that starts with a session ID and not with "[".
func SpawnedSessionID(parentSessionID, spawnID string) string {
	return uuidv5(string(canonicalName("spawn", parentSessionID, spawnID)))
}

// RedactedSessionID derives the session ID the format recommends for a
// redacted session whose redacted_from writes a profile: a UUIDv5
// under the nil namespace over the name ["redacted","<pseudonym>","<profile>"],
// a JSON array in the canonical form the entry hash uses, pseudonym
// being the original's, as redacted_from names it. Since 0.19 that is
// the original's keyed pseudonym ([Pseudonym], [RedactionNames]) and
// not its ID, unless the original is itself a redacted session, whose
// ID is its own pseudonym; given a native ID, RedactedSessionID derives
// what 0.18 derived, which anyone holding that ID can recompute, so a
// redactor derives through [RedactionNames]. It takes no key, so a
// reader recomputes a redacted session's ID from its own redacted_from
// ([CheckRedactedID]).
//
// An array cannot be read two ways, as a slash-joined name can when an
// ID holds a slash, and it is not a subsession's name, which starts with
// a session ID. One original under one profile and key then has one
// redacted session, whose ID stays as it grows, so a redaction repeated
// over a store finds the session it made before and appends only what
// the original added since. A profile names one set of rules, since two
// under one name write into one session. A string that is not UTF-8 is
// encoded as the JSON encoder does, its invalid bytes as U+FFFD.
func RedactedSessionID(pseudonym, profile string) string {
	return uuidv5(string(canonicalName("redacted", pseudonym, profile)))
}

// canonicalName is the JSON array of parts in the canonical form the
// entry hash uses, as a name for a derived ID.
func canonicalName(parts ...string) []byte {
	data, err := jsonx.MarshalNoEscape(parts)
	if err == nil {
		data, err = jcs.Transform(data)
	}
	if err != nil {
		// Strings always encode, and the encoding is JSON.
		panic(fmt.Sprintf("agentsession: derived session name: %v", err))
	}
	return data
}

// RedactionKeySize is the fewest bytes a redaction key holds (RFC 0001,
// The redacted session, since 0.19).
const RedactionKeySize = 32

// ErrRedactionKey is returned by [Pseudonym] and [NewRedactionNames]
// for a key shorter than [RedactionKeySize], an absent one included.
// There is no default key and no unkeyed fallback: a pseudonym derived
// without a secret is one anyone holding the ID recomputes.
var ErrRedactionKey = errors.New("agentsession: a redaction key is a secret of at least 32 bytes")

// Pseudonym returns the keyed pseudonym of the session ID id under key
// and profile (RFC 0001, The redacted session, since 0.19): the UUIDv8
// whose bytes are the first 16 of HMAC-SHA-256 under key over the
// canonical form of ["agentsession/pseudonym","<id>","<profile>"], its
// version and variant bits set as RFC 9562 sets them, in lowercase.
// profile is the one redacted_from writes, "" when it writes none; it is
// in the name so that two profiles' redactions of one original cannot
// be linked by their pseudonyms.
//
// This is the pseudonym of an ID that is not in the format's ID space.
// A redacted session's ID is its own pseudonym, which takes no key;
// [RedactionNames] tells the two apart. A holder of the originals
// computes it to look an original up in redacted_from. The key is
// whoever holds the originals' to keep, at least [RedactionKeySize]
// random bytes, one for a store of them: a shorter or absent key is
// refused with [ErrRedactionKey], and a lost key cannot resume what was
// redacted under it.
func Pseudonym(key []byte, id, profile string) (string, error) {
	if len(key) < RedactionKeySize {
		return "", ErrRedactionKey
	}
	return pseudonym(key, id, profile), nil
}

func pseudonym(key []byte, id, profile string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(canonicalName("agentsession/pseudonym", id, profile))
	var b [16]byte
	copy(b[:], mac.Sum(nil))
	b[6] = 0x80 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return formatUUID(b)
}

// RedactionNames derives what one redaction writes for each session it
// names (RFC 0001, The redacted session, since 0.19), under one key and
// one profile: the redacted session's ID, its redacted_from, and the ID
// that every reference into another session, and every quotation of a
// session ID in text, is rewritten to. Each is derived from the named
// session's pseudonym, so a reader can recompute the redacted session's
// ID from its header and a holder of a native ID cannot.
//
// Whether an ID is in the format's ID space, and so its own pseudonym,
// is decided of each ID apart: it is when it is the ID of an original
// that is itself a redacted session, when such an original's header
// names it, when an entry of the original carrying redacted names it in
// a member that names a session, or when the redactor can read the
// session it names, through [RedactionNames.WithResolve], and that
// session's header carries redacted. Every other ID is keyed, an ID
// named only by an entry appended natively to a redacted session among
// them, such as a child a resumed redacted session started: it was
// never published as it stands. A redaction of a redaction thus derives
// what 0.18 derived for what the redaction before it wrote.
//
// A RedactionNames is safe for concurrent use. It never prints its key:
// String, GoString and LogValue name the original and the profile alone;
// use it by pointer, as NewRedactionNames returns it.
type RedactionNames struct {
	key      []byte
	profile  string
	original string
	// redacted is whether the original is a redacted session.
	redacted bool
	// published are the IDs the original writes as a redaction wrote
	// them: its own and its header's when it is redacted, and those its
	// redacted entries name.
	published map[string]bool

	mu      sync.Mutex
	resolve func(id string) (*Session, error)
	read    map[string]bool // id -> readable and redacted, as resolve said
}

// NewRedactionNames returns the names a redaction of original writes
// under key and profile. key is the holder's secret, at least
// [RedactionKeySize] bytes, which the caller supplies and keeps; it is
// copied, and is refused with [ErrRedactionKey] when shorter or absent,
// also for an original that is itself a redacted session, so that a
// redactor never runs unkeyed by accident. profile is the one
// redacted_from is to carry. The original's entries are read once, for
// the IDs its redacted entries name; entries it gains later are not.
func NewRedactionNames(key []byte, original *Session, profile string) (*RedactionNames, error) {
	if len(key) < RedactionKeySize {
		return nil, ErrRedactionKey
	}
	if original == nil || original.ID() == "" {
		return nil, errors.New("agentsession: redaction of a session with no id")
	}
	h := original.Header()
	n := &RedactionNames{key: bytes.Clone(key), profile: profile, original: h.ID, redacted: h.Redacted, published: map[string]bool{}}
	if h.Redacted {
		n.published[h.ID], n.published[h.ParentSession] = true, true
	}
	for _, e := range original.Entries() {
		if _, ok := RedactedOf(e); ok {
			for _, id := range sessionsNamedBy(e) {
				n.published[id] = true
			}
		}
	}
	delete(n.published, "")
	return n, nil
}

// WithResolve lets the redaction read the sessions it names, resolve
// returning nil and no error for one it does not hold, and returns n,
// for chaining. A session it reads whose header carries redacted is in
// the format's ID space; and a reference to one from an original that
// is not itself redacted, as a fork of a redacted session names it in
// parent_session, names it by its own ID, since it is already a
// redaction (RFC 0001, The redacted session). Without it, only what the
// original says decides. Call it before the names are used.
func (n *RedactionNames) WithResolve(resolve func(id string) (*Session, error)) *RedactionNames {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.resolve, n.read = resolve, map[string]bool{}
	return n
}

// readRedacted reports whether the redactor can read the session id and
// its header carries redacted.
func (n *RedactionNames) readRedacted(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.resolve == nil || id == n.original {
		return false
	}
	if v, ok := n.read[id]; ok {
		return v
	}
	s, err := n.resolve(id)
	v := err == nil && s != nil && s.Header().Redacted
	n.read[id] = v
	return v
}

// Pseudonym is the pseudonym of the session ID id as this redaction
// names it: id itself when it is in the format's ID space, as
// [RedactionNames] says, and otherwise its keyed pseudonym
// ([Pseudonym]).
func (n *RedactionNames) Pseudonym(id string) string {
	if n.published[id] || n.readRedacted(id) {
		return id
	}
	return pseudonym(n.key, id, n.profile)
}

// Redaction is the ID this redaction writes for the session id in a
// reference, and in text that quotes it: the ID of that session's
// redaction under the same key and profile, whether or not anyone holds
// it; or, from an original that is not itself redacted, the ID of a
// session the redactor reads as redacted, which is a redaction already.
func (n *RedactionNames) Redaction(id string) string {
	if !n.redacted && n.readRedacted(id) {
		return id
	}
	return RedactedSessionID(n.Pseudonym(id), n.profile)
}

// ID is the redacted session's own ID: the [RedactionNames.Redaction]
// of the original.
func (n *RedactionNames) ID() string {
	return RedactedSessionID(n.Pseudonym(n.original), n.profile)
}

// RedactedFrom is the redacted_from the redacted session's header
// carries: the original's pseudonym and the profile, or no profile when
// it is "".
func (n *RedactionNames) RedactedFrom() *RedactedFrom {
	return &RedactedFrom{Session: n.Pseudonym(n.original), Profile: n.profile}
}

// String names the redaction by its original and profile, never its
// key.
func (n *RedactionNames) String() string {
	return "agentsession.RedactionNames{original: " + n.original + ", profile: " + n.profile + "}"
}

// GoString is String, so %#v does not print the key either.
func (n *RedactionNames) GoString() string { return n.String() }

// LogValue is the redaction for log/slog: its original and profile,
// never its key.
func (n *RedactionNames) LogValue() slog.Value {
	return slog.GroupValue(slog.String("original", n.original), slog.String("profile", n.profile))
}

// Session is a [Remap] Session callback: it re-points every reference
// into a session to that session's redaction, the original's own ID to
// the redacted session's.
func (n *RedactionNames) Session(id string) (string, bool) {
	return n.Redaction(id), true
}

// ReplaceIDs returns a function that replaces each occurrence of each
// of ids, as written and matched as a substring, by its
// [RedactionNames.Redaction], for a redactor to apply to text before or
// after its own rules (RFC 0001, Every redacted entry, since 0.19): ids
// are the session IDs it rewrites in references, which
// [SessionIDsNamed] reads from the original. Longer IDs are replaced
// first, so an ID that holds another is replaced whole. The function is
// deterministic, as [RewriteStrings] requires.
func (n *RedactionNames) ReplaceIDs(ids []string) func(string) string {
	ids = slices.Clone(ids)
	slices.SortFunc(ids, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a)
		}
		return strings.Compare(a, b)
	})
	ids = slices.Compact(ids)
	var pairs []string
	for _, id := range ids {
		if id != "" {
			pairs = append(pairs, id, n.Redaction(id))
		}
	}
	if len(pairs) == 0 {
		return func(s string) string { return s }
	}
	r := strings.NewReplacer(pairs...)
	return r.Replace
}

// SessionIDsNamed returns, sorted and each once, the session IDs a
// redactor of s rewrites wherever text quotes them (RFC 0001, Every
// redacted entry, since 0.19): s's own ID and each ID a member of it
// names a session by, its header's parent_session, an entry's parents
// element's session, the session of a link without a url and an item's
// copied_from session. A link carrying a url names a session in another
// system, which is not one of these. The set is read from s alone.
func SessionIDsNamed(s *Session) []string {
	h := s.Header()
	set := map[string]bool{h.ID: true, h.ParentSession: true}
	for _, e := range s.Entries() {
		for _, id := range sessionsNamedBy(e) {
			set[id] = true
		}
	}
	delete(set, "")
	return slices.Sorted(maps.Keys(set))
}

// sessionsNamedBy lists the session IDs e names in a member that names a
// session: a parents element's session, the session of a link without a
// url and a copied item's copied_from session.
func sessionsNamedBy(e Entry) []string {
	var out []string
	for _, r := range e.Base().Parents {
		out = append(out, r.Session)
	}
	switch v := e.(type) {
	case *LinkEntry:
		if v.URL == "" {
			out = append(out, v.Session)
		}
	case *ItemEntry:
		if v.CopiedFrom != nil {
			out = append(out, v.CopiedFrom.Session)
		}
	}
	return out
}

// ErrRedactedID is returned by [CheckRedactedID] for a redacted session
// whose ID is not the one its redacted_from derives.
var ErrRedactedID = errors.New("agentsession: redacted session's id is not the one its redacted_from derives")

// CheckRedactedID reports, with [ErrRedactedID], a redacted session
// whose header writes a redacted_from with a profile and whose ID is not
// [RedactedSessionID] of that redacted_from's session and profile (RFC
// 0001, The redacted session). The derivation is recommended, not
// required, so a reader MAY report the difference and does not refuse
// the file. A redacted_from written before 0.19 names the original's ID,
// which the 0.18 derivation took too, so the check reads a file of any
// minor alike. A header that is not redacted, or writes no profile, is
// not checked.
func CheckRedactedID(h Header) error {
	from := h.RedactedFrom
	if !h.Redacted || from == nil || from.Profile == "" {
		return nil
	}
	if want := RedactedSessionID(from.Session, from.Profile); h.ID != want {
		return fmt.Errorf("%w: %s, and %s derives %s", ErrRedactedID, h.ID, "redacted_from", want)
	}
	return nil
}

// uuidv5 returns the UUIDv5 of name under the nil namespace.
func uuidv5(name string) string {
	h := sha1.New()
	var ns [16]byte // the nil namespace
	h.Write(ns[:])
	h.Write([]byte(name))
	var b [16]byte
	copy(b[:], h.Sum(nil))
	b[6] = 0x50 | b[6]&0x0f
	b[8] = 0x80 | b[8]&0x3f
	return formatUUID(b)
}

func formatUUID(b [16]byte) string {
	var out [36]byte
	hex.Encode(out[:8], b[:4])
	out[8] = '-'
	hex.Encode(out[9:13], b[4:6])
	out[13] = '-'
	hex.Encode(out[14:18], b[6:8])
	out[18] = '-'
	hex.Encode(out[19:23], b[8:10])
	out[23] = '-'
	hex.Encode(out[24:], b[10:])
	return string(out[:])
}
