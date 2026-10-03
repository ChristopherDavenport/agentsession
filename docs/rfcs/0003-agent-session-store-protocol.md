# RFC 0003: Agent Session Store Protocol

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at 0.11 or later; RFC 0002,
Agent Session Store, with its Refs, Following a log and Exchange
sections.

## Summary

A store from RFC 0002 can be reached over HTTP. A client reads,
follows, writes and exchanges sessions through the same operations a
store offers in process, with the same guarantees. Nothing in a session
changes when it crosses the wire. Entries are RFC 0001's lines, objects
are RFC 0002's canonical bytes under their hashes, and every rule RFC
0002 states about appends, heads, refs, commits and exchange holds at
the server.

The protocol adds what a network needs and a process does not:
- **Leases.** A session's hold becomes a lease: it expires, and it
  carries a fencing token, so a writer that lost its lease cannot write
  after another has taken it.
- **Durability you can see.** Each change a follower receives says
  whether it is committed. A process cannot see another's fsync; a
  client is told.
- **Negotiation.** A push sends only the objects the receiver lacks.
  Objects are immutable under their hashes, so a client caches them
  forever and a cold start fetches each once.
- **Shallow reads.** A client can read a session's path with every
  envelope, which keeps the chain verifiable, and contents only from
  the newest compaction onward. That is what a resume needs, at a cost
  bounded by the context rather than the history.

## Motivation

RFC 0002 made exchange between stores a matter of rules: a push carries
a closure, the log merges as a set, the head moves by compare-and-swap,
one store is the record. It left the wire as a non-goal. Three uses now
need one.

- **Remote clients.** A terminal or dashboard on another machine
  follows a session an agent is writing. RFC 0002's following rules
  assume the reader shares the writer's file system.
- **Local-first agents.** An agent records into a local store, so
  appends cost no network round trip in the loop, and pushes to a
  shared store that others read. That is RFC 0002's exchange, done over
  a network.
- **Containerized agents.** A container's disk is ephemeral and its
  process is disposable. A container scheduled for a conversation
  resolves a ref, resumes from the record, and writes under a lease.
  The orchestrator may briefly run two copies; the fencing token makes
  sure only one writes.

## Goals

- Every store operation of RFC 0002 over HTTP, with the same results
  and the same refusals.
- Following over the network, with the committed state of each change.
- A writer that loses its lease can never write again under it, however
  late its requests arrive.
- Push and fetch that send each object a receiver lacks exactly once.
- A resume that reads the path's envelopes and the contents since the
  newest fold.
- A client library that implements RFC 0002's store, reader, follower
  and ref interfaces, so code written against a local store runs
  against a remote one unchanged.

## Non-goals

- Authentication and authorization. The protocol names what each
  request needs (read, write, refs, exchange), so a deployment can map
  its own scheme onto it. Which scheme is an open question.
- A query language. Listing is RFC 0002's `List` with its filter.
- Following every session in a store. A client follows sessions and
  refs by name.
- Multi-store transactions. A push is RFC 0002's two steps, admission
  then head and refs, and its failure modes are that document's.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are as in RFC 2119. Terms
from RFC 0001 and RFC 0002 keep their meanings.

- **Server**: an HTTP service in front of one RFC 0002 store.
- **Client**: anything that calls it: a reader, a follower, a writer,
  or another store exchanging with it.
- **Lease**: the network form of RFC 0002's hold on a session: granted
  to a holder for a time, renewed, released, and identified by a
  fencing token.
- **Fencing token**: a number the server issues with each lease of a
  session, greater than every token it issued for that session before.
- **Cursor**: RFC 0002's following position, opaque to the client and
  meaningful only to the server that issued it.

## Conventions

- All paths are under a version prefix, `/v1`. A server MUST answer
  `GET /v1/` with its capabilities: the RFC 0001 format minors it
  reads, the optional features it offers (shallow reads, exchange,
  refs, leases), and its limits.
- Request and response bodies are JSON, except object bodies, which are
  their canonical bytes (`application/octet-stream`), and streams, which
  are server-sent events (`text/event-stream`).
