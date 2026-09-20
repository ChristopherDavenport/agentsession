# RFC 0001: Agent Session Format

Status: draft 0.2
Author: Christopher Davenport
Discussion: to be opened against this repository, then proposed to the
Open Responses community as a companion specification.

## Summary

A session is the record of one conversation between a harness and a
model, kept so that every request the model received can be rebuilt and
every action taken on the model's behalf can be accounted for. It is an
append-only, tree-structured JSONL file.

The file holds two kinds of entry. **Context entries** are what the
model was sent and what it returned: items, responses, configuration and
compaction. Replaying them along a path rebuilds a request byte for
byte. **Record entries** are what happened around the conversation: why
a run started and how it ended, that a tool call was dispatched, who
decided a call's fate and how, the environment the tools ran in, where
the conversation branched, links to other sessions, and judgements of
the result. They never enter the model's context.

The envelope is harness-neutral. The payload of a context entry is an
Open Responses item, so the record is the wire format itself.

This is a storage, resume and audit format. ATIF covers evaluation
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
  record stopped, or is waiting on an answer.
- **Append-only.** A writer only ever appends lines. A crashed session
  is a valid prefix.
- **Tree-shaped.** Branching is a child of an earlier entry, in place.
- **Forward compatible.** Readers preserve what they do not understand.
- **Harness-neutral envelope, normative payload.** The envelope carries
  no harness vocabulary. Conversation payloads are Open Responses items.
- **Projectable.** Defined mappings to ATIF for training and to
  OpenTelemetry for observability, keyed by the same entry IDs.

## Non-goals

- Multi-writer concurrency on one file.
- Cross-session indexing, search or listing. That is a store's concern.
- Rendering hints beyond a display flag.
- Defining tool semantics. A tool is a name, a schema and a result.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as in
RFC 2119.

- **Session**: one file, one header, zero or more entries.
- **Entry**: one JSON object on one line after the header.
- **Path**: the sequence of entries from an entry to a root, reversed.
- **Leaf**: the entry the next append will name as its parent.
- **Item**: an Open Responses item as defined by the Open Responses
  specification at the version named in the header.
- **Run**: one dispatch of the harness's loop, from an input to the
  point the harness stops calling the model. A session holds many runs.
- **Call**: one `function_call` item and everything that happens to it:
  a decision about it, its dispatch, its output.
- **In context**: contributing to the request the context algorithm
  builds. A context entry is in context; a record entry is not.

## File

- UTF-8, one JSON object per line, lines terminated by LF.
- The first line MUST be the header. Every later line MUST be an entry.
- A reader MUST tolerate a final line that does not parse and MUST
  report it. A writer MUST NOT rely on such recovery.
- Media referenced by items MAY be stored inline as data URLs or beside
  the file under a directory named after the session ID. A header field
  says which.
- Entry order and `parent` links are the ordering. `ts` is informational
  and a reader MUST NOT order entries by it; clocks step backwards.

## Header

```json
{"type":"session","format":"agentsession/0.2","id":"…","created_at":"2026-09-17T12:00:00Z",
 "payload":"openresponses/2026-04-24","harness":{"name":"…","version":"…"},
 "cwd":"/path","parent_session":"…","spawned_by":"call_…","media":"inline"}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | the string `session` |
| `format` | MUST | `agentsession/<major>.<minor>` |
| `id` | MUST | globally unique; UUIDv7 RECOMMENDED. For a subsession, a UUIDv5 over `<parent session id>/<call_id>` is RECOMMENDED, so a reader can compute the child's ID from the parent's `link` or `function_call` alone. A second child for the same call appends a new root to the existing child session rather than minting a second ID |
| `created_at` | MUST | RFC 3339 |
| `payload` | MUST | payload profile; `openresponses/<spec-date>` is the only profile this RFC defines |
| `harness` | SHOULD | name and version of the writer |
| `cwd` | MAY | working directory at creation; an `env` entry's `cwd` takes precedence from that entry on |
| `parent_session` | MAY | session ID this was forked or spawned from |
| `spawned_by` | MAY | for a subsession, the `call_id` of the parent's function call that spawned it |
| `media` | MAY | `inline` (default) or `sidecar` |

Unknown header fields MUST be preserved by any tool that rewrites the
file.

## Entry envelope

```json
{"type":"item","id":"a1b2","parent":"9f8e","ts":"2026-09-17T12:00:01Z", …}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | entry type; core types below, or namespaced `ns:type` |
| `id` | MUST | unique within the file; opaque string |
| `parent` | MUST | ID of the parent entry, or `null` for a root |
| `ts` | MUST | RFC 3339 with sub-second precision RECOMMENDED |

A parent MUST appear earlier in the file than any child. Multiple roots
are permitted. An entry MUST NOT be modified after it is written;
corrections are new entries.

## Core entry types

