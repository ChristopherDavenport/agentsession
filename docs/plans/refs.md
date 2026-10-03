# Plan: refs

A ref is a name that points to a session, kept by the store, and moved
by compare-and-swap. This plan adds refs to every store and to RFC 0002.
It closes #129, reframed on 2026-10-02 from a keyed create to refs.
Written 2026-10-02, after the release wave that shipped format 0.11.

## Why

A harness names its conversations by something of its own: a channel,
a ticket, a user. The store has no place for that name. So each
harness keeps a map beside the store, and two harnesses on one root
race on it. #129 has the live evidence: two dexclaw daemons each
created a session for `handoff7`, and one's write of the map dropped
three of the other's keys. dexclaw now guards its map with a lock file
of its own (`host.UpdateKeys`).

#129 first asked for a session created *by* a name. What it needs is a
name that *points to* a session. The stack has two kinds of name, and
only one of them has a home:

- **A session's name,** the `info` entry `Summary.Name` reads, is
  record content: immutable, hashed, and it travels with the session.
- **A ref** is mutable store state: which session is current for
  something outside the stack. It isn't part of any session, and it
  moves when the conversation does.

RFC 0002 already has the concept one level down. "The head is a
session's resume point, and it is a ref", moved by compare-and-swap.
Named refs apply the same rule to the store: a session's head names an
entry, and a store's ref names a session.

The same primitive is what remote stores and containerized agents need:
- a push moves refs as it moves heads;
- an agent scheduled for "conversation X" resolves the ref and resumes;
- a client follows the ref and moves with the conversation.

## The rules (RFC 0002, new section "Refs", after "Head")

- **A ref** is a name and a target. The name is slash-separated
  segments of `[A-Za-z0-9._-]`. No segment is empty, `.` or `..`. No
  name is a prefix segment of another: `a/b` and `a/b/c` can't both
  exist, as git refuses a ref under a ref, so a ref store can be a
  tree. The target is a session ID, and optionally an entry in it
  that the ref pins.
- **A store MUST offer compare-and-swap on a ref:** set it from an
  expected target to a new one, or fail and report the target it
  holds. "No ref" is a valid expected value, which makes
  create-if-absent. "No ref" is also a valid new value, which deletes.
  There is no other way to write a ref.
- **A ref names a session the store holds when it is set.** Deleting a
  session doesn't delete its refs: a ref to a missing session resolves
  to `ErrNoSession`, as a dangling name, never to another session
  created later under that ID.
- **The ref log** records every update the store accepted, in order:
  name, old and new target, time, and an optional reason. It is what
  says where a conversation has lived. A store keeps it at least as
  long as the refs. Pruning it is the store's policy, as with the
  trash.
- **Refs and holds are separate.** A ref says *which* session is
  current; a hold or lease says *who* may write it. Setting a ref takes
  no hold on the session, and holding a session gives no claim on a
  ref.
- **Refs and continuation.** `Continue` retires a session into a
  successor with a `continued_in` link. A ref doesn't follow that link
  by itself. The writer that continues moves the ref, by
  compare-and-swap from the old session to the new. See the open
  questions for whether resolution should also walk the chain.
- **Exchange.** A push carries the refs the sender names for the
  session, and the receiver moves each by compare-and-swap after the
  closure is admitted, with the same two steps and the same failure as
  the head. Refs never merge. Two stores that disagree on a ref
  disagree until one moves it.

These are store rules. RFC 0001, the file format, doesn't change. A
projection to JSONL carries no refs.

## The API

```go
// RefStore is implemented by a store that keeps refs.
type RefStore interface {
	// ResolveRef returns the ref's target, or ErrNoRef.
	ResolveRef(ctx context.Context, name string) (RefTarget, error)
	// UpdateRef moves the ref from expected to next, the zero
	// RefTarget meaning none. On a mismatch it returns ErrRefMoved
	// wrapped with the target it holds (RefMovedError.Current).
	UpdateRef(ctx context.Context, name string, expected, next RefTarget, reason string) error
	// ListRefs lists the refs under prefix, in name order.
	ListRefs(ctx context.Context, prefix string) iter.Seq2[Ref, error]
	// RefLog lists the ref's updates, newest first.
	RefLog(ctx context.Context, name string) iter.Seq2[RefUpdate, error]
}

type RefTarget struct {
	Session string
	Entry   string // optional: the entry the ref pins
}
```