- An entry crosses the wire as its RFC 0001 line: the same bytes a
  projection holds, hashed the same way. A server MUST verify every
  entry it receives by recomputing its hash, as an append does.
- Errors are `application/problem+json` (RFC 9457) with a stable `code`.
  The codes are listed under each operation, and a client branches on
  `code`, never on the HTTP status alone. Each code is the error RFC
  0002 names: `no_session`, `no_entry`, `head_moved`, `ref_moved`,
  `ref_name`, `session_locked`, `lease_lost`, `not_record`,
  `bad_entry`, `format`.
- Sessions, refs and objects are path segments. A ref name's `/` is
  percent-encoded.

## Objects

```
GET  /v1/objects/{space}/{hash}
HEAD /v1/objects/{space}/{hash}
PUT  /v1/objects/{space}/{hash}
POST /v1/objects/missing
```

`space` is `entries` (envelopes), `contents` or `media`.

- A `GET` returns the object's canonical bytes. An object never
  changes under its hash, so a server SHOULD mark the response
  immutable and cacheable (`Cache-Control: public, max-age=31536000,
  immutable`), and a client SHOULD cache it for good. A missing object
  is 404 with `no_entry`.
- A `PUT` (exchange scope) uploads an object. The server MUST verify
  that the bytes hash to the name and refuse them with `bad_entry`
  otherwise. Uploading an object it holds is a no-op that succeeds. An
  uploaded object is reachable from nothing until a push names it, and
  is collected as RFC 0002's sweep collects any unreferenced object.