Context entries: `item`, `response`, `config`, `compaction`,
`branch_summary`. Record entries: `run`, `dispatch`, `decision`,
`label`, `info`, `env`, `outcome`, `link`, `custom`. A record entry
contributes nothing to context; the context algorithm below is the
normative statement.

A type is core only if the event it records belongs to the loop every
harness runs: an input arrives, the model is called, a call is decided
and dispatched, its output returns, the context is compacted, the
conversation branches, the run ends. An event that belongs to one
harness's features, however useful, is an extension (`ns:type`) or a
`custom` entry. That rule is what keeps the envelope harness-neutral as
the format grows.

### `item`

One conversation item in the payload profile.

```json
{"type":"item","id":"…","parent":"…","ts":"…",
 "item":{"type":"message","role":"user","content":[…]},
 "response":"resp_…","visible":true}
```

- `item` MUST be a valid item for the payload profile. Under the
  Open Responses profile that includes `message`, `function_call`,
  `function_call_output`, `reasoning`, `compaction`, `item_reference`
  and any slug-prefixed item the profile allows.
- `response` MAY name the response entry this item belongs to when the
  item was model output.
- `visible` MAY be `false` to mark an item that is part of the model
  context but that a renderer SHOULD hide.

### `response`

The envelope of one model call, written after its output items.

```json
{"type":"response","id":"…","parent":"…","ts":"…",
 "response_id":"resp_…","model":"…","status":"completed",
 "usage":{…},"incomplete":null,"error":null,
 "request_hash":"sha256:…","latency_ms":1234}
```

- `usage`, `incomplete` and `error` use the payload profile's shapes.
- `request_hash` SHOULD be the hash of the canonical request built by
  the context algorithm below, so a reader can check that the stored
  path rebuilds the request that was sent.

### `config`

A delta to request settings. The first entry on any root SHOULD be a
`config` carrying full settings.

```json
{"type":"config","id":"…","parent":"…","ts":"…",
 "model":"…","instructions":"…","reasoning":{…},"text":{…},
 "tools_added":[…],"tools_removed":["name"],"extra":{…},"replace":false}
```

Fields absent from a delta are unchanged. `replace: true` discards all
earlier config on the path before applying this one. Tool definitions
use the payload profile's tool shape.

### `compaction`

Replaces earlier context with a summary.

```json
{"type":"compaction","id":"…","parent":"…","ts":"…",
 "first_kept":"entry-id","summary":{…item…},"config":{…full config…},
 "tokens_before":50000,"usage":{…}}
```

- `first_kept` MUST name an entry on the path. Entries before it are
  excluded from context; entries from it up to this entry are kept.
- `summary` is an item. Under the Open Responses profile a server-side
  compaction stores the returned `compaction` item verbatim; a local
  summary is a `message` item.
- `config` is a full checkpoint so a reader need not replay config
  entries from before the compaction. Its shape is the settings the
  context algorithm produces, not a `config` delta:

  ```json
  {"model":"…","instructions":"…","reasoning":{…},"text":{…},
   "tools":[…],"extra":{…}}
  ```

  `tools` is the full list of tool definitions in force at the
  compaction, in the order the context algorithm would send them, not
  a delta; there are no `tools_added`, `tools_removed` or `replace`
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
 "source":"input|resume","ref":"…"}
{"type":"run","id":"…","parent":"…","ts":"…","run_id":"…","phase":"end",
 "reason":"done|stopped|input_required|aborted|error","ref":"…",
 "pending":["call_…"]}
```

- `source` is closed to two path shapes: `input`, a new input started
  the run, and `resume`, the run began by answering calls the previous
  run's `end` entry left pending. `ref` is the harness's opaque name for
  what triggered the input (a cron name, a channel message ID). How an
  input arrived, whether a schedule, a channel or another agent, is a
  harness feature and goes in `ref` or a `custom` entry.
- `reason` is closed. Each value is a shape of the run's segment, the
  entries on the path from the `start` entry to the `end` entry, where
  a pending call is a `function_call` on the segment with no
  `function_call_output` on it:
  - `done`: the last `response` on the segment has no `function_call`
    in its output and no error.
  - `stopped`: the last `response` has calls, every call has an output,
    there is no error, and no `response` follows. `ref` names the cause
    in the harness's own terms (a turn budget, a tool that asked to
    stop) and is opaque to readers.
  - `input_required`: at least one call is pending, and every pending
    call has a `hold` decision and no `dispatch`.
  - `aborted`: at least one pending call has a `dispatch`, or the last
    `response` is incomplete without an error.
  - `error`: the last `response` on the segment carries an error, or
    the harness failed before it could write one, which `ref` names.

  A reader MAY recompute `reason` from the segment. The segment is
  authoritative when the two disagree.
- `pending` lists the pending calls' IDs so a resume can read them
  without walking the segment. The segment is authoritative here too.
- Items and responses of the run follow its `start` entry on the path.
  A run with no `end` entry was cut off; that is the crash signal.

### `dispatch`

A call was handed to its tool.

```json
{"type":"dispatch","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id"}
```

`target` is the `item` entry holding the `function_call`. A call with a
`dispatch` and no `function_call_output` on the path was in flight when
the record stopped, and its side effect may have happened. A call with
neither was never started. A writer SHOULD write the dispatch before the
tool runs, and MUST NOT write it for a call that was rejected. A
`dispatch` with no `decision` before it on the path is the shape of a
call that proceeded without anyone deciding, so a writer that records
no decisions still produces a valid file.

### `decision`

A call's fate was decided outside the tool.

```json
{"type":"decision","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id",
 "verdict":"proceed|reject|hold","by":"human|policy|agent",
 "reason":"…","args":{…}}
