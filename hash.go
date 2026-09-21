package agentsession

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

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

// HashText returns the hash of a string in the format's notation:
// [HashPrefix] and the lowercase hexadecimal SHA-256 of its UTF-8
// bytes. It is how a config delta names the text of an instructions
// part it does not repeat, so a reader can tell the part it already
// has from one that changed.
func HashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return HashPrefix + hex.EncodeToString(sum[:])
}

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
