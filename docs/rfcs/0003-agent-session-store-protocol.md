# RFC 0003: Agent Session Store Protocol

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at 0.11 or later; RFC 0002,
Agent Session Store, with its Refs, Following a log and Exchange
sections.

## Summary

A store from RFC 0002 can be reached over HTTP. A client reads,
follows, writes and exchanges sessions through the same rules a store
applies in process. Nothing in a session changes when it crosses the
wire. Entries are RFC 0001's lines, objects are RFC 0002's canonical
bytes under their hashes, and every rule RFC 0002 states about appends,
heads, refs, commits and exchange holds at the server.

The protocol adds what a network needs and a process does not:
- **Leases.** A session's hold becomes a lease: it expires, and it
  carries a fencing token, so a writer that lost its lease cannot write
  through the server after another has taken it.
- **Durability you can see.** Each change a follower receives says
  whether it is committed, and a later event says when it became so. A
  process cannot see another's fsync; a client is told.
- **Negotiation.** A push sends only the objects the receiver cannot
  already show the sender may read. Objects are immutable under their
  hashes, so a client caches them and a cold start fetches each once.
- **Shallow reads.** A client can read a session's path with every
  envelope, which keeps the chain verifiable, and the item contents only
  from the newest fold's kept span onward. That is what rebuilding the
  model's request needs, at a cost bounded by the context rather than
  the history.

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
  a network. The client is the record and the server a mirror.
- **Containerized agents.** A container's disk is ephemeral and its
  process is disposable. A container scheduled for a conversation
  resolves a ref, resumes from the record, and writes under a lease.
  The server is the record. The orchestrator may briefly run two copies;
  the fencing token makes sure only one writes through the server.

The two writing stories are alternatives for one session, not
partners: see Which store is the record.

## Goals

- Every store operation RFC 0002 requires, over HTTP, with the same
  results, and each refusal under a code of its own.
- Following over the network, with the committed state of each change.
- A writer that loses its lease cannot append, commit, move a head or
  delete through the server again under it, however late its requests
  arrive. The fence is on the server's writes. It does not reach a
  tool's side effects, which a stalled harness may still perform.
- Push and fetch that send each object a receiver cannot already show
  the sender to hold exactly once.
- A resume that reads the path's envelopes and the contents needed to
  rebuild the request since the newest fold.
- A client library that implements the reference library's store,
  reader, follower and ref interfaces over these calls (see Reference
  implementation for what that does and does not give a caller).

## Non-goals

- Authentication and authorization schemes. The protocol names what each
  request needs (read, write, refs, exchange), so a deployment can map
  its own scheme onto it. Which scheme is an open question.
- A query language. Listing is the reference library's `List` with its
  filter.
- Following every session in a store. A client follows sessions and
  refs by name.
- Multi-store transactions. A push is RFC 0002's two steps, admission
  then head and refs, and its failure modes are that document's.
- Several servers over one store. A deployment runs one server per
  store; lease state and cursors are that server's.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are as in RFC 2119. Terms
from RFC 0001 and RFC 0002 keep their meanings.

- **Server**: an HTTP service in front of one RFC 0002 store.
- **Client**: anything that calls it: a reader, a follower, a writer,
  or another store exchanging with it.
- **Lease**: the network form of RFC 0002's hold on a session: granted
  to a holder for a time, renewed, released, and identified by a lease
  ID and a fencing token.
- **Lease ID**: a secret the server issues with a lease, unguessable,
  that authorizes renewing and releasing it.
- **Fencing token**: a number the server issues with each lease of a
  session ID, greater than every token it issued for that ID before.
  It orders leases; it is not a secret.
- **Cursor**: the reference library's following position in a session's
  log, opaque to the client and meaningful only to the server that
  issued it.
- **Incarnation**: the identity of one session as created, the hash of
  its header without `format` (RFC 0002, Refs). Every response about a
  session names it, so a client can tell a session from another created
  again under its ID.

## Conventions

- All paths are under a version prefix, `/v1`. A server MUST answer
  `GET /v1/` with its capabilities: the RFC 0001 format minors it
  reads, and for each optional feature whether this server's store
  supports it: `objects`, `shallow`, `exchange`, `marks`, `commit`,
  `committed_view` (which `durable=true` needs), `refs`, `ref_follow`,
  `leases`, plus its limits. A request for a feature the store lacks is
  501 `unsupported`. A server over a store that keeps no commit or
  mark (the reference memory, JSONL and SQLite stores) offers a subset.
- Request and response bodies are JSON, except object bodies, which are
  their canonical bytes (`application/octet-stream`), and streams.
- Streams are NDJSON (`application/x-ndjson`) or server-sent events
  (`text/event-stream`), and every stream that is not a follow ends with
  an explicit terminal frame: `{"end":true}` as the last NDJSON line, an
  `end` event in SSE. A failure after the status line is a frame too
  (`{"error":{…}}` or an `error` event carrying the problem body). A
  stream that stops without either was cut, and a client MUST treat it
  as an error and never as a short result. This is RFC 0002's rule that
  a listing which cannot be completed reports the error (Refs), kept
  over a transport that cannot change its status.
