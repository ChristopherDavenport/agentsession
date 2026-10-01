# RFC 0001: Agent Session Format

Status: draft 0.10
Author: Christopher Davenport
Discussion: to be opened against this repository, then proposed to the
Open Responses community as a companion specification.

## Summary

A session is the record of one conversation between a harness and a
model, kept so that every request the model received can be rebuilt and
every call the model made can be followed to its output or to the point
the record stopped. It is an append-only JSONL file whose entries form a
tree: every entry names one `parent`, and a context is built by walking
it. An entry is named by the hash of its envelope, which names its body
by hash and its parent by hash, so a leaf's ID commits to the whole path
above it and a reader verifies a file line by line. An entry MAY
additionally name predecessors it converges — the results of subagent
sessions, a branch merged back — which record provenance and never enter
a context.

A file may be written directly, or projected from a store, which RFC
0002 defines. A session that continues from a point in another names
that point in its header, and its file opens with the path to it, so a
file stands alone as it always did while a reader holding both can
check the one against the other by a single hash.

The file holds two kinds of entry. **Context entries** are what the
model was sent and what it returned: items, responses, configuration,
compaction and the summary carried across a branch. Replaying them
along a path rebuilds a request byte for byte, without resolving
anything outside the file: material that arrived from elsewhere is
carried in the entry that received it, and the reference saying where
it came from is provenance beside it. **Record entries** are
what happened around the conversation: how a run started and how it
ended, that a tool call was dispatched, what was decided about a call,
the environment the tools ran in, links to other sessions, and
judgements of the result. They never enter the model's context.

The envelope is harness-neutral. The payload of a context entry is an
Open Responses item, so the record is the wire format itself.

This is a storage and resume format. ATIF covers evaluation
trajectories and OpenTelemetry covers observability; this format defines
a projection to each.

## Motivation

Each coding agent has written its own version of this file. pi stores a
JSONL tree with `id` and `parentId`. Claude Code stores a JSONL tree with
`uuid` and `parentUuid` and documents that the format changes between
releases.
Codex stores JSONL whose `response_item` lines are Responses API items.
OpenCode and Gemini CLI store whole-session JSON. Letta maintains fifteen
adapters to read them and drops the lifecycle entries in every one.
Harbor maintains its own adapters to get any of them into ATIF for
training.

They share the same shape: append-only lines, a header, a parent-linked
tree, typed lifecycle entries, and a payload that is the model API's own
message shape. None of them documents it, so a second harness cannot
write it and a third cannot read it. Every consumer writes adapters
instead, and the adapters keep the messages and drop compaction,
branching, configuration and outcomes, which is the data that makes a
session worth training on.

## Goals

- **Lossless.** A conforming file contains enough to rebuild every
  request the model received, byte for byte where the payload allows.
- **Resumable.** For every call without an output, a reader can tell
  from the path whether it was never started, was in flight when the
  record stopped, is waiting on an answer, or was rejected or answered
  and is owed only its output, in any file whose header says the
  writer records dispatches and decisions.
- **Append-only.** A writer only ever appends lines, save for raising
  the header's `format`, which Versioning describes. A crashed session
  is a valid prefix.
- **Content-addressed.** An entry's ID is the hash of its envelope
  over the hash of its body, and its parent link is a hash, so a file
  verifies itself and two sessions that share history share the same
  entries.
- **Tree-shaped context, DAG-shaped provenance.** A context is built by
  walking one `parent` per entry, so branching is a child of an earlier
  entry, in place. Convergence — a subagent's result, a branch merged
  back — is recorded beside that tree in `parents` and never widens the
  walk.
- **Forward compatible.** Readers preserve what they do not understand.
- **Harness-neutral envelope, normative payload.** The envelope carries
  no harness vocabulary. Conversation payloads are Open Responses items.
- **Projectable.** Defined mappings to ATIF for training and to
  OpenTelemetry for observability, keyed by the same entry IDs.

## Non-goals

- Multi-writer concurrency on one file. A store MAY accept concurrent
  appends to a session; a file is what it projects afterwards.
- Cross-session indexing, search or listing, storage layout, and the
  head a session in a store resumes at. Those are the store's, and RFC
  0002 defines the store; the file section's resume rule governs a bare
  file.
- Rendering hints beyond a display flag.
- Defining tool semantics. A tool is a name, a schema and a result.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as in
RFC 2119.

- **Session**: a header, a base or none, a head, and the entries
  appended under it. A file is its projection; a file written directly
  is a session whose store is the file.
- **Entry**: one JSON object on one line after the header.
- **Path**: the sequence of entries from an entry to a root, reversed,
  following `parent` alone.
- **Leaf**: the entry the next append will name as its parent. In a
  store this is the session's head; in a file it is what the file's
  rules give.
- **Base**: the entry, in another session, that this session continues
  from, named by the header. The entries from the root to the base are
  the file's **prefix**; the entries after it are the session's own.
- **Convergence**: an entry naming predecessors in `parents` beyond its
  `parent`, recording that their work was merged into this entry's
  payload.
- **Item**: an Open Responses item as defined by the Open Responses
  specification at the version named in the header.
- **Run**: one pass of the harness's loop, from an input to the point
  the harness stops calling the model. A session holds many runs.
- **Call**: one `function_call` item and everything that happens to it:
  a decision about it, its dispatch, its output.
- **In context**: contributing items or settings to the request the
  context algorithm builds. `item`, `config`, `compaction` and
  `branch_summary` are in context; `response` sits beside them as the
  envelope of a model call and contributes nothing; a record entry is
  not in context.
- **`ref`**: wherever it appears, a string in the harness's own terms
  that names the thing beside it. Readers treat it as opaque.

## File

- UTF-8, one JSON object per line, lines terminated by LF.
- The first line MUST be the header. Every later line MUST be an entry.
- A reader MUST tolerate a final line that does not parse and MUST
  report it. A writer MUST NOT rely on such recovery.
- Media referenced by items MAY be stored inline as data URLs or beside
  the file under a directory named after the session ID. A header field
  says which. A sidecar reference is a URL of the form
  `sidecar:sha256:<hex>`, the hash being the blob's, and the blob is the
  file named `<hex>` alone in the session's directory, since a colon is
  not a legal file name everywhere; a reference names its bytes and a
  reader verifies the blob against its hash, as it verifies a content.
- Entry order and `parent` links are the ordering, and they order
  different things: `parent` orders a path, since an ancestor precedes
  every entry below it, and says nothing between two children of one
  entry. Entry order is what separates siblings, so it is what decides
  which of several branches below a point was written last in the file
  that carries them. That is a fact about the file and not about the
  session: a store's projection orders siblings by its own log, which
  RFC 0002 defines and which another store may order otherwise. A
  reader choosing among branches SHOULD prefer the branch that holds
  the leaf the resume rule gives, and use order only among the rest. In
  a file projected from a store, entry order is path order through the
  prefix and then the store's log order for the session's own entries;
  the file has no other.
- A reader resuming from a file MUST take as leaf the newest entry in
  file order that descends from the entry the last `leaf` label in force
  names and was written after that label, the label itself excepted;
  that entry itself when nothing follows it; and the last entry of the
  file when no such label is in force. A `leaf` label is not in force
  when its target is not in the file, is on the prefix above the base,
  or is itself a `leaf` label, since the leaf may rest on none of those;
  a leaf so found that is itself a `leaf` label resolves to its nearest
  ancestor that is not one. The label marks the branch that is live, not
  the tip it had when marked, so a branch marked and then extended
  resumes where it was extended to. In a store the head is the leaf, and
  this rule is how a projection carries a head that is not the last
  line.
- A file whose header names a `base` opens with the prefix: every entry
  from the root to the base, in path order, before any entry the
  session appended itself. The prefix is another session's record,
  carried here so the file stands alone; the header's `records` promise
  and the rules that rest on it apply to the entries after the base.
- `ts` is informational and a reader MUST NOT order entries by it;
  clocks step backwards. This is a rule about the member an entry
  carries, which its writer asserts. A sequence a store assigns as it
  accepts entries — a line number, a row key, a commit timestamp the
  store's clock is authoritative for — is a different thing and is what
  entry order is in a file. A store MAY order by one.

## Header

```json
{"type":"session","format":"agentsession/0.10","id":"…","created_at":"2026-09-17T12:00:00Z",
 "payload":"openresponses/2026-04-24","harness":{"name":"…","version":"…"},
 "records":["run","dispatch","decision"],
 "cwd":"/path","parent_session":"…","base":"sha256:…","spawned_by":"call_…",
 "media":"inline"}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | the string `session` |
| `format` | MUST | `agentsession/<major>.<minor>`; raised by a later writer, never lowered, as Versioning says |
| `id` | MUST | globally unique; UUIDv7 RECOMMENDED. For a subsession, a UUIDv5 under the nil namespace over `<parent session id>/<call_id>` is RECOMMENDED, so a reader can compute the child's ID from the parent's `link` or `function_call` alone. A retry of the call is another session, derived the same way over `<parent session id>/<call_id>/<n>` for the n-th attempt after the first |
| `created_at` | MUST | RFC 3339 |
| `payload` | MUST | payload profile; `openresponses/<spec-date>` is the only profile this RFC defines |
| `harness` | SHOULD | name and version of the writer |
| `records` | SHOULD | the record entry types, core or namespaced, this writer writes whenever their event occurs, so a reader may take their absence as the event not having happened. Absent or empty means no such promise |
| `cwd` | MAY | working directory at creation; an `env` entry's `cwd` takes precedence from that entry on |
| `parent_session` | MAY | session ID this was forked or spawned from. Provenance, not validated: a fork made at an entry on another fork's prefix may name either session, and a `fork_of` link records the one it was made from |
| `base` | MAY | hash of the entry this session continues from, never a `leaf` label, since the base is a fork's first leaf; `parent_session` names a session holding it, as provenance. Absent for a session that starts fresh. When present the file opens with the path to it, and the session's own entries hang from it |
| `spawned_by` | MAY | for a subsession, the `call_id` of the parent's function call that spawned it |
| `media` | MAY | `inline` (default) or `sidecar`. Fixed when the session is created: an item's bytes are hashed, so a rewriter MUST NOT convert media from one form to the other |
| `redacted` | MAY | `true` when bodies were changed after they were written, as export redaction does. The redactor MUST recompute every content hash and `id` over the redacted bodies and rewrite `parent`, every entry-naming member — `parents` into this file, `target`, `first_kept`, `from`, `queued_from` — and last the header's `base`, once the prefix is hashed, to the IDs assigned earlier in the file, as migration does, leaving a reference into another session's file as it was since it still names the unredacted original; the file then walks and verifies against itself; its IDs are then not the original's and its `request_hash` values are the original's and no longer match. A reader MUST NOT report such a file's hashes as verifying the original |

Unknown header fields MUST be preserved by any tool that rewrites the
file.

## Entry envelope

```json
{"type":"item","id":"a1b2","parent":"9f8e","ts":"2026-09-17T12:00:01Z", …}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | entry type; core types below, or namespaced `ns:type` |
| `id` | MUST | the entry's envelope hash, defined below; unique everywhere, not only in the file |
| `parent` | MUST | hash of the parent entry, or `null` for a root |
| `parents` | MAY | further predecessors this entry converges; provenance only, never walked when building a context |
| `ts` | MUST | RFC 3339 in one form, since it is hashed as a string: UTC, uppercase `T` and `Z`, seconds `00` to `59`, a fractional part only when non-zero, with no trailing zeros and at most nine digits, as in `2026-09-17T12:00:02.5Z`. Mainstream time types hold neither a tenth digit nor a second `60`; a writer truncates the fraction to nine digits and writes a second `60` as `59` with the same fraction. A reader MUST report any other spelling as it reports a hash that fails |

