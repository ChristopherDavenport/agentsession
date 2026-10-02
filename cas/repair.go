package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// A session's log is its only record, so a record that fails its
// checksum keeps the session from opening, and a sweep from running,
// until someone decides what the damage cost. Repair is that decision,
// made conservatively: it keeps what the log's readable records say and
// the objects bear out, writes that as a new log, and keeps the damaged
// one beside it, as recovery keeps the bytes it cuts.

// damagedLogPrefix names a session's damaged log, kept by a repair.
const damagedLogPrefix = "damaged-"

// ErrNotDamaged is returned by Repair for a session whose log has no
// damage to repair.
var ErrNotDamaged = errors.New("cas: the session's log is not damaged; there is nothing to repair")

// ErrUnrecoverable is returned by Repair when no entry of a damaged log
// can be kept. Delete is what remains.
var ErrUnrecoverable = errors.New("cas: nothing in the session's log can be recovered; Delete the session instead")

// RepairOptions says how Repair runs.
type RepairOptions struct {
	// DryRun reports what a repair would keep and drop, and writes
	// nothing.
	DryRun bool
}

// RepairReport is what Repair found, and kept and dropped.
type RepairReport struct {
	// Damage is each line of the log that fails its checksum.
	Damage []LogDamage
	// Kept is the entries the repaired log holds, in log order.
	Kept []string
	// Hidden is the kept entries no readable record appends: ancestors
	// of a kept entry, named by its envelope, whose records the damage
	// hid.
	Hidden []string
	// Salvaged is the kept entries only a damaged record names: its
	// line still spells the entry's hash, no readable record appends the
	// entry or records it lost, the entry's objects read and hash to
	// their names, and its parent is the base or kept.
	Salvaged []string
	// Dropped is each entry a readable record appends that the repaired
	// log does not hold, and each hash a damaged line spells that was
	// not salvaged, with why. A hash a damaged line spells may be what
	// the damage made of another; it is dropped for having no object.
	Dropped []DroppedEntry
	// Head is the repaired session's head. Named is the head the damaged
	// log last named, by its last readable head record or a damaged one
	// salvaged with its entry; when Head differs, that entry was dropped
	// and Head is the latest kept leaf. Named is empty when Unread is
	// set.
	Head, Named string
	// Unread is set when a damaged line follows the last head the
	// repair could read: that of the last readable head record, or of a
	// damaged append record salvaged with its entry, whichever is later
	// in the log. That line may have moved the head or appended entries
	// the repair cannot name, so the head the log last named is not
	// known; Head is then that last head read, the salvaged entry when
	// it is the later, or the latest kept leaf when that was dropped.
	Unread bool
	// Mark is the record mark, as the last readable mark record says.
	Mark string
	// DamagedLog is where the damaged log is kept, in the session's
	// directory; empty on a dry run.
	DamagedLog string
}

// DroppedEntry is an entry a repair dropped, or a hash a damaged line
// spells that it did not salvage, and why.
type DroppedEntry struct {
	Entry string
	Err   error
}

