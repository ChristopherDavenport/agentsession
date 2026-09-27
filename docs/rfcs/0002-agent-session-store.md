# RFC 0002: Agent Session Store

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at draft 0.5 or later.

## Summary

A store holds sessions as content-addressed entries and mutable refs.
An **entry** is one of RFC 0001's, stored once under the hash of its
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
and it moves by compare-and-swap.

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
  can lose.
- **Nothing is garbage.** Every entry a session appended stays in its
  log. Abandoned branches are preference data, as RFC 0001 says.
- **Projection is lossless.** A session projects to an RFC 0001 file
  that reads back to the same entries and the same head.

## Non-goals

- A query language or index beyond what the projections need.
- A network protocol between stores. What a store owes when it sends or
  receives a session is the exchange section's; how the bytes move is
  not this document's.
- Defining the entry's contents. RFC 0001 does; this document stores
  them.
- A spool for model output in flight; see durability and recovery.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as
in RFC 2119.

- **Entry**: as RFC 0001 defines it, addressed by its hash. The store
  holds it as an envelope naming a content object.
- **Content**: the part of an entry hashed on its own, as the content
  section defines; two entries alike in content share one content
  object however they differ in parent or `ts`.
- **Object**: canonical bytes addressed by their hash: an entry, a
  content, or a media blob.
- **Context hash**: the incremental hash over the content hashes of the
  context entries on a path, as the prefix-caching section defines it.
  Record entries and responses do not enter it.
- **Session**: a ref, consisting of a header, a base, a head and a log.
- **Base**: the entry a session continues from, or none. A session with
  a base is a **fork** of the session that appended that entry.
- **Head**: the entry a session's next append names as parent, unless
  the appender names another.
- **Log**: the entries a session appended, in the order the store
  accepted them. These are the session's **own** entries.
- **Prefix**: the path from a session's base to its root. A session's
  prefix entries are, or were, another session's own entries; a store
  may hold them without holding that session.
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

### Content

An entry's hash covers its parent, its `ts` and its body together,
which is right for the identity of a record and wrong for everything
else a hash is wanted for. The same tool output under two parents would
be two objects, and nothing could be shared or compared on its own.
So a store separates them, as git separates a blob from the commit that
names it.

An entry's **content** is its canonical members with `id`, `parent`,
`parents` and `ts` removed and `type` kept, since a `label` and an
`info` alike in every other member are not one content; its **content
hash** is `sha256:` over those canonical bytes. The entry's `id` is
unchanged: it is still the hash over the whole canonical entry, and a
file carries the body inline and verifies as it did.
Underneath, a store MUST hold content once by content hash and MUST be
able to serve an entry from its envelope and its content. Identical
bodies in a thousand sessions are one content object with a thousand
envelopes naming it.

Below the hash, an object's bytes are the store's to lay out: chunked,
compressed, or deduplicated by any means, so long as the store serves
them by hash unchanged. Deduplication of large payloads is a storage
concern and not a format one, and this document does not push a
reference into the payload profile, which has no shape for one.

## Sessions

A session is created with a header and optionally a base.

- The header is RFC 0001's, and its `base` member is the base entry's
  hash or absent. `parent_session` is provenance a store does not
  validate: a fork made at an entry on another fork's prefix has a base
  in the grandparent's log, and the header may name either. A `fork_of`
  link records the session a fork was actually made from.
- A store MUST refuse to create a session whose base it does not hold or
  whose base is a `leaf` label, since the base is the first head and the
  head never rests on one, and MUST refuse one whose `media` differs
  from that of a session it holds whose own entries include the base,
  since the prefix was written in that form and a projection carries
  media in one form. A session created with a base has that base as its
  head.
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

Append is the one write that adds to a session, and it is atomic.

- The store MUST hold the parent, per the entry rules, and the parent
  MUST satisfy the session's parent rule above.
- The store MUST store the entry and MUST append its hash to the
  session's log at the next sequence number, unless the log already
  holds it, in which case the append is a no-op, reported as such, and
  nothing moves. Otherwise the store MUST move the head to the entry
  when the entry's parent is the head, or, for a `leaf` label, to its
  target when the head section allows and otherwise nowhere, whatever
  the label's parent; the head never rests on a `leaf` label. All three
  happen or none does.
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
  be the base or one of the session's own entries other than a `leaf`
  label, since the head never rests on one.
