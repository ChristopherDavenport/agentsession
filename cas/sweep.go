package cas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
func (s *Store) keepEntry(k keepSet, id string) error {
	if k.entries[id] {
		return nil
	}
	k.entries[id] = true
	env, err := s.objs.read(spaceEntries, id)
	if errors.Is(err, os.ErrNotExist) {
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
	if errors.Is(err, os.ErrNotExist) {
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
		if err := s.keepEntry(k, id); err != nil {
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

// keepFromScan adds what the journal records name: every session's own
// entries and head, and the base of every session created in it.
func (s *Store) keepFromScan(k keepSet, scan *journalScan) error {
	for _, st := range scan.states {
		if st.deleted {
			continue
		}
		if st.base != "" {
			if err := s.keepPath(k, st.base); err != nil {
				return err
			}
		}
		for _, r := range st.recs {
			for _, id := range []string{r.Entry, r.Head} {
				if id != "" {
					if err := s.keepEntry(k, id); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// keepAll computes what the store holds: what the journal names, what
// every log names, and the path to every session's base. It returns
// the journal offset it read to, from which the sweep's last step reads
// what was committed since.
func (s *Store) keepAll() (keepSet, int64, error) {
	k := keepSet{entries: map[string]bool{}, contents: map[string]bool{}}
	scan, err := s.replay()
	if err != nil {
		return k, 0, err
	}
	if err := s.keepFromScan(k, scan); err != nil {
		return k, 0, fmt.Errorf("cas: sweep: %w", err)
	}
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return k, 0, fmt.Errorf("cas: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || !validSessionID(d.Name()) {
			continue
		}
		if st := scan.states[d.Name()]; st != nil && st.deleted {
			continue
		}
		dir := filepath.Join(s.root, "sessions", d.Name())
		hashes, _, err := readLog(dir)
		if err != nil {
			return k, 0, fmt.Errorf("cas: session %s: %w", d.Name(), err)
		}
		if head, err := readHead(dir); err == nil && head != "" {
			hashes = append(hashes, head)
		}
		for _, h := range hashes {
			if err := s.keepEntry(k, h); err != nil {
				return k, 0, fmt.Errorf("cas: sweep: %w", err)
			}
		}
		if h, err := readHeader(dir); err == nil && h.Base != "" {
			if err := s.keepPath(k, h.Base); err != nil {
				return k, 0, fmt.Errorf("cas: sweep: %w", err)
			}
		}
	}
	return k, scan.end, nil
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
// it needs no grace and takes no lock a writer waits on. It returns how
// many objects it packed.
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
	var objs []packObject
	var paths []string
	for _, sp := range []space{spaceEntries, spaceContents} {
		err := s.objs.eachLoose(sp, func(hash, path string, _ os.FileInfo, tmp bool) error {
			if tmp {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil // removed as we walked
			}
			if hashBytes(data) != hash {
				return nil // corrupt: left for Verify to report, never packed
			}
			objs = append(objs, packObject{sp, hash, data})
			paths = append(paths, path)
			return ctx.Err()
		})
		if err != nil {
			return 0, err
		}
	}
	if len(objs) == 0 {
		return 0, nil
	}
	if _, err := writePack(s.objs.packDir(), objs); err != nil {
		return 0, fmt.Errorf("cas: pack: %w", err)
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}
	// The pack is durable; a loose copy is now a duplicate, and a reader
	// that finds it gone looks in the packs.
	for _, p := range paths {
		os.Remove(p)
	}
	return len(objs), nil
}

// Sweep collects garbage as git's gc does: it repacks every object the
// store holds into one pack, drops what nothing needs, and removes the
// loose copies and the packs it replaced. What is held follows
// references down: an envelope is kept while the journal, a log or a
// prefix names it, a content while a kept envelope names it, a media
// blob while a kept content names it. It works from the journal, so an
// append whose log line never reached disk is kept.
//
// It runs beside live writers, in this process and others. The keep set
// and the new pack are built without any lock a writer waits on and
// without the store's own mutex. Then the sweep's lock is taken
// exclusive, waiting until ctx ends for writers between an object write
// and its commit, and held only for a short last step: the journal is
// read from where the keep set stopped, anything committed since that
// lies only in a replaced pack is written back loose, and the replaced
// packs go. Loose objects nothing needs are removed after, a batch at a
// time under the same lock, each checked again for a writer that
// freshened it.
//
// A loose object younger than grace is spared whether or not anything
// names it, as git spares a young loose object, and so is an unneeded
// object of a pack younger than grace, which is written back loose with
// the pack's age. grace must exceed the longest interval any live
// writer holds between writing an object and committing its record; a
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
	keep, offset, err := s.keepAll()
	if err != nil {
		return 0, err
	}
	young := started.Add(-grace)

	// What the new pack takes: every kept object, loose or packed.
	var objs []packObject
	inNew := map[space]map[string]bool{spaceEntries: {}, spaceContents: {}}
	type candidate struct {
		path string
		tmp  bool
	}
	var prune []candidate // loose, unneeded and old
	var packedLoose []string
	add := func(sp space, hash string, data []byte) {
		if !inNew[sp][hash] {
			inNew[sp][hash] = true
			objs = append(objs, packObject{sp, hash, data})
		}
	}
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
			data, err := os.ReadFile(path)
			if err != nil || hashBytes(data) != hash {
				return nil // gone, or corrupt and left for Verify
			}
			add(sp, hash, data)
			packedLoose = append(packedLoose, path)
			return ctx.Err()
		})
		if err != nil {
			return 0, err
		}
	}
	old := s.objs.packList()
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
		err = p.each(func(sp space, hash string, off, length int64) error {
			if inNew[sp][hash] {
				return nil
			}
			data, err := p.read(off, length)
			if err != nil || hashBytes(data) != hash {
				return nil
			}
			if keep.has(sp, hash) {
				add(sp, hash, data)
			} else if info.ModTime().After(young) {
				youngOrphans = append(youngOrphans, rescue{sp, hash, data, info.ModTime()})
			} else {
				dropped++
			}
			return nil
		})
		if err != nil {
			return 0, err
		}
	}
	newPack, err := writePack(s.objs.packDir(), objs)
	if err != nil {
		return 0, fmt.Errorf("cas: pack: %w", err)
	}
	if err := s.objs.reloadPacks(true); err != nil {
		return 0, err
	}
	writeLoose := func(sp space, hash string, data []byte, mtime time.Time) error {
		path, err := s.objs.loosePath(sp, hash)
		if err != nil {
			return err
		}
		if _, err := os.Stat(path); err == nil {
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
		if err := writeLoose(r.sp, r.hash, r.data, r.mtime); err != nil {
			return 0, err
		}
	}

	// The last step, under the sweep's lock: no writer is between an
	// object write and its commit.
	lk, err := s.sweepLock(ctx)
	if err != nil {
		return 0, err
	}
	scan, err := s.replayFrom(offset)
	if err != nil {
		lk.release()
		return 0, err
	}
	since := keepSet{entries: map[string]bool{}, contents: map[string]bool{}}
	if err := s.keepFromScan(since, scan); err != nil {
		lk.release()
		return 0, fmt.Errorf("cas: sweep: %w", err)
	}
	for _, pair := range []struct {
		sp  space
		set map[string]bool
	}{{spaceEntries, since.entries}, {spaceContents, since.contents}} {
		for hash := range pair.set {
			if inNew[pair.sp][hash] {
				continue
			}
			keep.entries[hash] = keep.entries[hash] || pair.sp == spaceEntries
			if pair.sp == spaceContents {
				keep.contents[hash] = true
			}
			if lp, err := s.objs.loosePath(pair.sp, hash); err == nil {
				if _, err := os.Stat(lp); err == nil {
					continue
				}
			}
			// Only in a replaced pack: written back loose before it goes.
			data, err := s.objs.read(pair.sp, hash)
			if err != nil {
				continue // not held at all; nothing to rescue
			}
			if err := writeLoose(pair.sp, hash, data, time.Time{}); err != nil {
				lk.release()
				return 0, err
			}
		}
	}
	// A pack a writer freshened since the sweep began holds an object
	// that writer found there and needed, perhaps a blob no record names
	// yet: what the new pack lacks goes back loose, young.
	for _, p := range old {
		if p.name == newPack {
			continue
		}
		if info, err := os.Stat(p.path); err != nil || !info.ModTime().After(started) {
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
			return writeLoose(sp, hash, data, time.Time{})
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
	return dropped, nil
}

// isKeptSince reports whether a loose object's path names an object the
// journal named after the keep set was taken.
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