A parent MUST appear earlier in the file than any child. Multiple roots
are permitted in a file with no `base`; a file with one has one prefix
and every own entry descends from the base. An entry MUST NOT be
modified after it is written; corrections are new entries.

### Entry hash

An entry hashes in two layers, as a git commit hashes over its tree
rather than over its files. Canonical throughout means the JSON
Canonicalization Scheme (RFC 8785): members sorted by UTF-16 code units,
as that scheme requires and as code-point order does not give for a name
holding a supplementary character; no insignificant whitespace; numbers
and strings in canonical form. That scheme is defined over I-JSON (RFC
7493), so every hashed member MUST be I-JSON, by a test on the value and
not on its spelling: every number is finite once rounded to binary64,
the rounding being expected and not an error; a number whose exact value
is a whole number MUST either be exactly representable in binary64 or
have the exact value of the canonical rendering of the nearest binary64
value, which rejects 9007199254740993 however it is spelled and accepts
9007199254740992, 1e20, 0.1 and 1152921504606847000; no object repeats a
member name; no string holds a lone surrogate. The second arm exists
because the canonical rendering is ECMAScript's, which below 10^21 pads
the shortest digits that round-trip a double with zeros, so from 2^53 it
writes many representable whole numbers, 2^60 as 1152921504606847000, as
a whole number no double holds; with the arm the canonical form is
always admissible and a canonical rewrite cannot change the result. The
arm admits a literal that a bignum reader and a double reader decode
differently, as RFC 8785 itself emits such literals; the format's
numbers are binary64 values, and the test rejects what a double reader
would silently alter in a spelling that need not have been altered. A
reader checks the whole-number rule mechanically by asking, of a number
whose exact value is a whole number, whether converting that value to
binary64 is exact, and if not, whether rendering the result canonically
gives back that exact value. A reader MUST report a line that fails the
test, as it reports a hash that fails. A model or a provider can emit
what the test rejects — a string cut inside a surrogate pair, a 64-bit
integer in a provider field — and the writing discipline says an output
is recorded before it is acted on, so a writer MUST normalise before it
writes: a lone surrogate becomes U+FFFD; each maximal subpart of an
ill-formed UTF-8 sequence, as Unicode §3.9 defines it, becomes one
U+FFFD, so that writers in every language substitute alike; a whole
number that fails the test is rounded to binary64 and written
canonically, which is what canonicalisation would have done silently and
which yields a value the test accepts, except where the payload profile
types the member as a string, in which case the digits are carried as
that string. The writer records what it changed in a member named
`normalised` on the entry: a JSON array of objects `{"at": …, "was":
…}`, `at` an RFC 6901 JSON Pointer relative to the body and `was` a
string holding the member's original JSON source text, escapes and a
string's quotes included, so a lone surrogate appears as the six ASCII
characters `\ud83d` inside its quotes, `"\ud83d"`, and a 64-bit integer
as its digits, and `was` is I-JSON whatever it describes. Output that
was not valid UTF-8 has no JSON text to record: the writer substitutes
U+FFFD as above, omits `was`, and carries the member's source bytes,
quotes included, in `raw` as base64 under RFC 4648 §4, with padding,
since the URL-safe alphabet and an unpadded form would give one response
two hashes. A string that had no source text, one the writer held
decoded, is recorded as its canonical string serialisation with the
ill-formed bytes kept as they were, so two writers holding the same
bytes record one `raw`. What no rewrite reaches, a number that is not
finite in binary64 or an object that repeats a member name, a writer
MUST NOT write: it refuses the entry, since no normalisation can make
the line pass the test. The array is sorted by `at` as UTF-16 code
units, matching the canonical form, so two writers normalising one
response produce one content hash. The normalised form is what the next
request carries, so the rebuilt request and `request_hash` agree with
what was sent. `ts` is hashed as the string it is, which is why the
envelope table admits one spelling of it.

- The entry's **content** is the object of its members with the
  envelope's — `id`, `type`, `parent`, `parents`, `ts` — removed, and
  its **content hash** is `sha256:` followed by the lowercase
  hexadecimal SHA-256 of the content's canonical bytes. Every member
  outside the envelope is in it, those this document does not define
  included, so that nothing a tool preserves can change unnoticed.
- `id` is `sha256:` followed by the SHA-256 of the canonical bytes of
  the **envelope object**: `type`; `parent`, `null` for a root;
  `parents` only when present and non-empty, and a writer MUST omit an
  empty one; `ts`; and `content` holding the content hash. `content` is
  computed, not written: a line carries the body inline, and a reader
  computes the content hash first and the envelope hash from it. The
  envelope's names — `id`, `type`, `parent`, `parents`, `ts`, `content`
  — are reserved, and a body MUST NOT carry a top-level member by any of
  them. A reader that meets one in a body MUST report the line as it
  reports a hash that fails, since an `id` over a body that collides
  with its own envelope verifies nothing.

Because `parent` is itself a hash, an entry's ID commits to its whole
path, and two files that agree on one ID agree on every byte above it.
Because the envelope names the body by hash, a chain of envelopes
verifies without the bodies, which is what lets a store hold or send
history it does not hold in full, and a body is held once however many
entries name it, which RFC 0002 builds on. Wherever this document has a
member name an entry — `parent`, `parents`, `target`, `first_kept`,
`from`, `queued_from` — it names it by `id`.

Two entries with the same type, content, parent, `parents` and `ts` are
one entry. A writer that means two makes them differ, and sub-second
`ts` is what usually does. A reader that meets an `id` a second time in
one file MUST treat the line as that same entry and report the repeat; a
projection never writes one, since a store's log holds a hash once, but
a hand-written file can.

Preservation is of members, not bytes. A rewriter MAY re-serialise a
line, since both hashes are over canonical forms and verification does
not depend on the bytes a file happens to carry; a projection from a
store writes canonical lines. An `id` is the hash of the line, not of
any reader's model of the entry: a reader MUST compute both hashes from
the line's own members in canonical form, and where a decode would hash
differently, the decode is wrong. A reader that verifies a line from its
canonical bytes alone, decoding nothing, and one that decodes it reach
the same answer, and a rewriter keeps each member as the line held it,
present or absent. `sha256:` is the only prefix, and a reader MUST
refuse an `id` carrying another.

A reader MUST verify each entry's `id` by computing its content hash and
then its envelope hash, and MUST report a line that fails. It is
corruption, not an extension, and a reader MUST NOT repair it. A file
that has been redacted has had its hashes recomputed over the redacted
bodies, as the header's `redacted` row requires, so it walks and
verifies against itself and not against the original; such a file MUST
say so in its header, and a reader MUST NOT report its hashes as
verifying the original.

Appending an entry of either kind under the leaf makes it the leaf; an
append elsewhere is a branch and the leaf does not move. A `leaf` label
makes its target the leaf. A record entry is a child of the leaf like
any other, so it lies on the path of every entry appended after it. The
run and call shapes below depend on that: a writer MUST NOT hang a
record entry off an earlier entry as a sibling.

A member of a core entry that this document does not define MUST be
preserved by any tool that rewrites the file and MUST be ignored by a
reader that does not know it. That is where a harness keeps detail
richer than a core member allows. The rule holds at every depth: a
member this document does not define inside an object it does, such as
a `host` inside `workspace`, is preserved the same way, and like every
member it is hashed as the line holds it. A reader MUST NOT compute an
entry's hashes from its own model of the entry when that model cannot
hold everything the line does.

A record entry named in the header's `records` is written whenever its
event occurs, so a reader MAY take its absence on a path as the event
not having happened. For a type the header does not name, absence means
the file does not say. A converter from a native format that carries no
such record leaves the type out of `records`.

### Convergence

`parent` is the entry's line of descent: exactly one, in this file, and
the only edge a context is built from. `parents` records that the entry
also converges work from elsewhere — the result of a subagent session,
a branch merged back, several workers joined at once.

```json
{"type":"item","id":"j1","parent":"9f8e","ts":"…",
 "parents":[{"entry":"w7"},{"session":"01J…","entry":"c4"}]}
```

- Each reference MUST name an `entry` by hash. `session` names a
  session whose file holds that entry, so a reader without a store
  knows where to look; it MAY be omitted when the entry is in this
  file. The hash is the identity, and it is the same hash wherever the
  entry is held.
- `parents` MUST NOT contain the value of `parent`, and MUST NOT name
  the same entry twice.
- A reference MUST name an entry that already existed when this entry
  was written. With `parent`, that is what makes the structure acyclic:
  every edge points at something older.
- `parents` is **provenance**. It is not walked when building a context,
  and an entry it names contributes nothing to any context by virtue of
  being named. Whatever crossed the boundary is in this entry's own
  payload, materialised, as the ingress rule requires of every entry
  that receives material from outside this session; `parents` only
  records where that material came from.
- Order is not meaningful. A writer MUST sort the references, by
  `session` then `entry`, with references that omit `session` sorting
  before those that carry one, so that a file does not depend on the
  order in which workers happened to finish.
- A reader that does not understand `parents` builds exactly the same
  context as one that does, losing only the provenance.

## Core entry types

Context entries: `item`, `response`, `config`, `compaction`,
`branch_summary`. Record entries: `run`, `dispatch`, `decision`,
`queued`, `label`, `info`, `env`, `outcome`, `link`, `custom`. A record entry
contributes nothing to context; the context algorithm below is the
normative statement.

A type is core only if the event it records belongs to the loop every
harness runs: an input arrives, the settings change, the model is
called, a call is decided and dispatched, its output returns, the
context is compacted, the conversation branches, the run ends; or if it
annotates that record in a way every harness needs: a bookmark, a name,
the environment, a judgement of the result, a link to another session,
state kept beside the conversation. An event that belongs to one
harness's features, however useful, is an extension (`ns:type`) or a
`custom` entry. The same test admits a member of a core entry: it names
an event of that loop, or a shape a reader can recompute from the path.
A harness's own detail goes in a `ref`, in `details`, in a member this
document does not define, or in a `custom` entry. That rule is what
keeps the envelope harness-neutral as the format grows.

### `item`

One conversation item in the payload profile.

```json
{"type":"item","id":"…","parent":"…","ts":"…",
 "item":{"type":"message","role":"user","content":[…]},
 "response":"resp_…","visible":true}
```

- `item` MUST be a valid item for the payload profile. Under the
  Open Responses profile that includes `message`, `function_call`,
  `function_call_output`, `reasoning`, `compaction` and any
  slug-prefixed item the profile allows. It does not include
  `item_reference`, which the profile permits and the ingress rule
  excludes: a reader accepts one, a writer does not produce one.
- `response` MAY carry the `response_id` of the model call this item
  belongs to, a provider's identifier and not an entry's, when the
  item was model output.
- `visible` MAY be `false` to mark an item that is part of the model
  context but that a renderer SHOULD hide.
- `source` MAY carry the trigger of an item a person or another system
  sent, in the shape `queued` defines, and `queued_from` MAY name the
  `queued` entry the item was accepted as.
