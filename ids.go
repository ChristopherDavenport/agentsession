package agentsession

import (
	"crypto/rand"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
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

// RedactedSessionID derives the session ID the format recommends for a
// redacted session whose redacted_from writes a profile: a UUIDv5
// under the nil namespace over the name ["redacted","<original>","<profile>"],
// a JSON array in the canonical form the entry hash uses, original
// being the original's session ID. An array cannot be read two ways, as
// a slash-joined name can when an ID holds a slash, and it is not a
// subsession's name, which starts with a session ID. One original under
// one profile then has one redacted session, whose ID stays as it
// grows, so a redaction repeated over a store finds the session it made
// before and appends only what the original added since. A profile
// names one set of rules, since two under one name write into one
// session. A string that is not UTF-8 is encoded as the JSON encoder
// does, its invalid bytes as U+FFFD.
func RedactedSessionID(original, profile string) string {
	return uuidv5(string(redactedName(original, profile)))
}

// redactedName is the name [RedactedSessionID] hashes.
func redactedName(original, profile string) []byte {
	data, err := jsonx.MarshalNoEscape([]string{"redacted", original, profile})
	if err == nil {
		data, err = jcs.Transform(data)
	}
	if err != nil {
		// Strings always encode, and the encoding is JSON.
		panic(fmt.Sprintf("agentsession: redacted session name: %v", err))
	}
	return data
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
