# RFC 0001: Agent Session Format

Status: draft 0.1
Author: Christopher Davenport
Discussion: to be opened against this repository, then proposed to the
Open Responses community as a companion specification.

## Summary

An append-only, tree-structured JSONL format for recording one agent
session: what the model was sent, what it returned, what tools did, how
the configuration changed, where the conversation branched, and what
happened afterwards. The envelope is harness-neutral. The payload of a
conversation entry is an Open Responses item, so the record is the wire
format itself.

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

## File

- UTF-8, one JSON object per line, lines terminated by LF.
- The first line MUST be the header. Every later line MUST be an entry.
- A reader MUST tolerate a final line that does not parse and MUST
  report it. A writer MUST NOT rely on such recovery.
- Media referenced by items MAY be stored inline as data URLs or beside
  the file under a directory named after the session ID. A header field
  says which.

## Header

```json
{"type":"session","format":"agentsession/0.1","id":"…","created_at":"2026-09-17T12:00:00Z",
 "payload":"openresponses/2026-04-24","harness":{"name":"…","version":"…"},
 "cwd":"/path","parent_session":"…","media":"inline"}
```

| field | req | meaning |
|---|---|---|
| `type` | MUST | the string `session` |
| `format` | MUST | `agentsession/<major>.<minor>` |
| `id` | MUST | globally unique; UUIDv7 RECOMMENDED |
| `created_at` | MUST | RFC 3339 |
| `payload` | MUST | payload profile; `openresponses/<spec-date>` is the only profile this RFC defines |
| `harness` | SHOULD | name and version of the writer |
| `cwd` | MAY | working directory at creation |
| `parent_session` | MAY | session ID this was forked or spawned from |
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

### `label`, `info`

```json
{"type":"label","id":"…","parent":"…","ts":"…","target":"entry-id","label":"checkpoint"}
{"type":"info","id":"…","parent":"…","ts":"…","name":"Refactor auth"}
```

Not in context. A `label` with `label: null` clears.

### `env`

A snapshot of the environment for replay.

```json
{"type":"env","id":"…","parent":"…","ts":"…",
 "cwd":"…","vcs":{"system":"git","revision":"…","dirty":true},
 "files":{"read":{"path":"sha256:…"},"written":{"path":"sha256:…"}},
 "tools":{"name":"version"}}
```

### `outcome`

A signal about how the session, or a range of it, went.

```json
{"type":"outcome","id":"…","parent":"…","ts":"…",
 "kind":"feedback|test|task|tool_error|custom","target":"entry-id",
 "score":1.0,"label":"…","details":{…}}
```

### `link`

A reference to another session, for subagents and forks.

```json
{"type":"link","id":"…","parent":"…","ts":"…",
 "rel":"subsession|fork_of|continued_in","session":"…","call_id":"…"}
```

`call_id` ties a subsession to the function call that spawned it.

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
   or a `compaction` selected in step 3. Every other core type and every
   unknown extension contributes nothing.
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

- Output items MUST be written only when complete. Partial streaming
  state MUST NOT be written as an `item`.
- A `response` entry MUST follow the items it envelopes and MUST be the
  last entry written for that model call.
- A user item or function call output SHOULD be written before the
  request that includes it is sent.
- Writers SHOULD fsync at least on each `response` entry.

## Projections

### ATIF

One ATIF document per root-to-leaf path. `session_id` is the header ID;
`trajectory_id` is the leaf entry ID. The mapping is given in the
accompanying session plan and is normative for the Open Responses
profile: user and system items to steps, one `response` with its items
to one agent step with `tool_calls`, `reasoning_content` and `metrics`,
function call outputs to observations by `source_call_id`, compaction
and branch summaries as copied-context system steps, `link` entries to
`subagent_trajectories`. Raw items travel in step `extra` so the
projection is lossless.

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
output for every leaf, expected `request_hash` values, and negative
cases for a broken parent link, a truncated last line and an unknown
type. Converters for pi, Claude Code and Codex are part of the initial
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
versioning, and adds the entries that none of them record: environment,
outcome and cross-session links.

## Open questions

- Whether to allow a second payload profile at 0.x, or hold the line at
  Open Responses and rely on converters.
- Sidecar media layout and naming.
- The venue: this repository, a standalone repository, or a proposal to
  openresponses.org as a companion document.
