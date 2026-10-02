# Plan: format 0.11

What the next minor of the Agent Session Format carries, why each item
needs a minor rather than a release, and the order the work goes in.
Written for the round 8 fix wave (2026-10-01), which did not bump the
format; the decision and its reasons are the first section. RFC 0001 is
normative and every item below is a change to it first.

## Why not in the round 8 wave

Four open issues wait on a minor: #172 (a resume that took up nothing),
#173 (what a hand-back's config entry repeats), #56 (nothing records that
an item left the context) and #145 item 3 (no link relation means judged
by). The round 8 studies measured the cost: a hand-back's config entry is
78 KB, 99.7% of what a hand-back appends (letta-memory/2), and since
agentturn v0.0.15 every cross-model handoff, Plan-to-Act switch or model
switch under reasoning leaves the rest of the session's responses
unhashed (agents-sdk-handoffs/3, cline, dex). The pressure is real.

A minor still waits, for three reasons.

- The versioning rule. A writer of 0.11 raises every file it appends to,
  and every reader of 0.10 then refuses those files until it is
  upgraded. Readers ship before writers, across six repositories pinned
  at released tags; nothing in this wave could consume 0.11 anyway.
- The shape of #56 is a design decision about the context algorithm, the
  part of the format a major version exists to protect, and three
  products' recorders hinge on it. It wants the maintainer's eye on the
  RFC text before code is written against it, not a fix-wave decision.
- Half of #173 did not need the minor and is done: v0.0.19 resolves a
  hashed part against any text the path gave its id. The other half
  (the writer naming parts out of force by hash, and the omitted list
  naming an earlier list) is small once the minor exists.

## What 0.11 carries

Each item names the issue, the RFC section, the rule or member as it
should read, what a reader and a writer change, and the fixtures.

### 1. A `resume` that took up nothing (#172)

RFC `### run`, the `source` bullet. Today `resume` is the shape where
the segment's first output, decision or dispatch takes up a call that was
pending at the run's start, and `input` is everything else, including a
run that adds nothing. A resume refused by a `run_start` subscriber, or
killed, before it took anything up has the segment `start, end` or
`start` alone, which computes `input`, so `VerifyRecords` fails it and
every leaf below it, for ever. v0.0.19 answers with `Run.Empty` and a
`verify` note.

The 0.11 rule: a run written `resume` whose segment holds nothing between
its `start` and its `end`, or nothing after its `start` when the run was
cut, is accepted as written. The writer started the run to take up a
call and the run ended before it did; `resume` records what it meant to
do, which the segment cannot contradict. A resume that adds a message or
an output and takes nothing up keeps the check, so round 6's case stays
reported.

- Reader: `VerifyRecords` accepts either source for an empty segment.
  The relaxation applies to 0.9 and 0.10 files too, as 0.10 applied its
  relaxation of the `answer` rule to 0.9 files: a reader of 0.11 reads
  such a file as whole. `verify` drops `emptyResumeNote`.
- Writer: nothing. `ComputeSource` is unchanged; its doc says the check
  accepts `resume` over an empty segment.
- Fixtures: a positive native fixture with a refused resume (the
  `TestVerifyEmptyResume` builder in `cmd/agentsession/main_test.go`
  has the shape); the negative fixture, a resume followed by a user
  message that takes up nothing, stays and keeps failing.
- Consumers: none. agentturn's recorder already writes this shape.

### 2. Instruction parts by hash out of force (#173, writer half)

RFC `#### Instructions as parts` already says a part named by `hash`
resolves to "the one the path already has for that id", and v0.0.19's
reader does that. `Settings.InstructionsDelta` still writes the text of
every part not in force, because a v0.0.18 reader resolves a hash only
against the parts in force and would fail every hash on a hand-back.

The 0.11 change is to the writer alone, gated by the declared format: a
writer of 0.11 names any unchanged part whose text the path has, in force
or not, by its hash. A reader of 0.10 that is v0.0.18 or earlier refuses
a 0.11 file by its header, which is the point of raising it. Measured on
the letta-memory probe: the parts go from 40,620 bytes to about 14,070,
the entry from 78 KB to about 52 KB.

