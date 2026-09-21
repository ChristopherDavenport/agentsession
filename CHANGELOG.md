# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

- RFC 0001 is revised to draft 0.3 and the library writes
  `agentsession/0.3`; 0.2 and 0.1 files read unchanged. Context
  building now states how the request context of one response is
  found, since two implementations rebuild it: walking back from the
  response entry, an entry that is not an item is skipped, an item
  whose `response` names the response is output, and the walk stops at
  the first item naming another response or none. The output items of
  one response need not be contiguous (#40).
- `Session.RequestContext` follows that rule, so a custom or record
  entry written between two output items of one response no longer
  truncates the rebuilt request. A composed product that records a
  guard's verdict where the guard runs passes `verify` again; only the
  response's own item entries are removed from the path, and every
  other entry stays where it was (#40).

- **Breaking.** `ComputeReason` takes the run's path as well as its
  segment, `Run` carries the `Path` the segment ends, and the
  `stopped` step of the cascade reads it: a run that answers a call an
  earlier run's model call made and ends without calling the model
  again is `stopped`, not `aborted`. A refusal and a resume whose
  approved call terminates are both that shape, and a recorder that
  wrote what happened failed `Run.Verify` before. Passing the segment
  for both arguments, or a `Run` built by hand, reads as it did. The
  cascade's `aborted` step no longer catches a segment with no
  response; a sixth step does, so every segment still matches exactly
  one value. One consequence is deliberate: a call an earlier run left
  without an output keeps a later run from reading as `stopped` (#34).

## v0.0.5 - 2026-09-20

- RFC 0001 is revised to draft 0.2. The summary now defines a session as
  two kinds of entry, context and record, and the core types section
  states the test for admitting a core type. Three record entries are
  added: `run` (source and end reason), `dispatch` (a call handed to its
  tool) and `decision` (a call's fate, any rewritten arguments and,
  optionally, who decided it). Run end reasons and decision verdicts are
  defined as shapes of the path a reader can recompute, so they belong
  to the format rather than to one harness, and the reasons form a
  first-match cascade with `error` and `interrupted` as the two values a
  writer adds; how an input arrived and who decided a call are a
  harness's own detail. The header gains `records`, the record types
  whose absence a reader may take as the event not having happened, so a
  converter over a log with no tool-start record does not assert that a
  call never ran; such entries are durable before the side effect they
  precede. Members of a core entry the RFC does not define are
  preserved. `outcome` gains `pass`, an `eval` kind, an unbounded
  `score` and a `target` that is an entry ID by rule; `env` gains
  `workspace`, a kind and one reference; the header also gains
  `spawned_by` and derived subsession IDs; `link` is written at
  dispatch; entry order, not `ts`, is the ordering. Instructions as
  parts, a durable leaf and a source on queued input are held as open
  questions. Specification only; the library follows in a later release
  (#15, #16, #17, #21, #24, #27, #28).
- The library implements draft 0.2 and writes `agentsession/0.2`; 0.1
  files read unchanged. New entry types `RunEntry`, `DispatchEntry` and
  `DecisionEntry` with their constants and constructors;
  `OutcomeEntry.Pass` and `OutcomeEval`; `EnvEntry.Workspace` and
  `SetWorkspace`; `Header.Records`, `Header.SpawnedBy`,
  `Header.HasRecord` and `AllRecords`; `SubsessionID`, the UUIDv5
  derivation the RFC recommends. `Calls`, `Session.Calls` and
  `Session.PendingCalls` collect each function call with its decisions,
  dispatch and output, and `Call.State` reads the header's records to
  say whether a missing dispatch means never started or unknown. `Runs`,
  `Session.Runs`, `Session.OpenRun` and `Session.EndRun` partition a
  path into run segments and build the end entry with its pending list;
  `ComputeReason` is the RFC's cascade and `Run.Verify` checks a written
  reason against it. `Session.VerifyRecords` checks run ends, forbids a
  dispatch after a reject and, when the header promises dispatches,
  requires one on every call that ran; `Session.Append` refuses a
  dispatch for a rejected call with `ErrCallRejected`. The jsonl store's
  `SyncOnResponse` now also syncs function call outputs and any record
  entry the header names, as the format requires. The ATIF export
  carries a run's source, trigger, reason, cause and pending calls under
  `run` on the first step of its segment and a call's decisions and
  dispatch under `calls` on the agent step that produced it.
  `agentsession verify` runs the record checks on every leaf, and `show`
  renders the new entries and header members.

- The ATIF export passes every entry's unknown members through to the
  document, as the store already did on a round trip; a config entry's
  ride under `config_extensions` (#29).
- `jsonl.WithStaleLockReport` tells a host when Create or Open takes
  over the lock of a process on this host that no longer runs, the only
  durable sign of a crash between two appends (#26).
- The leaf is durable: appending `Session.MarkLeaf`, a label entry
  carrying `LeafLabel`, keeps the leaf at its target and `Read` restores
  it from the last such label, so a branch survives a restart. A null
  label on the target clears it (#24).
- The sqlite store returns `ErrConcurrentWriter` when another process
  appended since it loaded a session, and refuses the session until
  `Release`, instead of reloading and re-parenting under a leaf the
  agent never saw (#18, the floor; a cross-process lock can follow).
- The ATIF export counts a compaction's or branch summary's usage under
  the step's `usage`, prices it under `cost_usd` when a price source is
  given, and adds both to the totals; a compacting agent no longer
  under-reports (#20).
- `atif.Validate` enforces Harbor's closed sets: four image media types,
  eight audio types with aliases normalised as Harbor normalises them
  (`atif.NormalizeAudioMediaType`), nine schema versions
  (`atif.SchemaVersions`), and the timestamp forms Harbor accepts, naive
  times and bare dates included. `atif.Parse` stays lenient on the
  schema version. The exporter emits one of the four image types or
  degrades the part to a text placeholder (#23).
- `export.NoPassthrough` strips the raw items from a document for a
  judge or a publication; `Items` then reports `ErrNoRawItems` (#22,
  first half).
- New nested module `otel`, the RFC's OpenTelemetry projection:
  `otel.Export` replays a session as spans with the entries' own
  timestamps, and `otel.Wrap` decorates a store so a live harness emits
  the same spans as it appends, resumes included. Session, run,
  inference and tool spans carry `session.id` and the entry ID; a tool
  span links the inference that produced it, an inference span links the
  previous turn's, and the first span after a branch links the branched-
  from entry when its span is known.

- `Continue` rolls a session over into a successor in the format's
  order: a new session with `parent_session`, a full config from the old
  leaf's settings, the summary, the display name, and a `continued_in`
  link on the old session. `Session.SupersededBy`,
  `Summary.SupersededBy` and `ListFilter.Current` let a listing show the
  successor rather than the session it retired, in every store;
  `agentsession list -current` does the same (#19).
- The sqlite store holds each open session in a `holders` table, so a
  second process gets `ErrSessionLocked` naming the holder instead of
  interleaving entries. A hold left by a dead process on this host is
  taken over and reported through `sqlite.WithStaleLockReport`;
  `LockHolder` and `BreakLock` mirror the jsonl store's; an append after
  a broken hold fails with `ErrSessionLocked` and refuses the session
  until `Release` (#18). `sqlite.Open` takes options.
- `export.ItemsFrom` rebuilds a conversation from a document's declared
  fields alone, so a document from any producer loads; its doc comment
  lists what is lost (#22).

## v0.0.4 - 2026-09-19

- `Context.ItemEntries` is aligned with `Context.Items`: the entry that
  contributed each item, so an index into the request input maps back
  to an entry without counting item entries by hand.
  `Session.CompactFrom(first, summary)` and
  `Session.CompactKeeping(kept, summary)` build a compaction from an
  item index or a kept count; both refuse to keep an earlier
  compaction's summary, which the context algorithm drops once a later
  compaction is on the path (#14).

## v0.0.3 - 2026-09-19

- The RFC now specifies the `config` checkpoint a `compaction` carries:
  the `Settings` shape with `tools` as the full list in force and
  `extra` as the merged passthrough map after null deletions (#13).
- `Session` stamps the header's `created_at` and each appended entry's
  `ts` in UTC, so a file written across a timezone change carries one
  offset. Timestamps the caller supplies are kept as given (#9).
- `jsonl`: each open session is guarded by an advisory lock file,
  `<file>.lock`, recording the holder's PID and host. A second process
  that opens, appends to or deletes the session gets
  `jsonl.ErrSessionLocked` instead of interleaving lines. `Release`,
  `Delete` and `Close` drop the lock; a lock left by a process on the
  same host that no longer runs is taken over; `LockHolder` reports the
  holder and `BreakLock` clears a lock from any other holder (#6).
- `cmd/agentsession`: a command over session files. `show` prints the
  entries in file order with forks, leaves and labels marked, then the
  context at the leaf or at `-leaf`; `verify` rebuilds every request
  and checks its hash, exiting 1 on a mismatch or a truncated final
  line; `export` writes ATIF documents for every leaf with `-secret`,
  `-redact-home` and `-redact-env` redaction; `list` prints a jsonl
  store's sessions. Standard library only, so the root module's
  dependency rule holds (#7).
- `Summary.Name` carries the session's display name, and
  `ListFilter.WithNames` asks for it. The in-memory store always fills
  it; `sqlite` keeps a `name` column current on info appends and
  migrates a database from v0.0.2 on open; `jsonl` scans each file's
  entries only when asked, so the header-only listing stays the cheap
  default. The `list` command shows it (#4).
- `ConfigEntry.SetExtra` and `ClearExtra` write a passthrough request
  member, or the null that removes it on replay, without touching raw
  JSON; `Settings.ExtraValue` decodes one back (#8).
- `export.Trajectories` takes `Preference` functions that decide which
  child of a fork was continued: `PreferCurrentLeaf`, `PreferLabel`,
  `PreferScore` (highest-scored outcome), with `PreferLatest`, the
  previous rule, as the fallback. `Options.Preferences` applies them to
  embedded subsessions; the `export` command takes `-prefer` (#11).
- `Read` and `UnmarshalEntry` split each line into its members once
  and decode the envelope, the unknown-member check and item bodies
  from the split, instead of parsing the line up to four times.
  `BenchmarkRead` measures it: sessions of 100 KB tool outputs read
  about twice as fast, sessions of short lines about half again (#10).

## v0.0.2 - 2026-09-19

- One version per repository. The `sqlite` module's `go.mod` requires
  the released root next to a `replace` that builds against the tree,
  so `go get` works for consumers and the checkout needs no workspace.
  `make release VERSION=` sets the requirement, dates the changelog,
  and tags the root and `sqlite` at one commit; the release workflow
  publishes nested tags too.
- `make check` now includes `tidy-check`, which fails when `go mod tidy`
  would change any module's `go.mod` or `go.sum`; CI uses the same target.

## v0.0.1 - 2026-09-19

- Initial implementation of the Agent Session Format (`agentsession/0.1`)
  over Open Responses items: the header and every core entry type, with
  unknown entry types, namespaced items, unknown members on known
  entries and unknown header fields preserved byte for byte.
- `Session`: the in-memory tree with leaf operations, in-place
  branching, multiple roots, labels and names.
- `Read` and `Write` for the JSONL form; a truncated final line loads
  and is reported through `Session.Truncated`.
- The context algorithm (`Session.ContextAt`, `BuildContext`) with
  config replay, `replace`, tool add/remove and compaction; golden
  fixtures under `testdata/context`.
- `RequestHash` and `HashRequestJSON`: RFC 8785 canonical JSON and
  SHA-256, with golden vectors in `testdata/hash/vectors.json` for other
  implementations; `Session.Verify` checks stored hashes.
- `Store` interface, `MemoryStore`, and the `jsonl` file store with a
  sync policy, `<root>/<project-key>/<created-at>_<id>.jsonl` layout
  and crash recovery. The `storetest` package is the shared suite.
- Helpers for `env`, `outcome`, `link` and `label` entries, including
  file hashing and reading the git revision without a git binary.
- `RecordResponse` writes one model call (output items and the response
  entry with the request hash) and `ConfigFromRequest` builds the full
  initial config from a request. `Session.Compact` and
  `Session.SummarizeBranch` build compaction and branch summary entries
  with the checkpoint filled in.
- `sqlite`: a SQLite store as a nested module over `modernc.org/sqlite`,
  passing the same suite.
- `atif`: Go types for ATIF v1.8 with unknown-member passthrough and a
  validator mirroring Harbor's.
- `export`: trajectories per leaf with preference-pair marking, `ToATIF`
  with lossless extras, subsession embedding, redactors for secrets,
  home paths and environment snapshots, and `WriteATIF` which spills
  inline media beside the documents. A session's current path is its
  main trajectory, written as `<session-id>.json`, which is where an
  unresolved subsession reference points.
