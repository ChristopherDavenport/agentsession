# Plan: the git checkouts a session works in

A session that runs git changes more than the one tree its `env` entry
describes. It creates worktrees, commits, switches branches and pushes,
and the record keeps none of that except as text in tool calls. This
plan lets a reader follow each checkout a session touched and how its
state moved, and lets a replay rebuild each one as the calls found it.
Written 2026-11-26, against format 0.21 (v0.0.41). No issue is filed
yet.

## Why

The `env` entry is "a snapshot of the environment for replay". Its
`vcs` says which revision one tree was on and whether it was dirty, and
`cwd` says which directory that tree is. That holds for a session that
edits one checkout in place. It fails for the way agents work in this
workspace:

- **A worktree made mid-session.** `git worktree add -b x
  .worktrees/repo-x origin/main` is a `function_call` and its output.
  Every later call that works there names the worktree only in its
  arguments. `cwd`, `workspace` and `vcs` still describe the root.
- **A root that is not a repository.** A session started in a directory
  that holds several repositories has no `vcs` at all, though every
  call it makes acts in one of them.
- **Commits, branches and pushes.** dax reads `vcs` at each run start
  and writes an `agenteval:snapshot` at run end: a patch of the root's
  tree against that start revision. Neither says which branch the work
  went on, which commits were made, or whether they were pushed.

A replay that starts from the recorded `vcs` rebuilds the wrong tree
for every call that worked anywhere but the root. A reader asking
"which branch did this session push" has to re-parse shell commands,
and gets no answer for git run from a script or a Makefile.

## What exists to build on

- RFC 0001's `env` entry: a whole snapshot that applies until the next
  one, with `cwd`, `vcs`, `files`, `tools` and `workspace`. A change
  to any member but `workspace` is not a substitution, so a new entry
  per checkout change costs a line and changes no verification.
- `vcs` already allows a namespaced member for what the core does not
  say (`"cline:tree"`), and the envelope section preserves it.
- `agentsession.GitRevision` (`env.go:188`) reads HEAD without a git
  binary and follows a worktree's `.git` file and `commondir`.
- `agenteval:snapshot` (agenteval `snapshot.go:15`): a run's workspace
  as `base` plus `data`, which a judge applies to the base in a fresh
  workspace. It has no notion of which tree it is.
- dax reads git by running it through the workspace (`dax/tool/gitdiff.go:85`),
  so the same code reads a local directory, a container or a remote
  runtime. It refuses to read a repository whose configuration would
  run a program (`execConfigKey`).
- dax's command analyzer (`dax/tool/check.go`) parses command lines
  into stages and knows git's subcommands. Today it only decides what
  runs without asking.
- agentworkspace confines every name to the workspace root, so a
  checkout a tool can act in is a path inside the workspace. A
  checkout outside it, such as `../.worktrees/x` from a repository
  root, is one the tools cannot reach.

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

- `path` is the checkout's top directory, relative to `cwd`, as an
  `fs.ValidPath` name (`.` for `cwd` itself). It is unique in the
  list.
- `repo` names the repository the checkout belongs to, as the
  checkout's common git directory relative to `cwd`. Worktrees of one
  repository share it, and that is how a reader groups them.
- `vcs` has the meaning the top-level `vcs` has, for this checkout.
- `branch` is the branch HEAD names, without `refs/heads/`. It is
  absent for a detached HEAD.
- `upstream`, optional, is the branch's upstream as the checkout's
  remote-tracking ref names it, and the revision that ref was on. A
  push or a fetch shows as the upstream moving. A remote's URL is
  never recorded: it can carry credentials, and the remote's name
  (`origin`) is enough to follow it.
- A checkout the writer saw but could not read, as dax refuses one
  whose configuration would run a program, is listed with `path` and
  `repo` and no `vcs`.

The top-level `vcs` keeps its meaning, the tree at `cwd`, so readers
of earlier minors read the same thing. When `checkouts` lists `.`, its
`vcs` equals the top-level one, and a writer MUST NOT write them
differently.

Since `env` is a whole snapshot, a checkout absent from a later entry
is no longer in force: removed, or no longer followed. A writer that
records a change to one checkout repeats the others (the size this
costs is under Cost, and the alternative under Open questions). Following a
session's git history is reading its `env` entries in path order and
comparing each checkout with the one before: a new `path` is a
checkout made, a new revision on the same branch is a commit (or a
reset), a new branch is a switch, and an upstream that moves is a push
or a fetch.

## Capture (dax)