- Reader: none beyond v0.0.19.
- Writer: `InstructionsDelta` takes the part history (`partHistory`,
  what `applyInstructionParts` keeps) and names a part by hash when the
  history has its text; `Settings` carries the history so the delta can
  see it. A `replace` and a compaction checkpoint start the history
  afresh, as the RFC says.
- Fixtures: a hand-back fixture, a, b, a, with a's second entry naming
  every part by hash and resolving.
- RFC text: one sentence under the `hash` bullet saying a writer of 0.11
  names a part out of force by hash and a writer of 0.10 did not, so a
  reader of a 0.10 file never meets one.

### 3. An omitted list named by an earlier entry's (#173, the 37 KB)

RFC `#### Instructions as parts`, the `instructions_omitted` bullets. The
list has no hash form because an element (id, reason, size, source) is
about the size of a hash, so hashing elements saves nothing. What a
hand-back repeats is the whole list an earlier entry on the path already
wrote. Measured: 474 elements, 37,447 bytes, on every hand-back.

The 0.11 member: a `keep` element MAY carry `of`, the id of a `config`
entry on the path before this one that carries `instructions_omitted`;
the cursor the element counts from is into the list that entry put in
force, as this document resolves it at that entry, rather than the list
in force before this entry. `[{"keep":474,"of":"sha256:…"}]` is then the
whole hand-back. Rules: the named entry MUST be on the path and before
this entry, and a `keep` with `of` that names an entry not on the path,
or one without the member, names no part a reader can rebuild, and is
kept where it stands as an unresolved element is. An element with `of`
and an `id` is a member this document does not define there. `of` moves
the cursor of its own element only; the next element without `of`
counts from where the ordinary cursor is. A compaction's checkpoint
writes the list whole, so nothing after it names an entry before it.

- Reader: the context replay keeps, per `config` entry on the path that
  put an omitted list in force, that list keyed by entry id, until the
  next compaction checkpoint; `applyOmitted` resolves `of` against it.
- Writer: a new `Settings.OmittedDelta(parts []OmittedPart)` that writes
  the shortest of the whole list, a delta against the list in force, and
  a delta against the last list the path recorded for these parts; it
  needs the entry id of the config entry whose list is in force, which
  `Settings` records as it replays.
- Fixtures: the hand-back fixture of item 2 with the omitted list named
  by `of`, resolving to the first entry's list.
- Consumers: agentturn/session writes the delta where it writes the
  omitted list today. agentkit's `PartsFrom` is unchanged.

### 4. Items that leave the context (#56)

RFC `### config` and `## Context building`. Nothing describes an item on
the path that a request leaves out. Since agentturn v0.0.15 the common
case is computable from the record: a request to model M leaves out the
reasoning items whose response was written under another model, since
the provider refuses a foreign signature. The recorder then writes the
responses without a `request_hash` and an `agentturn:unhashed` entry,
and nothing after the switch verifies, for as long as those items are on
the path.

The 0.11 member: `omit` on a `config` entry, an object in force as a
setting (a delta without it leaves it, `replace: true` clears it, `{}`
clears it explicitly), carried into a compaction's checkpoint like the
other settings:

```json
{"type":"config","id":"…","parent":"…","ts":"…",
 "omit":{"reasoning":"other_models","items":["sha256:…"]}}
```

- `reasoning` is closed. `other_models`: a `reasoning` item that is an
  output of a `response` and was written while a model other than the
  model in force for this request was in force, which is the model in
  force at the item's own entry and never the `response` entry's
  `model`, contributes nothing in step 4 of the context algorithm. An
  empty model name attributes nothing: an item written under none is
  never left out, and a request under none leaves nothing out. The rule is a
  function of the record, so a writer writes it once, at the entry that
  changes the model, and it covers every later request without listing
  items; a switch back is covered too, since the rule reads the model
  in force at each request.