- Resume reads the head. There is no inference from log order and no
  marker to find. The head is what RFC 0001's leaf label was standing
  in for; the label survives as the projection's marker and as a head
  move a writer may still append.
- Moving the head to an earlier entry is how a session branches back.
  The next append under it is a new child, and the entries that were on
  the old head's path stay in the log as an abandoned branch.
- Appending a `label` entry whose `label` is the reserved value `leaf`
  moves the head to the label's `target` rather than to the label, once
  the entry is in the log, and does so wherever the label's own parent
  sits. A target that is not the base or one of the session's own
  entries, or that is itself a `leaf` label, moves nothing, and the
  store reports it: the label is accepted, since no reference conditions
  acceptance, and the head is held to its rule. A `leaf` label the log
  already holds is a no-op like any other re-append and moves nothing; a
  writer that wants the head moved again uses the compare-and-swap. That
  is the head move a bare file can express, where the label is honoured
  wherever it is, and a store honours it so a writer built against files
  behaves the same against a store. A store MUST refuse a `label` entry
  carrying `synthetic`, which marks a projection's own marker.

## Ordering

The log is the order. Each own entry has a sequence number the store
assigned as it accepted the append, and that sequence is what RFC 0001
calls entry order: it separates siblings and it says which branch this
store's log ends with. `ts` remains informational and a reader MUST NOT
order by it. Which branch is live is the head, never the order, and the
head is the fact that travels between stores; a sequence is a store's
own, as the exchange section says.

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
- After a crash a store MUST recover to a state in which every append it
  acknowledged as durable is present in full and no append is present in
  part: no log entry without its entry, no head moved to an entry the
  log lacks.
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

Nothing in a log expires, so retention is a policy over sessions and
not a sweep over entries: the log is the record, where git's reflog is
a convenience with a shelf life. A host that no longer wants a session
deletes it, and the store sweeps what nothing else needs. The lossless
form of that is to push the session to a store that keeps cold objects,
or to export its projection, and delete the ref here afterwards; a
session so archived is fetched or imported again and verifies on the
way in. Where a session is kept, the
cost of keeping it is the store's to tier, and content shared across
sessions is paid for once.

## Media

A media blob referenced by an item is an object under its own hash. A
store holds it once however many items reference it. RFC 0001's `media`
header field fixes at creation how the session carries media: inline as
a data URL inside the item, or beside the file under a sidecar directory
named after the session, where the file name is the blob's hex digest
alone, as RFC 0001 says. An item's bytes are hashed, so a store MUST NOT
convert between the two; a projection carries media in the form the
session was written in. A sidecar blob is an object of its own and is
held once; an inline data URL is part of its entry's content and is held
once only as that content is. An item names a sidecar blob as RFC 0001
says, by a `sidecar:` URL carrying the blob's hash, so a push's closure
over media is computable: the blobs the pushed entries' items name.

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
   in the file, which is why the test is the rule and not the last line.
   This entry is the projection's, not the session's: it is not in the
   log, the member says so, and reading the file back does not make it
   an entry.
5. A session whose `media` is `sidecar` projects its blobs beside the
   file, each named by its hex digest as RFC 0001 says, since RFC 0001
   counts a sidecar as part of the session and the file is not
   self-contained without it.

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
log in file order, and the head is what RFC 0001's resume rule gives,
held to the head rule: when the rule names a prefix entry the head is
the base, when it names a `leaf` label the head is the nearest ancestor
that is not one, and the store reports either. An import is held to the
checks a push is: the own entries hang from the base or from each other,
or from null in a baseless session, and `media` equals that of a held
session whose own entries include the base. The marker names the head
and is then discarded, so an export and import cycle adds nothing, and a
genuine `leaf` label a writer appended is an entry like any other. Two
refusals follow. The imported session keeps the header's `id`, and a
store already holding a session with that ID MUST refuse the import. An
importer MUST verify each line's hash and MUST refuse a file in which
one fails, and MUST refuse a file whose header carries `redacted`
whether or not its lines verify: a redacted projection is a record to
read, not one to hold.

