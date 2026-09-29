# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

Draft 0.9 is amended in place again, from an independent review of
v0.0.13.

- **A call ID names one call in a session, on any branch, and is
  never empty.** A subsession's ID is derived from it, so two branches
  reusing one derived the same child. `Append` checks a set of the
  session's call IDs rather than reading the path, and refuses a
  function call with no `call_id`. The RFC adds that a writer that
  makes up its own IDs sends them to the provider, or omits
  `request_hash`, since the request the path rebuilds must be the one
  sent.
- **A decision or dispatch names its call by `target`**, and its
  `call_id` must be that call's. `Append` refuses one whose target is
  no function call on the path or names another call, with the new
  `ErrBadTarget`, and `VerifyRecords` reports one. `Calls`, and so
  every reader of calls, binds a decision or dispatch to the call its
  target names, and an output to the latest call with its ID.
- **A `reject` is for a call with no output.** `Append` refused an
  `answer` after the output and took a `reject`, which also hid a
  missing dispatch from `VerifyRecords`; it now refuses both with
  `ErrCallCompleted`, and `VerifyRecords` reports both.
- **`source: resume` is the run's opening shape**: its first item,
  decision or dispatch takes up a pending call. v0.0.13 let an entry
  anywhere in the segment decide it, which the writer of `start` could
  not know.
- **A run's calls are found by binding**, so in a file that repeats a
  call ID a run that answers the later call no longer lists the earlier
  one as pending.
- **Appending an entry the session holds is a no-op again** whatever
  followed it; the call rules ran before the held check and refused
  one.
- **Appending is faster on long paths.** A decision or dispatch reads
  only the entries about its call, and a function call checks a set:
  5,000 calls with a dispatch and an output each append in 3.6s, from
  about 8s in v0.0.13 and 5.7s in v0.0.12.
- **otel binds as the library does.** In a file that repeats a call
  ID the exporter matched the output to the first call and emitted no
  span for the later one.
- **`export.ItemsFrom` gives a repeated or missing tool call ID one of
  its own** (`call_0#2`), and the observation results after it follow,
  so a valid ATIF document that numbers calls per turn appends.
- **`agentsession verify` notes when a 0.9 file breaks a rule 0.9
  gained after v0.0.12**, which may mean that writer produced it
  rather than that it is corrupt. The RFC asks a reader to say the
  same.

## v0.0.13 - 2026-09-29

- **A run's calls include the earlier calls it took up.** RFC 0001
  draft 0.9 is amended in place: a run's calls are those on its
  segment and those made before it that the segment holds a decision,
  dispatch or output for, and its pending calls are those of them with
  no output on the path. `Run.Calls`, `Run.Pending`, `EndRun` and
  `ComputeReason` read them so. A resume whose policy holds a call a
  crash left never started, before any model call, now ends
  `input_required` with that call in `pending`; it read `aborted` with
  an empty list, and a writer that wrote `input_required` failed
  `VerifyRecords`. A call an earlier run left and this run does not
  touch is still not its call, and is not in its `pending` list:
  `Session.PendingCalls` reads every call the path left open. `source`
  follows: a run whose segment takes up such a call by a decision or
  dispatch is a `resume`, where the RFC asked for its output.
- **`CallRejected`.** A call whose `reject` is on the path and whose
  refusal output is not, since the record stopped between the two,
  reads as `CallRejected` rather than `CallNeverStarted`, or
  `CallUnknown` in a file that does not record dispatches: it is owed
  that output and nothing else, and must not be run. The RFC says so
  beside the same rule for `answer`. The otel exporter reports the
  state for a rejected call the record left without its output.
