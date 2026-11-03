# RFC 0001: Agent Session Format

Status: draft 0.17
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
along a path rebuilds a request byte for byte in canonical form
(RFC 8785), without resolving anything outside the file: material that
arrived from elsewhere is carried in the entry that received it, and
the reference saying where it came from is provenance beside it. Member
order inside opaque JSON, such as a tool's parameter schema, is not
kept across a rewrite or a store. **Record entries** are
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
  request the model received, byte for byte in canonical form (RFC
  8785). Preservation is of members, not bytes, so member order inside
  opaque JSON, such as a tool's parameter schema, may not survive a
  rewrite or a store; a renderer that needs the same bytes from a live
  request and a rebuilt one canonicalises both.
- **Resumable.** For every call without an output, a reader can tell
  from the path, and for a call a rebase left above its `dispatch` from
  the rest of the session, whether it was never started, was in flight when the
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
- **Universal root**: the one point every session's entries hang from,
  shared by every session and holding nothing. It is no entry and has
  no ID; `parent: null` is its spelling on the wire, naming an edge to
  it. An entry whose parent is the root is a **first entry**. A session
  is one tree, under the root; several first entries are branches at
  the root, not several trees.
- **Path**: the sequence of entries from an entry up to the universal
  root, reversed, following `parent` alone; its first entry is a first
  entry.
- **Leaf**: the entry the next append will name as its parent. In a
  store this is the session's head; in a file it is what the file's
  rules give.
- **Base**: the entry, in another session, that this session continues
  from, named by the header. The entries from the first entry to the
  base are the file's **prefix**; the entries after it are the session's own.
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
- A blob's media type, since 0.16, is the file named `<hex>.type`
  beside the blob in the session's directory: the type the data URL the
  reference replaced carried, in US-ASCII and nothing else in the file,
  a reader ignoring one trailing LF. It is what an RFC 2397 data URL
  carries before `;base64`: an RFC 6838 `type/subtype` and any
  parameters, each `;attribute=value`, where the type, the subtype, an
  attribute and a value are each an RFC 2045 token, so a type holds no
  space, no quoted value and no comma, and resolves to a data URL as it
  is. The session's type is the session's: two sessions may give the
  same bytes two types (RFC 0002, Media). The type is outside every
  hash: the reference names the bytes alone, a reader verifies the blob
  against its hash as before and verifies nothing of the type, and no
  entry, context, request or blob hash covers it. A blob with no type
  file has no type, as one a 0.15 writer projected has none; a reader
  that needs one, as a writer resolving a reference to a data URL does
  (Request hash), uses `application/octet-stream`, as RFC 0002's Media
  section says. A type file that does not hold a media type is
  malformed, and a reader importing the directory MUST refuse it rather
  than guess.
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
  file when no such label is in force. A `leaf` label whose `target` is
  `null` names the universal root, in a file with no `base`: every
  entry descends from it, so the leaf is the newest entry written after
  the label, and the root itself, the empty context, when nothing
  follows it. A `leaf` label is not in force
  when its target is not in the file, is on the prefix above the base,
  is the root in a file with a `base`,
  or is itself a `leaf` label, since the leaf may rest on none of those;
  a leaf so found that is itself a `leaf` label resolves to its nearest
  ancestor that is not one. The label marks the branch that is live, not
  the tip it had when marked, so a branch marked and then extended
  resumes where it was extended to. In a store the head is the leaf, and
  this rule is how a projection carries a head that is not the last
  line.
- A file whose header names a `base` opens with the prefix: every entry
  from the first entry to the base, in path order, before any entry the
  session appended itself. The prefix is another session's record,
  carried here so the file stands alone; the header's `records` promise
  and the rules that rest on it apply to the entries after the base.
  What the origin wrote off the path to the base is not carried, a
  `dispatch` for a prefix call among it; `dispatch` says how a reader
  finds one through `parent_session`.
- `ts` is informational and a reader MUST NOT order entries by it;
  clocks step backwards. This is a rule about the member an entry
  carries, which its writer asserts. A sequence a store assigns as it
  accepts entries — a line number, a row key, a commit timestamp the
  store's clock is authoritative for — is a different thing and is what
  entry order is in a file. A store MAY order by one.

## Header

