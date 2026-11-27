# Plan: the git checkouts a session works in

A session whose tools run git changes more than the one tree its `env`
entry describes. It creates worktrees, commits, switches branches and
pushes, and the record keeps none of that except as text in tool
calls. This plan lets any reader follow each checkout a session
touched and how its state moved, and lets a replay rebuild each one as
the calls found it.

The format is for every writer: a live harness, an importer converting
another agent's log, a converter from a trajectory format. dax and
agentimport are the first writers this plan builds and agenteval is the
first replay, but the rules are written so that a harness with none of
their machinery can follow them. Written 2026-11-26, against format
0.21 (v0.0.41). No issue is filed yet.

## Why

The `env` entry is "a snapshot of the environment for replay". Its
`vcs` says which revision one tree was on and whether it was dirty, and
`cwd` says which directory that tree is. That holds for a session that
edits one checkout in place. It fails for how coding agents work:

- **A worktree made mid-session.** `git worktree add -b x
  .worktrees/repo-x origin/main` is a `function_call` and its output.
  Every later call that works there names the worktree only in its
  arguments. `cwd`, `workspace` and `vcs` still describe the root.
- **A root that is not a repository.** A session started in a directory
  that holds several repositories has no `vcs` at all, though every
  call it makes acts in one of them.
- **Branches, commits and pushes.** `vcs` has no branch. Nothing says
  which commits a session made or whether they were pushed.

A replay that starts from the recorded `vcs` rebuilds the wrong tree
for every call that worked anywhere but the root. A reader asking
"which branch did this session push" has to re-parse shell commands,
and gets no answer for git run from a script or a Makefile.

The sources agentimport reads show the same gap from the other side.
Five of them record something of git, and the format has no place for most of
it:

| Source | What it records | Where it lands today |
| --- | --- | --- |
| Claude Code | the branch on every line, no revision | `claudecode:branch` in `vcs`, gap `agentimport/vcs-branch` |
| Codex | revision, branch and repository URL at session start | `revision`; `codex:branch` and `codex:repository_url` in `vcs`, gap `agentimport/vcs-branch` |
| Hermes Agent | the session's branch | `hermes:branch` in `vcs`, gap `agentimport/vcs-branch` |
| OpenCode | that the project is under git | `vcs` with `system` alone |
| Cline | a shadow git repository for its checkpoints, and the work tree it tracks | the work tree as the header's `cwd`; no `vcs` |

Three sources carry a branch under three namespaces, and
`docs/incompatibilities.md` in agentimport lists the gap against the
RFC. That is the case for a core member: the need is not one
harness's.

## What exists to build on

- RFC 0001's `env` entry: a whole snapshot that applies until the next
  one, with `cwd`, `vcs`, `files`, `tools` and `workspace`. A change
  to any member but `workspace` is not a substitution, so a new entry
  per checkout change costs a line and changes no verification.
- `vcs` already allows a namespaced member for what the core does not
  say (`"cline:tree"`), and the envelope section preserves it.
- `agentsession.GitRevision` (`env.go:188`) reads HEAD without a git
  binary and follows a worktree's `.git` file and `commondir`.
- agentimport's `RevisionEnv` (`derive.go:189`) already derives the
  `env` entry after a commit by repeating the one in force with the new
  revision, which is the rule this plan writes down for every writer.
- `agenteval:snapshot` (agenteval `snapshot.go:15`): a run's workspace
  as `base` plus `data`, which a judge applies to the base in a fresh
  workspace. It has no notion of which tree it is.

## The format (RFC 0001, `env`, format 0.22)

`env` gains `checkouts`, a list of the checkouts in force:

```json
{"type":"env","id":"…","parent":"…","ts":"…",
 "cwd":"/home/u/work","workspace":{"kind":"local","host":"nova"},
 "checkouts":[
  {"path":"agentsession","repo":"agentsession/.git",
   "vcs":{"system":"git","revision":"4c61d09…","dirty":false},
   "branch":"main","upstream":{"name":"origin/main","revision":"4c61d09…"}},
  {"path":".worktrees/agentsession-x","repo":"agentsession/.git",
   "vcs":{"system":"git","revision":"9a1f…","dirty":true},
   "branch":"x"}]}
```

- `path` is the checkout's top directory. It is relative to `cwd`, as
  an `fs.ValidPath` name (`.` for `cwd` itself), when the checkout is
  under `cwd`, and absolute otherwise, for a harness whose tools reach
  outside it. It is unique in the list.
- `repo` names the repository the checkout belongs to, in the same
  form as `path`. For git it is the common git directory, which every
  worktree of one repository shares, so a reader groups worktrees by
  it. Another system names what its checkouts share.
