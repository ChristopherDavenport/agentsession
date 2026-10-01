package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	// fanned holds the spaces whose fan-out directories this store has
	// seen made durable; fanning makes them, one space at a time.
	fanned  map[string]bool
	fanning sync.Mutex
	// noLink is set once the filesystem has refused a hard link, after
	// which every object is written durably.
	noLink atomic.Bool
	// failure is the first fsync of the store's data that failed, after
	// which the store writes nothing; syncing holds this process's lock
	// on fsyncs of each path in flight.
	failure atomic.Pointer[error]
	syncing map[string]*pathLock
	// checked holds the loose files this store wrote. An object's file
	// is replaced only by renaming a new one over it, so the same file
	// still holds this store's bytes; one not yet fsynced is fsynced by
	// this store alone, since no other process fsyncs a file it did not
	// write, so a failure of its writeback is reported here, at the next
	// commit, and stops the store. A file is known by its version, change
	// time included, since a file made after it is removed may take over
	// its inode.
	checked map[string]fileVersion
}

// fileVersion is one version of a file, as versionOf reads it.
type fileVersion struct {
	dev, ino    uint64
	ctime, size int64
}

// checkedLimit bounds the files checked remembers.
const checkedLimit = 1 << 16

func newObjects(root string) *objects {
	return &objects{root: root, pendFiles: map[string]bool{}, pendDirs: map[string]bool{}, fanned: map[string]bool{}, syncing: map[string]*pathLock{}, checked: map[string]fileVersion{}}
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
// sweep has removed is written loose. A loose copy is taken only when
// its bytes are this writer's, since a crash can leave a file of the
// right length holding zeros; one that is not is written again. With
// durable set a new file is fsynced before it is renamed into place; otherwise it is remembered
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
}

func newPendSet() *pendSet {
	return &pendSet{files: map[string]bool{}, dirs: map[string]bool{}}
}

