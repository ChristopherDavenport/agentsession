package agentsession

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ChristopherDavenport/openresponses"
)

// ErrNoEntry is returned when an entry ID is not in the session.
var ErrNoEntry = errors.New("agentsession: no such entry")

// ErrBadConvergence is returned for an entry whose Parents break the
// format's rules: a reference naming no entry, the same entry named
// twice, a reference to the entry's own parent, or a reference into
// this session naming an entry that does not already exist.
var ErrBadConvergence = errors.New("agentsession: bad convergence reference")

// Session is one session in memory: the header, the entries in file
// order, the tree they form and the current leaf. It is safe for
// concurrent use. Entries are shared, not copied, and must not be
// modified after they are appended.
type Session struct {
	mu        sync.RWMutex
	header    Header
	entries   []Entry
	byID      map[string]Entry
	children  map[string][]string
	leaf      string
	truncated *TruncatedLine
	now       func() time.Time
	// prefix holds the IDs on the path to the header's base, when the
	// session has one: the entries another session wrote, carried so
	// the file stands alone. Own entries hang from the base or from
	// each other.
	prefix map[string]bool
	// repeated lists the IDs Read met a second time, each treated as
	// the same entry; unresolved lists the entries a migration could
	// not rewrite, which keeps a migrated file from being re-emitted.
	repeated   []string
	unresolved []string
	migrated   bool
}

// New creates an empty session. Header fields left empty are filled:
// a UUIDv7 ID, the current time, this package's format and payload.
// Times the session assigns are in UTC so a file carries one offset
// however the writer's clock is configured; times the caller supplies
// are kept as given.
func New(h Header) *Session {
	s := &Session{now: utcNow}
	h.fill(s.now())
	s.header = h
	s.byID = map[string]Entry{}
	s.children = map[string][]string{}
	return s
}

// Header returns a copy of the header.
func (s *Session) Header() Header {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.header
}

// ID returns the session ID.
func (s *Session) ID() string { return s.Header().ID }

// Leaf returns the ID of the entry the next append will name as its
// parent, or "" when the next append starts a new root.
func (s *Session) Leaf() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.leaf
}

// Len returns the number of entries.
func (s *Session) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}

// Entry returns the entry with the given ID.
func (s *Session) Entry(id string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byID[id]
	return e, ok
}

// Entries returns the entries in file order. The slice is a copy; the
// entries are shared.
func (s *Session) Entries() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Entry(nil), s.entries...)
}

// Children returns the IDs of the entries whose parent is id, in file
// order. Pass "" for the roots.
func (s *Session) Children(id string) []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.children[id]...)
}

// Roots returns the IDs of the root entries in file order.
func (s *Session) Roots() []string { return s.Children("") }

// Leaves returns the IDs of the entries that have no children, in file
// order.
func (s *Session) Leaves() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, e := range s.entries {
		if len(s.children[e.Base().ID]) == 0 {
			out = append(out, e.Base().ID)
		}
	}
	return out
}

// Path returns the entries from the root to id, root first, or nil when
// id is not in the session.
func (s *Session) Path(id string) []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.path(id)
}

