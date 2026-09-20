# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

- RFC 0001 is revised to draft 0.2. The summary now defines a session as
  two kinds of entry, context and record, and the core types section
  states the test for admitting a core type. Three record entries are
  added: `run` (source and end reason), `dispatch` (a call handed to its
  tool) and `decision` (a call's fate, any rewritten arguments and,
  optionally, who decided it). Run end reasons and decision verdicts are
  defined as shapes of the path a reader can recompute, so they belong
  to the format rather than to one harness, and the reasons form a
  first-match cascade; how an input arrived and who decided a call are a
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
