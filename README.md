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
- **Resumable.** Run, dispatch and decision entries record what
  happened around the conversation. For every call without an output,
  the path says whether it was never started, was in flight when the
  record stopped, or is waiting on an answer, in any file whose header
  promises those entries are written.
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
place, and appending `sess.MarkLeaf()` makes that choice durable, so a
reopened session resumes from it rather than from the last line;
`sess.ResetLeaf()` starts a new root. `sess.Compact(firstKept,
summary)` and `sess.SummarizeBranch(from, summary)` build the entries
that fold context down or carry it across a branch switch; a caller
that split the request input at an index, or kept its last n items,
uses `sess.CompactFrom(i, summary)` or `sess.CompactKeeping(n, summary)`
instead of mapping the index to an entry ID itself. `Context.ItemEntries`
is the mapping, aligned with `Context.Items`.
`sess.Verify(responseEntryID)` rebuilds the request and checks the
hash.

A harness that composes its instructions from several layers records
them as parts, so a change to one layer costs that layer and not the
whole prompt:

```go
parts := []agentsession.InstructionPart{
    {ID: "product", Source: "product", Text: productPrompt},
    {ID: "agentsmd", Source: "agentsmd", Text: agentsmd.Render(res.Files)},
    {ID: "agentmemory", Source: "agentmemory", Text: block},
}
cfg, err := agentsession.ConfigFromRequestParts(req, parts...) // the first entry on a root
// ... later, when a layer re-renders ...
if delta := c.Settings.InstructionsDelta(parts); delta != nil {
    delta.InstructionsOmitted = omitted   // what was considered and left out
    store.Append(ctx, id, delta)
}
```

`InstructionsDelta` writes the whole ordered list of IDs with the text
of the parts that moved and a hash for the parts that did not, and
returns nil when nothing moved. `Settings.Instructions` is always the
parts joined with a blank line, so a reader that does not care about
the composition sees the string it always saw. `agentsession.Continue(ctx, store, id, summary)` rolls a session
that has outgrown its file into a successor that starts from the old
leaf's settings, and marks the old one superseded so a `Current`
listing shows only the successor.

A harness that runs the loop records the lifecycle around it. The
header's `Records` lists the record entry types the writer promises
to write whenever their event occurs, so a reader may take their
absence as the event not having happened; a converter over a native
log that has no such record leaves it empty.

```go
sess, err := store.Create(ctx, agentsession.Header{Records: agentsession.AllRecords})
store.Append(ctx, id, agentsession.NewRunStart("run-1", agentsession.SourceInput, "cron:nightly"))
// ... the request, the response with its function calls ...
store.Append(ctx, id, agentsession.NewDecision("call_1", callEntryID, agentsession.VerdictHold, agentsession.ByPolicy).
    WithReason("destructive; needs approval"))
store.Append(ctx, id, agentsession.NewDispatch("call_2", otherCallEntryID)) // synced before the tool runs
end, err := sess.EndRun(agentsession.ReasonInputRequired, "")           // pending computed from the path
store.Append(ctx, id, end)
```

An input a harness accepts while a run is in flight, a steer or a
follow-up, is recorded before it is acted on:

```go
q := agentsession.NewQueued(item, agentsession.ModeSteer).
    WithTrigger("human", "slack:1758412800.0002", "gateway")
store.Append(ctx, id, q)                 // the gateway can answer 202
// ... when the loop can take it ...
store.Append(ctx, id, q.Drain())         // the item, its source and queued_from
```

`sess.PendingQueued(leaf)` lists the inputs that have neither been
appended nor been closed by the end of the run they were queued into,
which is the inbox a restarted harness drains.

On resume, `sess.PendingCalls(leaf)` lists the calls without an
output and `Call.State(header)` says what the path knows about each.
A run's end reason is a shape of its segment and of the path the
segment ends: `ComputeReason` recomputes it and `Run.Verify` checks a
written one against it. A run that answers a held call and ends
without calling the model again, which is what a refusal and a
terminating resume are, reads as `stopped`.

Both file stores guard a session against a second writing process and
report `agentsession.ErrSessionLocked`, the one sentinel for that
condition, so a host written against the `Store` interface tells a
session another process holds from a store that is broken without
knowing which store it was given. `jsonl.WithReadOnly` and
`sqlite.WithReadOnly` open a store that takes no lock and refuses
every write with `agentsession.ErrReadOnly`, which is how a session
is read while an agent is writing it.

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

