package remote

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"slices"
	"strings"
)

// Scope is what a principal may do through a [Handler]. RFC 0003 names
// read, write (lease), refs, exchange and admin; this slice serves reads
// alone.
type Scope string

// ScopeRead reads and follows sessions and refs.
const ScopeRead Scope = "read"

// Principal is who a request is from, as an [Authenticator] resolved
// it, and what they may do.
type Principal struct {
	Name   string
	Scopes []Scope
}

// Has reports whether p holds s.
func (p Principal) Has(s Scope) bool { return slices.Contains(p.Scopes, s) }

// Authenticator resolves the principal a request is from. An error
// refuses the request as `auth`; its text is not sent.
type Authenticator func(r *http.Request) (Principal, error)

// BearerTokens is an [Authenticator] over static bearer tokens: a
// request whose Authorization header is "Bearer <token>" for a token in
// tokens is that token's principal. Tokens are compared in constant
// time, through their SHA-256, so neither their contents nor their
// lengths leak through timing.
func BearerTokens(tokens map[string]Principal) Authenticator {
	type entry struct {
		sum [sha256.Size]byte
		p   Principal
	}
	entries := make([]entry, 0, len(tokens))
	for tok, p := range tokens {
		if tok == "" {
			continue
		}
		entries = append(entries, entry{sha256.Sum256([]byte(tok)), p})
	}
	return func(r *http.Request) (Principal, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			return Principal{}, ErrUnauthenticated
		}
		sum := sha256.Sum256([]byte(tok))
		var found Principal
		matched := 0
		for _, e := range entries {
			if subtle.ConstantTimeCompare(sum[:], e.sum[:]) == 1 {
				found, matched = e.p, 1
			}
		}
		if matched == 0 {
			return Principal{}, ErrUnauthenticated
		}
		return found, nil
	}
}

type principalKey struct{}

// PrincipalFrom returns the principal a [Handler] resolved for the
// request whose context ctx is.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