func (s *Session) path(id string) []Entry {
	var rev []Entry
	for id != "" {
		e, ok := s.byID[id]
		if !ok {
			return nil
		}
		rev = append(rev, e)
		id = e.Base().Parent
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// Branch moves the leaf to id, so the next append becomes a child of
// that entry.
func (s *Session) Branch(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID[id]; !ok {
		return fmt.Errorf("%w: %s", ErrNoEntry, id)
	}
	s.leaf = id
	return nil
}

// ResetLeaf clears the leaf, so the next append starts a new root.
func (s *Session) ResetLeaf() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leaf = ""
}

// Append adds e to the tree. Its ID is the hash of its envelope over the
// hash of its body, as the format defines, and is computed here; an ID
// the caller set must match or the append is refused. An empty Parent
// is set to the current leaf, so an explicit Parent branches in place;
// a zero Timestamp is set to now, and any timestamp is taken to UTC,
// the one spelling the format admits. In a session with a base, the
// parent must be the base or an own entry.
//
// An append under the leaf makes the entry the leaf; an append elsewhere
// is a branch and the leaf does not move. A leaf label makes its target
// the leaf, wherever the label's own parent sits, when the target is the
// base or an own entry and not itself a label; otherwise nothing moves.
// The leaf never rests on a leaf label. An entry the session already
// holds — same type, body, parent, parents and timestamp — is a no-op
// that returns the existing ID.
//
// Parents, when the caller set any, is sorted into the order the format
// requires and checked against the convergence rules; it does not move
// the leaf and does not reach any context. On success e is owned by the
// session and must not be modified.
func (s *Session) Append(e Entry) (string, error) {
	r, err := s.Commit(e)
	return r.ID, err
}

// Outcome is what an append did, which the format has a store report.
type Outcome int

const (
	// Continued: the entry was added under the leaf and is the leaf.
	Continued Outcome = iota
	// Branched: the entry was added elsewhere and the leaf did not move.
	Branched
	// Held: the session already held the entry; nothing changed.
	Held
	// LeafMoved: a leaf label moved the leaf to its target.
	LeafMoved
	// LeafNotMoved: a leaf label named a target the leaf may not rest
	// on, and was added without moving it.
	LeafNotMoved
)

// String names the outcome.
func (o Outcome) String() string {
	switch o {
	case Continued:
		return "continued"
	case Branched:
		return "branched"
	case Held:
		return "held"
	case LeafMoved:
		return "leaf moved"
	case LeafNotMoved:
		return "leaf not moved"
	}
	return fmt.Sprintf("outcome(%d)", int(o))
}

// Result is what Commit reports: the entry's ID and what appending it
// did. A store adds what it alone can know.
type Result struct {
	ID      string
	Outcome Outcome
	// Unresolved lists references in the entry a store could not
	// resolve — a sidecar blob it does not hold, a convergence into a
	// session it does not have — which the format lets a store report
	// rather than refuse. A session in memory sets nothing here.
	Unresolved []string
	// Reopen is set by a store when the append is durable but the
	// caller's session could not be brought in step with it, so the
	// caller opens the session again before using it further.
	Reopen bool
}

// Prepare does everything Append does short of adding the entry: it
// fills the parent and the timestamp, sorts and checks the references,
// applies the parent rule, and computes the hashes, setting the ID on
// the entry. It reports the outcome Commit would have. A store uses it
// to know the entry's hashes before anything is written, so that
// nothing is visible in memory before the store's commit point; a
// Commit of the same entry afterwards recomputes the same values.
func (s *Session) Prepare(e Entry) (Result, error) {
	if err := validateEntry(e); err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prepare(e)
}

// Commit is Append with its outcome reported.
func (s *Session) Commit(e Entry) (Result, error) {
	if err := validateEntry(e); err != nil {
		return Result{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.prepare(e)
	if err != nil || r.Outcome == Held {
		return r, err
	}
	s.add(e)
	s.moveLeafFor(e)
	return r, nil
}

func (s *Session) prepare(e Entry) (Result, error) {
	b := e.Base()
	for k := range b.Unknown {
		if isEnvelopeKey(k) {
			return Result{}, fmt.Errorf("%w: %s", ErrReservedMember, k)
		}
	}
	if b.Parent == "" {
		b.Parent = s.leaf
	}
	if err := s.checkParentRule(b.Parent); err != nil {
		return Result{}, err
	}
	if b.Timestamp.IsZero() {
		b.Timestamp = s.now()
	}
	b.Timestamp = b.Timestamp.UTC()
	sortParents(b.Parents)
	if err := s.checkParents(b); err != nil {
		return Result{}, err
	}
	if d, ok := e.(*DispatchEntry); ok {
		// The format forbids a dispatch for a call a decision rejected.
		for _, c := range Calls(s.path(b.Parent)) {
			if c.ID() == d.CallID && c.Rejected() {
				return Result{}, fmt.Errorf("%w: %s", ErrCallRejected, d.CallID)
			}
		}
	}
	want := b.ID
	b.ID = ""
	if u, ok := e.(*UnknownEntry); ok {
		// The raw line is what gets written and hashed; keep it in step
		// with the envelope that was just filled in.
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return Result{}, err
		}
		u.Raw = raw
	}
	if err := s.hashEntry(e); err != nil {
		return Result{}, err
	}
	if want != "" && want != b.ID {
		return Result{}, fmt.Errorf("%w: given %s, computed %s", ErrBadID, want, b.ID)
	}
	if u, ok := e.(*UnknownEntry); ok {
		raw, err := rewriteEnvelope(u)
		if err != nil {
			return Result{}, err
		}
		u.Raw = raw
	}
	r := Result{ID: b.ID}
	if _, held := s.byID[b.ID]; held {
		r.Outcome = Held
		return r, nil
	}
	r.Outcome = s.outcomeFor(e)
	return r, nil
}

// outcomeFor says what moveLeafFor will do with e once added.
func (s *Session) outcomeFor(e Entry) Outcome {
	b := e.Base()
	if l, ok := e.(*LabelEntry); ok && l.Label != nil && *l.Label == LeafLabel {
		if s.mayRestOn(l.Target) {
			return LeafMoved
		}
		return LeafNotMoved
	}
	if b.Parent == s.leaf {
		return Continued
	}
	return Branched
}

// ErrBadID is returned by Append for an entry whose ID was set by the
// caller and does not match the hash the format defines, and by Read
// for a line whose id does not verify.
var ErrBadID = errors.New("agentsession: entry id does not match its hash")

// checkParentRule holds a parent to the format's rule: it exists, and in
// a session with a base it is the base or an own entry, since the
// prefix is another session's record and branching above the base is a
// new session with a lower base.
func (s *Session) checkParentRule(parent string) error {
	if parent == "" {
		if s.header.Base != "" {
			return fmt.Errorf("%w: a session with a base has no second root", ErrNoEntry)
		}
		return nil
	}
	if _, ok := s.byID[parent]; !ok {
		return fmt.Errorf("%w: parent %s", ErrNoEntry, parent)
	}
	if s.header.Base != "" && parent != s.header.Base && s.prefix[parent] {
		return fmt.Errorf("%w: parent %s is on the prefix above the base", ErrNoEntry, parent)
	}
	return nil
}

// hashEntry computes the entry's content hash and ID from its encoded
// form and sets them on the envelope.
func (s *Session) hashEntry(e Entry) error {
	data, err := MarshalEntry(e)
	if err != nil {
		return err
	}
	id, content, err := EntryHashes(data)
	if err != nil {
		return err
	}
	b := e.Base()
	b.ID = id
	b.content = content
	return nil
}

// moveLeafFor applies the leaf rule after an append: under the leaf, the
// entry is the leaf; elsewhere, a branch and nothing moves; a leaf label
// moves the leaf to its target when the target is one the leaf may rest
// on.
func (s *Session) moveLeafFor(e Entry) {
	b := e.Base()
	if l, ok := e.(*LabelEntry); ok && l.Label != nil && *l.Label == LeafLabel {
		if s.mayRestOn(l.Target) {
			s.leaf = l.Target
		}
		return
	}
	if b.Parent == s.leaf {
		s.leaf = b.ID
	}
}

// mayRestOn reports whether the leaf may rest on id: it exists, it is
// the base or an own entry, and it is not a leaf label.
func (s *Session) mayRestOn(id string) bool {
	e, ok := s.byID[id]
	if !ok {
		return false
	}
	if s.header.Base != "" && id != s.header.Base && s.prefix[id] {
		return false
	}
	if l, ok := e.(*LabelEntry); ok && l.Label != nil && *l.Label == LeafLabel {
		return false
	}
	return true
}

// Repeated returns the IDs Read met a second time in the file, each
// treated as the same entry as the first, in file order.
func (s *Session) Repeated() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.repeated...)
}

