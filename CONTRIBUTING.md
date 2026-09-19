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
make check        # gofmt, vet, deps, staticcheck, govulncheck, race tests
```

The individual targets are `fmt`, `vet`, `deps`, `lint`, `vuln`, `test`
and `tidy`. The repository has two modules: the library at the root and
`sqlite`, nested so its driver stays out of the library's dependency
graph. The Makefile targets cover both; a bare `go test ./...` at the
root does not. `deps` fails if the root module imports anything beyond
`openresponses` and the standard library; anything that needs another
dependency is a nested module listed in `SUBMODULES` in the Makefile.
`lint` and `vuln` run staticcheck and govulncheck through `go run`,
which may download a newer Go toolchain the first time.

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

Releases are annotated tags. The tag message becomes the GitHub release
notes, so write it as one:

```sh
git tag -a v0.1.0 -m "v0.1.0: one line per user-visible change"
git push origin v0.1.0
```

The release workflow publishes the GitHub release, and the Go module
proxy picks the version up from the tag. Before v1.0.0 the API may
change between minor versions; the changelog records every break.
