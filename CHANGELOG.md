# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

- The `sqlite` module is tagged as `sqlite/v0.0.1`. Its `go.mod`
  requires `agentsession v0.0.1` instead of a local `replace`, so
  `go get` works, and a committed `go.work` keeps local development
  building against the checked-out root.
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
