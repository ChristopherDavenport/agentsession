# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

- The ATIF export no longer guesses which of a subagent's leaves
  answered a call. A `link` names the child session and is written when
  the call is dispatched, before the child has a point to name; the
  output entry's `parents` names the leaf the answer was taken from,
  which is the first moment the parent knows it. Where the record says,
  the export follows it: the embedded subagent trajectory is that path,
  `trajectory_id` names it, and `extra.child_leaf` records that the
  document rests on the record. Where it does not, the projection picks
  the child's main path as before, and the absence of `child_leaf` is
  what says the document turned on that choice.
- A record naming a leaf the export cannot resolve embeds nothing,
  rather than putting a different path of the child in its place.
  `trajectory_id` still names what the record said, so the reference is
  precise even when the child is not loadable.
- `Call.From` returns what a call's output entry converged, so a
  consumer can read the same fact without walking `parents` by hand.
- RFC 0001 draft 0.4 is implemented and the library writes
  `agentsession/0.4`; 0.3, 0.2 and 0.1 files read unchanged. An entry
  may now carry `parents`, further predecessors it converges: the leaf
  of the subagent session that answered a call, a branch merged back,
  several workers joined at once. `EntryBase.Parents` holds them as
  `EntryRef` values, each naming an `entry` and optionally the
  `session` it is in, since a bare entry ID is unique only within a
  file.
- `parents` is provenance and nothing else. It is not walked when
  building a context, so `Path`, `ContextAt` and every path-derived
  query follow `parent` alone, and a 0.3 reader given a 0.4 file
  rebuilds the same request byte for byte — which is what keeps this a
  minor version. That is now measured rather than asserted: the
  round-trip fixture is read twice, once with its `parents` members
  stripped, and the rebuilt request hashes must match at every leaf.
- `Session.Append` sorts an entry's `Parents` by session then entry, as
  the format requires of a writer, so a file does not depend on the
  order the workers happened to finish in. A file that arrives unsorted
  is written back as it came; sorting is a rule for writers.
- `Append` and `Read` both reject a convergence that breaks the
  format's rules, with the new `ErrBadConvergence`: a reference naming
  no entry, one naming the entry's own parent, one naming the same
  entry twice, and one into this session naming an entry that does not
  exist yet — which with `parent` is what makes the structure acyclic.
  A reference carrying this session's own ID is held to the same rules
  as one that leaves the session out. A reference into another session
  is checked for shape only; resolving it is a store's job.
- `agentsession show` marks an entry that converges others. The PARENT
  column is unchanged, because it is still the whole of an entry's line
  of descent.