## Exchange between stores

A session moves between stores as a push or a fetch. The wire protocol
is a non-goal; what a store owes when it sends or receives one is not.
Exchange is between stores and is not import: import is of a projection
file, and its refusal of a session the store already holds does not
apply here, where holding the session already is the usual case.

- **A push carries the closure of the session**: its own entries and
  their contents, its prefix entries and their contents, and its media
  blobs. The prefix goes because the receiver's parent rule needs it,
  and the receiver retains it under the prefix rule even when it never
  holds the origin session. A sender MAY negotiate what the receiver
  lacks. A receiver admits objects as it admits an append, hash
  verified and parent first, with one difference: admission under
  exchange moves no head. A `leaf` label among the pushed entries is
  an entry like any other here, and the head moves only by the
  compare-and-swap below.
- **A receiver that lacks the session** first admits the prefix, so that
  it holds the base, then creates the session from the pushed header
  with the pushed base as its head, or no head when there is no base,
  and then admits the own entries. A receiver that holds a session with
  that ID MUST refuse the push unless the pushed header equals the held
  one, since a header is written once at creation and two sessions alike
  only in ID are not one session; their union would be no session at
  all.
- **The log merges as a set.** The receiver takes the union of the two
  logs and assigns its own sequence in the order it admits entries,
  admitting a parent before its child so that the merged log projects as
  a valid file, and MUST refuse a push whose own entries do not each
  name as `parent` the base, another own entry of the union, or, in a
  session with no base, null. Two stores may hold one session with
  different log orders and both are correct: the order says which branch
  was written last in that store, and the head is the fact that travels.
  Sequence numbers are never synchronised.
- **The entries land whether or not the head moves.** A push is two
  steps, admission and then the head, and only the second can fail.
  The head moves by compare-and-swap, with the expected value the
  remote head the sender last saw, or the head a freshly created
  session has when the receiver lacked it. A push whose expected value
  is wrong leaves its entries in the log as a branch and reports that
  the head did not move, which is what an append that is not a
  fast-forward does. A force is an explicit override, and a store MUST
  distinguish it from a push that fast-forwarded. With the record mark
  below, a wrong expected value means two stores each believed they
  were the record: a mirror declared itself the record while the old
  one continued, or a handover was retried across a failure. The
  compare-and-swap is what reveals it. There is no merge of heads: the
  two lines are a fork, which the format represents, and the loser's
  answer is to push its line as a session with a base rather than to
  reconcile.
- **One store of record, enforced.** A store marks each session it holds
  as one it is the record for or a mirror of, and the reference schema
  carries the mark. The mark is set at birth: a session created locally
  or imported from a projection is the record here, since a file has no
  record elsewhere; a session created by a push or a fetch is a mirror
  unless the push is a handover. A store that is a mirror MUST refuse a
  local append and a local head move for that session, accepting both
  only through exchange, so a mirror's head follows the record's because
  nothing else can move it. A push from a store that is not the record
  MUST be refused. A handover is a push that sets the mark at the
  receiver and then clears it at the sender, each atomically with its
  own step and in that order, so that a failure between the two leaves
  two records and never none; two records are what the compare-and-swap
  above reveals, and the sender finishes the handover by clearing. A
  mirror whose record has deleted the session without handing it over
  may declare itself the record, since nothing else can advance it.

Fetch is the reverse, and any store may fetch from any store that holds
the session, a mirror included, since a mirror holds what the record
pushed it. A fetch admits entries as a push does and moves the fetcher's
head to the fetched head only when the fetcher is a mirror and the
fetched head descends from the fetcher's or the fetcher has none, so a
fetch from a stale mirror moves nothing and says so; a record's head
moves only by its own writers. Publishing a corpus is pushing a
manifest, which a later document defines, and the objects it closes
over. Archiving a session is a handover to a store that keeps cold
objects followed by deleting the ref here, and taking it back is a
handover the other way; a fetch alone yields a mirror, which is what a
reader wants and a writer does not.

## Verification

A reader of a projection verifies every entry's `id` against its
canonical bytes and reports a line that fails, as RFC 0001 requires of
every reader; a projection is a file like any other.

