# CLAUDE.md

Guidance for working in this repository. The release section is the part
where a mistake is permanent; read it before touching a tag.

## Layout

Three Go modules in one repository, joined by `go.work`:

| Path     | Module path              | Published |
| -------- | ------------------------ | --------- |
| `.`      | `…/agentsession`         | yes       |
| `sqlite` | `…/agentsession/sqlite`  | yes       |
| `otel`   | `…/agentsession/otel`    | yes       |

The root module depends on `openresponses` and the standard library
alone; `make deps` enforces it. Anything needing another dependency —
a database driver, an OpenTelemetry SDK — is a nested module listed in
`SUBMODULES` in the Makefile.

The library implements the Agent Session Format specified in
`docs/rfcs/0001-agent-session-format.md`. The RFC is normative: when the
library and the RFC disagree, the RFC wins, and a change to the wire
shape starts as a change to the RFC.

## Everyday commands

```sh
make check      # fmt, tidiness, vet, deps, replaces, staticcheck, govulncheck, race tests
make release VERSION=vX.Y.Z   # the whole release; see below before running it
```

A bare `go test ./...` does not cross module boundaries even in
workspace mode. Use the Makefile targets.

The workflows enumerate targets one per step rather than running `make
check`, so a target added to `check` needs a step in
`.github/workflows/ci.yml` or it never runs in CI.

## Releases

### What is permanent

Pushing a tag publishes that version. Within minutes `proxy.golang.org`
has the zip and `sum.golang.org` has its hash, **both forever**. There
is no unpublish. Deleting the git tag does not remove the version; it
only makes it unverifiable, which is strictly worse. A bad version can
only be superseded and `retract`ed.

So the expensive mistakes all happen at `git push origin <tag>`. Every
guard below exists to run before that line.

### The versioning rule

All published modules are released **at one version, from one commit**,
and each requires its first-party siblings at **exactly that version**.

Two things follow, and both matter:

- A consumer who takes only `agentsession/sqlite` at vX.Y.Z gets root
  vX.Y.Z — the exact commit that module was built and tested against.
  The shared version line is a fact about the dependency graph, not a
  naming convention.
- A nested module can never be numbered below the root, so it can never
  drag a consumer's root module backwards through minimal version
  selection. `make release-guard` checks the ordering anyway.

There is a third consequence, and it is why this repository has fewer
release gates than it used to: the workspace build and the consumer
build are now the same code. `make check` builds each nested module
against the root in the tree; a consumer builds it against root vX.Y.Z,
which is that same tree at the tagged commit. Nothing can drift between
them, so nothing needs a gate to catch the drift.

### Why the `replace` directives are load-bearing

Each nested `go.mod` carries `replace …/agentsession => ../`, and `make
replaces`, part of `check`, refuses a first-party require that lacks one.
This is the inverse of the rule this repository used to carry, and it is
not cosmetic.

Requiring the version being released means the release commit names a
version that does not exist on the proxy until its tag is pushed.
`go mod tidy` ignores `go.work`, so without the replace, tidy would try
to fetch it and the release commit could not be built, tidied or
committed. The replace is what lets a release name its own version.

A replace belongs to the main module, so consumers ignore it and get the
require. That is safe here *only* because the require names the commit
the module is tagged from — which `release-guard` proves, by checking
that the root tag of that version points at the same commit. Remove that
check and the replace becomes the hazard it is in a repository that
releases modules at independent versions.

This is what OpenTelemetry-Go does, and for the same reason. Its
published `otel/sdk@v1.46.0` requires `otel v1.46.0` and ships
`replace go.opentelemetry.io/otel => ../`; its published `go.sum` has no
first-party entries at all, because tidy never resolves one from the
proxy. Ours look the same.

One real cost: `go install pkg@version` rejects a module carrying
`replace` directives. The nested modules ship no commands — the CLI is
in the root module, which has no replaces — so it does not bite today.
It is the thing to check before a nested module grows a binary.

### Before any tag

```sh
make release-guard TAG=sqlite/v0.1.0
```

It refuses to proceed unless the tree is clean, the tag is new locally
and on origin, the version sorts at or above the current root release and
moves that module forward, every first-party require names exactly that
version, the root tag of that version is this very commit, and the module
builds with `GOWORK=off`.

A nested tag is only checkable once the root tag exists, so guard and tag
in the order `make release` uses: root first, then each nested module.

### The release

Everything from one commit, one transaction. With the changelog's
*Unreleased* section written:

```sh
make release VERSION=v0.1.0
```

That points every nested module's first-party requires at v0.1.0, dates
the changelog, tidies, runs `make check`, reads the requires back to
confirm tidy did not move them, commits, then guards and tags the root,
guards and tags each nested module, and runs:

```sh
git push origin --atomic HEAD v0.1.0 sqlite/v0.1.0 otel/v0.1.0
```

`--atomic` lands every ref in a single transaction, so no window exists
in which one tag is visible without the others — or in which a published
`go.mod` names a version the proxy cannot serve.

Nothing is public until that push. If a guard refuses, `git reset --hard
HEAD~1` and `git tag -d` whatever was written.

There is no phased release and no `release-check`. Both existed to manage
a nested module requiring a *previous* root; that situation no longer
arises.

### When it has already gone wrong

Retract; never delete. In the affected module's `go.mod`:

```
// Why this version is bad, and what to use instead.
retract v0.0.1
```

Then release a higher version carrying that `retract`. Go reads
retractions from the highest available version, so the fix only takes
effect once the new version is published.

## What `go.work` is and is not for

`go.work` joins the three modules so editors and `go build ./...` in any
directory see the tree. It is *not* what makes the release work — the
`replace` directives are, because `go mod tidy` ignores the workspace
entirely. If you find yourself reaching for `go.work` to fix a module
resolution problem, check `make replaces` first.

Never "fix" a nested module by pointing a replace somewhere outside the
repository, and never by letting a first-party require drift off the
released version. `make replaces` and `scripts/versions.sh check` are
the two gates, and `release-guard` runs the second again before a tag.

## Conventions

Anything a harness records beyond the core entry types goes through a
namespaced entry type or a namespaced item, never a new core field. A
reader must preserve what it does not understand; that is tested, not
assumed.

Golden files live under `testdata/`. `go test ./... -update` rewrites the
generated ones; the session fixtures under `testdata/sessions` are
written by hand in the exact form the library emits, because the
round-trip test compares bytes.
