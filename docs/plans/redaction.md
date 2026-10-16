# Plan: redaction

A redacted entry is an entry a redactor made from entries of another
session, its original; a redacted session is the session that holds
them, linked back to its original. It is a session like any other: a
store holds it beside the original, it grows as the redactor redacts
what the original appends, it may be resumed, and it leaves the way
any session does, by push or projection. This plan covers what
agentsession owes that model, at format 0.13.

Written 2026-10-16. #237, #238 and #240 landed the first model, sealed
snapshots of a whole session; the same day it was revised to markers
on each entry, before any release. Closes #231–#236 and the library
half of #12.

The rules are in the RFCs: RFC 0001's Redaction section, the `redacted`
and `redacted_from` header rows and Changes since 0.12; RFC 0002's
Redacted sessions section and its import rule; RFC 0003's
`not_redacted` and `format` errors.

## What is here and what is not

Redaction is writing a session, so a writer outside this library does
it with the exported API and the helpers here. What this library owes
is the format, the store rules, and helpers that keep a writer from
re-deriving format knowledge. The redactors, the store-wide pass,
scanning, review and publishing belong to agentredact.

## Decisions

- **Redaction is marked per entry.** Every entry a redactor writes
  carries `redacted`, a body member any entry may carry, as
  `legacy_id` is: the positions, in the original's file order, of the
  entries it was made from. Each element is `n` or an inclusive range
  `[from, to]`; `[]` marks an entry the redaction added. No other entry
  carries it, so a reader always knows which entries a redaction made.
- **Any kind of redaction.** One entry per entry carries `[n]`; a
  placeholder for a block left out carries `[[from, to]]` and says in
  its body that a block was redacted; a merge carries several; a split
  names one entry twice. The format fixes validity and linkage, not
  method, and a redaction names itself in `redacted_from.profile`.
- **Positions, not hashes.** A position names an entry of the original
  for good, since sessions are append-only, and says nothing of its
  content, so it cannot confirm a guessed value. `request_hash` stays
  the original's on a redacted entry, or is removed; the confirmation
  risk of kept hashes is stated in the RFC and SECURITY.md.
- **A redacted session grows and may be resumed.** The redactor appends
  what it redacts as the original grows; another writer may resume the
  session, and its entries carry no `redacted`. There is no sealing, no
  refusal to create or append, and no snapshot per version.
- **`redacted_from` is `{session, profile}`** plus namespaced members.
  The version a copy holds is in its entries' positions.
- **Derived IDs**: UUIDv5 over the canonical JSON name
  `["redacted","<original>","<profile>"]`, so one original under one
  profile has one redacted session, and a store-wide pass resumes it.
  An array cannot be read two ways when an ID holds a `/`, and is not a
  subsession's name.
- **References into a held redaction are re-pointed.** A redaction of
  a whole store re-points references inside it to the redacted
  sessions; references to sessions not redacted alongside stay as
  written. A fork's redaction may be a fork of the origin's redaction,
  sharing its prefix, whose positions then count in the origin.
- **Redacted-only stores check entries.** A store set that way refuses
  a session whose header lacks `redacted` and every entry, by any path,
  that carries no `redacted`: a fork's own entries and a resumed
  session's native entries alike. It is how a publishing store fails
  closed.
- **Stores hold them.** RFC 0002's old import refusal, from when a
  redacted file did not verify, is gone; every store has an import.
- **No private record in the original.** Which sessions were made from
  an original is a store query over `redacted_from`.
- **Format 0.13.** `redacted` and `redacted_from` are new members, and
  before 0.13 both are unknown whatever their form; a writer does not
  raise a file holding either.

## Work, in order

1. **RFCs and plan**: this revision.
2. **Format and helpers**, on main's #238 and #240:
   `Header.RedactedFrom` as `{session, profile}` plus extras; the entry
   body member `redacted` with its forms and the minor gate;
   `RedactedSessionID(original, profile)`; no raise of a file whose
   entries hold `redacted`; `redacted` protected from `RewriteStrings`;
   a helper that sets an entry's positions.
3. **Stores** (#242): imports everywhere; redacted sessions created,
   appended to and exchanged as any; redacted-only stores refuse
   headers without `redacted` and entries without it by every path;
   `Original` and `Redactions` over positions.
4. **verify and export** (#243): verify labels kept `request_hash`
   mismatches on redacted entries as expected and holds native entries
   to every rule; ATIF carries `redacted` per step.

Main is not released between the first model and this one. One
release carries all of it. Siblings move to 0.13 through the usual
bump.

## Open questions

- Should `RewriteStrings` report what it changed (member paths, not
  values), so a publisher can review a redaction without the old
  values?
- Should RFC 0003 serve the `Redactions` query (#241 covers import)?
