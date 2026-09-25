# RFC 0001: Agent Session Format

Status: draft 0.4
Author: Christopher Davenport
Discussion: to be opened against this repository, then proposed to the
Open Responses community as a companion specification.

## Summary

A session is the record of one conversation between a harness and a
model, kept so that every request the model received can be rebuilt and
every call the model made can be followed to its output or to the point
the record stopped. It is an append-only JSONL file whose entries form
a tree: every entry names one `parent`, and a context is built by
walking it. An entry MAY additionally name predecessors it converges —
the results of subagent sessions, a branch merged back — which record
provenance and never enter a context.

The file holds two kinds of entry. **Context entries** are what the
model was sent and what it returned: items, responses, configuration,
compaction and the summary carried across a branch. Replaying them
along a path rebuilds a request byte for byte. **Record entries** are
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
  record stopped, or is waiting on an answer, in any file whose header
  says the writer records dispatches and decisions.
- **Append-only.** A writer only ever appends lines. A crashed session
  is a valid prefix.
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

- Multi-writer concurrency on one file.
- Cross-session indexing, search or listing. That is a store's concern.
- Rendering hints beyond a display flag.
- Defining tool semantics. A tool is a name, a schema and a result.

## Terminology

The key words MUST, MUST NOT, SHOULD and MAY are to be interpreted as in
RFC 2119.

- **Session**: one file, one header, zero or more entries.
- **Entry**: one JSON object on one line after the header.
- **Path**: the sequence of entries from an entry to a root, reversed,
  following `parent` alone.
- **Leaf**: the entry the next append will name as its parent.
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
  says which.
- Entry order and `parent` links are the ordering. `ts` is informational
  and a reader MUST NOT order entries by it; clocks step backwards.

## Header

```json
{"type":"session","format":"agentsession/0.3","id":"…","created_at":"2026-09-17T12:00:00Z",
 "payload":"openresponses/2026-04-24","harness":{"name":"…","version":"…"},
 "records":["run","dispatch","decision"],
 "cwd":"/path","parent_session":"…","spawned_by":"call_…","media":"inline"}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | the string `session` |
| `format` | MUST | `agentsession/<major>.<minor>` |
| `id` | MUST | globally unique; UUIDv7 RECOMMENDED. For a subsession, a UUIDv5 under the nil namespace over `<parent session id>/<call_id>` is RECOMMENDED, so a reader can compute the child's ID from the parent's `link` or `function_call` alone. A second child for the same call appends a new root to the existing child session rather than minting a second ID |
| `created_at` | MUST | RFC 3339 |
| `payload` | MUST | payload profile; `openresponses/<spec-date>` is the only profile this RFC defines |
| `harness` | SHOULD | name and version of the writer |
| `records` | SHOULD | the record entry types, core or namespaced, this writer writes whenever their event occurs, so a reader may take their absence as the event not having happened. Absent or empty means no such promise |
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
| `parents` | MAY | further predecessors this entry converges; provenance only, never walked when building a context |
| `ts` | MUST | RFC 3339 with sub-second precision RECOMMENDED |

A parent MUST appear earlier in the file than any child. Multiple roots
are permitted. An entry MUST NOT be modified after it is written;
corrections are new entries.

Appending an entry of either kind makes it the leaf. A record entry is
a child of the leaf like any other, so it lies on the path of every
entry appended after it. The run and call shapes below depend on that:
a writer MUST NOT hang a record entry off an earlier entry as a
sibling.

A member of a core entry that this document does not define MUST be
preserved by any tool that rewrites the file and MUST be ignored by a
reader that does not know it. That is where a harness keeps detail
richer than a core member allows.

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

- Each reference MUST name an `entry`. `session` names the session that
  entry is in and MAY be omitted when it is in this file, which is the
  only case a reader can resolve without a store.
- `parents` MUST NOT contain the value of `parent`, and MUST NOT name
  the same entry twice.
- A reference MUST name an entry that already existed when this entry
  was written. With `parent`, that is what makes the structure acyclic:
  every edge points at something older.
- `parents` is **provenance**. It is not walked when building a context,
  and an entry it names contributes nothing to any context by virtue of
  being named. Whatever crossed the boundary is in this entry's own
  payload, materialised. That is the rule the format already applies to
  `branch_summary` and to a subagent's `function_call_output`; `parents`
  only records where the material came from.
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
  `function_call_output`, `reasoning`, `compaction`, `item_reference`
  and any slug-prefixed item the profile allows.
- `response` MAY name the response entry this item belongs to when the
  item was model output.
- `visible` MAY be `false` to mark an item that is part of the model
  context but that a renderer SHOULD hide.
- `source` MAY carry the trigger of an item a person or another system
  sent, in the shape `queued` defines, and `queued_from` MAY name the
  `queued` entry the item was accepted as.

### `response`

The envelope of one model call, written after its output items.

```json
{"type":"response","id":"…","parent":"…","ts":"…",
 "response_id":"resp_…","model":"…","status":"completed",
 "usage":{…},"incomplete":null,"error":null,
 "request_hash":"sha256:…","latency_ms":1234}