- An entry crosses the wire as its RFC 0001 line: the same bytes a
  projection holds, hashed the same way. A server MUST verify every
  entry it receives by recomputing its hash, as an append does.
- Errors are `application/problem+json` (RFC 9457) with a stable `code`
  from the table under Errors. A client branches on `code`, never on
  the HTTP status alone.
- Every response about one session carries its incarnation (an
  `Incarnation` header, and a member in JSON bodies). A request MAY send
  `If-Incarnation`, and a server MUST refuse with `session_replaced`
  when it differs.
- Session IDs and object hashes are path segments. A ref name is not:
  refs are addressed by a trailing wildcard, `/v1/refs/*name`, with the
  name's own `/` unescaped (names are `[A-Za-z0-9._-]` segments, so
  nothing needs encoding), and the sub-resources `log` and `follow` by
  query, `GET /v1/refs/*name?view=log|follow`. A name that breaks the
  rules is `ref_name` on a read as on a write.
- **Retries.** A client that does not know whether a write landed
  retries it as the operation says below. Writes that are idempotent by
  hash say so; the others take an `Idempotency-Key` header, and the
  server MUST return the first outcome for a repeated key for at least
  the lease's longest TTL.

## Objects

```
GET  /v1/objects/{space}/{hash}
HEAD /v1/objects/{space}/{hash}
PUT  /v1/objects/{space}/{hash}
POST /v1/objects/missing
```

