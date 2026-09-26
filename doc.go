// Package agentsession implements the Agent Session Format: an
// append-only, tree-structured JSONL record of one agent session whose
// conversation payloads are Open Responses items. The format is
// specified in docs/rfcs/0001-agent-session-format.md; this package is
// its reference implementation.
//
// # Shape
//
// A session file is a header line followed by entries, one JSON object
// per line. Entries form a tree through their id and parent members,
// so a branch is a child of an earlier entry, in place, and an
// abandoned branch stays in the file. An entry may also name further
// predecessors in [EntryBase.Parents] — a subagent's result, a branch
// merged back — which record where converged work came from and are
// never walked when building a context; on a root entry the same member
// records the point in another session this one was forked from, whose
// path the session opens with. [Header] and [Entry] are the two
// line shapes; the concrete entry types are [ItemEntry] (one Open
// Responses item, verbatim), [ResponseEntry] (the envelope of one model
// call), [ConfigEntry] (a delta to request settings), [CompactionEntry]
// and [BranchSummaryEntry] (summaries that enter context), and the
// out-of-context [LabelEntry], [InfoEntry], [EnvEntry], [OutcomeEntry],
// [LinkEntry] and [CustomEntry]. Namespaced types decode to
// [UnknownEntry] and are written back byte for byte, as are namespaced
// items inside an item entry.
//
// # Sessions
//
// [Session] is the tree in memory: [Read] loads one, [Write] emits one,
// and [Session.Append] adds an entry as a child of the current leaf.
// [Session.Branch] moves the leaf so the next append forks; a fresh root
// follows [Session.ResetLeaf]. A file cut short by a crash still loads,
// and [Session.Truncated] reports the broken line.
//
// # Context
//
// [Session.ContextAt] runs the format's context algorithm: it walks the
// path to an entry, replays config entries into [Settings], applies the
// last compaction and returns the item list a model call there would
// receive. [Settings.Request] turns that into the canonical
// openresponses.Request, and [RequestHash] hashes it as the format
// defines (RFC 8785 canonical JSON, SHA-256). [Session.Verify] checks a
// stored response's hash against the rebuilt request, and reports
// [ErrNoHash] rather than nil when the response recorded none.
//
// # Stores
//
// [Store] is the persistence interface; [MemoryStore] is the in-memory
// one and the jsonl subpackage is the file store. The storetest
// subpackage is the conformance suite every store runs.
//
// # Export
//
// The export subpackage turns a session's root-to-leaf paths into ATIF
// documents, using the types in the atif subpackage, with the raw items
// carried in the extras so nothing is lost.
package agentsession
