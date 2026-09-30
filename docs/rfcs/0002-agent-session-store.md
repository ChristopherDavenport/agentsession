# RFC 0002: Agent Session Store

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at draft 0.5 or later.

## Summary

A store holds sessions as content-addressed entries and mutable refs. An
**entry** is one of RFC 0001's, stored once, its body under the hash of
the body and its envelope under the hash of the envelope, immutable, and
naming its parent by hash, so that a leaf hash covers the whole path
above it. A **session** is a ref: a header, a **base** entry it
continues from or none, a **head** entry its next append will name as
parent, and a **log** of the entries it appended, in the order the store
accepted them.

A session is to its entries what a git ref is to the history it names.
Its log is its own write-ahead log, and no log, lock or recovery spans
sessions. A writer chooses how often to **commit**, making the
session's appends durable, and how much to leave as **working state**,
which a crash may take.

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
- **No writer holds a lock.** Concurrent writers to one session both
  succeed; the store serialises each append only for as long as it
  takes to write its record, and the head is the only write a writer
  can lose.
- **Nothing is garbage.** Every entry a session appended stays in its
  log. Abandoned branches are the preference data a corpus is judged on,
  and RFC 0001's `outcome` entries score them.
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
- **Content**: an entry's body, hashed on its own, as RFC 0001 defines;
  two entries alike in content share one content object however they
  differ in type, parent or `ts`.
- **Object**: canonical bytes addressed by their hash: an envelope, a
  content, or a media blob.
- **Context hash**: the incremental hash over the types and content
  hashes of the entries on a path whose type is `item`, `config`,
  `compaction` or `branch_summary`, as the prefix-caching section
  defines it. Record entries and responses do not enter it.
- **Session**: a ref, consisting of a header, a base, a head and a log.
- **Base**: the entry a session continues from, or none. A session with
  a base is a **fork** of the session that appended that entry.
- **Head**: the entry a session's next append names as parent, unless
  the appender names another.
- **Log**: the entries a session appended, in the order the store
  accepted them. These are the session's **own** entries.
- **Commit**: making a session's appends durable; see durability and
  recovery.
- **Working state**: a session's appends the store has accepted but not
  yet committed; a crash may take them.
- **Prefix**: the path from a session's base to its root. A session's
  prefix entries are, or were, another session's own entries; a store
  may hold them without holding that session.
- **Projection**: an RFC 0001 file built from a session.

## Entries

An entry's hashes are defined by RFC 0001 in two layers: a content hash
over the body, and the entry's `id` over the envelope, which carries the
content hash. A store checks an `id` from an envelope and a content hash
without the body, and holds the two as separate objects. The `parent`
member names an entry by hash, and so do `parents`, `target`,
`first_kept`, `from` and `queued_from` wherever RFC 0001 has them name
an entry.

- A store computes an entry's hashes itself and MUST refuse an append
  whose `id` is present and differs from it, and MUST refuse one that an
  RFC 0001 reader would report: a body carrying a top-level member by
  one of the envelope's reserved names, a `ts` not in the one form that
  document requires, or a member that is not I-JSON. Each hashes
  consistently, so the `id` alone would not catch it, and a store
  holding one would project a line every reader must reject. A store
  MUST store an envelope under its `id` and a content under its content
  hash, and MUST NOT store two objects under one hash within a space.
  There are three hash spaces: envelopes, contents and media blobs.
  Contents and blobs are both bodies and MAY share a lookup table;
  envelopes MUST NOT share one with either; the context hash is never
  stored as an object. Storing an object whose hash is already present
  is a no-op that succeeds. `sha256:` is the only prefix, and a store
  MUST refuse an entry whose `id` carries another.
- A store MUST NOT accept an entry whose `parent` it does not hold. No
  other reference is a condition of acceptance: `parents`, `target`,
  `first_kept`, `from` and `queued_from` are the writer's to place, as
  RFC 0001 says, and a store MAY report one it cannot resolve.
- An entry is immutable. There is no update and no in-place correction;
  a correction is a new entry, as RFC 0001 already requires of entries.
- Acyclicity is structural. An entry names its parent by a hash that
  exists before the entry does, so no entry can name a descendant.