// Migrated reports whether the session was read from a file of an
// earlier minor version and rewritten in memory, so its entries carry
// LegacyID, and returns the IDs of entries the migration could not
// rewrite: extension entries, whose members may name entries the reader
// cannot recognise. A migrated session with unresolved entries cannot
// be written as 0.5.
func (s *Session) Migrated() (bool, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.migrated, append([]string(nil), s.unresolved...)
}

// Prefix reports whether id is on the path to the session's base: an
// entry another session wrote, carried so the file stands alone.
func (s *Session) Prefix(id string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.prefix[id]
}

// MarkLeaf builds the label entry that makes the current leaf durable,
// so a reopened session resumes on this branch rather than on whichever
// one the last line happens to be under. Append it through the store
// after Branch; the leaf stays where it is, and appending past the mark
// moves the resumed leaf with it. It returns an error when there is no
// leaf.
//
// A mark records which branch is live, so a host that moves to another
// branch marks again; nothing under an abandoned mark follows it, and
// the abandoned mark is what a reopen honours.
func (s *Session) MarkLeaf() (*LabelEntry, error) {
	leaf := s.Leaf()
	if leaf == "" {
		return nil, errors.New("agentsession: no leaf to mark")
	}
	return NewLabelEntry(leaf, LeafLabel), nil
}