- `items` is the general case, for a host's own filter: entry ids on the
  path whose items contribute nothing from this entry on. The set in
  force is the union of every `items` on the path since the last
  `replace` or checkpoint, so a later entry adds to it and never has to
  repeat it.
- Step 4 of the context algorithm gains the sentence: an `item` that
  `omit` in force excludes contributes nothing. The `request_hash` is
  over the request built this way, so it verifies. The reader's
  `Context` says which entries it left out and why, so a projection can
  show them as omitted rather than absent, and `Verify` reports a
  response whose request would verify only without the rule as a
  divergence, not as unhashed.
- Fixtures: a two-model session with reasoning on both, `omit.reasoning`
  written at each switch, every response hashed and verifying; the same
  session without the member, verifying only the responses before the
  first switch, as 0.10 writes it.
- Consumers: agentturn/session writes `omit.reasoning: other_models` at
  the `SetConfig` that changes `ModelName` when `ReasoningModels` holds
  items of another model, and hashes the request against the rebuilt
  context; the `agentturn:unhashed` entry stays for the cases the rule
  does not describe. agenteval's strict replay rebuilds the request
  through the same context algorithm and needs no change beyond the
  library. The ATIF exporter shows omitted items as such.

Why a config member rather than a `drop` entry or a per-response list: a
`drop` entry would be a new core type for a setting-shaped fact, and a
per-response list (`request_omits`) repeats the ids on every response,
dozens for a session that reasoned for a while before a switch, when the
rule is one short object written once. The `items` form keeps the
general case available without a second mechanism.

### 5. A link relation for a judge (#145 item 3)

RFC `### link`: `rel` gains `judged_by`, written into the judged session,
naming the judge's session, with an optional `target` naming the entry
the judgement is about, the same entry the judge's `outcome` names. A
reader selecting the judges of a run reads the run's `judged_by` links;
`ListFilter.ParentSession` goes on returning judges, subagents and forks
together, since the header cannot say which, and the link can. agenteval
's runner writes it when it records an outcome in a judge's session.

## Rollout

- RFC first: the five sections above, `## Changes since 0.10`, the
  conformance list, the header example and `Format` to `0.11`.
- Fixtures: native, built where `fork.jsonl` is built, since a 0.4
  source cannot express any of these members; the negative fixtures for
  the `of` and `omit` rules beside them.
- Reader: `VerifyRecords` (1), `applyOmitted` and the replay's list
  history (3), the context algorithm and `Context` (4), `LinkEntry`
  relations (5). A reader of 0.11 reads 0.5 to 0.10 files as it stands.
- Writer: `FormatMinor` to 11, the raise on append (already in place),
  `InstructionsDelta` (2), `OmittedDelta` (3).
- `verify`: drop `emptyResumeNote`; a 0.10 file keeps `sourceNote`.
- Consumers, in release order: agentsession 0.11 readers everywhere
  first, then agentturn/session writing (2), (3) and (4), agenteval
  writing (5), the exports and the studies' probes.

## Decided by the maintainer

- `omit` carries `reasoning: other_models` and the cumulative `items`
  list.
- Item 1 relaxes 0.9 and 0.10 files too, as drafted.
- The omitted-list cursor is spelled `of`.

The RFC text refines the draft in four places, each to make the rule
total or to match what a recorder can do:

- `omit.reasoning` compares the model in force at the reasoning item's
  own entry with the model in force for the request, attributes nothing
  to an empty model name, and never reads a
  `response` entry's `model`. A provider spells that member as its own
  snapshot of the alias requested, which a request's model never equals,
  and agentturn attributes reasoning by the settings' model already.
- An `omit` delta merges: it sets the `reasoning` it names, adds its
  `items` to those in force, and `{}` clears both.
- `of` is counted over a list of its own, with a cursor of its own per
  list, which an element naming a part by `id` moves in each list that
  names it, so a hand-back whose parts changed in the middle keeps both
  halves by `of`.
- A `replace` starts the lists `of` can name afresh, as it starts the
  parts; the draft said only a compaction's checkpoint did.