Two appends of the same type, content and `parents` under the same
parent at the same `ts` are one entry. A writer that means two entries
makes them differ; `ts` at sub-second precision is what usually does. A
log holds a hash once, so the second such append to one session is a
no-op the store reports as such.

### Content

A single hash over an entry's parent, its `ts` and its body together
would be right for the identity of a record and wrong for everything
else a hash is wanted for: the same tool output under two parents would
be two objects, and nothing could be shared or compared on its own. So
RFC 0001 hashes in two layers, as git hashes a blob apart from the
commit that names it, and a store holds the two apart.

An entry's **content** is its body, the members outside the envelope,
and its **content hash** is `sha256:` over the body's canonical bytes;
the envelope carries the type, so a body's meaning comes from the entry
that names it, as a git blob's comes from its tree entry, and a body
shared across types is one object. The `id` is the hash of the envelope
over the content hash, as RFC 0001 defines, so a file still carries the
body inline and verifies line by line, and a chain of envelopes verifies
without its bodies. Underneath, a store MUST hold content once by
content hash and MUST be able to serve an entry from its envelope and
its content. Identical bodies in a thousand sessions are one content
object with a thousand envelopes naming it.

Below the hash, an object's bytes are the store's to lay out: chunked,
compressed, packed with other objects into one file as git packs loose
objects, or deduplicated by any means, so long as the store serves
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
  head. A store MUST commit the log that holds the base, through the
  base, before it creates the fork: a fork's recovery reads only its own
  log, and a base a crash took from its origin's working state would
  leave the fork hanging from nothing.
- A session with no base is a fresh root. Its first append is a root
  entry, `parent` null.
- A session's own entry MUST name as `parent` the session's base, one of
  the session's own entries, or, in a session with no base, null.
  Branching above the base is a new session with a lower base, not an
  append to this one, so that a session's base is the one point it
  diverged from and the prefix is exactly the path to it.
- A session MAY have several roots only when it has no base. A session
  with a base has one prefix and everything it appends hangs from it.
- A session's header is written at creation and changes afterwards only
  in `format`. A store MUST raise the header's `format` to the minor of
  a writer that appends to the session, when that minor is later than
  the header's, durably and before the append is accepted, as RFC
  0001 requires of a writer of a file; so a reader of an earlier minor
  refuses a session a later one has continued, rather than reading
  entries it cannot represent. A raise that outlives an append a crash
  took is harmless. A store never lowers it, and a fork's header names
  a format no earlier than that of the session whose own entries
  include its base, since its prefix was written under that one.

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

- An append carries the `format` its writer writes, the version of
  RFC 0001 under which it built the entry. A store MUST refuse an
  append whose `format` names a major or a 0.x minor it does not read,
  and one whose minor is earlier than the session's header names,
  since that writer could not have read the session it appends to; a
  later one raises the header, as the sessions section says. A store
  that is itself the writer, as a library's store is, carries its own
  `format` and has nothing to check but the header.
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

Durability is the next section's concern: an append is atomic, and a
store offers an append that returns only once committed, which RFC
0001's writing discipline requires for an entry whose type the header
names in `records`; other appends may be left as working state until a
later commit.

## Head

The head is a session's resume point, and it is a ref.

- A store MUST offer a compare-and-swap on the head: move the head from
  an expected entry to a given entry, or fail if the head is not the
  expected entry. "No head" is a valid expected value. The target MUST
  be the base or one of the session's own entries other than a `leaf`
  label, since the head never rests on one.
- Resume reads the head. There is no inference from log order and no
  marker to find. The head is what RFC 0001's leaf label was standing in
  for. The label exists only because an append-only file with a
  header that changes only in `format` has nowhere else to record a
  head move; it survives
  as the projection's marker and as a head move a file-bound writer may
  still append, and a writer built against a store uses the
  compare-and-swap instead.
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

A session is a ref and its entries are objects, as a git branch is a
ref over the history it names. What makes an entry part of a session is
the session's log, so what accepts and commits an append belongs to the
session alone, as a ref's update does in git: a store writes no record
wider than the session to accept an append, and replays no record wider
than the session to recover one. Objects are shared across sessions and
are written, read and swept as the entries and retention sections say.

