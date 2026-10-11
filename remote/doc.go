// Package remote is the read side of RFC 0003, the Agent Session Store
// Protocol: a store reached over HTTP, for a reader or a follower in
// another process or on another machine.
//
// [NewHandler] serves a store the library offers. It publishes what the
// store supports at GET /v1/ and answers the rest with 501
// `unsupported`, as RFC 0003's Conventions say of a store that lacks a
// feature:
//
//	GET /v1/                          capabilities
//	GET /v1/sessions?…filter…         list, NDJSON ending with end
//	GET /v1/sessions/{id}             header, head, length, incarnation, cursor
//	GET /v1/sessions/{id}/path        the path, NDJSON (contents=all or none)
//	GET /v1/sessions/{id}/follow      following, server-sent events
//	GET /v1/refs?prefix=              ref listing, NDJSON
//	GET /v1/refs/{name}               resolve
//	GET /v1/refs/{name}?view=log      the ref's log, NDJSON
//
// Leases, writing, objects and negotiation, exchange, the log read and
// ref following are not in this slice.
//
// One follower per session is shared by every client that follows it,
// and a client that falls behind is cut with an `overflow` error and
// resumes from the last event it read.
//
// [Dial] returns a [Client] that implements [agentsession.Reader],
// [agentsession.Follower] and [agentsession.RefStore]'s reads over these
// calls. It verifies every entry it receives by its hash, and treats a
// stream cut before its terminal frame as an error, never as a short
// result.
package remote
