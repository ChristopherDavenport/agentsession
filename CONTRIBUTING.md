# Contributing

Issues and pull requests are welcome.

## Before you start

The library implements the Agent Session Format specified in
`docs/rfcs/0001-agent-session-format.md`, with Open Responses items as
the payload. The RFC is normative: when the library design in
`docs/plans/session-layer.md` and the RFC disagree, the RFC wins, and a
change to the wire shape starts as a change to the RFC.

Anything a harness records beyond the core entry types goes through a
namespaced entry type or a namespaced item, never a new core field. A
reader must preserve what it does not understand; that is tested, not
assumed.

For anything larger than a bug fix, open an issue first so the shape of
the change can be discussed before you spend time on it.

## Development

Go 1.25 or later is required. The full local check is:

```sh
make check        # gofmt, tidy, vet, deps, staticcheck, govulncheck, race tests
```

The individual targets are `fmt`, `tidy-check`, `vet`, `deps`, `lint`,
`vuln`, `test` and `tidy`.

The repository has three modules, joined by `go.work`: the library at
the root, and `sqlite` and `otel`, nested so their drivers stay out of
the library's dependency graph. The Makefile targets cover all of them;
a bare `go test ./...` at the root does not, even in workspace mode.
`deps` fails if the root module imports anything beyond `openresponses`
and the standard library; anything that needs another dependency is a
nested module listed in `SUBMODULES` in the Makefile. `lint` and `vuln`
run staticcheck and govulncheck through `go run`, which may download a
newer Go toolchain the first time. Workspace mode rejects `-mod=mod`, so
a `GOFLAGS=-mod=mod` in your environment has to go.

A nested module's `go.mod` requires a released root version and carries
no `replace`: the workspace is what builds it against the tree. That is
deliberate. A `replace` is a property of the main module and consumers
ignore it, so a nested module that carried one would build green here
while shipping a `go.mod` that names a root version without the API it
uses. `make release-check` builds each nested module with `GOWORK=off`,
against the versions its own `go.mod` requires, which is what a consumer
gets.

`release-check` is not part of `check`, and it fails by design between a
root API addition and the next root tag — a nested module that uses the
new API cannot name a version that carries it until that version exists.
That failure is the release ordering, not a bug; see below.

Golden files live under `testdata/`. `go test ./... -update` rewrites
the generated ones (`testdata/context`, `testdata/export`,
`testdata/hash/vectors.json`); review the diff before committing. The
session fixtures under `testdata/sessions` are written by hand in the
exact form the library emits, because the round-trip test compares
bytes.

## Pull requests

- Keep the change focused; unrelated cleanups belong in their own PR.
- Add or update tests. A wire-shape change needs a fixture under
  `testdata/sessions` and, usually, a context golden.
- Run `make check` before pushing. CI runs the same steps on the minimum
  and current Go versions.
- Note user-visible changes under *Unreleased* in `CHANGELOG.md`.

## Releases

Every module in the repository shares one version, but not one commit.
A nested module's requirement cannot name a tag that does not exist
yet, so the root is released first and the nested modules follow. With
the changelog's *Unreleased* section written:

```sh
make release-root VERSION=v0.1.0
```

dates the changelog, runs `make check`, commits, tags `v0.1.0` with the
changelog section as the message, and pushes the branch and the tag.
The nested modules still require the previous root release across this
commit, which is correct: `v0.1.0` did not exist when it was written.

Once that tag is on the module proxy:

```sh
make release-submodules VERSION=v0.1.0
```

walks `SUBMODULES` in order and, for each, sets its requirement on the
root and on any already-released sibling to the version, tidies, builds
and tests it with `GOWORK=off` against exactly those versions, commits,
tags `<dir>/v0.1.0` and pushes. One commit and one tag per module,
because `go mod tidy` and the `GOWORK=off` build both resolve a sibling
requirement from the proxy: a module has to be published before the
module that requires it is bumped. A module is built the way a consumer
builds it before its tag is written, and the release workflow runs
`make release-check` again on the tag before publishing.

The release workflow publishes a GitHub release per tag, and the Go
module proxy picks the versions up. Before v1.0.0 the API may change
between minor versions; the changelog records every break.