```

- Every member but the envelope's is optional. `error` and
  `incomplete` are read as null when absent; the run cascade reads
  those two and never `status`, so a converter over a log without a
  status need not invent one.
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
 "instructions_parts":[{"id":"agentsmd","text":"…","source":"agentsmd"},
                       {"id":"memory","hash":"sha256:…"}],
 "instructions_omitted":[{"id":"service/AGENTS.md","reason":"budget","size":4096,"source":"agentsmd"}],
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
- A delta carries the **whole ordered list** of ids. A part whose text
  changed, or that is new, carries its `text`. A part whose text is
  unchanged carries `hash` and no `text`: the SHA-256 of its text in
  the format's notation, `sha256:` and lowercase hexadecimal, and its
  text is the one the path already has for that id. A part the list
  leaves out is removed. Order is therefore explicit in every delta,
  and an unchanged part costs one id and one hash.
- A part named by `hash` alone also keeps the `source` it had on the
  path, since the hash form has no way to say that a part has none
  now. A writer that clears or changes a part's `source` writes the
  part's `text` with it.
- A part that carries neither `text` nor a `hash` this path can
  resolve has no text a reader can rebuild. When the same entry
  carries `instructions`, that string stands: it is the only record of
  what the model was sent, and a reader takes it over the join of
  parts it cannot resolve. Without it the instructions cannot be
  rebuilt and the `request_hash` will not verify, which is how such a
  file is found.
- A writer MUST NOT write a delta with `replace: true` whose parts are
  named by `hash` alone without `instructions` beside them: the
  replace discards the parts the hashes would have resolved against,
  so nothing on the path can rebuild them.
- `replace: true` discards the parts with the rest of the settings,
  so a `hash` in the same entry resolves against nothing; a delta that
  sets `instructions` as a string and no parts replaces the
  composition, and the parts no longer describe what is in force.

`instructions_omitted` records the parts the writer considered and
left out, each with its `id`, a `reason` in the writer's own terms,
the `size` in bytes it would have added, and optionally a `source`.
It is not settings: nothing in it reaches the request, it does not
replay, and it applies to the entry that carries it. It is where a
walk that dropped an instruction file for a budget, or a memory the
render left out, is recorded, so a session says what the model was not
given as well as what it was.

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
   "reasoning":{…},"text":{…},"tools":[…],"extra":{…}}
  ```

  `instructions_parts`, when the checkpoint carries it, is the full
  list of parts in force, each with its text, not a delta, and
  `instructions` is their join. `tools` is the full list of tool
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
 "source":"input|resume","ref":"…"}
{"type":"run","id":"…","parent":"…","ts":"…","run_id":"…","phase":"end",
 "reason":"done|stopped|interrupted|input_required|aborted|error",
 "ref":"…",
 "pending":["call_…"]}