```json
{"type":"session","format":"agentsession/0.17","id":"…","created_at":"2026-09-17T12:00:00Z",
 "payload":"openresponses/2026-04-24","harness":{"name":"…","version":"…"},
 "records":["run","dispatch","decision"],
 "cwd":"/path","parent_session":"…","base":"sha256:…","spawned_by":"call_…",
 "media":"inline"}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | the string `session` |
| `format` | MUST | `agentsession/<major>.<minor>`; raised by a later writer, never lowered, as Versioning says |
| `id` | MUST | globally unique; UUIDv7 RECOMMENDED. For a subsession, a UUIDv5 under the nil namespace over `<parent session id>/<call_id>` is RECOMMENDED, so a reader can compute the child's ID from the parent's `link` or `function_call` alone. A retry of the call is another session, derived the same way over `<parent session id>/<call_id>/<n>` for the n-th attempt after the first. A redacted session's is new, and derived as Redaction says |
| `created_at` | MUST | RFC 3339 |
| `payload` | MUST | payload profile; `openresponses/<spec-date>` is the only profile this RFC defines |
| `harness` | SHOULD | name and version of the writer |
| `records` | SHOULD | the record entry types, core or namespaced, this writer writes whenever their event occurs, so a reader may take their absence as the event not having happened. Absent or empty means no such promise |
| `cwd` | MAY | working directory at creation; a reader MAY take it as the working directory only before the first `env` entry on the path, as `env` says |
| `parent_session` | MAY | session ID this was forked or spawned from. Provenance, not validated for a fork: one made at an entry on another fork's prefix may name either session, and a `fork_of` link records the one it was made from, and it is the session a reader resolves a prefix call's `dispatch` through, as `dispatch` says. For a subsession it is the session holding the `subsession` link that names this one, and a verifier MAY check the two agree, as `link` says |
| `base` | MAY | hash of the entry this session continues from, never a `leaf` label, since the base is a fork's first leaf; `parent_session` names a session holding it, as provenance. Absent for a session that starts fresh. When present the file opens with the path to it, and the session's own entries hang from it |
| `spawned_by` | MAY | for a subsession, the `call_id` of the parent's function call that spawned it, which is the `call_id` of the parent's `subsession` link naming this session. A session a `subsession` link names carries it and `parent_session` both, since the check `link` describes compares each; a subsession's header without one reads as disagreeing with the link |
| `media` | MAY | `inline` (default) or `sidecar`. Fixed when the session is created: an item's bytes are hashed, so a rewriter MUST NOT convert media from one form to the other |
| `redacted` | MAY | `true` for a redacted session: one that holds entries a redactor made from another session, its original, as Redaction says. Its hashes verify it and not the original. It may grow and be resumed; an entry appended natively is not redacted, and each redacted entry carries `redacted` |
| `redacted_from` | MAY | for a redacted session, the session it was made from: `{"session":"<original id>","profile":"…"}`; `profile` names the redaction, as Redaction says. A redactor SHOULD write it, and it MUST NOT appear without `redacted` |

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
| `parent` | MUST | hash of the parent entry, or `null` for the universal root, which makes the entry a first entry |
| `parents` | MAY | further predecessors this entry converges; provenance only, never walked when building a context |
| `ts` | MUST | RFC 3339 in one form, since it is hashed as a string: UTC, uppercase `T` and `Z`, seconds `00` to `59`, a fractional part only when non-zero, with no trailing zeros and at most nine digits, as in `2026-09-17T12:00:02.5Z`. Mainstream time types hold neither a tenth digit nor a second `60`; a writer truncates the fraction to nine digits and writes a second `60` as `59` with the same fraction. A reader MUST report any other spelling as it reports a hash that fails |

A parent MUST appear earlier in the file than any child. A file with no
`base` may branch at the universal root, holding several first entries;
a file with one has one prefix
and every own entry descends from the base: the lines before the base
are the path to it, one first entry and each the child of the line before, and
every line after it names as `parent` the base or a line after it, never
`null` and never a prefix entry above the base. A reader MUST refuse a
file that breaks this, as it refuses a parent not in the file, since a
writer appending to the file is held to the same rule and could not
continue what it reads. An entry MUST NOT be
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
  the **envelope object**: `type`; `parent`, `null` for the root;
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
`from`, `queued_from`, `from_link`, `omit`'s `items`, `outputs`' `items`,
a `groups` element's `items` and `images`' `before` in a `config` and in a compaction's checkpoint, `instructions_omitted`'s `of` beside a positive
`keep` and no `id`, where alone it is defined, and `copied_from`'s
entry, naming an entry of the session its session member names — it
names it by `id`.

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
the same answer, and a rewriter that changes no member of a line writes
each back as the line held it, present or absent. `sha256:` is the only
prefix, and a reader MUST refuse an `id` carrying another.

A reader MUST verify each entry's `id` by computing its content hash and
then its envelope hash, and MUST report a line that fails. It is
corruption, not an extension, and a reader MUST NOT repair it. A
redacted session has had its hashes recomputed over the redacted
bodies, as Redaction requires, so it walks and verifies against itself
and not against its original; its header says so, and a reader MUST NOT
report its hashes as verifying the original.

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
one beside `kind` and `host` inside `workspace`, is preserved the same
way, and like every member it is hashed as the line holds it. A reader
MUST NOT compute an entry's hashes from its own model of the entry when that model cannot
hold everything the line does.

Three members record how a line came to be written, and not what the
entry holds or says: `legacy_id` (Versioning), `normalised` (Entry
hash) and `redacted` (Redaction). The header's `redacted` and
`redacted_from` are the same in the header. These are **history
members**, since 0.17. A writer writes one only where its own writing
made it true:

- `legacy_id` on an entry it migrated;
- `normalised` for a member it normalised;
- `redacted` on an entry it made by redaction, as Redaction already
  requires.

A writer that makes an entry from another, as a converter does, MUST
NOT carry the other entry's history members as its own. It MAY keep
them under its own namespace (`<slug>:legacy_id`). A redactor is the
exception Redaction makes: an entry it makes by redaction keeps the
`legacy_id` of the entry it made it from as written, and its
`normalised` as Redaction says, since they name and describe the line
the redaction rewrote, and carries a `redacted` of its own. An entry rebuilt,
every member restored, to the `id` of the entry it was rebuilt from is
that entry and not one made from it, so its history members are true
of it (Projections, ATIF). Every other member is a **meaning member**.
That includes every member this document does not define, since a
later minor may define it. A minor that defines a member says which of
the two it is.

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
`branch_summary`. Record entries: `run`, `dispatch`, `result`, `decision`,
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
- `failed` MAY be `true` on an `item` entry holding a
  `function_call_output`, since 0.14: the tool reported failure, or the
  harness gave the call up (cut it off, timed it out, could not learn
  its outcome), and the output describes it. It is a record member: the
  item is sent as it is, the member reaches no request, and the output
  ends the call as any output does. A reader maps it to its protocol's
  failure state, such as a tool span's error status or a replayed
  call's `failed`. Its absence says nothing: an output without it does
  not say the call succeeded. A writer MUST NOT write it on any other
  item, nor with any value but `true`; a reader keeps `failed` on
  another item, or holding anything but
  `true`, as a member it does not know. A model that has to see the
  failure sees it in the output; a member the model sees, as Claude's
  `is_error`, belongs inside the item under the payload profile's
  extension mechanism, not here. A writer, a converter among them,
  writes it from what the harness or the native record says of the
  call: an error flag, or a failed, aborted, interrupted or timed-out
  status. Since 0.17 it MUST NOT infer it from the output's text, or
  from an exit status the output reports, which a tool may report
  without failing. A judgement made later that a call failed is an
  `outcome` of kind `tool_error`, not `failed`. A tool's exit status is
  the tool's own detail, a member this document does not define.
- `trust` MAY be on an `item` entry, since 0.15: what the harness knows
  about where the item's content came from. It is an object with
  `reads`, an array of labels without repeats. `untrusted` says the
  content came from a source the harness does not trust, such as a web
  page or an inbox. `private` says the content holds data the session's
  owner would not have leave it. `{"reads": []}` says the harness
  judged the content and found neither, and an absent `trust` says
  nothing. A writer writes it on the output of a tool whose
  declaration labels what it reads, and MAY write it on any item, such
  as a message carrying pasted content. It is a record member, like
  `failed`, and reaches no request. A reader keeps a label this
  document does not define as written, and a reader that decides
  anything by trust reads such a label as `untrusted`. A writer MUST
  NOT write another form, and a reader keeps one as a member it does
  not know.
- The **trust of a path** is the union of the `reads` of every `trust`
  on it: each `item` entry's, items an `omit` leaves out or sends as a
  placeholder included, since the model may have read them on an
  earlier request, and each `compaction`'s and `branch_summary`'s.
  The trust of a `branch_summary`'s `from` path is folded into the
  summary's own member, as `branch_summary` says. A copied item whose
  `copied_from` names an entry MUST carry that entry's `trust` as
  written, and none when it carries none, so a label a session read
  reaches every session its conversation is copied into. A copied item
  that names no entry, a summary or another item made from the path it
  was copied from among them, carries the trust of that path. A reader
  that holds the source checks a copy's `trust` with its `item`, as
  `copied_from` says. The policy a harness applies by
  trust, to ask, refuse or taint a session, is its own.
- `from_link` MAY name, by `id`, a `peer` link entry before the item on
  its path, other than a cancel notice, since 0.14: the item carries
  that callee's later report, as a peer whose work outlives its call
  delivers its result some turns after the call's output acknowledged
  it. It is provenance. It does not make the call answered, which
  `link` decides from the call's output alone, and it reaches no
  request. A writer MUST NOT write one that names anything else, a
  cancel notice included; a reader keeps a `from_link` that is not a
  non-empty string as a member it does not know, and a tool that checks
  a file reports one that names no `peer` link before the item on its
  path, or names a cancel notice, which names no callee of its own but
  the link it is about. An item MAY carry
  `source` beside it, the harness's own trigger for the delivery. The
  reference points back, into this file, at an entry already written,
  so writing and verifying the file needs nothing from the peer's
  record, and the peer's record names nothing of this one.
- `copied_from` MAY be on an `item` entry, since 0.15: the item was not
  produced in this session but taken from the conversation of another,
  as when a run is prompted with the conversation of the session that
  spawned it, or a fork answers a call with the output its origin
  recorded. It is an object `{"session": s, "entry": e}`. `session` is
  required: a non-empty string other than this session's `id`, naming
  the session the item was taken from. `entry` is optional: the `id` of
  the entry of that session whose `item` this entry's `item` is,
  member for member in canonical form. A writer names `entry` only for
  an item it copied unchanged. It leaves `entry` out for an item it
  changed, and for an item it made for the copy, such as the output a
  seed gives to a call the copied conversation had not answered. The
  member is provenance: the item is sent as it is, the member reaches
  no request, and the ingress rule holds, since the item is carried
  whole. A writer MUST NOT write `copied_from` on any other entry, or
  in another form. A reader keeps a `copied_from` it cannot read this
  way as a member it does not know. A reader that holds the session
  named MAY check an item naming an entry against that entry, its
  `item` and its `trust`, and reports one that differs, or an entry
  that session does not hold. It checks with the read rights of whoever
  asks for the check and no more, since a check made with more answers
  whether an entry the asker may not read holds a given item; it does not check an item a redaction
  made against a session that was not redacted alongside it.
- `instructions_part` MAY be on an `item` entry holding a `message`,
  since 0.17. It says that the harness composed the item from an
  instruction layer and sent it as a message, and not in the
  instructions: an instruction file wrapped in a reminder, or a memory
  block. It is an object `{"id": i, "source": s}`, with the meanings
  `id` and `source` have on an instructions part (`config`,
  Instructions as parts). `source` is required, a non-empty string, and
  `id` is required when the layer has more than one part, as `agentsmd`
  does; `id`, when present, is a non-empty string. The item carries the
  layer's text as the harness composed it, wrapping included.

  It is a record member, like `trust`, and a meaning member (Entry
  envelope). The item is sent as it is, the member reaches no request,
  and the item is not an input of the run (`run`). A writer MUST NOT
  write it on any other item, or in another form, and a reader keeps
  one it cannot read this way as a member it does not know. A copy of
  the item (`copied_from`) MAY carry it.
- A `function_call` whose entry carries `copied_from` is a **copied
  call**: the call was made in the session named, and this session
  holds it only as context. A copied call is not a call of this
  session. It is never one of a run's calls, is never pending in this
  session, and no `records` promise covers it. A writer MUST NOT write
  a `dispatch`, a `decision` or a `result` whose `target` is a copied
  call. A
  `function_call_output` for a copied call is copied too, whether or
  not its entry carries the member. A writer MUST write `copied_from`
  on every item it copies and on every output it makes for a copied
  call. A copied call's `call_id` is held to the uniqueness rule like
  any other, since an output names its call by it. A writer gives each
  copied call an output, copied or made for the copy, before it sends a
  request whose context holds the call. A copied call with no output on
  the path is not resumable in this session: nothing here may run it.
- A `function_call_output` that carries `copied_from` but whose call is
  not a copied call is this session's output for that call, with its
  content taken from the entry named. It ends the call as any output
  does, and every rule about outputs holds for it, `answer` among them.
  A writer that answers a call with an output taken from another
  session's entry MUST name that session and entry in `copied_from`.
  This is how a fork answers a call in its prefix with the output the
  origin recorded off the path to the base: an `answer` decision, then
  the output, whose `copied_from` names the origin and the origin's
  output entry. A reader that holds the origin follows the member to
  the origin's `subsession` or `peer` link for the call, on the
  origin's path to that entry. The fork's own record cannot hold that
  link, since a `subsession` link names a child of the session that
  writes it.
- A copied item is never one of a response's output items. A writer
  MUST NOT write `response` on one, since no `response` entry of this
  session envelopes it, and a tool that checks a file reports one that
  carries it: an item of this session's own model call written as
  copied would escape every rule a call of this session is held to.
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

A delta to request settings. Every first entry SHOULD be a `config`
carrying full settings.

```json
{"type":"config","id":"…","parent":"…","ts":"…",
 "model":"…","instructions":"…","reasoning":{…},"text":{…},
 "instructions_parts":[{"keep":2},{"id":"AGENTS.md","text":"…","source":"agentsmd"},
                       {"id":"memory","hash":"sha256:…"}],
 "instructions_omitted":[{"id":"service/AGENTS.md","reason":"budget","size":4096,"source":"agentsmd"},
                         {"keep":3},{"keep":474,"of":"sha256:…"}],
 "omit":{"reasoning":"other_models","items":["sha256:…"]},
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
repeating it, such as `product` or the path of an instruction file.
`source` names the layer that produced the part. Since 0.17, four
values of `source` are defined:

- `product`: the harness's own prompt for the product it is;
- `agentsmd`: an instruction file, AGENTS.md or a harness's
  equivalent, one part per file, its `id` naming the file;
- `agentskill`: the catalogue of skills offered;
- `agentmemory`: memory kept between sessions.

A writer SHOULD use these values for those layers, and a reader MAY
rely on them, in a file of any minor, since writers used them with these
meanings before they were defined. Every other `source`
is in the harness's own terms and opaque to a reader. The same values
name the layer of an omitted part (`instructions_omitted`) and of an
`item`'s `instructions_part`.

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
- The text the path has for an `id` is the text of the part by that
  `id` in force, and of each part by that `id` that has left force: a
  part a later delta removed, or gave other text. It stays named by its
  `id` and `hash` until the next `replace` or compaction's checkpoint,
  which starts the path's parts afresh. A `hash` that the part in force
  does not have resolves to the text that most recently left force
  under that `id` with it, and to that part's `source` when the element
  carries none. A hash is resolved against the part in force under its
  `id` first, and only when there is none, or its text has another hash,
  against the parts that left force. A writer of 0.11 therefore names a
  part by its `hash` when no part is in force under its `id`, or the one
  in force has other text the path could resolve, and the path has the
  text and `source` it means among the parts that left force. It writes
  the `text` for any other part, among them a part in force with that
  text and another `source`, and one in force whose text the path could
  not resolve. A writer of 0.10 named only a part in force and wrote the
  text of every other, so a file of 0.10 holds no `hash` for a part out
  of force, and a reader of 0.10 that resolves a `hash` against the
  parts in force alone, which fails every one a 0.11 writer writes,
  refuses such a file by its header. A hand-back to an agent whose parts
  another agent replaced then costs the parts that changed, and not the
  whole prompt.
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
  beside `keep`, which a reader ignores, but `of`, which 0.11 defines
  below. A `keep` member on an element that carries an `id` is a member
  this document does not define there.
- A `keep` that runs past the end of the list in force or takes a
  part the list names elsewhere, which a reader finds by reading the
  whole list first, and an element with neither an `id` nor a
  positive integer `keep`, name no part a reader can rebuild. A reader
  keeps such an element in the list in force where it stands, as it
  keeps an instruction part it cannot resolve, and a later `keep` that
  takes it takes it as it is. Unlike a `keep` among the instruction
  parts, nothing checks one here: the list reaches no request, so no
  `request_hash` covers it.
- A `keep` MAY carry `of`, the `id` of an earlier `config` entry, and
  count over the list that entry put in force instead of the list in
  force before this one; the paragraphs below define it.
- `replace: true` discards the list in force with the rest of the
  settings, so a `keep` in the same entry resolves against nothing,
  and a writer MUST NOT write one there: a replace writes the list
  whole. A compaction's checkpoint is not a delta, and writes the list
  whole too; an element in force that named no part is written as it
  stands, and a reader of a checkpoint takes each element as it is,
  resolving no `keep` in it.

A hand-back repeats a list an earlier entry on the path already wrote:
an agent given a session again after another ran writes the omitted
list it wrote the first time, element for element. An element is about
the size of a hash, so hashing elements would save nothing; what can be
named is the list. A `keep` that carries `of` counts over the list an
earlier entry put in force, and not over the list in force before this
entry:

- `of` is the `id` of a `config` entry on the path before this one that
  carries `instructions_omitted`. The element stands for the next n
  parts of the list that entry put in force, as this document resolves
  it at that entry, each unchanged, `reason`, `size` and `source` alike.
  `[{"keep":474,"of":"sha256:…"}]` is then the whole list an earlier entry
  wrote, in under a hundred bytes where the list is 37 KB.
- Each list a delta counts over has a cursor of its own, starting at
  that list's first part. A `keep` moves the cursor of the list it
  counts over, past the parts it takes, whether or not it resolves. An
  element naming a part by `id` moves the cursor of each list that
  names the `id`, to just after the first part so named, and leaves
  that of a list that does not. The list in force before the entry is
  the list a `keep` without `of` counts over, so the cursor rule above
  is this rule with one list: an element carrying `of` moves the cursor
  of its own list and no other, and the next element without it counts
  from where the cursor over the list in force stands. A change to the
  61st of 126 parts of an earlier list is
  `[{"keep":60,"of":"…"},{"id":"m61",…},{"keep":65,"of":"…"}]`.
- A writer MUST NOT write `of` on an element that carries an `id` or
  that has no positive `keep`, a member this document does not define
  there, and MUST NOT write a `keep` that carries `of` and runs past the
  list it counts over or takes a part another element of the delta
  names. The list the delta resolves to names each `id` once, so a
  writer that counts over two lists takes no part from both.
- A `keep` that carries `of` names no part a reader can rebuild when
  `of` names an entry that is not on the path before this one, that is
  not a `config` entry, that does not carry the member, or whose list a
  `replace` or a compaction's checkpoint has discarded since, and when
  the `keep` runs past that list or takes a part the delta names
  elsewhere. A reader keeps such an element in the list in force where
  it stands, `keep` and `of` as written, as it keeps any element it
  cannot resolve. A tool that checks a file SHOULD say so, since nothing
  else does: the list reaches no request. A reader holds the list each
  such entry put in force since the last `replace` or checkpoint, and
  with long lists in a long session that is their sum. An element with
  an `id` takes `of` for a member this document does not define there,
  and the element is the part it names.
- A `replace` or a compaction's checkpoint starts the lists afresh, as
  it starts the parts: no `config` entry before one is named by an `of`
  after it, and an entry that carries `replace: true` writes its list
  whole, so carries no `of`, though a later entry may name its list. A
  checkpoint writes the list whole, so nothing after it names an entry
  before it.

#### Items left out of the context

Every `item` entry on a path is in the request, until a writer leaves
one out; and until 0.11 nothing said it had. A reasoning item is bound to
the model that produced it, and a provider refuses another model's
signature, so a harness that changes model part-way through a session
sends the rest of it without the reasoning that came before, a request
the path does not rebuild. A response written after the switch could not
carry a `request_hash` that verifies, nor could any after it, for as
long as those items are on the path. `omit` records the rule, once.

```json
{"type":"config","id":"…","parent":"…","ts":"…",
 "omit":{"reasoning":"other_models","items":["sha256:…"],
         "outputs":{"text":"[cleared]","items":["sha256:…"]},
         "images":{"keep":3,"placeholder":"[screenshot removed]"}}}
```

`omit` is an object, in force as a setting is: a delta without it
leaves the object in force as it was, `replace: true` without it clears
it with the rest of the settings, and a compaction's checkpoint carries
it. A `config` entry MAY carry it and no setting. Its members:

- `reasoning` is closed, and `other_models` is the one value this
  document defines. A request leaves out each `item` entry on the path
  that holds a `reasoning` item, carries a `response`, and was written
  while a model other than the request's was in force. The model in
  force at an entry is the `model` the settings hold there, the path from
  its first entry to it replayed as the context algorithm replays it, a
  compaction's checkpoint standing for the entries it folded; the model
  in force for a request is the one the settings hold at the end of its
  path. A `response` entry's own `model` is not read: it is the
  provider's name for the model that answered, which may be a dated
  snapshot of the alias the request named, and is not comparable with
  the name the request carried. The rule is a function of the record,
  so a writer writes it once, at the entry that changes the model, and
  it covers every later request without listing an item. A switch back
  is covered too, since the rule reads the model in force at each
  request. An empty model name attributes nothing: a reasoning item
  written while no model was in force is never left out, and a request
  under no model leaves nothing out. A `reasoning` value this document
  does not define has no effect, leaving the rule in force as it was,
  and a reader keeps the member as written.
- `items` is the general case, for a host's own filter: a list of entry
  ids, and an `item` entry on the path whose id it lists contributes
  nothing, whatever it holds. A writer writes no empty id, and a reader
  takes a list with no non-empty id as listing none. An id that names no
  `item` entry on the path names nothing; a `compaction`'s summary and
  pinned items and a `branch_summary`'s summary are not `item` entries,
  and `omit` does not reach them.
- `outputs`, since 0.14, is `{"text": t, "items": [ids]}`, `t` a
  string: an `item` entry on the path whose id the items in force list,
  and that holds a `function_call_output`, is sent with that item's
  `output` the string `t`, its `call_id` and every other member as they
  are. The call stays in the request with an output, since a call
  without its output is not a valid request; this is how a harness
  that clears old tool results, as the Claude API's context editing
  does, records the request it sent. The items in force are the union
  of every `outputs`' `items` on the path since the last `replace` or
  compaction's checkpoint, as for `items`, and `t` is the `text` of the
  latest `outputs` on the path. An id that names no such entry names
  nothing.

  Since 0.16 `outputs` MAY also carry `groups`, a list of
  `{"text": t, "items": [ids]}`, so that one request can send outputs
  with different texts, as when a host clears old results and a harness
  quarantines one it distrusts in the same request:

  ```json
  "outputs":{"groups":[{"text":"[cleared]","items":["sha256:…"]},
                       {"text":"[quarantined]","items":["sha256:…"]}]}
  ```

  The `text` and `items` beside `groups` are the group the 0.14 form
  writes, the *default group*, and are left out together when an
  `outputs` writes none, while `items` without `text` stays a form this
  document does not define. A `text` present writes the default group
  whatever it holds, so `"text":""` beside `groups` sets the default
  group's text to the empty string, as it would alone. Each element of `groups` is a group named by
  its `text`. An output is sent with the text of the group in force that
  lists its entry. In force are the default group, read as above: the
  union of every `items` beside a `text` since the last `replace` or
  compaction's checkpoint, with the latest such `text`; and each named
  group: the union of every `items` of a `groups` element of that `text`
  since then. A named group's text never changes, so a later element
  with the same `text` adds to it.

  An entry is in at most one group in force. A writer MUST NOT write an
  `outputs` that lists one id in two of its groups, the default among
  them, that holds two `groups` elements of one `text`, or that lists in
  one of its groups an id that a group in force other than that group
  already lists; a writer refuses one rather than write it. The default
  group and a named group are different groups whatever their texts, and
  a group listing an id it already lists is no conflict, as a 0.14
  `outputs` listing an id the default group holds was none. A reader
  takes such a member as a form this document does not define, and it
  changes nothing. A `replace`, or a compaction's checkpoint, starts the
  groups afresh, so what it writes is checked against nothing before
  it.

  An entry leaves its group only when the groups start afresh: a writer
  that moves an output to another text, or sends it as recorded again,
  writes `{}`, which clears the `omit` in force, and then the `omit` it
  wants whole, or a `replace` carrying it, and every request after
  rebuilds from that. A `groups` that is not a list of objects each holding a
  string `text` and a list `items` and nothing else is a form this
  document does not define too. A file of 0.14 or 0.15 holds no
  `groups`, and a reader of 0.16 reads its `outputs` as those minors
  did; a writer of 0.16 MAY write either form, and writes `groups` for a
  text other than the default group's.
- `images`, since 0.14, takes one of two forms, each with a string
  `placeholder` `t`, and an `images` in a later delta replaces the one
  in force:
  - `{"keep": n, "placeholder": t}`, `n` a non-negative integer: of the
    `input_image` parts of the items the request carries from `item`
    entries, counted in request order, every one but the last `n` is
    sent as an `input_text` part whose `text` is `t`.
  - `{"before": id, "placeholder": t}`: every `input_image` part of an
    `item` entry before the entry `id` names on the path is sent as an
    `input_text` part whose `text` is `t`. An `id` not on the path names
    nothing, and nothing is replaced.

  An `input_image` part is one in a `message`'s `content`, or in a
  `function_call_output`'s `output` when that is a list of parts. A
  loop that keeps the last three screenshots writes `keep`, or moves
  `before` each turn, or every 25 turns to keep a provider's cache
  warm, as Anthropic's computer-use guidance advises. A member holding
  both `keep` and `before`, or neither, or anything but a string
  `placeholder`, is not a form this document defines: a reader keeps it
  as written and replaces nothing by it, and a writer MUST NOT write it.
- `outputs` and `images` reach the items of `item` entries alone, as
  `items` does. They apply after `items` and `reasoning`, to the items
  those leave in the request: first each output `outputs` lists, then
  the image parts `images` reaches among what remains, so an output
  sent as `t` holds no image to count.
- An `omit` in a delta sets the `reasoning` it names when this document
  defines it, leaves the one in force when it names none, and adds its
  `items` to those in force: the set in force is the union of every
  `items` on the path since the last `replace` or compaction's
  checkpoint, so a later entry adds to it and never has to repeat it.
  Its `outputs` adds its `items` to the default group and sets its
  `text`, and adds each `groups` element's `items` to the group of that
  `text`, as `outputs` says; its `images` replaces the one in force; a delta without either
  leaves it as it was. An object that names no `reasoning`, lists no
  id and carries neither `outputs` nor `images`, `{}` above all, clears
  all four. A writer that wants to drop one and keep the others writes
  `{}` in one entry and what it keeps in the next.
- An `outputs` or `images` member in a form this document does not
  define, such as `images` without a `placeholder` or `outputs` without
  a string `text`, is carried all the same, as a `reasoning` value this
  document does not define is: it changes nothing, the object holding
  it is not one that clears, and a reader keeps it as written. A writer
  MUST NOT write either member in such a form.

The `omit` in force at the end of a path decides every `item` entry on
it, those before the entry that wrote it among them, so a request built
over a path that ends before the entry never meets the member. What a
request leaves out, or sends a placeholder for, stays in the record:
the entry is on the path and among the context's entries, as written,
and a projection can show it as omitted or replaced and not as absent.
The `request_hash` is over the request built with the `omit` in force
applied, so a response that hashes verifies. A response whose recorded
hash is the request built with an item the `omit` in force leaves out,
or with the output or image parts it sends a placeholder for, is a
divergence, a hash that does not match, and not a response that
recorded none.

### `compaction`

Replaces earlier context with a summary.

```json
{"type":"compaction","id":"…","parent":"…","ts":"…",
 "first_kept":"entry-id","summary":{…item…},"pinned":[{…item…}],
 "config":{…full config…},"tokens_before":50000,"usage":{…}}
```

- `trust` MAY be on a `compaction`, since 0.15: the trust of the path
  up to the compaction, as `item` defines it, so a reader holding only
  the path from the fold on, or the context alone, knows what the
  summary was made from. It MAY be wider than the union a reader
  computes from the entries it holds, since a redaction may have left
  some out. A reader takes the union of both, and a writer that writes
  a later fold carries the earlier one's labels and never narrows them.
- `first_kept` MUST name an entry on the path before this one, since
  0.14 in those words: a compaction cannot name itself. Entries before
  it are excluded from context; entries from it up to this entry are
  kept. A reader refuses to build a context through a compaction whose
  `first_kept` names no entry before it on the path.
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
   "instructions_omitted":[…],"omit":{…},
   "reasoning":{…},"text":{…},"tools":[…],"extra":{…}}
  ```

  `instructions_parts`, when the checkpoint carries it, is the full
  list of parts in force, each with its text, not a delta, and
  `instructions` is their join. `instructions_omitted` is the list of
  omitted parts in force, every part written out and no `keep` and no
  `of`, and a checkpoint without it has none in force. `omit` is the
  object in force, its `items` and its `outputs`' `items` the unions so
  far, its `outputs`' `text` and its `images` the latest, its
  `outputs`' `groups` each named group in force with its `items` the
  union so far, and a
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
left. `summary` is an item that enters context. `trust` MAY be on a
`branch_summary`, since 0.15, as on a `compaction`: the trust of the
path `from` names, since the summary was made from that branch.

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
  it writes `start`: it started the run to take up a call. A run written
  `resume` whose segment, between its `start` and its `end`, or after
  its `start` to the end of the path when the run was cut, holds no
  entry that is in context, no `response` and no `dispatch` or
  `decision`, is accepted as written: the writer started it to take up a call and the run ended
  before it did, a refusal by a subscriber to the run's start and a
  kill among the causes, and `resume` records what it meant to do,
  which such a segment cannot contradict. Record entries (`link`,
  `custom`, `env`, `info`, `label`, `outcome`, `queued`) do not make a
  segment the shape of `input`: a writer that resumes may replay a
  child's link onto a new branch, or a hook may annotate the run,
  before the run takes its call up. A `resume` over a segment that adds
  a message, a setting or an output, or holds a `response`, and takes up
  no call is not that, and a reader reports it: a `response` is a model
  call, and one that took no call up is the shape of an `input` that
  retries, as below. A reader of 0.14 applies the rule to every
  file it reads, as a reader of 0.11 applied its narrower form, a
  segment holding nothing, to files of 0.9 and 0.10, since the rule
  only accepts what an earlier reader reported. `input`:
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
  run's calls are the `function_call`s on the segment, copied calls
  excepted, and those on the path before it that the segment holds a
  `decision`, a `dispatch` or a `function_call_output` for, since the
  run took them up; a pending
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
     has calls, every call on the path but a copied one has an output, the segment
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
- A reader that rebuilds the inputs that drove a path, as a replay
  does to send them again, takes as a run's inputs the `message` items
  of role `user` on the segment of a run whose `source` is `input`. It
  takes those that come before the segment's first `response`, or all
  of them when the segment has none.

  Of those, it leaves out each item that is not an input of this
  session's run:

  - an item carrying `copied_from`, which another session's
    conversation holds;
  - an item carrying `from_link`, which carries a peer's report;
  - an item carrying `instructions_part`, which the harness composed
    (`item`).

  An item drained from a `queued` entry before the segment's first
  `response`, a steer, a delivery or a `followup`, is an input of the
  run: a `followup` and a delivery that start a run are its inputs. An
  item whose `queued_from` names a `queued` entry whose `mode` is
  `steer`, a delivery among them, after the first `response` joined the
  run in flight, and is not an input of a new turn. A user message on a
  `resume` segment belongs to the resume, such as the note a person
  typed with an approval. It is not an input.

  A reader that cannot reproduce an item that joined a run in flight
  where it stood MUST say so rather than send it as the input of a new
  turn. On a path with no `run` entries, a reader takes as one turn's
  inputs the user messages between one `response` and the next, those
  before the first included, leaving out the same items and every item
  drained from a steer, since it cannot tell one that started a turn
  from one that joined it. It says that it could not tell a resume's
  note, or a delivery that started a run, from an input.
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
`call_id` MUST be that call's. The `target` names the call. A
`dispatch` a withdrawal follows, as `decision` defines it, was no
hand-off: what this section says of a call's dispatches is said of
the others, and the call's state is read as though neither it nor the
withdrawal were there, within the limit `decision` sets: a withdrawal
withdraws the one `dispatch` it directly follows. A withdrawal holds on
its own path: a `dispatch` it withdraws may be followed on another
branch by the call's output, which says that branch handed the call
off, so for any path that does not hold the withdrawal the `dispatch`
is one like any other, on the path or off it. A call with
a `dispatch` and no `function_call_output` on the path may have run: its
side effect may have happened. It is **in flight**, handed to its tool
when the record stopped, unless the latest `decision` after its last
`dispatch` is a `hold` or an `answer`, or a `result` follows that
`dispatch`, when the call has returned, as `result` says. A `hold`
after a `dispatch` holds
a call that may have run, and whoever answers the hold answers that
call, as a replay rule would: a `proceed` and another `dispatch` run it
again, an `answer` ends it without running it. An `answer` with no
output after it is a call the harness answered and has not yet written
the output of, since the record stopped between the two; the harness
that continues the path writes that output and nothing else for the
call. When the header names `dispatch` in `records`, a call with neither
on its path was never started, a copied call excepted, which is no call
of this session, and unless the session holds a `dispatch`
naming it elsewhere, off the path, a withdrawn one included: a writer
that rebases to a point between a call and its `dispatch` leaves the
call on the new path with no `dispatch`, and it may have run. Otherwise the file does not say
whether it ran. A call in a fork's prefix has its `dispatch` entries,
if any, in the session the fork was made from: the prefix is the path
to the base, and a `dispatch` the origin wrote off that path is not
carried, so the fork's file does not say whether the call ran. A reader
that wants them resolves the fork's `parent_session` and asks that
session, and so up a chain of forks, bounded, and reads a `dispatch`
found there as one on a branch a rebase left: the call may have run,
under that dispatch's key and with the arguments it handed over, and its
output, when the origin holds one, is on the origin's branch. A
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

### `result`

A call's tool returned, and its output is not yet on the path. Since
0.15.

```json
{"type":"result","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id",
 "item":{"type":"function_call_output","call_id":"call_…","output":"…"},
 "failed":true}