```

- `verdict` is closed. Each value is defined by what follows the
  decision on the path:
  - `proceed`: a `dispatch` for the call follows.
  - `reject`: no `dispatch` ever follows, and a `function_call_output`
    for the call follows whose content is `reason`.
  - `hold`: neither follows from this decision; a later `decision` on
    the same call answers it with `proceed` or `reject`.

  A call may carry several decisions on the path, in order. The path
  already shows whether a `proceed` or `reject` answered a `hold`, so
  there is no separate verdict for an answer. A writer SHOULD write
  `proceed` only when it answers an earlier `hold` or carries `args`;
  otherwise the `dispatch` is the record that the call proceeded.
- `call_id`, `target` and `verdict` are required. `by` and `reason` are
  optional. `by` is closed: `human` is a person, `policy` is a rule the
  harness evaluated without waiting, `agent` is another model. `reason`
  is the text the decider gave, which for `reject` is also what the
  model saw as the output.
- `args`, when present, are the arguments the tool ran with when a
  decision rewrote them. The `function_call` item stays as the model
  produced it, so the request hash still verifies; the change is
  recorded beside the call, never inside it.

A `decision` is a lifecycle fact and carries no score. A judgement of
how something went is an `outcome`.

### `label`, `info`

```json
{"type":"label","id":"…","parent":"…","ts":"…","target":"entry-id","label":"checkpoint"}
{"type":"info","id":"…","parent":"…","ts":"…","name":"Refactor auth"}
```

Not in context. A `label` with `label: null` clears.

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
tag moves. Anything richer is an unknown member, which the envelope
already preserves. An `env` entry applies from its position on the path
until the next one.

### `outcome`

A judgement of how the session, or a range of it, went.

```json
{"type":"outcome","id":"…","parent":"…","ts":"…",
 "kind":"feedback|test|task|tool_error|eval|custom","target":"entry-id",
 "score":1.0,"pass":true,"label":"…","details":{…}}
