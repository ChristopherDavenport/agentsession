package remote_test

import (
	"net/http"
	"strings"

	"github.com/ChristopherDavenport/agentsession/remote"
)

// tokenAuth is the tests' authenticator, standing in for a program's: a
// request whose Authorization header is "Bearer <token>" for a token in
// tokens is that token's principal, and any other is unauthenticated.
func tokenAuth(tokens map[string]remote.Principal) remote.Authenticator {
	return func(r *http.Request) (remote.Principal, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		p, known := tokens[tok]
		if !ok || tok == "" || !known {
			return remote.Principal{}, remote.ErrUnauthenticated
		}
		return p, nil
	}
}

// bearerTransport adds token to every request as a program's transport
// would, over next, or the default transport when next is nil.
type bearerTransport struct {
	token string
	next  http.RoundTripper
}

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	next := b.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// withToken is the client option that sends token through
// [bearerTransport] over next.
func withToken(token string, next http.RoundTripper) remote.ClientOption {
	return remote.WithHTTPClient(&http.Client{Transport: bearerTransport{token, next}})
}