```

- `call_id`, `target` and `item` are required. `target` names the call
  as it does on `dispatch`, and `call_id` MUST be that call's. `item`
  MUST be a `function_call_output` whose `call_id` is the call's, and
  it is the output as the harness will append it, after whatever the
  harness does to what the tool returned. `failed` MAY be `true`, with
  the meaning it has on an output's entry, and `trust` MAY be on it, with
  the meaning it has on the output's entry, which carries the same. The
  entry is a record entry:
  its `item` is not in context and reaches no request.
- A writer writes a `result` only when a call's output settles before
  it can join the path: a harness that appends outputs in an order
  other than the order its calls settle, as one does that runs a batch
  in parallel and appends the outputs in the model's order, writes one
  for a call when its tool returns and the output cannot be appended
  yet. A harness that appends each output as its tool returns has no
  window to record, and writes none. The entry is written durably
  before the harness acts on the result in any other way. A writer
  writes it only after a `dispatch` of the call on the path that no
  withdrawal follows, and only when no output, `answer`, `reject` or
  other `result` of the call follows that `dispatch`. A `result`
  belongs to the latest `dispatch` of its call before it.
- A call with a `result` after its last `dispatch` and no output on the
  path has **returned**: its tool ran and returned the result's `item`.
  It is not in flight. The harness that continues the path appends the
  result's `item` as the call's output, with `failed` as the result
  carries it, and writes nothing else for the call. It writes no
  `dispatch`, since the tool has run, and no `decision`, save an
  `answer` when it withholds the result, as `answer` says of a tool
  that ran and whose result the harness withheld. The output after an
  `answer` is the harness's, and need not equal the result's `item`.
- An output after a `result` with no `answer` between MUST hold the
  result's `item`, member for member. A tool that checks a file reports
  one that does not, as it reports a `dispatch` after a `result`, a
  `decision` other than an `answer` after one, and a `result` that
  follows no `dispatch` of its call that nothing else ended.
- A writer that names `result` in the header's `records` writes one for
  every call whose output settles before it can join the path, as the
  writer rule above has it, before it appends that output; an output it
  appends as its tool returns needs none. A reader of such a file may
  then take a call that has a `dispatch`, no `result` and no output as
  one whose tool had not returned when the record stopped, since an
  output that had settled would have been appended or recorded.

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
    output on the path, save a withdrawal, below, and SHOULD NOT for one
    with a `dispatch` for it elsewhere in the session, which may have
    run and is ended by `answer`; a reader accepts such a `reject`,
    which 0.9 left as the only way to end that call without a second
    `dispatch`. A
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
    for a call with no output on the path, and with a `dispatch` for it
    on the path or elsewhere in the session or, in a file whose header
    does not name `dispatch` in `records`, without one. A call the record shows never started did not run, and
    a writer that ends one without running it writes `reject`. A call
    whose tool ran and whose result the harness then withheld, such as
    one a hook blocked after the tool returned, is ended by `answer`,
    never by `reject`.

  A call may carry several decisions on the path, in order. An answered
  `hold` is followed by a `proceed`, a `dispatch`, a `reject` or an
  `answer` on the same call and stays as written, a `reject` only when
  no `dispatch` came before it and an `answer` only when one came
  before it on the path, or the session holds one for the call
  elsewhere, or the file does not record dispatches; a call still held is what makes
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
  recorded beside the call, never inside it. The `args` of the last
  decision before a `dispatch` that carries them are what that hand-off
  ran with, and a reader asking what a further hand-off would run with
  reads the last such decision on the path. `args` on an `answer`
  names nothing, since no tool ran with them: a reader asking what the
  tool ran or runs with passes over an `answer`'s `args`, which are
  read from the `answer` alone, so an output the harness wrote never
  reads as a rewrite of the call.

A **withdrawal**, since 0.14, is a `reject` carrying `withdrawn: true`
that directly follows a `dispatch` of its call: the latest `decision`,
`dispatch`, `result` or `function_call_output` of the call on the path before it
is that `dispatch`, and entries of other calls or of no call may stand
between them. A writer that wrote a call's `dispatch` and then did not
hand the call to its tool, as when a subscriber to the hand-off refused
it once the `dispatch` was durable, writes a withdrawal directly after
that `dispatch`, before any output; its `reason` says why. A reader
reads that `dispatch` as a hand-off never made and the withdrawal as no
decision: the call is what it was before the `dispatch`, which for a
call with no other `dispatch` on the path, in a file whose header names
`dispatch` in `records`, is never started, and every rule of this
section and of `dispatch` applies to what follows as though neither
entry were there, save the definition of a withdrawal itself: a later
`reject` carrying `withdrawn` withdraws a `dispatch` only when that
`dispatch` is the call's latest `decision`, `dispatch` or
`function_call_output` before it, never one a withdrawal already
passed over. After `dispatch` D, `dispatch` D' and a withdrawal of D',
a second `reject` carrying `withdrawn` follows a withdrawal, not a
`dispatch`, so it is a `reject` without the member, after D, which
this section forbids. Another `dispatch` may follow a withdrawal, as
may any decision the call could take before the withdrawn one, or its
output. A `reject`
is the verdict so that a reader that does not know the member reads the
call as one that did not run, never as one in flight. A writer MUST NOT
write `withdrawn` anywhere else: a reader reads a `reject` carrying it
that follows no `dispatch` of its call directly as a `reject` without
it, which after a `dispatch` is the `reject` this section forbids, and
keeps the member on another verdict, or holding anything but `true`, as
a member it does not know.

A `reject`, a `hold` or a `proceed` MUST NOT follow a `result` of its
call, since 0.15; an `answer` may, as `result` says.

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
- `deliver` MAY be `true` on a `queued` entry whose `mode` is `steer`,
  since 0.15: the input arrived on its own time, as a background task's
  result does, and must reach the model whether or not a run is in
  flight to take it. It joins a run in flight as any steer does. When
  no run takes it, the harness starts a run that does, once the run in
  flight has ended, unless calls are pending, when the input waits for
  the resume. A `queued` entry carrying `deliver` that is owed when a
  harness resumes the path is handed back as a delivery, and starts a
  run when nothing else does, where an owed steer waits for the next
  input. A writer that writes the entry again after a run end, as the
  inbox rule below asks, writes `deliver` with it. A writer MUST NOT
  write `deliver` with `mode: "followup"`, nor with any value but
  `true`; a reader keeps such a member as one it does not know. The
  `item` that drains the entry carries nothing new: its `queued_from`
  names the entry, which says how the item arrived.
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
`label: null` naming the same target clears it. A `leaf` label's
`target` may be `null`, naming the universal root: the head at the
empty context. No other label names the root. A `label` carrying
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
 "workspace":{"kind":"local|container|remote","ref":"…","host":"…","instance":"…"}}
```

