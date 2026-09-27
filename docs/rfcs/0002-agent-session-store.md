# RFC 0002: Agent Session Store

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at draft 0.5 or later.

## Summary

A store holds sessions as content-addressed entries and mutable refs. A
**entry** is one entry of RFC 0001, stored once under the hash of its
canonical bytes, immutable, and naming its parent by hash, so that a
leaf hash commits to the whole path above it. A **session** is a ref:
a header, a **base** entry it continues from or none, a **head** entry
its next append will name as parent, and a **log** of the entries it
appended, in the order the store accepted them.

The JSONL file of RFC 0001 is a projection of a session: its header,
the path to its base, then its own entries in log order. A reader
verifies a projection entry by entry by recomputing hashes, and verifies
a fork against its origin by comparing one hash. Nothing that can be
checked is taken on trust.

Two writers appending to one session may both succeed. The store
assigns each entry its place in the log, and the head moves only when an
append continues it; an append that does not is a branch, recorded and
left where it was. The only write an appender can lose is the head,
and it moves by compare-and-swap. A store may still take writers in
turn; the model is what lets it not.

## Motivation

RFC 0001 made the file the unit of everything: the storage unit, the
boundary within which entry IDs are unique, and the unit a header's
promises are about. That was right for a single harness writing a
single thread. It strains as soon as sessions share history.

The fork rule attempted in the format showed where. A session forked
from a point in another had to open with a copy of that point's path,
and every rule after that existed to let a reader tell copy from
original inside a file that cannot say: a rule that the copy keep the
origin's IDs, a rule that the reference carry the origin's session lest
it name the copy, a rule rewriting copied references, a rule for
members that name entries off the copied path, a rule scoping the
header's promise to the lines the writer wrote, and a rule for what a
copy may include before the promise stops being decidable. Each was
correct and each was a patch over the same missing fact.

The missing fact is identity. Git shares history for free because a
commit is named by its content, so two branches naming the same commit
share it rather than copy it. A record of agent sessions wants the same
property for the same reason, and one more of its own: a hash of the
leaf identifies the entire context that leaf was built from, which is
what a cache keyed on context needs and what a verifier of a copied
prefix needs, and both are the same operation.

The other strain is concurrency. RFC 0001 orders siblings by their
position in the file and resolves which branch is live by a marker
label, and it left open who assigns the order when two writers append
at once. A store that owns the log and the head answers both: the log
is the order, and the head is a ref, never inferred.

## Goals

- **Content-addressed.** An entry is stored once under its hash.
  Sessions sharing a prefix share its entries.
- **Verifiable.** A projection is checked hash by hash; a fork is
  checked against its origin by one hash.
- **Appends need no session lock.** The model lets concurrent writers
  to one session both succeed, with the head the only write one of them
  can lose. A store MAY still take writers in turn.
- **Nothing is garbage.** Every entry a session appended stays in its
  log. Abandoned branches are preference data, as RFC 0001 says.
- **Projection is lossless.** A session projects to an RFC 0001 file
  that reads back to the same entries and the same head.

## Non-goals

- A query language or index beyond what the projections need.
- A network protocol between stores. Replication is by exchanging
  entries and refs, and how is a store's business. A session's ref has
  one store of record; two stores accepting the same appends would
  order two logs and could move two heads, and nothing here merges
  them.
- Defining the entry's contents. RFC 0001 does; this document stores
  them.
- A spool for model output in flight; see durability and recovery.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as
in RFC 2119.

- **Entry**: as RFC 0001 defines it, addressed by its hash. The store
  holds it as an object.
- **Object**: the canonical bytes of an entry or a media blob,
  addressed by their hash. Every entry is an object; not every object
  is an entry.
- **Session**: a ref, consisting of a header, a base, a head and a log.
- **Base**: the entry a session continues from, or none. A session with
  a base is a **fork** of the session that appended that entry.
- **Head**: the entry a session's next append names as parent, unless
  the appender names another.
- **Log**: the entries a session appended, in the order the store
  accepted them. These are the session's **own** entries.
- **Prefix**: the path from a session's base to its root. A session's
  prefix entries are another session's own entries.
- **Projection**: an RFC 0001 file built from a session.

## Entries

An entry's hash is defined by RFC 0001: `sha256:` followed by the
lowercase hexadecimal SHA-256 of the entry's canonical bytes with its
`id` member removed, canonical meaning the JSON Canonicalization Scheme
(RFC 8785). The `parent` member names an entry by hash, and so do
`parents`, `target`, `first_kept`, `from` and `queued_from` wherever
RFC 0001 has them name an entry.

