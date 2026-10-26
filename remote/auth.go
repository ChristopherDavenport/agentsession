package remote

import (
	"context"
	"net/http"
	"slices"
)

// Scope is what a principal may do through a [Handler]: RFC 0003's
// read, write (lease), refs, exchange and admin.
type Scope string

const (
	// ScopeRead reads and follows sessions and refs.
	ScopeRead Scope = "read"
	// ScopeWrite creates sessions, takes leases on them and, under a
	// lease, appends, moves the head, commits and deletes.
	ScopeWrite Scope = "write"
	// ScopeRefs updates refs. A ref is the store's state, not a
	// session's, so an update needs this and no lease.
	ScopeRefs Scope = "refs"
	// ScopeExchange uploads objects, asks which a push needs, pushes a
	// session (RFC 0002's exchange, over the wire) and declares this
	// server the record for a session it holds as a mirror. A push's
	// refs need ScopeRefs as well.
	ScopeExchange Scope = "exchange"
	// ScopeAdmin breaks a lease another holder has.
	ScopeAdmin Scope = "admin"
)

// AllScopes are every scope this server checks.
var AllScopes = []Scope{ScopeRead, ScopeWrite, ScopeRefs, ScopeExchange, ScopeAdmin}

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
//
// The package ships none: it says which scope each route needs and
// refuses a request whose principal lacks it, and the program decides
// who a request is from, by whatever credentials it issues, and passes
// that decision to [WithAuthenticator]. An authenticator that finds no
// credentials it accepts returns [ErrUnauthenticated], or any error.
type Authenticator func(r *http.Request) (Principal, error)

type principalKey struct{}

// PrincipalFrom returns the principal a [Handler] resolved for the
// request whose context ctx is.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