dax decides what is a checkout and when to look again. agentworkspace
stays VCS-agnostic: a worktree is a directory in the workspace's file
system, not a workspace of its own, and `Descriptor` keeps meaning
"which file system".

- **The set is the checkouts the session touched.** At session start
  it holds the checkout at the workspace root, if there is one, and
  nothing else. A checkout joins when a git stage runs in it, by `-C`
  or by the stage's directory, and when a `worktree add` creates it.
  The other worktrees of a repository do not join with it: a
  repository with forty stale worktrees costs one checkout until the
  session works in another. Nothing scans the file system. The set
  carries over from the previous `env` entry on resume.
- **When to look.** At each run start, every checkout in the set,
  as now. After a tool call whose command line parses to a git stage
  that can move state (`worktree`, `commit`, `switch`, `checkout`,
  `merge`, `rebase`, `reset`, `pull`, `fetch`, `push`, `branch`,
  `stash`, `cherry-pick`, `am`, `tag`), using the analyzer's parser,
  only the checkout that stage ran in, and the one a `worktree add`
  made. At run end, every checkout in the set once, as the backstop for
  git run indirectly, by a script or `make`.
- **A cheap look.** A look reads HEAD, the branch and the upstream
  (`git rev-parse`, `git symbolic-ref`, `git for-each-ref`), and sets
  `dirty` from whether `git status --porcelain=v1 -z` prints any
  line. It never builds a diff. The full diff dax builds today
  (`git add -A -N` and `git diff`, `dax/tool/gitdiff.go`) is kept for
  the run-end snapshot of a checkout the run changed.
- **What it writes.** A new `env` entry when a checkout differs from
  the one in force, through agentturn's `session.WithEnv` hook as
  today. Nothing when nothing changed.
- **Snapshots.** The run-end `agenteval:snapshot` is written per
  checkout the run changed, each naming its checkout, not only the
  root.
- **Safety.** Every read goes through `ws.Exec`, under the
  `execConfigKey` rule that refuses a repository whose configuration
  would run a program.

## Cost

Measured on 2026-11-26 in the transparent-ai workspace: 18 repositories
under one directory that is not itself a repository, 24 registered
checkouts between them (agentflow 4; agentsession, agentturn and
examples 2; the rest 1), and 535 entries under `.worktrees/`, 7 of them
checkouts.

- A dax session started there begins with an empty set. Most sessions
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
- The time cost is the looks. With looks scoped to the stage's
  checkout and a status in place of a diff, a look after a
  stage is one checkout's few git calls, and the run-end backstop
  reads each checkout in the set once.

The size is O(checkouts × changes) because each `env` entry repeats
every checkout. The entry shape question below is whether to keep that.

## Without git

Without git the plan records what dax records today in a directory
that is not a repository: an `env` entry with `cwd` and `workspace`,
and no `checkouts`. Nothing fails, and replay has nothing to rebuild
from but `env.files`, the last point below.