`workspace` says which file system `cwd` is a path in. `kind` is
closed. `ref` names what the file system was made from or is reached
by: a container's image digest, which SHOULD be a digest rather than a
tag, because a tag moves, or a remote runtime's endpoint. It is absent
for `local`. Since 0.14, `host` names the machine the file system is
on, as the harness names it (a hostname, a node name), and `instance`
names the file system among those that `kind`, `ref` and `host`
describe: a container's or a sandbox's ID, since a restart or a
reschedule from the same image is another file system with the same
digest. `ref`, `host` and `instance` are each a string, opaque to a
reader. A writer SHOULD write `host` whenever another machine could
hold a file system with the same `kind` and `ref`, every `local`
workspace among them, and SHOULD write `instance` whenever a restart
gives a new file system. A
local run MAY omit `workspace`, but a writer that records `env` for a
session that may be resumed elsewhere writes it for a local run too
(`kind: local` and `host`). Anything else that tells one file system
from another goes in a member of `workspace` this document does not
define, and not beside it in the entry: the substitution rule below
compares `workspace` alone, every member of it. The envelope section
says a rewriter preserves such members.

An `env` entry applies from its position on the path until the next
one, and it is a whole snapshot. A member it leaves out is not carried
from an earlier entry. It is absent from that entry on: for `workspace`
that is the absent workspace, which the substitution rule below
compares; for every other member it means the entry does not say. A
writer that records a change to one member, such as a new `cwd`,
SHOULD repeat every other member it still knows to hold, `vcs` and
`workspace` among them. The header's `cwd` names where the session was
created, and a reader MAY take it as the working directory only before
the first `env` entry on the path.

A file of a minor before 0.14 was free to name the host in `ref`, or
in a member of `workspace` of its own choosing. A reader reads such a
`ref` as written, and reads `host` or `instance` holding a string as
this section defines them, as the conformance fixtures of 0.7 to 0.13
already write them; one holding another form is a member the reader
does not know, and is preserved as one. Neither reading changes what the
substitution rule compares, so no earlier file reads differently. A
path whose later `env` entry moves the host from `ref` to `host`
records a substitution, as any change of spelling does.

`vcs` names a revision and whether the tree differs from it, so two
different dirty trees on one revision compare equal. `vcs.revision`
SHOULD be the version control system's full identifier for the
revision. A writer that knows only an abbreviation writes it as known.
A reader resolves it as that system resolves an abbreviation, and
treats one it cannot resolve uniquely as unknown. A reader comparing
two revisions compares what they resolve to. A harness that
needs to tell them apart, as one that restores files to a checkpoint
does, records the tree's identity in a namespaced member, beside the
others in the entry (`"cline:tree"`) or inside `vcs`. The envelope
section says a rewriter preserves either.

Before the first `env` entry on a path the workspace is absent. An
`env` entry after a `response` on the path whose `workspace` differs
from the one in force before it, the absent one included, is a
**substitution**: from that entry on, the tools ran against another
file system than the path recorded until then, as when a session
recorded in a container is resumed on a laptop, or one recorded with no
`env` entry is resumed in a container. An `env` entry before any
`response` on the path is not one, whatever it names, since nothing was
recorded yet to hold fixed; nor is one after a `response` that names
the workspace in force, a local run's absent one included.
Two `workspace` members are compared member by member in their
canonical form, as the entry hash writes them, every member this
document does not define included, and an absent one equals only
another absent one, an empty `kind` or `ref` being absent. A change to
any member other than `workspace` in the same workspace, those this
document does not define included, is not a substitution: a new `cwd`,
`vcs` revision, file list or tree identity is not. Recording the
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
 "rel":"subsession|peer|fork_of|continued_in|judged_by","session":"…","call_id":"…",
 "target":"entry-id","url":"…","task":"…","context":"…"}
{"type":"link","id":"…","parent":"…","ts":"…",
 "rel":"subsession","session":"…","call_id":"…","target":"link-entry-id","cancel":"sent"}