- **Nothing but its output follows a `reject`, and a `reject` follows
  no `dispatch`.** `Append` refuses a decision after a `reject` with
  `ErrCallRejected`, and a `reject` for a dispatched call with the new
  `ErrRejectDispatched`; a call that may have run is ended by an
  `answer`. `VerifyRecords` reports both in a file another writer
  produced. A `hold` after a `reject` read as a call waiting on an
  answer. This also refuses an `answer` after a `reject` (#103).
- **A call ID names one call on a path.** The RFC now says so: a
  writer whose provider repeats an ID, or gives none, writes one of its
  own. `Append` refuses a function call whose call ID is already on the
  path with the new `ErrCallIDRepeated`, and `VerifyRecords` reports
  one. In a file that repeats one, `Calls` takes what follows to name
  the latest call with the ID; it folded every repeat into the first
  call, so a later call's decisions, dispatch and output landed on the
  earlier one. (#121)
- **0.9 is amended in place.** A 0.9 file v0.0.12 wrote may fail
  `VerifyRecords` under these rules: a run end whose run took up a call
  made before its segment and left it without an output, a decision
  after a `reject`, a `reject` after a `dispatch`, or a repeated call
  ID.

## v0.0.12 - 2026-09-29

- **RFC 0001 draft 0.9; the library writes `agentsession/0.9`.** One
  element and a paragraph in the `config` section. A 0.5 to 0.8 file
  reads as it stands, with nothing rehashed; v0.0.11 refuses a 0.9
  file, as a 0.x reader refuses a later minor.
  `testdata/sessions/v0.8/replay.jsonl` keeps the 0.8 fixture as
  v0.0.11 released it, and a test reads it. 0.8 kept the omitted
  list in force until a config changed it, but a config that changed
  it wrote all of it: under a memory at its budget every save of a new
  fact and every forget moves a part across the budget, and one part
  moved rewrote 475, 37 KB and 12% more than the joined string.
  `instructions_omitted` now takes the `{"keep":n}` element
  `instructions_parts` took in 0.7, counted by the same cursor over
  the list in force, and the same save is 93 bytes. `OmittedPart.Keep`,
  taken only from a positive integer written as digits, and
  `OmittedPart.ID` is now `omitempty`. `Settings.OmittedDelta` returns
  the member that takes the list in force to a new one: nil when
  nothing moved, `[]` to clear, and otherwise every run of unchanged
  parts in order as a keep. The context resolves each keep against the
  list before its entry, so `Settings.InstructionsOmitted` and a
  compaction's checkpoint hold the list whole; a keep that cannot be
  satisfied stays in the list as written, which
  `OmittedPart.Unresolved` reports. `Append` refuses a keep carrying
  other members, a list with a keep that names an id twice, and a keep
  in a replace, which discards the list it counts over. `describe`
  prints a keep as `+n`, and `show` an unresolved one. The writer half,
  writing keep runs, is agentturn/session's. (#115)
- **cas packs its objects, as git does.** Each envelope and body was a
  file of its own, so a store of 220-byte objects took several times
  its bytes on disk. `Store.Pack` moves loose objects into one
  immutable pack with a sorted index, named by its checksum, and
  `Sweep` is now git's gc: it repacks everything the store holds into
  one pack and drops what nothing needs, sparing a young unneeded
  object by writing it back loose with its pack's age. `Import` and an
  exchange write what they bring as a pack. `Summary.Size` is the
  bytes of the session's own objects, carried in the log, where it was
  the size of the log. (#106)
- **cas has a read-only open.** `WithReadOnly` takes no session lock,
  recovers in memory and refuses every write with
  `agentsession.ErrReadOnly`, so an operator can read, project and
  verify a session a harness holds, and a read of an idle one no
  longer locks the harness out of it. (#107)
- **The cas journal is checksummed, and recovery only adds.** A
  flipped byte in a journal record was skipped as a torn write, and
  recovery then rewrote the intact log without that append, or left a
  session that would not open. Each record now ends in a CRC-32C, and
  a whole line that does not read is reported as damage; a crash cuts
  a record short only at the journal's end. A log holding entries the
  journal lacks keeps them, and the head does not move back past them.
  Records written by v0.0.11 have no checksum and read as before.
  (#108)
- **A cas sweep waits for writers rather than stopping, and does not
  block its own process.** It stopped with `ErrSessionLocked` at the
  first object a writer in another process held, and held the store's
  mutex for its whole run. It now builds its keep set and pack without
  either, waits for writers until its context ends, and holds the
  sweep lock only for a short last step that rescues anything
  committed since it looked. A second sweep or pack gets
  `ErrSweepRunning`. (#109)
- **cas appends cost fewer fsyncs, and a writer may choose when to
  pay them.** HEAD, the log and the mark are indexes the journal
  rebuilds, now written without an fsync, and a commit syncs each
  directory it touched once: a durable append is five fsyncs where it
  was seven. `WithSync` takes jsonl's policies, `SyncEveryAppend`,
  `SyncOnResponse` and `SyncNever`, with `Store.Sync`; a lazy append's
  record says so, the next durable commit flushes its objects first,
  and after a crash a lazy append whose objects were lost is gone with
  what followed it, not damage; a store that recovers a session
  holding another process's unsynced lazy appends syncs them first,
  and `Release` syncs what this store left. `Result.Durable` says
  which an append got. (#110)
- **cas List and Delete after a crash.** List no longer yields an
  error for a directory a crash left without a header, nor lists a
  session the journal deleted, and Delete succeeds once its record is
  committed, leaving the directory to recovery. (#111)
- **cas push and fetch.** `Store.Push` and `Store.Fetch` carry a
  session's closure between two stores as RFC 0002's exchange section
  describes: admission parent first as one pack, the log merged as a
  set, the head moved by compare-and-swap or force on a push and only
  forward on a mirror's fetch, a push only from the record, and a
  handover that makes the receiver the record before the sender a
  mirror. A mirror can now follow its record. (#113)
- **cas names a corrupt object, and verifies a whole store.** Every
  object read is checked against its name, and one that fails is
  reported as `ErrCorrupt` with its hash and path, where it surfaced as
  a bad line of a file cas assembled, once for each session sharing
  it. `Store.Verify` walks the store as git fsck does: journal records,
  loose objects, packs, every entry held and every session. (#114)
- **Appending an entry the session holds is a no-op in every store.**
  RFC 0002 makes a second append of the same type, content and parent
  at the same `ts` one entry, reported as such. jsonl wrote its line
  again, so every reader then reported a repeated id, and sqlite failed
  the append on the database's uniqueness rule, so a writer retrying an
  append whose acknowledgement it lost got an error for one that had
  succeeded. Both now return the entry's id and write nothing, as cas
  did, and the store conformance suite holds every store to it. (#119)
- **`sqlite.Open` refuses another program's table of its names.** A
  file holding a foreign `entries` table, such as the one
  agentmemory/sqlite v0.0.5 wrote, failed with `no such column: id`
  and kept the `sessions` table and indexes created before it. `Open`
  now checks each existing `sessions`, `entries` and `holders` table
  against the columns the store gives it, names the table, its columns
  and the wanted ones, and applies the schema and its migrations in one
  transaction, so a refusal leaves the file as it found it. (#116)
- **The OpenTelemetry env event carries every workspace member and
  marks a substitution.** It copied `workspace.host` and
  `workspace.instance` by name, so a substitution through any other
  member, such as the node a pool rescheduled a sandbox onto, left two
  env events that read the same while the session and a strict replay
  said the file system changed. Every string member of `workspace` is
  now `workspace.<member>`, by key after `workspace.kind` and
  `workspace.ref`, and an env event whose workspace is not
  `SameWorkspace` with the one in force before it carries
  `agentsession.substitution=true`, from `Export` and from `Wrap`
  alike; a first env entry is not a substitution. (#117)
- **The CLI reads a cas store, and `verify` of nothing fails.** It
  took only a file and a jsonl root, and a cas session's directory
  holds `header`, a valid session file with no entries: `verify` of it
  printed `0 verified, 0 without hash, 0 failed` and exited 0 for a
  session whose entries it never read. `list <cas-root>` now lists a
  cas store, and `show`, `verify` and `export` take `<cas-root> <id>`,
  open the store with `cas.WithReadOnly`, so beside a writer holding
  the session's lock, and read the session as the file `Project`
  writes, so the output is what that file gives. A path inside
  `sessions/<id>` is read as that session through the store, with a
  note on stderr. `verify <cas-root>` runs `Store.Verify` and prints
  each problem and a count. `verify` says how many entries it read and
  had their ids checked against their hashes, and fails on a file with
  a header and no entries, and on a store with no sessions, since
  nothing was checked. (#105)
- **A store raises a session's format when it first appends to it.**
  A header was written once, at creation, so a 0.7 session v0.0.11
  had continued still said 0.7, and v0.0.10 read the 0.8 records in
  it by 0.7 rules, appended what 0.8 forbids, and reported the rest
  as hash mismatches. Every store now writes `agentsession.Format`
  into the stored header before this package's first append to a
  session whose header names an earlier minor, so an older reader
  refuses the session as a later format. The header is not hashed,
  so no id moves. cas writes its `header` file; sqlite updates the
  row in the append's transaction; jsonl rewrites the file once, the
  raised header and every later byte as it was, synced and renamed
  into place under the session's lock, and appends to the new file.
  An open, a read-only store and an append the session already holds
  change nothing. jsonl and sqlite leave a header before 0.5 alone,
  since a reader rehashes the entries of such a file and would not
  under a raised header; RFC 0001 puts appending to one out of scope,
  and cas never holds one, since an import migrates it. RFC 0001 now requires the raise of a writer, and RFC 0002
  of a store. (#112)

## v0.0.11 - 2026-09-29

- **RFC 0001 draft 0.8; the library writes `agentsession/0.8`.** One
  optional member, one verdict, and paragraphs in four sections. A
  0.5, 0.6 or 0.7 file reads as it stands, with nothing rehashed;
  v0.0.10 refuses a 0.8 file, as a 0.x reader refuses a later minor.
  Two rules read a 0.7 file differently, stated in the RFC: the
  omitted instruction parts stay in force past the entry that wrote
  them, and a run whose pending call is held after its dispatch ends
  `input_required` rather than `aborted`. No request and no hash moves
  with either. `testdata/sessions/v0.7/parts.jsonl` keeps the 0.7
  fixture as v0.0.10 released it, and a test reads it.
- **Omitted instruction parts stay in force.** The RFC said
  `instructions_omitted` applied to the entry that carried it, so a
  writer with a memory larger than its budget repeated every omitted
  part on every delta: 474 of them cost 37 KB a memory write, more
  than the joined string the parts were meant to beat. The list is
  now in force until a later config carries the member: a delta
  without it leaves it, `[]` or a `replace` without it clears it, and
  a compaction's checkpoint carries it. `Settings.InstructionsOmitted`
  holds the list in force, `Context.InstructionsOmitted` returns it,
  and a compaction and `Continue` carry it over. `ConfigEntry`'s field
  is now `omitzero`, so an empty, non-nil list is written as `[]`, and
  `null` is absent. A checkpoint member the typed field cannot hold
  exactly, one that is not a list of parts or a part whose id is
  spelled `ID`, is kept as written and none is in force, so a 0.7
  checkpoint carrying one still verifies. `describe` prints a clearing
  `[]` as `omitted cleared`. (#97)
- **A dispatch carries its idempotency key.** `DispatchEntry.IdempotencyKey`,
  promoted only from a non-empty string, and `WithIdempotencyKey`. A
  hand-off that repeats another carries its key, and a new key is a
  new operation, which a harness mints for new arguments.
  `Call.IdempotencyKey` returns the last dispatch's key, the one a
  further run repeats, and `Call.DispatchedArgs` the arguments that
  dispatch ran with, since a key paired with later arguments would
  replay the wrong operation; `Call.Args` stays the arguments a new
  hand-off runs with. The key has to outlive the process that minted
  it, and a harness that wrote it as a member of its own could not be
  read by another. On read the member moves out of
  `EntryBase.Unknown` into the typed field, so code that looked for it
  in `Unknown` finds it gone. `describe` prints it. (#98)
- **A second dispatch is a second hand-off.** `Call.Dispatches` holds
  every dispatch in path order, with `Dispatch` still the first. The
  ATIF projection keeps the first under `dispatch` and, for a call
  handed over more than once, lists every one under `dispatches`,
  where it used to overwrite the first with the last. otel opens a
  tool span per hand-off, carrying `agentsession.call.dispatch`, the
  hand-off's number. A hand-off its run left without an output ends
  with the run, in flight, and the next hand-off or the answer links
  to it; a tracker primed from an earlier process never saw that
  process's spans and links to none. A hold answered by a later
  decision no longer leaves the span reading held, and a decision
  after the run end that changes what the path reads for a pending
  call reaches a span of its own. A span that records an answer or a
  later state links to the hand-off as `follows_hand_off`, keeping
  `dispatched_again` for a hand-off, and starts no earlier than its
  run.
  `Call.InFlight` reports a call with a dispatch and no output that no
  later decision holds or answers, which is what the RFC now calls in
  flight. `Call.Held` reads a hold after a dispatch as holding the
  call, where it used to read any call with a dispatch as not held;
  such a call's `State` is `CallHeld`, its `Dispatches` say it may
  have run, and `ComputeReason` reads the run `input_required`. (#99)
- **`answer`, a verdict for a call answered without running again.**
  `VerdictAnswer` ends a call that may already have run with an output
  the harness wrote, and its `by` and `reason` say who answered and
  why; neither `proceed` nor `reject` could say it honestly, so who
  answered an ambiguous call after a crash went unrecorded.
  `Call.Answered` reports one. An answer whose output the record
  stopped before reads as the new `CallAnswered` state, not in flight:
  the harness that continues writes the output and nothing else.
  Only the output may follow an answer: `Append` refuses a dispatch or
  any decision after one with `ErrCallAnswered`, an answer to a call
  that has its output with `ErrCallCompleted`, and, in a session whose
  header promises dispatch records, an answer to a call with no
  dispatch with `ErrAnswerNotDispatched`; `VerifyRecords` reports each
  in a file. otel ends the call's span with the state `answered`, at
  its run's end too when the output is owed, and not as an error.
  (#100)
- **A trigger keeps its own members.** `Trigger.Unknown`, encoded
  inline beside kind, ref and source, with `SetMember`, `Clone` and
  `Equal`, so a queued firing keeps when it was due and which attempt
  it is, and `Drain` copies them to the item. A run start's trigger
  with members of its own is now typed rather than kept whole in the
  entry's unknown members. `Trigger` holds a map, so `==` on it no
  longer compiles; use `Equal`. (#101)
- `testdata/sessions/replay.jsonl` is the 0.8 conformance fixture: a
  crash with two calls in flight, one run again under its key and one
  answered, omitted parts in force across a delta and a compaction and
  cleared by `[]`, and a queued firing drained with its own members,
  every run end and request hash verified.

## v0.0.10 - 2026-09-29

- **RFC 0001 draft 0.7; the library writes `agentsession/0.7`.**
  Additive: two optional members and one paragraph. A 0.5 or 0.6 file
  reads as it stands, with nothing rehashed; v0.0.9 refuses a 0.7 file,
  as a 0.x reader refuses a later minor. As with 0.6, the typed field
  of a new member is filled only from a member that decodes into it,
  and an earlier file's member of the same name in any other form is
  kept as written.
- **A run of unchanged instruction parts costs one element.** A delta
  named every unchanged part by its id, source and hash, about 130
  bytes a part, so a 3 byte patch to a memory block of 126 short facts
  wrote 17 KB, and the saving over the joined string shrank as the
  memory grew. `InstructionsDelta` now writes `{"keep":n}` for each run
  of parts that are unchanged and in the order they are in force, and
  leaves `source` off a part it names by hash, which the reader already
  inherited. The same patch is 427 bytes. `keep` counts from a cursor
  into the parts in force: naming a part in force moves the cursor to
  just after it, so a removal is a keep, the next part by hash and a
  keep, and an insertion is a keep, the new part and a keep. A keep
  that runs past the parts in force, or takes a part the delta names
  elsewhere, resolves as an unknown hash does: the `instructions`
  string beside it stands, and without one the request hash does not
  verify. So does a hash or keep over a part the path itself could not
  rebuild, which v0.0.9 turned into empty text, ignoring the string
  beside it; an element with neither an id nor a keep; and, in a
  compaction, parts the path could not rebuild, which the checkpoint
  now leaves out so that its `instructions` stand alone. A part with an
  id and neither text nor hash is empty text, as the writer spells
  one, and moves the cursor like any other part named; 0.6 called such
  a part unresolved, which the library never did. A delta over a part
  in force that is unresolved writes that part's text. `Append`
  refuses a keep that carries any other member, and a replacing config
  with a keep and no string. `InstructionPart.ID` is now `omitempty`,
  since a keep has none, and `InstructionPart.Unresolved` says whether
  a part in force has text the path could rebuild. (#94)
- **`attempts` on a response, and `llm_call_count` from it.** The
  exporter wrote `llm_call_count: 1` on every step, so a benchmark
  against a rate-limited provider read it as slow rather than flaky.
  `ResponseEntry.Attempts` records the calls a response took when the
  failed ones were retried without an entry of their own, `Calls`
  reads it with absent as one, the ATIF step carries it, and `show`
  prints it. `Append` refuses a negative count. The format carries the
  count rather than the exporter counting another project's custom
  entries; agentturn's half is to write it from its retry loop
  (agentturn#117). (#93)
- **A workspace holds its own host and instance.** The library told a
  writer to put a container's host beside `workspace`, where the
  substitution rule does not look, so a move to another host or a
  restart from the same image read as the same workspace. `Workspace`
  now has `Unknown`, encoded inline and kept on rewrite, and
  `SetMember` to set one; `SetWorkspace` returns the workspace it
  sets. `SameWorkspace` applies the substitution rule, every member
  compared. RFC 0001 says the members that tell one file system from
  another go inside `workspace`, and that members are compared in
  their canonical form. A key in another case, such as `Kind`, is a
  member of its own rather than read as `kind`, and `kind` is written
  only when set, so a workspace without one, which v0.0.9 refused on
  read, reads as written. The OpenTelemetry env event carries
  `workspace.host` and `workspace.instance` beside `workspace.kind` and
  `workspace.ref` when the workspace holds them as strings. (#95)
- `testdata/sessions/parts.jsonl` is the 0.7 conformance fixture:
  deltas that keep runs, a response that took retries, and a workspace
  with its host and instance followed by a restart onto another
  instance, every request hash verified.

## v0.0.9 - 2026-09-28

- **A member nested inside an object the library types is kept and
  hashed as read.** A reader computed an entry's id from its own
  re-encoding, so a member such as `host` inside `workspace`, which RFC
  0001 invites, was dropped on rewrite, and a file another writer
  hashed correctly was refused as corrupt while an edit to such a member
  went unnoticed. A member the line holds more of than the typed fields
  encode, a nested member they do not define or a zero value they omit,
  is now remembered at decode and grafted back onto what the fields hold
  when the entry is written: onto an object member the caller left in
  place, and onto an array element that still encodes as it did at
  read, matched where it was first and otherwise to the one element it
  still equals, so sorting `parents` keeps each reference's extras and a
  changed element takes none unless the change makes it equal to one
  that was removed. Equal elements that differ only in their extras
  cannot be told apart once one is removed. A null a payload type adds
  where the line has nothing reads as the same line; another zero value
  it adds does not, so a function tool written without `description`,
  which openresponses encodes as `""`, is refused as it was before and
  cannot carry a nested extra. A key in
  another case, such as `CWD` beside `cwd`, is a member the format does
  not define: Go's decoder would read it as the member it resembles, so
  such a line is decoded again without it and keeps it as unknown. A
  required member left out or a value the reader does not reproduce is
  refused as before. The RFC says preservation holds at every depth.
  `Read` canonicalises the typed encoding once, and a line that equals
  it, as a canonical line the typed fields hold whole does, is hashed as
  it stands without being decoded and re-encoded again, so reading is
  faster than before: about a fifth in the read benchmark. A line with
  members kept as read takes the slower comparison. (#89)
- **Who closes a run left open.** A rewind into a run leaves it open on
  the new path, as a crash leaves one open at the leaf; RFC 0001 now
  says the writer that continues such a path closes the run before
  appending anything else, `interrupted` after a rewind and `error`
  after a cut, and drops the sentence that a branch closes the
  open run, which held only for a branch to before the run's start. The
  loop owns the decision; `Branch`, `SummarizeBranch` and `EndRun` say
  when to write it. (#86; the entry is agentturn's to write, from
  `Rebase` and from `Resume`, which agentturn#120 tracks.)
- The member round-trip guard compares values as well as keys, fills
  slices with two elements, and walks the table `UnmarshalEntry` now
  decodes through, so a new core type is covered without being listed.
  (#50)
- **RFC 0001 draft 0.6; the library writes `agentsession/0.6`.**
  Additive: three optional members and three paragraphs. A 0.5 file
  reads as it stands, with nothing rehashed and no `legacy_id`, and its
  header takes the current format; v0.0.8 refuses a 0.6 file, as a 0.x
  reader refuses a later minor. A 0.5 file was free to hold `trigger` or
  `call_id` in any form while the names were undefined, so the typed
  field is filled only from a member that decodes into it; any other
  form stays in `Unknown` as written, and what the field cannot hold of
  a member it does take is written back as read (see #89 below), so the
  entry's hash still verifies.
  - `RunEntry.Trigger`: a run start carries the `Trigger` a `queued`
    entry already had, beside `Ref`, which is unchanged. A due time or
    an attempt goes in the run entry's own unknown members. The ATIF
    run record carries it as `trigger_parts`, since `trigger` there has
    always been the start's `ref`; OTel adds
    `agentsession.run.trigger.{kind,ref,source}`. (#82; the recorder
    half, writing it from the loop's trigger, is agentturn's.)
  - `CustomEntry.CallID`: the call a record belongs to, since a record's
    position cannot say which call of a parallel batch wrote it. The
    ATIF `custom` record carries it. (#87; the recorder half, filling it
    from `agenttool.CallFrom`, is agentturn's.)
  - A `proceed` no longer promises a `dispatch`: an approval that an
    abort or a later refusal overtook stays as written. `Call.State`
    and `VerifyRecords` already read it that way; tests now pin it.
    (#78, settling agentturn #105's open question.)
  - `env`: a later entry with a different `workspace` is a
    substitution, and a reader that holds the environment fixed treats
    the path from it on as unverifiable. Text only. (#88)
- **Every store honours a header's `base` at `Create`.** `MemoryStore`,
  `jsonl` and `sqlite` accepted a header with a base and refused the
  session's first append, and a jsonl file written that way could not
  be read back. `Create` now forks the session holding the base, the one
  `parent_session` names when it holds it and any other otherwise, and
  writes the prefix after the header, as `cas` already did; a base the
  store does not hold (`ErrNoEntry`), a `leaf` label or a media form
  other than the origin's is refused by `Create`, as is a prefix that
  could not stand as a file of its own: one holding an entry that
  converges an entry of the origin off the path (`ErrBadConvergence`),
  or an entry a migration could not rewrite (`ErrUnresolvedMigration`).
  The fork takes the origin's payload profile. The origin is read,
  never claimed or written. The jsonl store writes a new session's
  header and prefix to a temporary file and links it into place, so a
  crash cannot leave a fork whose prefix was cut short. `storetest` holds every store to it. The
  sqlite store gains an index on `entries(id)`. (#79, #84, #85; the
  sqlite store had the same defect. agentturn#118, `session.Start` on a
  forked header, can now build on it.)
- `Fork` keeps a `parent_session` the caller named, since RFC 0002 makes
  it provenance a header may point at either the origin or the session
  that owns the base, and refuses a `media` other than the origin's.
- `Session.Resolve` returns the entry an ID names, or the one whose
  `legacy_id` it is, so a report written before a file was migrated
  still finds its targets. `export.At` and the CLI's `-leaf` accept
  such an ID, and the document is built at and named by the entry's
  current ID; `Entry` stays strict, since `Append` checks a parent with
  it. (#80)
- `export.At` carries the outcomes below the entry it ends at that
  target its path, in `Trajectory.Outcomes`, so the document a score
  names says how it scored; steps and totals do not count them. (#81)

## v0.0.8 - 2026-09-28

- **RFC 0001 draft 0.5 is implemented and the library writes
  `agentsession/0.5`.** An entry's `id` is now the hash of its envelope
  — `type`, `parent`, `parents` when non-empty, `ts` and the hash of its
  body — so `Append` computes it and refuses an id a caller set that
  does not match (`ErrBadID`), a reader verifies every line and reports
  one that fails, and `EntryBase.ContentHash` gives the body's own hash.
  `ts` has one spelling, UTC with at most nine fractional digits, which
  `Append` converts a caller's time to and `Read` refuses any other
  form of. Every hashed member must be I-JSON by the exactness rule;
  `Append` refuses a body that is not, and the envelope's names are
  reserved in a body (`ErrReservedMember`). `Write` emits canonical
  lines, since preservation is of members and not bytes. Breaking for
  any caller that chose entry IDs.
- **Migration.** A 0.4 or earlier file reads by migrating in memory:
  every entry is rehashed with its references rewritten, keeps its old
  id in `EntryBase.LegacyID`, and takes the one `ts` spelling. A file
  holding an extension entry cannot be written back as 0.5, since the
  reader cannot rewrite what such an entry names (`Session.Migrated`,
  `ErrUnresolvedMigration`). A repeated id in a file is one entry,
  reported by `Session.Repeated`.
- **Leaf rule.** An append under the leaf makes the entry the leaf; an
  append elsewhere is a branch and the leaf does not move. A `leaf`
  label moves the leaf to its target wherever the label sits, when the
  target is one the leaf may rest on; the leaf never rests on a label,
  and a label it cannot honour is not in force on resume either.
  Appending an entry the session already holds is a no-op that returns
  its id.
- **Sessions with a base.** `Header.Base` names the entry another
  session's file this one continues from, `Fork` makes such a session
  in memory, and its file opens with the prefix; own entries hang from
  the base or from each other, and `Session.Prefix` tells the two apart.
  `Header.Redacted` marks a file whose hashes were recomputed over
  redacted bodies.
- **Context hash.** `Session.ContextHash` computes RFC 0002's cache key
  at an entry: incremental over the type and content hash of the
  entries that carry context, with per-run provenance members removed
  and a compaction's `first_kept` substituted.
- The CLI shows entry ids abbreviated to twelve hex characters, as git
  shows a commit, and `-leaf` resolves a full id, a unique prefix or a
  migrated entry's legacy id. ATIF documents are named by the leaf's
  digest without its `sha256:` prefix, since a colon is not a legal
  file name everywhere.
- **Writer-side normalisation.** `Append` rewrites a body that fails
  the I-JSON test rather than refusing it, as the format has a writer
  do: a whole number outside binary64 is rounded and written
  canonically, a lone surrogate escape becomes U+FFFD, a string that is
  not valid UTF-8 has each invalid byte replaced, and each change is
  recorded in the entry's `normalised` with its pointer and original
  source text, or its bytes in base64 when there was no valid text. The
  rewrite is what the caller's entry holds afterwards, so what is hashed
  and written is what a reader sees. A Go string that is not valid
  UTF-8, which the marshaller would otherwise repair silently, is
  repaired in place and recorded the same way. `NormaliseJSON` is the
  same rewrite over arbitrary bytes, for a harness that wants the
  record of a lone surrogate Go's decoder would otherwise repair
  silently before the entry exists. A repeated member name and a number
  that is not finite in binary64 are still refused, as is a
  `Normalised` element a caller set without a pointer or with both or
  neither of `Was` and `Raw` (`ErrBadNormalisation`); a caller's list
  is sorted on append. The `normalised` fixture carries 9007199254740993
  in three spellings and once in a member the profile types as a
  number, a lone surrogate with its `was`, and invalid UTF-8 with its
  `raw`.
- **The whole-number rule admits the canonical rendering of a double.**
  The RFC now says so: a whole number is I-JSON when binary64 holds it
  exactly or when it is the exact value of the canonical rendering of
  the nearest double. Canonical form is ECMAScript's, which pads the
  shortest digits with zeros below 10^21, so from 2^53 it writes many
  representable whole numbers, 2^60 as 1152921504606847000, as a whole
  number no double holds; under the old rule `Write` produced a line
  `Read` refused for any of them, and a 64-bit id could not be
  normalised at all, since its rounding failed the same test. The RFC
  also now fixes the substitution for invalid UTF-8 at one U+FFFD per
  maximal subpart (Unicode §3.9), which is what other languages'
  decoders do, says an out-of-bound integer is rounded wherever the
  profile does not type the member as a string, and says `was` and
  `raw` hold a string's quotes.
- export: a config entry inside the kept window after a compaction
  applies to the window alone. A step there shows the model and effort
  the call ran under, taken from the path, and a step after the
  compaction the checkpoint's then later deltas; the step's price and
  the path totals now agree for a response that names no model.
- otel's tests read their fixtures from the module's own `testdata`, so
  they run from the published module; the copies are generated with the
  root fixtures and a root test keeps them in step.
- The session fixtures under `testdata/sessions` are generated from the
  0.4 sources now kept under `testdata/sessions/v0.4`, and carry
  `legacy_id`; see CLAUDE.md.
- **New package `cas`: the content-addressed store of RFC 0002**, on a
  filesystem, laid out as git lays out a repository. An entry is two
  objects, its body under the content hash and its envelope under the
  id, so a body shared by many entries is held once and a fork stores
  nothing until it appends. A session is a directory with its header, an
  append-only log of entry hashes, a `HEAD` file and a record or mirror
  mark. `Append` writes the objects first, idempotently, then one
  journal record, which is the commit point and is fsynced, then the
  log line and the head; `Open` replays the journal's tail so a crash
  between the commit and the indexes loses nothing acknowledged.
  `Create` with `Header.Base` makes a fork whose prefix is the origin's
  shared objects. `SetHead` is the compare-and-swap of the head, with
  `ErrHeadMoved` when it is not where the caller thought; an append
  elsewhere than the head is a branch that moves nothing, and a `leaf`
  label moves the head to its target. `Project` writes the RFC 0001 file
  with a synthetic marker only when the resume rule would miss the head;
  `Import` reads one as a mirror unless told it is the record, verifies
  every line, refuses a redacted header and discards the marker; a
  mirror refuses local writes until `DeclareRecord`. `Sweep` removes
  what no log or prefix needs, following references down, and leaves a
  temporary file a writer may be about to rename. `Write` is `Append`
  reporting what happened, as the format asks: continued, branched,
  held, leaf moved or leaf not moved. A session ID or a hash that cannot
  be a path is refused (`ErrBadName`), a synthetic marker appended as an
  entry is refused (`ErrSynthetic`), and every rename or creation is
  followed by an fsync of its directory. Several processes share a
  store on one machine: each session is held by one at a time through
  `flock`, which the kernel drops when the holder exits, so there is no
  stale lock to detect; the journal is shared and never truncated by a
  reader, a record a crash cut short is skipped and the records after
  it still count, and recovery is per session by the process that holds
  it. `Sweep` works from the journal, so an acknowledged append whose
  log line never reached disk is kept, and it spares every object
  younger than a grace period the caller gives, as git spares a young
  loose object, so it needs no lock on writers and runs alongside live
  sessions; an object a new append finds already stored is freshened, as
  git freshens a loose object, and a fork's prefix is freshened when the
  fork is made, so neither can be swept between the write and the record
  that names it. A grace of zero is safe only with no writer active.
  Lock files live under `locks/` and are never unlinked, so a holder is
  never left locking an inode a delete removed. A network filesystem is
  not supported, since `O_APPEND` is not atomic across NFS clients. A session changed behind the store's
  back, by an append made on it directly or a leaf moved to an entry
  never committed, is refused (`ErrModified`). An index write that
  fails after the journal commit does not fail the append, which is
  durable; the next open repairs the index, and nothing after the commit
  point turns a durable append into a reported failure. Media blobs are
  content objects: `PutBlob` stores one under its hash, `Blob` reads it,
  the sweep follows a content's `sidecar:` references to keep them, and
  `ProjectDir` writes a sidecar session's blobs beside its file; an
  append naming a blob the store does not hold is accepted and reported
  in `Result.Unresolved`. The sweep holds the store's lock per object
  only, around a second stat and the remove, and a writer's shared lock
  is taken without blocking and retried until its context ends, so a
  writer never waits longer than one removal. `Result.Reopen` tells a
  caller that a durable append could not be applied to its session and
  it should open the session again. One session whose files cannot be
  read is reported at its own `Open` and hides no other. It runs the
  store conformance suite.
- A 0.x file of a later minor than this reader's is refused, since the
  0.x series is exempt from the rule that a reader reads every minor of
  its major. A migration reports a reference it cannot rewrite, into
  another session or to an id not seen earlier, and such a file is not
  re-emitted. A `ts` spelled with second 60 in an earlier file migrates
  as 59 with the same fraction. `ErrDuplicateEntry` is gone; nothing
  returned it. Exchange between stores, media sidecars and
  the SQLite relayout are not in this change.
- `Session.Prepare` and `Session.Commit` split `Append` into the part
  that computes an entry's hashes and outcome without adding it and the
  part that adds it, so a store can write and commit before anything is
  visible in memory. `Read` checks every line as written, before the
  decoder can repair a repeated member, a lone surrogate or invalid
  UTF-8; `ValidHash` says what a hash string may be.

- **Breaking for writers.** `Append` now refuses an item entry holding
  an `openresponses.ItemReference`. RFC 0001 gains the **ingress** rule
  — an entry receiving material from outside the session carries it
  materialised, and a reference to where it came from never stands in
  for it — and an item reference is the one shape that could satisfy
  every other rule and still not say what the model was sent. The
  format had already declined that dependency on the request side,
  which is built with `store: false` and no `previous_response_id`;
  this closes the same door on the payload side. Reading is unchanged:
  a file carrying one loads, projects and round-trips byte for byte,
  because such a file is incomplete rather than malformed.
- The RFC also gains the matching writing-discipline rule: per-run
  content a harness injects — a timestamp, a session ID — belongs in
  an `env` or record entry rather than in the request, where it
  rewrites `request_hash` every run and costs the prefix a provider had
  cached. `request_hash` itself is calibrated: it identifies a request
  and is not a token-exact prefix, so equal hashes imply equal leading
  tokens only where a provider's template, tool-schema serialisation
  and tokenizer are deterministic.
- The ATIF export no longer guesses which of a subagent's leaves
  answered a call. A `link` names the child session and is written when
  the call is dispatched, before the child has a point to name; the
  output entry's `parents` names the leaf the answer was taken from,
  which is the first moment the parent knows it. Where the record says,
  the export follows it: the embedded subagent trajectory is that path,
  `trajectory_id` names it, and `extra.child_leaf` records that the
  document rests on the record. Where it does not, the projection picks
  the child's main path as before, and the absence of `child_leaf` is
  what says the document turned on that choice.
- A record naming a leaf the export cannot resolve embeds nothing,
  rather than putting a different path of the child in its place.
  `trajectory_id` still names what the record said, so the reference is
  precise even when the child is not loadable.
- `Call.From` returns what a call's output entry converged, so a
  consumer can read the same fact without walking `parents` by hand.
- RFC 0001 draft 0.4 is implemented and the library writes
  `agentsession/0.4`; 0.3, 0.2 and 0.1 files read unchanged. An entry
  may now carry `parents`, further predecessors it converges: the leaf
  of the subagent session that answered a call, a branch merged back,
  several workers joined at once. `EntryBase.Parents` holds them as
  `EntryRef` values, each naming an `entry` and optionally the
  `session` it is in, since a bare entry ID is unique only within a
  file.
- `parents` is provenance and nothing else. It is not walked when
  building a context, so `Path`, `ContextAt` and every path-derived
  query follow `parent` alone, and a 0.3 reader given a 0.4 file
  rebuilds the same request byte for byte — which is what keeps this a
  minor version. That is now measured rather than asserted: the
  round-trip fixture is read twice, once with its `parents` members
  stripped, and the rebuilt request hashes must match at every leaf.
- `Session.Append` sorts an entry's `Parents` by session then entry, as
  the format requires of a writer, so a file does not depend on the
  order the workers happened to finish in. A file that arrives unsorted
  is written back as it came; sorting is a rule for writers.
- `Append` and `Read` both reject a convergence that breaks the
  format's rules, with the new `ErrBadConvergence`: a reference naming
  no entry, one naming the entry's own parent, one naming the same
  entry twice, and one into this session naming an entry that does not
  exist yet — which with `parent` is what makes the structure acyclic.
  A reference carrying this session's own ID is held to the same rules
  as one that leaves the session out. A reference into another session
  is checked for shape only; resolving it is a store's job.
- `agentsession show` marks an entry that converges others. The PARENT
  column is unchanged, because it is still the whole of an entry's line
  of descent.
- A leaf label now names the branch that is live rather than the entry
  the leaf is pinned at, so a session marked and then written on resumes
  where it was written to instead of rewinding to the mark. `Read`
  resolves the leaf to the newest entry in file order that descends from
  the mark and was appended after it; a mark nothing followed resolves
  to itself, and a file with no mark still resumes at its last line,
  which is now the degenerate case of one rule rather than a second
  rule. The mark carries both coordinates a log entry has — where it
  points in the tree and where it sits in the file — and only the first
  was being read (#55).
- That defect was not confined to context. Every path-derived query
  takes a leaf, so a reopen that rewound past the mark reported no
  pending call and no open run for work that was in flight; a
  `dispatch` is durable before the side effect it precedes, so a resume
  could fail to answer a call that may already have run. `PendingCalls`,
  `PendingQueued`, `Runs` and `OpenRun` all read correctly now.
- The change is in the reader, so a file already written resolves
  correctly without being rewritten, and `sqlite` inherits it by
  rebuilding the JSONL form and calling the same `Read`. The store
  conformance suite covers it, so every store is checked.
- One behaviour a caller may have relied on is gone: a mark can no
  longer pin the leaf at an entry its own branch has grown past.
  Nothing in the library could produce such a mark — `MarkLeaf` builds
  only from the current leaf — and the reserved label has always
  documented itself as naming the entry the next append should hang
  from. A durable pin would want its own label rather than an overload
  of this one.

## v0.0.7 - 2026-09-23

- `sqlite` and `otel` now require the root at exactly the version they
  are released at, rather than at the previous release, and carry a
  `replace` of the root pointing at the tree. Taking
  `agentsession/sqlite` alone now resolves the root commit it was built
  and tested against, instead of the one before it. Consumers ignore a
  `replace` in a dependency, so only the `require` reaches them; the
  published `go.sum` files no longer carry first-party entries. This is
  the shape OpenTelemetry-Go publishes.
- Requires `openresponses` v0.0.12, up from v0.0.10.

## v0.0.6 - 2026-09-21

- RFC 0001 is revised to draft 0.3 and the library writes
  `agentsession/0.3`; 0.2 and 0.1 files read unchanged. Context
  building now states how the request context of one response is
  found, since two implementations rebuild it: walking back from the
  response entry, an entry that is not an item is skipped, an item
  whose `response` names the response is output, and the walk stops at
  the first item naming another response or none. A reader identifies
  a response's output items by `response` and never by position; a
  writer SHOULD still keep them contiguous, so the envelope reads in
  the order it happened, and the RFC now says why rather than merely
  permitting both spellings. An entry another layer raises while a
  model call is in flight — a guard's verdict, a dispatch, a run
  boundary — is not an entry "for that model call", which the writing
  discipline previously left ambiguous (#40).
- `Session.RequestContext` follows that rule, so a custom or record
  entry written between two output items of one response no longer
  truncates the rebuilt request. A composed product that records a
  guard's verdict where the guard runs passes `verify` again; only the
  response's own item entries are removed from the path, and every
  other entry stays where it was (#40).

- New export `OutputEntries(path []Entry, resp *ResponseEntry)
  []*ItemEntry`, the rule for finding a response's own output items,
  over a path the caller already holds. `Session.RequestContext` calls
  it rather than keeping its own copy of the walk. The rule is
  implemented twice in the workspace — here, which excludes those
  entries from the rebuilt request, and in `agenteval`'s replay, which
  serves their items — with a comment rather than a compiler keeping
  the two in step. Exporting the selection is what lets the second
  reader drop its copy: it returns entries rather than items, because
  a reader that excludes them needs entry identity and items carry
  none, and it returns them in path order, not the backward order the
  walk runs in. The entries are the session's own, so a caller that
  serves their items to something that records must clone them. The
  RFC now states the two things a second implementation had to infer:
  a `response` with no `response_id` has no output items, and the
  order is normative.

- `CompactionEntry` gains `Pinned`, written as the optional `pinned`
  member: the items a fold kept verbatim from before `first_kept`.
  `BuildContext` places them immediately after `summary` and before
  the kept window, which is where the request that was sent had them,
  so a harness that holds an item out of a fold can record a
  `request_hash` it stands behind instead of recording none. Because
  `Context` carries them too, such a pin survives `Continue`,
  `Resume` and `Rebase` rather than being dropped at the fold. The
  ATIF projection carries them beside the fold's summary, since the
  entries they were copied from are before `first_kept` and so are not
  in the document. Additive: no file in existence carries the member,
  each pinned item is also an `item` entry on the path, and a reader
  that ignores it rebuilds a request short of those items rather than
  one that invents them. The RFC states the three rules a writer will
  otherwise get wrong: only an item carried by an `item` entry may be
  pinned, so a summary cannot be; the pins' order among themselves is
  the order the request carried them in; and only the last compaction
  on a path contributes items, so a later fold must restate a pin that
  is to survive it.

- **Fixed.** A `run` entry no longer drops the other phase's members
  when it is written back. The encoder writes one member list for a
  start and another for an end, and a file from elsewhere carrying
  `source` on an end, or `reason` or `pending` on a start, lost it
  silently: the member is declared on `RunEntry`, so the envelope rule
  did not preserve it in `Unknown` either. Those members are now
  written when set. Nothing this library builds sets them, so a
  well-formed entry is written exactly as before, byte for byte.

- **Breaking.** `Session.Verify` returns the new `ErrNoHash` for a
  response that recorded no request hash, where it returned nil. The
  two cases it could not tell apart — the rebuilt request hashed to
  the recorded value, and there was no recorded value to check — are
  now distinct, so a caller gating a build on `err == nil` is told
  when nothing was verified rather than passing. A caller that accepts
  unverified requests opts in with `errors.Is(err,
  agentsession.ErrNoHash)`. The recorder legitimately writes an empty
  hash whenever a layer edits the request outside the transcript — a
  transform that injects, a `BeforeModelCall` hook, a compaction whose
  folds were never reported — so a session in that state could pass a
  gate with nothing checked and nothing said. The CLI's `verify`
  output and exit code are unchanged: it already read `RequestHash`
  itself to draw the distinction the library would not provide.

- **Breaking.** `ComputeReason` takes the run's path as well as its
  segment, and reads the last response's own output items on the path
  to decide whether the model asked for a tool, so a run that starts
  between a function call and its response is not read as `done` while
  the call goes unanswered; `Run` carries the `Path` the segment ends, and the
  `stopped` step of the cascade reads it: a run that answers a call an
  earlier run's model call made and ends without calling the model
  again is `stopped`, not `aborted`. A refusal and a resume whose
  approved call terminates are both that shape, and a recorder that
  wrote what happened failed `Run.Verify` before. Passing the segment
  for both arguments, or a `Run` built by hand, reads as it did. The
  cascade's `aborted` step no longer catches a segment with no
  response; a sixth step does, so every segment still matches exactly
  one value. One consequence is deliberate: a call an earlier run left
  without an output keeps a later run from reading as `stopped` (#34).

- One `agentsession.ErrSessionLocked` in the root module for a session
  another process holds. `jsonl.ErrSessionLocked` and
  `sqlite.ErrSessionLocked` are that error, so a host written against
  the `Store` interface tells "another process has this session" from
  "the store is broken" without being told by its caller what its own
  store's errors mean. Matching either name still works; the message
  no longer carries the store's prefix (#41).
- `jsonl.WithReadOnly` and `sqlite.WithReadOnly` open a store that
  takes no lock or hold on the sessions it opens and refuses `Create`,
  `Append`, `Delete` and `Sync` with `agentsession.ErrReadOnly`, so
  `verify`, `show` and `export` work while the agent that owns the
  session is running. A read-only jsonl open reports a cut-short final
  line and leaves it in the file, since trimming it is a write. The
  CLI's `list` opens its store read-only; its other commands already
  read the file directly. `BreakLock` is refused too: a store that
  takes no lock has no business dropping another process's, and a
  read-only jsonl store does not create its root directory either. Two
  things it is not: a read-only sqlite store still creates or migrates
  the database's tables when it opens the file, which is what makes a
  database an earlier release wrote readable, and a session either
  store has opened is cached, so `Release` and a second `Open` are how
  a reader sees what has been appended since (#44).
- The sqlite store parses a holder's `since` and `heartbeat` before it
  decides what to do with the row, so the refusal an operator sees
  names when the holder took the session and when it last appended
  rather than year one, which read like a broken lock and invited
  breaking a live one (#43).

- The config entry records the instructions as parts, which is the
  round's one format change. `instructions_parts` is an ordered list
  of `{id, text, source}`, one per layer that writes the prompt: a
  delta carries the whole ordered list with the text of the parts that
  changed and `{id, hash}` for the parts that did not, and a part the
  list leaves out is removed. `instructions` remains valid and, where
  both are present, is the parts' texts joined with one blank line, a
  rule the RFC states so a writer and a reader agree; `Settings`
  derives it, so nothing downstream of the settings changes and the
  request hash is untouched. `Settings.InstructionsDelta` builds the
  delta from the parts in force and returns nil when nothing moved;
  `ConfigFromRequestParts` builds the full config on a root.
  `instructions_omitted` beside it records the parts a writer
  considered and left out, with a reason and a size, which is where
  `agentsmd.Result.Omitted` and a memory manifest go, and
  `Context.InstructionsOmitted` reads the last of them on a path. A 13
  byte edit to one of four parts now costs that part rather than the
  whole prompt: the composed study's 2,787 byte delta, and the memory
  study's 33,440 byte one, become the part that changed plus an id and
  a hash for each part that did not. A part named by a hash keeps the
  source it had, so a writer that clears or changes a part's source
  writes its text with it; a `Settings` never shares its parts with
  another or with the entry they came from; and where a part cannot be
  resolved, an `instructions` string written beside it stands, which a
  replacing delta must carry (#27).

- New record entry `queued`: the input a harness accepted before it
  could append it, a steer that joins the run in flight or a follow-up
  that waits for it, with the `trigger` that brought it in. The
  context algorithm ignores it, and the item entry that drains it
  carries `source`, the same trigger, and `queued_from` naming the
  queued entry, so two people steering one run are told apart and the
  record says why an item is there. `QueuedEntry`, `NewQueued`,
  `WithTrigger` and `Drain` write it; `Session.PendingQueued` and
  `Queued` list the inputs a harness still owes the conversation,
  which is the durable inbox a gateway that answers 202 drains on
  resume, and a run end closes one. The header's `records` may promise
  `queued`. `ItemEntry.Source` also stands alone, for any item a
  person or another system sent (#42).

- `export.Options.Cost` is asked once per model call rather than once
  for the step and again for the totals, so a price source that counts
  or charges for what it is asked is not double counted.
- The ATIF export tells what a run cost from what a document shows.
  `final_metrics` now totals every model call on the path, including
  the ones a compaction folded out of the document, `total_steps`
  counts the steps the document holds plus the calls it does not
  show, and a line in `notes` says so, which is what ATIF asks of a
  `total_steps` that is not the number of steps. A run that folded
  seven times reported the cost of the three calls that survived
  (#36).
- **Breaking.** `extra.run` in an exported document is a list, on a
  step and at the root. A run that produces no step, which is what a
  refusal on resume is, had its record replaced by the next run's and
  vanished from the document; a list keeps every record and gives a
  reader an order where several land in one place (#37).
- `export.Options.ModelName` overrides the model name the document
  reports, in `agent.model_name` and on every agent step, without
  touching the model the request was sent with, for a consumer that
  derives a provider by splitting the name on a slash. Costs are still
  priced by the model that was sent (#38).
- `export.At(s, entryID)` builds the trajectory of the path that ends
  at any entry, not only at a leaf, with the same `PreferredOver`,
  `AbandonedAt` and `Main` treatment; the entry it ends at is not read
  as a fork, since what was appended below it is not a branch this
  path abandoned; `Trajectories` is it over each
  leaf. A judge that appends an outcome moves the leaf, and the
  document a score names can now be built again from the entry the
  score targets. `Trajectory` gains `Path`, the root-first path before
  compaction, which the totals are taken over (#39).

- `agentsession show` renders a custom entry as its namespace and the
  size of its data, and an extension entry as its type and size, in
  place of the "(unknown entry type)" that four policy verdicts used
  to print as four identical lines; `-v` prints the data itself. Every
  core entry type now has a case, which a test holds it to (#35).

## v0.0.5 - 2026-09-20

- RFC 0001 is revised to draft 0.2. The summary now defines a session as
  two kinds of entry, context and record, and the core types section
  states the test for admitting a core type. Three record entries are
  added: `run` (source and end reason), `dispatch` (a call handed to its
  tool) and `decision` (a call's fate, any rewritten arguments and,
  optionally, who decided it). Run end reasons and decision verdicts are
  defined as shapes of the path a reader can recompute, so they belong
  to the format rather than to one harness, and the reasons form a
  first-match cascade with `error` and `interrupted` as the two values a
  writer adds; how an input arrived and who decided a call are a
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
- The library implements draft 0.2 and writes `agentsession/0.2`; 0.1
  files read unchanged. New entry types `RunEntry`, `DispatchEntry` and
  `DecisionEntry` with their constants and constructors;
  `OutcomeEntry.Pass` and `OutcomeEval`; `EnvEntry.Workspace` and
  `SetWorkspace`; `Header.Records`, `Header.SpawnedBy`,
  `Header.HasRecord` and `AllRecords`; `SubsessionID`, the UUIDv5
  derivation the RFC recommends. `Calls`, `Session.Calls` and
  `Session.PendingCalls` collect each function call with its decisions,
  dispatch and output, and `Call.State` reads the header's records to
  say whether a missing dispatch means never started or unknown. `Runs`,
  `Session.Runs`, `Session.OpenRun` and `Session.EndRun` partition a
  path into run segments and build the end entry with its pending list;
  `ComputeReason` is the RFC's cascade and `Run.Verify` checks a written
  reason against it. `Session.VerifyRecords` checks run ends, forbids a
  dispatch after a reject and, when the header promises dispatches,
  requires one on every call that ran; `Session.Append` refuses a
  dispatch for a rejected call with `ErrCallRejected`. The jsonl store's
  `SyncOnResponse` now also syncs function call outputs and any record
  entry the header names, as the format requires. The ATIF export
  carries a run's source, trigger, reason, cause and pending calls under
  `run` on the first step of its segment and a call's decisions and
  dispatch under `calls` on the agent step that produced it.
  `agentsession verify` runs the record checks on every leaf, and `show`
  renders the new entries and header members.

- The ATIF export passes every entry's unknown members through to the
  document, as the store already did on a round trip; a config entry's
  ride under `config_extensions` (#29).
- `jsonl.WithStaleLockReport` tells a host when Create or Open takes
  over the lock of a process on this host that no longer runs, the only
  durable sign of a crash between two appends (#26).
- The leaf is durable: appending `Session.MarkLeaf`, a label entry
  carrying `LeafLabel`, keeps the leaf at its target and `Read` restores
  it from the last such label, so a branch survives a restart. A null
  label on the target clears it (#24).
- The sqlite store returns `ErrConcurrentWriter` when another process
  appended since it loaded a session, and refuses the session until
  `Release`, instead of reloading and re-parenting under a leaf the
  agent never saw (#18, the floor; a cross-process lock can follow).
- The ATIF export counts a compaction's or branch summary's usage under
  the step's `usage`, prices it under `cost_usd` when a price source is
  given, and adds both to the totals; a compacting agent no longer
  under-reports (#20).
- `atif.Validate` enforces Harbor's closed sets: four image media types,
  eight audio types with aliases normalised as Harbor normalises them
  (`atif.NormalizeAudioMediaType`), nine schema versions
  (`atif.SchemaVersions`), and the timestamp forms Harbor accepts, naive
  times and bare dates included. `atif.Parse` stays lenient on the
  schema version. The exporter emits one of the four image types or
  degrades the part to a text placeholder (#23).
- `export.NoPassthrough` strips the raw items from a document for a
  judge or a publication; `Items` then reports `ErrNoRawItems` (#22,
  first half).
- New nested module `otel`, the RFC's OpenTelemetry projection:
  `otel.Export` replays a session as spans with the entries' own
  timestamps, and `otel.Wrap` decorates a store so a live harness emits
  the same spans as it appends, resumes included. Session, run,
  inference and tool spans carry `session.id` and the entry ID; a tool
  span links the inference that produced it, an inference span links the
  previous turn's, and the first span after a branch links the branched-
  from entry when its span is known.

- `Continue` rolls a session over into a successor in the format's
  order: a new session with `parent_session`, a full config from the old
  leaf's settings, the summary, the display name, and a `continued_in`
  link on the old session. `Session.SupersededBy`,
  `Summary.SupersededBy` and `ListFilter.Current` let a listing show the
  successor rather than the session it retired, in every store;
  `agentsession list -current` does the same (#19).
- The sqlite store holds each open session in a `holders` table, so a
  second process gets `ErrSessionLocked` naming the holder instead of
  interleaving entries. A hold left by a dead process on this host is
  taken over and reported through `sqlite.WithStaleLockReport`;
  `LockHolder` and `BreakLock` mirror the jsonl store's; an append after
  a broken hold fails with `ErrSessionLocked` and refuses the session
  until `Release` (#18). `sqlite.Open` takes options.
- `export.ItemsFrom` rebuilds a conversation from a document's declared
  fields alone, so a document from any producer loads; its doc comment
  lists what is lost (#22).

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