`export.At(s, entryID)` builds the document of the path that ends at
one entry, which is what a consumer holding a score needs once the
outcomes a judge appended have moved the leaf; `Trajectories` is that
over each leaf. A document's steps are the context after compaction
and its `final_metrics` are the whole path, so a run that folded
reports what it spent, with `total_steps` and a line in `notes`
saying what the steps leave out. `export.Options.ModelName` overrides
the model name the document reports, for a consumer that derives a
provider from it, without changing the name the request was sent
with.

Each document is one root-to-leaf path with compaction applied. The
session's current path is written as `<session-id>.json`, the others
as `<session-id>_<leaf>.json`. A branch that was continued lists the
leaves it was preferred over in `extra.preferred_over`; an abandoned
one names the fork in `extra.abandoned_at`. By default the continued
branch is the one appended to last; pass `export.PreferCurrentLeaf`,
`export.PreferLabel("kept")` or `export.PreferScore` to `Trajectories`
to decide otherwise. `export.Items(doc)` reads the raw items back out, and
`export.ItemsFrom(doc)` rebuilds them, lossily, from the declared
fields of a document any producer wrote. `export.NoPassthrough()`
strips the raw items for a document a judge will read.

## Inspecting from a shell

`cmd/agentsession` reads session files without taking their lock, and
`list` opens the store with `jsonl.WithReadOnly`, so every command is
safe to run beside a harness that is writing.

```
go install github.com/ChristopherDavenport/agentsession/cmd/agentsession@latest

agentsession show session.jsonl            # entries in file order, then the context at the leaf
agentsession show session.jsonl -leaf ID   # the context at another entry
agentsession verify session.jsonl          # rebuild every request and check its hash
agentsession export session.jsonl -out dir -secret "$OPENAI_API_KEY" -redact-home
agentsession list ~/.agent/sessions        # a jsonl store's sessions, newest first
agentsession list ~/.agent/sessions -current   # leave out sessions continued in a successor
```

`verify` exits 1 on a mismatch, a truncated final line, a run end that
disagrees with its segment, a dispatch after a reject, or a call that
ran without the dispatch the header promised. `export`
writes one ATIF document per leaf and embeds a linked subsession when
its file is beside the exported one or in the same store.

## Tracing

The `otel` module is the RFC's OpenTelemetry projection. `otel.Export`
replays a session's path into any tracer with the entries' own
timestamps: a session span, a span per run carrying its source and end
reason, a `chat <model>` span per model call that links the previous
turn's, and an `execute_tool <name>` span per call that links the
inference that produced it and carries each decision as an event and
the call's state when the record stopped. `otel.Wrap` decorates a
store so a live harness emits the same spans as it appends, and a
session reopened in a new process picks up the calls the last one
left pending.

```go
store := otel.Wrap(jsonlStore, otel.Tracer("my-agent"))
// ... record as usual; call store.Close(sessionID) when done ...

_, err := otel.Export(ctx, tracer, sess, sess.Leaf()) // a stored session, after the fact
```

## Packages

| package | purpose |
|---|---|
| `agentsession` | header, entries, tree, context algorithm, request hash, `Store` interface, in-memory store |
| `jsonl` | the file store: one JSONL file per session with a sync policy, crash recovery and a per-session lock against a second writing process, reporting a dead holder's lock when it takes one over, or read-only and taking no lock |
| `sqlite` | a SQLite store, as a nested module so its driver stays out of the library, holding each open session against a second process, or read-only and taking no hold |
| `otel` | the OpenTelemetry projection, as a nested module: replay a session as spans, or wrap a store so a live run emits them |
| `atif` | Go types for ATIF v1.8 with unknown-member passthrough and validation |
| `export` | trajectories, ATIF conversion, redactors, writer |
| `storetest` | the conformance suite every store runs |
| `cmd/agentsession` | the command: show, verify, export and list session files |

## Interoperating

Three things a writer built elsewhere must agree on are written in
the RFC rather than shared as code: the request hash, the `link` entry
for subsessions, and the subsession ID, a UUIDv5 under the nil
namespace over `<parent session id>/<call_id>` that `SubsessionID`
derives. `testdata/hash/vectors.json` holds request documents
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
