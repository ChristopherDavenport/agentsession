package cas

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// ErrCorrupt is returned for an object whose bytes do not hash to its
// name, and for a pack or index that fails its checksum. RFC 0002 has a
// store refuse to serve such an object; the error names it and where it
// lies, so the damage is found in one place rather than as a bad line of
// every session that uses it.
var ErrCorrupt = errors.New("cas: corrupt object")

func hashBytes(b []byte) string { return agentsession.HashBytes(b) }

// objects is the store's object database: loose objects, one file each,
// and packs. A loose object is written first and packed later, as git
// does; a lookup tries the loose file and then the packs, and when both
// miss it reloads the pack list once, since another process may have
// packed the object and removed the loose copy in between.
type objects struct {
	root string

	mu       sync.Mutex
	packs    []*pack
	packStat time.Time // the pack directory's mtime when the list was read
	// bad records packs whose index does not read, reported by Verify
	// and skipped, so one damaged index does not close the store.
	bad map[string]error

	// Written but not yet fsynced, under a lazy sync policy: files and
	// the directories they were renamed into.
	pendFiles map[string]bool
	pendDirs  map[string]bool
	// pendRenamed holds the objects written durably whose directories
	// are yet to be synced, which a failed sync of one writes again.
	pendRenamed map[string]bool
	// fanned holds the spaces whose fan-out directories this store has
	// seen made durable; fanning makes them, one space at a time.
	fanned  map[string]bool
	fanning sync.Mutex
	// rewrite holds pending objects an fsync failed on, which the next
	// flush writes again rather than fsyncs again.
	rewrite map[string]bool
	// checked holds the large loose files this store fsynced, after
	// writing them or finding them to hold their object's bytes. Their
	// bytes are on the disk, so an eviction since reads them back; and
	// an object's file is replaced only by renaming a new one over it,
	// so the same file still holds them, unless an fsync of it has
	// failed since. A file not yet fsynced is not taken on trust: its
	// writeback may fail in the background, and its pages be evicted
	// and read back as zeros, before any fsync reports it. A file is
	// known by its version, change time included, since a file made
	// after it is removed may take over its inode.
	checked map[string]fileVersion
}

// fileVersion is one version of a file, as versionOf reads it.
type fileVersion struct {
	dev, ino    uint64
	ctime, size int64
}

// checkedSize is the least size of a loose object whose check is
// remembered; a smaller one is read and compared in microseconds.
const checkedSize = 64 << 10

func newObjects(root string) *objects {
	return &objects{root: root, pendFiles: map[string]bool{}, pendDirs: map[string]bool{}, pendRenamed: map[string]bool{}, fanned: map[string]bool{}, rewrite: map[string]bool{}, checked: map[string]fileVersion{}}
}

func (o *objects) packDir() string { return filepath.Join(o.root, "objects", "pack") }

func (o *objects) spaceDir(sp space) string {
	if sp == spaceEntries {
		return filepath.Join(o.root, "objects", "entries")
	}
	return filepath.Join(o.root, "objects", "contents")
}

func (o *objects) loosePath(sp space, hash string) (string, error) {
	return objectPath(o.spaceDir(sp), hash)
}

// rel is an object's path relative to the root, for error messages.
func (o *objects) rel(path string) string {
	if r, err := filepath.Rel(o.root, path); err == nil {
		return r
	}
	return path
}

// reloadPacks reads the pack list again when the directory changed
// since the last read, or always when force is set, since a pack added
// within the directory's timestamp granularity leaves its mtime alone.
func (o *objects) reloadPacks(force bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	info, err := os.Stat(o.packDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force && info.ModTime().Equal(o.packStat) && o.packs != nil {
		return nil
	}
	ents, err := os.ReadDir(o.packDir())
	if err != nil {
		return err
	}
	have := map[string]*pack{}
	for _, p := range o.packs {
		have[p.name] = p
	}
	var next []*pack
	seen := map[string]bool{}
	for _, e := range ents {
		name, ok := strings.CutSuffix(e.Name(), ".idx")
		if !ok || !strings.HasPrefix(name, "pack-") {
			continue
		}
		seen[name] = true
		if p, ok := have[name]; ok {
			next = append(next, p)
			continue
		}
		p, err := openPack(o.packDir(), name)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // removed by a sweep as this was reading
			}
			if o.bad == nil {
				o.bad = map[string]error{}
			}
			o.bad[name] = err
			continue
		}
		delete(o.bad, name)
		next = append(next, p)
	}
	// A pack dropped from the list is closed once no read is in it; a
	// read that located it then finds it closed and looks again.
	for name, p := range have {
		if !seen[name] {
			p.close()
		}
	}
	sort.Slice(next, func(i, j int) bool { return next[i].name < next[j].name })
	o.packs = next
	o.packStat = info.ModTime()
	return nil
}