`space` is `entries` (envelopes), `contents` or `media`. The hash of an
`entries` or `contents` object is `sha256:` over its canonical bytes. A
`media` object is a sidecar blob: its hash is the SHA-256 of its raw
bytes, written as RFC 0001 names a sidecar file (`sidecar:sha256:` and
the hex digest in an item's URL). An inline data URL needs no object: it
is part of its entry's content. The server stores a session's media in
the form the session was written in and MUST NOT convert (RFC 0002,
Media).

- A `GET` returns the object's canonical bytes. An object never changes
  under its hash, so a server SHOULD mark the response
  `Cache-Control: private, max-age=31536000, immutable`, and a client
  SHOULD cache it for good. It is `private`, never `public`: the
  response is authorized for this caller, and a shared cache must not
  serve it to another. A missing object, or one the caller may not read
  (below), is 404 `no_object`.
- **Reach.** An object is *reachable by a caller* when some session the
  caller may read holds it: as an own or prefix entry's envelope, as the
  content such an envelope names, or as a media blob such a content
  names through a `sidecar:` URL. A server MUST serve, and MUST report
  as held, only objects reachable by the caller. A hash is a name, not a
  capability. Where the server's store shares one lookup table between
  contents and media (RFC 0002, Entries), the check is still per space.
- A `PUT` (exchange scope) uploads an object. The server MUST verify
  that the bytes hash to the name and refuse them with `bad_object`
  otherwise. Uploading an object it holds is a no-op that succeeds, and
  the response does not say whether it was held. An uploaded object is
  reachable from nothing, so by no one but its uploader's push, until a
  push names it; it is collected as RFC 0002's sweep collects any
  unreferenced object (Deletion and retention), and the grace period
  that spares it is the server's, published in the capabilities. An
  object swept before its push is `missing_object` at the push.
  Entry-level rules (reserved members, `ts` form, I-JSON, parent) are
  not checked at `PUT`, which sees one object; admission checks them.
- `POST /v1/objects/missing` takes `{"entries": [...], "contents":
  [...], "media": [...]}` and returns, in the same shape, the hashes the
  server lacks **for this caller**: every hash not reachable by the
  caller is reported missing, including one another tenant's session
  holds. It is the negotiation RFC 0002 allows a sender ("MAY negotiate
  what the receiver lacks"), and it can neither disclose that a hash
  exists nor let a push claim an object by naming it. A sender that
  needs an object admitted therefore uploads it, and the server accepts
  the bytes whether or not they are already stored.

## Sessions

```
GET    /v1/sessions?…filter…
POST   /v1/sessions[?lease_ttl_ms=n]
GET    /v1/sessions/{id}
DELETE /v1/sessions/{id}
```

- **List** streams the reference library's summaries as NDJSON, newest
  first, ending with `end`. The filter's members are query parameters:
  `cwd`, `parent_session`, `harness`, `top_level`, `after` and `before`
  (RFC 3339), `limit`, `with_names`, `current`; and `extra`, which is
  repeated as `extra=<name>:<canonical JSON value>` and matches as the
  library does, by canonical form. A server MAY page with a `page`
  token returned in the `end` frame. A summary carries the header, the
  name, `superseded_by`, the size and the modified time, and never a
  file path.
- **Create** takes a header (RFC 0001) and returns the session, its
  incarnation and the header the server filled. A header naming a
  `base` makes a fork under RFC 0002's creation rules (Sessions),
  including their refusals, and the server commits the log holding the
  base before it creates the fork. The base MUST be an entry of a
  session the caller may read; one held elsewhere is `no_entry`, as if
  it did not exist. A create is retry-safe only with a **complete
  header**, one whose `id` and `created_at` the client chose: creating
  an ID the server holds under another header is 409 `session_exists`,
  and under the same header (compared without `format`) returns the
  existing session. A header the server must fill is not retry-safe
  without an `Idempotency-Key`. With `lease_ttl_ms` the new session
  comes back leased, with the lease in the response: the creator holds
  the session from its first instant.
- **Get** returns the header, the head, the record mark, the log's
  length, the incarnation and the log's current cursor, from one
  committed view when the store has one (see Reading). It is not a read
  of the entries.
- **Delete** needs a lease, below, and is RFC 0002's delete: the ref,
  header and log go in one step, and refs outlive it as RFC 0002's Refs
  say. A second delete is `no_session`.

## Reading

```
GET /v1/sessions/{id}/log?after={cursor}&durable={bool}
GET /v1/sessions/{id}/path?leaf={entry}&contents={all|since-fold|none}
```

- **The log** is the session's own records in log order after a cursor
  (from the start without one), as NDJSON ending with `end`. Each line
  is `{"seq", "entry": hash, "head": hash|null, "outcome", "durable",
  "cursor"}`: the entry the record appended, the head after it, and the
  cursor after it. A head move that appends nothing (a compare-and-swap)
  is `{"seq", "head": hash, "durable", "cursor"}` with no `entry`. The
  first line, or the `end` frame when the log is empty, also carries the
  session's header, incarnation and mark. A client fetches the envelopes
  and contents it lacks through Objects, or reads `path`, and builds the
  session as the reference library's reader does.
- **`durable=true`** serves only committed records, and it is the reading
  RFC 0002's exchange requires of a sender ("a sender MUST commit the
  session before it pushes or serves it").
  - A server that holds the session's writer (the usual case) commits
    its working state first, then serves it. A `durable` read can
    therefore write; it needs read scope only because it commits what
    the leased writer already accepted. It does not wait on another
    session's commit.
  - A server whose store cannot commit another process's working state
    serves only what the log shows committed, which may include an
    append whose commit is still in flight and which the writer takes
    back if the commit fails (RFC 0002, Exchange). Such a record can
    still be undone by a `reset`.
  - The head and mark in a `durable` response are those of the committed
    view the records come from, never the live head. A fetcher adopts
    that head, not one read by another call.
  - Only a store with a committed view offers this; the capability
    `committed_view` says so, and otherwise `durable=true` is 501
    `unsupported`.
- **The path** is the entries from the root to `leaf` (the head by
  default), with the root first, as NDJSON ending with `end`. Each line
  is `{"entry": hash, "envelope": {…}, "content": {…}|null}`: the
  envelope inline, so the chain verifies from the response alone, and
  the content according to `contents`. The first line also carries the
  head and cursor the path was read at, and the incarnation, so a client
  can follow from it with no gap. A `leaf` that is no entry of a session
  the caller may read is `no_entry`. Across a fork's base the path
  crosses into the origin's entries, whose session the caller must also
  be able to read, or the base is `no_entry`.
  - `all`: every content.
  - `since-fold`: the contents RFC 0001's context building needs to
    rebuild the request at `leaf`, and the contents of every entry that
    is not an `item`:
    - Let the newest `compaction` on the path to `leaf` be the one
      nearest `leaf`. The path is the single chain of `parent` links
      from `leaf` to a root, across the base into the prefix, so the
      newest compaction is well defined for a given `leaf`, and
      differs between leaves on different branches.
    - Contents come for the compaction itself (its checkpoint, summary
      and pinned items), for every entry from the entry its `first_kept`
      names onward to `leaf`, and for every entry above `first_kept`
      whose type is not `item` or `branch_summary`: `config`, `info`,
      `link`, `run`, `dispatch`, `decision`, `queued`, `env`, `label`,
      `outcome` and extension entries. Those bodies are small and the
      reference library reads them (a session's name, its successor, a
      run's state). Only the contents of `item` and `branch_summary`
      entries above `first_kept` are left out; they are the history the
      fold replaced.
    - Every envelope still comes, so the chain verifies; omitted
      contents verify by hash when fetched.
    - A compaction whose content carries no `config` checkpoint (RFC 0001,
      `compaction`) does not start the settings afresh, so a reader
      would need the `config` entries before it. The server then answers
      as `all`, and says so with `"contents": "all"` in the first line.
    - Instructions parts named by `hash`, `keep` runs and omitted
      lists named by `of` all resolve against the path since the last
      `replace` or compaction checkpoint, and RFC 0001's `config` rule
      is that a checkpoint starts both afresh: no entry after it names
      a part or list before it. Nothing above the fold is needed to
      resolve them.
    - The server enforces `call_id` uniqueness on append (RFC 0001 makes
      it unique in a session), so a shallow client appending a function
      call needs no call IDs from entries above the fold.
    - This is RFC 0002's open question on shallow boundaries, answered
      for reading and not for storage.
  - `none`: envelopes only, for verification or a tree view.

  A client MUST NOT treat a shallow path as the session: a projection
  of it is not an RFC 0001 file, since the file holds every entry's
  body. The client library returns a shallow session that reports which
  contents it lacks, and refuses `Verify` or a request rebuild that
  needs one. It supports the context rebuild and the entries it holds;
  it does not offer the library's `Follower` snapshot, which is the
  whole session. A harness that must recover state from item contents
  above the fold reads `all`.

## Following

```
GET /v1/sessions/{id}/follow?after={cursor}&durable={bool}
GET /v1/refs/*name?view=follow          (optional: ref_follow)
```

Following is RFC 0002's following a log, carried as server-sent events.
The server follows the log in process and fans live changes out, so N
clients at the tail cost the store one follower, not N. A client that
resumes from a cursor costs a fresh load, which is the whole session in
the reference stores, so a server MAY rate-limit resumes.

- The events are `snapshot`, `appended`, `head`, `reset`, `committed`
  and `error`. The first four are the reference library's `Change`
  kinds; `committed` and `error` belong to the wire.
  - A `snapshot` or `reset` carries the session's records from the
    start in the shape of the log lines above, with the header,
    incarnation, mark and head, and the entries inlined (below) up to
    the size a server publishes, past which the client reads `path`.
  - An `appended` carries the record: the entry line and its content
    inline, so a follower needs no per-entry object fetch, plus `seq`,
    `outcome` (continued, branched, held, leaf_moved or leaf_not_moved),
    **the head after it**, and `durable`. A `held` append changes
    nothing and is not sent.
  - A `head` carries the leaf and `durable`, for a head move that no
    append explains: a compare-and-swap or a head record. An append
    that moved the head says so in its own `appended`, and no second
    event repeats it.
  - A `committed` carries the cursor through which the log is now
    committed. Every earlier record sent with `durable: false` at or
    before that cursor is durable from then, and the server MUST send
    one when a commit covers records it sent as not durable.
  - `durable` on each event is new in the protocol; the reference
    library adds it to its `Change` so a library follower reports what
    the wire says.
- Every event has its own SSE `id`: the cursor after it, then `.`, then
  its index among the events of that cursor (`<cursor>.<n>`). A client
  resumes with `Last-Event-ID` (or `after`, with `Last-Event-ID`
  winning when both are sent), and the server resumes after exactly that
  event. A cursor the server can no longer resume from, because the log
  was replaced, or one that is no cursor of this server's, gets a
  `reset`, never a silent skip.
- A `reset` is also what answers a resume cursor from a session that was
  deleted and created again under the same ID: the client gets the new
  session read afresh, as another session, and drops what it derived
  from the old, which is the library's behaviour. A **live** follow that
  sees its session deleted ends with an `error` event, `no_session`; the
  stream does not continue into a session created after.
- A record seen as not durable that a crash or a failed commit takes
  back is answered with a `reset`, as RFC 0002 says of a local
  follower. With `durable=true` the server sends only committed
  records, and a `reset` can still follow one a commit in flight had
  covered. The stream is gated like the `durable` log read.
- A server SHOULD send a comment line every 15 seconds on an idle
  stream, so intermediaries do not close it.
- **Following a ref** is an optional feature (`ref_follow`), not in the
  reference library. It sends a `target` event with the current
  `{name, target, state}`, `state` being `live` or `dangling` (RFC
  0002, Refs: the session is gone or another now holds its ID), first
  on connect and again whenever the target or the state changes. A live
  to dangling flip, from a session's deletion or re-creation, is a
  `target` event although the ref did not move. A ref update to none is
  a `target` event with a null target. A ref has no cursor; a
  reconnecting client gets the current state again. A client following a
  conversation follows its ref, and switches to following the session
  it points to.

## Leases

```
POST   /v1/sessions/{id}/lease
PUT    /v1/sessions/{id}/lease        Lease-ID: …
DELETE /v1/sessions/{id}/lease        Lease-ID: …
POST   /v1/sessions/{id}/lease/break  (admin)
```

A lease is RFC 0002's hold, over a network. A process's hold ends when
the process does; a client's cannot be seen to end, so it expires.

Over the wire every write to a session needs a lease. RFC 0002
(Append) lets a store take concurrent writers and branch the loser, and
also allows a store to hold a session for one writer at a time. The
protocol takes the second. A caller that wants concurrent branching
writes to a local store and pushes: the merge of RFC 0002's exchange is
where branches meet. The reason is that a network writer cannot be told
apart from a dead one, and an append the server accepts from a writer
that lost its place cannot be taken back.

- **Acquire** takes `{"holder": "...", "ttl_ms": n}` and returns
  `{"lease_id": s, "token": t, "expires": time}`, or 409
  `session_locked`. The `holder` is a label, not an identity; the 409
  carries it to callers with write scope. The server MAY cap `ttl_ms`.
  A session held as a mirror cannot be leased: `mirror`.
- **Renew** and **Release** take the `Lease-ID` and need no token. The
  lease ID is unguessable (at least 128 bits) and is the only thing
  that authorizes them; a numeric token alone never does. Renewing an
  expired or superseded lease is 409 `lease_lost`. Release ends the
  lease at once.
- **Expiry** is measured on the server's monotonic clock from its receipt
  of the acquire or last renew. `expires` is advisory: a client
  computes its own deadline from its clock at the time it sent the
  request, less a margin, and stops writing before it. An expired lease
  is not current, whether or not anyone has taken it since.
- **Fencing.** Every write to a session (append, commit, head move,
  delete, mark change) MUST carry the lease's numeric token in a
  `Lease-Token` header, and the server MUST refuse with `lease_lost` a
  write whose token is not the session's current, unexpired lease.
  - The check and the write happen under the same per-session lock as
    acquire, renew and expiry, so a write that passed the check cannot
    land after a newer lease was granted.
  - Tokens are persisted with the session's store state and strictly
    increase per session ID across server restarts **and** across the
    session's deletion and re-creation, so a token never repeats under
    an ID. The counter is not removed with the session.
  - A lock-breaking admin operation (`lease/break`, or the store's own
    break of a stale lock) advances the token, so the breaker's
    successor has a token greater than any the broken holder had.
  - Because tokens only increase, a writer whose lease expired and was
    taken, whose request is delayed in the network, cannot write after
    the new holder has. Expiry alone could not promise that; the token
    does. It fences the server's writes, not a stalled harness's tool
    calls, which a harness that holds a lease past its deadline can
    still make. RFC 0001's writing discipline puts a record entry before
    a side effect, and the fence refuses that record, which is the
    signal to stop.
- **Takeover.** Acquiring after a lease expired first commits the
  previous holder's working state, as a release does, so the new holder
  resumes from a durable log and not from appends a crash could take
  back. An append in flight when its lease expires either completes
  before the acquire returns or is refused with `lease_lost`; it is
  never split.
- **One mechanism.** Leases and the store's local holds are one
  mechanism: a session held locally cannot be leased, and a leased
  session cannot be held locally, including by another writer in the
  server's own process, which the reference stores' per-store hold
  would not exclude on its own. A server keeps the lease table in the
  store's state, so a local writer and the server see one table. A
  deployment that lets an operator break a lock does so through
  `lease/break`.
- **What a push is.** A push is not leased. It is governed by RFC 0002's
  record mark: a mirror refuses local writes and accepts only exchange,
  only the record may initiate a push, and the head compare-and-swap
  reveals two records. It takes the
  session's per-session lock for its admission and head step, so it is
  ordered against a lease holder's writes, but it presents no token. A
  ref step in a push is not leased either; it needs refs scope.
- What a writer does on `lease_lost` is a harness's choice. A resumable
  harness acquires again, reads the log from its last cursor to learn
  whether its last write landed, resumes from the record, and continues,
  which is how a container replaced by its orchestrator picks up.
- **Continue** (the reference library's `Continue`) creates a successor
  and appends a `continued_in` link to the old session, so it takes a
  lease on each: it creates the successor with `lease_ttl_ms`, writes
  the successor's entries under that lease, acquires the old session's
  lease for the link, and moves the named ref (when it has one) by
  compare-and-swap, in that order. A crash between steps leaves what RFC
  0002 describes (Refs, Refs and continuation): the ref on the retired
  session, which a caller finds by following `continued_in`.

## Writing

```
POST /v1/sessions/{id}/entries
POST /v1/sessions/{id}/commit
PUT  /v1/sessions/{id}/head
POST /v1/sessions/{id}/record
```

All four need a lease, except where the record section below says
otherwise. A write names the format minor of the client that built it in
a `Session-Format` request header, which is RFC 0002's rule that an
append carries its writer's `format` (Append). The server refuses an
earlier minor than the session's, or one it does not read, with
`format`, and raises the header's `format` durably before it accepts a
later one, as RFC 0002's Sessions say.

- **Append** takes one entry line, or several as newline-delimited
  lines. Each is applied in order, and the batch **stops at the first
  refusal**. The response lists, for each applied entry, `{"id",
  "outcome", "durable", "unresolved": […]}` and, if the batch stopped,
  the index of the refused line and its problem body. The applied entries
  stay in the log, visible to followers; a client resumes from the
  index.
  - `outcome` is the library's: `continued`, `branched`, `held`,
    `leaf_moved` or `leaf_not_moved`. These name what an append did;
    they are not error codes. (`head_moved` is the compare-and-swap's
    refusal and is a different thing.)
  - `unresolved` lists references the server could not resolve: a
    sidecar blob it lacks, or a convergence into a session it does not
    hold. The entry is accepted, as the library accepts one, and the
    server reports what is missing. A session with an unresolved blob
    cannot be pushed or projected until it arrives.
  - Refusals are `bad_entry` family codes (below), `no_entry` for a
    parent the session does not hold, `format`, and `mirror` on a
    session held as a mirror.
  - **Retries.** An entry line the client builds completely, `id`,
    `parent` and `ts` set, is the same entry on a retry: the second
    append is `held`, and a `held` response with `?commit=true` still
    commits before it answers, so a retry of a durable append is
    durable. After a timeout a client reads the log from the cursor it
    last had, since a `held` on a retry says the entry is there and says
    nothing of what its first outcome was. A line the client leaves
    incomplete is accepted and the server fills it, but the fill makes
    each retry a different entry, so such an append is retry-safe only
    with an `Idempotency-Key`.
- **Commit** makes the session's working state durable and returns the
  cursor through which everything is committed. It commits one session
  and does not wait on another's. An append MAY ask for a commit with
  `?commit=true`, and the response says `durable` either way, as RFC
  0002 requires of a store that acknowledges before it commits. A commit
  that fails after the append was accepted cuts the entries back out of
  the log (RFC 0002, Following a log), and the response is an error:
  the entries did not land, and followers get a `reset`.
- **Head** is the compare-and-swap: `{"expected": e|null, "to": t,
  "durable": bool}`, `null` meaning no head. It is `head_moved` with the
  current head on a mismatch, `no_entry` for a target that is not the
  base or an own entry, and `head_target` for a `leaf` label. The
  response says whether the move was committed, and `durable: true`
  commits it before it answers, as RFC 0002 asks (Durability and
  recovery).
- **Record** is RFC 0002's declare-record: `POST /v1/sessions/{id}/record`
  makes this server the record for a session it holds as a mirror. RFC
  0002 permits it when the record has deleted the session without
  handing it over (Exchange, the record mark). The guard: the request
  carries `{"basis": "record_deleted"|"sole_copy", "head": h}`, the head
  the caller believes the server holds, and the server refuses with
  `head_moved` if its head is another, which is the compare-and-swap
  that reveals two records. The mark is committed before the response.
  A mark that stays a mirror elsewhere is the caller's to clear; the
  protocol cannot see the other store.

A client that writes to a remote store pays a round trip per append. A
harness that cannot afford that in its loop writes to a local store and
pushes, below.

## Refs

```
GET    /v1/refs?prefix={p}
GET    /v1/refs/*name
PUT    /v1/refs/*name
GET    /v1/refs/*name?view=log
GET    /v1/refs/*name?view=follow
```

RFC 0002's refs, unchanged.

- **Resolve** returns the target as `{"session", "entry"}` (`entry`
  empty for the zero-entry form) with the incarnation the ref was set
  against. A ref that names no session the store holds is 404 `no_ref`
  when the ref is absent, and a dangling ref, whose session is gone or
  another now holds its ID, is 409 `ref_dangling` with the target in the
  body, so a client can see which session it was.
- **Update** takes `{"expected": target|null, "next": target|null,
  "reason": "..."}`: compare-and-swap, `null` being "no ref". A
  mismatch is 409 `ref_moved` with the current target. A `next` whose
  session the server does not hold is `no_session`, and one whose entry
  it does not hold is `no_entry`. A same-target update on a dangling ref
  is refused with `ref_dangling` (RFC 0002, Refs: a ref is re-adopted by
  deleting it and setting it again). Setting a live ref to the target it
  holds changes nothing and is not logged. A ref update needs refs
  scope, not a lease: a ref is the store's state, not a session's. A
  store open read-only refuses it as `read_only`.
- **List** and **log** stream as NDJSON with `end`, in name order and
  newest first respectively, and a listing that cannot be completed
  ends in an `error` frame.
- **`SessionFor`** of the reference library maps onto these calls:
  create the session with a lease (`lease_ttl_ms`), claim the ref with
  expected `null`; the winner keeps its lease or releases it, and the
  loser opens the winner's session and deletes its own under its own
  lease, which nothing else has seen. The creator holds the session from
  creation, so there is no window for another caller to lease it before
  the loser deletes it.

## Which store is the record

RFC 0002 enforces one store of record per session between stores that
exchange. The two writing stories of the Motivation are the two
arrangements, and a session lives in one at a time.

- **Container story: the server is the record.** Containers resolve a
  ref, acquire a lease, and write through the server. Nothing is
  local. The server's mark is `record` (a session created on the server
  is the record there, RFC 0002, Exchange).
- **Local-first story: the client is the record.** The client's local
  store holds the session as the record, pushes to the server, and the
  server holds it as a **mirror**, which refuses an append or a head move
  for it (`mirror`) and accepts it only through exchange. Followers read
  the server.

A session moves between the two by handover, in either direction.
- **Client to server.** The client's store pushes with `mark: "record"`
  (below): the server sets its mark and acknowledges it committed, and
  only then does the client clear its own, as RFC 0002 orders it.
- **Server to client.** A client store takes the session back by
  fetching it and then asking the server to clear its mark: `POST
  /v1/sessions/{id}/handover` with `{"to": "client", "head": h}`, which
  the server accepts only if its head is `h` and it holds the mark as
  record, commits as a mirror, and acknowledges. The client then declares
  the record in its own store (RFC 0002's declare). A failure between the
  two leaves two records, never none, and the head compare-and-swap
  reveals them at the next push.
- **A local-first writer that dies** leaves the server with a mirror of
  the session as of its last push, which nothing can advance. A
  replacement in the container story declares the record at the server
  (`POST …/record`, with `basis: "sole_copy"` and the head it fetched) and
  then writes under a lease. What the dead writer had not pushed is
  lost, as the local-first story already says.

## Exchange

```
POST /v1/sessions/{id}/push
```

A push is RFC 0002's, over the wire.

1. The sender asks which objects of the closure the receiver lacks
   (`POST /v1/objects/missing`), and uploads every one so reported
   (`PUT`). Because `missing` answers per caller, an object that no
   session the sender may read holds is uploaded even when another
   tenant's session holds it.
2. It posts `{"header": …, "entries": [own entry hashes in log order],
   "prefix": [prefix entry hashes, root first], "head": {"expected": e,
   "to": t, "force": bool}, "mark": "mirror"|"record", "refs": [{"name",
   "expected", "next"}]}`. `mark` is the **receiver's resulting mark**:
   `mirror` (the usual push) or `record` (a handover to the receiver).
   The sender is the record by definition, or the server refuses with
   `not_record`; the request cannot say otherwise. Each ref's `next`
   MUST name the pushed session (and may pin an entry of it the server
   holds); any other is `bad_ref_target`.
3. The server admits the closure as RFC 0002 says: hash verified,
   parent first, the log merged as a set, the `call_id` collision
   refused, a header that differs from a held one refused. Every object
   the push names that the server cannot find is `missing_object` with
   the hashes, and nothing is admitted. Only then does it take the head
   step and the ref steps.
4. The response is 200 with `{"admitted": n, "created": bool, "head":
   {"moved": bool, "forced": bool, "current": h, "why": s}, "mark":
   "mirror"|"record", "refs": [{"name", "moved", "current", "why"}]}`.
   **A 2xx means the admitted entries are committed**, and for a handover
   that the receiver's mark is committed too; the sender clears its own
   mark only on that. `forced` is true when `force` moved a head that was
   not the expected one, so a force is never taken for a fast-forward.
   A head that did not move, and any ref that was refused, is in the 200:
   a refused head or ref undoes nothing admitted, and a ref that is
   refused does not stop the others.

Failures after the closure was admitted are not a 200 with silence:
- A handover whose head would not move is refused before anything lands,
  with `head_moved`. A handover whose head step fails after admission
  lands the entries, changes no mark, and answers 409 `head_moved` with
  the same body as above, the entries and `head.why` reported, and
  `refs` left `null`: refs are not attempted when a handover fails.
- A ref step that is already at its `next` succeeds, with `moved: true`:
  a retried push is safe. The admission of a retried push is a no-op, and
  the head step is a no-op when the head already equals `to`.
- A push of a session the receiver holds under another header is
  `header_differs`; a `call_id` the receiver holds and the sender does
  not is `call_id_repeated`; a format it does not read is `format`.

A push needs exchange scope, and a `refs` member needs refs scope as well.
A push is not leased (see Leases).

A **fetch** needs nothing new. The fetcher reads the log with
`durable=true`, which carries the session's header, incarnation, mark
and head from one committed view, and reads the **closure**: the
session's own entries from the log, and its prefix from `path` (the
entries from the root to the base, root first, which `header.base`
marks), with their contents, and the media the contents name. It
admits them in its own store, and moves its head and refs under RFC
0002's fetch rules (Exchange), which hold the head for a record. The
fetch creates a mirror. The server does not know a fetch happened,
which is how a mirror can serve one.

**Local-first writers** combine the two. A harness records into its own
store with no network in the loop, and pushes at commits, on a timer,
and on shutdown (a container's grace period), each push carrying the
conversation's ref. What a crash can lose is what the harness had not
yet pushed, which it chooses by how often it pushes. Followers read the
server.

## Errors

The codes below are this document's. RFC 0002 states refusals in prose
and names none; each row is one the reference library already makes.
Where the library returns a plain error, the code is the one it ought to
carry. A request refused for authentication or scope is 401 `auth` or
403 `scope`.

| Code | Status | Meaning |
|---|---|---|
| `no_session` | 404 | no such session (also: a live follow whose session was deleted; a ref `next` naming a missing session) |
| `session_exists` | 409 | Create of an ID held under another header |
| `session_replaced` | 409 | the incarnation differs from `If-Incarnation` |
| `no_entry` | 404 | a parent, base, head target, ref pin or `leaf` the session (or the caller) does not hold |
| `no_object` | 404 | an object hash not held or not reachable by the caller |
| `no_ref` | 404 | no ref of that name |
| `ref_dangling` | 409 | a ref whose session is gone or another; also a same-target update of one |
| `ref_moved` | 409 | the ref is not at `expected`; body has the current target |
| `ref_name` | 400 | a name or prefix that breaks RFC 0002's name rules, or conflicts with a held ref |
| `head_moved` | 409 | the head is not `expected` (also a handover whose head did not move) |
| `head_target` | 422 | a head target that is a `leaf` label, or not the base or an own entry |
| `session_locked` | 409 | the session is leased or held locally |
| `lease_lost` | 409 | the token is not the current, unexpired lease |
| `mirror` | 409 | a local append, head move or mark change on a mirror |
| `not_record` | 409 | a push from a store that is not the record |
| `header_differs` | 409 | the receiver holds another session under this ID |
| `call_id_repeated` | 409 | an append or exchange that would repeat a `call_id` |
| `synthetic` | 422 | a `leaf` label carrying `synthetic` |
| `bad_entry` | 422 | malformed: `id` differs, reserved member, `ts` form, not I-JSON, unknown prefix |
| `base_rule` | 422 | a parent that is not the base, an own entry, or null; a null parent in a session with a base |
| `bad_base` | 422 | a fork whose base is a `leaf` label, or whose `media` differs from the origin's |
| `bad_object` | 422 | bytes that do not hash to the name |
| `missing_object` | 409 | a push names objects the server cannot find; body has the hashes |
| `bad_ref_target` | 422 | a pushed ref whose `next` is not the pushed session |
| `format` | 422 | a format the server does not read, or earlier than the session's |
| `read_only` | 403 | a write to a store open read-only |
| `unsupported` | 501 | a feature the store lacks (marks, exchange, commit, committed view, refs, objects) |
| `store_fault` | 500/503 | a store failure the client cannot fix: corrupt object, stopped store, layout, a session modified outside the store |

`no_session` on a ref update means `next` names a session the server
does not hold. On a dangling ref the library's refusal of a same-target
update is a `no_session` error; here it is `ref_dangling`, so a client
need not guess which of the two refusals it got. The `bad_entry`
refusals carry a `reason` member naming which rule, and `base_rule` is
the session's parent rule (RFC 0002, Sessions), which a caller handles
differently from a malformed line.

## Out of scope and admin operations

Not carried, because they are a local operator's or a file's:
- **Import** and **Project** (RFC 0002, Projection to JSONL): moving a
  session as a file is not moving it as a closure. A client imports into
  its own store and pushes.
- **Repair**, **Verify**, **Pack** and **Sweep**: store maintenance. A
  deployment MAY expose them under its own admin scope; the protocol
  does not define them. A remote reader that needs to check a session
  does it by hash over `path`.
- **Declare record** is carried (`POST …/record`). **Lock break** is
  carried as `lease/break` under admin scope.
- **Prefix caching.** The context hash (RFC 0002, Prefix caching) is
  computed at append and stored with each entry. The `path` and log
  lines carry it as `context` for each entry, so a router that keys on
  it need not recompute; a server that does not keep it omits the member.

## Security considerations

- Everything a session holds, every prompt, tool output and model
  response, is readable by anyone with read scope. A deployment MUST
  authenticate and authorize every request, over TLS, and SHOULD
  separate read, write (lease), refs, exchange and admin scopes.
- Objects are named by hash. Knowing a hash is not authorization to
  read it, and the protocol makes sure naming one is not either: the
  server reports an object held only to a caller who may read a session
  that holds it (`missing`, `GET`, `HEAD`), a push must upload every
  object not so reachable, a fork's `base` and a `path`'s `leaf` are
  authorized against the sessions holding them, and an object response
  is `private`. A dedup that answered "already stored" to anyone would
  let one tenant read, or confirm a guess at, another's content.
- A push's `refs` need refs scope in addition to exchange scope, so that
  moving a ref cannot be done under a weaker grant than a ref update.
- A server MUST verify every entry and object it receives. Nothing a
  client sends is taken on trust except what RFC 0002 already takes on
  trust from a pushing store: the head and the record mark, until heads
  are signed.
- A lease ID is a secret, and the holder label is not an identity. Lease
  operations are authorized by the lease ID, never by the numeric token.
- An approval in a session (a decision `by` a human) records a string,
  not an authenticated identity. A server that admits writes from many
  principals SHOULD record the authenticated principal of each append
  beside it, in store state rather than in the entry, since the entry's
  hash is the harness's.
- A server SHOULD bound the number and age of open streams, and the
  work a resume may cost.

## Reference implementation

`agentsession/remote`, a nested module:
- **A server** that wraps a store the reference library offers, with
  following fanned out from one in-process follower per session, and
  leases kept in the store's own holds. Of the reference stores only
  the `cas` store has marks, exchange, a per-session commit, a committed
  view and the object spaces; a server over memory, JSONL or SQLite
  publishes the subset it has (see Conventions).
- **A client** that implements the library's `Store`, `Reader`,
  `Follower` and `RefStore` over these calls, with an object cache keyed
  by hash. That is not the claim that code written against a local store
  runs unchanged. `Append` returns the entry's ID, and the outcome,
  durable and unresolved fields come from the response through a result
  type the library adds (`Write`); `Open` returns a client-side session
  that the client keeps up to date and that is not shared with the store,
  so `Session.Branch` does not reach the server (a head move is the
  compare-and-swap); `Open` acquires a lease; and `Follow` yields
  `Change`s with the durable field the library adds. A caller that needs
  the wire's extra information uses those additions.
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
- **Several servers.** Leases, cursors and the committed view are one
  server's. Sharing them across replicas needs a store that keeps lease
  state and a cursor that survives a node.