- A store computes an entry's hash itself and MUST refuse an append
  whose `id` is present and differs from it. A store MUST store an entry
  under its hash and MUST NOT store two objects under one hash. Storing
  an entry whose hash is already present is a no-op that succeeds.
  `sha256:` is the only prefix, and a store MUST refuse an entry whose
  `id` carries another.
- A store MUST NOT accept an entry whose `parent` it does not hold. No
  other reference is a condition of acceptance: `parents`, `target`,
  `first_kept`, `from` and `queued_from` are the writer's to place, as
  RFC 0001 says, and a store MAY report one it cannot resolve.
- An entry is immutable. There is no update and no in-place correction;
  a correction is a new entry, as RFC 0001 already requires of entries.
- Acyclicity is structural. An entry names its parent by a hash that
  exists before the entry does, so no entry can name a descendant.

Two appends of the same content under the same parent at the same `ts`
are one entry. A writer that means two entries makes them differ; `ts`
at sub-second precision is what usually does. A log holds a hash once,
so the second such append to one session is a no-op the store reports
as such.

## Sessions

A session is created with a header and optionally a base.

- The header is RFC 0001's, and its `base` member is the base entry's
  hash or absent. `parent_session` is provenance a store does not
  validate: a fork made at an entry on another fork's prefix has a base
  in the grandparent's log, and the header may name either. A `fork_of`
  link records the session a fork was actually made from.
- A store MUST refuse to create a session whose base it does not hold.
  A session created with a base has that base as its head.
- A session with no base is a fresh root. Its first append is a root
  entry, `parent` null.
- A session's own entry MUST name as `parent` the session's base or one
  of the session's own entries. Branching above the base is a new
  session with a lower base, not an append to this one, so that a
  session's base is the one point it diverged from and the prefix is
  exactly the path to it.
- A session MAY have several roots only when it has no base. A session
  with a base has one prefix and everything it appends hangs from it.

A fork is a session created with a base. Nothing is copied: the fork's
prefix is the origin's entries, stored once. The header's `base` says
what point was diverged from, and the projection carries the path to
it, so a file built from the fork is self-contained as RFC 0001's
ingress rule requires, and a reader holding both projections checks
the fork's claim by finding the base hash in the origin's log.

A subsession that inherits its parent's context is a fork whose header
also carries `spawned_by`. A subsession that starts fresh has no base.
A retry of either is another session; a retry that inherits context has
the same base as the attempt it retries, and shares that prefix with
it at no cost.

## Append

Append is the one write to a session's content, and it is atomic.

- The store MUST hold the parent, per the entry rules, and the parent
  MUST satisfy the session's parent rule above.
- The store MUST store the entry and MUST append its hash to the
  session's log at the next sequence number, unless the log already
  holds it, in which case the append is a no-op, reported as such, and
  nothing moves. Otherwise the store MUST move the head to the entry
  when the entry's parent is the head. All three happen or none does.
- If the entry's parent is not the head, the head does not move, except
  for the `leaf` label the head section describes. The append succeeded
  and created a branch. The store MUST tell the appender which
  happened.
- An entry with `parent` null continues when the session has no head and
  is otherwise a branch. A session with a base MUST refuse it, since
  every own entry hangs from the base.
- A store MAY hold a session for one writer at a time and refuse or
  wait for another, provided an append it accepted is never re-parented
  and the log and head rules hold. The rules above are what let a store
  accept writers concurrently, not a requirement that it do so.

Two writers appending to one session at once both succeed. If both
name the head as parent, the store serialises them: the first moves the
head, the second finds its parent is no longer the head, is recorded as
a branch, and is told so. Whether the second writer then re-appends
under the new head, sets the head to its own entry, or leaves the branch
is the writer's decision, not the store's. This is the whole of the
concurrency model, and it is the one git has for refs.

Durability is the next section's concern: an append is one commit, and a
store offers one that returns only when the commit is durable, which is
the one RFC 0001's writing discipline requires for an entry whose type
the header names in `records`.

## Head

The head is a session's resume point, and it is a ref.

- A store MUST offer a compare-and-swap on the head: move the head from
  an expected entry to a given entry, or fail if the head is not the
  expected entry. "No head" is a valid expected value. The target MUST
  be the base or one of the session's own entries.
- Resume reads the head. There is no inference from log order and no
  marker to find. The head is what RFC 0001's leaf label was standing
  in for; the label survives as the projection's marker and as a head
  move a writer may still append.
- Moving the head to an earlier entry is how a session branches back.
  The next append under it is a new child, and the entries that were on
  the old head's path stay in the log as an abandoned branch.
