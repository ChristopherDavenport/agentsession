# Plan: following a session as it is written

A store can be read today, not watched. This plan adds `Follow`: a
reader subscribes to a session and receives each entry as the store
accepts it, beside a writer in this process or in another. Written
2026-10-02, after the release wave that shipped format 0.11, for a
terminal client that renders an agent's conversation from its record
rather than from the loop's events alone.

## Why

A client that shows what an agent did has two sources. The loop's
events (`agentturn.Agent.Subscribe`) are live but ephemeral: text
deltas, tool progress, the state of a turn. The session is durable,
hashed and branched: what happened, verifiably. The rule a client wants
is that the record is the truth for everything committed, and the
events cover only what is not committed yet. The recorder already draws
that line: `item_start` and `item_update` write nothing, and an item
becomes an entry at `item_end`.

The rule needs a way to see entries as they land, and there is none.
`Reader.Read` returns a snapshot that no later append reaches. A
session `Open` holds is live, but it is the writer's, offers nothing to
wait on, and holds the session against every other process. The only
way to see more is to read again. Every `Read` rebuilds the whole
session: jsonl reads the file, cas rebuilds from the log and objects,
sqlite selects every entry. No call starts after an entry, and an entry
ID is a hash, not a position.

The same gap shows elsewhere. dexclaw would watch a session from its
status endpoint. An evaluation watches runs in progress. An operator
wants `agentsession show -f`. Nothing in the issue tracker asks for a
tail yet. The nearest issues are #107 (read-only open), #174 (Reader)
and #113 (exchange).

## What exists to build on

- **One order per session.** RFC 0002 makes the log the order. Each
  store has one holder per session and assigns a sequence at append:
  the line order in jsonl, the log record's `Seq` in cas, and
  `entries.seq` in sqlite. A follower reads that order and never orders
  by `ts`.
- **Visibility before durability.** In every store an append becomes
  visible to another process before it is durable:
  - jsonl, when the line's `write(2)` returns;
  - cas, when its log record is written, since the objects go first;
  - sqlite, at COMMIT.

  `Reader` already says that a read can show an append a crash takes
  back. A follower inherits that.
- **Incremental reads half exist.**
  - jsonl is append-only `O_APPEND`, so a reader can resume at a byte
    offset.
  - cas has `readSessionLog(dir, from)`, which reads from an offset.
  - sqlite can select `seq > ?`.

  Nothing uses any of these for a tail yet.
- **The places a log is replaced rather than appended to.** A follower
  has to notice each of these:
  - jsonl's `raiseFormat` rewrites the file by tmp and rename on the
    first append to an older minor;
  - cas's `writeRecovered` replaces the log when a writer's open
    recovers a crash;
  - `Repair` and `migrate` write new logs;
  - `appendRecords` truncates the log back after a failed write or
    fsync, so an entry a follower has seen can vanish;
  - `Delete` moves the session to `trash/`, and its ID can be created
    again.

## The API

```go
// Follower is implemented by a store that can follow a session: read
// it, then receive each change the store accepts, without holding it.
type Follower interface {
	Follow(ctx context.Context, id string, from Cursor) iter.Seq2[Change, error]
}

// Cursor is a position in one session's log, opaque and the store's
// own. The zero Cursor is the start: Follow begins with a snapshot.
type Cursor string

type ChangeKind int

const (
	Snapshot ChangeKind = iota // Session is the session as read
	Appended                   // Entry was appended; Session includes it
	Head                       // the store recorded Leaf as the head
	Reset                      // the log was replaced; Session is re-read
)

type Change struct {
	Kind    ChangeKind
	Session *Session // the follower's session after this change
	ID      string   // Appended: the entry's ID
	Entry   Entry    // Appended: the entry
	Leaf    string   // Head: the leaf recorded
	Cursor  Cursor   // the position after this change, to resume from
}
```

The follower keeps a session of its own and extends it with each
append, so `Path`, `ContextAt` and `Verify` work on it as on any read
session. That session is the follower's alone. It is valid until the
next step, and a caller never appends to it. Keeping the session
in-process also means a consumer that only wants the entries can ignore
it at no extra I/O.

## Semantics

1. **Start.** With the zero Cursor, the first change is `Snapshot`:
   the session as `Read` would return it, plus the cursor at its end.
   The snapshot and the tail come from one read, so no append falls
   between them. With a cursor, the follower resumes after it. A cursor
   that no longer names a place in the current log, because the log was
   replaced since, gives `Reset`. It is never a silent skip.
2. **Appends** come in log order: every branch, in the order the store
   accepted them, and not the order of a path. A fork's prefix is in
   the snapshot. Its appends are its own entries.
3. **Head.** When the store records a leaf (a leaf label in jsonl or
   sqlite, a head record in cas), the follower yields `Head`. A leaf a
   writer moved with `Session.Branch` and did not record is not seen,
   as with `Read`.