// Repair rewrites a session's damaged log from the records that still
// read, holding the session's lock. It keeps every record that passes
// its checksum, and of the entries those append, the ones whose objects
// are present and hash to their names, whose convergence references
// name kept entries, and whose parent is the session's base, or no
// parent in a session without one, or a kept entry; an entry hanging
// from a dropped one is dropped too. A parent no readable record
// appends is one the damage hid: the child's envelope names it by its
// hash, so it is kept, with its own ancestors, when they all read and
// reach the base. An entry only a damaged record names, which the
// hidden-parent rule cannot reach since no readable child names it, a
// branch's leaf or the session's last entry, is salvaged from the hash
// the damaged line still spells, wherever the line is in the log, under
// the same tests, and reported; a hash a damaged line spells that is not
// salvaged is reported as dropped, with why. A lost record is recovery's
// word to the writer that a crash took the append before it, so an
// entry a damaged lost record still legibly names is not kept from an
// append before that record, a readable one either, since a repair
// would bring back what the writer was told was lost; an append after
// it is the writer appending the entry again, and stands, as it does
// after a readable lost record. The head is the kept entry the last readable head record,
// or a salvaged one, names, or else the latest kept leaf; when a damaged
// line follows the last head read, the report says the head the log
// last named is not known. The
// mark is the last readable mark record's, or a mirror's when none
// reads. The new log is written durably and renamed into place; the
// damaged one is kept in the session's directory as damaged-<time>,
// which Verify reports until someone removes it, and while it is there a
// sweep keeps everything it names, readable or not.
//
// Repair is not recovery, and its rule differs: a lazy append whose
// objects are gone is dropped with what hangs from it, not with every
// record after it, since a damaged log no longer says where its
// commits ended; every append kept is committed by the new log, its
// objects written durably first when no readable commit covered them.
//
// Repair refuses a session whose log lost no record to damage with
// ErrNotDamaged: one with a record's newline damaged and both records
// read, which opens and sweeps as it is, and one a crash left blocks
// unwritten in, which its next open recovers. It refuses a session this
// store holds open, which is released first, and a read-only or stopped
// store, except for a dry run, which writes nothing and takes no lock
// on a read-only store. When nothing can be kept it returns
// ErrUnrecoverable.
func (s *Store) Repair(ctx context.Context, id string, opts RepairOptions) (RepairReport, error) {
	var rep RepairReport
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	if !opts.DryRun {
		if err := s.writable(); err != nil {
			return rep, err
		}
	}
	dir, err := s.sessionDir(id)
	if err != nil {
		return rep, err
	}
	if _, err := os.Stat(filepath.Join(dir, "header")); errors.Is(err, os.ErrNotExist) {
		return rep, fmt.Errorf("%w: %s", agentsession.ErrNoSession, id)
	}
	h, held := s.claim(id)
	if held {
		h.mu.Unlock()
		return rep, fmt.Errorf("cas: session %s is open in this store; release it before repairing it", id)
	}
	defer s.abandon(id, h)
	lk, err := s.lockSession(id)
	if err != nil {
		return rep, err
	}
	defer lk.release()

	hdr, err := readHeader(dir)
	if err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	path := filepath.Join(dir, logName)
	data, err := os.ReadFile(path)
	if err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	l, err := parseSessionLog(bytes.NewReader(data), 0, int64(len(data)))
	if err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	if l.legacy {
		return rep, fmt.Errorf("cas: session %s: %w", id, ErrLegacyStore)
	}
	if !l.lost || tailLoss(l) {
		// No damage that cost a record, or only what a crash leaves in
		// the uncommitted tail: the session opens, and its recovery cuts
		// that tail.
		return rep, fmt.Errorf("%w: session %s", ErrNotDamaged, id)
	}
	rep.Damage = l.damage

	lines := readLines(data, l.damage)
	plan, err := s.planRepair(id, hdr, l.recs, lines)
	if err != nil {
		return rep, err
	}
	rep.Kept, rep.Hidden, rep.Salvaged, rep.Dropped, rep.Head, rep.Named = plan.kept, plan.hidden, plan.salvaged, plan.dropped, plan.head, plan.named
	if lines.lastDamaged > max(lines.lastHead, plan.headAt) {
		rep.Unread, rep.Named = true, ""
	}
	rep.Mark = plan.mark
	if rep.Mark == "" {
		rep.Mark = readMark(dir)
	}
	if rep.Mark != MarkRecord {
		// A mark that cannot be read is a mirror's, as readMark takes
		// it: nothing this store did not finish marking is advanced.
		rep.Mark = MarkMirror
	}
	if len(plan.kept) == 0 {
		return rep, fmt.Errorf("%w: session %s", ErrUnrecoverable, id)
	}
	if opts.DryRun {
		return rep, nil
	}

	// No sweep's last step between the objects made durable here and
	// the log that names them.
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return rep, err
	}
	defer guard.release()
	// An append no readable commit covers may have objects never
	// fsynced; the new log says every kept append is committed, so they
	// are written durably first.
	pend := newPendSet()
	for _, e := range plan.uncommitted {
		if err := s.freshenEntry(e, pend); err != nil {
			return rep, fmt.Errorf("cas: session %s: %w", id, err)
		}
		if c, err := s.contentOf(e); err == nil {
			if body, err := s.objs.read(spaceContents, c); err == nil {
				for _, b := range blobsNamedBy(body) {
					_ = s.objs.freshenTo(spaceContents, b, pend) // a blob not held stays unresolved
				}
			}
		}
	}
	if err := s.objs.flushSet(pend); err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}

	out := []logRecord{{Op: opCreate, Session: id, Base: hdr.Base}, {Op: opMark, Session: id, Mark: rep.Mark}}
	for i, e := range plan.kept {
		out = append(out, logRecord{Op: opAppend, Session: id, Entry: e, Seq: i + 1, Size: plan.sizes[e]})
	}
	if plan.head != "" {
		out = append(out, logRecord{Op: opHead, Session: id, Head: plan.head, Seq: len(plan.kept)})
	}
	fresh, err := encodeRecords(true, out)
	if err != nil {
		return rep, err
	}

	// The damaged log is kept first, durably, and its place in the
	// directory too, so no crash leaves the new log without it.
	aside := filepath.Join(dir, fmt.Sprintf("%s%d", damagedLogPrefix, time.Now().UnixNano()))
	if _, err := s.objs.writeFile(aside, data, true); err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	if err := s.objs.fsyncDir(dir); err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	// A copy kept for a repair that did not take place would say one
	// had: it goes while the damaged log is still the log.
	unkeep := func() {
		if again, err := os.ReadFile(path); err == nil && bytes.Equal(again, data) {
			os.Remove(aside)
		}
	}
	if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, data) {
		unkeep()
		return rep, fmt.Errorf("cas: session %s: the log changed as repair read it", id)
	}
	if err := s.objs.writeAtomic(path, fresh); err != nil {
		unkeep()
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
	rep.DamagedLog = aside
	// Indexes of the new log; the next open rebuilds any left stale.
	_ = writeHead(dir, plan.head)
	_ = writeIndex(filepath.Join(dir, "record"), []byte(rep.Mark+"\n"))
	_ = os.Remove(filepath.Join(dir, summaryName))
	s.change(func(x *entryIndex) {
		x.drop(id)
		for _, e := range plan.kept {
			x.own(e, id)
		}
		if plan.path != nil {
			x.setPath(id, plan.path)
		}
	})
	s.setFaulty(id, nil)
	return rep, nil
}