- Appending a `label` entry whose `label` is the reserved value `leaf`
  moves the head to the label's `target`, which MUST be the base or one
  of the session's own entries, rather than to the label, once the entry
  is in the log, and does so wherever the label's own parent sits. That
  is the head move a bare file can express, where the label is honoured
  wherever it is, and a store honours it so a writer built against files
  behaves the same against a store. A store MUST refuse a `label` entry
  carrying `synthetic`, which marks a projection's own marker.

## Ordering

The log is the order. Each own entry has a sequence number the store
assigned as it accepted the append, and that sequence is what RFC 0001
calls entry order: it separates siblings and it says which branch was
written last. `ts` remains informational and a reader MUST NOT order
by it. Which branch is live is the head, never the order.

This answers the question RFC 0001 left open, who assigns the sequence
when two writers append at once: the store does, always, because it
serialises appends to a log even when it accepts them concurrently. No
session is short of an order, and no reader has to guess a head from
one.

## Durability and recovery

An append is three writes — the object, the log entry, the head — and
the store promises them as one. At scale the store also promises them
fast, to many writers at once, and those two promises are met by the
same structure, which a database calls a write-ahead log.

- A store MUST have a single commit point per append, after which the
  append is acknowledged and before which nothing of it is visible. An
  object written ahead of its record is not an entry the store holds, so
  it cannot satisfy another append's parent rule.
- A store MUST offer an append that does not return until its commit is
  durable. A writer MUST use it for an entry the header's `records`
  names that precedes a side effect, as RFC 0001's writing discipline
  requires, and SHOULD for a `response` entry and for a
  `function_call_output` item, which is where that discipline asks for
  an fsync. A store MAY acknowledge other appends before they are
  durable, and MUST say which it did.
- After a crash a store MUST recover to a state in which every append
  it acknowledged as durable is present in full and no append is
  present in part: no log entry without its entry, no head moved to a
  entry the log lacks.
- An object whose bytes do not hash to its name is corrupt. A store
  MUST NOT serve it and MAY discard it. Content addressing is what makes
  a half-written object detectable and a rewrite of it harmless.

The design that meets these under contention is a store-wide journal.
Every append is one record naming the session, the entry's hash and
whether the head moved, written to a sequential log. The object is
written before its record, idempotently, since a second write of the
same bytes under the same hash changes nothing. The record is the
commit point, and durability is the journal's fsync, or on object
storage the acknowledged write of the record, which a store shares
across the appends of many writers in one call. Recovery
replays the journal tail against the objects, the logs and the heads.
Each session's log is then a projection of the journal, and the
journal's order is the total order the ordering section describes, of
which a session's log is a filter.

A store built on a database that has its own write-ahead log gets
atomicity and recovery from the database. Durability it must still ask
for: SQLite's WAL commit at `synchronous=NORMAL`, which the reference
store runs, is not fsynced, so its durable append is what sets the
pragma or forces the checkpoint. A store built on a filesystem or on
object storage builds the journal first.

What the journal does not hold is a model call in flight. RFC 0001
forbids writing partial output as an entry, so the bytes of a response
still streaming are a harness's to buffer, outside the store, and reach
it as entries only when the items are complete. That spool is not this
document's concern, and it is not this journal.

## Deletion and retention

- A store MUST NOT delete an entry that any session's log references.
- A store MUST NOT sweep an entry on any session's prefix, its base
  included: the session's projection needs it.
- Deleting a session removes its ref, its header and its log. Entries
  referenced by no remaining log and on no remaining prefix MAY then be
  swept.
- A store MUST NOT sweep by reachability from heads. A session's
  abandoned branches are in its log and are its record; RFC 0001's
  outcome and preference rules depend on them. What a store may sweep
  is an object that no log names and no prefix needs, and it may tier
  cold objects wherever it likes.

## Media

A media blob referenced by an item is an object under its own hash. A
store holds it once however many items reference it. RFC 0001's
`media` header field fixes at creation how the session carries media:
inline as a data URL inside the item, or beside the file under a
sidecar directory named after the session, where the file name is the
blob's hash. An item's bytes are hashed, so a store MUST NOT convert
between the two; a projection carries media in the form the session
was written in, and an inline data URL is part of its entry and does
not dedupe.

## Projection to JSONL

`ToJSONL(session)` builds an RFC 0001 file:

1. The header, with `base` set when the session has one.
2. The prefix, root first: the path from the root to the base, every
   entry on it, each as its canonical line with `id` set to its hash.
3. The session's own entries in log order, the same way.
4. If RFC 0001's resume rule over the lines already written would not
   name the head, a `label` entry whose `label` is the reserved value
   `leaf`, naming the head, carrying `synthetic: true`, appended as a
   child of the last line written, so that the rule lands a reader on
   the head. A genuine `leaf` label among the own entries stays in force
   in the file, which is why the test is the rule and not the last
   line. This entry is
   the projection's, not the session's: it is not in the log, the
   member says so, and reading the file back does not make it an entry.
