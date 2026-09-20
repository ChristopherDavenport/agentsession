# Plan: session layer

A standalone library for recording, resuming, branching and exporting
agent conversations. JSONL is the default store and the reference format.
Sessions double as training material for improving the agent loop, so
the format records what training needs, not only what resume needs.

The on-disk format is specified separately in
[RFC 0001: Agent Session Format](../rfcs/0001-agent-session-format.md);
this plan covers the library that implements it.

The methodology is pi's session manager: an append-only file, a header
line, entries forming a tree through `id` and `parentId`, a current leaf,
in-place branching, and compaction entries that mark the first kept
entry. The payload differs from pi's. Entries hold Open Responses items
verbatim.

## Goals

- Lossless. What the model was sent and what it returned are stored as
  the wire carried them, reasoning and unknown types included.
- Replayable. Every model call in a session can be rebuilt exactly from
  the path to its entry: settings, items, compaction.
- Append-only and crash-tolerant. A session file is always a valid prefix
  of the run.
- Tree-shaped. Branching creates children of an earlier entry without a
  new file. Abandoned branches are kept because they are the best
  preference signal in the data.
- Exportable. A pipeline from trees to linear trajectories to ATIF
  documents, with redaction, so sessions drop into Harbor, Agent Data
  Protocol, Phoenix and Letta pipelines without a bespoke adapter.
- Independent of the agent layer's types. Depends on `openresponses`
  only. The subscriber that feeds a live `agentturn` run into a session
  lives in `agentturn` as a nested module and imports this one; this
  module never imports it.

## Non-goals

- Running the loop. The session is a subscriber and a source of
  transcripts.
- Multi-writer concurrency on one session file. One writer per session;
  a fleet uses the SQLite or a server-backed store.
- Rendering. Fronts read the tree; the library gives them the walk.

## Module and packages

```
agentsession/         entries, tree, context building, Store interface, in-memory store
agentsession/jsonl    default file store
agentsession/sqlite   sibling module, separate go.mod, never a core dependency
agentsession/atif     Go types for ATIF v1.8 with unknown-field passthrough
agentsession/export   trajectories, ATIF conversion, redaction, writers
```

## File layout (jsonl)

```
<root>/<project-key>/<timestamp>_<session-id>.jsonl
```

`project-key` derives from the working directory as pi does: leading
separator stripped, path separators and colons replaced by `-`. The
session ID is a UUIDv7 unless supplied.

## Header

First line, not part of the tree.

```json
{"type":"session","version":1,"id":"…","created_at":"…","cwd":"/path",
 "spec_version":"2026-04-24","parent_session":"…optional…"}
```

`spec_version` is `openresponses.SpecVersion` at write time so a reader
knows the item shapes. `version` is the session format version; loaders
migrate older versions in memory as pi does.

## Entry base

```go
type Entry interface {
    Base() *EntryBase
    EntryType() string
}

type EntryBase struct {
    ID        string    `json:"id"`         // short unique, UUIDv7 fallback
    ParentID  string    `json:"parent_id"`  // "" for a root
    Timestamp time.Time `json:"timestamp"`
}
```

Unknown entry types decode to `UnknownEntry`, which keeps the raw line
and re-emits it unchanged, matching openresponses' handling of unknown
items.

## Entry types

| type | in model context | purpose |
|---|---|---|
| `item` | yes | one `openresponses.Item`, verbatim |
| `response` | no | envelope of a model response: response id, model, status, usage, incomplete reason, timing, request hash |
| `config` | replayed | delta to request settings: model, instructions, tools added and removed, reasoning, text format, extra |
| `compaction` | yes | replaces everything before `first_kept_entry_id`; holds either the server's `Compaction` item or a local summary item, plus a full config checkpoint |
| `branch_summary` | yes | summary of an abandoned path; `from_id` is the leaf left behind |
| `custom` | no | app state keyed by `custom_type` |
| `custom_item` | yes | app-injected item that the model sees, keyed by `custom_type`, with `display` |
| `label` | no | bookmark on `target_id` |
| `session_info` | no | display name and other metadata |
| `environment` | no | cwd, git revision, file hashes read and written, tool versions |
| `outcome` | no | feedback, test results, task status, tool error counts, free-form details |