- A `function_call`'s `call_id` MUST NOT be empty and MUST differ from
  that of every other `function_call` in the session, on any branch: a
  `function_call_output`, a `link`, a `custom` entry, a run end's
  `pending` list and a projection name the call by it alone, and a
  subsession's ID is derived from it. A writer whose provider repeats an
  ID, or gives none, or whose call ID another branch of the session
  already holds, writes one of its own, such as the native ID with a
  suffix, drawn from letters, digits, `_` and `-` so every provider
  takes it, and SHOULD keep the native one in a member of the `item`
  entry this document does not define, beside `item` rather than in it.
  It sends the provider the ID it records, so the request the path
  rebuilds is the request sent; a writer whose provider will not take it
  omits `request_hash` from every `response` while the call is in the
  context. A reader of a file that repeats one takes a
  `function_call_output`, `link` or `custom` entry to name the latest
  `function_call` before it on the path with that `call_id`; a
  `decision` or `dispatch` names its call by `target`, and one whose
  `target` names no `function_call` before it on the path, which this
  document forbids, a reader MAY take to name the latest call with its
  `call_id`, reporting the entry.

### `response`

The envelope of one model call, written after its output items.

```json
{"type":"response","id":"…","parent":"…","ts":"…",
 "response_id":"resp_…","model":"…","status":"completed",
 "usage":{…},"incomplete":null,"error":null,
 "request_hash":"sha256:…","latency_ms":1234,"attempts":2}
```

- Every member but the envelope's is optional. `error` and
  `incomplete` are read as null when absent; the run cascade reads
  those two and never `status`, so a converter over a log without a
  status need not invent one.
- `usage`, `incomplete` and `error` use the payload profile's shapes.
- `request_hash` SHOULD be the hash of the canonical request built by
  the context algorithm below, so a reader can check that the stored
  path rebuilds the request that was sent.
- `attempts` is the number of calls to the model this response took,
  itself included, when the harness retried calls that failed and
  recorded none of them as a `response` of its own: a retry after a
  rate limit or a dropped connection. It is a positive integer, and
  absent means 1. A failed call the file does record as a `response`
  is not counted again. `latency_ms`, when present, is the time the
  harness measured; whether it spans the failed attempts is the
  harness's to say.

### `config`

A delta to request settings. The first entry on any root SHOULD be a
`config` carrying full settings.

```json
{"type":"config","id":"…","parent":"…","ts":"…",
 "model":"…","instructions":"…","reasoning":{…},"text":{…},
 "instructions_parts":[{"keep":2},{"id":"agentsmd","text":"…","source":"agentsmd"},
                       {"id":"memory","hash":"sha256:…"}],
 "instructions_omitted":[{"id":"service/AGENTS.md","reason":"budget","size":4096,"source":"agentsmd"},
                         {"keep":3}],
 "tools_added":[…],"tools_removed":["name"],"extra":{…},"replace":false}
```

Fields absent from a delta are unchanged. `replace: true` discards all
earlier config on the path before applying this one. Tool definitions
use the payload profile's tool shape.

#### Instructions as parts

The instructions a harness sends are composed: a product prompt, the
instruction files that apply at the working directory, a catalogue of
skills, a block of memory. Each changes for its own reasons, and with
one string every change to any of them rewrites all of them into the
path.

`instructions_parts` is that composition: an ordered list of parts,
each with an `id`, its `text`, and optionally a `source` naming the
layer that produced it. `id` is a stable string the harness chooses,
the same across the session, so a later entry can name a part without
repeating it; `product`, `agentsmd`, `agentskill` and `agentmemory`
are the obvious ones. `source` is in the harness's own terms and
readers treat it as opaque.

- `instructions` remains valid, and a writer that composes nothing
  writes it alone. When both are present, `instructions` MUST equal
  the parts' texts joined, in order, with one blank line, the two
  characters `\n\n`. That join rule is the whole agreement between a
  writer and a reader: settings carry the joined string, the request
  carries it, and the `request_hash` covers it.
- A writer MAY write the parts alone and leave `instructions` out; a
  reader then derives the string by the same join. A file whose config
  entries carry parts alone does not rebuild its instructions in a
  reader that does not know `instructions_parts`, which is the cost of
  this member and the reason it arrives in a new minor version.
- A delta carries the **whole ordered list**: every part in force
  after it, in order, and no other. A part whose text changed, or that
  is new, carries its `id` and `text`. A part whose text is unchanged
  carries its `id` and `hash` and no `text`: the SHA-256 of its text in
  the format's notation, `sha256:` and lowercase hexadecimal, and its
  text is the one the path already has for that id. A part the list
  leaves out is removed. A list names each `id` once. Order is
  therefore explicit in every delta. A part with an `id` and neither
  `text` nor `hash` has empty text, which is how a writer that omits
  an empty string writes one.
- A part named by `hash` alone also keeps the `source` it had on the
  path, since the hash form has no way to say that a part has none
  now, so a writer leaves `source` off it. A `source` present on such a
  part, as a 0.6 writer repeated it, is the part's source. A writer
  that clears or changes a part's `source` writes the part's `text`
  with it.
- A run of unchanged parts MAY be named by position instead:
  `{"keep":n}`, an element with no `id` and a positive integer `keep`,
  stands for the
  next n parts in force before the entry, each unchanged, `text` and
  `source` alike. "Next" is counted from a cursor into that list. The
  cursor starts at its first part; an element naming a part in force
  by `id` moves it to just after that part,
  wherever that is; a `keep` moves it past the n parts it takes; an
  element naming an `id` not in force leaves it where it is. A change
  to the 61st of 126 parts is then
  `[{"keep":60},{"id":"m61","text":"…"},{"keep":65}]`, and removing
  the 61st is `[{"keep":60},{"id":"m62","hash":"sha256:…"},{"keep":64}]`.
  An unchanged part costs one id and one hash when it is out of place
  and nothing beyond its run's element when it is not. A writer MUST
  NOT write a `keep` that runs past the end of the list in force, or
  one that takes a part another element of the same list names, and
  MUST NOT write any other member beside `keep`, which a reader
  ignores. A `keep` member on an element that carries an `id` is a
  member this document does not define there. Unlike a `hash`, a
  `keep` names no text, so nothing but the `request_hash` checks it.
- A part named by a `hash` this path cannot resolve, a `keep` that
  runs past the end of the list in force or takes a part the list
  names elsewhere, a `hash` or `keep` naming a part the path itself
  could not rebuild, and an element with neither an `id` nor a
  positive `keep`, have no text a reader can rebuild. When the same entry
  carries `instructions`, that string stands: it is the only record of
  what the model was sent, and a reader takes it over the join of
  parts it cannot resolve. Without it the instructions cannot be
  rebuilt and the `request_hash` will not verify, which is how such a
  file is found.
- A writer MUST NOT write a delta with `replace: true` whose parts are
  named by `hash` alone or by `keep` without `instructions` beside
  them: the replace discards the parts they would have resolved
  against, so nothing on the path can rebuild them.
- `replace: true` discards the parts with the rest of the settings,
  so a `hash` or a `keep` in the same entry resolves against nothing; a delta that
  sets `instructions` as a string and no parts replaces the
  composition, and the parts no longer describe what is in force.

`instructions_omitted` records the parts the writer considered and
left out, each with its `id`, a `reason` in the writer's own terms,
the `size` in bytes it would have added, and optionally a `source`.
It is where a
walk that dropped an instruction file for a budget, or a memory the
render left out, is recorded, so a session says what the model was not
given as well as what it was.

Nothing in the list reaches the request, and the `request_hash` does
not cover it, but it stays in force as settings do, so a writer whose
omissions do not change between turns writes them once:

- The list in force is the one the last `config` entry on the path
  that carries the member wrote: a later entry with the member
  replaces the list, it does not add to it. The member stands for
  every part omitted as of the entry, in order, and no other, though
  it MAY name runs of them by `keep`, below.
- A delta without the member leaves the list as it was. A delta with
  `replace: true` and without the member clears it with the rest of
  the settings, and `[]` clears it explicitly.
- A `config` entry MAY carry the member and no setting, when what was
  left out changed and nothing in force did.
- A `null` member is an absent one: it leaves the list as it was.
- A compaction's `config` checkpoint carries the list in force, below,
  and the list after the compaction starts from it.

A list in force changes by a part or two at a time, as a memory at its
budget does whenever a fact is saved or forgotten, so a delta MAY name
a run of parts that stay omitted, unchanged, by position, as a delta
of instruction parts does:

- `{"keep":n}`, an element with no `id` and a positive integer
  `keep`, stands for the next n parts of the list in force before the
  entry, each unchanged, `reason`, `size` and `source` alike. "Next"
  is counted from a cursor into that list by the rule
  `instructions_parts` uses: the cursor starts at its first part; an
  element naming a part in force by `id` moves it to just after that
  part, wherever that is, the first such part when the list in force
  names the `id` twice; a `keep` moves it n parts on, whether or not
  it resolves; an element naming an `id` not in force leaves it where
  it is. An element that names a part carries all of it, since an
  omitted part has no hash form. A save that pushes one fact out of a
  memory ahead of 474 already omitted is then
  `[{"id":"memory/user/n-0125","reason":"budget","size":20,"source":"agentmemory"},{"keep":474}]`.
- A list with a `keep` MUST name each `id` once, and a writer MUST NOT
  write a `keep` over a list in force that names an `id` twice; it
  writes that list whole. A writer MUST NOT write a `keep` that runs
  past the end of the list in force, or one that takes a part another
  element of the same list names, and MUST NOT write any other member
  beside `keep`, which a reader ignores. A `keep` member on an element
  that carries an `id` is a member this document does not define there.
- A `keep` that runs past the end of the list in force or takes a
  part the list names elsewhere, which a reader finds by reading the
  whole list first, and an element with neither an `id` nor a
  positive integer `keep`, name no part a reader can rebuild. A reader
  keeps such an element in the list in force where it stands, as it
  keeps an instruction part it cannot resolve, and a later `keep` that
  takes it takes it as it is. Unlike a `keep` among the instruction
  parts, nothing checks one here: the list reaches no request, so no
  `request_hash` covers it.
- `replace: true` discards the list in force with the rest of the
  settings, so a `keep` in the same entry resolves against nothing,
  and a writer MUST NOT write one there: a replace writes the list
  whole. A compaction's checkpoint is not a delta, and writes the list
  whole too; an element in force that named no part is written as it
  stands, and a reader of a checkpoint takes each element as it is,
  resolving no `keep` in it.

### `compaction`

Replaces earlier context with a summary.

```json
{"type":"compaction","id":"…","parent":"…","ts":"…",
 "first_kept":"entry-id","summary":{…item…},"pinned":[{…item…}],
 "config":{…full config…},"tokens_before":50000,"usage":{…}}
```

- `first_kept` MUST name an entry on the path. Entries before it are
  excluded from context; entries from it up to this entry are kept.
- `summary` is an item. Under the Open Responses profile a server-side
  compaction stores the returned `compaction` item verbatim; a local
  summary is a `message` item.
- `pinned`, when present, is an ordered list of items the writer kept
  verbatim from before `first_kept`. A reader MUST place them
  immediately after `summary` and before the entries from
  `first_kept`, in the order written; their order among themselves is
  the order the request carried them in, so a reader that reorders
  them rebuilds a different request.
- Only an item carried by an `item` entry may be pinned, and a writer
  MUST make each pinned item reachable as such an entry on the path
  before `first_kept`. `pinned` is a copy of context that was already
  recorded, never a new input, so a reader that ignores the member
  loses context but never invents it. A `summary` or a
  `branch_summary` is in context but is not an `item` entry, so it
  cannot be pinned; a writer that wants an earlier fold's summary to
  survive this one restates it as this entry's `summary`.
