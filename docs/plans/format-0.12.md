# Plan: format 0.12

What the next minor of the Agent Session Format carries, the work it
asks of agentturn and its ACP front, and the order it lands in. Written
2026-10-02, while agentturn's `front/acp` (agentturn#217) brought ACP v2
steering, where a person can take back a message they queued. RFC 0001 is
normative and the format item below is a change to it first. This is a
plan: nothing here is built yet.

## Why a minor

A queued input can leave the queue three ways today. An `item` entry
drains it and names it in `queued_from`; a `run` end after it closes it;
or nothing happens and it is owed, a durable inbox a resume drains. The
second is how a host declines an input, and it says only that the run
ended first. It cannot say that someone took the input back.

ACP v2 makes that a thing people do. A prompt sent while the agent works
is steered into the run, and is accepted only once it is inserted. Press
Esc before the run takes it, or withdraw it from the client, and the
message must not reach the model. agentturn has no way to do this, so
`front/acp` in #217 keeps such a message and starts the next turn with
it, since `Agent.Abort` leaves the queues for the next run.

The record must not be edited to make the message disappear. A
withdrawal is a command into the loop that targets an earlier entry, and
it is written as one: an entry after the `queued` entry, saying who took
the input back and why. The `queued` entry stays as written. A new
record type is a minor: a 0.11 reader refuses a file holding it.

## What 0.12 carries

### 1. `withdraw`, a queued input taken back

RFC: a new `### withdraw` section after `### queued`, `withdraw` in the
list of record entry types under `## Core entry types`, and in the
conformance list.

```json
{"type":"withdraw","id":"…","parent":"…","ts":"…",
 "target":"<queued entry id>","by":"human","reason":"cancelled"}
```

- `target` is required: the `queued` entry whose input is taken back. It
  names an entry by `id`, as `label`'s `target` does.
- `by` is required, with `decision`'s values: `human`, `policy` or
  `agent`, so an auditor reads who withdrew it as they read who refused
  a call.
- `reason` is optional, in the harness's own terms: `cancelled` for a
  turn the person stopped, `withdrawn` for a message taken back alone.
- The entry is a record entry and contributes nothing to context. The
  input never entered it, and this entry says it never will.
- A `queued` entry with a `withdraw` after it on the path naming it is
  closed. It is not owed, and no `item` entry after the `withdraw` may
  name it in `queued_from`.
- A writer writes `withdraw` only for a `queued` entry on the path that
  no `item` names in `queued_from` and that no `run` end after it has
  closed. An input the recorder wrote again after a run end, because
  the agent still holds it, is withdrawn by naming the newest `queued`
  entry for it, the one that is open.
- A writer that names `withdraw` in the header's `records` writes one
  for every input it takes back, so a reader may take the absence of
  one after a closed `queued` entry as the run end having closed it.

Reader:

- `Queued(path)` and `Session.PendingQueued` leave out a `queued` entry
  a `withdraw` names.
- `VerifyRecords` fails a `withdraw` whose `target` is not a `queued`
  entry on its path, or names one an `item` already drained or a `run`
  end already closed, and fails an `item` whose `queued_from` names a
  withdrawn entry.
- `WithdrawEntry` and `NewWithdraw(target, by, reason)` beside
  `QueuedEntry` and `NewQueued`, and `TypeWithdraw` in the record types
  `AllRecords` lists.
- Fixtures: a positive native fixture where an input is queued and
  withdrawn, and the next run drains another input; negative fixtures
  for a `withdraw` of a drained input, of an entry that is not
  `queued`, and an `item` draining a withdrawn input.
- Exports: ATIF and any other export leave a withdrawn input out of the
  trajectory, as they leave out a closed one, and may list it as a
  record.

### Writer

`FormatMinor` to 12. The raise on append is already in place, so a
recorder that writes a `withdraw` raises the file it appends to.

## The work in agentturn

### 2. `Agent.Withdraw` and the `withdrawn` event (root module)