`config` replaces pi's separate model-change and thinking-level-change
entries, and takes over what pi carries in system messages, because in
Open Responses instructions and tools are request fields rather than
items. The first entry of a session is a `config` with the full initial
settings; later ones are deltas.

`item` entries for a model response carry `response_id` so the `response`
entry that follows them can be joined without positional reasoning.

## Tree

```go
type Session struct { /* header, entries by id, children index, leaf */ }

func (s *Session) Leaf() string
func (s *Session) Entry(id string) (Entry, bool)
func (s *Session) Children(id string) []string
func (s *Session) Path(from string) []Entry            // from to root, reversed to root-first
func (s *Session) Branch(id string)                    // move leaf
func (s *Session) ResetLeaf()                          // next append becomes a new root
```

Every append returns the new entry ID and sets it as the leaf. Multiple
roots are allowed, as in pi.

## Context building

```go
type Context struct {
    Settings Settings            // replayed config: model, instructions, tools, reasoning, text
    Items    openresponses.Items // ready to be the request input
    Entries  []Entry             // the selected entries, for renderers
}

func (s *Session) BuildContext() (Context, error)
```

Algorithm, following pi:

1. Walk leaf to root and reverse.
2. Replay `config` entries along the whole path to get `Settings`.
3. If the path contains compaction entries, take the latest. Emit its
   config checkpoint as the settings baseline, its summary item first,
   then entries from `first_kept_entry_id` up to the compaction entry,
   then everything after it.
4. Map entries to items: `item` and `custom_item` yield their item,
   `compaction` and `branch_summary` yield their summary item, all other
   types yield nothing but stay in `Entries`.

A test asserts that the request rebuilt for any stored `response` entry
hashes to the `request_hash` recorded on it. The hash is defined in the
RFC (JCS canonical form, SHA-256) and implemented here as
`agentsession.RequestHash(openresponses.Request) string`, with golden
vectors so `agentturn` can test its own copy against the same inputs.

## Store interface

```go
type Store interface {
    Create(ctx, Header) (*Session, error)
    Open(ctx, id string) (*Session, error)
    Append(ctx, sessionID string, e Entry) (id string, err error)
    List(ctx, ListFilter) iter.Seq2[Summary, error]
    Delete(ctx, id string) error
}
```

`Append` is the only write. The JSONL store writes one line per call and
offers a sync policy: every append, every terminal response, or never.
Loading tolerates a truncated final line and reports it.

## Writing discipline

These come from the barrier decision in the agent layer and pi's rule
that a pending stop reason never reaches disk. They are stated here
because the format depends on them; the subscriber that enforces them
against a live run is `agentturn/session`, built against this list.

- Assistant items are appended on `item_end`, never from partial stream
  state.
- The `response` entry is appended after its items, on the terminal
  response event, and carries the request hash computed from the exact
  request sent.
- User items and function call outputs are appended when the loop
  emits their `item_end`.
- The session subscriber returns only after the append has been written
  according to the sync policy. Because the agent treats assistant
  `item_end` as a barrier, tool preflight cannot start before the
  assistant item is durable.
- `environment` entries are written at session start and after each tool
  batch that reported file writes.

## Export

The export target is Harbor's Agent Trajectory Interchange Format (ATIF,
RFC 0001, currently v1.8). It is the de facto interchange for agent
training data: Harbor's SFT exporter accepts only ATIF, neulab's Agent
Data Protocol uses it as its interchange layer before per-agent SFT
conversion, Arize Phoenix imports it into OpenTelemetry spans, Letta's
trajectory library reads it, and community converters exist for Claude
Code, Codex and Copilot logs. Emitting ATIF puts sessions from this
library into every one of those pipelines without a bespoke adapter.

ATIF is a whole-trajectory JSON document with no append semantics and no
branching, so it is an export. The session JSONL tree remains the record. A session exports as one ATIF document per
root-to-leaf path; all documents from one session share `session_id`
and each carries its own `trajectory_id`, the distinction ATIF v1.7
introduced for this case.

