package agentsession

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/ChristopherDavenport/agentsession/internal/ijson"
	"github.com/ChristopherDavenport/agentsession/internal/jcs"
	"github.com/ChristopherDavenport/openresponses"
)

// HashPrefix precedes the hexadecimal digest in a request hash.
const HashPrefix = "sha256:"

// RequestHash computes the request hash the format defines: the request
// serialised with the JSON Canonicalization Scheme (RFC 8785) and
// digested with SHA-256, rendered as "sha256:" plus lowercase hex. The
// input must be the request as it was sent, passthrough members
// included, because those reached the model.
func RequestHash(req openresponses.Request) (string, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("agentsession: encode request: %w", err)
	}
	return HashRequestJSON(data)
}

// HashText returns the hash of a string in the format's notation,
// which is [HashBytes] over its UTF-8 bytes: [HashPrefix] and the
// lowercase hexadecimal SHA-256. It is how a config delta names the
// text of an instructions part it does not repeat, so a reader can
// tell the part it already has from one that changed.
func HashText(s string) string { return HashBytes([]byte(s)) }

// HashRequestJSON computes the request hash of an already-encoded
// request. Member order and whitespace in data do not matter.
func HashRequestJSON(data []byte) (string, error) {
	canonical, err := jcs.Transform(data)
	if err != nil {
		return "", fmt.Errorf("agentsession: canonicalise request: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return HashPrefix + hex.EncodeToString(sum[:]), nil
}

// CanonicalTime renders t in the one form the format admits for ts:
// UTC, uppercase T and Z, a fractional part only when non-zero, with
// no trailing zeros and at most nine digits. It is what Go's
// RFC3339Nano layout produces for a UTC time, so a time.Time round
// trips unchanged, which is the property the rule was chosen for.
func CanonicalTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// ParseCanonicalTime parses s and reports whether it is spelled in the
// canonical form: a spelling the format admits renders back to itself.
func ParseCanonicalTime(s string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, CanonicalTime(t) == s
}

// EntryHashes computes an entry's two hashes from its encoded form, a
// JSON object with the envelope members in it: the content hash over the
// body, the members outside the envelope, and the id over the envelope
// object — type, parent, parents when present and non-empty, ts, and
// content holding the content hash. The id member in data, if any, is
// ignored; ts is taken as the string it is. Both are "sha256:" plus
// lowercase hex over the canonical bytes.
func EntryHashes(data []byte) (id, content string, err error) {
	// The test runs on the line as written: the decoder would repair a
	// duplicate member, a lone surrogate or invalid UTF-8 on its way to
	// the structs, and the format says a reader reports them.
	if err := ijson.Check(data); err != nil {
		return "", "", fmt.Errorf("agentsession: entry: %w", err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return "", "", fmt.Errorf("agentsession: entry: %w", err)
	}
	body := make(map[string]json.RawMessage, len(all))
	for k, v := range all {
		if !isEnvelopeKey(k) {
			body[k] = v
		}
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return "", "", err
	}
	if err := ijson.Check(bodyJSON); err != nil {
		return "", "", fmt.Errorf("agentsession: entry body: %w", err)
	}
	content, err = HashRequestJSON(bodyJSON)
	if err != nil {
		return "", "", err
	}
	env := map[string]json.RawMessage{
		"type":    all["type"],
		"parent":  all["parent"],
		"ts":      all["ts"],
		"content": json.RawMessage(strconv.Quote(content)),
	}
	if env["parent"] == nil {
		env["parent"] = json.RawMessage("null")
	}
	if p, ok := all["parents"]; ok {
		// Decided on the value, not the spelling: an empty array however
		// written is omitted, as a conforming writer omits it.
		var refs []json.RawMessage
		if json.Unmarshal(p, &refs) == nil && len(refs) > 0 {
			env["parents"] = p
		}
	}
	envJSON, err := json.Marshal(env)
	if err != nil {
		return "", "", err
	}
	id, err = HashRequestJSON(envJSON)
	if err != nil {
		return "", "", err
	}
	return id, content, nil
}

// ValidHash reports whether s is a hash in the format's notation:
// "sha256:" followed by exactly 64 lowercase hexadecimal characters.
// Anything a store puts on a filesystem as a hash is checked with it.
func ValidHash(s string) bool {
	if len(s) != len(HashPrefix)+64 || s[:len(HashPrefix)] != HashPrefix {
		return false
	}
	for _, c := range s[len(HashPrefix):] {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isEnvelopeKey reports whether k is one of the envelope's names, which
// are reserved: a body MUST NOT carry a top-level member by any of them.
func isEnvelopeKey(k string) bool {
	switch k {
	case "id", "type", "parent", "parents", "ts", "content":
		return true
	}
	return false
}

// entryHashesCanonical is EntryHashes for bytes already in canonical
// form, a compact object with its members sorted as JCS writes one: the
// body and the envelope are sliced out of it rather than decoded and
// canonicalised again, which is most of what hashing a large line
// costs. What it cannot slice goes to EntryHashes. c must have passed
// the I-JSON test already, as Read tests every line it reads; testing
// it again here was a fifth of what reading a file cost.
func entryHashesCanonical(c []byte) (id, content string, err error) {
	members, ok := canonicalMembers(c)
	if !ok {
		return EntryHashes(c)
	}
	id, content = memberHashes(c, members)
	return id, content, nil
}

// memberHashes is entryHashesCanonical for c already sliced into its
// members.
func memberHashes(c []byte, members []canonicalMember) (id, content string) {
	body := make([]byte, 0, len(c))
	body = append(body, '{')
	env := map[string][]byte{}
	for _, m := range members {
		if isEnvelopeKey(m.key) {
			env[m.key] = m.val
			continue
		}
		if len(body) > 1 {
			body = append(body, ',')
		}
		body = append(body, m.raw...)
	}
	body = append(body, '}')
	sum := sha256.Sum256(body)
	content = HashPrefix + hex.EncodeToString(sum[:])
	orNull := func(v []byte) []byte {
		if v == nil {
			return []byte("null")
		}
		return v
	}
	envJSON := make([]byte, 0, 256)
	envJSON = append(envJSON, `{"content":"`...)
	envJSON = append(envJSON, content...)
	envJSON = append(envJSON, `","parent":`...)
	envJSON = append(envJSON, orNull(env["parent"])...)
	if p, ok := env["parents"]; ok {
		// Decided on the value, as EntryHashes decides it.
		var refs []json.RawMessage
		if json.Unmarshal(p, &refs) == nil && len(refs) > 0 {
			envJSON = append(envJSON, `,"parents":`...)
			envJSON = append(envJSON, p...)
		}
	}
	envJSON = append(envJSON, `,"ts":`...)
	envJSON = append(envJSON, orNull(env["ts"])...)
	envJSON = append(envJSON, `,"type":`...)
	envJSON = append(envJSON, orNull(env["type"])...)
	envJSON = append(envJSON, '}')
	sum = sha256.Sum256(envJSON)
	return HashPrefix + hex.EncodeToString(sum[:]), content
}

// canonicalMember is one member of a compact object: its key, the whole
// "key":value span and the value's span.
type canonicalMember struct {
	key      string
	raw, val []byte
}

// canonicalMembers slices a compact JSON object into its members, in the
// order they are written. It reports false for anything else.
func canonicalMembers(c []byte) ([]canonicalMember, bool) {
	if len(c) < 2 || c[0] != '{' || c[len(c)-1] != '}' {
		return nil, false
	}
	var out []canonicalMember
	i := 1
	if c[i] == '}' {
		return out, i == len(c)-1
	}
	for {
		if i >= len(c) || c[i] != '"' {
			return nil, false
		}
		keyEnd := skipString(c, i)
		if keyEnd < 0 || keyEnd >= len(c) || c[keyEnd] != ':' {
			return nil, false
		}
		var key string
		if bytes.IndexByte(c[i:keyEnd], '\\') >= 0 {
			if json.Unmarshal(c[i:keyEnd], &key) != nil {
				return nil, false
			}
		} else {
			key = string(c[i+1 : keyEnd-1])
		}
		v := keyEnd + 1
		end := skipValue(c, v)
		if end < 0 || end >= len(c) {
			return nil, false
		}
		out = append(out, canonicalMember{key: key, raw: c[i:end], val: c[v:end]})
		switch c[end] {
		case ',':
			i = end + 1
		case '}':
			return out, end == len(c)-1
		default:
			return nil, false
		}
	}
}

// skipString returns the index just past the string starting at c[i],
// or -1.
func skipString(c []byte, i int) int {
	for j := i + 1; j < len(c); j++ {
		switch c[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return -1
}

// skipValue returns the index just past the compact value starting at
// c[i], or -1.
func skipValue(c []byte, i int) int {
	if i >= len(c) {
		return -1
	}
	switch c[i] {
	case '"':
		return skipString(c, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(c); j++ {
			switch c[j] {
			case '"':
				k := skipString(c, j)
				if k < 0 {
					return -1
				}
				j = k - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i
	for j < len(c) && c[j] != ',' && c[j] != '}' && c[j] != ']' {
		j++
	}
	return j
}