5. A session whose `media` is `sidecar` projects its blobs beside the
   file, each named by its hash, since RFC 0001 counts a sidecar as
   part of the session and the file is not self-contained without it.

The prefix is what keeps the file self-contained: the material the
model was sent is in the file, and the header says where it came from.
It is the same duplication RFC 0001 accepts for compaction and the same
trade, a file that answers what the model was sent without resolving
anything, and a store pays it only on export.

Reading a projection back yields the same entries, since the hashes are
in the file and a reader verifies each, and the same head, from the last
own entry or the marker. A store imports a projection as follows: each
line but a synthetic marker is an entry it stores under its hash, the
prefix entries join no log, the own entries join the imported session's
log in file order, and the head is what RFC 0001's resume rule gives;
the marker names the head and is then discarded, so an export and import
cycle adds nothing, and a genuine `leaf` label a writer appended is an
entry like any other. Two refusals follow. The imported session keeps
the header's `id`, and a store already holding a session with that ID
MUST refuse the import. An importer MUST verify each line's hash and
MUST refuse a file in which one fails, so a redacted projection, which
RFC 0001 says no longer verifies and must say so in its header, cannot
be imported: it is a record to read, not one to hold.

## Verification

A reader of a projection verifies every entry's `id` against its
canonical bytes and reports a line that fails, as RFC 0001 requires of
every reader; a projection is a file like any other.

A reader holding a fork's projection and its origin's checks the fork
with one comparison: the fork header's `base` is one of the origin's
own entries or on its prefix.
Because the hash commits to the whole path, agreement on that hash is
agreement on every byte of the prefix. This is the check RFC 0001's
fork fixture performed by rebuilding requests at the fork point and
comparing; here it is one lookup.

`request_hash` is unchanged; RFC 0001 says how the two hashes divide
the work.

## Prefix caching

An entry hash identifies the context at that entry, so a router that
keys a provider's cached prefix on the hash of the last entry before a
request has a key that is stable across sessions sharing the prefix and
free to compute. Equal keys mean the same request. Unequal keys need not
mean a different one, since a `dispatch` or `run` entry on the path
changes the hash and not the request. RFC 0001's calibration applies
without change: equal hashes say the same entries were sent, and say the
model saw the same leading tokens only where the provider's template,
tool-schema serialisation and tokenizer are deterministic. The
writing-discipline rule that keeps per-run content out of the request is
what keeps sibling forks sharing a key.

## Reference implementation

The SQLite store of the reference library lays out as follows, and
another store is conforming if it satisfies the rules above by any
layout.

```sql
CREATE TABLE objects  (hash TEXT PRIMARY KEY, bytes BLOB NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, header TEXT NOT NULL,
                       base TEXT, head TEXT);
CREATE TABLE log      (session TEXT NOT NULL, seq INTEGER NOT NULL,
                       hash TEXT NOT NULL, PRIMARY KEY (session, seq),
                       UNIQUE (session, hash));
CREATE TABLE edges    (parent TEXT NOT NULL, child TEXT NOT NULL,
                       PRIMARY KEY (parent, child));
```

The sketch declares no foreign keys. The rows are immutable and
content-addressed, so a constraint buys little and costs a lookup on
every append; the append's own rules are what keep the tables in step.

`edges` is the reverse index the parent hashes cannot give: finding a
entry's children, a session's leaves and the subtree below a point all
walk it downward. It spans sessions, since an entry's children may be in
several logs, so a session's leaves are the entries of its log with no
child in that same log. A store at scale keys objects by hash, which
spreads writes rather than hot-spotting a tail, keeps a membership table
per session rather than a session column on the entry, since an entry
belongs to every session whose prefix it is on, and avoids a secondary
index ordered by time, which puts the tail of every busy session on one
range again.

## Relationship to RFC 0001

RFC 0001 defines what an entry is, what a file is, and how a reader
rebuilds a request from a path. This document defines where entries live
and what a session is. The dependency is one way: a conforming RFC 0001
file can be read without any store, and a store's projection is such a
file. What this document takes off RFC 0001 is everything a file was
doing that a ref does better: the durable leaf marker as a resume
mechanism, the ordering open question, and the fork rule that would
otherwise have had to make a copy testify to being one.

## Open questions

- What a store owes an importer of a projection whose prefix it already
  holds under a different session: nothing but the log entries, since
  the entries are the same entries. Whether it should record that the
  two sessions share a base is a query the `edges` table answers.
- Retention of swept objects: whether a store MUST keep a tombstone so
  a reference to a deleted session's own entry can be told from a
  reference to an entry that never existed.