```

`call_id` ties a subsession to the function call that spawned it. A
`subsession` link naming a session of this store, or one derived for
it, SHOULD be written when the call is dispatched,
before the `dispatch` entry and before the child's header exists, so a
link whose session cannot be found means the child never started
rather than a child that was never linked.

A `subsession` link and the child's header are two records of one
spawn, written by the parent and by the child, and they agree: the
named session's header, when it exists, has `parent_session` equal to
the linking session's `id` and `spawned_by` equal to the link's
`call_id`. This is the exception to `parent_session` being provenance
alone. A verifier that can read the named session MAY check it, and a
session it cannot find is not a failure, since the link says the child
never started. A link on a fork's prefix was the origin's, checked
there against the origin's `id`. The check is what finds a line moved
to a fork after a `call_id` collision (RFC 0002) with a level of its
subsessions left under the old IDs: two stores derive one ID for the
subsession of one call, so such a link names the other store's
subagent, and only that session's header says whose it is.

`judged_by` is written into the session that was judged and names the
session of the judge, with an optional `target` naming the entry the
judgement is about, the entry the judge's `outcome` names. A judge is a
session of its own, since the agent that scores another's work holds a
conversation with a model, and its header says only which session it
was derived from: `parent_session` holds a judge, a subagent and a fork
alike, and cannot say which. The link can. A reader selecting the
judges of a run reads the `judged_by` links on its path, and `target`
tells the several judgements a session may hold apart. On this
relation `target` names an entry on a path in the judged session, which
a store does not check, as it does not check an `outcome`'s; it is
defined for it and for a cancel notice, below, alone. A `judged_by` link's `call_id` means nothing, and the
checks below are the `subsession` link's alone.

`continued_in` is written into a session its writer has rolled over
into a successor, and names the successor; the successor's header
names this session in `parent_session`. A session holds at most one: a
writer MUST NOT continue a session that holds one, and continues the
session at the end of the chain instead (RFC 0002, Refs and
continuation).

A call's callee may run in another system, reached over a protocol
such as A2A, rather than as a session this store holds. Which relation
links it follows the callee's lifetime, not where it runs or whether it
publishes a record. A callee the caller started for the call, whose
work the caller ends, or asks to stop, when it gives the call up, is a
`subsession`, here or elsewhere. A callee whose work outlives the call,
which the caller does not stop when it gives the call up and which
keeps its own record, is a `peer`. A `peer` link is written into the
caller's session, carries the `call_id` of the call it answers, and is
a record entry like any link. From a `peer` link a reader may conclude
that the call's output came from that callee's work, and nothing more:
not that the work can be replayed, since its record is the peer's, and
not that it ended with the call, since a peer's work may go on after
the caller's run ends.

A `subsession` or `peer` link that names a callee in another system
carries `url`, the endpoint the call was sent to as the caller resolved
it, never a name the callee's own description claims for itself;
`task`, the callee's ID for the unit of work the call started; and
`context`, the callee's ID for the conversation that work belongs to,
when the protocol has one. A callee may answer without starting a unit
of work, as an A2A server answers `message/send` with a message: the
link then carries `url`, and `context` when the answer names one, and
no `task`, and is written when the answer arrives. `task` and `context`
are written only beside `url`. `session` is the callee's own session ID
when it publishes one, and otherwise the empty string; a `peer` link
MUST carry `call_id` and a `url` or a non-empty `session`. The members
are defined for these two relations alone: on a link of another
relation, as anything but a non-empty string, or (`task`, `context`)
with no `url` beside them, a reader keeps them as written, as members
it does not know. Such a callee names its task only when it answers, so
the link is written as soon as the caller learns the task, after the
call's `dispatch`; a hand-off with no such link after it reached no
work the caller learned of. A link belongs to a `dispatch` of its call:
one carrying `url` to the latest `dispatch` of its call before it on
the path, and an in-store subsession's, written before the hand-off it
starts, to the first after it. A call dispatched again, after a restart
or by the rule that replays a call, starts new work at the callee, and a
later link of the call that is not a cancel notice belongs to that later
`dispatch`. A reader tells a call's callees apart by the link's entry
and the `dispatch` it belongs to, not by `task`, which a callee that
answered with a message has none of. A callee answered the call when the
call's output follows its link, the link belongs to the call's last
`dispatch` before the output, no cancel notice names the link, and no
`answer` decision of the call comes between the link and the output,
which would make the output the harness's; a reader concludes nothing
of a link that is not so. A peer's later report, delivered after the
call's output, is an `item` whose `from_link` names the link. The header check above is the in-store
subsession's alone: a callee in another system did not derive its
session from this one, so a verifier does not check a link carrying
`url` against it. A link without `url` is checked as it was, in a file
of any minor.

A caller that gives up a call whose callee it linked, and tells the
callee it no longer wants the work, MAY record that it did by a cancel
notice: a later link of the same `rel` and `call_id` carrying `cancel:
"sent"` and `target`, the ID of the link entry, on the same path, of the
callee the cancel was sent to. That `target` names an entry of this
session, as a `dispatch`'s does: migration and redaction rewrite it
with the other entry-naming members, and whoever copies or re-points a
call's links, onto a new branch or into another store by exchange,
re-points the notice with them. It records that the caller asked, not
that the callee stopped: the caller does not wait for the callee's
answer, so it knows nothing more, and the callee's work may still have
run to its end. A cancel notice names no new callee and need not repeat
the members of the link it names. `cancel` is defined for the
`subsession` and `peer` relations, with the value `sent` and a `target`
beside it; any other value, one with no `target`, or the member on
another relation, a reader keeps as written. A writer MUST NOT append a
`peer` link with no `call_id` or with neither `url` nor `session`, or a
cancel notice whose `target` is not a `subsession` or `peer` link of
the same `rel` and `call_id` before it on its path. A reader reads
either as written and does not refuse the file; a verifier reports the
ones that carry a member this section defines (`url`, or `cancel` and
its `target`), and holds a link that carries none of them, which an
earlier file may hold under an extension's meaning, to nothing new.

A reader of 0.12 reads `url`, `task`, `context` and `cancel` on a
`subsession` or `peer` link of a file of any minor as this section
defines them, since a link does not say which minor wrote it; an
earlier file that held one of those names under an extension's meaning
is read as though a writer of 0.12 had written it, which is the one
exception to such a member being kept as one the reader does not know.
A verifier's rules turn on the members a link carries, not on the
file's header, so raising a file's `format` never makes an entry that
verified before fail.

A link names a session, not a point in one, and at the moment it is
written there is no point to name. The other half of the round trip is
`parents`: the entry carrying the child's `function_call_output`
SHOULD name the child's leaf there, which is the first moment the
parent knows it. Without that, a child that branched leaves no record
of which of its leaves the parent actually took the answer from, and a
projection has to guess.

**Cost across sessions.** A reader that reports what a path cost,
including the work it delegated, SHOULD add to the path's own cost the
cost of each session that a `subsession` link on the path names, the
prefix included and cancel notices excepted. A session that several
links name is counted once. A child's cost is the cost of the child's
path to the leaf named in `parents` by the entry that carries the
call's output. When no leaf is named, it is the cost of the path to the
leaf a reader resuming the child would take (File; in a store, its
head). When the links naming one child count it at different leaves,
its cost is that of the union of those paths, each entry once, so the
prefix they share is counted once. The child's cost is computed by this same rule, so it includes
the child's own children. Each model call is priced as the reader
prices the path's own calls, at the model that served it in the child
and not at the parent's.

A reader MUST say when it could not read or price a child. It leaves
that child out rather than guess, and it MUST say which leaf it took
for a child that no `parents` names. A `subsession` callee in another
system whose session the reader does not hold is delegated work it
cannot count: it MUST say so, apart from a child it could not read.
Some sessions are not this path's delegated work, and this rule does
not count them: a `peer` callee, whose record is its own, and a session
named by `judged_by` or `fork_of`. A reader that counts one of these
says so. Token totals and the count
of model calls stay the path's own.

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

A custom entry whose `data` is an object with a top-level `usage`
member in the payload profile's usage shape (Open Responses `usage`)
records a model call the path paid for that produced no `response`
entry: a fold whose summary was refused, with the usage of its calls
summed, or a call whose answer earned no entry in context. An optional
top-level `model` names the model billed; without one, the model in
force from the `config` entries applies. An optional top-level
`attempts` beside `usage`, since 0.14, is the number of model calls the
usage sums, a positive integer with `response`'s meaning; absent, or in
another form, it is 1. A reader computing what the path cost counts
such an entry as `attempts` model calls, and the ATIF projection's
`final_metrics` do. The rest of `data` is the writer's. A writer that records a paid call under
another member name records it for itself alone.

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

1. Walk `parent` links from the leaf up to the universal root, where the
   path ends; reverse to root-first. A leaf at the root gives an empty
   path, the empty context.
   `parents` is provenance and MUST NOT be walked.
2. Replay `config` entries along the path in order to produce settings,
   honouring `replace`. A `config` that carries `instructions_parts`
   resolves them against the parts in force, as that member defines,
   and the settings' instructions are the resolved parts joined with
   one blank line. The list of omitted parts in force replays beside
   them, as `instructions_omitted` defines, each `keep` in it resolved
   against the list in force before its entry, or against the list an
   earlier entry put in force when it carries `of`, and reaches no
   request. The `omit` in force replays beside them, as the section on
   items left out defines.
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
   `dispatch`, `result`, `decision`, `queued`, `label`, `info`, `env`,
   `outcome`, `link`, `custom`) and every unknown extension
   contributes nothing. A `queued` entry holds an item and is not in
   context: the input enters when it is appended as an `item`. An
   `item` entry that the `omit` in force at the end of the path leaves
   out contributes nothing either; it stays on the path, and a reader
   reports that it was left out and why. Then the `omit` in force sends
   the outputs its `outputs` lists, and the image parts its `images`
   reaches, as their placeholders, among the items of `item` entries
   that remain, as the section on items left out defines; the entries
   keep what they hold, and a reader reports which items it sent so.
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
  the model, with one exception, below: in a sidecar session the input
  holds each `sidecar:` URL as the item records it.
- The object is serialised with the JSON Canonicalization Scheme (RFC
  8785): members sorted by UTF-16 code units, no insignificant
  whitespace, numbers and strings in their canonical forms.
- The value is `sha256:` followed by the lowercase hexadecimal SHA-256
  of the canonical bytes.

A writer that records `request_hash` MUST compute it this way. A reader
MAY verify it by rebuilding the request from the path and comparing.

A writer that did not itself send the request it records, a converter
among them, writes `request_hash` on a `response` only when it has
checked the request the path rebuilds against a record of the request
the source sent. It compares them in the source provider's form: the
rebuilt request translated as a harness would send it to that
provider. Since 0.17 it MUST NOT write one it did not so check. A hash
computed from the path alone verifies by construction and says nothing
about what was sent. A response whose request could not be checked, or
did not match, carries no `request_hash`, and a converter that reports
on the conversion says which and why. The check is about the record's
fidelity, not whether the request could be sent again: a request that
matches the source's record may still be one a provider would refuse.

In a sidecar session, since 0.14, the canonical request is built from
items as recorded, `sidecar:` URLs included, and `request_hash` covers
that form, which is the only form a reader can rebuild without the
blobs. A writer resolves each `sidecar:` URL to its blob's bytes before
it sends the request, as the payload profile carries media, and MUST
NOT send a `sidecar:` URL to a provider, which cannot resolve it. Since
0.16 it resolves one to a data URL of the blob's media type, as the
store holding the blob returns it (RFC 0002, Media), or as the type
file beside the blob gives it (File), and of `application/octet-stream`
for a blob with none. A
reader that resolves the references, to replay a call against a model,
resolves them after it rebuilds and hashes the request. Converting an
item's media between the two forms is the writer's, before it records
the item: a store MUST NOT, as RFC 0002 says, since the item's bytes
are hashed.

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
- Writers SHOULD fsync at least on each `response` entry, on each
  `result` entry and on each `function_call_output` item, since the
  result and the output are the record that a side effect happened.
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

## Redaction

A redacted entry is an entry a redactor made from entries of another
session, its original, changing what was written after it was written:
a secret replaced, a home directory shortened, an image swapped for a
placeholder, a tool output or a whole run of entries left out. A
redacted session is the session that holds them. Its header carries
`redacted: true`, an `id` of its own and `redacted_from`, which names
the original. The original is unchanged. It is usually absent wherever
the redacted session travels, but a holder of it can resolve the link,
and that is where what is learned about a published copy finds its way
back to.

A redacted session is a session like any other. It grows as the
redactor redacts what its original appends, and it may be resumed: an
entry a writer appends to it natively, as to any session, is not
redacted. Every redacted entry says so in its `redacted` member and no
other entry carries one, so a reader always knows which entries a
redaction made and which it did not.

This section fixes what makes redacted entries valid and linked, not
how to redact. Which values are replaced and with what, which entries
are left out or merged, and what a redaction records about itself are
the redactor's, and redactions of different kinds may be made of one
original. A converter's list of identity members (Conformance) names
values a redactor of its output looks for.

### Redacted entries

`redacted` is a body member any entry may carry, as `legacy_id` is. It
lists the entries of the original, by position in the original's file
order, prefix included and counting from 1, that the entry was made
from. Each element is a position `n` or an inclusive range
`[from, to]` with `from` below `to`, in ascending order and not
overlapping. A projection's synthetic leaf marker (RFC 0002) belongs to
the projection, not the session, and is not counted. Sessions are
append-only, so a position names one entry of the original for good,
and it says nothing of that entry's content.

- An entry made from one entry of the original carries `[n]`.
- A placeholder for a run of entries the redaction left out carries the
  run, `[[from, to]]`, and says in its own body, as the redactor
  chooses, that a block was redacted. An entry merged from several
  carries each of them.
- An entry the redaction added, made from none, carries `[]`.
- An entry of the original may be named by more than one redacted
  entry, when a redaction split it.

A writer MUST write `redacted` on every entry it makes by redaction and
MUST NOT write it on any other. A holder of the original maps each
redacted entry to the entries it was made from by its positions, and a
redactor resuming its work finds what it has done in them. A position
counts in the file order of the original named by the `redacted_from`
of the session whose own entry it is: for a redacted session that is a
fork, its prefix is another session's own entries, and their positions
count in the original of the session `parent_session` names. A
redactor MAY instead write a fork's redaction without a base, its
prefix redacted into entries of its own.

### Every redacted entry

A redactor:

- MUST write a valid session: every entry valid for its type, every
  `parent` an entry the file holds or null, and the base rule held.
  When it leaves an entry out, the entries under it hang from what
  stands in that entry's place, and a member that named it names that
  or is removed, member by member as Leaving entries out says, so that
  no member names an entry of this file the file does not hold;
- MUST compute every content hash and `id` over what it wrote, and
  rewrite `parent` and every entry-naming member to the IDs of the
  entries it wrote — `parents` into this file, `target`, `first_kept`,
  `from`, `queued_from`, `from_link`, `omit`'s `items`, `outputs`'
  `items`, a `groups` element's `items` and `images`' `before` in a
  `config` and in a compaction's
  checkpoint, and `instructions_omitted`'s `of` where it
  is defined — and the header's `base`. A member of one of those names
  in a form or place this document does not define there is body text,
  not a reference;
- MUST keep instructions whole: where it changes a part's text, the
  later parts that name that text by `hash` name it by the hash of the
  new text, and instructions that were the join of their parts stay the
  join of the rewritten ones. A redactor that changes text across a
  part boundary breaks the join and MUST NOT;
- MUST NOT write an instruction part, or an omitted part's `of`, under
  a hash taken from the original whose text, or entry, it cannot
  resolve on the path: a part named by a `hash` whose text is in no
  entry before it that the redactor redacted, or an `of` naming an
  entry it did not write. Such a hash is the original's, which neither
  verifies in the redacted session nor hides the text. It writes the
  part as text of its own instead, such as fixed placeholder text, the
  part's form changing from a `hash` to its text, or it does not redact
  the entry. A part so written changes the request the path rebuilds,
  so the `response` entries after it carry no `request_hash` or the one
  the redacted session rebuilds, as below;
- MUST keep an entry's `legacy_id` as written, since 0.17 a rule and
  before it what writers did, so output named by the original's earlier
  IDs still resolves in the redacted session;
- MUST treat a `normalised` record as holding the member it describes:
  `was` is that member's original source text and `raw` its original
  bytes, so what the redactor removes from the member it removes from
  the record too, decoding `raw` to find it;
- MUST keep media in its form: an inline item stays inline and a sidecar
  item stays sidecar. A sidecar blob it replaces is stored under the
  replacement's hash, with the replacement's media type, and the item's
  `sidecar:` URL names that;
- MUST leave a reference into another session as it was, since it names
  that session's original entries, unless it holds that session's
  redaction, made by the same redaction. Then it rewrites the reference
  to it: `parent_session`, a `parents` entry into that session, a
  `link`'s `session` and an item's `copied_from`. A redactor MUST NOT
  remove `copied_from` from a copied call or its output, since without
  it the call reads as this session's and fails the record rules. When
  it changes an item that carries one and does not hold the source
  session's redaction, made by the same redaction, it removes the
  member's entry, which then no longer names an equal item, and keeps
  its session; when it holds it, the entry named was changed alike,
  and it rewrites both members to that redaction, as above. A
  link's `target` names an entry of this session,
  so it is rewritten as the members above are. A redaction of a whole
  store holds a redaction of every session in it, and so re-points
  every reference that stays inside the store;
- MUST reach data the format keeps in member names: a member name is
  data, and a redactor reaches it as it reaches a string value, in the
  objects keyed by data, an `env` entry's `files` maps and `tools` map,
  and at any depth in every object whose members this document does not
  define: a `custom` entry's `data`, an `outcome`'s `details`, a
  `decision`'s `args`, the value of each member of a `config`'s `extra`
  and a compaction checkpoint's, the value of any member this document
  does not define, and the body of an extension entry, its own member
  names among them but the envelope's. In the objects this document
  defines, an entry, a `trigger` or a `workspace` among them, member
  names are the format's, and a redactor leaves them as they are, the
  name of a member this document does not define included, whose value
  is data; so are the names inside an item, which the payload profile
  defines. The names this document defines inside data are the
  format's too: `usage`, `model` and `attempts` at the top level of a
  `custom` entry's `data`, and the names inside that `usage`, which the
  payload profile defines, so a redaction does not change how many
  model calls the entry records; and the names of `extra`'s members,
  which are request member names the provider reads. Two names of
  one object that a redaction makes one MUST NOT both be kept, nor an
  extension entry's name made one its envelope holds;
- MUST keep `trust` as written, since 0.15: its labels are the
  format's vocabulary, not text. An entry merged from several carries
  the union of theirs. A `compaction`'s or `branch_summary`'s `trust`
  is kept as written even when the entries behind it are left out,
  which is why it may be wider than the union a reader recomputes, and
  a redactor MUST NOT narrow it. A redactor likewise keeps a `source`
  value this document defines (`config`, Instructions as parts) as
  written, since 0.17, on an instructions part, an omitted part and an
  `item`'s `instructions_part`; any other `source`, and every `id`, is
  text;
- MUST give each `response` it writes either no `request_hash` or the
  one the redacted session rebuilds for it, computed as Request hash
  says, and never another: a redacted session hashes independently, and
  a hash kept from the original would neither verify nor hide what was
  redacted from it. It SHOULD give one to a response made from entries
  that carried one, and MUST NOT give one to a response made from an
  entry whose `request_hash` did not verify in the original, so that a
  redaction never certifies a request the original could not.

Some strings tie entries together: a `call_id` pairs a call with its
output, dispatch and links, and a subsession's ID is derived from its
parent's. A redactor that changes one changes every one in step, and
across sessions redacted alongside, or the pairs no longer hold. Inline
media is a data URL whose payload is base64, so text inside an image's
bytes is reached only by decoding it.

A redacted session walks and verifies against itself as any session
does: its IDs and every `request_hash` are its own. A reader MUST NOT
report its hashes as verifying the original, and holds a redacted entry
to every rule it holds any entry to, its `request_hash` among them, so
a `request_hash` that does not match is a fault in a redacted session as
in any other.

### Leaving entries out

A redactor that leaves an entry of the original out writes nothing for
it, or a placeholder: an entry of its choosing, such as a `custom`
entry saying that a block was redacted, which stands for a run of
left-out entries on one path and carries their positions in
`redacted`. A placeholder for one left-out entry carries `[n]`, since a
range needs `from` below `to`. A position records what an entry was
made from, not how: `[n]` on a placeholder and on an entry rewritten
from entry `n` are one mark, and the entry's own body says which it is.

What **stands in the place** of a left-out entry is the placeholder
whose `redacted` covers its position on that path, when the redactor
wrote one, and otherwise the nearest entry above it on its path that
the redaction wrote, or the universal root when it wrote none. In a
file with a `base` the base is written and above every own entry, so
an own entry's left-out ancestors are replaced by an own entry or the
base, never the root, which the base rule forbids an own entry to hang
from. A member
that named the left-out entry then follows the rule for its kind:

- A member naming one predecessor names what stands in the left-out
  entry's place: `parent`; the `target` of a `label`, an `outcome` and a
  `judged_by` link; a `branch_summary`'s `from`;
  and `instructions_omitted`'s `of`. A compaction's `first_kept` and
  `images`' `before` bound a range from below and have a place of their
  own, below. A redactor MUST NOT remove such a member. Where the
  universal root stands in the place and the member may not name it,
  since only `parent` and a `leaf` label's `target` do, the redactor
  leaves the entry holding the member out too, or does not redact. What
  stands in the place of the `config` entry an `of` names holds no
  omitted list, so its `keep` then names no part a reader can rebuild,
  as `of` says; a redactor that would keep those parts writes them
  whole instead.
- A member that lists entries, or records where an item came from,
  loses the left-out entry: a `parents` element and an ID in `omit`'s
  `items`, in `outputs`' `items` and in a `groups` element's `items`, in
  a `config` and in a compaction's checkpoint, are removed from their
  lists, and an `item`'s
  `queued_from` and `from_link`, provenance that names one entry and
  reaches no request, are removed whole. A list so emptied is
  removed, and an `omit` left with no member is removed with it, since
  an empty `omit` in a `config` clears what is in force rather than
  adding nothing. An `outputs` whose `items` is removed keeps its
  `text`, which it still sets. A `groups` element whose `items` is
  emptied is removed, since a named group sets nothing else, a `groups`
  so emptied is removed, and an `outputs` left with no member is
  removed, and its `omit` with it when that leaves the `omit` with none.
- An entry of a call goes with the call: a redactor that leaves out a
  `function_call`'s entry leaves out every `dispatch`, `decision`,
  `result`, `function_call_output` and `link` entry of that call, the links that
  carry its `call_id`, a withdrawal, an output marked `failed` and a
  cancel notice among them, and, since 0.15, every `custom` entry whose
  `call_id` names it, since each says what became of a call the file
  no longer holds. A withdrawal also goes with the `dispatch` it
  follows, which it would otherwise not follow, and a cancel notice
  with the link its `target` names: neither a placeholder nor an entry
  above the link is a link, and `link` forbids a notice naming one, so
  a notice's `target` is never re-pointed to what stands in a link's
  place.
- A member that bounds a range from below, a compaction's `first_kept`
  and `images`' `before` in a `config` and in a compaction's
  checkpoint, names the entry it bounds from and none above it: the
  entries above `first_kept` were folded, and the images above `before`
  were sent as placeholders, so naming one would put it back into every
  later request. When the redaction leaves the named entry out, what
  stands in its place is the placeholder covering it or, with none, the
  first entry the redaction wrote after it on its path, before the
  compaction for `first_kept`. `first_kept` names an entry before the
  compaction on its path, as `compaction` says, so a redaction that
  leaves out every entry from `first_kept` up to the compaction MUST
  write a placeholder for them, for `first_kept` to name. A `before`
  with nothing written after it names nothing on the path, and the
  redactor leaves the member's `images` out or does not redact.
- A `run` end whose segment the redaction changed MUST carry the
  `reason` and `pending` its redacted segment reads, computed as `run`
  says, unless its `reason` is a written value, `error` or
  `interrupted`, which stands; or the redaction leaves the run end out.
  `pending` loses every call the redaction left out, as a list does.
- A `queued` entry goes with the `item` that drained it: a redactor that
  leaves out an `item` entry whose `queued_from` names a `queued` entry
  leaves out that `queued` entry too, since a `queued` entry no `item`
  names reads as an input the harness still owes. Leaving out the
  `queued` entry and keeping the `item` removes the item's
  `queued_from`, as above.
- A `result`'s `item` is a copy of the call's output. A redactor that
  removes or replaces an output, or any part of it, does the same to
  the `result` of the same hand-off, and leaves out the `result` when
  it leaves out the output.
- A compaction's `pinned` items are copies of items of entries before
  `first_kept`, and a copy says what its source says. A redactor that
  removes or replaces a pin's source, the item or any part of it, MUST
  do the same to the pin, and one that leaves out every entry a pin was
  copied from removes the pin.

Leaving an entry out never leaves its children without a parent, and
the members of the entries it keeps name only entries it wrote, so a
redacted session verifies as any session does.

### The redacted session

`redacted_from` names the original's `session`, a non-empty string
other than the redacted session's own `id`, and, as `profile`, which a
redactor SHOULD write, the redaction: its kind and its rules, at a
version, so that two redactions of one original are told apart; it is a
non-empty string. A redaction records what else it wants to say about
itself in namespaced members of `redacted_from`. A reader keeps as
written a member of `redacted_from` it does not know, namespaced or
not, and `session` or `profile` in another form, as it does any member
it does not know; the refusal of `redacted_from` without `redacted`
binds the object that carries `session` as a non-empty string.

The redacted session's `id` is new, since its entries are not the
original's and a store or dataset may hold both. A redactor that writes
a `profile` SHOULD derive the ID as a UUIDv5 under the nil namespace
over the name `["redacted","<original id>","<profile>"]`, written in
the canonical form the entry hash uses. An array cannot be read two
ways, as a `/`-joined name can when an ID holds a `/`, and it is not a
subsession's name, which starts with a session ID, unless a session's
ID is itself such an array. One original under one profile then has one
redacted session, whose ID stays as it grows, and repeating a redaction
over a store finds the session it made before and appends only what the
original added since. Two sets of rules under one profile write into
one session; a profile names one set.

A redactor writes the redacted session's header from the original's,
member by member, since 0.14 in these words:

- `cwd`, `harness` and every member this document does not define are
  text, and a redactor reaches them as it reaches an entry's body text,
  at any depth.
- `type`, `format`, `created_at`, `payload`, `records`, `media`,
  `redacted` and `redacted_from` are the redaction's to write: `type`
  and `format` as any writer does, `redacted` and `redacted_from` as
  this section says, and the rest as the redactor chooses, `media` in
  the original's form, as Every redacted entry requires of the media
  itself. No text rule reaches them.
- `id` is derived as below.
- `base` and `parent_session` are references, under the rules for
  references above: `base` names an entry this file holds, and
  `parent_session` stays as written unless the redaction holds that
  session's redaction.
- `spawned_by` is a `call_id`, which changes in step with the call IDs:
  a redactor that changes the call IDs of the parent session writes the
  one it gave the call that spawned this one, and otherwise leaves it as
  written.

A fork of a redacted session is a session of its own. Its header carries
`redacted` only when a redactor made it, and its prefix entries carry
their `redacted` as they did.

### What a kept hash reveals

An original hash a redacted session keeps lets anyone who can guess a
redacted value confirm the guess, by putting it back and recomputing
the hash. That matters for a value with few possibilities: a username,
a home directory, an email address. It does not matter for a random
key. A redacted session's own hashes are computed over what it holds,
its `request_hash` values among them, and `redacted` names entries by
position rather than by hash, so the one original hash it can keep is a
reference into a session not redacted alongside it; a redactor that
redacts those sessions too keeps none. Redaction alone does not hide a
value that can be guessed.

## Projections

### ATIF

One ATIF document per path from a first entry to a leaf. `session_id` is the header ID;
`trajectory_id` is the leaf entry ID. A document from a redacted session carries
`redacted: true` and `redacted_from`, as the header holds it, under
`agentsession` in its top-level `extra`, so a published document names
its original as the session file does, and a step made from a redacted
entry carries that entry's `redacted` under `agentsession` in the
step's `extra`, so a reader tells redacted steps from steps appended
natively. A step made from several entries — an agent step, from its
items, its response and the outputs observed in it — carries under
`redacted_entries` there an object from each redacted entry's `id` to
its `redacted`. The mapping is given in the
accompanying session plan and is normative for the Open Responses
profile: user and system items to steps, one `response` with its items
to one agent step with `tool_calls`, `reasoning_content` and `metrics`,
its `llm_call_count` the response's `attempts`, 1 when absent,
function call outputs to observations by `source_call_id`, compaction
and branch summaries as copied-context system steps, `link` entries to
`subagent_trajectories`. Where the output entry carries `parents`, the
reference into the child session names the leaf the answer was taken
from; where it does not, the projection takes the leaf a reader
resuming the child would take, as `link` counts a child's cost, and the
reference says the leaf was not named, since the document then depends
on that choice rather than on the record. `run`,
`dispatch`, `result`, `decision` and `queued` entries have no step of their own.
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
the projection, and a withdrawal's record carries `withdrawn: true`. A
call's `result` travels there as `result`, holding its `entry_id`, its
`ts` and `failed`, and its `item` when the document holds no output
for the call, so a document cut before the append still says the tool
returned. A
`custom` entry whose `call_id` names a call whose agent step is in the
document travels in that step's `extra`, under `calls[call_id].custom`,
as a list in path order, since the step after it may be the model's
next turn or another call's; one naming no call in the document
travels, as every other `custom` entry does, in the list `custom` of
the next step's `extra`, or of the top-level `extra` when no step
follows. A queued
input travels as a list under `queued`, a delivered input's record
carrying `deliver: true`, and the step of the item that
drained one carries its trigger under `source`; a fold's usage
travels under `usage` in its system step's `extra`, and a fold's
`pinned` items travel beside its `summary` in the same step, since the
entries they were copied from are before `first_kept` and so are not
in the document. Raw items travel in step `extra` so the projection is
lossless.

A member of an entry that this document does not define travels as
written, in the `extra` of the step, observation result or record the
entry maps to.

Since 0.17 a document carries, under `agentsession` in its top-level
`extra`, `entries`: an object from the `id` of each entry the document
maps, a step's, an observation result's and a record's, to that entry's
envelope and history members as the entry writes them: `type`, `ts`,
`parent`, `null` for a first entry, `parents` when it has them, and
`legacy_id`, `normalised` and `redacted` when it has them. For an entry
that is not an `item` or a `response`, whose body no step carries
whole, the object also holds the body, every other member, under
`body`. An observation result carries its output's `id` as `entry_id`
under `agentsession` in its `extra`, as a step does. A document of a
redacted session MUST carry `entries`, and any other document MAY. A
document stripped of its raw items, or of an entry's body, cannot be
rebuilt to that entry's `id`.

A converter that rebuilds a session from a document this projection
wrote restores on each entry:

- the meaning members the document carries for it (Entry envelope):
  `failed`, `trust`, `copied_from`, `from_link`, a decision's
  `withdrawn`, a call's `result`, `instructions_part` and the like, as
  core members and not under `agentsession`;
- every member it does not know, as written.

It SHOULD rebuild each entry to the `id` the document names for it
when the document allows it: the body from the members the document
carries for the entry, its raw item and meaning members or its `body`,
and the envelope and history members from `entries`. It MUST compute
each rebuilt entry's `id` (Entry hash) and compare it with the `id` the
document names. An entry whose `id` matches is the exported entry
itself, not a new one, so its history members are true of it and are
restored with it. Any other entry is a new one: the converter MUST NOT
carry a history member on it as its own, as the entry envelope says,
and MAY keep them under its own namespace.

The converter MUST restore the header's `redacted` and `redacted_from`
exactly when every entry it rebuilt matches its `id` and the document
holds the whole path, every entry from a first entry to the leaf with
each `parent` an entry of the document or `null`. A document built
after a compaction holds no entry before the fold's `first_kept`, and
one without `entries` holds no envelope, so neither holds the whole
path. Otherwise it MUST NOT restore them, since the converter is not
the redactor, and it reports why. A file so rebuilt from a document of
a redacted session is a projection of that session and not a
conversion of it: its `id`s are the redacted session's, and it reads as
redacted.

An `item` entry that `omit` leaves out of the request at the trajectory's
leaf is in the step that holds it, since the model produced it or the
person sent it, and the step's `extra` lists it under `omitted` with the
reason, `other_models` or `items`, so a reader sees it as left out and
not as absent. An `item` entry the request at the leaf sends with a
placeholder, by `omit`'s `outputs` or `images`, is in its step, or its
observation result, as recorded, and that `extra` lists it under
`agentsession` as `replaced`, an object with the `entry_id`, the
`reason`, `outputs` or `images`, the placeholder `text` sent, and for
`images` the number of `parts` replaced, one object per member that
reaches it; the item as
sent travels beside the raw item as `sent`, so the projection rebuilds
both. `omitted` and `replaced` describe the leaf's request. Each agent
step's `extra` also lists, under `agentsession`, the item entries its
own request left out, as `request_omitted`, and those it sent with a
placeholder, as `request_replaced`, each with its reason, from the
request context of the step's `response`, since a model switch or a
later `omit` makes the leaf's request a different prompt from the one
an earlier step was sent; each `request_replaced` object carries the
placeholder `text` that request sent, which a later `omit` may change.
Both are absent when empty, and when the projection is built from the
context alone, without the path it was built over, since a step's
request is rebuilt from the path.

An output's entry marked `failed` carries `failed: true` under
`agentsession` in the `extra` of the observation result, or step, it
maps to, and an `item` carrying `from_link` carries it there in its
step's `extra`, so a reader tells a failed call and a peer's late
report from any other. An `item` carrying `instructions_part` carries it
there in its step's `extra` too, so a reader tells a composed layer from
a person's input.

An `item` entry carrying `copied_from` maps to the step its item would
map to, marked `is_copied_context: true`, with no `model_name`, no
`metrics` and no `llm_call_count`, and with `copied_from` under
`agentsession` in the step's `extra`, always a list, in item order,
one element for a step made of one item. Copied model-output items that follow one another
form one step. A copied call is a tool call of that step, and its
output is the step's observation, never a failure of this path. A
copied step is no model call of this path, so neither `final_metrics`
nor `total_steps` counts it. An output carrying `copied_from` whose
call is not copied is an observation of its call's step, as any output
is, with `copied_from` under `agentsession` in the observation
result's `extra`, a list of one.

An entry's `trust` travels under `agentsession` in the `extra` of the
step or observation result it maps to, an agent step carrying the union
of its items', a fold's in its system step's `extra`, and a `result`'s
in its record.

A document's steps are the context the algorithm produces, so a run
that compacted is described by its last summary and what followed it.
Its `final_metrics` are not: they total every model call on the path,
the ones a fold replaced and the ones a `custom` entry's `usage`
records included, and `total_steps` counts those model calls, which a
reader can count in the path and check against the document: a
`response` as its `attempts`, 1 when absent, a `compaction` and a
`branch_summary` one each, and a `custom` entry carrying `usage` as
the `attempts` beside it, 1 when absent. So `total_steps` is the sum of
the agent steps' `llm_call_count` plus the path's other model calls,
those a fold replaced included. It is not the number of
steps: a user turn is a step and no model call, and a fold's calls are
model calls and no step. Whenever it differs from the number of steps
the root `notes` says so, which is what ATIF requires of a
`total_steps` that is not the number of steps, and when a fold left
model calls out of the steps the note says how many. Without that rule
a cost column reads the tail's cost as the run's, and an agent that
folded eleven times outranks one that did not.

ATIF defines `total_cost_usd` to include subagents. The root's
`total_cost_usd` is the path's cost plus the cost of the work it
delegated, counted as `link` says of a reader that reports cost: each
child once, over the union of the paths its links name, whether or not
the document embeds one trajectory of it or several. The root `notes`
says so, and says how many children it counted at a resume leaf. The
token totals and `total_steps` stay the path's, as ATIF defines them
over the steps. A child the projection could not read or price is not
counted, and the notes say that the cost excludes it.

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
before it. A hand-off's span ends at the call's output or, when the run
the hand-off was made in ends first, at that run's end, carrying the
state the path reads for the call there: in flight, with an error
status, unless a `decision` after the `dispatch` holds or answers it;
a hand-off still open when the record stops ends there the same way. A
later `dispatch` for the call or its output opens a span of its own,
linked to the hand-off's; a second hand-off within one run ends the
first in flight at the moment of the second. A withdrawn hand-off's span
ends at its withdrawal, without an error status, since the tool never
ran, and an output marked `failed` ends its call's span with an error
status. A copied call has no tool span: the session that made it
traces it. A hand-off's span ends at its `result` when it has one, in
the state the path reads, since the output's append is not the tool's
time.

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
header.

Adding an optional member to the envelope is a minor change. Changing
what an existing member means, or what the context algorithm does with
any member, is major — which is the line an addition has to stay behind
to arrive in a minor version at all.

The 0.x series is exempt from that rule until the first release. A 0.x
minor MAY change the envelope, the header or the context algorithm, and
a reader of 0.x supports the minors it names rather than every minor of
the major. The guarantee that a reader of a major reads every minor of
it begins at 1.0. A reader of 0.17 reads a 0.5 to 0.16 file
as it stands, since 0.6 to 0.17 add optional members, elements and
relations, 0.15 a record entry type and 0.16 a file beside a sidecar blob and an `omit` member, 0.17 an `item` member and defined `source` values, 0.10, 0.11 and 0.14 relax a rule or two each, and the hashes do not change, save for the few rules each minor's changes below say read
an earlier file differently; a member a later minor defines that an earlier file holds
in another form, which it was free to while the name was undefined, is
a member the reader does not know, and is preserved as one. A reader
of 0.17 MUST read a 0.x file earlier than 0.5 by migrating it in memory: walk the entries in file order, rewrite
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
documents which native entries it maps and which it drops. It also
documents which native members it carries into the session that
identify a person, an account or organisation, a machine or the native
session (an email, an organisation ID, a login name, a home directory,
a host name, a native session ID), and where each lands: the header, or
an entry type and an RFC 6901 JSON Pointer into its body. A converter
that reports on a conversion SHOULD list those members by path and
count, and MUST NOT put their values in the report.

The reference implementation is the Go `agentsession` library, which
writes this draft. The conformance suite is a directory of fixture files
with expected context output for every leaf, expected `request_hash`
values, every entry's content hash and `id` recomputed, 9007199254740993
in three spellings and once in a member the format types as a number, a
lone surrogate normalised with its `was`, output that was not valid
UTF-8 replaced with its `raw`, 2^60 written as its canonical rendering
1152921504606847000 and read back, a forked fixture whose `base` is
found in its origin, an extension entry and a core entry each with a
`parents` element carrying members this document does not define,
committed and read back under the `id` its line holds, an
instructions delta naming runs of parts by `keep` beside the parts it rebuilds, a `response` with `attempts`, an
`env` whose `workspace` holds a host and an instance and a later one
that substitutes another instance, a call dispatched twice under one
`idempotency_key` beside a call answered after a crash, a list of
omitted parts in force across a delta, carried by a compaction's
checkpoint and cleared by `[]`, a queued `trigger` with members of its
own drained into an `item`'s `source`, a list of omitted parts naming
runs of the list in force by `keep` across deltas that move a part
across a budget, beside a `replace` and a compaction's checkpoint that
write it whole, the recomputed `reason` for every `run` end, a call whose `dispatch` a
withdrawal follows after another call's `dispatch`, read as never
started at its run's end and dispatched again by the next, beside an
output marked `failed`, a `run` written `resume`
whose segment holds nothing, beside one that adds a message and is
reported, and runs written `resume` whose segments hold record entries
alone, one refused and one cut, a hand-back naming every instructions part by `hash`, those
out of force among them, and its omitted list by `of`, a two-model session
whose `omit` is written at each switch, every response hashed and
verifying, beside the same session written as 0.10 does, and one whose
`omit` a compaction's checkpoint carries into a switch of model, a `judged_by`
link with its `target`, calls to callees in another system, each linked
after its `dispatch` in a file that promises them: a `peer` that answered
without a task, and a `subsession` call whose first task a cancel notice
names and whose second dispatch's task answered, an `of` naming no `config` entry on the path,
or one before a compaction's checkpoint, kept as written and noted by a
tool that checks a file, a `peer` call acknowledged by its output and
answered turns later by an item whose `from_link` names the link, beside
a `from_link` naming a cancel notice, which is reported, a session that
sends old tool outputs as a placeholder by `omit`'s `outputs` and old
screenshots as one by `images`, in both of its forms, across a
compaction's checkpoint, every response hashed and verifying, a path
with two compactions, the second restating one pin and dropping
another, whose context applies only the last, a `response` with no
`response_id` among `item` entries, which has no output items and whose
request is the whole path before it, a child prompted with its
parent's user message, reasoning and call, each carrying `copied_from`
with its `entry`, and a placeholder output carrying it without one,
which verifies under a header promising `dispatch` and `decision` with
neither written for the copied call, beside a fork that answers a call
in its prefix with an `answer` and an output whose `copied_from` names
the origin's output, a delivered input closed by a run end, queued
again after it carrying `deliver` and drained by the next run, beside
another owed at a cut while a call waits for approval, a parallel batch
cut before its append with two calls returned, one failed, and one in
flight, continued by a resume that appends one result unchanged and
answers the others, a web output marked `untrusted` and an inbox output
marked `private` folded by a compaction carrying both, beside a label
this document does not define, kept as written, a session that sends one
output with the default group's text and two with a group of their own,
written in two deltas, every response hashed and verifying, a
configuration naming two instruction files by path under `source`
`agentsmd`, beside a product prompt, a skill catalogue and a memory
part, an omitted `agentsmd` part, and a message carrying
`instructions_part`, every response hashed and verifying and the
context hash the same session's without the member, `env` snapshots,
a `cwd`-only entry after a response read as a substitution to the
absent workspace and a whole one not, a path whose runs' inputs leave
out a steer that joined a run in flight, copied items, a peer's report,
a composed layer and a resume's note, and keep a followup and a
delivery that starts a run, a parent with two
children, one counted at the leaf its output's `parents` names with a
grandchild of its own and a cancel notice counted once, and one
missing and reported, an ATIF document whose entries carry history
members, meaning members and a member this document does not define,
each where this document puts it, and a redacted session's document
rebuilt to its `id`s with its entries' `redacted` and its header's
`redacted` and `redacted_from` restored, beside the same document
changed after the export and one without its first entry, neither of
which restores them, and
negative cases, each a file broken in one way, for a broken parent link,
a truncated last line, an unknown type, a file with a `base` holding a
second first entry, a branch at the root a file with a base may not have, an own entry hung from the prefix above the base, or a line
before the base that is not on the path to it, a `dispatch` that follows a
`reject`, an `answer` or the call's output, a decision that follows a
`reject`, a `reject` that follows a `dispatch` or an output, a
withdrawal that a decision or the output separates from its `dispatch`,
a `dispatch` or a `reject` naming a copied call, a `copied_from` naming
its own session, a `result` with no `dispatch` before it or after the
call's output, a `dispatch` or a `reject` after a `result`, an output
after a `result` that differs from its `item` with no `answer` between,
a `target`
naming another call, a repeated `call_id`, an empty `call_id`, a `run`
start whose `source` is not the shape of its segment, a response whose
recorded hash is the request an `omit` in force leaves an item out of,
an `outputs` that lists an entry the default group in force holds in a
group of its own, which a reader takes as changing nothing and the
response after it is hashed so, an `instructions_part` on an output,
one with no `source` and one naming `agentsmd` with no `id`, each kept
as a member the reader does not know, and a header
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

## Changes since 0.16

One optional member, four defined values of an existing member, a
class of members, and rules for converters and readers. A 0.16 file is
a 0.17 file that holds no `instructions_part`. No hash changes. The new
member reaches no request, and RFC 0002's context hash leaves it out.
A writer of 0.17 raises every file it appends to, as Versioning says,
so every reader of 0.16 refuses those files until it is upgraded.

- `source` on an instructions part or an omitted part has four defined
  values, `product`, `agentsmd`, `agentskill` and `agentmemory`, naming
  the product prompt, an instruction file, the skill catalogue and
  memory kept between sessions (`config`, Instructions as parts). 0.16
  told readers that every `source` was opaque and suggested these names
  as part ids, and an id names one part, so a reader could not find a
  session's instruction files without knowing its harness. A reader
  reads the values in a file of any minor, as writers already used
  them, and a redaction keeps them as written.
- `item` defines `instructions_part`, `{id, source}`: an item the
  harness composed from an instruction layer and sent as a message. A
  reader of 0.16 read such an item as a user's input. It is a record
  member, removed from RFC 0002's context hash, and kept by a
  redaction, its `id` as text. A reader takes it by its form in a file
  of any minor, as 0.15's `trust`, so an earlier file that already held
  an `instructions_part` in this form under an extension's meaning now
  has its context key move at that entry and after it; no entry hash
  moves.
- `legacy_id`, `normalised` and `redacted` are history members (Entry
  envelope). A writer writes one only where its own writing made it
  true, and a converter does not carry another's as its own. 0.16
  forbade this for `redacted` alone. A member each minor defines is
  classed from now on; `instructions_part` is a meaning member.
- The ATIF projection carries an entry's undefined members as written,
  `instructions_part`, and under `entries` each entry's envelope and
  history members, and the body of each entry that is not an `item` or
  a `response`; a document of a redacted session MUST. A converter
  reading a document back restores meaning members and unknown members.
  It SHOULD rebuild each entry to the document's own `id`, MUST check
  each rebuilt `id`, and keeps history members only on an entry that
  matches. It restores the header's `redacted` and `redacted_from`
  exactly when every entry matches and the document holds the whole
  path, and otherwise reports why.
- A writer that did not send the request it records, a converter among
  them, writes `request_hash` only after checking the rebuilt request
  against the source's record of it (Request hash). 0.16 allowed a hash
  computed from the path alone, which verifies by construction and
  certifies nothing.
- A writer writes `failed` from what the harness or the native record
  says, and never infers it from an output's text or exit status
  (`item`).

Clarified, with no file reading differently:

- An `env` entry is a whole snapshot, and a writer repeats what still
  holds (`env`). The header's `cwd` is the working directory only
  before the first `env` entry.
- `vcs.revision` SHOULD be the full identifier, and a reader resolves
  an abbreviation (`env`).
- A reader that reports cost adds delegated work by `subsession` links
  (`link`), and the ATIF rule points to it.
- A reader that rebuilds a path's inputs leaves out steers that joined a
  run in flight, copied items, peer reports and composed layers, and
  keeps a followup or a delivery that starts a run (`run`).
- A converter documents the identity members it carries and reports
  them without values (Conformance).

## Changes since 0.15

A sidecar blob's media type, and groups of placeholder texts for
outputs. A 0.15 session is a 0.16 session whose blobs have no type and
whose `omit` holds no `groups`. No hash changes. The minor changes the
session's directory, which RFC 0001 counts as part of the session
(File), what a writer does with it, and what an `omit` may hold, so it
is a minor's and not a clarification. A writer of 0.16 raises every file it
appends to, as Versioning says, so every reader of 0.15 refuses those
files until it is upgraded.

- A blob's media type is the file `<hex>.type` beside the blob (File).
  A 0.15 session lost the type when a writer converted a data URL to a
  blob, and a writer resuming it could not rebuild the data URL its
  request must send, since a `sidecar:` URL MUST NOT reach a provider.
  The reference `sidecar:sha256:<hex>` is as it was, and the type is
  outside every hash.
- A writer resolves a `sidecar:` URL to a data URL of the blob's type,
  and of `application/octet-stream` for a blob with none (Request hash).
  A 0.15 writer had no type to resolve one to.
- A reader of 0.15 refuses a 0.16 file by its header, as it does any
  later minor (Versioning), so no reader of 0.15 reads a 0.16 session
  and drops its types, and no writer of 0.15 appends to one and writes
  a blob without its type. A reader of 0.16 given a 0.15 directory finds
  no type file and reads each blob as one with none.
- Redaction: a blob a redactor replaces is stored with the
  replacement's type.
- `omit`'s `outputs` may carry `groups`, each a placeholder text and the
  outputs sent with it (Items left out of the context). A 0.15 `outputs`
  held one text for every output it listed, so a request that sent a
  host's trim and a harness's quarantine under two texts could not be
  described, and every response after it was written without a
  `request_hash`. An entry is in one group at most; a writer refuses an
  `outputs` that would put one in two, and a reader takes one as a form
  this document does not define. A `groups` element's `items` joins the
  members that name an entry, and Redaction says what leaving one out
  does. The default group is read as 0.14 and 0.15 read `outputs`, so
  no earlier file reads differently.

## Changes since 0.14

Optional members, one record entry type, and the rules that go with
them. A 0.14 file is a 0.15 file that holds none of them. No hash
changes. As with 0.12's and 0.14's, the new rules turn on the members
and entries a file holds, not on the header, so a reader of 0.15 reads
each one below by its form in a file of any minor. No writer of an
earlier minor wrote a `result` entry, since a reader of that minor
reported the type. A writer of 0.15 raises every file it appends to, as
Versioning says, so every reader of 0.14 refuses those files until it
is upgraded.

- `item` defines `copied_from`: the item was taken from another
  session's conversation. A copied call is no call of this session,
  and the record rules do not ask it for a `dispatch` or a `decision`,
  which a writer may not write for one. An output for one of this
  session's calls taken from another session names its source, so a
  fork reaches the origin's child. A reader of 0.14 read a child
  prompted with its parent's conversation as having made the parent's
  calls, and exported the parent's turn as the child's. RFC 0002's
  context hash leaves the member out, and its `entry` joins the members
  that name an entry, into another session, which a redaction holding
  that session's redaction rewrites.

- `queued` defines `deliver`: a steer that must reach the model, and
  that a harness starts a run for when no run takes it. A reader of
  0.14 handed an owed delivery back as a steer, which waited for the
  next input.

- A new record entry, `result`, records that a call's tool returned
  before its output joined the path, written when an output settles
  before it can join, as inside a parallel batch. Such a call has
  *returned* and is not in flight, and the harness that continues the
  path appends that output and runs nothing again. A reader of 0.14
  read a call settled inside a parallel batch cut by a crash as in
  flight. Redaction: a `result` goes with its call, and its `item` is
  redacted in step with its output.

- `item`, `result`, `compaction` and `branch_summary` define `trust`,
  and a path's trust is the union over it, folds included. A reader of
  0.14 lost an untrusted output's mark at the first fold, unless it
  walked the whole path for a harness's own namespace. A redaction
  keeps `trust`, and never narrows a fold's. RFC 0002's context hash
  leaves it out.

- The ATIF projection marks copied steps `is_copied_context` without a
  model, metrics or a call count and leaves them out of `total_steps`,
  and carries `copied_from`, `deliver`, a call's `result` and `trust`.
  The OpenTelemetry projection gives a copied call no tool span and ends
  a hand-off's span at its `result`. A projection is not a file, so no
  file reads differently.

- Redaction: a `custom` entry carrying a call's `call_id` goes with the
  call, as its `dispatch`, `decision` and `link` entries do. A
  redaction of 0.14 that left a call out could keep such an entry,
  which then named a call the file no longer held. The rule tightens
  redactors alone; no file reads differently. A call a tool makes from
  inside another, which no `function_call` item holds, stays a harness's
  extension (a `custom` entry carrying the parent call's `call_id`), so
  it too goes with the call that made it.

## Changes since 0.13

Optional members, two relaxed rules, the form a sidecar session's
request hash covers, the universal root, and the rules of redaction. A 0.13 file is a 0.14
file that holds none of the new members. No hash changes. Like 0.12's, the new rules turn on the members an entry
carries and not on the header, so a reader of 0.14 reads each member
below by its form in a file of any minor, and raising a file's
`format` never makes an entry that verified before fail. A writer of
0.14 raises every file it appends to, as Versioning says, so every
reader of 0.13 refuses those files until it is upgraded.

- The universal root is defined: the one point every session's entries
  hang from, shared by every session, which `parent: null` names on the
  wire. A session is one tree under it, and what earlier drafts called a
  second root is a branch at the root; an entry with a null parent is a
  first entry. No line changes and no hash moves. A `leaf` label whose
  `target` is `null` names the root in a file with no `base`, so a
  writer can record a head at the empty context; a 0.13 reader holds such
  a label not in force, as it holds any label on a target the file does
  not hold.

- `env` defines two members of `workspace`, `host`, the machine the
  file system is on, and `instance`, the file system among those its
  `kind`, `ref` and `host` describe, both opaque strings, and says that
  `ref` is absent for `local`. A writer SHOULD write `host` whenever
  another machine could hold a file system with the same `kind` and
  `ref`, every `local` workspace among them, and `instance` whenever a
  restart gives a new file system. 0.13 told a writer to put the host
  and the instance in members of `workspace` it did not name, so two
  writers could spell them differently and a resume across them read as
  a substitution. The substitution rule already compared every member
  of `workspace`, so no earlier file reads differently, and a 0.13 file
  that names the host in `ref` stays valid.

- `item` defines `failed`, `true` on an output's entry whose call
  failed or was given up. A reader of 0.13 could not tell such a call
  from one that succeeded: the output's text says so by a harness's
  convention, which no reader can rely on. Like `from_link`, it reaches
  no request, and RFC 0002's context hash leaves it out.

- A `reject` carrying `withdrawn` directly after a call's `dispatch`
  withdraws that hand-off, and is the one `reject` a `dispatch` may
  precede. A reader of 0.13 read a call whose hand-off a subscriber
  refused after the `dispatch` was durable as in flight, and a harness
  that knew better recorded it in a member of its own that no other
  reader knew. A reader of 0.13 reports a withdrawal as a `reject`
  after a `dispatch`, and refuses a file of 0.14 by its header in any
  case.

- `item` defines `from_link`, naming the `peer` link whose callee's
  later report the item carries. A reader of 0.13 could tell that report
  from any other input only through a harness's own members. It joins
  the members that name an entry, which migration and redaction rewrite.

- `omit` defines `outputs` and `images`, which send a tool output, or
  the image parts of items, as placeholder text. A reader of 0.13 read
  a request so edited as the request the path rebuilds, so every
  response after the first edit was written without a `request_hash`
  or verified as a divergence. `outputs`' `items` and `images`'
  `before` join the members that name an entry. A reader of 0.14 reads
  either member by its form in a file of any minor, as it reads `omit`;
  a 0.13 file that held one under another meaning, which it was free
  to, rebuilds requests differently under 0.14, and no writer of this
  library wrote one.

- Redaction says what leaving entries out means, member by member
  (Leaving entries out): what stands in a left-out entry's place, which
  members name it and which lose it, which entries go with a call, a
  `dispatch` or a drained `queued` entry, how a placeholder for one
  entry is marked, and what a run end and a pin become. 0.13 said only
  that a member "names that or is removed", and two redactors could
  answer differently and both be valid. Two rules tighten the format: a
  redactor that changes a pin's source changes the pin, and `first_kept`
  names an entry before its compaction, which the context algorithm
  already required; a redaction that leaves out every entry from
  `first_kept` to the compaction writes a placeholder for it to name.
  No file a reader of 0.13 accepted reads differently.

- Redaction says which members of a redacted session's header are
  text, which the redaction writes, and which are references or a
  `call_id` (The redacted session). 0.13 named header members only as
  references, and left a redactor to decide whether a member it did not
  know held text.

- Redaction forbids writing an instruction part, or an omitted part's
  `of`, under an original hash whose text or entry the redactor cannot
  resolve on the path: it writes the part as text of its own, or does
  not redact the entry. 0.13 forbade keeping an original hash anywhere
  but in a reference into another session, and did not say what a
  redactor that cannot resolve a part does instead.

- Redaction says where a member name is data, which a redactor reaches:
  an `env` entry's `files` and `tools`, and every object whose members
  this document does not define, at any depth. 0.13 named `files`
  alone, so a path or a secret used as a key in a `custom` entry's
  `data` survived a redaction that rewrote every string value, and a
  redactor that refused such keys refused a quarter of a real corpus.

- `custom` defines `attempts` beside `usage`, the number of model calls
  the usage sums. 0.13 counted such an entry as one call, so a failed
  fold that asked twice read as one call while a retried `response`
  read as its `attempts`, and `total_steps` disagreed with the sum of
  `llm_call_count`.

- The ATIF projection carries what a step's own request left out
  (`request_omitted`, `request_replaced`), the placeholders the leaf's
  request sends (`replaced`, with the item as `sent`), a `custom` entry
  naming a call in that call's step, `failed`, `from_link` and a
  withdrawal; counts `total_steps` by `attempts`; and includes embedded
  subagents in the root's `total_cost_usd`, as ATIF defines it. A
  projection is not a file, so no file reads differently.

- Request hash says which form a sidecar session hashes: the request
  with each `sidecar:` URL as recorded, while the provider is sent the
  blob's bytes. 0.13 said the request "as it was sent", which no reader
  of a sidecar session can rebuild. No writer of this library wrote a
  sidecar session's hash any other way.

- A run written `resume` whose segment holds record entries alone, no
  entry in context, no `response` and no `dispatch` or `decision`, is accepted as
  written, as 0.11 accepted one whose segment held nothing. A writer
  that replays a child's link, or a hook that annotates the run, before
  the run takes its call up no longer writes a run a reader reports.
  The rule accepts only what a reader of 0.13 reported, so a reader of
  0.14 applies it to a file of any minor.

## Changes since 0.12

A body member, a header member, and the rules for redaction, which
were the `redacted` row's. A 0.12 file is a 0.13 file that holds
neither member. No hash changes.

- A redacted entry carries `redacted`, the positions of the original's
  entries it was made from (Redaction). A redaction may replace a run
  of entries with one placeholder that names the run, merge entries,
  split them or add some; the 0.12 row allowed only bodies to change.
  Positions name the original's entries without a hash that confirms a
  guessed value.
- A redacted session is a session of its own: an `id` of its own and
  `redacted_from`, naming its original and, as `profile`, the
  redaction. A 0.12 redacted file kept the original's `id` under
  different entry IDs, so a store or dataset holding both held two
  sessions under one ID, and nothing linked the copy to what it was made
  from.
- A redacted session grows and may be resumed. The redactor appends
  what it redacts as the original grows, and an entry another writer
  appends is not redacted and carries no `redacted`, so the two are
  told apart entry by entry.
- A redacted session's ID SHOULD be derived from its original's and the
  profile, so one original under one profile has one redacted session
  and a redaction repeated over a store appends only what is new.
- A redactor that holds the redaction of a session another references
  rewrites the reference to it. Left as it was, the reference named
  entries a set redacted together does not hold, by hashes that confirm
  a guessed value.
- The members a redactor rewrites include `omit`'s `items` and
  `instructions_omitted`'s `of`, an instruction part's `hash` follows
  its text, `normalised` records are redacted with their members, and
  data in `env` files' keys is reached. Each followed from the 0.12
  row's "every entry-naming member" or from a member's definition; the
  row listed none of them.
- What an original hash kept in a redacted session lets a reader
  confirm is stated.

A writer of 0.13 raises every file it appends to, as Versioning says, so
every reader of 0.12 refuses those files until it is upgraded. A 0.12
redacted file is read as it stands: a redacted session without
`redacted_from` whose entries carry no `redacted`, and whose `id` may be
its original's. Every entry of it is the redaction's, since 0.12 marked
none and forbade appending to one, so a reader treats each as redacted
where the difference matters, as for a `request_hash` that does not
match.

In a file whose header names a minor before 0.13, `redacted_from` in
the header and `redacted` on an entry are members the reader does not
know, whatever their form. The rule that comes with `redacted_from` is
a refusal, and reading either member by its form would refuse a 0.12
file that was valid when written, or read an entry as redacted that was
not. These are the 0.13 rules that turn on the header rather than on
the member. For the same reason a writer MUST NOT raise to 0.13 or
later a header that holds `redacted_from`, or a file an entry of which
holds `redacted`, and refuses to append to such a file, since the raise
would give the member a meaning it was not written with. A writer MUST
NOT create a session at 0.13 or later whose prefix holds such an entry,
a fork of such a file among them, for the same reason.

### Changed after writers of 0.13 were released

0.13 as first released let a redactor keep a redacted `response`'s
`request_hash`, the original's, which no longer matched the request the
redacted session rebuilds, and asked readers not to report it. It now
requires the hash recomputed over the redacted session, which then
verifies like any other, and readers hold a redacted entry's
`request_hash` to the usual rule. No writer of 0.13 that redacts had
been released when it changed, so no 0.13 file holds a kept hash, and a
reader of 0.13 applies the rule to every 0.13 file. A 0.12 redacted
file is read as it stands, as above.

## Changes since 0.11

One relation, four members of `link` and a second use of `target`, all
optional. A 0.11 file is a 0.12 file that holds none of them. No hash
changes. The new rules turn on the members a link carries, not on the
header, so a file reads and verifies the same before and after a writer
raises it, save one exception `link` states: a reader of 0.12 reads the
four members on an earlier file's `subsession` or `peer` link as 0.12
defines them.

- A `peer` relation on `link` names a callee whose work outlives the
  call that reached it (`link`). Which relation links a callee follows
  its lifetime: a callee the caller started, whose work the caller ends
  or asks to stop with the call, is a `subsession`, wherever it runs. A
  header's `parent_session` and the subsession check cannot describe a
  callee in another system that the caller did not spawn.
- `url`, `task` and `context` on a `subsession` or `peer` link name a
  callee in another system, reached over a protocol such as A2A: the
  endpoint called, the callee's task, absent when it answered with a
  message, and its conversation (`link`). A call dispatched again gets a
  link for each callee it reached, told apart by `task`. A call to such a
  callee left the record with nothing that said which work answered it.
- A cancel notice, a later link of the call with `cancel: "sent"` and a
  `target` naming the link of the callee it was sent to, records that
  the caller told the callee it no longer wanted the work, and not that
  the work stopped (`link`). A caller that gives up a call does not wait
  for the callee to confirm, so the record can say only what it sent,
  and `target` says which of a call's callees it was sent to. The
  `target` is an entry-naming member, rewritten by migration and
  redaction and re-pointed by whoever copies the links.
- A link belongs to a `dispatch` of its call, and a callee answered the
  call only when the output follows its link and its call's last
  `dispatch`, with no cancel notice naming it and no `answer` between
  (`link`).

A writer of 0.12 raises every file it appends to, as Versioning says, so
every reader of 0.11 refuses those files until it is upgraded. A member
0.12 defines that an earlier file holds in another form, which it was
free to while the name was undefined, is a member the reader does not
know, and is kept as written.

## Changes since 0.10

Five changes: one relaxes a rule, one is a writer's use of a rule a
reader already had, and three add a member or a relation. A 0.10 file
is a 0.11 file that holds none of the members and reads under the one
relaxed rule. No hash changes.

- A run written `resume` whose segment holds nothing is accepted as
  written (`run`). A refusal by a subscriber to a run's start, or a kill
  before the run took anything up, left a segment the 0.10 rule computed
  as `input`, and every leaf below it failed to verify for ever. The rule
  is relaxed for every file a reader of 0.11 reads, 0.9 and 0.10 ones
  included, since the file such a run left was never corrupt: its writer
  meant to take up a call and the run ended first. A `resume` over a
  segment that adds a message or an output and takes up nothing is still
  reported.
- A writer of 0.11 names an unchanged instructions part by its `hash`
  whether or not the part is in force, since the path has its text
  (`config`, instructions as parts). A hand-back to an agent whose parts
  another agent replaced repeated the text of every part the other agent
  displaced, and that was the 99.7% of what a hand-back appended in the
  study that measured it. The reader's rule is not new, only a writer's
  use of it, and a reader of 0.10 that resolves a `hash` against the
  parts in force alone refuses a file of 0.11 by its header.
- A `keep` in an omitted list MAY carry `of`, naming an earlier `config`
  entry whose list it counts over (`config`, instructions as parts). A
  hand-back repeated the whole omitted list an earlier entry had written,
  474 elements and 37 KB, and an element is the size of a hash, so only
  naming the list could save it.
- `omit` on a `config` entry records the items a request leaves out of the
  context (`config`, items left out, and the context algorithm). A
  reasoning item bound to another model, and any item a host filters,
  were on the path and not in the request, and nothing said so: every
  response after a model switch under reasoning went unhashed. `reasoning:
  other_models` is a rule of the record that a writer writes once, and
  `items` is the general case.
- A `judged_by` relation on `link` names the session that judged this
  one (`link`). A header's `parent_session` cannot tell a judge from a
  subagent or a fork.

A writer of 0.11 raises every file it appends to, as Versioning says, so
every reader of 0.10 refuses those files until it is upgraded, and
readers of 0.11 are upgraded before writers of 0.11 reach the files they
read. A member 0.11 defines that an earlier file holds in another form,
which it was free to while the name was undefined, is a member the
reader does not know, and is kept as written.

## Changes since 0.9

One rule is relaxed and one reading corrected. A `dispatch` for a call
elsewhere in the session, off the path, now satisfies the `answer`
rule, and a reader no longer reads the call as never started, in a 0.9
file as in a 0.10 one: a writer that rebases to a point between a call
and its `dispatch` leaves a call that may have run, which 0.9 let it
end only by a `reject`, which says the call never reached its tool, or
by a second `dispatch`, which says it was handed over again. A 0.9 file
is a 0.10 file. 0.10 also exists because 0.9's rules were amended in place after
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

### Clarified after writers of 0.10 were released

0.10's rules do not change. These paragraphs were written into this
document after writers of 0.10 were released, each saying what the
format already meant where the text was silent or a reader was lax, so
a reader of 0.10 applies them to every 0.10 file and no writer of 0.10
wrote what they refuse: a `subsession` link and the header of the
session it names agree, which a verifier that reads the target MAY
check (`link`); a `custom` entry whose `data` carries a top-level
`usage` records a paid model call, which a reader totalling the path's
cost counts (`custom`, ATIF); the workspace before the first `env`
entry is absent, so a first `env` entry after a `response` that names
one is a substitution, as a reader holding the environment fixed
already read it (`env`); a reader refuses a file with a `base` that
breaks the base rule, which a writer appending to the file was always
held to (envelope); a reader asking what a tool runs with passes over
an `answer`'s `args`, which the text already said name nothing
(`decision`); a prefix call's `dispatch` is resolved through
`parent_session` (`dispatch`); and how a hand-off's span ends
(OpenTelemetry). Each would have been a rule of 0.11 had it changed
what a file may hold or how its context is built; none does.

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
this version would otherwise have needed, a first entry's `parents` naming the
point another session was copied from together with every rule that
let a reader tell copy from original inside one file, is not written,
because `base` says it in one member and the hash proves it in one
comparison.

A retry of a subsession's call is its own session, derived with an
attempt counter, where 0.4 appended a second first entry, a branch at
the root, to the existing child; a session with a base has one prefix,
so a branch at the root has no place in it.

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
  IDs, with a retried call appending a branch at the root to the existing child
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
  digest in the session's directory, and, since 0.16, its media type in
  the file `<hex>.type` beside it. Whether a store's projection may
  share one directory across sessions is still open.
- The venue: this repository, a standalone repository, or a proposal to
  openresponses.org as a companion document.
