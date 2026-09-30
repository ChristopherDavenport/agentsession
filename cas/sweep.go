package cas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// keepSet is what the store holds, as a set of object names per space.
type keepSet struct {
	entries  map[string]bool
	contents map[string]bool
}

func (k keepSet) has(sp space, hash string) bool {
	if sp == spaceEntries {
		return k.entries[hash]
	}
	return k.contents[hash]
}

// keepEntry adds an entry, its content and the blobs its content names,
// following references down as RFC 0002's retention does. An object
// that is not there needs nothing kept; any other failure to read one
// fails the sweep, since what it would name cannot be known and a sweep
// that guessed would remove it.
//
// A torn entry is kept by name and not followed when torn is set: the
// append is one recovery cut, or will, so nothing reads what it names.
func (s *Store) keepEntry(k keepSet, id string, torn bool) error {
	if k.entries[id] {
		return nil
	}
	k.entries[id] = true
	env, err := s.objs.read(spaceEntries, id)
	if errors.Is(err, os.ErrNotExist) || (torn && errors.Is(err, ErrCorrupt)) {
		return nil
	}
	if err != nil {
		return err
	}
	c, ok := envelopeContent(env)
	if !ok {
		return fmt.Errorf("%w: entry %s names no content", ErrCorrupt, id)
	}
	k.contents[c] = true
	body, err := s.objs.read(spaceContents, c)
	if errors.Is(err, os.ErrNotExist) || (torn && errors.Is(err, ErrCorrupt)) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, b := range blobsNamedBy(body) {
		k.contents[b] = true
	}
	return nil
}

// keepPath adds the path to base.
func (s *Store) keepPath(k keepSet, base string) error {
	for id := base; id != "" && !k.entries[id]; {
		if err := s.keepEntry(k, id, false); err != nil {
			return err
		}
		parent, err := s.parentOf(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		id = parent
	}
	return nil
}

// keepRecords adds what log records name: the entries they append and
// the heads they set, and a created session's base. An append a crash
// may have left torn, one a lost record names or a lazy one no sync
// record covers yet, does not fail the sweep: recovery cuts it.
func (s *Store) keepRecords(k keepSet, recs []logRecord) error {
	synced := -1
	lostAt := map[string]int{}
	for i, r := range recs {
		switch r.Op {
		case opSync:
			synced = i
		case opLost:
			lostAt[r.Entry] = i
		}
	}
	for i, r := range recs {
		torn := false
		if r.Op == opAppend {
			at, lost := lostAt[r.Entry]
			torn = (lost && at > i) || (r.Lazy && i > synced)
		}
		for _, id := range []string{r.Entry, r.Head} {
			if id != "" {
				if err := s.keepEntry(k, id, torn); err != nil {
					return err
				}
			}
		}
		if r.Base != "" {
			if err := s.keepPath(k, r.Base); err != nil {
				return err
			}
		}
	}
	return nil
}

// logMarks is how far a sweep read each session's log, from which its
// last step reads what was accepted since.
// A log recovery cut shorter since, or wrote again as a new file, is
// read again whole.
type logMarks map[string]logMark

type logMark struct {
	off  int64
	file os.FileInfo
}

// keepAll computes what the store holds: what every session's log
// names, and the path to every session's base.
func (s *Store) keepAll() (keepSet, logMarks, error) {
	k := keepSet{entries: map[string]bool{}, contents: map[string]bool{}}
	marks := logMarks{}
	if err := s.keepLogs(k, marks); err != nil {
		return k, nil, err
	}
	return k, marks, nil
}

// keepLogs adds what each session's log names beyond the mark it has in
// marks, and moves the marks to where it read to. A session with no
// mark is read whole, its header's base with it.
func (s *Store) keepLogs(k keepSet, marks logMarks) error {
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || !validSessionID(d.Name()) {
			continue
		}
		id := d.Name()
		dir := filepath.Join(s.root, "sessions", id)
		mark, seen := marks[id]
		info, serr := os.Stat(filepath.Join(dir, logName))
		from := int64(0)
		if seen && serr == nil && os.SameFile(mark.file, info) && info.Size() >= mark.off {
			if info.Size() == mark.off {
				continue
			}
			from = mark.off
		}
		l, err := readSessionLog(dir, from)
		if err == nil && l.legacy {
			err = ErrLegacyStore
		}
		if err == nil && l.lost && tailLoss(l) {
			// Blocks a crash left unwritten in the uncommitted tail:
			// recovery cuts them as the tail's loss, and the lazy
			// appends after them name nothing a sweep need stop at.
			l.lost, l.whole = false, l.lossOff
		}
		if err == nil && l.lost {
			// What a damaged record named cannot be known, and a sweep
			// that guessed would remove it: RFC 0002 forbids deleting an
			// entry any log references.
			err = firstLoss(l)
		}
		if err != nil {
			return fmt.Errorf("cas: session %s: %w", id, err)
		}
		if err := s.keepRecords(k, l.recs); err != nil {
			return fmt.Errorf("cas: sweep: %w", err)
		}
		if from == 0 {
			if h, err := readHeader(dir); err == nil && h.Base != "" {
				if err := s.keepPath(k, h.Base); err != nil {
					return fmt.Errorf("cas: sweep: %w", err)
				}
			}
		}
		marks[id] = logMark{off: l.whole, file: info}
	}
	return nil
}

