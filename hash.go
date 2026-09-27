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
	if p, ok := all["parents"]; ok && len(bytes.TrimSpace(p)) > 0 && string(bytes.TrimSpace(p)) != "null" && string(bytes.TrimSpace(p)) != "[]" {
		env["parents"] = p
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

// isEnvelopeKey reports whether k is one of the envelope's names, which
// are reserved: a body MUST NOT carry a top-level member by any of them.
func isEnvelopeKey(k string) bool {
	switch k {
	case "id", "type", "parent", "parents", "ts", "content":
		return true
	}
	return false
}
