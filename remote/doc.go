// Package remote is RFC 0003, the Agent Session Store Protocol: a store
// reached over HTTP, for a reader, a follower or a writer in another
// process or on another machine.
//
// [NewHandler] serves a store the library offers. It publishes what the
// store supports at GET /v1/ and answers the rest with 501
// `unsupported`, as RFC 0003's Conventions say of a store that lacks a
// feature:
//
//	GET    /v1/                             capabilities
//	GET    /v1/sessions?…filter…            list, NDJSON ending with end
//	POST   /v1/sessions[?lease_ttl_ms=n]    create, leased from its first instant
//	GET    /v1/sessions/{id}                header, head, length, incarnation, cursor
//	DELETE /v1/sessions/{id}                delete, under the lease
//	GET    /v1/sessions/{id}/path           the path, NDJSON (contents=all or none)
//	GET    /v1/sessions/{id}/follow         following, server-sent events
//	POST   /v1/sessions/{id}/lease          acquire; PUT renews, DELETE releases
//	POST   /v1/sessions/{id}/lease/break    end another holder's lease (admin)
//	POST   /v1/sessions/{id}/entries        append, NDJSON lines, under the lease
//	POST   /v1/sessions/{id}/commit         commit, where the store has one
//	PUT    /v1/sessions/{id}/head           the head's compare-and-swap
//	GET    /v1/refs?prefix=                 ref listing, NDJSON
//	GET    /v1/refs/{name}                  resolve; PUT is the ref's compare-and-swap
//	GET    /v1/refs/{name}?view=log         the ref's log, NDJSON
//
// Every write to a session carries its lease's fencing token, and one
// whose lease is not the session's current one is refused, so a writer
// that lost its lease cannot write after the next holder. Objects and
// negotiation, exchange and push, declare-record, the log read, shallow
// reads, ref following and the committed view are not served yet.
//
// One follower per session is shared by every client that follows it,
// and a client that falls behind is cut with an `overflow` error and
// resumes from the last event it read.
//
// [Dial] returns a [Client] that is the library's [agentsession.Store],
// [agentsession.Writer], [agentsession.Reader], [agentsession.Follower]
// and [agentsession.RefStore] over these calls. It verifies every entry
// it receives by its hash, treats a stream cut before its terminal frame
// as an error, never as a short result, and writes under leases it
// renews until it lets them go.
package remote