func (o *objects) packList() []*pack {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]*pack(nil), o.packs...)
}

// locate finds an object: the pack holding it, or its loose path. The
// packs' indexes are in memory and are looked in first, so a read from
// a packed store costs no stat of a loose path that is not there. A
// miss reloads the pack list if the directory changed and looks again;
// with sure set, a miss then forces a reload and looks a third time, so
// a pack another process wrote within the directory's timestamp
// granularity is found. A read, and anything that decides an object is
// gone, is sure; a writer asking whether to write a copy need not be,
// since a duplicate is harmless.
func (o *objects) locate(sp space, hash string, sure bool) (loose string, p *pack, off, length int64, err error) {
	path, err := o.loosePath(sp, hash)
	if err != nil {
		return "", nil, 0, 0, err
	}
	d, err := digestOf(hash)
	if err != nil {
		return "", nil, 0, 0, err
	}
	attempts := 2
	if sure {
		attempts = 3
	}
	for attempt := 0; attempt < attempts; attempt++ {
		if attempt > 0 {
			if err := o.reloadPacks(attempt > 1); err != nil {
				return "", nil, 0, 0, err
			}
		}
		for _, p := range o.packList() {
			if off, length, ok := p.find(sp, d); ok {
				return "", p, off, length, nil
			}
		}
		if _, err := os.Stat(path); err == nil {
			return path, nil, 0, 0, nil
		}
	}
	return "", nil, 0, 0, os.ErrNotExist
}

// read returns an object's bytes, checked against its name.
func (o *objects) read(sp space, hash string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		loose, p, off, length, err := o.locate(sp, hash, true)
		if err != nil {
			return nil, fmt.Errorf("cas: %s %s: %w", sp, hash, err)
		}
		var data []byte
		where := ""
		if loose != "" {
			data, err = os.ReadFile(loose)
			where = o.rel(loose)
			if errors.Is(err, os.ErrNotExist) && attempt < 3 {
				continue // packed and removed between the stat and the read
			}
		} else {
			data, err = p.read(off, length)
			where = o.rel(p.path)
			if errors.Is(err, errPackClosed) && attempt < 3 {
				continue // dropped since it was located; look again
			}
		}
		if err != nil {
			return nil, fmt.Errorf("cas: %s %s: %w", sp, hash, err)
		}
		if hashBytes(data) != hash {
			// A copy that fails may have a good one elsewhere: in another
			// pack, or loose, as a sweep writes back.
			if good, ok := o.otherCopy(sp, hash); ok {
				return good, nil
			}
			return nil, fmt.Errorf("%w: %s object %s (%s)", ErrCorrupt, sp, hash, where)
		}
		return data, nil
	}
}

// otherCopy looks through every pack and the loose path for a copy of
// an object that matches its name.
func (o *objects) otherCopy(sp space, hash string) ([]byte, bool) {
	d, err := digestOf(hash)
	if err != nil {
		return nil, false
	}
	for _, p := range o.packList() {
		if off, length, ok := p.find(sp, d); ok {
			if data, err := p.read(off, length); err == nil && hashBytes(data) == hash {
				return data, true
			}
		}
	}
	if lp, err := o.loosePath(sp, hash); err == nil {
		if data, err := os.ReadFile(lp); err == nil && hashBytes(data) == hash {
			return data, true
		}
	}
	return nil, false
}