// writeTo is write, remembering what it leaves unsynced in pend, or in
// the store's own set when pend is nil.
func (o *objects) writeTo(sp space, hash string, data []byte, durable bool, pend *pendSet) error {
	path, err := o.loosePath(sp, hash)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	// replacing is set when a file is in the object's place, which may be
	// one another session committed: a copy put over it is written
	// durably, since a rename can reach the disk before an unsynced
	// file's bytes do, and a crash would leave the committed object
	// zeros.
	replacing := false
	if info, err := os.Stat(path); err == nil && info.Size() == int64(len(data)) {
		ok, err := o.reuse(path, len(data))
		if err == nil && !ok {
			replacing = true
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
		// A loose copy of the wrong length is written again below: this
		// write holds its right bytes.
		replacing = true
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
	var info os.FileInfo
	if !durable && !replacing && !o.noLink.Load() {
		// A new object, written lazily, takes its place only if nothing
		// has since: one another process wrote meanwhile is replaced
		// durably instead. So is the object where the filesystem makes
		// no hard link, from then on, which is slower but as sound.
		info, err = writeFileWith(path, data, nil, false)
		if errors.Is(err, errNoLink) {
			o.noLink.Store(true)
		}
		if errors.Is(err, os.ErrExist) || errors.Is(err, errNoLink) {
			durable = true
		}
	} else if !durable {
		durable = true
	}
	if durable {
		info, err = o.writeFile(path, data, true)
	}
	if err != nil {
		return err
	}
	o.check(path, info)
	if !durable {
		return o.remember(pend, path, dir)
	}
	return o.remember(pend, "", dir)
}

// fanOut makes a space's fan-out directories, all of them at once, and
// syncs the space's directory, the first time this store writes into
// the space; no object is renamed into a directory a crash could take.
// A failed sync stops the store; the directories made here are removed,
// since no write has used them: none writes into a space before its
// fan-out is durable.
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
	if err := o.fsyncDir(space); err != nil {
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

// reuse freshens the loose file at path and takes it for data's copy
// when it is a version of the file this store wrote, and otherwise
// reports false, for the caller to write a copy of its own. A file this
// store did not write is not trusted: a crash may have left it zeros,
// and another process's fsync of it may have failed and been reported
// to that process alone, after which an fsync of it here succeeds for
// bytes the disk does not hold. The file is opened once, and checked
// and freshened through that one descriptor, so a file put in its place
// meanwhile is not taken for it.
func (o *objects) reuse(path string, size int) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	pre, err := f.Stat()
	if err != nil {
		return false, err
	}
	v, ok := versionOf(pre)
	o.mu.Lock()
	ok = ok && pre.Size() == int64(size) && o.checked[path] == v
	o.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := touchFile(f, time.Now()); err != nil {
		return false, err
	}
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
	return true, nil
}

// check remembers a loose file this store wrote. Past checkedLimit
// files the memory starts again, and a file forgotten is written again
// at its next use.
func (o *objects) check(path string, info os.FileInfo) {
	v, ok := versionOf(info)
	if !ok {
		return
	}
	o.mu.Lock()
	if len(o.checked) >= checkedLimit {
		o.checked = map[string]fileVersion{}
	}
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
	var files, dirs map[string]bool
	if pend == nil {
		files, dirs = o.pendFiles, o.pendDirs
		o.pendFiles, o.pendDirs = map[string]bool{}, map[string]bool{}
	} else {
		files, dirs = pend.files, pend.dirs
		pend.files, pend.dirs = map[string]bool{}, map[string]bool{}
	}
	o.mu.Unlock()
	if err := o.stopped(); err != nil {
		return err
	}
	if _, err := syncAll(files, func(f string) error {
		fh, err := os.Open(f)
		if errors.Is(err, os.ErrNotExist) {
			return nil // packed since, and a pack is written durably
		}
		if err != nil {
			return err
		}
		defer fh.Close()
		if err := o.fsync(f, fh, syncObject); err != nil {
			return err
		}
		if info, err := fh.Stat(); err == nil {
			o.check(f, info)
		}
		return nil
	}); err != nil {
		return err
	}
	// Every object's rename is in a directory synced here.
	for f := range files {
		dirs[filepath.Dir(f)] = true
	}
	_, err := syncAll(dirs, func(d string) error {
		err := o.fsyncDir(d)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
	return err
}

// ErrStopped is returned by every write to a store after an fsync of
// its data failed, until it is opened again; the package documentation
// says why, and what an open after it guarantees. Reads go on.
var ErrStopped = errors.New("cas: the store stopped writing after an fsync failed; open it again")

// stopped returns ErrStopped, with the fsync that failed, once one has.
func (o *objects) stopped() error {
	if err := o.failure.Load(); err != nil {
		return fmt.Errorf("%w: %v", ErrStopped, *err)
	}
	return nil
}

// fsync fsyncs the file f, open at path, with sync, as every fsync of
// the store's data is made: one fsync of a path at a time in this
// process, none once the store has stopped, and a failure stops it
// before the next fsync of that path runs, so that fsync, which the
// kernel would tell it succeeded, never does.
func (o *objects) fsync(path string, f *os.File, sync func(*os.File) error) error {
	if o == nil {
		return sync(f) // a file of no store's
	}
	unlock := o.lockPath(path)
	defer unlock()
	if err := o.stopped(); err != nil {
		return err
	}
	if err := sync(f); err != nil {
		err = fmt.Errorf("%s: %w", o.rel(path), err)
		o.failure.CompareAndSwap(nil, &err)
		return o.stopped()
	}
	return nil
}

// fsyncDir fsyncs a directory of the store's, as fsync does a file.
func (o *objects) fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return o.fsync(dir, d, syncDirFile)
}

// writeAtomic is writeAtomic for a file of the store's.
func (o *objects) writeAtomic(path string, data []byte) error {
	if _, err := o.writeFile(path, data, true); err != nil {
		return err
	}
	return o.fsyncDir(filepath.Dir(path))
}

// fsyncFile fsyncs a file of the store's by its path.
func (o *objects) fsyncFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return o.fsync(path, f, syncFile)
}

// writeFile is writeFile for a file of the store's, whose fsync, when
// durable, is the store's.
func (o *objects) writeFile(path string, data []byte, durable bool) (os.FileInfo, error) {
	if !durable {
		return writeFileWith(path, data, nil, true)
	}
	return writeFileWith(path, data, func(f *os.File) error {
		return o.fsync(f.Name(), f, syncFile)
	}, true)
}

// lockPath takes this process's lock on fsyncs of path.
func (o *objects) lockPath(path string) func() {
	o.mu.Lock()
	l := o.syncing[path]
	if l == nil {
		l = &pathLock{}
		o.syncing[path] = l
	}
	l.users++
	o.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		o.mu.Lock()
		if l.users--; l.users == 0 {
			delete(o.syncing, path)
		}
		o.mu.Unlock()
	}
}

type pathLock struct {
	mu    sync.Mutex
	users int
}

// syncFile fsyncs a file of the store's other than a log or a loose
// object; a variable so a test can fail it.
var syncFile = func(f *os.File) error { return f.Sync() }

// syncDirFile fsyncs a directory; a variable so a test can fail it.
var syncDirFile = func(d *os.File) error { return d.Sync() }

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