A reader holding a fork's projection and its origin's checks the fork
with one comparison: the fork header's `base` is one of the origin's own
entries or on its prefix. Because the hash commits to the whole path,
agreement on that hash is agreement on every byte of the prefix. This is
the check RFC 0001's fork fixture performed by rebuilding requests at
the fork point and comparing; here it is one lookup.

`request_hash` is unchanged; RFC 0001 says how the two hashes divide
the work.

## Prefix caching

An entry hash is the wrong key for a provider's cached prefix. It
covers `ts`, so two sessions that sent the same system prompt and the
same first message a second apart have different hashes where the
provider has one prefix, and it covers record entries, so a `dispatch`
on the path changes the hash and not the request.

The **context hash** is the key a store can compute as it appends. It
is defined over the path ending at an entry, by the entry's type and
not by any later leaf:

- An entry **contributes** when its type is `item`, `config`,
  `compaction` or `branch_summary`. A `response`, a record entry and an
  extension entry do not.
- The context hash before any contributing entry is `sha256:` over the
  canonical bytes of the JSON value `null`. A root that does not
  contribute has that value.
- At an entry that does not contribute, the context hash is its
  parent's, unchanged.
- At an entry that contributes, the context hash is `sha256:` over the
  canonical bytes of a two-element JSON array: the parent's context
  hash, then this entry's content hash, both as strings carrying their
  `sha256:` prefix.

It excludes `ts` and `parents`, it is incremental, and two sessions
whose context entries are byte-identical share it. Sibling forks share
it up to the fork by construction. Two limits are worth knowing. A
`compaction` names `first_kept` and a `branch_summary` names `from`,
both entry hashes that cover `ts`, so past the first of either on a
path the key is shared only by sessions that share those entries, which
forks do and independent sessions do not. And content covers every
member of an entry, undefined ones included, so a harness member on an
`item` that never reaches the model still moves the key; a harness that
wants the key stable keeps such detail in a record entry, as the
writing discipline already asks of per-run content. And an extension
entry that its extension puts in context is excluded, since a store
cannot know the extension, so two paths that differ only in such an
entry share a key while their requests differ; `request_hash` tells
them apart.

It is a hash over history, and `request_hash` is a hash over what was
sent. On a path with no compaction the two identify the same request.
After a fold they part, since the context hash still covers the entries
the fold excluded and `request_hash` covers the summary that replaced
them; a router that wants a key past a fold computes `request_hash`
from the path as a reader would. Keying a router on either is a
routing decision, and RFC 0001's calibration is about what neither can
promise: equal hashes say the same entries were sent, and say the model
saw the same leading tokens only where the provider's template,
tool-schema serialisation and tokenizer are deterministic. The
writing-discipline rule that keeps per-run content out of the request
is what keeps the key stable at all.

## Reference implementation

The SQLite store of the reference library lays out as follows, and
another store is conforming if it satisfies the rules above by any
layout.

```sql
CREATE TABLE contents (hash TEXT PRIMARY KEY, bytes BLOB NOT NULL);
CREATE TABLE entries  (hash TEXT PRIMARY KEY, parent TEXT, ts TEXT NOT NULL,
                       parents TEXT, content TEXT NOT NULL,
                       context TEXT NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, header TEXT NOT NULL,
                       base TEXT, head TEXT,
                       record INTEGER NOT NULL);
CREATE TABLE log      (session TEXT NOT NULL, seq INTEGER NOT NULL,
                       hash TEXT NOT NULL, PRIMARY KEY (session, seq),
                       UNIQUE (session, hash));
CREATE TABLE edges    (parent TEXT NOT NULL, child TEXT NOT NULL,
                       PRIMARY KEY (parent, child));
```

`contents` holds each body once, media blobs included; `entries` is the
envelope, naming its content and carrying the context hash the store
computed at append; `record` is the mark the exchange section enforces,
set when this store may advance the session. The sketch declares no
foreign keys. The rows are immutable and content-addressed, so a
constraint buys little and costs a lookup on every append; the append's
own rules are what keep the tables in step.

`edges` is the reverse index the parent hashes cannot give: finding an
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