- A reader MUST NOT reject a file whose pinned item it cannot find on
  the path. It cannot tell a writer that sent the item and failed to
  record the copy from one that pinned an item it never sent: in the
  first case the rebuilt request is the one that was sent and
  verifies, and in the second the rebuild differs and `request_hash`
  reports it. Rejecting up front would refuse a valid record in order
  to catch an invalid one the hash already catches.
- Only the last `compaction` on a path contributes items, so a later
  compaction that does not restate a pin drops it. A writer that wants
  a pin to survive a second fold MUST repeat it in that fold's
  `pinned`.
- `config` is a full checkpoint so a reader need not replay config
  entries from before the compaction. Its shape is the settings the
  context algorithm produces, not a `config` delta:

  ```json
  {"model":"…","instructions":"…","instructions_parts":[…],
   "instructions_omitted":[…],
   "reasoning":{…},"text":{…},"tools":[…],"extra":{…}}
  ```

  `instructions_parts`, when the checkpoint carries it, is the full
  list of parts in force, each with its text, not a delta, and
  `instructions` is their join. `instructions_omitted` is the list of
  omitted parts in force, every part written out and no `keep`, and a
  checkpoint without it has none in force. `tools` is the full list of tool
  definitions in force at the compaction, in the order the context
  algorithm would send them, not a delta; there are no `tools_added`, `tools_removed` or `replace`
  members. `extra` is the merged map of passthrough request members
  after every earlier delta has been applied and null deletions have
  removed their keys, so it never contains a null value. Members whose
  value is empty MAY be omitted. A writer built elsewhere MUST produce
  this shape so the same path rebuilds the same request and its
  `request_hash` verifies.

### `branch_summary`

Context carried across a branch switch.

```json
{"type":"branch_summary","id":"…","parent":"…","ts":"…",
 "from":"abandoned-leaf-id","summary":{…item…},"usage":{…}}
```

`parent` is where the new branch continues; `from` is the leaf that was
left. `summary` is an item that enters context.

### `run`

Why a run started and how it ended. Two entries per run, paired by
`run_id`, both written by the harness that runs the loop.

```json
{"type":"run","id":"…","parent":"…","ts":"…","run_id":"…","phase":"start",
 "source":"input|resume","ref":"…",
 "trigger":{"kind":"schedule","ref":"nightly","source":"cron"}}
{"type":"run","id":"…","parent":"…","ts":"…","run_id":"…","phase":"end",
 "reason":"done|stopped|interrupted|input_required|aborted|error",
 "ref":"…",
 "pending":["call_…"]}
```

- `run_id` and `phase` are required on both entries. `source` is
  required on `start`; `reason` and `pending` are required on `end`.
  `ref` is optional on both, and `trigger` is optional on `start`.
- `source` is closed to two path shapes. `resume`: the segment's first
  `function_call_output`, `decision` or `dispatch` after `start` takes
  up a call that was on the path with no output when the run began,
  whether the previous run ended by leaving it pending or was cut off. A
  resume that holds such a call again, or rejects it, is one, and a
  message the run adds before or after it, such as the approval a person
  typed, does not change that. The shape is what the writer knows when
  it writes `start`: it started the run to take up a call. `input`:
  otherwise, including a run that answers nothing and adds nothing, such
  as a retry after an error, which `ref` names. `ref` on `start` names
  what triggered the input (a cron name, a channel message ID). How an
  input arrived, whether a schedule, a channel or another agent, is a
  harness feature and goes in `trigger`, `ref` or a `custom` entry.
- `trigger` on `start` says how the input that started the run arrived,
  in the object `queued` defines: `kind`, `ref` and `source`, each
  opaque. It sits beside `ref` and changes nothing about it; `ref` stays
  one opaque string. A writer that holds a trigger's parts SHOULD write
  them here rather than joined into `ref`, since a joined string cannot
  be split back. Anything richer, such as when a scheduled firing was
  due or which attempt at it this is, SHOULD go in members of
  `trigger` this document does not define, as `queued` says, so the
  one object carries the firing from the `queued` entry to the `item`
  that drains it and the `run` it starts. Such facts written in
  members of the `run` entry itself, where 0.6 and 0.7 put them, are
  valid still, and the envelope section says a rewriter preserves
  both.
- `reason` is closed. Each value is a shape of the run's segment, the
  entries on the path from the `start` entry to the `end` entry. A
  run's calls are the `function_call`s on the segment and those on the
  path before it that the segment holds a `decision`, a `dispatch` or
  a `function_call_output` for, since the run took them up; a pending
  call is one of the run's calls with no `function_call_output` on
  the path. Five values are computable, tested in this order with the
  first match winning:
  1. `error`: the last `response` on the segment carries a non-null
     `error`.
  2. `input_required`: at least one pending call is held, as `decision`
     defines it, and no pending call is in flight, as `dispatch`
     defines it.
  3. `aborted`: any other segment with a pending call, or whose last
     `response` has a non-null `incomplete`.
  4. `done`: the segment has a `response` and its last one has no
     `function_call` in its output.
  5. `stopped`: the last response on the path before the segment's end
     has calls, every call on the path has an output, the segment
     holds at least one output or decision, and no response follows.
     The harness chose not to call the model again; `ref` names the
     cause (a turn budget, a tool that asked to stop).
  6. `aborted`: anything left, which is a run that neither called the
     model nor finished a call.

  The `stopped` step reads the path and not the segment alone. A run
  that answers a call and ends without calling the model again holds
  no `response` of its own: a resume whose approved call asks the
  harness to terminate, and a refusal that ends the turn, are both
  that shape, and the `response` that made the calls is on the path
  before the segment. One consequence is deliberate: a call an earlier
  run left without an output keeps every later run on that path from
  reading as `stopped`, because the path still holds an unanswered
  call, and `aborted` is the value that says so.

  A call made before the segment is one of the run's calls once the
  segment holds a decision, dispatch or output for it, and not before.
  A resume that asks its policy about a call an earlier run left, and
  holds it, ends `input_required` before any model call, since the run
  took the held call up. A run that leaves such a call untouched has
  not taken it up and its end does not list it, though the call still
  keeps the run from reading as `stopped`.

  Two values record what the segment cannot show and are written, not
  computed: `error` when the harness failed at any point, which `ref`
  names, and `interrupted` when a person or the host told the harness
  to stop. A written `error` or `interrupted` stands over any segment.
  Every segment matches exactly one computable value on its path; a
  reader MAY recompute it, and when the written value is computable
  and the two disagree the segment is authoritative.
- `pending` lists the pending calls' IDs, those made before the
  segment among them, so a resume can read them without walking the
  segment. The segment is authoritative here too. It lists the run's
  calls only: a call an earlier run left that this run did not take
  up is pending on the path and not in the list, and a resume that
  takes up every call left without an output reads them from the
  path.
- Items and responses of the run follow its `start` entry on the path.
  Runs do not nest: an input that arrives while a writer is running a
  run joins that run. A branch to an entry before a run's `start`
  leaves that run off the new path, so the run needs no `end` there,
  and the next run on the new branch begins with its own `start`.
  When the header names `run` in `records`, no writer holds the file
  open and the leaf is on the run's segment, a run with no `end` entry
  was cut off; that is the crash signal.
- A branch to an entry inside a run, such as a rewind to a checkpoint
  the run made, leaves that run open on the new path, since its `end`,
  if it has one, is on the branch left behind. The two shapes are one
  case: a path on which a run is open that no writer is running. The
  writer that continues such a path owns that run and closes it before
  it appends anything else: it appends the run's `end` at the leaf,
  with `interrupted` and a `ref` naming the rewind when it branched
  into the run, and with `error` and a `ref` naming the cut when it
  resumes a run that was cut off, since a harness that stopped without
  recording why has failed. Both are written values and stand over the
  segment. The record
  then says what happened on that path, and no later reader takes a
  rewind for a crash or a resumed session for one still running.

### `dispatch`

A call was handed to its tool.

```json
{"type":"dispatch","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id","idempotency_key":"…"}
```

`call_id` and `target` are required; `target` is the `item` entry
holding the `function_call`, on the path before the `dispatch`, and
`call_id` MUST be that call's. The `target` names the call. A call with
a `dispatch` and no `function_call_output` on the path may have run: its
side effect may have happened. It is **in flight**, handed to its tool
when the record stopped, unless the latest `decision` after its last
`dispatch` is a `hold` or an `answer`. A `hold` after a `dispatch` holds
a call that may have run, and whoever answers the hold answers that
call, as a replay rule would: a `proceed` and another `dispatch` run it
again, an `answer` ends it without running it. An `answer` with no
output after it is a call the harness answered and has not yet written
the output of, since the record stopped between the two; the harness
that continues the path writes that output and nothing else for the
call. When the header names `dispatch` in `records`, a call with neither
was never started; otherwise the file does not say whether it ran. A
writer that names `dispatch` MUST write it, durably, before the tool
runs, and no writer may write it for a call that was rejected or
answered or that has its output: the output ends the call, and a harness
that runs the tool again for it has made a new call. A `dispatch` with
no `decision` before it on the path means no decision was recorded for
the call, which under the rule in `decision` is the shape of a routine
approval as much as of no decider at all; a writer that records no
decisions produces a valid file. A call cancelled after its `dispatch`
while the harness was running it carries no decision: its
`function_call_output`, or the absence of one, is the record. A call
answered after the record stopped, without being handed to its tool
again, carries an `answer` decision, below.

- `idempotency_key` is optional: the key the harness gave the tool for
  this hand-off, in the harness's own terms, so that a tool that makes
  a second run safe by its key is handed the same key after a crash.
  The record holds it because the key has to outlive the process that
  minted it. A `dispatch` that repeats an earlier hand-off, the same
  operation run again, carries that hand-off's key. A new key says the
  harness treats the run as a new operation, which it does only when a
  decision gave the call new `args` or when it chooses to on purpose;
  a replay rule deciding whether the call may run again reads the key
  of its last `dispatch` beside the arguments that hand-off ran with.
- Each `dispatch` for a call is one hand-off to its tool, in path
  order. A second `dispatch` for the same call means the harness ran it
  again: after a restart, a call that was in flight when the record
  stopped and that its tool says is safe to run twice. It is written as
  durably as the first, so a crash during the re-run is told from the
  first crash, and a reader counts the times the tool may have run by
  counting them. A call with several dispatches and no output was in
  flight at its last one.

### `decision`

A call's fate was decided outside the tool.

```json
{"type":"decision","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id",
 "verdict":"proceed|reject|hold|answer","by":"human|policy|agent",
 "reason":"…","args":{…}}
```

