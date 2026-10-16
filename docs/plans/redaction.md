# Plan: redaction

A redacted session is a new session made from an existing one by
changing bodies after they were written, linked back to its original. A
store holds it beside the original, and it leaves the way any session
does, by push or projection. This plan covers what agentsession owes
that model, at format 0.13. Written 2026-10-16; closes #231–#236 and
the library half of #12.

The rules are in the RFCs: RFC 0001's Redaction section and the
`redacted` and `redacted_from` header rows, RFC 0002's Redacted sessions
section and its import rule, RFC 0003's `redacted` and `not_redacted`
errors. This plan records why they are shaped as they are and the order
the code goes in.

## What is here and what is not

Redaction is writing a session, so a writer outside this library can do
it with the exported API: `MarshalEntry`, a walk over the canonical JSON
(which holds the members kept as written), `UnmarshalEntry`, then
`Commit` with the `id` cleared and the new `parent` set. A kept line is
written only while the fields still encode what was read
(`marshalEntry`'s fingerprint), so a changed entry never writes the
original's bytes.

What this library owes is the format, the store rules, and helpers that
keep a writer from re-deriving format knowledge. The redactors, the
store-wide pass, scanning, review and publishing are a separate tool's.
They need dependencies and network access the root module cannot take,
and policy the format has no reason to own.

## Decisions

- **A redacted session is a session of its own** (#231): a new `id` and
  `redacted_from: {session, entries}`. Sharing the original's ID put two
  sessions under one ID in any store holding both.
- **The format fixes validity, not method.** Other kinds of redaction
  will exist: leaving out a tool output or a branch, replacing an entry
  with a placeholder of another type, removing `request_hash`. RFC 0001
  requires only what makes a redacted session valid and linked, and a
  redaction names itself in `redacted_from.profile` and says more in
  namespaced members there, which `Header.RedactedFrom` preserves.
- **Aligned redaction is one kind, declared.** One entry for each of
  the original's in file order, only bodies changed, marked
  `aligned: true`; a holder of the original then maps a redacted entry
  back by position, which is how feedback on a published copy reaches
  it. A redaction that is not aligned records its own mapping if it
  wants one.
- **A count, not a hash, pins the version.** Sessions are append-only,
  so `entries` pins what was redacted, and it gives no hash against
  which a guessed value can be confirmed. `request_hash` stays the
  original's, as RFC 0001 always said; the confirmation risk is stated
  in the RFC and SECURITY.md instead.
- **Derived IDs** make a store-wide redaction repeatable: UUIDv5 over
  the canonical JSON name `["redacted","<original>",<n>,"<profile>"]`.
  An array cannot be read two ways when an ID holds a `/`, and is not a
  subsession's `<parent>/<call_id>` name.
- **References into a held twin are re-pointed.** A redaction of a
  whole store holds every session's twin, so references inside it name
  twins, and the original's hashes stay out of the published set.
  References to sessions not redacted alongside stay as written.
- **Written whole, never appended.** `redacted: true` says every body
  went through redaction. A store refuses to create one and refuses
  appends to one; it arrives by import or push. Continuing one is a
  fork, which is not redacted.
- **Stores hold them** (#233). RFC 0002's import refusal dates from
  `8e22534`, when a redacted file did not verify; it goes. A store may
  be set to hold only redacted sessions, which is how a publishing store
  fails closed.
- **No private record in the original.** Which sessions were made from
  an original is a store query over `redacted_from`.
- **A format minor, not an extension member.** `redacted_from` and the
  new-ID rule change what a 0.12 file may hold, which RFC 0001's
  versioning makes a new minor.

## Work, in order

1. **RFC and plan** (this branch): RFC 0001 0.13 (Redaction section,
   header rows, Changes since 0.12), RFC 0002, RFC 0003 errors,
   SECURITY.md.
2. **Format 0.13** (#231, #232): `Format` to 0.13 and the fixtures'
   headers; `Header.RedactedFrom`, refused without `Redacted`;
   `profile`, `aligned` and namespaced members preserved;
  `RedactedSessionID(original string, entries int, profile string)`
   beside `SubsessionID`. Check whether a pre-0.5 file can hold
   `omit.items` or `instructions_omitted[].of`, and if one can, add them
   to migration's switch (`codec.go:243`).
3. **Writer helpers** (#236): `RemapRefs` over every entry-naming
   member, migration moved onto it with its fixtures unchanged;
   `RewriteStrings` over every body string but the envelope,
   references, hashes and sidecar URLs, recomputing changed part hashes.
   Test: a string planted in every place an entry can hold one — typed
   field, `Unknown`, kept extra, extension entry, inline media — is gone
   from the written bytes, and the entry verifies.
4. **Stores** (#233): cas drops its import refusal (`cas/cas.go:3169`);
   every store refuses create of and appends to a redacted session and
   imports one; a redacted-only option; `Original` and `Redactions`;
   remote maps the two new errors. `storetest` cases every store runs.
5. **verify** (#234): a redacted file verifies against itself, not the
   original; name `redacted_from`; no `request_hash` mismatch reported
   as a fault.
6. **Export** (#235): `ToATIF` over a redacted session, `redacted_from`
   in the root `extra.agentsession`.

One release carries 2–6. Siblings move to 0.13 through the usual bump.

## Open questions

- Helpers beyond aligned redaction (#236 covers rewriting bodies and
  re-pointing references): leaving an entry out and re-parenting its
  children is the obvious next one, once a redactor needs it.
- Should `RewriteStrings` report what it changed (member paths, not
  values), so a publisher can review a redaction without the old
  values?
- Should RFC 0003 serve the `Redactions` query, or is it local only?