```

- `run_id` and `phase` are required on both entries. `source` is
  required on `start`; `reason` and `pending` are required on `end`.
  `ref` is optional on both.
- `source` is closed to two path shapes. `resume`: at least one call
  that was on the path with no output when the run began has its
  output at the start of the segment, whether the previous run ended by
  leaving it pending or was cut off. `input`: otherwise, including a
  run that answers nothing and adds nothing, such as a retry after an
  error, which `ref` names. A run that both answers a pending call and
  adds a message is `resume`. `ref` on `start` names what triggered
  the input (a cron name, a channel message ID). How an input arrived,
  whether a schedule, a channel or another agent, is a harness feature
  and goes in `ref` or a `custom` entry.
- `reason` is closed. Each value is a shape of the run's segment, the
  entries on the path from the `start` entry to the `end` entry, where
  a pending call is a `function_call` on the segment with no
  `function_call_output` on it. Five values are computable, tested in
  this order with the first match winning:
  1. `error`: the last `response` on the segment carries a non-null
     `error`.
  2. `input_required`: at least one pending call is held, as `decision`
     defines it, and no pending call has a `dispatch`.
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

  Two values record what the segment cannot show and are written, not
  computed: `error` when the harness failed at any point, which `ref`
  names, and `interrupted` when a person or the host told the harness
  to stop. A written `error` or `interrupted` stands over any segment.
  Every segment matches exactly one computable value on its path; a
  reader MAY recompute it, and when the written value is computable and the two
  disagree the segment is authoritative.
- `pending` lists the pending calls' IDs so a resume can read them
  without walking the segment. The segment is authoritative here too.
- Items and responses of the run follow its `start` entry on the path.
  Runs do not nest: an input that arrives while a run is open joins
  that run. A branch closes the open run without an `end` entry, since
  the new leaf is not on its segment; the next append on the new
  branch begins a run. When the header names `run` in `records`, no
  writer holds the file open and the leaf is on the run's segment, a
  run with no `end` entry was cut off; that is the crash signal.

### `dispatch`

A call was handed to its tool.

```json
{"type":"dispatch","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id"}
```

`call_id` and `target` are required; `target` is the `item` entry
holding the `function_call`. A call with a `dispatch` and no
`function_call_output` on the path was in flight when the record
stopped, and its side effect may have happened. When the header names
`dispatch` in `records`, a call with neither was never started;
otherwise the file does not say whether it ran. A writer that names
`dispatch` MUST write it, durably, before the tool runs, and no writer
may write it for a call that was rejected. A `dispatch` with no
`decision` before it on the path means no decision was recorded for
the call, which under the rule in `decision` is the shape of a routine
approval as much as of no decider at all; a writer that records no
decisions produces a valid file. A call cancelled after its `dispatch`
carries no decision: its `function_call_output`, or the absence of
one, is the record.

### `decision`

A call's fate was decided outside the tool.

```json
{"type":"decision","id":"…","parent":"…","ts":"…",
 "call_id":"call_…","target":"entry-id",
 "verdict":"proceed|reject|hold","by":"human|policy|agent",
 "reason":"…","args":{…}}