- `verdict` is closed. Each value says what this decision did, and
  the path shows whether it held:
  - `proceed`: this decision let the call go on toward its tool. A
    `dispatch` for the call follows when the call reaches its tool; a
    `reject` after it, an `answer` after it for a call that may have
    run, or a run end with the call pending, says it did not. That is
    an approval something else overtook, such as an abort before the
    call's turn in a serial batch or a refusal by the loop itself, and
    the `proceed` stays as written, with its `by` and its `args`, since
    those are what an auditor asks about such a call.
  - `reject`: this decision ended a call that did not run. No
    `dispatch` and no other `decision` ever follow, and a
    `function_call_output` for the call follows that carries `reason`.
    A writer writes `reject` only for a call with no `dispatch` and no
    output on the path; one that may have run is ended by `answer`. A
    `reject` with no output after it is a call the harness refused and
    has not yet written the output of, since the record stopped between
    the two; the harness that continues the path writes that output and
    nothing else for the call.
  - `hold`: this decision neither let the call go nor ended it. The
    call waits. A call is held while its latest decision is a `hold`
    with no `dispatch` after it on the path; a later decision answers
    it. A call held after a `dispatch` is held and may have run, as
    `dispatch` says.
  - `answer`: this decision ended a call that may already have run,
    with an output the harness wrote rather than one its tool returned.
    It is the answer to a call in flight when the record stopped, to one
    held after its `dispatch`, or to one the file cannot say about, that
    the harness does not hand to its tool again, such as a call whose
    tool cannot say that a second run is safe and whose outcome is
    therefore unknown. No `dispatch` and no other `decision` follow it,
    and a `function_call_output` for the call follows; `by` says who
    answered and `reason` SHOULD say why. A writer writes `answer` only
    for a call with no output on the path, and with a `dispatch` on it
    or, in a file whose header does not name `dispatch` in `records`,
    without one. A call the record shows never started did not run, and
    a writer that ends one without running it writes `reject`. A call
    whose tool ran and whose result the harness then withheld, such as
    one a hook blocked after the tool returned, is ended by `answer`,
    never by `reject`.

  A call may carry several decisions on the path, in order. An answered
  `hold` is followed by a `proceed`, a `dispatch`, a `reject` or an
  `answer` on the same call and stays as written, a `reject` only when
  no `dispatch` came before it and an `answer` only when one did or
  the file does not record dispatches; a call still held is what makes
  a run end `input_required`. The decision that answers a `hold` says
  by its verdict what the answer did. A writer SHOULD write `proceed`
  only when it answers an earlier `hold` or carries `args`, or lets a
  call that may have run go to its tool again; otherwise the
  `dispatch` is the record that the call proceeded. With these, a
  reader tells a call that ran once, one dispatch and its output, from
  one that ran twice, two, and from one answered without running
  again, a `dispatch` and an `answer`.
- `call_id`, `target` and `verdict` are required, and `target` names
  the call as it does on `dispatch`. `reason` is required
  when `verdict` is `reject`, since it is what the model saw as the
  output and what tells a rejected call from a tool failure; otherwise
  `reason`, `by` and `args` are optional. `by` is closed: `human` is a
  person, `policy` is a rule the harness evaluated without waiting,
  `agent` is another model.
- `args`, when present, are the arguments the tool ran with when a
  decision rewrote them. The `function_call` item stays as the model
  produced it, so the request hash still verifies; the change is
  recorded beside the call, never inside it. `args` on an `answer`
  names nothing, since no tool ran with them.

A `decision` is a lifecycle fact and carries no score. A judgement of
how something went is an `outcome`.

### `queued`

An input the harness accepted before it could append it: a steer typed
while the model is running, or a follow-up that waits for the run to
end.

```json
{"type":"queued","id":"…","parent":"…","ts":"…",
 "item":{…item…},"mode":"steer|followup",
 "trigger":{"kind":"human","ref":"slack:1758412800.0002","source":"gateway"},
 "ref":"inbox-1"}
```

- `item` and `mode` are required. `mode` is `steer` for an input that
  joins the run in flight and `followup` for one that waits for it to
  end.
- The entry is a record entry: its `item` is not in context and
  reaches no request. The input enters the conversation when it is
  appended as an `item` entry, which carries `queued_from` naming this
  entry.
- `trigger` says how the input arrived: `kind` is the sort of thing it
  came from, `ref` names the thing itself, `source` names the layer
  that took it. All three are in the harness's own terms and a reader
  treats them as opaque. This is where the trigger of an input that
  joins a run already in flight lives, since the `run` start's
  `trigger` and `ref` name what started the run and not what arrived
  during it: two people steering one run are two triggers.
- Members of `trigger` beyond `kind`, `ref` and `source` are the
  harness's, for what it knows about the arrival beyond those three:
  when a scheduled firing was due and which attempt at it this is, so
  a 03:00 slot that fires at 09:00 behind a busy run keeps its slot.
  A reader keeps them as written, as the envelope section says of any
  member this document does not define, and wherever a `trigger`
  object appears, on a `run` start or as an `item`'s `source`, the
  same holds.
- `ref` names the queued input in the harness's own terms, for a
  caller holding a handle to it.
- A `queued` entry with no `item` entry naming it in `queued_from`,
  and no `run` end after it on the path, is an input the harness still
  owes the conversation: a durable inbox a resume drains. A `run` end
  after it closes it, since the run it was queued into has ended; a
  harness that still wants the input queues it again. A writer that
  names `queued` in the header's `records` writes one for every input
  it accepts before appending it, so a reader may take the absence of
  one as nothing having been queued.

The `item` entry that drains a queued input carries two optional
members: `source`, the trigger the queued entry held, the whole
object with any members of its own, and `queued_from`, the ID of that
entry. `source` on an `item` is not
restricted to a drained input: any item a person or another system
sent rather than the model or the loop may carry it.

### `label`, `info`

```json
{"type":"label","id":"…","parent":"…","ts":"…","target":"entry-id","label":"checkpoint"}
{"type":"info","id":"…","parent":"…","ts":"…","name":"Refactor auth"}
```

Not in context. A `label` with `label: null` clears. The value `leaf`
is reserved: a `label` carrying it marks the branch its `target` is on
as the one a reader resumes on, as the file section says, and a later
`label: null` naming the same target clears it. A `label` carrying
`synthetic: true` was written by a projection to record a store's head,
not by the session; a reader honours it as any other, and a store
importing the file discards it, as RFC 0002 says.

### `env`

A snapshot of the environment for replay: where the tools ran and what
they saw.

```json
{"type":"env","id":"…","parent":"…","ts":"…",
 "cwd":"…","vcs":{"system":"git","revision":"…","dirty":true},
 "files":{"read":{"path":"sha256:…"},"written":{"path":"sha256:…"}},
 "tools":{"name":"version"},
 "workspace":{"kind":"local|container|remote","ref":"…"}}
```

`workspace` says which file system `cwd` is a path in: `kind` is
closed, and `ref` is one string the harness can resolve to that file
system (an image digest, a host, an instance ID). A local run MAY omit
it. A container's `ref` SHOULD be a digest rather than a tag, because a
tag moves. A container on a remote host is `container`, with the
digest as `ref` and the host in a member of `workspace` this document
does not define. Anything else that tells one file system from
another, a container's instance above all, since a restart from the
same image is another file system with the same digest, goes in such
members of `workspace` too, and not beside it in the entry: the
substitution rule below compares `workspace` alone. The envelope
section says a rewriter preserves them. An `env` entry applies from its
position on the path until the next one.

A later `env` entry whose `workspace` differs from the one in force
before it on the path is a **substitution**: from that entry on, the
tools ran against another file system than the path recorded until
then, as when a session recorded in a container is resumed on a laptop.
Two `workspace` members are compared member by member in their
canonical form, as the entry hash writes them, every member this
document does not define included, and an absent one equals only
another absent one, an empty `kind` or `ref` being absent; a new
`cwd`, `vcs` revision or file list in the same workspace is not a
substitution. Recording the
substitution is the point, so a writer writes the entry and nothing
refuses it. A reader that holds the environment fixed, such as a strict
replay or an evaluation comparing runs, treats the path from that entry
on as not verifiable against what came before it, as it treats a call
whose output it cannot reproduce.

### `outcome`

A judgement of how the session, or a range of it, went.

```json
{"type":"outcome","id":"…","parent":"…","ts":"…",
 "kind":"feedback|test|task|tool_error|eval|custom","target":"entry-id",
 "score":1.0,"pass":true,"label":"…","details":{…}}
```

- `target` MUST name an entry on a path in this file, the prefix
  included, usually the last entry of the range judged. Placing it on
  a path is the writer's obligation; a store does not check it. A
  reader that selects branches by outcome resolves `target` as an entry
  on a path; a task or test name belongs in `details`.
- `score` is any finite number. Its scale is the judge's, named by
  `label`; a normalised score belongs beside the raw one in `details`,
  not in place of it. `pass` is the judge's verdict when it has one.
- `kind: "eval"` is a score produced by an evaluation run over the
  session, as opposed to `feedback` from a person or `test` from a
  verifier.

### `link`

A reference to another session, for subagents and forks.

```json
{"type":"link","id":"…","parent":"…","ts":"…",
 "rel":"subsession|fork_of|continued_in","session":"…","call_id":"…"}
```

`call_id` ties a subsession to the function call that spawned it. A
`subsession` link SHOULD be written when the call is dispatched,
before the `dispatch` entry and before the child's header exists, so a
link whose session cannot be found means the child never started
rather than a child that was never linked.

A link names a session, not a point in one, and at the moment it is
written there is no point to name. The other half of the round trip is
`parents`: the entry carrying the child's `function_call_output`
SHOULD name the child's leaf there, which is the first moment the
parent knows it. Without that, a child that branched leaves no record
of which of its leaves the parent actually took the answer from, and a
projection has to guess.

### `custom`

App state that is not in context:
`{"type":"custom","ns":"…","data":…,"call_id":"call_…"}`. App state
that is in context uses a namespaced `item` with the payload profile's
extension mechanism instead.

`call_id` is optional and names the function call the record belongs
to, when the writer knows it: a record a tool writes while it runs, or
one the harness writes about a call. With two calls of one batch in
flight, a record's position on the path does not say which call it
belongs to, and `call_id` does. A reader MAY use it to attribute the
record and MUST NOT require it.

## Namespaced types

Any type of the form `ns:name` is an extension. Readers MUST preserve
extension entries and MUST NOT fail on them. Extension entries are not
in context unless the extension says so, and a reader that does not know
the extension MUST treat them as not in context.

## Ingress

Material reaches a session from places this file cannot see: a subagent
that ran in a session of its own, a branch that was abandoned and
summarised, an item another system produced, a tool whose output came
off a machine that is now gone.

An entry carrying such material MUST carry it **materialised** — the
payload as the model was or will be sent it, in this entry. A reference
saying where it came from is provenance, and provenance is never a
substitute for the material. A reader that cannot resolve the reference,
because the other session was never kept or the system no longer exists,
MUST still rebuild the same context and the same `request_hash`.

This is what every projecting edge in the format already does, and it is
written here once rather than implied in four places:

- `branch_summary` carries the summary item, not a pointer to the branch
  it summarises.
- `compaction` carries its `summary` and its `pinned` items, not a rule
  for recomputing them.
- A subagent's `function_call_output` carries the output; the `link`
  naming the child session contributes nothing to any context.
- `parents` records where converged work came from and is never walked.

The cost is duplication: the same bytes sit in the child's file and in
the parent's. That is the price of a file that answers "what was the
model sent" without resolving anything, and it is the trade the format
already makes for compaction. It is what the **Lossless** goal asks for,
stated as a rule a writer can be held to.

Two things sit outside the rule, because neither is material from
elsewhere. Media referenced by an item MAY be a sidecar, as the file
section says; a sidecar is part of the session rather than outside it.
An `instructions_parts` entry named by `hash` alone, or taken by a
`keep`, resolves against the parts already on this path, which is the
same file, and so does an omitted part taken by a `keep`.

