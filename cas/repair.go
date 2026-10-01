package cas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	// Dropped is each entry a readable record appends that the repaired
	// log does not hold, and why.
	Dropped []DroppedEntry
	// Head is the repaired session's head. Named is the head the damaged
	// log's last readable head record named; when Head differs, that
	// entry was dropped and Head is the latest kept leaf.
	Head, Named string
	// Mark is the record mark, as the last readable mark record says.
	Mark string
	// DamagedLog is where the damaged log is kept, in the session's
	// directory; empty on a dry run.
	DamagedLog string
}

// DroppedEntry is an entry a repair dropped.
type DroppedEntry struct {
	Entry string
	Err   error
}

// Repair rewrites a session's damaged log from the records that still
// read, holding the session's lock. It keeps every record that passes
// its checksum, and of the entries those append, the ones whose objects
// are present and hash to their names and whose parent is the session's
// base, or no parent in a session without one, or a kept entry; an
// entry hanging from a dropped one is dropped too. The head is the kept
// entry the last readable head record names, or else the latest kept
// leaf, and the mark is the last readable mark record's. The new log is
// written durably and renamed into place; the damaged one is kept in
// the session's directory as damaged-<time>, which Verify reports until
// someone removes it, and while it is there a sweep keeps everything its
// readable records name. Damage in the uncommitted tail, past a block a
// crash left unwritten, is no damage here: the tail is not kept, as
// recovery would not keep it.
//
// Repair refuses a session that is not damaged with ErrNotDamaged, one
// this store holds open, which is released first, and a read-only or
// stopped store, except for a dry run, which writes nothing and takes no
// lock on a read-only store. When nothing can be kept it returns
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
	recs, damage := l.recs, l.damage
	if tailLoss(l) {
		// Past a block a crash left unwritten nothing was committed:
		// the tail is cut, as recovery cuts it, and is no damage.
		recs, damage = recs[:l.lossRec], nil
		for _, d := range l.damage {
			if d.Offset < l.lossOff {
				damage = append(damage, d)
			}
		}
	}
	if len(damage) == 0 {
		return rep, fmt.Errorf("%w: session %s", ErrNotDamaged, id)
	}
	rep.Damage = damage

	plan, err := s.planRepair(id, hdr, recs)
	if err != nil {
		return rep, err
	}
	rep.Kept, rep.Dropped, rep.Head, rep.Named = plan.kept, plan.dropped, plan.head, plan.named
	rep.Mark = plan.mark
	if rep.Mark == "" {
		rep.Mark = readMark(dir)
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
	rep.DamagedLog = aside
	if again, err := os.ReadFile(path); err != nil || !bytes.Equal(again, data) {
		return rep, fmt.Errorf("cas: session %s: the log changed as repair read it", id)
	}
	if err := s.objs.writeAtomic(path, fresh); err != nil {
		return rep, fmt.Errorf("cas: session %s: %w", id, err)
	}
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

// repairPlan is what a repair keeps of a log's readable records.
type repairPlan struct {
	kept        []string
	dropped     []DroppedEntry
	sizes       map[string]int64
	uncommitted []string // kept appends no readable commit covers
	head, named string
	mark        string
	path        []string // the path to the base, base first
}

// planRepair works out what a repair keeps of a log's readable records.
func (s *Store) planRepair(id string, hdr agentsession.Header, recs []logRecord) (repairPlan, error) {
	p := repairPlan{sizes: map[string]int64{}}
	synced := -1
	lostAt := map[string]int{}
	for i, r := range recs {
		switch r.Op {
		case opSync:
			synced = i
		case opLost:
			lostAt[r.Entry] = i
		case opMark:
			p.mark = r.Mark
		}
	}
	gone := func(entry string, i int) bool {
		at, ok := lostAt[entry]
		return ok && at > i
	}
	kept := map[string]bool{}
	seen := map[string]bool{}
	p.named = hdr.Base
	for i, r := range recs {
		switch r.Op {
		case opHead:
			if !gone(r.Head, i) {
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
		if r.Head != "" {
			p.named = r.Head
		}
		if seen[r.Entry] {
			continue
		}
		seen[r.Entry] = true
		drop := func(err error) { p.dropped = append(p.dropped, DroppedEntry{Entry: r.Entry, Err: err}) }
		if _, err := s.loadLine(r.Entry); err != nil {
			drop(err)
			continue
		}
		parent, err := s.parentOf(r.Entry)
		if err != nil {
			drop(err)
			continue
		}
		if !kept[parent] && parent != hdr.Base {
			if seen[parent] {
				drop(fmt.Errorf("its parent %s was dropped", parent))
			} else {
				drop(fmt.Errorf("its parent %s is not in the session's readable records", parent))
			}
			continue
		}
		kept[r.Entry] = true
		p.kept = append(p.kept, r.Entry)
		if r.Size > 0 {
			p.sizes[r.Entry] = r.Size
		}
		if r.Lazy && i > synced {
			p.uncommitted = append(p.uncommitted, r.Entry)
		}
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
		if p.path, err = s.pathTo(hdr.Base); err != nil {
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