- A leaf label now names the branch that is live rather than the entry
  the leaf is pinned at, so a session marked and then written on resumes
  where it was written to instead of rewinding to the mark. `Read`
  resolves the leaf to the newest entry in file order that descends from
  the mark and was appended after it; a mark nothing followed resolves
  to itself, and a file with no mark still resumes at its last line,
  which is now the degenerate case of one rule rather than a second
  rule. The mark carries both coordinates a log entry has — where it
  points in the tree and where it sits in the file — and only the first
  was being read (#55).
- That defect was not confined to context. Every path-derived query
  takes a leaf, so a reopen that rewound past the mark reported no
  pending call and no open run for work that was in flight; a
  `dispatch` is durable before the side effect it precedes, so a resume
  could fail to answer a call that may already have run. `PendingCalls`,
  `PendingQueued`, `Runs` and `OpenRun` all read correctly now.
- The change is in the reader, so a file already written resolves
  correctly without being rewritten, and `sqlite` inherits it by
  rebuilding the JSONL form and calling the same `Read`. The store
  conformance suite covers it, so every store is checked.
- One behaviour a caller may have relied on is gone: a mark can no
  longer pin the leaf at an entry its own branch has grown past.
  Nothing in the library could produce such a mark — `MarkLeaf` builds
  only from the current leaf — and the reserved label has always
  documented itself as naming the entry the next append should hang
  from. A durable pin would want its own label rather than an overload
  of this one.

## v0.0.7 - 2026-09-23

- `sqlite` and `otel` now require the root at exactly the version they
  are released at, rather than at the previous release, and carry a
  `replace` of the root pointing at the tree. Taking
  `agentsession/sqlite` alone now resolves the root commit it was built
  and tested against, instead of the one before it. Consumers ignore a
  `replace` in a dependency, so only the `require` reaches them; the
  published `go.sum` files no longer carry first-party entries. This is
  the shape OpenTelemetry-Go publishes.
- Requires `openresponses` v0.0.12, up from v0.0.10.

## v0.0.6 - 2026-09-21

- RFC 0001 is revised to draft 0.3 and the library writes
  `agentsession/0.3`; 0.2 and 0.1 files read unchanged. Context
  building now states how the request context of one response is
  found, since two implementations rebuild it: walking back from the
  response entry, an entry that is not an item is skipped, an item
  whose `response` names the response is output, and the walk stops at
  the first item naming another response or none. A reader identifies
  a response's output items by `response` and never by position; a
  writer SHOULD still keep them contiguous, so the envelope reads in
  the order it happened, and the RFC now says why rather than merely
  permitting both spellings. An entry another layer raises while a
  model call is in flight — a guard's verdict, a dispatch, a run
  boundary — is not an entry "for that model call", which the writing
  discipline previously left ambiguous (#40).
- `Session.RequestContext` follows that rule, so a custom or record
  entry written between two output items of one response no longer
  truncates the rebuilt request. A composed product that records a
  guard's verdict where the guard runs passes `verify` again; only the
  response's own item entries are removed from the path, and every
  other entry stays where it was (#40).

- New export `OutputEntries(path []Entry, resp *ResponseEntry)
  []*ItemEntry`, the rule for finding a response's own output items,
  over a path the caller already holds. `Session.RequestContext` calls
  it rather than keeping its own copy of the walk. The rule is
  implemented twice in the workspace — here, which excludes those
  entries from the rebuilt request, and in `agenteval`'s replay, which
  serves their items — with a comment rather than a compiler keeping
  the two in step. Exporting the selection is what lets the second
  reader drop its copy: it returns entries rather than items, because
  a reader that excludes them needs entry identity and items carry
  none, and it returns them in path order, not the backward order the
  walk runs in. The entries are the session's own, so a caller that
  serves their items to something that records must clone them. The
  RFC now states the two things a second implementation had to infer:
  a `response` with no `response_id` has no output items, and the
  order is normative.

- `CompactionEntry` gains `Pinned`, written as the optional `pinned`
  member: the items a fold kept verbatim from before `first_kept`.
  `BuildContext` places them immediately after `summary` and before
  the kept window, which is where the request that was sent had them,
  so a harness that holds an item out of a fold can record a
  `request_hash` it stands behind instead of recording none. Because
  `Context` carries them too, such a pin survives `Continue`,
  `Resume` and `Rebase` rather than being dropped at the fold. The
  ATIF projection carries them beside the fold's summary, since the
  entries they were copied from are before `first_kept` and so are not
  in the document. Additive: no file in existence carries the member,
  each pinned item is also an `item` entry on the path, and a reader
  that ignores it rebuilds a request short of those items rather than
  one that invents them. The RFC states the three rules a writer will
  otherwise get wrong: only an item carried by an `item` entry may be
  pinned, so a summary cannot be; the pins' order among themselves is
  the order the request carried them in; and only the last compaction
  on a path contributes items, so a later fold must restate a pin that
  is to survive it.

- **Fixed.** A `run` entry no longer drops the other phase's members
  when it is written back. The encoder writes one member list for a
  start and another for an end, and a file from elsewhere carrying
  `source` on an end, or `reason` or `pending` on a start, lost it
  silently: the member is declared on `RunEntry`, so the envelope rule
  did not preserve it in `Unknown` either. Those members are now
  written when set. Nothing this library builds sets them, so a
  well-formed entry is written exactly as before, byte for byte.

- **Breaking.** `Session.Verify` returns the new `ErrNoHash` for a
  response that recorded no request hash, where it returned nil. The
  two cases it could not tell apart — the rebuilt request hashed to
  the recorded value, and there was no recorded value to check — are
  now distinct, so a caller gating a build on `err == nil` is told
  when nothing was verified rather than passing. A caller that accepts
  unverified requests opts in with `errors.Is(err,
  agentsession.ErrNoHash)`. The recorder legitimately writes an empty
  hash whenever a layer edits the request outside the transcript — a
  transform that injects, a `BeforeModelCall` hook, a compaction whose
  folds were never reported — so a session in that state could pass a
  gate with nothing checked and nothing said. The CLI's `verify`
  output and exit code are unchanged: it already read `RequestHash`
  itself to draw the distinction the library would not provide.

- **Breaking.** `ComputeReason` takes the run's path as well as its
  segment, and reads the last response's own output items on the path
  to decide whether the model asked for a tool, so a run that starts
  between a function call and its response is not read as `done` while
  the call goes unanswered; `Run` carries the `Path` the segment ends, and the
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

- One `agentsession.ErrSessionLocked` in the root module for a session
  another process holds. `jsonl.ErrSessionLocked` and
  `sqlite.ErrSessionLocked` are that error, so a host written against
  the `Store` interface tells "another process has this session" from
  "the store is broken" without being told by its caller what its own
  store's errors mean. Matching either name still works; the message
  no longer carries the store's prefix (#41).
- `jsonl.WithReadOnly` and `sqlite.WithReadOnly` open a store that
  takes no lock or hold on the sessions it opens and refuses `Create`,
  `Append`, `Delete` and `Sync` with `agentsession.ErrReadOnly`, so
  `verify`, `show` and `export` work while the agent that owns the
  session is running. A read-only jsonl open reports a cut-short final
  line and leaves it in the file, since trimming it is a write. The
  CLI's `list` opens its store read-only; its other commands already
  read the file directly. `BreakLock` is refused too: a store that
  takes no lock has no business dropping another process's, and a
  read-only jsonl store does not create its root directory either. Two
  things it is not: a read-only sqlite store still creates or migrates
  the database's tables when it opens the file, which is what makes a
  database an earlier release wrote readable, and a session either
  store has opened is cached, so `Release` and a second `Open` are how
  a reader sees what has been appended since (#44).
- The sqlite store parses a holder's `since` and `heartbeat` before it
  decides what to do with the row, so the refusal an operator sees
  names when the holder took the session and when it last appended
  rather than year one, which read like a broken lock and invited
  breaking a live one (#43).

- The config entry records the instructions as parts, which is the
  round's one format change. `instructions_parts` is an ordered list
  of `{id, text, source}`, one per layer that writes the prompt: a
  delta carries the whole ordered list with the text of the parts that
  changed and `{id, hash}` for the parts that did not, and a part the
  list leaves out is removed. `instructions` remains valid and, where
  both are present, is the parts' texts joined with one blank line, a
  rule the RFC states so a writer and a reader agree; `Settings`
  derives it, so nothing downstream of the settings changes and the
  request hash is untouched. `Settings.InstructionsDelta` builds the
  delta from the parts in force and returns nil when nothing moved;
  `ConfigFromRequestParts` builds the full config on a root.
  `instructions_omitted` beside it records the parts a writer
  considered and left out, with a reason and a size, which is where
  `agentsmd.Result.Omitted` and a memory manifest go, and
  `Context.InstructionsOmitted` reads the last of them on a path. A 13
  byte edit to one of four parts now costs that part rather than the
  whole prompt: the composed study's 2,787 byte delta, and the memory
  study's 33,440 byte one, become the part that changed plus an id and
  a hash for each part that did not. A part named by a hash keeps the
  source it had, so a writer that clears or changes a part's source
  writes its text with it; a `Settings` never shares its parts with
  another or with the entry they came from; and where a part cannot be
  resolved, an `instructions` string written beside it stands, which a
  replacing delta must carry (#27).

- New record entry `queued`: the input a harness accepted before it
  could append it, a steer that joins the run in flight or a follow-up
  that waits for it, with the `trigger` that brought it in. The
  context algorithm ignores it, and the item entry that drains it
  carries `source`, the same trigger, and `queued_from` naming the
  queued entry, so two people steering one run are told apart and the
  record says why an item is there. `QueuedEntry`, `NewQueued`,
  `WithTrigger` and `Drain` write it; `Session.PendingQueued` and
  `Queued` list the inputs a harness still owes the conversation,
  which is the durable inbox a gateway that answers 202 drains on
  resume, and a run end closes one. The header's `records` may promise
  `queued`. `ItemEntry.Source` also stands alone, for any item a
  person or another system sent (#42).

- `export.Options.Cost` is asked once per model call rather than once
  for the step and again for the totals, so a price source that counts
  or charges for what it is asked is not double counted.
- The ATIF export tells what a run cost from what a document shows.
  `final_metrics` now totals every model call on the path, including
  the ones a compaction folded out of the document, `total_steps`
  counts the steps the document holds plus the calls it does not
  show, and a line in `notes` says so, which is what ATIF asks of a
  `total_steps` that is not the number of steps. A run that folded
  seven times reported the cost of the three calls that survived
  (#36).
- **Breaking.** `extra.run` in an exported document is a list, on a
  step and at the root. A run that produces no step, which is what a
  refusal on resume is, had its record replaced by the next run's and
  vanished from the document; a list keeps every record and gives a
  reader an order where several land in one place (#37).
- `export.Options.ModelName` overrides the model name the document
  reports, in `agent.model_name` and on every agent step, without
  touching the model the request was sent with, for a consumer that
  derives a provider by splitting the name on a slash. Costs are still
  priced by the model that was sent (#38).
- `export.At(s, entryID)` builds the trajectory of the path that ends
  at any entry, not only at a leaf, with the same `PreferredOver`,
  `AbandonedAt` and `Main` treatment; the entry it ends at is not read
  as a fork, since what was appended below it is not a branch this
  path abandoned; `Trajectories` is it over each
  leaf. A judge that appends an outcome moves the leaf, and the
  document a score names can now be built again from the entry the
  score targets. `Trajectory` gains `Path`, the root-first path before
  compaction, which the totals are taken over (#39).

- `agentsession show` renders a custom entry as its namespace and the
  size of its data, and an extension entry as its type and size, in
  place of the "(unknown entry type)" that four policy verdicts used
  to print as four identical lines; `-v` prints the data itself. Every
  core entry type now has a case, which a test holds it to (#35).

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