An append is three writes — the object, the log entry, the head — and
the store promises them as one.

- A store MUST accept an append at a single point, after which it is
  visible and may be acknowledged, and before which nothing of it is
  visible. Accepting is not committing: an append may be accepted and
  committed later. An object written ahead of its append's acceptance
  is not an entry the store holds, so it cannot satisfy another append's
  parent rule.
- A store MUST offer an append that does not return until it is
  committed. A writer MUST use it for an entry the header's `records`
  names that precedes a side effect, as RFC 0001's writing discipline
  requires, and SHOULD for a `response` entry and for a
  `function_call_output` item, which is where that discipline asks for
  an fsync. A store MAY acknowledge other appends before they are
  committed, and MUST say which it did. A store MUST also offer a
  commit of a session on its own, so a writer that needs what it has
  appended durable need not append again to get it.
- A head move by compare-and-swap is accepted and committed as an
  append is, and a store MUST offer a durable one and say which it
  performed. A record mark is committed before it is acknowledged.
- After a crash a store MUST recover to a state in which every append it
  acknowledged as committed is present in full and no append is present
  in part: no log entry without its entry, no head moved to an entry the
  log lacks.
- An object whose bytes do not hash to its name is corrupt. A store
  MUST NOT serve it and MAY discard it. Content addressing is what makes
  a half-written object detectable and a rewrite of it harmless.

### Working state and commits

A session's appends accepted and not yet committed are its **working
state**. Unlike git's working tree, working state is in the log: it
moves the head as any append does, and a reader of the session sees it,
but a crash may take it. A **commit** makes a session's working state
durable, with the append or the request that asks for it: first every
object its appends name, including one already present when an append
wrote it, since another session may have written that one and not yet
committed it, and then the log through the commit. What durability
costs a store is paid per commit and not per append, so how often a
writer commits is how it trades speed for what a crash can take. RFC
0001's writing discipline names where a commit is required or
recommended; elsewhere a writer MAY leave an append uncommitted. A
commit covers one session: it does not make another session's working
state durable, and need not wait for another session's commit, though a
store MAY take the commits of one process in turn.

After a crash, a store keeps a session's working state up to the first
append whose objects the crash took or left corrupt, and drops that
append and every record the session accepted after it, head moves and
marks included, as the loss of an uncommitted tail and not as damage.
What it keeps, it commits before it accepts anything more in the
session. A writer that must know what survived reads the head.

### A log per session

The design that meets these on a filesystem is a log per session that
is the session's write-ahead log and its record. Git has no
counterpart: its ref is the record and its reflog a convenience, where
here the log is the record and the head an index of it. Each record is
one line carrying a checksum: an append, naming the entry, its sequence
and where the head went; a head move; a record mark, as the exchange
section defines it; a commit, after which recovery checks no object of
an earlier record; or a loss, naming an append recovery dropped. The
objects are written first, idempotently, since a second write of the
same bytes under the same hash changes nothing; then the records; then
the head, which is an index of the head the log's last record names and
is rebuilt from it. An append is accepted when its record is written,
and a commit is an fsync of its objects and then of the log. An fsync
that fails may leave pages marked written that never reached the disk,
and a later fsync of the same file can succeed without writing them,
so after a failed fsync a store MUST NOT count what it covered as
committed until it has written those bytes again, to a new file, or
recovered the session from what the disk holds. A store
appends to a session's log under the session's own lock, as git
updates a ref under its lock file and as the ordering section requires,
so appends to different sessions commit independently and at once, and
a filesystem that journals its metadata joins their fsyncs into one of
its own. The header is a file of its own, written at creation and
replaced, durably, when its `format` is raised.

A session is created by writing its header and its first records where
no session is, and making them visible under its ID in one step that
fails if the ID is held, such as renaming a directory into place; it is
deleted in one step after which nothing of it is read, such as renaming
its directory away. A session created afterwards under the same ID
starts from nothing.