One thing the payload profile permits, this format does not. An `item`
entry MUST NOT carry an `item_reference`. It names an item in the
provider's store instead of holding one, so the path records that an
item was named and not what it said, and the session stops answering
"what was the model sent" the moment the provider expires it.

This is not a new position, it is an existing one applied where it was
being dodged: the canonical request is already built with
`store: false` and no `previous_response_id`, so the format has
declined to lean on the provider's store. An item reference is the same
dependency arriving through the payload rather than through the
request. Worth stating rather than leaving implied, because the failure
is silent — `request_hash` verifies, since the reference is what was
sent, so the file checks out and is hollow.

A reader MUST still accept one. The profile allows it, files carrying
one exist, and such a file is incomplete rather than malformed; a
reader that refused would lose the parts of it that are intact. This is
a rule for writers, which is where the choice is made.

## Context building

Given a leaf, a reader MUST produce the request settings and the item
list as follows.

1. Walk `parent` links from the leaf to a root; reverse to root-first.
   `parents` is provenance and MUST NOT be walked.
2. Replay `config` entries along the path in order to produce settings,
   honouring `replace`. A `config` that carries `instructions_parts`
   resolves them against the parts in force, as that member defines,
   and the settings' instructions are the resolved parts joined with
   one blank line. The list of omitted parts in force replays beside
   them, as `instructions_omitted` defines, each `keep` in it resolved
   against the list in force before its entry, and reaches no request.
3. Find the last `compaction` on the path, if any. If found:
   settings start from its `config` checkpoint and then replay any
   `config` after it; the item list starts with its `summary`, then its
   `pinned` items in order, then the items of entries from `first_kept`
   up to but excluding the compaction, then the items of entries after
   it.
   If none: the item list is the items of all entries on the path.
4. An entry contributes an item if it is `item`, or `branch_summary`,
   or a `compaction` selected in step 3, which contributes its
   `summary` and each of its `pinned` items. Every record entry (`run`,
   `dispatch`, `decision`, `queued`, `label`, `info`, `env`,
   `outcome`, `link`, `custom`) and every unknown extension
   contributes nothing. A `queued` entry holds an item and is not in
   context: the input enters when it is appended as an `item`.
5. The canonical request is settings plus the item list, encoded as the
   payload profile's request with `store: false` and no
   `previous_response_id`. Its hash is `request_hash`.

### Request context of a response

A reader that checks a `request_hash`, or replays a model call, needs
the context of the request that produced a `response` entry: the path
to that entry with the response's own output items removed.

The output items are found by walking back from the `response` entry.
An entry that is not an `item` is skipped. An `item` whose `response`
names this response is one of its output items. The walk stops at the
first `item` whose `response` names another response or nothing. Only
those item entries are removed; every other entry on the path stays,
and the context algorithm above runs over the result.

A `response` that carries no `response_id`, which the entry permits,
has no output items: there is nothing for an `item` to name. A reader
MUST present a response's output items in path order, which is the
order the model produced them in. The rule above is most easily
implemented by walking backward, and such a reader has to restore the
order before it serves them: a request rebuilt from them reversed is a
different request, and the hash reports it as a divergence with no
field to point at.

A writer SHOULD write a response's output items contiguously, so that
the envelope reads in the order it happened and a reader scanning the
file by eye sees one response as one block.

A reader MUST NOT rely on that. Output items are identified by
`response`, never by position: a reader MUST skip an entry that is not
an `item` rather than take it as the end of the output. An entry that
contributes nothing to context changes neither the rebuilt request nor
its hash wherever it falls, so a file that interleaves is valid and
verifies, and a reader that fails such a file is reporting a
divergence that did not happen.

Writers do interleave, and the reason is timing, not carelessness: a
guard that runs inside a response records its verdict when it runs,
and holding the entry until the response closes would move the verdict
away from the item it judged. A writer that can keep the block intact
without losing that ordering should; one that cannot is still writing
a valid file.

### Request hash

`request_hash` is defined here so that a writer and a reader built
separately agree without sharing code.

- The input is the request object of step 5 as it was sent, including
  any passthrough keys the payload profile allows, because those reached
  the model.
- The object is serialised with the JSON Canonicalization Scheme (RFC
  8785): members sorted by UTF-16 code units, no insignificant
  whitespace, numbers and strings in their canonical forms.
- The value is `sha256:` followed by the lowercase hexadecimal SHA-256
  of the canonical bytes.

A writer that records `request_hash` MUST compute it this way. A reader
MAY verify it by rebuilding the request from the path and comparing.

`request_hash` and an entry's hashes have different jobs. The entry
hash identifies a record, `ts` included, so a replay never collides
with the original; the content hash identifies a body, so a store holds
it once; the request hash identifies what the model was sent, `ts`
excluded, so a replay that sent the same request matches. A verifier
uses them all.

`request_hash` identifies a request; it is not a token-exact prefix. It
hashes the canonical request JSON, which sits a layer above whatever a
provider's chat template, tool-schema serialisation and tokenizer make
of it. Two equal hashes say the same request was sent. They say the
model saw the same leading tokens only if all three of those are
deterministic, which this document cannot promise on a provider's
behalf. Verify a record with it, or key a cache on it; what it cannot do
is predict that the provider's cache will hit.

## Writing discipline

- Output items MUST be written only when complete. Partial streaming
  state MUST NOT be written as an `item`.
- A `response` entry MUST follow the items it envelopes and MUST be the
  last entry written for that model call. An entry another layer raises
  while a model call is in flight — a guard's verdict, a dispatch, a
  run boundary — is not an entry "for that model call" and the rule
  above does not forbid it.
- A user item or function call output SHOULD be written before the
  request that includes it is sent.
- Writers SHOULD fsync at least on each `response` entry and on each
  `function_call_output` item, since the output is the record that a
  side effect happened.
- A writer that names a type in the header's `records` MUST have each
  such entry durable before the side effect it precedes: a `dispatch`
  is written and synced before the tool runs, and a `run` end before
  the harness reports the run as ended. Without that, the absence a
  reader relies on could be a lost line.
- Content the harness injects that varies from one run to the next — a
  wall-clock timestamp, a session or request ID, a nondeterministic
  ordering — SHOULD go in an `env` entry or a record entry rather than
  into `instructions` or any other context entry. Put in the request,
  it changes `request_hash` on every run and moves the leading tokens
  under everything after it, so it costs the whole prefix a provider
  had cached to say something about the run rather than to the model.
  A value the model is genuinely meant to act on is not this: that one
  is in the request on purpose, and pays its cost knowingly.

## Projections

### ATIF

One ATIF document per root-to-leaf path. `session_id` is the header ID;
`trajectory_id` is the leaf entry ID. The mapping is given in the
accompanying session plan and is normative for the Open Responses
profile: user and system items to steps, one `response` with its items
to one agent step with `tool_calls`, `reasoning_content` and `metrics`,
its `llm_call_count` the response's `attempts`, 1 when absent,
function call outputs to observations by `source_call_id`, compaction
and branch summaries as copied-context system steps, `link` entries to
`subagent_trajectories`. Where the output entry carries `parents`, the
reference into the child session names the leaf the answer was taken
from; where it does not, the projection chooses one, and the document
then depends on that choice rather than on the record. `run`,
`dispatch`, `decision` and `queued` entries have no step of their own.
A run's `source`, its start `ref` as `trigger`, its end `reason` and
its end `ref` as `cause` travel under `run` in the `extra` of the first
step its segment produces, or in the trajectory's top-level `extra`
when it produces none. `run` is
a **list** in either place, in the runs' own order: a run that
produces no step, which is what a refusal on resume is, would
otherwise be replaced by the next run's record, and a reader needs a
rule for which record is which when several land in one place. A
call's decisions and its first dispatch travel under `calls`, keyed by
call ID, in the `extra` of the agent step that produced the call, and
a call handed to its tool more than once carries every dispatch there
too, in path order, as the list `dispatches`, so the count survives
the projection; a queued
input travels as a list under `queued`, and the step of the item that
drained one carries its trigger under `source`; a fold's usage
travels under `usage` in its system step's `extra`, and a fold's
`pinned` items travel beside its `summary` in the same step, since the
entries they were copied from are before `first_kept` and so are not
in the document. Raw items travel in step `extra` so the projection is
lossless.

A document's steps are the context the algorithm produces, so a run
that compacted is described by its last summary and what followed it.
Its `final_metrics` are not: they total every model call on the path,
the ones a fold replaced included, and `total_steps` counts the
document's steps plus the model calls it does not show. When the two
differ the root `notes` says so, which is what ATIF requires of a
`total_steps` that is not the number of steps. Without that rule a
cost column reads the tail's cost as the run's, and an agent that
folded eleven times outranks one that did not.

One document per path is a projection of the whole session, so a
trajectory may be built at any entry and not only at a leaf: anything
appended after an export, a judge's `outcome` above all, moves the
leaf, and the document a score names has to be reproducible from the
entry it targets.

### OpenTelemetry

Spans emitted for a session carry `session.id` and the entry ID of the
entry they correspond to. Tool spans link to the inference span whose
output contained the call; each inference span links to the previous
turn's; the first span after a branch links to the branched-from entry.
A tool span covers one hand-off, so a call with several `dispatch`
entries has a span for each, and each after the first links to the one
before it.

## Versioning

`format` is `agentsession/<major>.<minor>`. A minor version adds entry types or
optional fields. A major version changes the envelope, the header, or
the context algorithm. Readers MUST accept any minor version of a major
they support. A reader migrates a file in memory and never rewrites it.

A writer that appends to a file whose header names an earlier minor than
the one it writes MUST first raise the header's `format` to its own,
since what it appends may use what its minor adds. A 0.x reader of the
earlier minor, which refuses a 0.x minor it does not name, then refuses
the file as a later one, which tells its operator what to do, rather
than misreading the entries it cannot represent. The raise MUST NOT
leave the file without a whole header at any point a crash could stop
it, so it is written as a new file renamed over the old, or overwritten
in place only at the same length. Nothing hashed moves: the header is
not an entry. A file never lowers its `format`. Appending to a file
earlier than 0.5 is out of scope: no writer of such files remains, a
reader migrates one only to read it, and a raised header would have
its entries read as hashed.

A minor's rules do not change once a writer of it is released, in 0.x
as after it: what a file of that minor may hold, and how a reader reads
it, stay as they were, and a change to either is a new minor. A writer
that appends raises the header to its own minor, as above, so a 0.x
writer of the earlier minor, which refuses a minor it does not name,
then refuses the file rather than append what the later one forbids,
and a reader can tell which rules a file was written under from its
header. A minor cut only to mark that an earlier one's rules were
amended in place after release, as 0.10 is for 0.9, changes no rule.

Adding an optional member to the envelope is a minor change. Changing
what an existing member means, or what the context algorithm does with
any member, is major — which is the line an addition has to stay behind
to arrive in a minor version at all.

