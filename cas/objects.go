package cas

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

	// Written but not yet fsynced, under a lazy sync policy: files and
	// the directories they were renamed into.
	pendFiles map[string]bool
	pendDirs  map[string]bool
}

func newObjects(root string) *objects {
	return &objects{root: root, pendFiles: map[string]bool{}, pendDirs: map[string]bool{}}
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
// since the last read. Packs already open stay open, so a pack another
// process removed stays readable to this one until it is dropped.
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
			return err
		}
		next = append(next, p)
	}
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

// locate finds an object: its loose path, or the pack holding it.
func (o *objects) locate(sp space, hash string) (loose string, p *pack, off, length int64, err error) {
	path, err := o.loosePath(sp, hash)
	if err != nil {
		return "", nil, 0, 0, err
	}
	d, err := digestOf(hash)
	if err != nil {
		return "", nil, 0, 0, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := os.Stat(path); err == nil {
			return path, nil, 0, 0, nil
		}
		for _, p := range o.packList() {
			if off, length, ok := p.find(sp, d); ok {
				return "", p, off, length, nil
			}
		}
		if err := o.reloadPacks(attempt > 0); err != nil {
			return "", nil, 0, 0, err
		}
	}
	return "", nil, 0, 0, os.ErrNotExist
}

// read returns an object's bytes, checked against its name.
func (o *objects) read(sp space, hash string) ([]byte, error) {
	for attempt := 0; ; attempt++ {
		loose, p, off, length, err := o.locate(sp, hash)
		if err != nil {
			return nil, fmt.Errorf("cas: %s %s: %w", sp, hash, err)
		}
		var data []byte
		where := ""
		if loose != "" {
			data, err = os.ReadFile(loose)
			where = o.rel(loose)
			if errors.Is(err, os.ErrNotExist) && attempt == 0 {
				continue // packed and removed between the stat and the read
			}
		} else {
			data, err = p.read(off, length)
			where = o.rel(p.path)
		}
		if err != nil {
			return nil, fmt.Errorf("cas: %s %s: %w", sp, hash, err)
		}
		if hashBytes(data) != hash {
			return nil, fmt.Errorf("%w: %s object %s (%s)", ErrCorrupt, sp, hash, where)
		}
		return data, nil
	}
}

// has reports whether the database holds the object, loose or packed.
func (o *objects) has(sp space, hash string) bool {
	_, _, _, _, err := o.locate(sp, hash)
	return err == nil
}

// size returns an object's stored length.
func (o *objects) size(sp space, hash string) (int64, bool) {
	loose, _, _, length, err := o.locate(sp, hash)
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

// write stores an object loose, idempotently: one the database already
// holds is left as it is, and a loose copy is freshened, as git does a
// loose object it finds it already has, since the sweep spares a young
// object and this write is what makes the object needed again. With
// durable set the file is fsynced before it is renamed into place;
// otherwise it is remembered for the next flush. The directory's fsync
// is always left to the flush, which the durable commit that names the
// object does first, so the directories a commit touched are synced once
// each however many objects it wrote.
func (o *objects) write(sp space, hash string, data []byte, durable bool) error {
	path, err := o.loosePath(sp, hash)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err == nil {
		now := time.Now()
		if err := os.Chtimes(path, now, now); err == nil || !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Packed and removed since the stat; it is in a pack now.
	}
	if o.has(sp, hash) {
		return nil
	}
	dir := filepath.Dir(path)
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := o.syncOrDefer(filepath.Dir(dir), true, false); err != nil {
			return err
		}
	}
	if err := writeFile(path, data, durable); err != nil {
		if _, serr := os.Stat(path); serr == nil {
			return nil // another writer stored the same object
		}
		return err
	}
	if !durable {
		o.mu.Lock()
		o.pendFiles[path] = true
		o.mu.Unlock()
	}
	return o.syncOrDefer(dir, true, false)
}

// syncOrDefer fsyncs a directory now, or remembers it for flush.
func (o *objects) syncOrDefer(path string, isDir, now bool) error {
	if now {
		return syncDir(path)
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if isDir {
		o.pendDirs[path] = true
	} else {
		o.pendFiles[path] = true
	}
	return nil
}

// flush fsyncs everything written lazily since the last flush: the
// files first, then the directories they were renamed into, each once.
// A file gone since, packed and removed, needs nothing: the pack that
// holds it was written durably.
func (o *objects) flush() error {
	o.mu.Lock()
	files, dirs := o.pendFiles, o.pendDirs
	o.pendFiles, o.pendDirs = map[string]bool{}, map[string]bool{}
	o.mu.Unlock()
	for f := range files {
		fh, err := os.Open(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		err = fh.Sync()
		fh.Close()
		if err != nil {
			return err
		}
	}
	for d := range dirs {
		if err := syncDir(d); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
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
