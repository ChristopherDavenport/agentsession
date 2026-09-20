# agentsession

[![Go Reference](https://pkg.go.dev/badge/github.com/ChristopherDavenport/agentsession.svg)](https://pkg.go.dev/github.com/ChristopherDavenport/agentsession)
[![CI](https://github.com/ChristopherDavenport/agentsession/actions/workflows/ci.yml/badge.svg)](https://github.com/ChristopherDavenport/agentsession/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

The reference Go implementation of the
[Agent Session Format](docs/rfcs/0001-agent-session-format.md): an
append-only, tree-structured JSONL record of one agent session whose
conversation payloads are [Open Responses](https://www.openresponses.org)
items. One file holds what the model was sent, what it returned, what
tools did, how the configuration changed, where the conversation
branched, and what happened afterwards.

- **Lossless.** Items are stored as the wire carried them, reasoning
  and extension types included. Anything a reader does not understand
  is written back byte for byte.
- **Replayable.** Every model call can be rebuilt from the path to its
  entry: settings, items, compaction. A recorded `request_hash` (RFC
  8785 canonical JSON, SHA-256) lets a reader check its work.
- **Append-only and crash-tolerant.** A session file is always a valid
  prefix of the run; a line cut short by a crash is reported and
  skipped.
- **Tree-shaped.** Branching creates children of an earlier entry in
  the same file. Abandoned branches stay, because they are preference
  data.
- **Exportable.** One ATIF v1.8 document per root-to-leaf path, with
  the raw items in the extras, redaction at export and media spilled
  beside the documents.

The root module depends on
[`openresponses`](https://github.com/ChristopherDavenport/openresponses)
and the standard library alone. Requires Go 1.25. The library is
pre-1.0: minor versions may change the API, and
[CHANGELOG.md](CHANGELOG.md) records every break.

## Install

```sh
go get github.com/ChristopherDavenport/agentsession
```

## Recording a session

```go
store, err := jsonl.Open(filepath.Join(home, ".agent", "sessions"))
sess, err := store.Create(ctx, agentsession.Header{
    CWD:     cwd,
    Harness: &agentsession.Harness{Name: "my-agent", Version: "1.0"},
})
id := sess.ID()

// The first entry on a root is a config with the full settings.
cfg, err := agentsession.ConfigFromRequest(openresponses.Request{
    Model: "gpt-5", Instructions: "Be brief.", Tools: tools,
})
store.Append(ctx, id, cfg)
store.Append(ctx, id, agentsession.NewItemEntry(openresponses.UserText("What is 2+2?")))

// Build the request from the tree, send it, record what came back.
c, err := sess.Context()
req, err := c.Request()                 // store: false, no previous_response_id
start := time.Now()
resp, err := client.Create(ctx, req)
_, err = agentsession.RecordResponse(ctx, store, id, req, resp, time.Since(start))
```

`RecordResponse` appends the output items and the response entry with
the hash of the request it was given. Every append becomes the leaf.
`sess.Branch(entryID)` moves the leaf so the next append forks in
place; `sess.ResetLeaf()` starts a new root. `sess.Compact(firstKept,
summary)` and `sess.SummarizeBranch(from, summary)` build the entries
that fold context down or carry it across a branch switch; a caller
that split the request input at an index, or kept its last n items,
uses `sess.CompactFrom(i, summary)` or `sess.CompactKeeping(n, summary)`
instead of mapping the index to an entry ID itself. `Context.ItemEntries`
is the mapping, aligned with `Context.Items`.
`sess.Verify(responseEntryID)` rebuilds the request and checks the
hash.

## Reading one

```go
f, _ := os.Open("session.jsonl")
sess, err := agentsession.Read(f)
if t := sess.Truncated(); t != nil {
    log.Printf("last line was cut short: %v", t) // the prefix loaded
}
for _, e := range sess.Entries() {
    switch v := e.(type) {
    case *agentsession.ItemEntry:
        fmt.Println(v.Item.ItemType())
    case *agentsession.UnknownEntry:
        fmt.Println("extension", v.Type) // re-emitted verbatim by Write
    }
}
```

## Exporting to ATIF

```go
docs := func(yield func(*atif.Trajectory) bool) {
    for tr, err := range export.Trajectories(sess) {
        if err != nil {
            continue
        }
        doc, err := export.ToATIF(tr, export.Options{
            Redactors: []export.Redactor{
                export.Secrets(os.Getenv("OPENAI_API_KEY")),
                export.HomePaths(home),
            },
        })
        if err != nil || !yield(doc) {
            return
        }
    }
}
err := export.WriteATIF("out/", docs)
```

Each document is one root-to-leaf path with compaction applied. The
session's current path is written as `<session-id>.json`, the others
as `<session-id>_<leaf>.json`. A branch that was continued lists the
leaves it was preferred over in `extra.preferred_over`; an abandoned
one names the fork in `extra.abandoned_at`. By default the continued
branch is the one appended to last; pass `export.PreferCurrentLeaf`,
`export.PreferLabel("kept")` or `export.PreferScore` to `Trajectories`
to decide otherwise. `export.Items(doc)` reads the raw items back out.

## Inspecting from a shell

`cmd/agentsession` reads session files without taking their lock, so
it is safe to run beside a harness that is writing.

```
go install github.com/ChristopherDavenport/agentsession/cmd/agentsession@latest

agentsession show session.jsonl            # entries in file order, then the context at the leaf
agentsession show session.jsonl -leaf ID   # the context at another entry
agentsession verify session.jsonl          # rebuild every request and check its hash
agentsession export session.jsonl -out dir -secret "$OPENAI_API_KEY" -redact-home
agentsession list ~/.agent/sessions        # a jsonl store's sessions, newest first
```

`verify` exits 1 on a mismatch or a truncated final line. `export`
writes one ATIF document per leaf and embeds a linked subsession when
its file is beside the exported one or in the same store.

## Packages

| package | purpose |
|---|---|
| `agentsession` | header, entries, tree, context algorithm, request hash, `Store` interface, in-memory store |
| `jsonl` | the file store: one JSONL file per session with a sync policy, crash recovery and a per-session lock against a second writing process |
| `sqlite` | a SQLite store, as a nested module so its driver stays out of the library |
| `atif` | Go types for ATIF v1.8 with unknown-member passthrough and validation |
| `export` | trajectories, ATIF conversion, redactors, writer |
| `storetest` | the conformance suite every store runs |
| `cmd/agentsession` | the command: show, verify, export and list session files |

## Interoperating

Two things a writer built elsewhere must agree on are written in the
RFC rather than shared as code: the request hash and the `link` entry
for subsessions. `testdata/hash/vectors.json` holds request documents
and their hashes so another implementation can test itself against the
same inputs. The Python one-liner
`json.dumps(obj, sort_keys=True, separators=(",", ":"), ensure_ascii=False)`
reproduces the canonical form for requests whose numbers are integers.

## Development

```sh
make check   # gofmt, vet, deps, staticcheck, govulncheck, race tests
```

See [CONTRIBUTING.md](CONTRIBUTING.md). The format is specified in
[docs/rfcs/0001-agent-session-format.md](docs/rfcs/0001-agent-session-format.md)
and the library design in
[docs/plans/session-layer.md](docs/plans/session-layer.md).