- `vcs` has the meaning the top-level `vcs` has, for this checkout.
- `branch` is the branch the checkout is on, without `refs/heads/` for
  git. It is absent when the checkout is on no branch, as a detached
  HEAD is.
- `upstream`, optional, is the branch's upstream as the checkout names
  it (`origin/main`), and the revision it was on. A push or a fetch
  shows as the upstream moving.
- A remote's URL is never in `checkouts`: it can carry credentials,
  and the upstream's name is enough to follow it. A writer that keeps a
  URL in a namespaced member, as the Codex importer does, removes any
  userinfo first.
- A checkout the writer knows of but could not read is listed with
  `path` and `repo` and no `vcs`.

The top-level `vcs` keeps its meaning, the tree at `cwd`, so readers
of earlier minors read the same thing. When `checkouts` lists `.`, its
`vcs` equals the top-level one, and a writer MUST NOT write them
differently.

Absent `checkouts` means the entry does not say, as for any member of
`env`. An empty list says there are no checkouts in force. A writer
that could not look, because the workspace has no git or the source
log never recorded it, leaves the member out.

Since `env` is a whole snapshot, a checkout absent from a later entry
is no longer in force: removed, or no longer followed. A writer that
records a change to one checkout repeats the others (the size this
costs is under Cost, and the alternative under Open questions).
Following a session's git history is reading its `env` entries in path
order and comparing each checkout with the one before: a new `path` is
a checkout made, a new revision on the same branch is a commit (or a
reset), a new branch is a switch, and an upstream that moves is a push
or a fetch.

## What a writer records (RFC 0001, normative for any writer)

These go into the RFC as SHOULDs, so that two harnesses recording the
same work write the same checkouts.

- **The checkouts the session worked in.** A writer lists a checkout
  once the session's tools act in it, and a checkout a tool made, as a
  `worktree add` makes one. It does not list every checkout it could
  find: not every worktree of a repository, not a scan of the
  directory. A writer that cannot tell where its tools acted lists the
  checkout at `cwd` alone.
- **When it changes.** A writer that can observe the workspace writes
  a new `env` entry when a checkout it lists differs from the one in
  force, and does not write one when nothing changed. How often it
  looks is its own choice; the RFC says only that an entry describes
  the state at its position.
- **What it knew.** A writer converting a log records what the log
  says and no more: a branch with no revision is a `vcs` with no
  `revision`, and a log that records git state only at the start gives
  one `env` entry, not a guess at later ones.
- **Reading safely.** A writer that reads a repository by running its
  version control system runs it as a tool would, in the workspace,
  and does not run one whose configuration would execute a program
  of the repository's choosing.

## Writers

### dax, a live harness

dax is the first live writer. It decides when to look; agentworkspace
stays VCS-agnostic, since a worktree is a directory in the workspace's
file system, not a workspace of its own.

- **The set.** At session start, the checkout at the workspace root if
  there is one. A checkout joins when a git stage runs in it, by `-C`
  or by the stage's directory, and when a `worktree add` creates it.
  The set carries over from the previous `env` entry on resume.
- **When it looks.** At each run start, every checkout in the set.
  After a tool call whose command line parses to a git stage that can
  move state (`worktree`, `commit`, `switch`, `checkout`, `merge`,
  `rebase`, `reset`, `pull`, `fetch`, `push`, `branch`, `stash`,
  `cherry-pick`, `am`, `tag`), using the parser of its command analyzer
  (`dax/tool/check.go`), only the checkout that stage ran in. At run
  end, every checkout once, as the backstop for git run by a script or
  `make`.
- **A cheap look.** HEAD, the branch and the upstream (`git rev-parse`,
  `git symbolic-ref`, `git for-each-ref`), and `dirty` from whether
  `git status --porcelain=v1 -z` prints any line. The full diff dax
  builds today (`dax/tool/gitdiff.go`) is kept for the run-end snapshot
  of a checkout the run changed.
- **Where it reads.** Through `ws.Exec`, so a local directory, a
  container and a remote runtime read alike, under the `execConfigKey`
  rule. Entries go out through agentturn's `session.WithEnv` hook, as
  today, so any harness built on agentturn has the same place to write
  them.
- **Snapshots.** The run-end `agenteval:snapshot` is written per
  checkout the run changed, each naming its checkout.

### agentimport, converters

Each converter maps what its source records, and the
`agentimport/vcs-branch` gap closes:

- Claude Code: each line's `gitBranch` becomes `branch` on the `.`
  checkout, with no revision, in the `env` entry the converter already
  writes when the branch changes (`claudecode/convert/walk.go:349`).
