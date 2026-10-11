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
//	GET    /v1/sessions/{id}/path           the path, NDJSON (contents=all, since-fold or none)
//	GET    /v1/sessions/{id}/log            the log's records after a cursor, NDJSON; durable=true
//	GET    /v1/sessions/{id}/follow         following, server-sent events; durable=true
//	POST   /v1/sessions/{id}/lease          acquire; PUT renews, DELETE releases
//	POST   /v1/sessions/{id}/lease/break    end another holder's lease (admin)
//	POST   /v1/sessions/{id}/entries        append, NDJSON lines, under the lease
//	POST   /v1/sessions/{id}/commit         commit, where the store has one
//	PUT    /v1/sessions/{id}/head           the head's compare-and-swap
//	GET    /v1/refs?prefix=                 ref listing, NDJSON
//	GET    /v1/refs/{name}                  resolve; PUT is the ref's compare-and-swap
//	GET    /v1/refs/{name}?view=log         the ref's log, NDJSON
//	GET    /v1/refs/{name}?view=follow      the ref's target as it moves, server-sent events
//	GET    /v1/objects/{space}/{hash}       an object; HEAD too; PUT uploads one
//	POST   /v1/objects/missing              which objects the caller cannot reach
//	POST   /v1/sessions/{id}/push           RFC 0002's push, admitted here
//	POST   /v1/sessions/{id}/record         declare this server the record
//	POST   /v1/sessions/{id}/handover       hand the session to a client store
//
// Every write to a session carries its lease's fencing token, and one
// whose lease is not the session's current one is refused, so a writer
// that lost its lease cannot write after the next holder. A push is not
// leased: it is governed by the record mark, as RFC 0002's exchange is,
// and the objects and exchange routes need a store with object spaces
// and marks (the cas store). The log read and durable=true need a store
// that keeps a log of records with a commit (the cas store); a shallow
// path is computed from the session, over any store.
//
// One follower per session is shared by every client that follows it,
// and a client that falls behind is cut with an `overflow` error and
// resumes from the last event it read. A snapshot carries entries up to
// [WithSnapshotLimit]; past it a server with objects sends the records
// alone, which the client reads as objects, and one without refuses.
//
// [Dial] returns a [Client] that is the library's [agentsession.Store],
// [agentsession.Writer], [agentsession.Reader], [agentsession.Follower]
// and [agentsession.RefStore] over these calls. It verifies every entry
// it receives by its hash, treats a stream cut before its terminal frame
// as an error, never as a short result, and writes under leases it
// renews until it lets them go. [Push] sends a session a local cas store
// is the record for, uploading only the objects the server lacks: the
// helper of a local-first writer; [Fetch] reads one from the server's
// committed log into a local cas store, and [TakeBack] makes the local
// store the record again.
package remote