// durableLeafAt returns the entry the last leaf label in force names
// and the index of that label, or ("", -1) when none is in force: a
// later leaf label replaces an earlier one, and a null label on the
// marked entry clears it.
func (s *Session) durableLeafAt() (string, int) {
	marked, at := "", -1
	for i, e := range s.entries {
		l, ok := e.(*LabelEntry)
		if !ok {
			continue
		}
		switch {
		case l.Label != nil && *l.Label == LeafLabel:
			// In force only when the leaf may rest on the target: an
			// entry in the file that is the base or an own entry and
			// not itself a leaf label. Otherwise the label moved
			// nothing when it was appended, and moves nothing now.
			if s.mayRestOn(l.Target) {
				marked, at = l.Target, i
			}
		case l.Label == nil && l.Target == marked:
			marked, at = "", -1
		}
	}
	return marked, at
}

// resolveLeaf returns the entry the next append should hang from: the
// newest entry in file order that descends from the durable leaf mark
// and was appended after it, the mark itself when nothing follows it,
// and the last line when no mark is in force.
//
// The mark says which branch is live, not which entry is its tip, so a
// branch marked and then extended resolves to where it was extended to
// rather than rewinding to the mark. That is the reading the export
// package's PreferCurrentLeaf already takes of it.
//
// Two rules decide it, and the mark carries both coordinates: where it
// points in the tree, and where it sits in the file. Entries under the
// mark that predate it are work the mark was placed in spite of, not
// work done on the branch since, so only entries after its line are
// candidates. When the mark is on a branch that was later left without
// re-marking, nothing under it follows it and the mark stands, which is
// what a mark is for.
func (s *Session) resolveLeaf() string {
	if len(s.entries) == 0 {
		return ""
	}
	marked, at := s.durableLeafAt()
	if marked == "" {
		return s.notOnLabel(s.entries[len(s.entries)-1].Base().ID)
	}
	// A parent always precedes its children in the file, so descent is a
	// single forward pass and needs no walk back up any path.
	under := map[string]bool{marked: true}
	leaf := marked
	for i, e := range s.entries {
		b := e.Base()
		if under[b.Parent] {
			under[b.ID] = true
			if i > at {
				leaf = b.ID
			}
		}
	}
	return s.notOnLabel(leaf)
}

// notOnLabel walks up from id while it is a leaf label, since the leaf
// never rests on one; the base stands in when the walk would leave the
// session's own entries, and "" when a baseless session has nothing
// above.
func (s *Session) notOnLabel(id string) string {
	for id != "" {
		e := s.byID[id]
		l, ok := e.(*LabelEntry)
		if !ok || l.Label == nil || *l.Label != LeafLabel {
			break
		}
		id = e.Base().Parent
	}
	if s.header.Base != "" && (id == "" || (s.prefix[id] && id != s.header.Base)) {
		return s.header.Base
	}
	return id
}