```go
// One linear path per leaf, root to leaf, compaction applied.
func Trajectories(s *Session) iter.Seq[Trajectory]

// ATIF document for one path. Lossless: raw items ride in step extras.
func ToATIF(t Trajectory, opts ATIFOptions) (*atif.Trajectory, error)

type Redactor interface{ Redact(*atif.Trajectory) error }

func WriteATIF(dir string, docs iter.Seq[*atif.Trajectory]) error  // one JSON file per trajectory, media in images/ and audio/
```

There is no separate native record format. Anyone who wants the wire
shape back reads it out of `extra`.

### Mapping

| session | ATIF |
|---|---|
| header `id` | `session_id` |
| one root-to-leaf path | one document, `trajectory_id` = leaf entry ID |
| replayed `config` at the first step | `agent.name`, `agent.version`, `agent.model_name`, `agent.tool_definitions` |
| `config` change mid-path | `model_name` and `reasoning_effort` on the following agent steps; tool changes recorded in step `extra` |
| `item` with a user message | step `source: "user"`, `message` as text or content parts |
| `item` with a developer or system message, and `instructions` | step `source: "system"` |
| assistant message items and the `response` entry for one model call | one step `source: "agent"`: `message` from output text, `reasoning_content` from reasoning items, `tool_calls` from function calls, `metrics` from the response usage, `llm_call_count: 1` |
| `function_call_output` items for that call | `observation.results[]` with `source_call_id` = `call_id` |
| `response.usage` | `metrics.prompt_tokens`, `completion_tokens`, `cached_tokens` from the input token details; `cost_usd` only when a price source is configured |
| `compaction` | the ATIF context-management convention; steps before `first_kept_entry_id` are not emitted, the summary item becomes a `source: "system"` step with `is_copied_context: true` |
| `branch_summary` | `source: "system"` step, `is_copied_context: true`, with `extra.branch_from` |
| `custom_item` | step by its role, `extra.custom_type` |
| `custom`, `label`, `session_info` | `extra` on the nearest following step, or `extra` on the root |
| `environment` | root `extra.environment` for the first, step `extra.environment` afterwards |
| `outcome` | root `final_metrics.extra.outcome`, and `extra.outcome` on the step it attaches to |
| subagent sessions | `subagent_trajectories[]` embedded, referenced from the tool observation through `subagent_trajectory_ref` |
| tool results with image or file parts | `content` as ATIF content parts, media written beside the document |

`final_metrics` sums the path: total prompt, completion and cached
tokens, cost when known, total steps.

### Lossless extras

Every agent step carries `extra.openresponses` with the raw output items
and the response envelope verbatim, every user and system step carries
the raw input item, and the root carries `extra.openresponses.spec_version`
and the session format version. A test round-trips a session to ATIF and
back to a path of entries and asserts the items are byte-identical.

### Preference pairs

Two children of one parent where one path was continued and the other
abandoned are a preference pair. The exporter marks the continued
trajectory with `extra.preferred_over` listing sibling `trajectory_id`s,
and the abandoned one with `extra.abandoned_at` naming the divergence
step. ATIF has no first-class field for this, so the convention lives in
`extra` and is documented alongside the exporter for upstream proposal.

Which child was continued is a judgement. The default rule takes the
child whose subtree holds the most recently appended entry, which is
right when a user abandons a branch and moves on. `Trajectories` takes
`Preference` functions for the other cases: the branch holding the
session's current leaf, the branch holding an entry with a given label,
or the branch with the highest-scored `outcome`. The first preference
with an opinion decides; the default rule is the fallback.

### Redaction and publishing

Redaction runs at export, never at write, over the ATIF document and
its `extra` payloads alike. Built-in redactors cover exact secret values
from a supplied list, absolute paths under home, and environment entry
scrubbing. The publishing pipeline copies pi-share-hf: deterministic
redaction, a TruffleHog scan that blocks on any finding, an LLM review
with project context, a manifest with source and redacted hashes, then
upload. A dataset repo carries both the redacted native session files
and the ATIF documents.

### Observability projection