```go
// Withdraw takes items out of the queues before any run appends them
// and returns the ones it took back.
func (a *Agent) Withdraw(ctx context.Context, items ...openresponses.Item) openresponses.Items
```

- An item is matched by identity, as `item_end` delivers the item it
  was given. An item already drained, or never queued, is not taken
  back and is not in the result. That is the race a front loses when
  the run takes the item first, and the front answers the prompt as
  inserted.
- Who withdrew it and why ride on the context, as a queued input's
  trigger does: a `Withdrawal{By, Reason}` set with
  `ContextWithWithdrawal`, validated where it enters, as `Trigger` is.
- Each item taken back is reported as a `withdrawn` event (`Withdrawn`:
  the item, its queue mode, the hidden mark, by and reason). It belongs
  to no run and is delivered the way `queued` is, by the goroutine that
  owns delivery at its next event. An item withdrawn before its `queued`
  report went out is reported `queued` and then `withdrawn`, in that
  order, so the record says the input was accepted and then taken back
  rather than nothing.
- `State` stops counting withdrawn items, and `Continue` and `Deliver`
  stop seeing them.

### 3. The recorder (`agentturn/session`)

- On `withdrawn`, the recorder writes a `withdraw` entry naming the
  `queued` entry it holds for the input in its inbox, with the event's
  `by` and `reason`, and forgets the input. The inbox already maps each
  held input to its open `queued` entry.
- `Recorder.Requeue` and `Resume` hand back only inputs that are still
  owed, which `PendingQueued` now computes without withdrawn ones.
- The header's `records` names `withdraw` beside `queued`.
- Requires agentsession 0.12.

### 4. `front/acp`

- v2 `session/cancel`: before the turn reports idle, the front
  withdraws every message whose prompt is still waiting for insertion,
  by `human` with reason `cancelled`, and answers each of those prompts
  with an error, which v2 asks of a request rejected before insertion.
  A message the run took first is answered as inserted. This replaces
  #217's behaviour of starting the next turn with what is still queued.
- `_agentturn/withdraw`, an extension method taking `sessionId` and
  `messageId`, withdraws one waiting message without stopping the turn,
  by `human` with reason `withdrawn`. It answers `{"withdrawn": true}`,
  or `false` when the message was already inserted, and the withdrawn
  prompt is answered with an error. The method is advertised under
  `_meta` in the initialize response, so a client knows to offer it.
- v1 has no queue: a prompt waits for its turn, so neither applies.

## Rollout

1. agentsession: the RFC (`### withdraw`, `## Changes since 0.11`, the
   conformance list, the header example and `Format` to `0.12`), the
   reader, the fixtures and the exports. Release.
2. agentturn: `Withdraw` and `withdrawn` in the root module, the
   recorder in `agentturn/session` on agentsession 0.12, and the
   `front/acp` wiring, in one release so every module's version moves
   together.
3. Siblings: the usual bump PRs where a module records sessions or
   builds on agentturn's events; agentkit's manual test names any new
   `Config` field, and this plan adds none.
4. examples/: probes that queue and cancel (oh-my-pi's Esc on a queued
   steer, the codex permissions study) flip the checks the release
   answers, with a dated update under each finding.

#217 merges before this, as it stands. A message it keeps after a cancel
is already recorded without an edit: a `queued` entry, a run end that
closes it, the `queued` entry written again, and an `item` that drains
it in the next turn. Withdrawal changes what the front does, and adds an
entry; it never rewrites one.

## Open questions

- Should a `withdraw` also be able to name an input the harness never
  wrote as `queued`, one dropped before its report went out? This plan
  says no: the agent reports it `queued` first, so every `withdraw` has
  a target.
- `reason` is free text here. If a reader needs to tell a cancel from a
  single withdrawal, a closed `cause` beside it, as `run` ends have,
  may be worth adding before the RFC text is final.
- Follow-ups: the same entry withdraws a `followup`, and agentturn's
  `Withdraw` covers both queues, but no front offers it yet.
