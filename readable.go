package agentsession

import "context"

type readableKey struct{}

// WithReadable returns a context that carries readable, which reports
// whether the caller a store works for may read a session, as a server
// that authorizes per caller knows (RFC 0003, Security considerations).
// A store that finds the session a fork continues from, at a create, an
// import or a push, considers only sessions readable admits: one it does
// not admit is as one the store does not hold, so a refusal, or a header
// the store fills from the origin, says nothing of a session the caller
// may not read. A store asks readable holding none of its own locks, so
// readable may read the store it guards, as an authorizer that finds a
// session's owner in its header does. A context without it admits every
// session.
func WithReadable(ctx context.Context, readable func(session string) bool) context.Context {
	return context.WithValue(ctx, readableKey{}, readable)
}

// ReadableIn returns the filter [WithReadable] put in ctx, or one that
// admits every session.
func ReadableIn(ctx context.Context) func(session string) bool {
	if f, ok := ctx.Value(readableKey{}).(func(string) bool); ok && f != nil {
		return f
	}
	return func(string) bool { return true }
}