A crash damages only what was written after a session's last commit: it
may cut the log short or, on a filesystem that writes a file out of
order, leave blocks of that uncommitted tail unwritten, and what it
takes there is working state, dropped as a crash's loss. Anything else
that fails its checksum, a record or bytes that are no record, is
damage, reported rather than skipped, since the log is the session's
only record. The head, the record mark and any other index are rebuilt
from the log and never read over it, so a crash that leaves one behind
or ahead of the log changes nothing. Recovery reads one session's log,
when the session is opened, so what a store pays to recover a session is
proportional to that session's log, however large the store has grown. A
session's log is deleted with the session and holds nothing of any
other, so there is nothing store-wide to compact.

A store built on a database that has its own write-ahead log gets
atomicity and recovery from the database. Durability it must still ask
for: SQLite's WAL commit at `synchronous=NORMAL`, which the reference
store runs, is not fsynced, so its durable append is what sets the
pragma or forces the checkpoint. A store built on object storage, which
has no append, writes a session's log as objects named by the session
and a sequence, each written only if absent, so a commit is the
acknowledged write of one such object. There a writer MAY batch appends
and submit them together; until the store writes them they are not
accepted, and when it does it assigns their sequences and tells the
writer which continued the head.

What a session's log does not hold is a model call in flight. RFC 0001
forbids writing partial output as an entry, so the bytes of a response
still streaming are a harness's to buffer, outside the store, and reach
it as entries only when the items are complete. That spool is not this
document's concern, and it is not this log.

## Deletion and retention

- A store MUST NOT delete an entry that any session's log references.
- A store MUST NOT sweep an entry on any session's prefix, its base
  included: the session's projection needs it.
- Deleting a session removes its ref, its header and its log in one
  step, as the durability section says. Entries referenced by no
  remaining log and on no remaining prefix MAY then be swept.
- An object no log names may belong to an append the store has yet to
  accept. A store sets a grace period and MUST NOT sweep an object
  younger than it, as git's collector spares a young loose object for
  `gc.pruneExpire`; and when it accepts an append it MUST hold that
  append's objects, writing back any a sweep took, which is harmless
  since a write of an object is idempotent.
- An append recovery dropped as lost, or one no commit covers whose
  objects a crash left torn, does not hold its objects for the first
  rule: nothing reads them, and a torn one is not damage for a sweep
  to stop at. A store MAY sweep them or keep them.
- A store MUST NOT sweep by reachability from heads. A session's
  abandoned branches are in its log and are its record; RFC 0001's
  `outcome` entries score them and a consumer's preference between
  branches is read from them. What a store may sweep is what nothing
  retained names, following references all the way down as git's
  collector follows a commit to its tree to its blobs: an envelope is
  retained while any log or prefix needs it; a content is retained while
  any retained envelope names it; a media blob is retained while any
  retained content names it through a `sidecar:` URL; only what is left
  may be swept, and a store may tier cold objects wherever it likes.

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
   `leaf`, naming the head, carrying `synthetic: true` and the head
   entry's own `ts`, so that one store projects one session to the same
   bytes every time and two stores whose logs agree do too, appended as
   a child of the last line written, so that the rule lands a reader on
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
that is not one, the base when no such ancestor lies above the base, and
no head in a baseless session with none, and the store reports any of
these. An import is held to the checks a push is: the own entries hang
from the base or from each other, or from null in a baseless session,
and `media` equals that of a held session whose own entries include the
base. The marker names the head and is then discarded, so an export and
import cycle adds nothing, and a genuine `leaf` label a writer appended
is an entry like any other. Two refusals follow. The imported session
keeps the header's `id`, and a store already holding a session with that
ID MUST refuse the import. An import is committed before it is
acknowledged. An importer MUST verify each line's hash and
MUST refuse a file in which one fails, and MUST refuse a file whose
header carries `redacted` whether or not its lines verify against
themselves, which RFC 0001 requires they do: a redacted projection is a
record to read, not one to hold.

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
  holds the origin session. A push carries the sender's record mark for
  the session, and until heads are signed, which a later document takes
  up, the receiver takes the sender's word for it: the rules here are
  what two honest stores agree to, not a defence against a dishonest
  one. A sender MAY negotiate what the receiver lacks. A receiver admits
  objects as it admits an append, hash verified and parent first, with
  one difference: admission under exchange moves no head. Exchange never
  rests on working state: a sender MUST commit the session before it
  pushes or serves it, and one that cannot, such as a store open
  read-only beside another process's writer, MUST serve only what that
  writer committed; a receiver MUST commit what it admits, and a mark
  it sets, before it acknowledges them. A `leaf` label
  among the pushed entries is an entry like any other here, and the head
  moves only by the compare-and-swap below.
