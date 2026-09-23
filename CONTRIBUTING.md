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
make check        # gofmt, tidy, vet, deps, replaces, staticcheck, govulncheck, race tests
```

The individual targets are `fmt`, `tidy-check`, `vet`, `deps`, `replaces`,
`lint`, `vuln`, `test` and `tidy`.

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

Each nested module's `go.mod` requires the root — and any sibling it
uses — at **exactly the version the whole repository is released at**,
and carries a matching `replace` pointing at the tree. The two go
together. Requiring the version being released means the release commit
names a version the proxy cannot serve until its tag is pushed, and
`go mod tidy` ignores `go.work`; the `replace` is what lets tidy, build
and test resolve it locally. Consumers ignore a `replace` in a
dependency and get the `require`, which names the commit the module was
tagged from.

`make replaces`, part of `check`, refuses a first-party require that
lacks a replace — losing one would make the next release fail at `make
tidy`, or silently pin that module to the previous release. This is the
same shape OpenTelemetry-Go publishes.

The upshot for you: a consumer who takes only `agentsession/sqlite` at
vX.Y.Z gets root vX.Y.Z, the exact commit it was built and tested
against. The workspace build and the consumer build are the same code,
so there is no `release-check` and no phased release — both existed to
manage a nested module requiring a *previous* root, which no longer
happens.

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

`CLAUDE.md` holds the full procedure and the reasoning, including what to
do when a tag goes out wrong. The essentials:

Every published module is released at one version, from one commit, and
requires its first-party siblings at exactly that version. With the
changelog's *Unreleased* section written:

```sh
make release VERSION=v0.1.0
```

points every nested module's first-party requires at `v0.1.0`, dates the
changelog, runs `make tidy` and `make check`, reads the requires back to
confirm tidy did not move them, commits, then guards and tags the root,
guards and tags `sqlite/v0.1.0` and `otel/v0.1.0`, and pushes the branch
and all three tags with `git push origin --atomic`.

`make release-guard TAG=<tag>` is what stands between a mistake and a
permanent one. It refuses a dirty tree, a tag that already exists, a
version that sorts below the current root release or does not move its
module forward, a first-party require that does not name that version, a
root tag that is not this commit, and a module that will not build with
`GOWORK=off`. `make release` runs it for every tag it writes, and the
root is guarded and tagged first because a nested module's guard needs
the root tag to exist.

Nothing is public until the push. If a guard refuses, `git reset --hard
HEAD~1` and `git tag -d` whatever was written.

The release workflow publishes a GitHub release per tag, and the Go
module proxy picks the versions up. Before v1.0.0 the API may change
between minor versions; the changelog records every break. A pushed
version is permanent — the proxy and the checksum database keep it
forever — so a bad one is superseded and `retract`ed, never deleted.