OpenTelemetry is a projection of the same data. The agent layer emits `invoke_agent`, inference and `execute_tool` spans with the
session ID and entry ID as attributes, and uses span links for the
relationships a span tree cannot hold: tool span to the inference span
that produced the call, turn to previous turn, first span after a branch
to the entry it branched from. Because ATIF and the spans both carry
entry IDs, a Phoenix-style ATIF import lands on the same identifiers the
live traces use.

## Milestones

1. Entry types, JSON encoding with unknown-entry passthrough, header and
   version migration scaffold.
2. Tree, leaf operations, `BuildContext` with compaction. Golden tests
   from hand-written JSONL fixtures.
3. In-memory store and JSONL store with sync policy and truncated-line
   recovery.
4. Extension round-trip: a fixture with namespaced entries and
   namespaced items (for example `agentturn:note`) loads, rebuilds
   context without them, and writes back byte-identical lines.
   `RequestHash` with golden vectors.
5. `environment` and `outcome` entries and the front-facing helpers to
   write them.
6. `atif` package: Go types for ATIF v1.8 with unknown-field
   passthrough, validated against the RFC's examples.
7. Export: trajectories, `ToATIF` with the mapping table, lossless
   extras round-trip test, preference pair marking, redactors, writer.
8. Publishing tool: redaction, TruffleHog, LLM review, manifest, upload,
   modelled on pi-share-hf.
9. SQLite store as a separate module with the same test suite.

## Open questions

- Whether `config` should store full instructions each time or a
  reference to a content-addressed blob, given prompts are large and
  change rarely.
- ID scheme for entries: pi's 8-hex-char with UUID fallback versus
  UUIDv7 everywhere.
- Whether images and file parts are stored inline or spilled to a
  sidecar directory with a hash reference.
- Whether `cost_usd` is computed in the exporter from a price table or
  left absent and filled by consumers, which is what most ATIF producers
  do today.
- Whether to propose `phase`, encrypted reasoning, outcome and preference
  fields upstream to the ATIF RFC once the `extra` conventions have been
  used for a while.
- Whether the `atif` package should live in this repo, the session
  module, or stand alone so other Go harnesses can use it.

## Implementation notes

Where this plan and the RFC disagree, the implementation follows the
RFC. The differences a reader of this plan should know about:

- Envelope members are `id`, `parent` (null for a root) and `ts`, not
  `parent_id` and `timestamp`. The header is `format`, `payload`,
  `harness`, `cwd`, `parent_session` and `media`, not `version` and
  `spec_version`.
- The entry types are the RFC's: `info` (not `session_info`), `env`
  (not `environment`), and no `custom_item`; application state that
  the model sees is an `item` entry holding a namespaced item, which
  decodes to `openresponses.UnknownItem` and round-trips byte for byte.
- An `item` entry names its model call through `response`, whose value
  is the payload's response ID, matching `response_id` on the
  `response` entry.
- The `config` checkpoint a `compaction` carries uses the shape of
  `agentsession.Settings`: `model`, `instructions`, `reasoning`,
  `text`, `tools` and `extra`. The RFC leaves the checkpoint shape
  open; this is the shape to propose.
- Entry IDs are eight hexadecimal characters, widened on collision;
  session IDs are UUIDv7. Both are opaque to readers.
- `config.extra` and `Settings.Extra` hold request members beyond the
  named ones as raw JSON; a null value in a delta removes the key.
  `Settings.Request` flattens them into `openresponses.Request.Extra`.
- The JSONL store cuts a truncated final line from the file when it
  opens a session, so later appends continue a valid file. The bytes
  removed were never a complete entry.
- ATIF export follows the mapping table above with these
  specifics: the compaction summary is a `system` step carrying the
  `context_management` convention with `boundary: "replace"` and its
  summary as the observation content; entries kept across the
  compaction are `is_copied_context: true`; every step carries
  `extra.openresponses` (the raw item, or the raw output items and the
  response entry) and `extra.agentsession.entry_id`; a `link` with
  `rel: subsession` embeds the child's continued path under
  `subagent_trajectories` when a resolver is supplied and otherwise
  becomes a `trajectory_path` reference to `<session-id>.json`, the
  name `WriteATIF` gives the child's main trajectory. The continued
  branch at a fork is the one whose subtree holds the most recently
  appended entry; the path ending at the last appended entry is the
  session's main trajectory.