- **A receiver that lacks the session** first admits the prefix, so that
  it holds the base, then creates the session from the pushed header
  with the pushed base as its head, or no head when there is no base,
  and then admits the own entries. A receiver that holds a session with
  that ID MUST refuse the push unless the pushed header equals the held
  one apart from `format`, since a header is fixed at creation but for
  its `format`, and two sessions alike only in ID are not one session;
  their union would be no session at all. The receiver keeps the later
  of the two formats, as the sessions section requires of an append, and
  refuses a push whose header names a minor it does not read.
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
- **One store of record, enforced between stores that exchange.** A
  store marks each session it holds as one it is the record for or a
  mirror of, and the reference schema carries the mark. The mark is set
  at birth: a session created locally is the record here; a session
  imported from a projection is a mirror unless the importer declares it
  the record, since a file cannot say whether the store that wrote it
  still holds the session and two records would otherwise surface only
  at the next push; a session created by a push or a fetch is a mirror
  unless the push is a handover. A store that is a mirror MUST refuse a
  local append and a local head move for that session, accepting both
  only through exchange, so a mirror's head follows the record's because
  nothing else can move it. A push from a store that is not the record
  MUST be refused. A handover is a push that sets the mark at the
  receiver and then clears it at the sender, each atomically with its
  own step and in that order, the sender clearing only once the
  receiver has acknowledged its mark committed, so that a failure
  between the two leaves two records and never none; two records are
  what the compare-and-swap above reveals, and the sender finishes the
  handover by clearing. A
  mirror whose record has deleted the session without handing it over
  may declare itself the record, since nothing else can advance it.

Fetch is the reverse, and any store may fetch from any store that holds
the session, a mirror included, since a mirror holds what the record
pushed it. The two rules turn on who initiates and not on which way the
bytes move: a push is initiated by the sender and only the record may
initiate one; a fetch is initiated by the receiver and any holder may
serve it, the record mark travelling with the session either way. A
fetch admits entries as a push does and moves the fetcher's head to the
fetched head only when the fetcher is a mirror and the fetched head
descends from the fetcher's or the fetcher has none, so a fetch from a
stale mirror moves nothing and says so; a record's head moves only by
its own writers. Publishing a corpus is pushing a manifest, which a
later document defines, and the objects it closes over. Archiving a
session is a handover to a store that keeps cold objects followed by
deleting the ref here, and taking it back is a handover the other way; a
fetch alone yields a mirror, which is what a reader wants and a writer
does not.

## Verification

A reader of a projection computes every entry's content hash and then
its envelope hash, compares the latter to its `id`, and reports a line
that fails, as RFC 0001 requires of every reader; a projection is a file
like any other.

A reader holding a fork's projection and its origin's checks the fork
with one comparison: the fork header's `base` is one of the origin's own
entries or on its prefix. Because the hash covers the whole path,
agreement on that hash is agreement on every byte of the prefix. RFC
0001's conformance suite asks for a forked fixture whose base is found
in its origin; this is that check, and it is one lookup.

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
  canonical bytes of a three-element JSON array: the parent's context
  hash, this entry's `type`, then this entry's content hash, the hashes
  as strings carrying their `sha256:` prefix. The type is there for the
  reason a git tree entry records a mode beside its blob hash: an `item`
  and a `config` with identical bodies are different context. For a
  `compaction` the content hash used here is over the content with
  `first_kept` replaced by a two-element array of the context hash at
  the entry it names and that entry's `type`, when the entry is on the
  path ending here, and by `null` otherwise — the type because a record
  entry and its contributing parent share a context hash while a
  compaction keeping from one keeps a different item list from one
  keeping from the other; for a `branch_summary` it is over the content
  with `from` removed, present or not, since the leaf that was left is
  provenance and not context, and a store need not hold it; and for an
  `item` it is over the content with `queued_from` removed, for the same
  reason. For every type, `legacy_id`, `normalised`, and on an `item`
  `response` and `source` are removed as well, since each is provenance
  a provider or a harness minted per run and none reaches the model;
  `response` above all, which a provider mints anew for every call, so
  two sessions replaying one conversation would otherwise never share a
  key past the first model output, so a migrated body and the same body
  written natively give one key while their content hashes differ. The
  key then depends on context and not on the identity of the entries
  that shaped it, and two stores compute it from the path alone.

