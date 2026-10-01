package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Problem is one thing Verify found wrong.
type Problem struct {
	// Kind is "log" for a damaged log record, "corrupt" for an object or
	// pack whose bytes fail their name or checksum, "missing" for an
	// object something the store holds names and the store lacks,
	// "session" for a session that cannot be opened, and KindLeftover.
	Kind string
	// Object is the object's hash, for "corrupt", "missing" and
	// KindLeftover.
	Object string
	// Session is the session concerned, when there is one.
	Session string
	Err     error
}

func (p Problem) String() string {
	switch {
	case p.Object != "" && p.Session != "":
		return fmt.Sprintf("%s %s (session %s): %v", p.Kind, p.Object, p.Session, p.Err)
	case p.Object != "":
		return fmt.Sprintf("%s %s: %v", p.Kind, p.Object, p.Err)
	case p.Session != "":
		return fmt.Sprintf("%s %s: %v", p.Kind, p.Session, p.Err)
	}
	return fmt.Sprintf("%s: %v", p.Kind, p.Err)
}

// Report is what Verify found.
type Report struct {
	Sessions int // sessions checked
	Entries  int // entries whose objects were checked
	Objects  int // objects read and hashed, loose and packed
	Problems []Problem
}

// KindLeftover is the Kind of a loose object whose bytes fail its name
// and which nothing the store holds needs, such as what a crash left of
// an append recovery recorded lost. It is no failure: a sweep removes
// it once it is older than the sweep's grace.
const KindLeftover = "leftover"

// OK reports whether Verify found nothing wrong, a leftover aside.
func (r Report) OK() bool {
	for _, p := range r.Problems {
		if p.Kind != KindLeftover {
			return false
		}
	}
	return true
}