The helper for #129's race is plain Go, with no store support:

```go
// SessionFor returns the session ref names, creating one from h and
// setting the ref when there is none. Two callers racing both create;
// one sets the ref, and the other opens the winner's session and
// deletes its own, which nothing else has seen.
func SessionFor(ctx context.Context, st Store, name string, h Header) (*Session, error)
```

There's also `ContinueRef(ctx, st, name, …)`, which runs `Continue`
and then moves the ref from the old session to the successor.

## Per store

- **cas:**
  - `refs/<name>` is a file holding the target, written to `tmp/` and
    renamed into place.
  - A compare-and-swap is taken under `locks/refs/<name>` (flock, as a
    session's hold is): read, compare, rename, then append to the log.
  - The log is `logs/refs/<name>`, per ref as git keeps it, in the
    checksummed record form the session log uses.
  - Recovery: a log record without its rename is completed on the next
    update under the lock, and a rename without its record is logged.
    The lock makes the pair atomic to every reader that takes it.
  - `Sweep` treats a ref's target as reachable, as it treats a head.
- **sqlite:** `refs(name PRIMARY KEY, session, entry, updated_at)` and
  `ref_log`, with compare-and-swap as an `UPDATE … WHERE` in one IMMEDIATE
  transaction with the log row.
- **jsonl:** `<root>/refs.json`, written whole by tmp and rename under
  `refs.json.lock`, with `refs.log` appended under the same lock. A jsonl
  root is one machine, and refs are few.
- **memory:** a map and a slice under the store's lock.
- **Exchange (cas `Push` and `Fetch`):** they carry the refs a caller
  names, and apply them after the closure as the head is applied.

## Tests

- **storetest `Refs` group:**
  - create-if-absent;
  - compare-and-swap with a stale expected value reports the holder;
  - delete by compare-and-swap;
  - name rules, including the ref-under-a-ref refusal;
  - a ref to a deleted session resolves to `ErrNoSession`;
  - `ListRefs` by prefix and in order;
  - the ref log's order and contents;
  - a second store value on the same root sees updates;
  - `SessionFor` under a race of N goroutines, and N processes for cas
    and sqlite, ends with one session and one ref, the losers' sessions
    gone.
- **cas:** a crash between the rename and the log record, and between
  the record and the rename; `Sweep` keeps a ref's target; and #129's
  scenario as a test: two stores on one root, one name, one session.
- **Exchange:** a pushed ref moves on the receiver by compare-and-swap,
  and a receiver whose ref moved refuses it and keeps the closure.

## Order of work

1. The RFC 0002 section, for review before code. It is the
   cross-language contract.
2. The API, memory store, storetest, `SessionFor` and `ContinueRef`.
3. cas, then sqlite, then jsonl.
4. Exchange carries refs.
5. CLI: `agentsession refs [prefix]`, `agentsession ref <name>` (with
   `--log`), and `show` accepting `ref:<name>` where it takes a session.
6. dexclaw drops `host.UpdateKeys` for `SessionFor` (agentstudies).
7. CHANGELOG, and the PR with `Fixes #129`.

## Later, not in this plan

- **Following a ref:** a `Follow` on a ref that yields the session it
  points to, and each move. It wants `Follow` (#194) released, and a
  change kind of its own.
- **Refs over a wire,** in RFC 0003, the store protocol: list by
  prefix, resolve, compare-and-swap, and the log.
- **Signed ref updates,** with signed heads, which RFC 0002 defers.

## Open questions

- **Should resolution walk `continued_in`?** If a writer continues a
  session and crashes before moving the ref, the ref points at a
  retired session. `ResolveRef` could follow the chain to its end and
  report that it did. The alternative is to leave the ref as written
  and let `ContinueRef` be the only way to continue a named session.
  The plan leaves `ResolveRef` literal and adds `ResolveCurrent`, which
  walks the chain, so the choice is the caller's.
- **Should a ref be able to pin an entry?** It's useful for a fork base
  or an evaluation baseline (a tag, in git's terms). It costs a second
  field in every ref and a rule that the entry is in the session. The
  plan includes it, since adding a field later would change the cas
  file format and the sqlite schema.
- **Name length and case.** File-based stores on case-insensitive file
  systems fold `A` and `a`. The plan refuses names that differ only in
  case, so every store behaves alike.