- **No `git` binary in the workspace**, as in a slim container image or
  a remote runtime. Today, with snapshots on, `StartSnapshot`'s `git
  rev-parse` fails at the workspace's command lookup
  (agentworkspace `local.go:532`). dax prints `dax: snapshot: git
  rev-parse: …` on every run (`dax/front.go:314`), records no `vcs`,
  and writes the run-end snapshot with only an `Error`. Under the plan,
  dax tells a missing binary from a failed command, notices it once
  per session, stops looking, and leaves `checkouts` out. An absent
  member is RFC 0001's "the entry does not say", which is the truth
  here. An empty list would say there are no checkouts, which dax
  does not know.
- **A local fallback.** `agentsession.GitRevision` reads HEAD without
  a binary (`env.go:188`). For a local workspace, dax MAY use it to
  record `revision` and `branch` for the root checkout, with no
  `dirty`, no `upstream` and no worktree list, which it cannot read
  that way. It reads the host's file system, so it is no use for a
  container or a remote workspace.
- **No git, no moves.** Without a binary no stage can commit, switch or
  push, so the triggers never fire and nothing is missed. The gap is a
  repository already on disk, which the fallback covers in part.
- **Another version control system.** Mercurial, Subversion and Fossil
  record nothing under this plan. Jujutsu needs a note: a colocated jj
  repository has a `.git`, so the git reader reads it, but jj keeps the
  working copy as a commit on a detached HEAD, and every edit then
  reads as a new revision on no branch. dax should recognise a
  colocated jj repository (a `.jj` beside the `.git`) and record its
  `path` and `repo` with no `vcs` until jj has a reader of its own,
  rather than record the churn.
- **Replay with no version control at all.** The format already has a
  record for it: `env.files`, the content hashes of the files a run
  read and wrote (`{"read":{path:hash},"written":{…}}`), with helpers in
  `env.go:142`. A replay can check files against it whatever the
  workspace holds. dax does not fill it today (`envOf` sets `cwd`,
  `workspace` and `vcs`). Filling it is separate work, and it is the
  fallback this plan leans on where there is no git.

## Replay (agenteval)

- `Snapshot` gains `Path`, the checkout it is of; absent means `cwd`,
  as every snapshot written today is read.
- The runner's carry-forward of the previous `env` (`runner.go:786`)
  carries `checkouts`.
- A replay prepares each checkout the path names when it is first in
  force: the repository at its revision, the worktree on its branch,
  the snapshot's patch applied. A call then runs against the checkout
  its directory falls in.

## Tests

- RFC conformance fixtures for 0.22: an `env` with `checkouts`; one
  where `.` is listed with the top-level `vcs`; a reader of 0.21 that
  preserves `checkouts` as a member it does not know.
- A refusal of an entry whose `.` checkout and top-level `vcs`
  disagree, and of a `path` or `repo` that is not a valid path.
- dax: a session that adds a worktree, commits in it and pushes,
  against a local bare remote, records the worktree's creation, the
  commit and the upstream move as three `env` changes. A commit made by
  a script is caught at run end.
- dax: a repository with several worktrees, of which the session works
  in one, records that one alone. A stage in one checkout looks at that
  checkout and no other.
- dax: a workspace with no `git` binary writes `env` entries without
  `checkouts`, reports the missing binary once, and runs no further
  looks. A colocated jj repository is recorded with no `vcs`.
- agenteval: a replay of that session rebuilds the worktree at its
  recorded revision before the first call that works in it.

## Order of work

1. The RFC 0001 change to `env`, for review before code.
2. agentsession: `EnvEntry.Checkouts`, the `Checkout` type, the
   validation, fixtures, and `agentsession show` printing each
   checkout. Release.
3. dax: the set, the triggers, the `env` writes and the per-checkout
   snapshots, on the released agentsession.
4. agenteval: `Snapshot.Path`, the carry-forward and the replay.
5. agentimport, where a source records it. The Claude Code importer
   already writes each line's `gitBranch` as a namespaced member inside
   `vcs` (`claudecode/convert/walk.go:355`). It would write the
   `checkouts` entry for `.` with `branch` instead. The other importers
   to be checked one by one.

## Not in this plan

- **Moving sessions between stores from the command line**
  (`agentsession push` / `fetch`). Separate work, though a fetched
  session can only be replayed on another machine if it carries this.
- **Version control systems other than git.** `vcs.system` leaves room;
  the triggers and the set are git's. See Without git for what is
  recorded meanwhile.
- **Filling `env.files`** in dax, the record for a workspace with no
  version control. It is the fallback Without git names, and its own
  piece of work.
- **Recording each commit as an entry** with its message and author.
  The `env` comparison says a commit happened and which revision it
  made; the commit itself is in the repository.

## Open questions

- **Core member or namespaced?** `AGENTS.md` sends what a harness
  records beyond the core through a namespaced type or member. The plan
  puts `checkouts` in the core because `vcs` is already core, `env`
  exists for replay, and nothing here is dax's own: any harness that
  runs git has the same checkouts. A second writer already needs part
  of it: agentimport records Claude Code's branch as
  `claudecode:branch` because the core has no place for it. The alternative is
  `agentsession:checkouts`, or `dax:checkouts` promoted once a second
  writer exists. The RFC review decides.
- **Revisions only one machine has.** A commit made in the session and
  never pushed is not fetchable anywhere else, and neither is a base
  that was local. A replay elsewhere needs those objects. One answer
  is a git bundle of what the upstream lacks, stored as a blob the
  `env` entry names (dax's box already makes bundles for `dax/box.pull`).
  It is the same question as packs for push and fetch, so it may belong
  with that work.
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
- **How often.** Looking after every git stage is the most precise;
  looking only at run start and run end is the cheapest, and loses the
  order of changes within a run. The plan does both, scoped to the
  stage's checkout, and lets the run end catch what the triggers
  missed.
- **A checkout the tools cannot reach.** A worktree made outside the
  workspace root exists on disk but no tool can act in it. The plan
  leaves it out of `checkouts`, since `path` is relative to `cwd` and
  confined. Its creation is still visible as the repository's worktree
  list, if a reader wants it.