// sortParents puts convergence references in the order the format
// requires of a writer: by session, then by entry. A reference to an
// entry in this session omits the session, and the empty string sorts
// before every other, so "those come first" falls out of the same
// comparison rather than needing a case of its own. Sorting is what
// keeps a file independent of the order workers happened to finish in.
//
// Only a writer is held to this. A file that arrives unsorted is read
// and written back as it was, like every other thing a reader is given
// and does not get to improve.
func sortParents(refs []EntryRef) {
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Session != refs[j].Session {
			return refs[i].Session < refs[j].Session
		}
		return refs[i].Entry < refs[j].Entry
	})
}

// checkParents applies the convergence rules to an entry's Parents:
// every reference names an entry, no entry is named twice, none names
// the entry's own parent, and a reference into this session names an
// entry that already exists. That last one is what makes the structure
// acyclic — with parent, every edge points at something older — so it
// is checked on read as well as on append.
//
// A reference naming this session's own ID is a reference into this
// file, which the format lets a writer say either way, so it is held
// to the same rules as one that leaves the session out. A reference
// into another session is not resolvable here and is only checked for
// shape; whether that session exists is a question for a store.
func (s *Session) checkParents(b *EntryBase) error {
	if len(b.Parents) == 0 {
		return nil
	}
	seen := make(map[EntryRef]bool, len(b.Parents))
	for _, r := range b.Parents {
		if r.Entry == "" {
			return fmt.Errorf("%w: entry %s names a predecessor with no entry id", ErrBadConvergence, b.ID)
		}
		if r.Session == s.header.ID {
			r.Session = ""
		}
		if seen[r] {
			return fmt.Errorf("%w: entry %s names %s twice", ErrBadConvergence, b.ID, r.Entry)
		}
		seen[r] = true
		if r.Session != "" {
			continue
		}
		if r.Entry == b.Parent {
			return fmt.Errorf("%w: entry %s converges its own parent %s", ErrBadConvergence, b.ID, r.Entry)
		}
		if _, ok := s.byID[r.Entry]; !ok {
			return fmt.Errorf("%w: entry %s converges %s, which is not in this session yet", ErrBadConvergence, b.ID, r.Entry)
		}
	}
	return nil
}

// validateEntry rejects an entry that could not be written: the checks
// MarshalEntry makes, applied before the tree changes.
func validateEntry(e Entry) error {
	switch v := e.(type) {
	case nil:
		return errors.New("agentsession: nil entry")
	case *ItemEntry:
		if v.Item == nil {
			return errors.New("agentsession: item entry has no item")
		}
		if _, ok := v.Item.(*openresponses.ItemReference); ok {
			// The format's ingress rule: an entry carries what the
			// model saw, and a reference to the provider's store is
			// not that. The same dependency is already refused on the
			// request side, which is built with store: false and no
			// previous_response_id. A file carrying one still reads —
			// this is a rule for writers, and this is the writer.
			return errors.New("agentsession: an item entry cannot hold an item_reference: it names an item in the provider's store rather than carrying what the model saw")
		}
	case *CompactionEntry:
		if v.Summary == nil {
			return errors.New("agentsession: compaction entry has no summary")
		}
	case *BranchSummaryEntry:
		if v.Summary == nil {
			return errors.New("agentsession: branch_summary entry has no summary")
		}
	case *ConfigEntry:
		return validateConfig(v)
	case *UnknownEntry:
		if len(v.Raw) == 0 {
			return errors.New("agentsession: unknown entry has no raw bytes")
		}
	case *RunEntry, *DispatchEntry, *DecisionEntry, *QueuedEntry:
		return validateLifecycle(e)
	}
	return nil
}