It excludes `ts` and `parents`, it is incremental, and two sessions
whose context entries are byte-identical share it, a compaction
included. Sibling forks share it up to the fork by construction. Two
limits are worth knowing. Content covers every member of an entry,
undefined ones included, so a harness member on an `item` that never
reaches the model still moves the key; a harness that wants the key
stable keeps such detail in a record entry, as the writing discipline
already asks of per-run content. And an extension entry that its
extension puts in context is excluded, since a store cannot know the
extension, so two paths that differ only in such an entry share a key
while their requests differ; `request_hash` tells them apart.

It is a hash over history, and `request_hash` is a hash over what was
sent. On a path with no compaction the two identify the same request.
After a fold they part, since the context hash still covers the entries
the fold excluded and `request_hash` covers the summary that replaced
them; a router that wants a key past a fold computes `request_hash` from
the path as a reader would. Keying a router on either is a routing
decision, and RFC 0001's request hash section is about what neither can
promise: equal hashes say the same entries were sent, and say the model
saw the same leading tokens only where the provider's template,
tool-schema serialisation and tokenizer are deterministic. The
writing-discipline rule that keeps per-run content out of the request is
what keeps the key stable at all.

## Reference implementation

The SQLite store of the reference library becomes the following, from a
layout keyed on a session and a sequence today, and another store is
conforming if it satisfies the rules above by any layout.

```sql
CREATE TABLE contents (hash TEXT PRIMARY KEY, bytes BLOB NOT NULL);
CREATE TABLE entries  (hash TEXT PRIMARY KEY, type TEXT NOT NULL,
                       parent TEXT, ts TEXT NOT NULL, parents TEXT,
                       content TEXT NOT NULL, context TEXT NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, header TEXT NOT NULL,
                       base TEXT, head TEXT,
                       record INTEGER NOT NULL);
CREATE TABLE log      (session TEXT NOT NULL, seq INTEGER NOT NULL,
                       hash TEXT NOT NULL, PRIMARY KEY (session, seq),
                       UNIQUE (session, hash));
CREATE TABLE edges    (parent TEXT NOT NULL, child TEXT NOT NULL,
                       PRIMARY KEY (parent, child));
```

`contents` holds each body once, media blobs included, and a row leaves
it only when no retained `entries` row's `content` and no retained
content's `sidecar:` URL names it, an `entries` row being retained while
any log or prefix needs it; `entries` is the envelope, naming its
content and carrying the context hash the store computed at append;
`record` is the mark the exchange section enforces, set when this store
may advance the session. The sketch declares no foreign keys. The rows
are immutable and content-addressed, so a constraint buys little and
costs a lookup on every append; the append's own rules are what keep the
tables in step.

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
- Retention of swept objects: whether a store MUST keep a tombstone so a
  reference to a deleted session's own entry can be told from a
  reference to an entry that never existed.
- Shallow boundaries. A push carries the full prefix and a store holds
  an entry's parent before the entry, so a long session that compacted
  early pushes its pre-compaction history to every mirror forever. The
  envelope layer makes a chain verifiable without its bodies, so a store
  could hold envelopes above a compaction and no contents, as git holds
  a graft. Not needed yet.
- A base whose prefix converges an entry off the prefix. An entry on the
  path to the base may name in `parents`, with no `session`, an entry of
  the origin that is not on that path, such as a branch it merged. The
  fork's projection opens with the prefix alone, so it names an entry
  its file does not hold, and RFC 0001's reader refuses the file. The
  reference implementation refuses such a base at creation, in every
  store, so that nothing is written a reader cannot read. The
  alternatives are a reader rule that a prefix entry's `parents`
  resolves in the origin named by `parent_session`, or a projection that
  carries the merged entries beside the prefix; either changes what a
  fork's file holds, and neither is taken until a harness needs to fork
  below a merge.