- `POST /v1/objects/missing` takes `{"entries": [...], "contents":
  [...], "media": [...]}` and returns the hashes the server lacks, in
  the same shape. It is the negotiation RFC 0002 allows a sender ("MAY
  negotiate what the receiver lacks").

## Sessions

```
GET    /v1/sessions?…filter…
POST   /v1/sessions
GET    /v1/sessions/{id}
DELETE /v1/sessions/{id}
```

- **List** streams RFC 0002 summaries as server-sent events, newest
  first, with the filter's members as query parameters. A client stops
  reading when it has enough.
- **Create** takes a header (RFC 0001) and returns the session with the
  header the server filled. A header naming a `base` makes a fork, under
  RFC 0002's creation rules, including its refusals. Creating an ID the
  server holds under another header is 409 `session_exists`; under the
  same header it returns the existing session, so a create can be
  retried.
- **Get** returns the header, the head, the record mark, the log's
  length and its current cursor. It is not a read of the entries.
- **Delete** needs a lease, below, and is RFC 0002's delete.

## Reading

```
GET /v1/sessions/{id}/log?after={cursor}&durable={bool}
GET /v1/sessions/{id}/path?leaf={entry}&contents={all|since-fold|none}
```

- **The log** is the session's records in log order after a cursor
  (from the start without one), as newline-delimited JSON. Each record
  names its entry by hash, says whether it is a head move, and carries
  the cursor after it and `durable`, whether the server has committed
  it. With `durable=true` the server stops before the first record it
  has not committed: that is the reading RFC 0002's exchange requires
  ("a sender MUST commit the session before it pushes or serves it"). A
  client fetches the envelopes and contents it lacks through Objects,
  and builds the session as RFC 0002's reader does.
- **The path** is the entries from the root to `leaf` (the head by
  default), as a list of envelope hashes, plus the contents according
  to `contents`.
  - `since-fold`: contents only for the entries from the newest
    `compaction` on the path onward, plus the compaction's own checkpoint.
    The envelopes above it still come, so the chain verifies; their
    contents do not. This is what a resume needs: RFC 0001 rebuilds
    the context from the newest fold, and a harness that resumes reads
    nothing above it. It is RFC 0002's open question on shallow
    boundaries, answered for reading rather than for storage. A client
    that later needs an omitted content fetches it by hash.
  - `none`: envelopes only, for verification or a tree view.

  A client MUST NOT treat a shallow path as the session: a projection
  of it is not an RFC 0001 file, since the file holds every entry's
  body. Its library returns a shallow session that reports which
  contents it lacks.

## Following

```
GET /v1/sessions/{id}/follow?after={cursor}&durable={bool}
GET /v1/refs/{name}/follow
```

Following is RFC 0002's following a log, carried as server-sent events.
The server follows the log in process and fans the changes out, so N
clients cost the store one follower, not N.

- The events are `snapshot`, `appended`, `head` and `reset`, as in the
  reference implementation's `Follow`. A `snapshot` or `reset` carries
  the log from the start, as the log read above does. An `appended`
  carries the record. A `head` carries the leaf. Every event carries the
  cursor after it as its SSE `id`, so a reconnecting client resumes with
  `Last-Event-ID`. A cursor the server can no longer resume from gives a
  `reset`, never a silent skip.
- Every `appended` and `head` carries `durable`. A record seen as not
  durable that a crash or a failed commit takes back is answered with a
  `reset`, as RFC 0002 says of a local follower. With `durable=true` the
  server sends only committed records, at the cost of latency under a
  lazy commit policy.
- A deleted session ends the stream with an `error` event, `no_session`.
  A session created again under the same ID is another session: a
  reconnecting client gets `no_session` for its cursor, and starts over
  if it wants the new one.
- Following a ref (`/v1/refs/{name}/follow`) sends a `target` event
  each time the ref moves, with the session and entry and the identity
  check's result (live or dangling). A client following a conversation
  follows its ref, and switches to following the session it points to.
- A server SHOULD send a comment line every 15 seconds on an idle
  stream, so intermediaries do not close it.

## Leases

```
POST   /v1/sessions/{id}/lease
PUT    /v1/sessions/{id}/lease/{token}
DELETE /v1/sessions/{id}/lease/{token}
```

A lease is RFC 0002's hold, over a network. A process's hold ends when
the process does; a client's cannot be seen to end, so it expires.

- **Acquire** takes `{"holder": "...", "ttl_ms": n}` and returns
  `{"token": t, "expires": time}`, or 409 `session_locked` with the
  current holder and expiry. The server MAY cap `ttl_ms`.
- **Renew** extends the lease. Renewing an expired or superseded lease
  is 409 `lease_lost`.
- **Release** ends it at once.
- **Fencing.** Every write to a session (append, commit, head move,
  delete) MUST carry the lease's token in a `Lease-Token` header, and
  the server MUST refuse with `lease_lost` a write whose token is not
  the session's current lease. Because tokens only increase, a writer
  whose lease expired and was taken, whose request is delayed in the
  network, cannot write after the new holder has. Expiry alone could
  not promise that; the token does.
- A server whose store also has local writers MUST make leases and its
  local holds one mechanism: a session held locally cannot be leased,
  and a leased session cannot be held locally.
- What a writer does on `lease_lost` is a harness's choice. A resumable
  harness acquires again, resumes from the record, and continues, which
  is how a container replaced by its orchestrator picks up.

## Writing

```
POST /v1/sessions/{id}/entries
POST /v1/sessions/{id}/commit
PUT  /v1/sessions/{id}/head
```

All three need a lease.

- **Append** takes one entry line, or several as newline-delimited
  lines appended in order, and returns, for each, its ID, RFC 0002's
  outcome (continued, branched, held, head moved) and whether it is
  durable. An entry whose objects the server lacks (a `media` blob it
  names) is accepted as RFC 0002 accepts one, with the missing hashes
  reported. Refusals are RFC 0002's: `bad_entry`, `no_entry` for a
  parent the session does not hold, `format`, and `not_record` on a
  mirror.
- **Commit** makes the session's working state durable and returns the
  cursor through which everything is committed. An append MAY ask for
  a commit with `?commit=true`.
- **Head** is the compare-and-swap: `{"expected": e, "to": t}`, with
  `head_moved` and the current head on a mismatch.

A client that writes to a remote store pays a round trip per append. A
harness that cannot afford that in its loop writes to a local store and
pushes, below.

## Refs

```
GET    /v1/refs?prefix={p}
GET    /v1/refs/{name}
PUT    /v1/refs/{name}
GET    /v1/refs/{name}/log
```

RFC 0002's refs, unchanged.

- **Resolve** returns the target, and for a dangling ref the target
  with `no_session` (the session is gone or another now holds its ID),
  so a client can see which session it was.
- **Update** takes `{"expected": target|null, "next": target|null,
  "reason": "..."}`: compare-and-swap, `null` being "no ref". A
  mismatch is 409 `ref_moved` with the current target. RFC 0002's
  refusals apply, the refusal of a same-target update on a dangling ref
  included. A ref update needs refs scope, not a lease: a ref is the
  store's state, not a session's.
- **List** and **log** stream as server-sent events, in name order and
  newest first respectively.
- `SessionFor` of the reference library works unchanged over these
  calls: create, then claim the ref with expected `null`; the loser
  opens the winner's session and deletes its own.

## Exchange

```
POST /v1/sessions/{id}/push
```

A push is RFC 0002's, over the wire.

1. The sender asks which objects of the closure the receiver lacks
   (`POST /v1/objects/missing`), and uploads them (`PUT`).
2. It posts `{"header": …, "entries": [own entry hashes in log order],
   "prefix": [prefix entry hashes], "head": {"expected": e, "to": t},
   "record": "mirror"|"handover", "refs": [{"name", "expected",
   "next"}]}`.
3. The server admits the closure as RFC 0002 says: hash verified,
   parent first, the log merged as a set, the `call_id` collision
   refused. Only then does it take the head step and the ref steps.
4. The response reports admission, the head step, and each ref's
   outcome separately, because a refused head or ref undoes nothing
   admitted.

The record mark's rules are RFC 0002's. A push from a store that is not
the record is `not_record`. A handover sets the mark at the receiver
before the sender clears its own.

A **fetch** needs nothing new. The fetcher reads the log with
`durable=true` and the objects it lacks, admits them in its own store,
and moves its head and refs under RFC 0002's fetch rules. The server
does not know a fetch happened, which is how a mirror can serve one.

**Local-first writers** combine the two. A harness records into its own
store with no network in the loop, and pushes at commits, on a timer,
and on shutdown (a container's grace period), each push carrying the
conversation's ref. What a crash can lose is what the harness had not
yet pushed, which it chooses by how often it pushes. Followers read the
server.

## Security considerations

- Everything a session holds, every prompt, tool output and model
  response, is readable by anyone with read scope. A deployment MUST
  authenticate and authorize every request, and SHOULD separate read,
  write (lease), refs and exchange scopes.
- Objects are named by hash. Knowing a hash is not authorization to
  read it: a server MUST check that the caller may read some session
  that reaches the object, or MUST NOT serve objects across tenants.
- A server MUST verify every entry and object it receives. Nothing a
  client sends is taken on trust except what RFC 0002 already takes on
  trust from a pushing store: the head and the record mark, until heads
  are signed.
- An approval in a session (a decision `by` a human) records a string,
  not an authenticated identity. A server that admits writes from many
  principals SHOULD record the authenticated principal of each append
  beside it, in store state rather than in the entry, since the entry's
  hash is the harness's.

## Reference implementation

`agentsession/remote`, a nested module:
- **A server** that wraps any store the reference library offers
  (memory, jsonl, cas, sqlite, and a database store behind it), with
  following fanned out from one in-process follower per session, and
  leases backed by the store's holds.
- **A client** that implements `Store`, `Reader`, `Follower` and
  `RefStore` over these calls, with an object cache keyed by hash.
- **A push helper** for local-first writers.

## Open questions

- **Authentication.** Bearer tokens with scopes are the simple answer;
  mTLS for store-to-store exchange is the likely second. The protocol
  stays agnostic until a deployment chooses.
- **Signed heads and refs.** RFC 0002 defers signing. A protocol
  between organisations needs it. The fields would be a signature over
  the head (or ref) move by the store of record, verified by the
  receiver.
- **Following many sessions.** A store-wide feed of ref moves would let
  a dashboard watch every conversation without a stream per session. It
  wants an index the stores do not keep.
- **The principal of an append.** Whether a server records who appended
  each entry, beside it in store state, as the security section
  suggests, or whether that belongs in the format.
- **Shallow storage.** `since-fold` answers shallow boundaries for
  reading. A store that holds envelopes above a fold without their
  contents, which RFC 0002 leaves open, would let a receiver of a push
  do the same and keep long sessions cheap on every mirror.