// gcLock takes the lock that keeps sweeps and packs apart.
func (s *Store) gcLock() (*dirLock, error) {
	lk, err := lockFile(filepath.Join(s.root, "gc.lock"))
	if errors.Is(err, ErrSessionLocked) {
		return nil, ErrSweepRunning
	}
	return lk, err
}

// sweepLock takes the sweep's lock exclusive, waiting for the writers
// holding it shared to reach their commits, until ctx ends.
func (s *Store) sweepLock(ctx context.Context) (*dirLock, error) {
	return lockExclusive(ctx, filepath.Join(s.root, "sweep.lock"))
}

// Pack moves the store's loose objects into one new pack and removes the
// loose copies, as git's repack does without -a: nothing is dropped, so
// it needs no grace and takes no lock a writer waits on. Then it merges
// the smallest packs, as git's geometric repack does, until each pack
// is at least twice the size of all the packs smaller than it together,
// so a store holds a number of packs that grows with the logarithm of
// its size and each object is rewritten a logarithmic number of times.
// It returns how many loose objects it packed.
func (s *Store) Pack(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.readOnly {
		return 0, errReadOnly()
	}
	gc, err := s.gcLock()
	if err != nil {
		return 0, err
	}
	defer gc.release()
	n, err := s.packLoose(ctx)
	if err != nil {
		return n, err
	}
	return n, s.consolidate(ctx)
}

// packLoose is Pack's first step, under the gc lock.
func (s *Store) packLoose(ctx context.Context) (int, error) {
	var objs []packObject
	paths := map[objKey]string{}
	for _, sp := range []space{spaceEntries, spaceContents} {
		err := s.objs.eachLoose(sp, func(hash, path string, _ os.FileInfo, tmp bool) error {
			if tmp {
				return nil
			}
			objs = append(objs, packObject{sp: sp, hash: hash, load: func() ([]byte, error) { return os.ReadFile(path) }})
			paths[objKey{sp, hash}] = path
			return ctx.Err()
		})
		if err != nil {
			return 0, err
		}
	}
	if len(objs) == 0 {
		return 0, nil
	}
	// One that is corrupt, or removed as we walked, is left out and left
	// where it is, for Verify to report.
	_, skipped, err := writePackSkipping(s.objs.packDir(), objs, nil)
	if err != nil {
		return 0, fmt.Errorf("cas: pack: %w", err)
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}
	for _, o := range skipped {
		delete(paths, objKey{o.sp, o.hash})
	}
	// The pack is durable; a loose copy is now a duplicate, and a reader
	// that finds it gone looks in the packs.
	for _, p := range paths {
		os.Remove(p)
	}
	return len(paths), nil
}