// validateConfig checks the instructions parts of a config delta: a
// part is named, named once, and when every part carries its text the
// instructions string beside them is their join, which is the rule a
// reader replays. A delta decoded from a file is not checked, since a
// reader preserves what it is given; this is what a writer is held
// to.
func validateConfig(c *ConfigEntry) error {
	seen := make(map[string]bool, len(c.InstructionsParts))
	full := true
	for _, p := range c.InstructionsParts {
		if p.ID == "" {
			return errors.New("agentsession: an instructions part has no id")
		}
		if seen[p.ID] {
			return fmt.Errorf("agentsession: instructions part %q is named twice", p.ID)
		}
		seen[p.ID] = true
		if p.Text == "" && p.Hash != "" {
			full = false
		}
	}
	if full && len(c.InstructionsParts) > 0 && c.Instructions != nil && *c.Instructions != JoinInstructions(c.InstructionsParts) {
		return errors.New("agentsession: instructions and instructions_parts disagree: the string is the parts joined with a blank line")
	}
	if !full && c.Replace && c.Instructions == nil {
		// A replace discards the parts a hash would name, so nothing on
		// the path resolves it and the entry would set instructions a
		// reader cannot rebuild.
		return errors.New("agentsession: a replacing config must carry the text of every instructions part, or the instructions string beside them")
	}
	for _, o := range c.InstructionsOmitted {
		if o.ID == "" {
			return errors.New("agentsession: an omitted instructions part has no id")
		}
	}
	return nil
}

// add links an already-validated entry into the indexes.
func (s *Session) add(e Entry) {
	b := e.Base()
	s.entries = append(s.entries, e)
	s.byID[b.ID] = e
	s.children[b.Parent] = append(s.children[b.Parent], b.ID)
}

// Truncated reports the final line of the file this session was read
// from when that line did not parse, or nil. A truncated line is what a
// crash mid-append leaves behind; the entries before it are intact.
func (s *Session) Truncated() *TruncatedLine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.truncated
}

// Labels returns the current label of every labelled entry: the last
// label entry per target wins, and a null label clears.
func (s *Session) Labels() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]string{}
	for _, e := range s.entries {
		if l, ok := e.(*LabelEntry); ok {
			if l.Label == nil {
				delete(out, l.Target)
			} else {
				out[l.Target] = *l.Label
			}
		}
	}
	return out
}

// Name returns the display name from the last info entry that set one.
func (s *Session) Name() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	name := ""
	for _, e := range s.entries {
		if i, ok := e.(*InfoEntry); ok && i.Name != "" {
			name = i.Name
		}
	}
	return name
}

// Compact builds the compaction entry for the current leaf: FirstKept
// names the earliest entry on the path that stays in context, summary
// is the item that replaces everything before it, and the settings
// checkpoint is taken from the context at the leaf. The entry is not
// appended; set TokensBefore, Usage or Pinned if they apply, then
// append it through the store. A caller that knows an item index
// rather than an entry ID uses [Session.CompactFrom] or
// [Session.CompactKeeping].
func (s *Session) Compact(firstKept string, summary openresponses.Item) (*CompactionEntry, error) {
	leaf, ctx, err := s.compactionContext(summary)
	if err != nil {
		return nil, err
	}
	onPath := false
	for _, e := range s.Path(leaf) {
		if e.Base().ID == firstKept {
			onPath = true
			break
		}
	}
	if !onPath {
		return nil, fmt.Errorf("%w: first_kept %s is not on the path to %s", ErrNoEntry, firstKept, leaf)
	}
	return &CompactionEntry{FirstKept: firstKept, Summary: summary, Config: ctx.Settings}, nil
}

// CompactFrom is [Session.Compact] for a caller that split the request
// input at an index: FirstKept is the entry that contributed item
// first of the context at the leaf, and the items before it are what
// summary replaces. The index counts the items the context algorithm
// produces, [Context.Items], so entries that contribute no item do not
// shift it. The item at first cannot be an earlier compaction's
// summary or one of its pinned items: the format keeps entries, and
// what a compaction contributes is kept only while it is the last
// compaction on the path.
func (s *Session) CompactFrom(first int, summary openresponses.Item) (*CompactionEntry, error) {
	_, ctx, err := s.compactionContext(summary)
	if err != nil {
		return nil, err
	}
	if first < 0 || first >= len(ctx.Items) {
		return nil, fmt.Errorf("agentsession: first kept item %d is outside a context of %d items", first, len(ctx.Items))
	}
	return compactionFrom(ctx, first, summary)
}

