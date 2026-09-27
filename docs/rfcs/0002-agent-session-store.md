# RFC 0002: Agent Session Store

Status: draft
Author: Christopher Davenport
Depends on: RFC 0001, Agent Session Format, at the version that makes
an entry's `id` its content hash.

## Summary

A store holds sessions as content-addressed nodes and mutable refs. A
**node** is one entry of RFC 0001, stored once under the hash of its
canonical bytes, immutable, and naming its parent by hash, so that a
leaf hash commits to the whole path above it. A **session** is a ref:
a header, a **base** node it continues from or none, a **head** node
its next append will name as parent, and a **log** of the nodes it
appended, in the order the store accepted them.

The JSONL file of RFC 0001 is a projection of a session: its header,
the path to its base, then its own nodes in log order. A reader
verifies a projection node by node by recomputing hashes, and verifies
a fork against its origin by comparing one hash. Nothing in a file is
trusted that can be checked.

Two writers appending to one session both succeed. The store assigns
each node its place in the log, and the head moves only when an append
continues it; an append that does not is a branch, recorded and left
where it was. The only contended write is the head, and it moves by
compare-and-swap.

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

- **Content-addressed.** A node is stored once under its hash. Sessions
  sharing a prefix share its nodes.
- **Verifiable.** A projection is checked hash by hash; a fork is
  checked against its origin by one hash.
- **Lock-free appends.** Concurrent writers to one session both
  succeed; only the head is contended, and it never blocks an append.
- **Nothing is garbage.** Every node a session appended stays in its
  log. Abandoned branches are preference data, as RFC 0001 says.
- **Projection is lossless.** A session projects to an RFC 0001 file
  that reads back to the same nodes and the same head.

## Non-goals

- A query language or index beyond what the projections need.
- A network protocol between stores. Replication is by exchanging
  nodes and refs, and how is a store's business.
- Defining the node's contents. RFC 0001 does; this document stores
  them.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as
in RFC 2119.

- **Node**: one RFC 0001 entry, addressed by its hash.
- **Object**: the canonical bytes of a node or a media blob, addressed
  by their hash. Every node is an object; not every object is a node.
- **Session**: a ref, consisting of a header, a base, a head and a log.
- **Base**: the node a session continues from, or none. A session with
  a base is a **fork** of the session that appended that node.
- **Head**: the node a session's next append names as parent, unless
  the appender names another.
- **Log**: the nodes a session appended, in the order the store
  accepted them. These are the session's **own** nodes.
- **Prefix**: the path from a session's base to its root. A session's
  prefix nodes are another session's own nodes.
- **Projection**: an RFC 0001 file built from a session.

## Nodes

A node's hash is defined by RFC 0001: `sha256:` followed by the
lowercase hexadecimal SHA-256 of the entry's canonical bytes with its
`id` member removed, canonical meaning the JSON Canonicalization Scheme
(RFC 8785). The `parent` member names a node by hash, and so do
`parents`, `target`, `first_kept`, `from` and `queued_from` wherever
RFC 0001 has them name an entry.

- A store MUST store a node under its hash and MUST NOT store two
  objects under one hash. Storing a node whose hash is already present
  is a no-op that succeeds.
- A store MUST NOT accept a node whose `parent` it does not hold, and
  MUST NOT accept one whose `parents` name a node it does not hold
  unless the reference carries a `session` the store does not hold
  either, in which case the reference is foreign provenance and is
  accepted unresolved, as RFC 0001 permits.
- A node is immutable. There is no update and no in-place correction;
  a correction is a new node, as RFC 0001 already requires of entries.
- Acyclicity is structural. A node names its parent by a hash that
  exists before the node does, so no node can name a descendant.

Two appends of the same content under the same parent at the same `ts`
are one node. A writer that means two entries makes them differ; `ts`
at sub-second precision is what usually does. A store MAY report to an
appender that the node it appended already existed.

## Sessions

A session is created with a header and optionally a base.

- The header is RFC 0001's, and its `base` member is the base node's
  hash or absent. `parent_session`, when set, names the session whose
  log holds the base node, or for a subsession with no base the
  session that spawned it.
- A store MUST refuse to create a session whose base it does not hold.
- A session with no base is a fresh root. Its first append is a root
  node, `parent` null.