// consolidate merges the smallest packs into one while the next is less
// than twice their size together, under the gc lock. The merged pack
// holds every object the packs it replaces did, so a reader holding one
// of those still reads, and one that looks again finds the merged pack.
// A pack holding an object that fails its name is not removed.
func (s *Store) consolidate(ctx context.Context) error {
	type sized struct {
		p    *pack
		size int64
	}
	var packs []sized
	for _, p := range s.objs.packList() {
		if info, err := os.Stat(p.path); err == nil {
			packs = append(packs, sized{p, info.Size()})
		}
	}
	sort.Slice(packs, func(i, j int) bool { return packs[i].size < packs[j].size })
	m, acc := 1, int64(0)
	if len(packs) > 0 {
		acc = packs[0].size
	}
	for m < len(packs) && packs[m].size < 2*acc {
		acc += packs[m].size
		m++
	}
	if m < 2 {
		return nil
	}
	var objs []packObject
	for _, sp := range packs[:m] {
		p := sp.p
		err := p.each(func(sp space, hash string, off, length int64) error {
			objs = append(objs, packObject{sp: sp, hash: hash, load: func() ([]byte, error) { return p.read(off, length) }})
			return nil
		})
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	name, skipped, err := writePackSkipping(s.objs.packDir(), objs, nil)
	if err != nil {
		return fmt.Errorf("cas: pack: %w", err)
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return err
	}
	if len(skipped) > 0 {
		return nil // the old packs stay, with what Verify will report
	}
	for _, sp := range packs[:m] {
		if sp.p.name == name {
			continue
		}
		os.Remove(filepath.Join(s.objs.packDir(), sp.p.name+".idx"))
		os.Remove(sp.p.path)
	}
	if err := syncDir(s.objs.packDir()); err != nil {
		return err
	}
	return s.objs.reloadPacks(true)
}

// Sweep collects garbage as git's gc does: it repacks every object the
// store holds into one pack, drops what nothing needs, and removes the
// loose copies and the packs it replaced. What is held follows
// references down: an envelope is kept while a log or a
// prefix names it, a content while a kept envelope names it, a media
// blob while a kept content names it. It works from every session's
// log, which is where an append is accepted.
//
// It runs beside live writers, in this process and others. The keep set
// and the new pack are built without any lock a writer waits on and
// without the store's own mutex. Then the sweep's lock is taken
// exclusive, waiting until ctx ends for writers between an object write
// and its acceptance, and held only for a short last step: each log is
// read from where the keep set stopped, anything accepted since that
// lies only in a replaced pack is written back loose, and the replaced
// packs go. Loose objects nothing needs are removed after, a batch at a
// time under the same lock, each checked again for a writer that
// freshened it.
//
// A loose object younger than grace is spared whether or not anything
// names it, as git spares a young loose object, and so is an unneeded
// object of a pack younger than grace, which is written back loose with
// the pack's age. grace must exceed the longest interval any live
// writer holds between writing an object and accepting its record; a
// grace of zero is therefore safe only when no writer is active, as git
// says of pruning with an expiry of now. An hour is a reasonable grace
// for a live store. It returns how many objects went.
func (s *Store) Sweep(ctx context.Context, grace time.Duration) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if s.readOnly {
		return 0, errReadOnly()
	}
	gc, err := s.gcLock()
	if err != nil {
		return 0, err
	}
	defer gc.release()
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}
	started := time.Now()
	keep, marks, err := s.keepAll()
	if err != nil {
		return 0, err
	}
	young := started.Add(-grace)

	// What the new pack takes: every kept object, loose or packed, by
	// where it is; its bytes are read as the pack is written. A second
	// copy is kept as a fallback, taken if the first fails its name.
	var objs []packObject
	alts := map[objKey][]packObject{}
	inNew := map[space]map[string]bool{spaceEntries: {}, spaceContents: {}}
	type candidate struct {
		path string
		tmp  bool
	}
	var prune []candidate // loose, unneeded and old
	var packedLoose []string
	add := func(o packObject) {
		if inNew[o.sp][o.hash] {
			k := objKey{o.sp, o.hash}
			alts[k] = append(alts[k], o)
			return
		}
		inNew[o.sp][o.hash] = true
		objs = append(objs, o)
	}
	loosePaths := map[objKey]string{}
	for _, sp := range []space{spaceEntries, spaceContents} {
		err := s.objs.eachLoose(sp, func(hash, path string, info os.FileInfo, tmp bool) error {
			if tmp {
				if info.ModTime().Before(young) {
					prune = append(prune, candidate{path, true})
				}
				return nil
			}
			if !keep.has(sp, hash) {
				if info.ModTime().Before(young) {
					prune = append(prune, candidate{path: path})
				}
				return nil
			}
			add(packObject{sp: sp, hash: hash, load: func() ([]byte, error) { return os.ReadFile(path) }})
			loosePaths[objKey{sp, hash}] = path
			return ctx.Err()
		})
		if err != nil {
			return 0, err
		}
	}
	old := s.objs.packList()
	scanned := map[string]time.Time{} // each old pack's time when read
	dropped := 0
	type rescue struct {
		sp    space
		hash  string
		data  []byte
		mtime time.Time
	}
	var youngOrphans []rescue
	for _, p := range old {
		info, err := os.Stat(p.path)
		if err != nil {
			continue
		}
		scanned[p.name] = info.ModTime()
		err = p.each(func(sp space, hash string, off, length int64) error {
			if keep.has(sp, hash) {
				add(packObject{sp: sp, hash: hash, load: func() ([]byte, error) { return p.read(off, length) }})
				return nil
			}
			if info.ModTime().After(young) {
				data, err := p.read(off, length)
				if err != nil || hashBytes(data) != hash {
					return nil
				}
				youngOrphans = append(youngOrphans, rescue{sp, hash, data, info.ModTime()})
				return nil
			}
			dropped++
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	newPack, skipped, err := writePackSkipping(s.objs.packDir(), objs, alts)
	if err != nil {
		return 0, fmt.Errorf("cas: pack: %w", err)
	}
	// An object no copy of which reads is not in the new pack: it is
	// looked for again below like anything else the pack lacks, and its
	// loose copy stays for Verify.
	for _, o := range skipped {
		delete(inNew[o.sp], o.hash)
		delete(loosePaths, objKey{o.sp, o.hash})
	}
	for _, path := range loosePaths {
		packedLoose = append(packedLoose, path)
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}
	writeLoose := func(sp space, hash string, data []byte, mtime time.Time, replace bool) error {
		path, err := s.objs.loosePath(sp, hash)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil && !replace {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := writeAtomic(path, data); err != nil {
			return err
		}
		if !mtime.IsZero() {
			return os.Chtimes(path, mtime, mtime)
		}
		return nil
	}
	// A young unneeded object of a replaced pack goes back loose, with
	// the pack's age, so it keeps its grace and still expires.
	for _, r := range youngOrphans {
		if err := writeLoose(r.sp, r.hash, r.data, r.mtime, false); err != nil {
			return 0, err
		}
	}

	// The last step, under the sweep's lock: no writer is between an
	// object write and its record.
	lk, err := s.sweepLock(ctx)
	if err != nil {
		return 0, err
	}
	since := keepSet{entries: map[string]bool{}, contents: map[string]bool{}}
	if err := s.keepLogs(since, marks); err != nil {
		lk.release()
		return 0, err
	}
	for _, pair := range []struct {
		sp  space
		set map[string]bool
	}{{spaceEntries, since.entries}, {spaceContents, since.contents}} {
		for hash := range pair.set {
			if inNew[pair.sp][hash] {
				continue
			}
			if lp, err := s.objs.loosePath(pair.sp, hash); err == nil {
				if cur, err := os.ReadFile(lp); err == nil && hashBytes(cur) == hash {
					continue
				}
			}
			// Only in a replaced pack, or loose and corrupt: written back
			// loose, from the pack, before it goes.
			data, err := s.objs.readPacked(pair.sp, hash)
			if errors.Is(err, os.ErrNotExist) {
				continue // not held at all; nothing to rescue
			}
			if err == nil {
				err = writeLoose(pair.sp, hash, data, time.Time{}, true)
			}
			if err != nil {
				lk.release()
				return 0, fmt.Errorf("cas: sweep: %w", err)
			}
		}
	}
	// A pack a writer freshened since the sweep read it holds an object
	// that writer found there and needed, perhaps a blob no record names
	// yet: what the new pack lacks goes back loose, young.
	for _, p := range old {
		if p.name == newPack {
			continue
		}
		if info, err := os.Stat(p.path); err != nil || info.ModTime().Equal(scanned[p.name]) {
			continue
		}
		err := p.each(func(sp space, hash string, off, length int64) error {
			if inNew[sp][hash] {
				return nil
			}
			data, err := p.read(off, length)
			if err != nil || hashBytes(data) != hash {
				return nil
			}
			return writeLoose(sp, hash, data, time.Time{}, false)
		})
		if err != nil {
			lk.release()
			return 0, err
		}
	}
	for _, p := range old {
		if p.name == newPack {
			continue
		}
		os.Remove(filepath.Join(s.objs.packDir(), p.name+".idx"))
		os.Remove(p.path)
	}
	syncDir(s.objs.packDir())
	lk.release()
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}

	// Loose copies of what the new pack holds are duplicates.
	for _, p := range packedLoose {
		os.Remove(p)
	}
	// Unneeded loose objects, a batch at a time: a writer that reused one
	// after the keep set was taken freshened it, and the second stat
	// under the lock sees that; one that reuses it after the remove finds
	// it gone and writes it again.
	const batch = 256
	for i := 0; i < len(prune); i += batch {
		if err := ctx.Err(); err != nil {
			return dropped, err
		}
		lk, err := s.sweepLock(ctx)
		if err != nil {
			return dropped, err
		}
		for _, c := range prune[i:min(i+batch, len(prune))] {
			again, err := os.Stat(c.path)
			if err != nil || again.ModTime().After(young) {
				continue
			}
			if !c.tmp && isKeptSince(s, c.path, since) {
				continue
			}
			if err := os.Remove(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
				lk.release()
				return dropped, err
			}
			if !c.tmp {
				dropped++
			}
		}
		lk.release()
	}
	s.cleanPackDir(young)
	s.cleanStaging(young)
	return dropped, nil
}

// cleanStaging removes what a crash left staged or discarded: a session
// created or imported in tmp and never renamed into place, and one a
// delete renamed into trash and did not finish removing. Neither is
// read; each goes once older than young, so a create still staging is
// left to finish.
func (s *Store) cleanStaging(young time.Time) {
	for _, dir := range []string{"tmp", "trash"} {
		ents, _ := os.ReadDir(filepath.Join(s.root, dir))
		for _, e := range ents {
			p := filepath.Join(s.root, dir, e.Name())
			if info, err := os.Stat(p); err == nil && info.ModTime().Before(young) {
				os.RemoveAll(p)
			}
		}
	}
}

// isKeptSince reports whether a loose object's path names an object the
// logs named after the keep set was taken.
func isKeptSince(s *Store, path string, since keepSet) bool {
	for _, sp := range []space{spaceEntries, spaceContents} {
		dir := s.objs.spaceDir(sp)
		rel, err := filepath.Rel(dir, path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		return since.has(sp, "sha256:"+strings.ReplaceAll(rel, string(filepath.Separator), ""))
	}
	return false
}

// cleanPackDir removes what an interrupted pack write left: temporary
// files and packs with no index, once they are old.
func (s *Store) cleanPackDir(young time.Time) {
	ents, err := os.ReadDir(s.objs.packDir())
	if err != nil {
		return
	}
	idx := map[string]bool{}
	for _, e := range ents {
		if name, ok := strings.CutSuffix(e.Name(), ".idx"); ok {
			idx[name] = true
		}
	}
	for _, e := range ents {
		info, err := e.Info()
		if err != nil || info.ModTime().After(young) {
			continue
		}
		name, isPack := strings.CutSuffix(e.Name(), ".pack")
		if strings.HasPrefix(e.Name(), ".tmp-") || (isPack && !idx[name]) {
			os.Remove(filepath.Join(s.objs.packDir(), e.Name()))
		}
	}
}

// envelopeContent reads the content hash out of raw envelope bytes.
func envelopeContent(env []byte) (string, bool) {
	var e struct {
		Content string `json:"content"`
	}
	if json.Unmarshal(env, &e) != nil || e.Content == "" {
		return "", false
	}
	return e.Content, true
}

// autoPackLoose is the estimated count of loose objects past which a
// writing store packs on its own, as git's gc.auto is. A variable so
// tests can lower it.
var autoPackLoose = 4096

// autoPackEvery is how many appends a store makes between looks at
// whether to pack.
const autoPackEvery = 1024

// looseEstimate estimates the store's loose objects as git does, from
// one of the 256 fan-out directories of each space.
func (s *Store) looseEstimate() int {
	n := 0
	for _, sp := range []space{spaceEntries, spaceContents} {
		ents, _ := os.ReadDir(filepath.Join(s.objs.spaceDir(sp), "17"))
		n += len(ents)
	}
	return n * 256
}

// maybePack packs when the loose objects look to have passed
// autoPackLoose. A pack that cannot run, because a sweep, pack or
// another pack holds the gc lock, is left for the next look; its failure
// is no failure of the caller's.
func (s *Store) maybePack() {
	if s.readOnly || s.looseEstimate() < autoPackLoose {
		return
	}
	_, _ = s.Pack(context.Background())
}