4. **Reset.** When the log is replaced, the follower reads the session
   again and yields `Reset` with it. A consumer drops what it derived
   and starts from that session. A `Reset` may lack an entry that was
   yielded before it: the truncation after a failed append takes back
   an append its writer saw fail. Everything a follower yields is
   visible, never promised durable, as `Reader` says already.
5. **End.** The iterator ends without an error when ctx is done. A
   deleted session ends it with `ErrNoSession`. A session created again
   under the same ID is another session, and the follower never
   continues into it.
6. **Partial writes.** A follower consumes only complete records:
   - jsonl, up to the last newline;
   - cas, records whose checksum holds and whose objects resolve, with
     the retry `Read` already does for an object a pack moved;
   - sqlite, committed rows.

   An incomplete tail means wait, never an error.
7. **No hold.** Following writes nothing and takes no hold, recovery
   included, like `Read`. A store opened read-only can follow.

## Waking up

- **Same store, same process.** When the follower and the writer share
  a store value, `Append` wakes the follower directly. The store keeps
  a broadcast per followed session, a channel closed and replaced on
  each append and head record. There's no polling and no I/O beyond the
  entry.
- **Another process or another store value.** The follower polls a
  cheap signal and reads only what is new. The root module takes no
  dependency, so the poll is a `stat`, not fsnotify.
  - jsonl: size and inode. Growth reads from the offset. A new inode
    or a shrink is a `Reset`.
  - cas: the size and identity of `sessions/<id>/log`. Growth goes
    through `readSessionLog(from)`. A replaced or shorter log is a
    `Reset`. A missing directory is a deletion.
  - sqlite: `PRAGMA data_version`, then `seq > ?`. A seq below the
    cursor's, which only a rebuild makes, is a `Reset`.
  - memory: in-process only; there is no other process.

  The interval is an option, `WithFollowInterval`, defaulting to 100 ms
  and backing off to 1 s while nothing changes. A same-process follower
  ignores it.

The cursor encodes what each store needs to resume and to detect a
replacement:
- jsonl: the byte offset, the inode and the header's hash;
- cas: the log offset, the log's identity and the last `Seq`;
- sqlite: the last seq and the session's creation stamp.

## RFC 0002

The API is the library's, but what a follower may assume of the files
is the store format's. A follower in another language will tail these
files too. RFC 0002 gets a short section, "Following a log", under
Ordering. It says three things:
- a reader that tails a log consumes complete records only;
- a log is append-only except for the named replacements, each of which
  changes the log's identity or shortens it;
- what a follower sees is the visible order, which a crash or a failed
  append can take back.

This adds no new wire shape, and no format minor.

## Order of work

1. **API, memory store, conformance tests.** Add `Follower`, `Cursor`
   and `Change`, the memory store's implementation, and `storetest`
   cases every store runs:
   - snapshot, then appends;
   - a branch, then a head;
   - a follower on a second, read-only store value;
   - resume from a cursor;
   - resume from a cursor the log no longer holds;
   - delete;
   - cancel;
   - a reader that stops mid-iteration.
2. **jsonl:** the offset tail, the reset on `raiseFormat` (a 0.10 file
   appended to by a 0.11 writer), and the partial line.
3. **cas:** the log tail, and a reset on each of recovery, `Repair`,
   `migrate` and the truncation after a failed append. Inject the
   failure as the store's tests already do. Also: a follower across
   `Pack` and `Sweep`, which move objects and not the log.
4. **sqlite:** `data_version` and `seq > ?`.
5. **CLI:** `agentsession show -f <session>` prints each change as it
   lands. It is the tool an operator wanted, and it tests the API from
   outside.
6. **RFC 0002 section,** and a CHANGELOG entry. File an issue for the
   feature, which the PR then closes.

## Not in this plan

- **Following only what is durable.** Another process cannot see an
  fsync, so a durable-only follow would mean a durable marker in the
  log. If a consumer needs one, that's a format question for later.
- **Following every session in a store**, a store-wide feed. `List`
  and a follower per session cover the first consumers. A feed would
  want an index the stores don't keep.
- **Following across exchange.** `Fetch` and `Push` mirror between
  stores. A follower watches one store.
- **A wire.** Serving a follow over a network belongs to the client
  that needs it. The record half of that wire is already JSONL.

## Open questions

- **Should `Change.Session` stay?** It costs an in-memory session per
  follower. A consumer that renders a path needs one anyway, and it
  keeps `ContextAt` usable without a second read, so the plan keeps it.
- **Should a `Head` change carry the path it names?** A consumer can
  compute it from `Session`, so the plan leaves it out until a consumer
  shows otherwise.
- **Should a cas follower read through packs?** cas reads objects after
  a sweep has packed them, and the follower uses the same path, so it
  reads through packs either way. The open question is whether that
  makes it too slow for a long session's snapshot. That needs measuring
  in step 3.