- Codex: `commit_hash` and `branch` become the `.` checkout.
  `repository_url` stays `codex:repository_url`, with userinfo removed.
- Hermes Agent: the session's branch becomes the `.` checkout's
  `branch`.
- OpenCode: unchanged, since it records no branch or revision.
- Cline: unchanged; its shadow repository is Cline's checkpoint store,
  not a checkout the agent worked in, and it records nothing of the
  work tree's own git state.
- `RevisionEnv` repeats `checkouts` with the new revision on the
  checkout the commit was in.

Each change is a finding code and a line in the source's format
document, as agentimport's rules require.

### Other harnesses

A harness that records sessions through agentturn gets the hook dax
uses. One that writes the format directly needs only the rules above.
The library gives it the `Checkout` type and the reading helpers below;
it does not give it a git reader, since how to reach the workspace is
the harness's.

## Readers

- **The library.** `EnvEntry.Checkouts`, `Checkout` and `Upstream`
  types that keep members they do not know, as `VCS` does, and
  `CheckoutChanges(before, after *EnvEntry)`, which names what moved
  between two entries: a checkout added or removed, a revision, a
  branch, an upstream, `dirty`. Following a session's git history is
  folding that over its `env` entries.
- **`agentsession show`** prints each `env` entry's checkouts, and the
  changes from the entry before.