- A session's own node MUST name as `parent` the session's base or one
  of the session's own nodes. Branching above the base is a new session
  with a lower base, not an append to this one, so that a session's
  base is the one point it diverged from and the prefix is exactly the
  path to it.
- A session MAY have several roots only when it has no base. A session
  with a base has one prefix and everything it appends hangs from it.

A fork is a session created with a base. Nothing is copied: the fork's
prefix is the origin's nodes, stored once. The header's `base` says
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

- The store MUST hold the parent, per the node rules, and the parent
  MUST satisfy the session's parent rule above.
- The store MUST store the node, MUST append its hash to the session's
  log at the next sequence number, and MUST then, if the node's parent
  is the session's head, move the head to the node. All three happen or
  none does.
- If the node's parent is not the head, the head does not move. The
  append succeeded and created a branch. The store MUST tell the
  appender which happened.

Two writers appending to one session at once both succeed. If both
name the head as parent, the store serialises them: the first moves the
head, the second finds its parent is no longer the head, is recorded as
a branch, and is told so. Whether the second writer then re-appends
under the new head, sets the head to its own node, or leaves the branch
is the writer's decision, not the store's. This is the whole of the
concurrency model, and it is the one git has for refs.

The durability rules of RFC 0001's writing discipline apply to append:
a node whose type the header names in `records` MUST be durable before
the side effect it precedes, so a store's append MUST NOT return before
the node is durable when the store is asked for that, and a store that
buffers MUST offer an append that does not.

## Head

The head is a session's resume point, and it is a ref.

- A store MUST offer a compare-and-swap on the head: move the head from
  an expected node to a given node, or fail if the head is not the
  expected node. The target MUST be the base or one of the session's
  own nodes.
- Resume reads the head. There is no inference from log order and no
  marker to find. The head is what RFC 0001's leaf label was standing
  in for, and the label survives only in the projection, below.
- Moving the head to an earlier node is how a session branches back.
  The next append under it is a new child, and the nodes that were on
  the old head's path stay in the log as an abandoned branch.

## Ordering

The log is the order. Each own node has a sequence number the store
assigned as it accepted the append, and that sequence is what RFC 0001
calls entry order: it separates siblings and it says which branch was
written last. `ts` remains informational and a reader MUST NOT order
by it. Which branch is live is the head, never the order.

This answers the question RFC 0001 left open, who assigns the sequence
when two writers append at once: the store does, always, because it
serialises appends to a log even when it accepts them concurrently. No
session is short of an order, and no reader has to guess a head from
one.

## Deletion and retention

- A store MUST NOT delete a node that any session's log references.
- Deleting a session removes its ref, its header and its log. Nodes
  referenced by no remaining log MAY then be swept. Nodes on a deleted
  session's prefix belong to other logs and stay.
- A store MUST NOT sweep by reachability from heads. A session's
  abandoned branches are in its log and are its record; RFC 0001's
  outcome and preference rules depend on them. What a store may sweep
  is an object no log names, and it may tier cold objects wherever it
  likes.

## Media

A media blob referenced by an item is an object under its own hash. A
store holds it once however many items reference it. RFC 0001's
`media` header field says how a projection carries it: inline as a data
URL, or beside the file under a sidecar directory named after the
session, where the file name is the blob's hash.

## Projection to JSONL

`ToJSONL(session)` builds an RFC 0001 file:

1. The header, with `base` set when the session has one.
2. The prefix, root first: the path from the root to the base, every
   node on it, each as its canonical line with `id` set to its hash.
3. The session's own nodes in log order, the same way.
4. If the head is not the last node in the log, a `label` entry of the
   reserved leaf kind naming the head, appended as a child of the last
   node, so that RFC 0001's rule for honouring the marker lands a
   reader on the head. This entry is the projection's, not the
   session's: it is not in the log, and reading the file back does not
   make it a node.

The prefix is the ingress rule at work: the material the model was
sent is in the file, materialised, and the header says where it came
from. It is the same duplication RFC 0001 accepts for compaction and
the same trade, a file that answers what the model was sent without
resolving anything, and a store pays it only on export.