// size returns an object's stored length.
func (o *objects) size(sp space, hash string) (int64, bool) {
	loose, _, _, length, err := o.locate(sp, hash, true)
	if err != nil {
		return 0, false
	}
	if loose == "" {
		return length, true
	}
	info, err := os.Stat(loose)
	if err != nil {
		return 0, false
	}
	return info.Size(), true
}

// write stores an object loose, idempotently. An object the database
// already holds is not written again but freshened, as git freshens an
// object it finds it already has, since the sweep spares what is young
// and this write is what makes the object needed again: a loose copy
// has its time touched, a packed one its pack's, and one whose pack a
// sweep has removed is written loose. A loose copy's bytes are not
// read, but its length is checked, so one a crash left short, as a file
// renamed before it was synced can be, is written again from the bytes
// this writer holds; Verify is what checks the rest. With durable set a new file is
// fsynced before it is renamed into place; otherwise it is remembered
// for the next flush. A loose copy found in place is remembered too,
// since another writer may have left it unsynced, and every
// directory's fsync is left to the flush, which the durable commit that
// names the object does first, so the directories a commit touched are
// synced once each however many objects it wrote.
func (o *objects) write(sp space, hash string, data []byte, durable bool) error {
	return o.writeTo(sp, hash, data, durable, nil)
}

// pendSet is what a flush owes: files written or found lazily, and the
// directories they are in. A session holds its own, so its commit
// flushes the objects its appends named and no other session's.
type pendSet struct {
	files, dirs map[string]bool
	// renamed holds the objects written durably into dirs, which a
	// failed sync of their directory writes again.
	renamed map[string]bool
}

func newPendSet() *pendSet {
	return &pendSet{files: map[string]bool{}, dirs: map[string]bool{}, renamed: map[string]bool{}}
}