// CompactKeeping is [Session.Compact] for a caller that knows how many
// items of the request input it kept: the last kept items of the
// context at the leaf stay, and summary replaces the rest. kept must be
// at least 1, because FirstKept names an entry, and at most the number
// of items in the context.
func (s *Session) CompactKeeping(kept int, summary openresponses.Item) (*CompactionEntry, error) {
	_, ctx, err := s.compactionContext(summary)
	if err != nil {
		return nil, err
	}
	if kept < 1 || kept > len(ctx.Items) {
		return nil, fmt.Errorf("agentsession: cannot keep %d items of a context of %d", kept, len(ctx.Items))
	}
	return compactionFrom(ctx, len(ctx.Items)-kept, summary)
}

// compactionContext checks the summary and returns the leaf and the
// context at it, the inputs every Compact variant shares.
func (s *Session) compactionContext(summary openresponses.Item) (string, Context, error) {
	if summary == nil {
		return "", Context{}, errors.New("agentsession: compaction needs a summary item")
	}
	leaf := s.Leaf()
	if leaf == "" {
		return "", Context{}, errors.New("agentsession: no leaf to compact")
	}
	ctx, err := s.ContextAt(leaf)
	if err != nil {
		return "", Context{}, err
	}
	return leaf, ctx, nil
}

// compactionFrom builds the entry that keeps ctx.Items[first:].
func compactionFrom(ctx Context, first int, summary openresponses.Item) (*CompactionEntry, error) {
	e := ctx.ItemEntries[first]
	if _, ok := e.(*CompactionEntry); ok {
		// A compaction contributes its summary and then each of its
		// pinned items, so the index decides which one this is.
		// Neither can be kept: a later compaction replaces both.
		what := "the summary"
		if first > 0 && ctx.ItemEntries[first-1] == e {
			what = "a pinned item"
		}
		return nil, fmt.Errorf("agentsession: item %d is %s of compaction %s, which a later compaction replaces rather than keeps", first, what, e.Base().ID)
	}
	return &CompactionEntry{FirstKept: e.Base().ID, Summary: summary, Config: ctx.Settings}, nil
}

// SummarizeBranch builds the branch summary that carries context from
// the abandoned leaf from to the current leaf, where the new branch
// continues. Move the leaf with Branch first, then append the result
// through the store; its parent is set on append.
func (s *Session) SummarizeBranch(from string, summary openresponses.Item) (*BranchSummaryEntry, error) {
	if summary == nil {
		return nil, errors.New("agentsession: branch summary needs a summary item")
	}
	if _, ok := s.Entry(from); !ok {
		return nil, fmt.Errorf("%w: %s", ErrNoEntry, from)
	}
	if s.Leaf() == "" {
		return nil, errors.New("agentsession: no leaf to continue from")
	}
	return &BranchSummaryEntry{From: from, Summary: summary}, nil
}

// utcNow is the session clock: the current time in UTC with the
// monotonic reading dropped, so what is stamped is what reaches disk.
func utcNow() time.Time { return time.Now().UTC().Round(0) }

// Fork creates a session that continues from entry at of origin: a
// session with a base, in the format's terms. The new session's header
// is h with Base set to at and ParentSession to origin's ID; its
// entries open with origin's path to at, the prefix, which the new
// session shares with origin rather than copies, and its leaf is the
// base. Every entry it appends hangs from the base or from an entry it
// appended itself, and its file opens with the prefix so it stands
// alone. The base may not be a leaf label, since it is the fork's first
// leaf and the leaf never rests on one.
func Fork(origin *Session, at string, h Header) (*Session, error) {
	origin.mu.RLock()
	defer origin.mu.RUnlock()
	path := origin.path(at)
	if path == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoEntry, at)
	}
	if l, ok := path[len(path)-1].(*LabelEntry); ok && l.Label != nil && *l.Label == LeafLabel {
		return nil, fmt.Errorf("%w: a base may not be a leaf label", ErrNoEntry)
	}
	h.Base = at
	h.ParentSession = origin.header.ID
	if h.Media == "" {
		h.Media = origin.header.Media
	}
	if h.Payload == "" {
		h.Payload = origin.header.Payload
	}
	s := New(h)
	s.prefix = map[string]bool{}
	for _, e := range path {
		s.add(e)
		s.prefix[e.Base().ID] = true
	}
	s.leaf = at
	return s, nil
}