- **agenteval, the first replay.** `Snapshot` gains `Path`, the
  checkout it is of; absent means `cwd`, as every snapshot written
  today is read. The runner's carry-forward of the previous `env`
  (`runner.go:786`) carries `checkouts`. A replay prepares each
  checkout when it is first in force (the repository at its revision,
  the worktree on its branch, the snapshot's patch applied) and runs a
  call against the checkout its directory falls in.

## Cost

Measured on 2026-11-26 in the transparent-ai workspace: 18 repositories
under one directory that is not itself a repository, 24 registered
checkouts between them (agentflow 4; agentsession, agentturn and
examples 2; the rest 1), and 535 entries under `.worktrees/`, 7 of them
checkouts.

- A session started there begins with no checkouts. Most sessions
  touch one or two repositories and record one or two checkouts.
- The worst case is a sweep that works in every repository, as a fix
  batch does: 24 checkouts. A checkout is about 300 bytes of JSON (a
  path, a repo, two 40-hex revisions, a branch), so an `env` entry
  listing all 24 is about 7 KB. Each entry differs from the one before
  it, so the content store shares nothing between them. A long sweep
  of 30 runs with five git stages that move state in each writes up to
  about 180 entries: 1 to 1.5 MB of `env`.
- None of it reaches the model's context. The cost is storage, and the
  time a reader takes to walk the path.
- For a live writer the time cost is the looks. dax's, scoped to the
  stage's checkout and with a status in place of a diff, is one
  checkout's few git calls after a stage, and each checkout once at
  run end.

The size is O(checkouts × changes) because each `env` entry repeats
every checkout. The entry shape question below is whether to keep that.

## Without git

Without git a writer records what is recorded today in a directory
that is not a repository: an `env` entry with `cwd` and `workspace`,
and no `checkouts`. Nothing fails, and a replay has nothing to rebuild
from but `env.files`, the last point below.

- **No `git` binary in the workspace**, as in a slim container image or
  a remote runtime. The writer leaves `checkouts` out, which says it
  does not know, rather than an empty list, which would say there are
  none. dax today, with snapshots on, fails `git rev-parse` at the
  workspace's command lookup (agentworkspace `local.go:532`), prints
  `dax: snapshot: git rev-parse: …` on every run (`dax/front.go:314`)
  and writes the run-end snapshot with only an `Error`. Under the plan
  it tells a missing binary from a failed command, notices it once per
  session and stops looking.
- **A local fallback.** `agentsession.GitRevision` reads HEAD without
  a binary (`env.go:188`). A writer whose workspace is the local file
  system MAY use it for `revision` and `branch` of the root checkout,
  with no `dirty`, no `upstream` and no worktree list. It reads the
  host's files, so it is no use for a container or a remote workspace.
- **No git, no moves.** Without a binary no tool can commit, switch or
  push, so nothing is missed. The gap is a repository already on disk,
  which the fallback covers in part.
- **Another version control system.** `checkouts` is not git's alone:
  `vcs.system`, `repo` and `branch` are worded for any system. This
  plan builds git's writers only. Jujutsu needs a note: a colocated jj
  repository has a `.git`, so a git reader reads it, but jj keeps the
  working copy as a commit on a detached HEAD, and every edit then
  reads as a new revision on no branch. A git writer that finds a
  `.jj` beside the `.git` records the checkout's `path` and `repo` with
  no `vcs` until there is a jj writer, rather than record the churn.
- **Replay with no version control at all.** The format already has a
  record for it: `env.files`, the content hashes of the files a run
  read and wrote (`{"read":{path:hash},"written":{…}}`), with helpers in
  `env.go:142`. A replay can check files against it whatever the
  workspace holds. dax does not fill it today (`envOf` sets `cwd`,
  `workspace` and `vcs`); filling it is separate work.

## Tests

- RFC conformance fixtures for 0.22: an `env` with `checkouts`; one
  where `.` is listed with the top-level `vcs`; one with an absolute
  `path`; one with an empty list; a reader of 0.21 that preserves
  `checkouts` as a member it does not know.
- A refusal of an entry whose `.` checkout and top-level `vcs`
  disagree, of a duplicate `path`, and of a relative `path` or `repo`
  that is not a valid path.
- `CheckoutChanges` over each kind of move, and over members it does
  not know, which it compares and keeps.
- dax: a session that adds a worktree, commits in it and pushes,
  against a local bare remote, records the worktree's creation, the
  commit and the upstream move as three `env` changes. A commit made by
  a script is caught at run end. A repository with several worktrees,
  of which the session works in one, records that one alone. A
  workspace with no `git` binary writes no `checkouts` and reports the
  missing binary once. A colocated jj repository is recorded with no
  `vcs`.
- agentimport: each source's fixture with a branch converts to a `.`
  checkout, and no longer reports `agentimport/vcs-branch`.
- agenteval: a replay of the dax session rebuilds the worktree at its
  recorded revision before the first call that works in it.

## Order of work

1. The RFC 0001 change: `checkouts` in `env`, and the writer rules.
   For review before code.
2. agentsession: the types, the validation, `CheckoutChanges`, the
   fixtures and `agentsession show`. Release.
3. On that release, in parallel, since they share no files:
   - agentimport: the converters, `RevisionEnv`, the finding codes, and
     the gap removed from `docs/incompatibilities.md`.
   - dax: the set, the looks, the `env` writes and the per-checkout
     snapshots.
   - agenteval: `Snapshot.Path`, the carry-forward and the replay.

## Not in this plan

- **Moving sessions between stores from the command line**
  (`agentsession push` / `fetch`). Separate work, though a fetched
  session can only be replayed on another machine if it carries this.
- **Writers for version control systems other than git.** The format
  leaves room; see Without git for what is recorded meanwhile.
- **Filling `env.files`** in dax, the record for a workspace with no
  version control. It is the fallback Without git names, and its own
  piece of work.
- **Recording each commit as an entry** with its message and author.
  The `env` comparison says a commit happened and which revision it
  made; the commit itself is in the repository.
- **A git reader in the library.** How a writer reaches its workspace
  (a local directory, a container, a remote runtime) is the harness's.
  If a second live harness wants dax's reader, it can move to a shared
  package then.

## Open questions

- **Core member or namespaced?** `AGENTS.md` sends what a harness
  records beyond the core through a namespaced type or member. The plan
  puts `checkouts` in the core because `vcs` is already core, `env`
  exists for replay, and the need is every harness's: three importers
  already carry a branch under three namespaces for want of a core
  member. The alternative is one shared namespace,
  `agentsession:checkouts`, which every writer would still have to
  agree on. The RFC review decides.
- **Revisions only one machine has.** A commit made in the session and
  never pushed is not fetchable anywhere else, and neither is a base
  that was local. A replay elsewhere needs those objects. One answer
  is a git bundle of what the upstream lacks, stored as a blob the
  `env` entry names. It is the same question as packs for push and
  fetch, so it may belong with that work.
- **Entry shape.** The plan lists every checkout in each `env` entry,
  so a reader gets the whole environment from one entry, as `env` is
  read today, and pays O(checkouts × changes) in size (see Cost). The
  alternative is an entry per checkout, applying until the next one for
  the same `path`, with an entry that ends a checkout. A change then
  costs one checkout's bytes, and a reader rebuilds the set by folding
  every such entry on the path. That gives up `env`'s whole-snapshot
  reading for checkouts and adds a rule the rest of `env` does not
  have. At the sizes measured here the plan's shape is affordable; the
  RFC review decides whether a larger workspace makes it not.
- **Absolute paths.** A checkout outside `cwd` is written as an
  absolute path, which names the user's directories. agentredact's
  home redaction already rewrites such paths in `cwd`; it needs to
  cover `checkouts` too, and the RFC's redaction section should say so.