Reading a projection back yields the same nodes, since the hashes are
in the file and a reader verifies each, and the same head, from the
last own node or the marker. A store MUST accept a projection as an
import: each line is a node it stores under its hash, the prefix nodes
join no log, the own nodes join the imported session's log in file
order, and the head is what the file's rule gives.

A projection that has been redacted no longer verifies, because
redaction changes the bytes the hashes were taken over. That is by
design. A redacted file MUST say so in its header, and a reader MUST
NOT treat its hashes as verified; it may still treat its structure as
a valid RFC 0001 file, because every rule but verification holds.

## Verification

A reader of a projection MUST verify every entry's `id` against the
hash of its canonical bytes and MUST report a line that fails. A
failing line is corruption or tampering, not an unknown extension, and
a reader MUST NOT repair it.

A reader holding a fork's projection and its origin's checks the fork
with one comparison: the fork header's `base` is in the origin's log.
Because the hash commits to the whole path, agreement on that hash is
agreement on every byte of the prefix. This is the check RFC 0001's
fork fixture performed by rebuilding requests at the fork point and
comparing; here it is one lookup.

`request_hash` is unchanged and does a different job. A node hash
identifies a record, timestamps included, so a replay never collides
with the original. A request hash identifies what the model was sent,
timestamps excluded, so a replay that sent the same request matches.
A verifier uses both.

## Prefix caching

A node hash identifies the context at that node, so a router that keys
a provider's cached prefix on the hash of the last node before a
request has a key that is exact, stable across sessions sharing the
prefix, and free to compute. RFC 0001's calibration applies without
change: equal hashes say the same entries were sent, and say the model
saw the same leading tokens only where the provider's template,
tool-schema serialisation and tokenizer are deterministic. The
writing-discipline rule that keeps per-run content out of the request
is what keeps sibling forks sharing a key.

## Reference implementation

The SQLite store of the reference library lays out as follows, and
another store is conforming if it satisfies the rules above by any
layout.

```sql
CREATE TABLE objects  (hash TEXT PRIMARY KEY, bytes BLOB NOT NULL);
CREATE TABLE sessions (id TEXT PRIMARY KEY, header TEXT NOT NULL,
                       base TEXT REFERENCES objects(hash),
                       head TEXT REFERENCES objects(hash));
CREATE TABLE log      (session TEXT NOT NULL REFERENCES sessions(id),
                       seq INTEGER NOT NULL, hash TEXT NOT NULL,
                       PRIMARY KEY (session, seq));
CREATE TABLE edges    (parent TEXT NOT NULL, child TEXT NOT NULL,
                       PRIMARY KEY (parent, child));
```

`edges` is the reverse index the parent hashes cannot give: finding a
node's children, a session's leaves and the subtree below a point all
walk it downward. A store at scale keys objects by hash, which spreads
writes rather than hot-spotting a tail, keeps a membership table per
session rather than a session column on the node, since a node belongs
to every session whose prefix it is on, and avoids a secondary index
ordered by time, which puts the tail of every busy session on one
range again.

## Relationship to RFC 0001

RFC 0001 defines what a node is, what a file is, and how a reader
rebuilds a request from a path. This document defines where nodes live
and what a session is. The dependency is one way: a conforming RFC 0001
file can be read without any store, and a store's projection is such a
file. What this document takes off RFC 0001 is everything a file was
doing that a ref does better: the durable leaf marker as a resume
mechanism, the ordering open question, and the fork rule that would
otherwise have had to make a copy testify to being one.

## Open questions

- Whether `ts` belongs in the hash. In: a record's identity includes
  when it happened, and a replay never collides with the original. Out:
  identical content under one parent is one node whatever the clock
  said, which is what a cache wants. Held: in, since a node is a
  record and `request_hash` already serves the cache.
- Hash agility. `sha256:` is the only prefix; a store meeting another
  MUST refuse it. Whether to admit a second algorithm before there is a
  reason to is held: no.
- What a store owes an importer of a projection whose prefix it
  already holds under a different session: nothing but the log entries,
  since the nodes are the same nodes. Whether it should record that the
  two sessions share a base is a query the `edges` table answers.
- Retention of swept objects: whether a store MUST keep a tombstone so
  a reference to a deleted session's own node can be told from a
  reference to a node that never existed.