```

- `target` MUST name an entry in this session, usually the last entry of
  the range judged. A reader that selects branches by outcome resolves
  `target` as an entry on a path; a task or test name belongs in
  `details`.
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
`subsession` link SHOULD be written when the call is dispatched, before
the child's header exists, so a link whose session cannot be found means
the child never started rather than a child that was never linked.

### `custom`

App state that is not in context: `{"type":"custom","ns":"…","data":…}`.
App state that is in context uses a namespaced `item` with the payload
profile's extension mechanism instead.

## Namespaced types

Any type of the form `ns:name` is an extension. Readers MUST preserve
extension entries and MUST NOT fail on them. Extension entries are not
in context unless the extension says so, and a reader that does not know
the extension MUST treat them as not in context.

## Context building

Given a leaf, a reader MUST produce the request settings and the item
list as follows.

1. Walk `parent` links from the leaf to a root; reverse to root-first.
2. Replay `config` entries along the path in order to produce settings,
   honouring `replace`.
3. Find the last `compaction` on the path, if any. If found:
   settings start from its `config` checkpoint and then replay any
   `config` after it; the item list starts with its `summary`, then the
   items of entries from `first_kept` up to but excluding the
   compaction, then the items of entries after it.
   If none: the item list is the items of all entries on the path.
4. An entry contributes an item if it is `item`, or `branch_summary`,
   or a `compaction` selected in step 3. Every record entry (`run`,
   `dispatch`, `decision`, `label`, `info`, `env`, `outcome`, `link`,
   `custom`) and every unknown extension contributes nothing.
5. The canonical request is settings plus the item list, encoded as the
   payload profile's request with `store: false` and no
   `previous_response_id`. Its hash is `request_hash`.

### Request hash

`request_hash` is defined here so that a writer and a reader built
separately agree without sharing code.

- The input is the request object of step 5 as it was sent, including
  any passthrough keys the payload profile allows, because those reached
  the model.
- The object is serialised with the JSON Canonicalization Scheme
  (RFC 8785): members sorted by code point, no insignificant whitespace,
  numbers and strings in their canonical forms.
- The value is `sha256:` followed by the lowercase hexadecimal SHA-256
  of the canonical bytes.

A writer that records `request_hash` MUST compute it this way. A reader
MAY verify it by rebuilding the request from the path and comparing.

## Writing discipline

- Output items MUST be written only when complete. Partial streaming
  state MUST NOT be written as an `item`.
- A `response` entry MUST follow the items it envelopes and MUST be the
  last entry written for that model call.
- A user item or function call output SHOULD be written before the
  request that includes it is sent.
- Writers SHOULD fsync at least on each `response` entry and on each
  `function_call_output` item, since the output is the record that a
  side effect happened.

## Projections

### ATIF

One ATIF document per root-to-leaf path. `session_id` is the header ID;
`trajectory_id` is the leaf entry ID. The mapping is given in the
accompanying session plan and is normative for the Open Responses
profile: user and system items to steps, one `response` with its items
to one agent step with `tool_calls`, `reasoning_content` and `metrics`,
function call outputs to observations by `source_call_id`, compaction
and branch summaries as copied-context system steps, `link` entries to
`subagent_trajectories`. `run`, `dispatch` and `decision` entries have
no step of their own. A run's `source`, `ref` and end `reason` travel under
`run` in the `extra` of the step holding the run's first input; a
call's decisions and dispatch travel under `calls`, keyed by call ID,
in the `extra` of the agent step that produced the call; a fold's usage
travels under `usage` in its system step's `extra`. Raw items travel in
step `extra` so the projection is lossless.

### OpenTelemetry

Spans emitted for a session carry `session.id` and the entry ID of the
entry they correspond to. Tool spans link to the inference span whose
output contained the call; each inference span links to the previous
turn's; the first span after a branch links to the branched-from entry.

## Versioning

`format` is `agentsession/<major>.<minor>`. A minor version adds entry types or
optional fields. A major version changes the envelope, the header, or
the context algorithm. Readers MUST accept any minor version of a major
they support. Files are migrated in memory, never rewritten in place.

## Conformance

A conforming **writer** produces files that satisfy every MUST in this
document. A conforming **reader** implements the context algorithm and
the preservation rules. A conforming **converter** from a native format
documents which native entries it maps and which it drops.

The reference implementation is the Go `agentsession` library. The
conformance suite is a directory of fixture files with expected context
output for every leaf, expected `request_hash` values, the recomputed
`reason` for every `run` end, and negative cases for a broken parent
link, a truncated last line, an unknown type and a `dispatch` that
follows a `reject`. Converters for pi, Claude Code and Codex are part of
the initial proposal so the format arrives with three existing corpora
behind it.

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

## Changes since 0.1

All additive; a 0.1 reader preserves every new entry and rebuilds the
same context.

- The Summary names the two kinds of entry, and the core types section
  states the test for admitting a core type.
- New record entries `run`, `dispatch` and `decision`, which make the
  Resumable goal true. A run's `source`, its end `reason` and a
  decision's `verdict` are defined as shapes of the path a reader can
  recompute, not as one harness's vocabulary; how an input arrived and
  who decided a call are optional or belong in `ref` and `custom`.
- `outcome`: `target` is an entry ID by rule, `pass` added, `score`
  unbounded, `eval` kind.
- `env`: `workspace` member, a `kind` and one `ref`; `cwd` precedence
  over the header.
- Header: `spawned_by`; derived subsession IDs, with a retried call
  appending a root to the existing child session; `link` written at
  dispatch.
- File: entry order is the ordering, not `ts`.
- Writing discipline: fsync on function call outputs.

Considered and held: instructions as parts in `config`, which would add
a second spelling of settings and change step 2 for a storage cost that
belongs to the store; and a durable leaf marker, which a library can
carry as a reserved `label` without a format change; and a `source`
on the item envelope for an input that joins a run already in flight,
which the `run` entry cannot name. All three are open questions below.

## Open questions

- Whether `config` should carry `instructions_parts` so a delta names
  only the part that changed. Held: it changes step 2 and adds a second
  spelling of the same settings; try deduplication in the store first.
- Whether the current leaf needs a durable marker. Held: a reserved
  `label` a library honours on open covers it without a format change.
- Whether the `item` envelope needs a member naming how an input
  queued into a run already in flight arrived. Held: `run` says only
  whether a run began from an input or a resume; how an input arrived
  is a harness feature, so a harness that steers a running agent records
  it in a `custom` entry until the case is better understood.
- Whether to allow a second payload profile at 0.x, or hold the line at
  Open Responses and rely on converters.
- Sidecar media layout and naming.
- The venue: this repository, a standalone repository, or a proposal to
  openresponses.org as a companion document.