The 0.x series is exempt from that rule until the first release. A 0.x
minor MAY change the envelope, the header or the context algorithm, and
a reader of 0.x supports the minors it names rather than every minor of
the major. The guarantee that a reader of a major reads every minor of
it begins at 1.0. A reader of 0.10 reads a 0.5 to 0.9 file
as it stands, since 0.6 to 0.9 add optional members and elements, 0.10
changes no rule, and the hashes do not change, save for the few rules each minor's changes below say read
an earlier file differently; a member a later minor defines that an earlier file holds
in another form, which it was free to while the name was undefined, is
a member the reader does not know, and is preserved as one. A reader
of 0.10 MUST read a 0.x file earlier than 0.5 by migrating it in memory: walk the entries in file order, rewrite
each `ts` to the one form the envelope table requires, converting a non-UTC
offset to UTC with the instant unchanged and, as a writer does,
truncating a fraction to nine digits and writing a second `60` as `59`,
compute each entry's hashes with its `parent` and every entry-naming
member rewritten to the hashes already assigned to entries earlier in
the file, an `item`'s `response` not among them since it carries a
provider's `response_id`, and read the result as a 0.5 file with no
`base`. Each migrated entry carries `legacy_id`, the ID it had, as a
member outside the envelope, added before the hashes are computed so
that the migrated file verifies by construction and two readers give one
file the same IDs; the ATIF and OpenTelemetry projections already
emitted from the earlier file then still resolve, and a projection MAY
emit `legacy_id` beside the new ID. The member is part of the content,
so a migrated body never hashes as the same body written natively does,
and a migrated file shares nothing with one; that is the price of
keeping the old name. A reference the reader cannot rewrite — a
`parents` entry in another session, or an entry named inside a member of
an extension the reader does not know — keeps its original string and is
reported as unresolved, and a file holding one MUST NOT be re-emitted as
0.5 or later. An earlier entry whose body carries a top-level member by one of
the envelope's reserved names, which earlier versions allowed, is
reported as unresolved the same way, and a file holding one MUST NOT be
re-emitted as 0.5 or later; `content` is the name this will most often be. No two
migrated entries hash alike, since `legacy_id` was unique in the earlier
file, so migration never merges.

## Conformance

A conforming **writer** produces files that satisfy every MUST in this
document. A conforming **reader** implements the context algorithm and
the preservation rules. A conforming **converter** from a native format
documents which native entries it maps and which it drops.

The reference implementation is the Go `agentsession` library, which
writes this draft. The conformance suite is a directory of fixture files
with expected context output for every leaf, expected `request_hash`
values, every entry's content hash and `id` recomputed, 9007199254740993
in three spellings and once in a member the format types as a number, a
lone surrogate normalised with its `was`, output that was not valid
UTF-8 replaced with its `raw`, 2^60 written as its canonical rendering
1152921504606847000 and read back, a forked fixture whose `base` is
found in its origin, an instructions delta naming runs of parts by
`keep` beside the parts it rebuilds, a `response` with `attempts`, an
`env` whose `workspace` holds a host and an instance and a later one
that substitutes another instance, a call dispatched twice under one
`idempotency_key` beside a call answered after a crash, a list of
omitted parts in force across a delta, carried by a compaction's
checkpoint and cleared by `[]`, a queued `trigger` with members of its
own drained into an `item`'s `source`, a list of omitted parts naming
runs of the list in force by `keep` across deltas that move a part
across a budget, beside a `replace` and a compaction's checkpoint that
write it whole, the recomputed `reason` for every `run` end, and
negative cases, each a file broken in one way, for a broken parent link,
a truncated last line, an unknown type, a `dispatch` that follows a
`reject`, an `answer` or the call's output, a decision that follows a
`reject`, a `reject` that follows a `dispatch` or an output, a `target`
naming another call, a repeated `call_id`, an empty `call_id`, a `run`
start whose `source` is not the shape of its segment, and a header
naming `dispatch` beside a call that has an output and no `dispatch`;
and a fork naming `dispatch` whose prefix, from a session that promised
nothing, holds such a call, and which verifies.
Converters for pi, Claude Code and Codex are part of the initial
proposal so the format arrives with three existing corpora behind it.

## Prior art

| | store | tree | lifecycle entries | payload | spec |
|---|---|---|---|---|---|
| pi | JSONL | yes | compaction, branch, config, custom | pi messages | docs only |
| Claude Code | JSONL | yes | some | Anthropic content blocks | none, changes per release |
| Codex | JSONL | no | turn context, events | Responses items | none |
| OpenCode | JSON | no | few | own | none |
| ATIF | JSON | no | copied-context flag | own steps | RFC, versioned |
| Letta trajectory | records | no | dropped | own | JSON schema |

This RFC takes pi's tree and lifecycle model, Codex's choice of the wire
item as payload, ATIF's discipline about copied context and
versioning, and adds the entries that none of them record: runs,
dispatches and decisions, environment, outcome and cross-session links.

## Changes since 0.9

None to what a file may hold or how a reader reads it: a 0.9 file is a
0.10 file. 0.10 exists because 0.9's rules were amended in place after
writers of it were released, so a 0.9 header does not say which of
them a file was written under, and a writer of an early 0.9 appended
what later ones forbid without complaint. A writer of 0.10 raises a
0.9 file's header before it appends, so every reader of 0.9, which
refuses a minor it does not name, refuses the file from then on, an
early 0.9 writer among them, and every entry appended after the raise
was written under the rules this draft states. Readers of 0.9 are
upgraded before writers of 0.10 reach the files they read. Versioning
gains the rule that a released minor's rules do not change.

A file raised from 0.9 may still hold entries an early 0.9 writer
appended before the raise; a reader that finds one of the rules below
broken in such a file, which it cannot tell from a 0.10 file, reads it
as broken.

## Changes since 0.8

One element and a paragraph in the `config` section, a writer's rule in
Versioning, which calls are a run's, and what may follow a `reject`. No
hash changes, and a 0.8 file is a 0.9 file with no `keep` in its omitted
lists, save for the run, `reject`, `target` and `call_id` rules below.
An element of an omitted list with no `id`, which a 0.9 reader reads as
a `keep`, was not a 0.8 omitted part, which always had its `id`. A 0.8
run end whose run took up a call made before its segment and left it
without an output may carry a `reason` or `pending` list that 0.9
recomputes differently, and a 0.8 file may hold a decision after a
`reject`, a `reject` after a `dispatch` or an output, a `dispatch` after
an output, a `target` that names another call, or a repeated or empty
`call_id`, which 0.9 forbids.

These rules were added to 0.9 after the reference library first wrote
it, in the same draft, while 0.9 had been public for less than a day.
A 0.9 file an earlier writer of 0.9 produced, or a file such a
writer appended to and raised to 0.9, may break them, and nothing in
the file says which rules it was written under; a reader that finds
one broken in a 0.9 file SHOULD say it may come from such a writer
rather than call the file corrupt.

- A writer that appends to a file whose header names an earlier minor
  raises the header's `format` first. Under 0.8 a header kept the
  minor the file was created under, so a reader of that minor read
  entries a later writer had appended by its own rules.
- `instructions_omitted` takes the `{"keep":n}` element
  `instructions_parts` took in 0.7, counted by the same cursor over
  the list in force: naming a part moves the cursor past it, and a
  `keep` takes the next n parts in force unchanged. A `replace` and a
  compaction's checkpoint write the list whole. Under 0.8 a list that
  changed was written whole, and under a memory at its budget it
  changes on every save of a new fact and every forget: one part
  moving across the budget rewrote a list of 475 parts, 37 KB and 12%
  more than the joined string the parts were adopted to replace. With
  a `keep` it costs about 100 bytes.
- A run's pending calls include the calls of earlier runs it took up
  by a decision, dispatch or output on its segment. Under 0.8 a
  pending call was a `function_call` on the segment, so a resume that
  held a call an earlier run left, before any model call, had no
  pending call and read `aborted`, and its `pending` list could not
  name the call it was waiting on. `resume` follows: a run whose first
  output, decision or dispatch takes up such a call is one, messages
  before it aside, where 0.8 asked for its output at the start of the
  segment.
- A `reject` with no output after it is a call owed its refusal and
  nothing else, as an `answer` with no output is owed its answer, and
  no decision follows a `reject`, as none follows an `answer`. A
  `reject` is for a call with no `dispatch`; one that may have run is
  ended by `answer`. 0.8 said no `dispatch` follows a `reject` but not
  what a reader makes of a record that stopped between the two, and a
  `hold` after a `reject` read as a call waiting on an answer.
- A `call_id` names one `function_call` in a session, on any branch,
  and is never empty. 0.8 did not say, and a provider that repeats
  IDs, or a converter that numbers calls per turn, left a later call's
  output matched to an earlier call, and two branches' subsessions
  derived one ID. A writer that makes up its own IDs sends them to the
  provider, or omits `request_hash`. A reader of such a file matches an
  output, `link` or `custom` entry to the latest call with the ID.
- A `decision` or `dispatch` names its call by `target`, and its
  `call_id` MUST be that call's. 0.8 required both and said nothing
  of a pair that disagreed.
- No `dispatch` follows a call's output. 0.8 did not say; a harness
  that runs a tool again after its output has made a new call, with a
  new `function_call` and `call_id`.

## Changes since 0.7

Additive but for two rules. One optional member, one verdict, and
paragraphs in the `config`, `dispatch`, `decision`, `run` and `queued`
sections. No hash changes, and a 0.7 file is a 0.8 file with none of
the new members, with two exceptions. First, `instructions_omitted`,
which 0.7 said applied to the entry that carried it, is in force in
0.8 until a later `config` changes it, so a 0.7 file whose writer
relied on its absence to say that nothing was left out reads the last
list it wrote as still in force, and a 0.7 compaction, whose
checkpoint has no list, clears it. Nothing in the list reaches a
request, so no context and no `request_hash` changes with it, and the
reference library already read the last non-empty list among a
context's entries as the one in force. Second, a run whose pending
call is held after its `dispatch` ends `input_required`. 0.7 held such
a call too, but its `input_required` step asked that no pending call
have a `dispatch` at all, so that segment read `aborted`; in 0.8 the
step asks that none be in flight, and a held call is not. The call
still reads as one that may have run, and a 0.7 `run` end that wrote
`aborted` for that segment no longer agrees with the `reason` a 0.8
reader recomputes.

- `instructions_omitted` stays in force as settings do: a delta
  without it leaves it, `[]` or a `replace` without it clears it, and
  a compaction's checkpoint carries it. Under 0.7 a writer with a
  memory larger than its budget repeated every omitted part on every
  delta, and 474 of them cost 37 KB a turn, more than the joined
  string the parts were meant to beat. A `null` member is absent.
- A `dispatch` carries `idempotency_key`, the key the harness gave the
  tool, so a tool that makes a second run safe by its key gets the same
  one after a crash; a key that lives only in memory does not survive
  the crash it is for. A hand-off that repeats another carries its key,
  and a new key is a new operation.
- `dispatch` defines a call in flight: one with a `dispatch` and no
  output whose latest decision after its last `dispatch` is not a
  `hold` or an `answer`. A call held after a
  `dispatch` is held and may have run, and one answered with no output
  yet is owed its output.
- Each `dispatch` for a call is one hand-off to its tool, and a second
  one means the call ran again. 0.7 accepted a second and said nothing
  of it, and readers kept one. The ATIF projection lists every one, and
  a tool span covers one hand-off.
- A `decision` with the verdict `answer` ends a call that may already
  have run with an output the harness wrote rather than one its tool
  returned, and says who gave it and why. 0.7 had no verdict for it:
  `proceed` says the call went on toward its tool, which it did not,
  and `reject` says no `dispatch` came before, which is false.
- Members of a `trigger` beyond `kind`, `ref` and `source` are the
  harness's, kept as written wherever the object appears, so a queued
  firing keeps when it was due and which attempt it is. 0.6 put those
  facts in members of the `run` entry, which a queued input has no
  place for; they are valid there still.