// logLine is one line of a damaged log that failed its checksum, by
// its offset, with how many readable records come before it, so a
// repair reads it where it is in the log.
type logLine struct {
	off    int64
	bytes  []byte
	before int
}

// logLines is what a repair reads of a damaged log's lines beyond its
// records: the damaged lines, in order, the offset of the last line
// holding a readable head record, and of the last damaged line, each -1
// when there is none.
type logLines struct {
	damaged     []logLine
	lastHead    int64
	lastDamaged int64
}

// readLines reads a damaged log's lines as logLines says. A record's
// newline damaged into another byte lost nothing, and is not counted.
// The readable records are counted as parseSessionLog reads them, line
// by line through decodeLine, so a damaged line's place among them is
// its place in the records the repair plans from.
func readLines(data []byte, damage []LogDamage) logLines {
	ls := logLines{lastHead: -1, lastDamaged: -1}
	damaged := map[int64]bool{}
	for _, d := range damage {
		if !errors.Is(d.Err, errNewline) {
			damaged[d.Offset] = true
			ls.lastDamaged = max(ls.lastDamaged, d.Offset)
		}
	}
	checked := 0
	for off := 0; off < len(data); {
		end := bytes.IndexByte(data[off:], '\n')
		if end < 0 {
			end = len(data) - off - 1
		}
		line := data[off : off+end+1]
		if damaged[int64(off)] {
			ls.damaged = append(ls.damaged, logLine{off: int64(off), bytes: line, before: checked})
		}
		recs, _ := decodeLine(line)
		for _, r := range recs {
			if !r.checked {
				continue
			}
			checked++
			if r.Op == opHead || (r.Op == opAppend && r.Head != "") {
				ls.lastHead = int64(off)
			}
		}
		off += len(line)
	}
	return ls
}

// hashToken matches a well-formed hash wherever it is spelled.
var hashToken = regexp.MustCompile(`sha256:[0-9a-f]{64}`)

// oneDigitOff returns the hash among tokens that differs from e in one
// byte, as the hash one damaged byte makes of another does, or "".
func oneDigitOff(e string, tokens [][]byte) string {
	for _, t := range tokens {
		h := string(t)
		if len(h) != len(e) || h == e {
			continue
		}
		diff := 0
		for i := range h {
			if h[i] != e[i] {
				diff++
			}
		}
		if diff == 1 {
			return h
		}
	}
	return ""
}

// errDamagedLost and errLostRecord say why a hash a damaged line spells
// is not salvaged: a lost record, damaged but legible, or readable,
// names it.
var (
	errDamagedLost = errors.New("a damaged lost record legibly names it: recovery recorded the append lost, and a repair does not bring back what the writer was told was lost")
	errLostRecord  = errors.New("a lost record names it: recovery recorded the append lost")
)