// Verify walks the whole store as git fsck does and reports what is
// wrong with it: every log record's checksum, every loose object
// and every pack against their names and checksums, every entry the
// store holds for its two objects and the blobs its content names, and
// every session by building it as Open does, which checks each entry's
// hashes and its parent. It changes nothing, takes no session lock, and
// runs on a read-only store. A loose object that fails its name and
// that a sweep would not keep is reported as KindLeftover, not as
// corrupt: nothing reads it. An error is returned only when the walk
// itself cannot proceed; what it finds is in the report.
func (s *Store) Verify(ctx context.Context) (Report, error) {
	var rep Report
	add := func(p Problem) { rep.Problems = append(rep.Problems, p) }

	kept, _ := filepath.Glob(filepath.Join(s.root, damagedPrefix+"*"))
	sort.Strings(kept)
	for _, p := range kept {
		add(Problem{Kind: "log", Err: fmt.Errorf("a migration retired a journal with damaged lines, kept as %s", filepath.Base(p))})
	}
	if _, err := os.Stat(filepath.Join(s.root, journalFile)); err == nil {
		// Each session that failed to migrate is reported below.
		add(Problem{Kind: "log", Err: errors.New("the journal of a store from before per-session logs is kept, as a session has not migrated; each writing open tries it again")})
	}

	if err := s.objs.reloadPacks(true); err != nil {
		add(Problem{Kind: "corrupt", Err: err})
	}
	s.objs.mu.Lock()
	for _, err := range s.objs.bad {
		add(Problem{Kind: "corrupt", Err: err})
	}
	s.objs.mu.Unlock()
	for _, p := range s.objs.packList() {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		for _, e := range verifyPack(p) {
			add(Problem{Kind: "corrupt", Err: e})
		}
		rep.Objects += len(p.idx) / idxRecord
	}
	// Whether a loose object that fails its name is needed is whether a
	// sweep would keep it, worked out once, on the first such object.
	// Where a sweep could not work it out, as past a damaged log, every
	// such object is needed and reported corrupt.
	var keep *keepSet
	needed := func(sp space, hash string) bool {
		if keep == nil {
			k, _, err := s.keepAll()
			if err != nil {
				k = keepSet{}
			}
			keep = &k
		}
		return keep.entries == nil || keep.has(sp, hash)
	}
	for _, sp := range []space{spaceEntries, spaceContents} {
		err := s.objs.eachLoose(sp, func(hash, path string, _ os.FileInfo, tmp bool) error {
			if tmp {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return nil
			}
			rep.Objects++
			// A loose copy beside a pack holding the object, as a crash can
			// leave the removal of a commit pack's unsynced loose copies,
			// is a duplicate no read takes: the pack's copy is checked
			// above.
			if hashBytes(data) != hash {
				if _, p, _, _, err := s.objs.locate(sp, hash, true); err == nil && p != nil {
					return ctx.Err()
				}
				err := fmt.Errorf("%w: %s object (%s)", ErrCorrupt, sp, s.objs.rel(path))
				if !needed(sp, hash) {
					add(Problem{Kind: KindLeftover, Object: hash, Err: fmt.Errorf("%w; nothing the store holds needs it, and a sweep removes it once it is older than the sweep's grace", err)})
					return ctx.Err()
				}
				add(Problem{Kind: "corrupt", Object: hash, Err: err})
			}
			return ctx.Err()
		})
		if err != nil {
			return rep, err
		}
	}

	// Every session, as Open would build it, without its lock.
	dirs, err := os.ReadDir(filepath.Join(s.root, "sessions"))
	if err != nil {
		return rep, fmt.Errorf("cas: %w", err)
	}
	checked := map[string]bool{}
	var ids []string
	for _, d := range dirs {
		if d.IsDir() && validSessionID(d.Name()) {
			ids = append(ids, d.Name())
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		dir := filepath.Join(s.root, "sessions", id)
		v, err := s.reconcile(id, dir)
		if err != nil {
			kind := "session"
			if errors.As(err, new(LogDamage)) {
				kind = "log"
			}
			add(Problem{Kind: kind, Session: id, Err: err})
			continue
		}
		if !v.exists {
			continue
		}
		rep.Sessions++
		cuts, _ := filepath.Glob(filepath.Join(dir, cutPrefix+"*"))
		sort.Strings(cuts)
		for _, c := range cuts {
			add(Problem{Kind: "log", Session: id, Err: fmt.Errorf("recovery cut a commit that had finished, after a block left unwritten; its bytes are kept as %s", filepath.Base(c))})
		}
		damaged, _ := filepath.Glob(filepath.Join(dir, damagedLogPrefix+"*"))
		sort.Strings(damaged)
		for _, d := range damaged {
			add(Problem{Kind: "log", Session: id, Err: fmt.Errorf("a repair rewrote a damaged log; the damaged log is kept as %s, and a sweep keeps every object it names until it is removed", filepath.Base(d))})
		}
		if v.cutCommitted {
			add(Problem{Kind: "log", Session: id, Err: errors.New("a block left unwritten is followed by a commit that had finished, which recovery will cut")})
		}
		for _, d := range v.damage {
			add(Problem{Kind: "log", Session: id, Err: d})
		}
		hdr, err := readHeader(dir)
		if err != nil {
			add(Problem{Kind: "session", Session: id, Err: err})
			continue
		}
		var path []string
		for e := hdr.Base; e != ""; {
			path = append(path, e)
			parent, err := s.parentOf(e)
			if err != nil {
				break // reported below, as the entry's objects
			}
			e = parent
		}
		bad := false
		for _, e := range append(path, v.log...) {
			if checked[e] {
				continue
			}
			checked[e] = true
			rep.Entries++
			if p, ok := s.checkEntry(e); !ok {
				p.Session = id
				add(p)
				bad = true
			}
		}
		if bad {
			continue // the session cannot build; its objects are the problem
		}
		var prefix, own [][]byte
		for i := len(path) - 1; i >= 0; i-- {
			line, _ := s.loadLine(path[i])
			prefix = append(prefix, line)
		}
		for _, e := range v.log {
			line, _ := s.loadLine(e)
			own = append(own, line)
		}
		if _, err := s.assemble(hdr, prefix, own, v.head); err != nil {
			add(Problem{Kind: "session", Session: id, Err: err})
		}
	}
	return rep, nil
}

// checkEntry reads an entry's objects and the blobs its content names.
func (s *Store) checkEntry(id string) (Problem, bool) {
	kind := func(err error) string {
		if errors.Is(err, ErrCorrupt) {
			return "corrupt"
		}
		return "missing"
	}
	env, err := s.objs.read(spaceEntries, id)
	if err != nil {
		return Problem{Kind: kind(err), Object: id, Err: err}, false
	}
	c, ok := envelopeContent(env)
	if !ok {
		return Problem{Kind: "corrupt", Object: id, Err: fmt.Errorf("%w: entry %s names no content", ErrCorrupt, id)}, false
	}
	body, err := s.objs.read(spaceContents, c)
	if err != nil {
		return Problem{Kind: kind(err), Object: c, Err: fmt.Errorf("%w (the content of entry %s)", err, id)}, false
	}
	for _, b := range blobsNamedBy(body) {
		if _, err := s.objs.read(spaceContents, b); err != nil {
			return Problem{Kind: kind(err), Object: b, Err: fmt.Errorf("%w (a blob entry %s names)", err, id)}, false
		}
	}
	return Problem{}, true
}