## Changes since 0.6

Additive but for one rule. Two optional members, and paragraphs in
the instruction parts and `env` sections. No hash changes, and a 0.6
file is a 0.7 file with none of the new members, with one exception: a
part with an `id` and neither `text` nor `hash`, which 0.6 said had no
text a reader could rebuild, is empty text in 0.7, because that is how
a writer that omits an empty string, the reference library among
them, wrote an empty part under 0.6. An element of a delta with no
`id`, which a 0.7 reader may read as a `keep`, was not a 0.6 part.

- A `response` carries `attempts`, the calls the model took to produce
  it when the failed ones were retried without an entry of their own,
  and the ATIF projection writes it as the step's `llm_call_count`.
  Before it a reader could not tell a flaky provider from a slow one.
- An instructions delta names a run of unchanged parts as `{"keep":n}`,
  by position in the list in force, so a change to one part of a
  composition of many small parts, a memory of a few hundred facts,
  costs that part and not an id and a hash for every other. A part
  named by `hash` leaves its `source` off, as it already kept the one
  it had. The section also says that a part with an `id` and neither
  `text` nor `hash` has empty text, as above, that a `source` on a part
  named by `hash` is its source, and that a `hash` naming a part the
  path could not rebuild does not rebuild it either.
- `env` says the members that tell one file system from another, a
  host or an instance, go inside `workspace`, so the substitution rule,
  which compares `workspace` alone, covers them; the 0.6 text left
  their place unstated.

## Changes since 0.5

Additive. Three optional members and three paragraphs, and nothing a
0.5 file holds changes meaning or hash, so a 0.5 file is a 0.6 file
with none of the new members.

- A `run` start carries `trigger`, the object `queued` already defined,
  beside `ref`, so a scheduled or channel-driven run can say how it
  arrived in parts rather than one joined string.
- A `custom` entry carries `call_id` naming the call a record belongs
  to, as `link` does, since a record's position on the path cannot say
  which of a batch's calls it belongs to.
- A `proceed` no longer promises a `dispatch`: an approval something
  else overtook is followed by a `reject` or by nothing, and stays as
  written.
- `env` says that a later entry with a different `workspace` is a
  substitution, and what a reader holding the environment fixed does
  with it.
- The preservation rule says it holds inside the objects this document
  defines as well as at an entry's top level, and that a member is
  hashed as the line holds it.
- `run` says who closes a run left open on a path no writer is
  running: the writer that continues the path, before anything else,
  with `interrupted` after a rewind into the run and `error` after a
  crash. The old sentence that a branch closes the open
  run held only for a branch to before the run's `start`.

## Changes since 0.4

The envelope changed, which after 1.0 would make this a major version;
the 0.x series is exempt, as the versioning section now says. An entry's
`id` is the hash of its envelope over the hash of its body and `parent`
names a parent by hash, so a file verifies line by line, a leaf commits
to its path, and a chain of envelopes verifies without its bodies. A
migrated entry keeps its old ID in `legacy_id`. The header names a
`base`, the entry in another session this one continues from, and a file
with one opens with the path to it.

RFC 0002 arrives beside this version and takes three things off it. The
durable leaf marker stops being a resume mechanism: a store's head is,
and the marker survives as the projection's way of saying where the
head was when the head is not the last line. The ordering open
question closes: a store's log is the order, always. And the fork rule
this version would otherwise have needed, a root's `parents` naming the
point another session was copied from together with every rule that
let a reader tell copy from original inside one file, is not written,
because `base` says it in one member and the hash proves it in one
comparison.

A retry of a subsession's call is its own session, derived with an
attempt counter, where 0.4 appended a second root to the existing
child; a session with a base has one prefix, so a second root has no
place in it.

`ts` has one spelling, since it is hashed as a string, and every hashed
member is I-JSON. Nothing changes in the context algorithm, in
convergence or in ingress. Among the entry types, `label` gains the
reserved `leaf` value and the `synthetic` member, an `outcome`'s
`target` may name a prefix entry, and an `item`'s `response` is said to
be what it always carried, the provider's `response_id`, and not an
entry's ID. A 0.4 file migrates in memory as the versioning section
says.

## Changes since 0.3

Additive, with nothing tightened. `parents` on the entry envelope
records convergence — a subagent's result, a branch merged back,
several workers joined at once — as provenance.

The context algorithm is untouched, deliberately. `parents` is never
walked, so a 0.3 reader given a 0.4 file walks the same `parent` chain,
replays the same entries and rebuilds the same request, byte for byte.
It cannot report the provenance, but it does not drop it either: a
member a reader does not know is preserved by any tool that rewrites
the file, so `parents` survives a round trip through 0.3. That is what
keeps this a minor version rather than a major one, and it is the
constraint the member was designed against rather than a property it
happened to have.

A 0.3 file is a 0.4 file with no `parents` anywhere.

**Ingress** says that an entry carrying material from outside this
session carries it materialised, and that a reference to its origin
never stands in for it. Every projecting edge in 0.3 already worked
that way — `branch_summary`, `compaction`, a subagent's
`function_call_output` — so mostly this gives a property a name and a
MUST instead of leaving it four coincidences.

The **writing discipline** gains the matching rule for the other
direction: per-run content the harness injects belongs in `env` or a
record entry, not in the request, where it would rewrite `request_hash`
every run and cost the prefix a provider had cached.

One rule is tightened, as 0.3 tightened one before it. A writer MUST
NOT put an `item_reference` in an `item` entry: it names an item in the
provider's store rather than carrying it, which is the one way a file
could satisfy every other rule here and still not say what the model
was sent. The format had already declined that dependency through the
request, with `store: false` and no `previous_response_id`; this closes
the same door on the payload side. A reader still accepts one, so no
file becomes unreadable — what changes is that a writer may no longer
produce one, and the reference implementation now refuses to.

The per-run rule is a SHOULD: a 0.3 file with a timestamp in its
instructions is a valid 0.4 file, one whose prefix never hit.

## Changes since 0.2

Additive but for one tightened rule and two members a 0.2 reader
cannot resolve. The request context of a response skips entries that
are not items rather than stopping at them, so a file whose output
items are interleaved with record entries now rebuilds the request
that was sent. `instructions_parts` and `pinned` are where a 0.2
reader loses something: it rebuilds the same instructions for a file
that writes the string and none at all for one that writes parts
alone, and it rebuilds a compaction's context without the pinned
items. Neither loss invents anything — a short request fails
`request_hash` loudly rather than passing as a request that was never
sent — and there is no installed base for `pinned`, since no file in
existence carries it. Every other addition is a new entry type or an
optional member, which a 0.2 reader preserves and ignores.

- Context building states how the request context of a response is
  found, and that a reader identifies a response's output items by
  `response` rather than by position; a writer still SHOULD keep them
  contiguous.
- `compaction` gains `pinned`, an ordered list of items the writer
  kept verbatim from before `first_kept`, which a reader places
  immediately after `summary`. It is how a harness that holds one item
  out of a fold records what it held, so the rebuilt request is the
  one that was sent and its `request_hash` verifies. Each pinned item
  is also an `item` entry on the path, so a 0.2 reader that ignores
  the member rebuilds a request short of those items rather than one
  that invents them.
- The `stopped` step of the run end cascade reads the path before the
  segment, so a run that answers a call and ends without calling the
  model again is `stopped` rather than `aborted`. The cascade's
  `aborted` step no longer catches a segment with no `response`;
  a sixth step does, so every segment still matches exactly one value.
- `config` gains `instructions_parts`, the composition of the
  instructions as an ordered list of named parts, with a delta
  carrying the text of the parts that changed and a hash for the
  parts that did not, and `instructions_omitted`, the parts the
  writer considered and left out. `instructions` remains valid and is
  the parts' texts joined with one blank line. The compaction
  checkpoint carries the parts in force.
- New record entry `queued`, the input a harness accepted before it
  could append it, with the trigger that brought it in; the `item`
  entry that drains one carries `source` and `queued_from`. A queued
  entry with neither an item that names it nor a run end after it is
  an inbox a resume drains. `records` may name `queued`.
- The ATIF projection: `extra.run` is a list, so a run that produces
  no step keeps its record; `final_metrics` totals the whole path and
  `total_steps` counts the model calls a fold left out of the steps,
  with a line in `notes`; a document may be built at any entry, not
  only at a leaf.

## Changes since 0.1

Additive but for two tightened rules: a reader MUST NOT order entries
by `ts`, and `outcome.target` MUST be an entry ID, so a 0.1 file whose
target held a task name needs that name moved to `details`. A 0.1
reader preserves every new entry and rebuilds the same context.

- The Summary names the two kinds of entry, and the core types section
  states the test for admitting a core type and a core member.
- New record entries `run`, `dispatch` and `decision`, which make the
  Resumable goal true. A run's `source`, its end `reason` and a
  decision's `verdict` are defined as shapes of the path a reader can
  recompute, not as one harness's vocabulary; the reasons form a
  first-match cascade so every segment has exactly one computable
  value, with `error` for a harness failure and `interrupted` for a
  stop a person or the host asked for as the two a writer adds. How an
  input
  arrived and who decided a call are optional or belong in `ref` and
  `custom`.
- `outcome`: `target` is an entry ID by rule, `pass` added, `score`
  unbounded, `eval` kind.
- `env`: `workspace` member, a `kind` and one `ref`; `cwd` precedence
  over the header.
- Header: `records`, the record types whose absence a reader may read
  as the event not having happened; `spawned_by`; derived subsession
  IDs, with a retried call appending a root to the existing child
  session; `link` written at dispatch.
- Entry envelope: members of a core entry this document does not
  define are preserved.
- File: entry order is the ordering, not `ts`.
- Writing discipline: fsync on function call outputs; entries named in
  `records` are durable before the side effect they precede.

Considered and held: instructions as parts in `config`, which would add
a second spelling of settings and change step 2 for a storage cost that
belongs to the store, and which 0.3 adopts; and a durable leaf marker, which a library can
carry as a reserved `label` without a format change; and a `source`
on the item envelope for an input that joins a run already in flight,
which the `run` entry cannot name and which 0.3 adopts beside the
`queued` entry. All three were open questions; two are now answered.

## Open questions

- Whether the rule for honouring a durable leaf marker belongs here.
  Answered by RFC 0002: a session resumes at its store's head, and the
  marker is how a projection records a head that is not the last line.
  A reader of a bare file applies the rule the file section now
  states, the newest entry in file order that descends from the mark
  and was written after it, or the mark itself when nothing follows,
  and with the head in the store that rule is no longer what a resume
  depends on.
- Whether a core `exchange` type is worth defining for the common case
  of one entry converging several subagent results, or whether
  `parents` on an `item` already covers it. Held: `parents` covers it,
  and a type earns its place only once a reader needs to treat the
  convergence differently from the item that carries it.
- Who assigns a session's append sequence when two writers append at
  once. Answered by RFC 0002: the store does, always, by serialising
  appends to a session's log even when it accepts them concurrently,
  and which branch is live is the head, a ref, never inferred from the
  order.
- Whether to allow a second payload profile at 0.x, or hold the line at
  Open Responses and rely on converters.
- Sidecar media layout and naming. Answered in the file section: a
  `sidecar:` URL carrying the blob's hash, and a file named by the hex
  digest in the session's directory. Whether a store's projection may
  share one directory across sessions is still open.
- The venue: this repository, a standalone repository, or a proposal to
  openresponses.org as a companion document.