// writeTo is write, remembering what it leaves unsynced in pend, or in
// the store's own set when pend is nil.
func (o *objects) writeTo(sp space, hash string, data []byte, durable bool, pend *pendSet) error {
	path, err := o.loosePath(sp, hash)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	o.mu.Lock()
	suspect := o.rewrite[path]
	o.mu.Unlock()
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(data)) && !suspect {
		ok, err := o.reuse(path, data)
		if err == nil && !ok {
			goto write
		}
		if err == nil {
			return o.remember(pend, path, dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Packed and removed since the stat; look in the packs.
	} else if errors.Is(err, os.ErrNotExist) {
		o.mu.Lock()
		delete(o.checked, path)
		o.mu.Unlock()
	} else if err == nil {
		// A loose copy of the wrong length, or one whose fsync failed,
		// is written again below: this write holds its right bytes.
		goto write
	}
	if _, lp, _, _, lerr := o.locate(sp, hash, false); lerr == nil && lp != nil {
		now := time.Now()
		if err := os.Chtimes(lp.path, now, now); err == nil {
			return nil // a pack is written durably
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// The pack was removed by a sweep; write the object loose.
	}
write:
	if err := o.fanOut(filepath.Dir(dir)); err != nil {
		return err
	}
	info, err := writeFileInfo(path, data, durable)
	if err != nil {
		return err
	}
	// A new file, whose writeback has not failed.
	o.mu.Lock()
	delete(o.rewrite, path)
	o.mu.Unlock()
	if durable {
		o.check(path, info)
	}
	if !durable {
		return o.remember(pend, path, dir)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	renamed, dirs := o.pendRenamed, o.pendDirs
	if pend != nil {
		renamed, dirs = pend.renamed, pend.dirs
	}
	renamed[path] = true
	dirs[dir] = true
	return nil
}

// fanOut makes a space's fan-out directories, all of them at once, and
// syncs the space's directory, the first time this store writes into
// the space; no object is renamed into a directory a crash could take.
// A directory that exists is not made again, so those made here whose
// parent's sync failed are removed, for a later write to make again,
// rather than kept on the word of a later sync; no write has used them,
// since none writes into a space before its fan-out is durable.
func (o *objects) fanOut(space string) error {
	o.mu.Lock()
	done := o.fanned[space]
	o.mu.Unlock()
	if done {
		return nil
	}
	o.fanning.Lock()
	defer o.fanning.Unlock()
	o.mu.Lock()
	done = o.fanned[space]
	o.mu.Unlock()
	if done {
		return nil
	}
	if err := os.MkdirAll(space, 0o755); err != nil {
		return err
	}
	var made []string
	for i := range 256 {
		d := filepath.Join(space, fmt.Sprintf("%02x", i))
		if err := os.Mkdir(d, 0o755); err == nil {
			made = append(made, d)
		} else if !errors.Is(err, os.ErrExist) {
			for _, m := range made {
				os.Remove(m)
			}
			return err
		}
	}
	if err := syncDir(space); err != nil {
		for _, m := range made {
			os.Remove(m)
		}
		return err
	}
	o.mu.Lock()
	o.fanned[space] = true
	o.mu.Unlock()
	return nil
}

// reuse takes the loose file at path for a copy of data, and freshens
// it, when it holds data's bytes: it is read and compared, unless it is
// the version this store fsynced. The file is opened once, and read,
// compared and freshened through that one descriptor, so a file put in
// its place meanwhile is not taken for it.
func (o *objects) reuse(path string, data []byte) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	pre, err := f.Stat()
	if err != nil {
		return false, err
	}
	if pre.Size() != int64(len(data)) {
		return false, nil
	}
	v, known := versionOf(pre)
	o.mu.Lock()
	known = known && o.checked[path] == v
	o.mu.Unlock()
	if !known {
		have, err := io.ReadAll(f)
		if err != nil {
			return false, err
		}
		if !bytes.Equal(have, data) {
			return false, nil
		}
	}
	if err := touchFile(f, time.Now()); err != nil {
		return false, err
	}
	if known {
		// Freshening changed the file's change time; the version this
		// store knows follows it.
		if post, err := f.Stat(); err == nil {
			if pv, ok := versionOf(post); ok {
				o.mu.Lock()
				if o.checked[path] == v {
					o.checked[path] = pv
				}
				o.mu.Unlock()
			}
		}
	}
	return true, nil
}

// check remembers a large loose file this store fsynced.
func (o *objects) check(path string, info os.FileInfo) {
	v, ok := versionOf(info)
	if !ok || v.size < checkedSize {
		return
	}
	o.mu.Lock()
	o.checked[path] = v
	o.mu.Unlock()
}

// freshenTo is write for an object known only by its name: it reads
// the object, from a pack a sweep removed if this store still holds it
// open, and writes or freshens it, remembering what it leaves unsynced
// in pend.
func (o *objects) freshenTo(sp space, hash string, pend *pendSet) error {
	data, err := o.read(sp, hash)
	if err != nil {
		return err
	}
	return o.writeTo(sp, hash, data, true, pend)
}

// readPacked reads an object from the packs alone, checked against its
// name.
func (o *objects) readPacked(sp space, hash string) ([]byte, error) {
	d, err := digestOf(hash)
	if err != nil {
		return nil, err
	}
	for _, p := range o.packList() {
		if off, length, ok := p.find(sp, d); ok {
			data, err := p.read(off, length)
			if err != nil {
				return nil, err
			}
			if hashBytes(data) != hash {
				return nil, fmt.Errorf("%w: %s object %s (%s)", ErrCorrupt, sp, hash, o.rel(p.path))
			}
			return data, nil
		}
	}
	return nil, os.ErrNotExist
}

// hasQuick is has without the forced reload: for a writer deciding
// whether to send an object, where a duplicate is harmless.
func (o *objects) hasQuick(sp space, hash string) bool {
	_, _, _, _, err := o.locate(sp, hash, false)
	return err == nil
}

// remember adds a file, when path is set, and its directory to what the
// next flush of pend syncs, or of the store's own set when pend is nil.
func (o *objects) remember(pend *pendSet, path, dir string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	files, dirs := o.pendFiles, o.pendDirs
	if pend != nil {
		files, dirs = pend.files, pend.dirs
	}
	if path != "" {
		files[path] = true
	}
	dirs[dir] = true
	return nil
}

// flush fsyncs everything written lazily since the last flush: the
// files first, then the directories they were renamed into, each once.
// A file gone since, packed and removed, needs nothing: the pack that
// holds it was written durably.
//
// The fsyncs of each group run concurrently. A journaling filesystem
// commits concurrent fsyncs together, so a flush of many small objects
// waits for a few commits rather than one per object, as git's batch
// fsync does; each file is still fsynced, and every file before any
// directory.
func (o *objects) flush() error {
	return o.flushSet(nil)
}

// flushSet flushes what pend owes, or the store's own set when pend is
// nil, as flush does.
func (o *objects) flushSet(pend *pendSet) error {
	o.mu.Lock()
	var files, dirs, renamed map[string]bool
	if pend == nil {
		files, dirs, renamed = o.pendFiles, o.pendDirs, o.pendRenamed
		o.pendFiles, o.pendDirs, o.pendRenamed = map[string]bool{}, map[string]bool{}, map[string]bool{}
	} else {
		files, dirs, renamed = pend.files, pend.dirs, pend.renamed
		pend.files, pend.dirs, pend.renamed = map[string]bool{}, map[string]bool{}, map[string]bool{}
	}
	o.mu.Unlock()
	// A failed fsync is not tried again. Linux marks the pages whose
	// writeback failed clean and reports the error once, so a second
	// fsync, on a descriptor opened since, succeeds for data that never
	// reached the disk. The object is written again instead, from the
	// bytes the page cache still holds, checked against its name.
	putBack := func(files, dirs []string) {
		o.mu.Lock()
		defer o.mu.Unlock()
		pf, pd := o.pendFiles, o.pendDirs
		if pend != nil {
			pf, pd = pend.files, pend.dirs
		}
		for _, f := range files {
			pf[f] = true
		}
		for _, d := range dirs {
			pd[d] = true
		}
	}
	failed, err := syncAll(files, func(f string) error {
		o.mu.Lock()
		again := o.rewrite[f]
		o.mu.Unlock()
		if again {
			return o.rewriteObject(f)
		}
		fh, err := os.Open(f)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err == nil {
			if err = syncObject(fh); err == nil {
				if info, serr := fh.Stat(); serr == nil {
					o.check(f, info)
				}
			}
			fh.Close()
		}
		if err != nil {
			if rerr := o.rewriteObject(f); rerr != nil {
				return fmt.Errorf("%w; and writing it again: %w", err, rerr)
			}
		}
		return nil
	})
	if err != nil {
		putBack(failed, slices.Collect(maps.Keys(dirs)))
		o.mu.Lock()
		pr := o.pendRenamed
		if pend != nil {
			pr = pend.renamed
		}
		for f := range renamed {
			pr[f] = true
		}
		o.mu.Unlock()
		return err
	}
	// Every rename above, and every object's, is in a directory synced
	// here.
	for f := range files {
		dirs[filepath.Dir(f)] = true
	}
	failed, err = syncAll(dirs, func(d string) error {
		if err := syncObjectDir(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	if err != nil {
		// The names a failed directory holds are renamed into it again
		// by the next flush, so it has something of its own to sync:
		// a sync of it again could report the failed one's names
		// written when they are not.
		var again []string
		for f := range files {
			if slices.Contains(failed, filepath.Dir(f)) {
				again = append(again, f)
			}
		}
		for f := range renamed {
			if slices.Contains(failed, filepath.Dir(f)) {
				again = append(again, f)
			}
		}
		o.mu.Lock()
		for _, f := range again {
			o.rewrite[f] = true
			delete(o.checked, f)
		}
		o.mu.Unlock()
		putBack(again, failed)
	}
	return err
}

// rewriteObject writes a loose object again, durably, from the bytes
// its file reads: after a failed fsync, the page cache's. Bytes that no
// longer match the object's name were evicted and read back from the
// disk, and the object is lost: its file is moved into the trash, so no
// later write takes it for a copy and recovery finds it gone, and it
// stays owed, so every later flush that owes it fails until a write
// holding its bytes puts it back or an open recovers the session from
// the disk.
func (o *objects) rewriteObject(path string) error {
	sp, hash, ok := o.objectAt(path)
	if !ok {
		return fmt.Errorf("cas: %s is no object", o.rel(path))
	}
	err := o.writeAgain(sp, hash, path)
	o.mu.Lock()
	if err != nil {
		o.rewrite[path] = true
		delete(o.checked, path)
	} else {
		delete(o.rewrite, path)
	}
	o.mu.Unlock()
	return err
}

func (o *objects) writeAgain(sp space, hash, path string) error {
	lost := fmt.Errorf("%w: %s %s was lost before it reached the disk", ErrCorrupt, sp, hash)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		// Packed since, and a pack is written durably; or moved aside
		// by another process that found it lost.
		if _, p, _, _, lerr := o.locate(sp, hash, true); lerr == nil && p != nil {
			return nil
		}
		return lost
	}
	if err != nil {
		return err
	}
	if hashBytes(data) == hash {
		info, err := writeFileInfo(path, data, true)
		if err == nil {
			o.check(path, info)
		}
		return err
	}
	trash := filepath.Join(o.root, "trash")
	if err := os.MkdirAll(trash, 0o755); err != nil {
		return err
	}
	aside := filepath.Join(trash, fmt.Sprintf("object-%s-%d", filepath.Base(path), time.Now().UnixNano()))
	if err := os.Rename(path, aside); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// Another process may have written a good copy between the read
	// and the rename; that one is put back, durably.
	if good, err := os.ReadFile(aside); err == nil && hashBytes(good) == hash {
		os.Remove(aside)
		return writeFile(path, good, true)
	}
	return lost
}

// objectAt returns the object a loose path names.
func (o *objects) objectAt(path string) (space, string, bool) {
	for _, sp := range []space{spaceEntries, spaceContents} {
		rel, err := filepath.Rel(o.spaceDir(sp), path)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		h := agentsession.HashPrefix + strings.ReplaceAll(rel, string(filepath.Separator), "")
		return sp, h, agentsession.ValidHash(h)
	}
	return 0, "", false
}

// syncObjectDir fsyncs an object directory; a variable so a test can
// fail it.
var syncObjectDir = syncDir

// syncObject fsyncs an object's file; a variable so a test can fail it.
var syncObject = func(f *os.File) error { return f.Sync() }

// flushWorkers bounds the fsyncs a flush has in flight.
const flushWorkers = 32

// syncAll calls fsync for every path, flushWorkers at a time, and
// returns the paths that failed and the first error once all have
// finished.
func syncAll(paths map[string]bool, fsync func(string) error) ([]string, error) {
	type result struct {
		path string
		err  error
	}
	if len(paths) <= 1 {
		for p := range paths {
			if err := fsync(p); err != nil {
				return []string{p}, err
			}
		}
		return nil, nil
	}
	work := make(chan string)
	results := make(chan result, len(paths))
	var wg sync.WaitGroup
	for range min(flushWorkers, len(paths)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range work {
				results <- result{p, fsync(p)}
			}
		}()
	}
	for p := range paths {
		work <- p
	}
	close(work)
	wg.Wait()
	close(results)
	var failed []string
	var first error
	for r := range results {
		if r.err != nil {
			failed = append(failed, r.path)
			if first == nil {
				first = r.err
			}
		}
	}
	return failed, first
}

func (o *objects) close() {
	o.mu.Lock()
	defer o.mu.Unlock()
	for _, p := range o.packs {
		p.close()
	}
	o.packs = nil
}

// eachLoose calls fn for every loose object of a space, and for every
// temporary file a crashed write left, with tmp set.
func (o *objects) eachLoose(sp space, fn func(hash, path string, info os.FileInfo, tmp bool) error) error {
	dir := o.spaceDir(sp)
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // removed as we walked
		}
		if strings.HasPrefix(d.Name(), ".tmp-") {
			return fn("", path, info, true)
		}
		rel, _ := filepath.Rel(dir, path)
		return fn(agentsession.HashPrefix+strings.ReplaceAll(rel, string(filepath.Separator), ""), path, info, false)
	})
}