// repairPlan is what a repair keeps of a log's readable records.
type repairPlan struct {
	kept        []string
	hidden      []string
	salvaged    []string
	dropped     []DroppedEntry
	sizes       map[string]int64
	uncommitted []string // kept appends no readable commit covers
	head, named string
	headAt      int64 // the offset of a damaged line salvaged as naming the head, or -1
	mark        string
	path        []string // the path to the base, base first
}

// planRepair works out what a repair keeps of a log's readable records
// and its damaged lines, read in log order. A record damaged in a way
// that leaves it decoding without its checksum is no readable record.
//
// A damaged append record still spells the hash of the entry it
// appends, as its entry and as its head, and a single damaged byte
// spoils at most one. The log is the session's own file and the objects
// are the evidence, so each hash a damaged line spells, wherever the
// line is, is salvaged when no readable record appends the entry or
// records it lost, no damaged line legibly records it lost, its
// objects read and hash to their names, and its parent is the base or
// kept: nothing is taken on the damaged record's word that the objects
// do not bear out. It is salvaged where its line is, so the records
// after it find it kept, as their parent or as an entry they converge;
// one a readable record's entry then hangs from is reported hidden, as
// the child's envelope would have kept it had the line spelled nothing.
// It is the head only when the line names it as its head. A hash not
// salvaged is dropped, with why, so the report accounts for every hash
// the damage touched; one the damage made of another, a digit off a
// hash the line also spells, is dropped as no entry. A lost record is
// recovery's word to the writer that a crash took the append before it,
// and a damaged one that still legibly says so is taken at that word
// over the appends before it, a readable one too, since a salvage would
// bring back what the writer was told was lost; it says nothing of an
// append after it, by which the writer appended the entry again, so the
// damaged and the readable lost records are read alike, by position.
func (s *Store) planRepair(id string, hdr agentsession.Header, all []logRecord, lines logLines) (repairPlan, error) {
	p := repairPlan{sizes: map[string]int64{}, headAt: -1}
	var recs []logRecord
	for _, r := range all {
		if r.checked {
			recs = append(recs, r)
		}
	}
	onPath := map[string]bool{}
	if hdr.Base != "" {
		var err error
		if p.path, err = s.pathTo(hdr.Base); err != nil {
			return p, fmt.Errorf("cas: session %s: the path to its base: %w", id, err)
		}
		for _, e := range p.path {
			onPath[e] = true
		}
	}
	synced := -1
	lostAt := map[string]int{}
	appended := map[string]bool{} // by a readable record, which decides it
	for i, r := range recs {
		switch r.Op {
		case opSync:
			synced = i
		case opLost:
			lostAt[r.Entry] = i
		case opMark:
			p.mark = r.Mark
		case opAppend:
			appended[r.Entry] = true
		}
	}
	gone := func(entry string, i int) bool {
		at, ok := lostAt[entry]
		return ok && at > i
	}
	// What the damaged lines legibly record lost, each hash with the
	// last such line naming it. A lost record stands against an append
	// before it, which is the one recovery cut, and not against one after
	// it, which is the writer appending the entry again once told; the
	// readable and the damaged lost records are read alike, by position.
	lostBy := map[string]logLine{}
	for _, l := range lines.damaged {
		if bytes.Contains(l.bytes, []byte(`"op":"lost"`)) {
			for _, h := range hashToken.FindAll(l.bytes, -1) {
				lostBy[string(h)] = l
			}
		}
	}
	// lostAfterRecord reports whether a damaged lost record after the
	// readable record i legibly names entry, as gone reports a readable
	// one; lostAfterLine reports whether a lost record of either kind
	// comes after the damaged line l and names entry.
	lostAfterRecord := func(entry string, i int) bool {
		l, ok := lostBy[entry]
		return ok && l.before > i
	}
	lostAfterLine := func(entry string, l logLine) (error, bool) {
		if d, ok := lostBy[entry]; ok && d.off > l.off {
			return errDamagedLost, true
		}
		if at, ok := lostAt[entry]; ok && at >= l.before {
			return errLostRecord, true
		}
		return nil, false
	}
	kept := map[string]bool{}
	seen := map[string]bool{}      // appended by a readable record read so far
	failed := map[string]error{}   // hidden entries that cannot be kept
	listed := map[string]bool{}    // dropped as a hash a damaged line spells
	parents := map[string]string{} // of each entry checked
	dropSpelled := func(e string, err error) {
		if !listed[e] {
			listed[e] = true
			p.dropped = append(p.dropped, DroppedEntry{Entry: e, Err: err})
		}
	}
	// check reads an entry's objects, checked against their names, and
	// returns its parent once its convergence references are all in the
	// session.
	check := func(e string) (string, error) {
		if _, err := s.loadLine(e); err != nil {
			return "", err
		}
		env, err := s.envelope(e)
		if err != nil {
			return "", err
		}
		var parent *string
		if err := json.Unmarshal(env["parent"], &parent); err != nil {
			return "", fmt.Errorf("cas: entry %s: %w", e, err)
		}
		if raw, ok := env["parents"]; ok {
			var refs []agentsession.EntryRef
			if err := json.Unmarshal(raw, &refs); err != nil {
				return "", fmt.Errorf("cas: entry %s: %w", e, err)
			}
			for _, r := range refs {
				if (r.Session == "" || r.Session == id) && !kept[r.Entry] && !onPath[r.Entry] {
					return "", fmt.Errorf("it converges %s, which is not kept", r.Entry)
				}
			}
		}
		if parent == nil {
			return "", nil
		}
		parents[e] = *parent
		return *parent, nil
	}
	keep := func(e string, i int, r logRecord) {
		kept[e] = true
		p.kept = append(p.kept, e)
		if r.Size > 0 {
			p.sizes[e] = r.Size
		}
		if i < 0 || (r.Lazy && i > synced) {
			p.uncommitted = append(p.uncommitted, e)
		}
	}
	// hidden keeps the ancestors of an entry that no readable record
	// appends, down to a kept entry or the base: the damage hid their
	// records, and the entry's envelope names them by their hashes. An
	// entry of the session hangs from the base or another of its own, so
	// each is the session's. It fails if any of them is unreadable, or
	// was dropped, or the chain leaves the session.
	hidden := func(parent string) error {
		var chain []string
		var err error
		for e := parent; ; {
			if kept[e] || e == hdr.Base {
				break
			}
			if e == "" {
				err = errors.New("its parent chain does not reach the session's base")
				break
			}
			if _, lost := lostBy[e]; seen[e] || lost || listed[e] {
				err = fmt.Errorf("its parent %s was dropped", e)
				break
			}
			if ferr, ok := failed[e]; ok {
				err = ferr
				break
			}
			next, cerr := check(e)
			if cerr != nil {
				err = fmt.Errorf("its parent %s, which no readable record appends, cannot be kept: %w", e, cerr)
				break
			}
			chain = append(chain, e)
			e = next
		}
		if err != nil {
			for _, e := range chain {
				failed[e] = err
			}
			return err
		}
		for i := len(chain) - 1; i >= 0; i-- {
			keep(chain[i], -1, logRecord{})
			p.hidden = append(p.hidden, chain[i])
		}
		return nil
	}
	// salvage keeps what a damaged line still spells, under the tests
	// above. A hash held by no object is said once the line's other
	// hashes are known, since it may be what the damage made of one.
	salvage := func(l logLine) {
		// A damaged lost record names what was lost, not what to keep;
		// what it names and nothing kept is listed once the log is read.
		if bytes.Contains(l.bytes, []byte(`"op":"lost"`)) {
			return
		}
		tokens := hashToken.FindAll(l.bytes, -1)
		type unheld struct {
			e   string
			err error
		}
		var unhelds []unheld
		for _, h := range tokens {
			e := string(h)
			if kept[e] || appended[e] || onPath[e] || e == hdr.Base || listed[e] {
				continue
			}
			if err, lost := lostAfterLine(e, l); lost {
				dropSpelled(e, err)
				continue
			}
			if _, err := s.objs.read(spaceEntries, e); errors.Is(err, os.ErrNotExist) {
				listed[e] = true
				unhelds = append(unhelds, unheld{e, err})
				continue
			}
			parent, err := check(e)
			switch {
			case err != nil:
				err = fmt.Errorf("named only by a damaged record, and its objects do not read: %w", err)
			case parent == "" && hdr.Base != "":
				err = errors.New("named only by a damaged record, and its parent chain does not reach the session's base")
			case !kept[parent] && parent != hdr.Base:
				err = fmt.Errorf("named only by a damaged record, and its parent %s is not kept", parent)
			}
			if err != nil {
				dropSpelled(e, err)
				continue
			}
			keep(e, -1, logRecord{})
			p.salvaged = append(p.salvaged, e)
			// The head, unless a readable record after the line moved it.
			if bytes.Contains(l.bytes, []byte(`"head":"`+e+`"`)) && l.off > lines.lastHead {
				p.named, p.headAt = e, l.off
			}
		}
		for _, u := range unhelds {
			err := fmt.Errorf("named only by a damaged record, and no object of it is held: %w", u.err)
			if twin := oneDigitOff(u.e, tokens); twin != "" && kept[twin] {
				err = fmt.Errorf("no entry: the damage made it of %s, which is kept: %w", twin, u.err)
			}
			p.dropped = append(p.dropped, DroppedEntry{Entry: u.e, Err: err})
		}
	}
	p.named = hdr.Base
	next := 0 // the damaged lines read so far
	for i, r := range recs {
		for next < len(lines.damaged) && lines.damaged[next].before <= i {
			salvage(lines.damaged[next])
			next++
		}
		switch r.Op {
		case opHead:
			if !gone(r.Head, i) && !lostAfterRecord(r.Head, i) {
				p.named = r.Head
			}
			continue
		case opAppend:
		default:
			continue
		}
		if r.Entry == "" || gone(r.Entry, i) {
			continue
		}
		if lostAfterRecord(r.Entry, i) {
			continue
		}
		if r.Head != "" && !lostAfterRecord(r.Head, i) {
			p.named = r.Head
		}
		if seen[r.Entry] {
			continue
		}
		seen[r.Entry] = true
		if kept[r.Entry] {
			continue // kept already, as an ancestor an earlier entry named
		}
		drop := func(err error) { p.dropped = append(p.dropped, DroppedEntry{Entry: r.Entry, Err: err}) }
		parent, err := check(r.Entry)
		if err != nil {
			drop(err)
			continue
		}
		if !kept[parent] && parent != hdr.Base {
			if err := hidden(parent); err != nil {
				drop(err)
				continue
			}
		}
		keep(r.Entry, i, r)
	}
	for ; next < len(lines.damaged); next++ {
		salvage(lines.damaged[next])
	}
	// What a damaged lost record legibly names and nothing kept, an
	// append before it and nothing after, is accounted for as dropped.
	for _, e := range slices.Sorted(maps.Keys(lostBy)) {
		if !kept[e] && !onPath[e] && e != hdr.Base {
			dropSpelled(e, errDamagedLost)
		}
	}
	// An entry salvaged from its line that a readable record's entry
	// hangs from is one the damage hid, as the hidden rule would have
	// kept it had the line spelled nothing: the child's envelope is the
	// stronger evidence, and the report says so.
	if len(p.salvaged) > 0 {
		named := map[string]bool{}
		for e, parent := range parents {
			if seen[e] && kept[e] {
				named[parent] = true
			}
		}
		salvaged := p.salvaged[:0]
		for _, e := range p.salvaged {
			if named[e] {
				p.hidden = append(p.hidden, e)
			} else {
				salvaged = append(salvaged, e)
			}
		}
		p.salvaged = salvaged
	}
	if len(p.kept) == 0 {
		return p, nil
	}
	var prefix [][]byte
	if hdr.Base != "" {
		var err error
		if prefix, err = s.pathLines(hdr.Base); err != nil {
			return p, fmt.Errorf("cas: session %s: the path to its base: %w", id, err)
		}
	}
	own := make([][]byte, 0, len(p.kept))
	for _, e := range p.kept {
		line, err := s.loadLine(e)
		if err != nil {
			return p, fmt.Errorf("cas: session %s: %w", id, err)
		}
		own = append(own, line)
	}
	p.head = p.named
	if p.head != "" && p.head != hdr.Base && !kept[p.head] {
		// The head named was dropped: the latest kept leaf, one a head
		// may rest on, takes its place.
		sess, err := s.assemble(hdr, prefix, own, "")
		if err != nil {
			return p, fmt.Errorf("cas: session %s: what a repair keeps does not build: %w", id, err)
		}
		p.head = hdr.Base
		leaves := sess.Leaves()
		for i := len(leaves) - 1; i >= 0; i-- {
			e, _ := sess.Entry(leaves[i])
			if l, ok := e.(*agentsession.LabelEntry); ok && l.Label != nil && *l.Label == agentsession.LeafLabel {
				continue
			}
			if kept[leaves[i]] {
				p.head = leaves[i]
				break
			}
		}
	}
	if _, err := s.assemble(hdr, prefix, own, p.head); err != nil {
		return p, fmt.Errorf("cas: session %s: what a repair keeps does not build: %w", id, err)
	}
	return p, nil
}