```

- `verdict` is closed. Each value says what this decision did, and
  the path shows whether it held:
  - `proceed`: this decision let the call go to its tool. A `dispatch`
    for the call follows.
  - `reject`: this decision ended the call. No `dispatch` ever follows,
    and a `function_call_output` for the call follows that carries
    `reason`.
  - `hold`: this decision neither let the call go nor ended it. The
    call waits. A call is held while its latest decision is a `hold`
    with no `dispatch` and no `reject` after it on the path.

  A call may carry several decisions on the path, in order. An answered
  `hold` is followed by a `dispatch` or a `reject` on the same call and
  stays as written; a call still held is what makes a run end
  `input_required`. There is no separate verdict for an answer. A
  writer SHOULD write `proceed` only when it answers an earlier `hold`
  or carries `args`; otherwise the `dispatch` is the record that the
  call proceeded.
- `call_id`, `target` and `verdict` are required. `reason` is required
  when `verdict` is `reject`, since it is what the model saw as the
  output and what tells a rejected call from a tool failure; otherwise
  `reason`, `by` and `args` are optional. `by` is closed: `human` is a
  person, `policy` is a rule the harness evaluated without waiting,
  `agent` is another model.
- `args`, when present, are the arguments the tool ran with when a
  decision rewrote them. The `function_call` item stays as the model
  produced it, so the request hash still verifies; the change is
  recorded beside the call, never inside it.

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
  joins a run already in flight lives, since the `run` entry's `ref`
  names what started the run and not what arrived during it: two
  people steering one run are two triggers.
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
members: `source`, the trigger the queued entry held, and
`queued_from`, the ID of that entry. `source` on an `item` is not
restricted to a drained input: any item a person or another system
sent rather than the model or the loop may carry it.

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
tag moves. A container on a remote host is `container`, with the
digest as `ref` and the host in a member this document does not
define. Anything richer goes in such members too, which the envelope
section says a rewriter preserves. An `env` entry applies from its
position on the path until the next one.

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
   `parents` is provenance and MUST NOT be walked.
2. Replay `config` entries along the path in order to produce settings,
   honouring `replace`. A `config` that carries `instructions_parts`
   resolves them against the parts in force, as that member defines,
   and the settings' instructions are the resolved parts joined with
   one blank line.
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

## Projections

### ATIF

One ATIF document per root-to-leaf path. `session_id` is the header ID;
`trajectory_id` is the leaf entry ID. The mapping is given in the
accompanying session plan and is normative for the Open Responses
profile: user and system items to steps, one `response` with its items
to one agent step with `tool_calls`, `reasoning_content` and `metrics`,
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
call's decisions and dispatch travel under `calls`, keyed by call ID,
in the `extra` of the agent step that produced the call; a queued
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

## Versioning

`format` is `agentsession/<major>.<minor>`. A minor version adds entry types or
optional fields. A major version changes the envelope, the header, or
the context algorithm. Readers MUST accept any minor version of a major
they support. Files are migrated in memory, never rewritten in place.

Adding an optional member to the envelope is a minor change. Changing
what an existing member means, or what the context algorithm does with
any member, is major — which is the line an addition has to stay behind
to arrive in a minor version at all.

## Conformance

A conforming **writer** produces files that satisfy every MUST in this
document. A conforming **reader** implements the context algorithm and
the preservation rules. A conforming **converter** from a native format
documents which native entries it maps and which it drops.

The reference implementation is the Go `agentsession` library. The
conformance suite is a directory of fixture files with expected context
output for every leaf, expected `request_hash` values, the recomputed
`reason` for every `run` end, and negative cases for a broken parent
link, a truncated last line, an unknown type, a `dispatch` that
follows a `reject`, and a header naming `dispatch` beside a call that
has an output and no `dispatch`. Converters for pi, Claude Code and
Codex are part of the initial proposal so the format arrives with three
existing corpora behind it.

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

- Whether the current leaf needs a durable marker. Held: a reserved
  `label` a library honours on open covers it without a format change.
  What that library does with the marker is not specified here, and two
  conforming readers can currently disagree about where a reopened
  session resumes — which for a resume format is the last resume-
  critical algorithm left unwritten.
- Whether a core `exchange` type is worth defining for the common case
  of one entry converging several subagent results, or whether
  `parents` on an `item` already covers it. Held: `parents` covers it,
  and a type earns its place only once a reader needs to treat the
  convergence differently from the item that carries it.
- What supplies the total order when entries do not share one file.
  Within a file, file order is the ordering and `ts` MUST NOT be used
  for it. A store that holds entries outside a single file needs a
  replacement, and resolving a durable leaf marker depends on having
  one.
- Whether to allow a second payload profile at 0.x, or hold the line at
  Open Responses and rely on converters.
- Sidecar media layout and naming.
- The venue: this repository, a standalone repository, or a proposal to
  openresponses.org as a companion document.
