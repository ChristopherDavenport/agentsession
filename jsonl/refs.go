package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Refs are two files at the root, which is one machine's: refs.json,
// the refs written whole, and refs.log, every update appended a line
// at a time, both guarded by refs.json.lock, the advisory lock a
// session's file has, taken over when its holder on this host is gone.
//
// refs.json is {"seq": N, "refs": {name: {"session", "entry"}}}, N the
// sequence of the last log record it holds. An update takes the lock,
// compares, appends the record numbered N+1 to refs.log and fsyncs it,
// and then replaces refs.json by a temporary file and a rename carrying
// N+1: the log is written ahead, so an update that cannot be logged is
// not accepted. A crash between the two leaves refs.json at N and the
// log at N+1; the next update finishes it, and a read sees the record
// at once, as the log says. A last line cut short is a record being
// written; the next update cuts it. A whole line that does not read is
// damage, reported, and never skipped.

const (
	refsFile = "refs.json"
	refsLog  = "refs.log"
)

type refTarget struct {
	Session string `json:"session"`
	Entry   string `json:"entry,omitempty"`
	// Ident is the session's HeaderIdent when the ref was set, which the
	// store compares at resolution and no caller sees.
	Ident string `json:"ident,omitempty"`
}

func (t refTarget) get() agentsession.RefTarget {
	return agentsession.RefTarget{Session: t.Session, Entry: t.Entry}
}

func asRef(t agentsession.RefTarget, ident string) refTarget {
	return refTarget{Session: t.Session, Entry: t.Entry, Ident: ident}
}

// incarnation is the identity of the session now under id, and false
// when none is.
func (s *Store) incarnation(id string) (string, bool) {
	path, err := s.find(id)
	if err != nil {
		return "", false
	}
	sum, err := summarize(path, false)
	if err != nil {
		return "", false
	}
	ident, err := agentsession.HeaderIdent(sum.Header)
	return ident, err == nil
}

type refsDoc struct {
	Seq  int64                `json:"seq"`
	Refs map[string]refTarget `json:"refs"`
}

type refRecord struct {
	Seq    int64     `json:"seq"`
	Name   string    `json:"name"`
	Old    refTarget `json:"old"`
	New    refTarget `json:"new"`
	At     string    `json:"at"`
	Reason string    `json:"reason,omitempty"`
}

func (r refRecord) update() agentsession.RefUpdate {
	at, _ := time.Parse(time.RFC3339Nano, r.At)
	return agentsession.RefUpdate{Name: r.Name, Old: r.Old.get(), New: r.New.get(), Time: at, Reason: r.Reason}
}

// refStep is called at the steps of an update that a test holds still.
var refStep = func(stage string) {}

func (s *Store) refsPath() string   { return filepath.Join(s.root, refsFile) }
func (s *Store) refLogPath() string { return filepath.Join(s.root, refsLog) }

func (s *Store) readRefsDoc() (refsDoc, error) {
	doc := refsDoc{Refs: map[string]refTarget{}}
	data, err := os.ReadFile(s.refsPath())
	if errors.Is(err, os.ErrNotExist) {
		return doc, nil
	}
	if err != nil {
		return doc, fmt.Errorf("jsonl: refs: %w", err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, fmt.Errorf("jsonl: %s does not read: %w", refsFile, err)
	}
	if doc.Refs == nil {
		doc.Refs = map[string]refTarget{}
	}
	return doc, nil
}

// refLogTail is the last whole record of refs.log, and where its whole
// lines end.
type refLogTail struct {
	last  *refRecord
	whole int64
	size  int64
}

func (s *Store) readRefLogTail() (refLogTail, error) {
	var t refLogTail
	f, err := os.Open(s.refLogPath())
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("jsonl: refs log: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return t, err
	}
	t.size, t.whole = info.Size(), info.Size()
	for chunk := int64(8 << 10); ; chunk *= 2 {
		start := max(0, info.Size()-chunk)
		buf := make([]byte, info.Size()-start)
		if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
			return t, err
		}
		end := len(buf)
		if i := bytes.LastIndexByte(buf, '\n'); i < 0 {
			end = 0
		} else if i != len(buf)-1 {
			end = i + 1
		}
		t.whole = start + int64(end)
		lines := bytes.Split(bytes.TrimRight(buf[:end], "\n"), []byte{'\n'})
		if start > 0 {
			lines = lines[1:]
		}
		if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
			if start == 0 {
				return t, nil
			}
			continue
		}
		var rec refRecord
		if err := json.Unmarshal(lines[len(lines)-1], &rec); err != nil {
			return t, fmt.Errorf("jsonl: %s: its last record does not read: %w", refsLog, err)
		}
		t.last = &rec
		return t, nil
	}
}

// effectiveRefs is the refs as refs.json and the end of refs.log say
// them together: a last record that is one past refs.json's sequence
// is an update that was committed and not yet made, and is applied.
func (s *Store) effectiveRefs() (refsDoc, refLogTail, error) {
	doc, err := s.readRefsDoc()
	if err != nil {
		return doc, refLogTail{}, err
	}
	tail, err := s.readRefLogTail()
	if err != nil {
		return doc, tail, err
	}
	if tail.last != nil && tail.last.Seq > doc.Seq {
		applyRecord(&doc, *tail.last)
	}
	return doc, tail, nil
}

func applyRecord(doc *refsDoc, r refRecord) {
	if r.New.Session == "" {
		delete(doc.Refs, r.Name)
	} else {
		doc.Refs[r.Name] = r.New
	}
	doc.Seq = r.Seq
}

// ResolveRef implements [agentsession.RefStore].
func (s *Store) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	if err := agentsession.ValidRefName(name); err != nil {
		return agentsession.RefTarget{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentsession.RefTarget{}, err
	}
	doc, _, err := s.effectiveRefs()
	if err != nil {
		return agentsession.RefTarget{}, err
	}
	t, ok := doc.Refs[name]
	if !ok {
		return agentsession.RefTarget{}, fmt.Errorf("%w: %s", agentsession.ErrNoRef, name)
	}
	ident, found := s.incarnation(t.Session)
	if !found {
		return t.get(), fmt.Errorf("%w: %s, which ref %s names", agentsession.ErrNoSession, t.Session, name)
	}
	if t.Ident != "" && ident != t.Ident {
		return t.get(), fmt.Errorf("%w: %s was created again after ref %s was set", agentsession.ErrNoSession, t.Session, name)
	}
	return t.get(), nil
}

// ListRefs implements [agentsession.RefStore].
func (s *Store) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	return func(yield func(agentsession.Ref, error) bool) {
		if err := agentsession.ValidRefPrefix(prefix); err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		doc, _, err := s.effectiveRefs()
		if err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		var names []string
		for n := range doc.Refs {
			if strings.HasPrefix(n, prefix) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		for _, n := range names {
			if !yield(agentsession.Ref{Name: n, Target: doc.Refs[n].get()}, nil) {
				return
			}
		}
	}
}

// RefLog implements [agentsession.RefStore].
func (s *Store) RefLog(ctx context.Context, name string) iter.Seq2[agentsession.RefUpdate, error] {
	return func(yield func(agentsession.RefUpdate, error) bool) {
		if err := agentsession.ValidRefName(name); err != nil {
			yield(agentsession.RefUpdate{}, err)
			return
		}
		data, err := os.ReadFile(s.refLogPath())
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			yield(agentsession.RefUpdate{}, fmt.Errorf("jsonl: refs log: %w", err))
			return
		}
		i := bytes.LastIndexByte(data, '\n')
		if i < 0 {
			return
		}
		lines := bytes.Split(bytes.TrimRight(data[:i+1], "\n"), []byte{'\n'})
		for n := len(lines) - 1; n >= 0; n-- {
			var rec refRecord
			if err := json.Unmarshal(lines[n], &rec); err != nil {
				yield(agentsession.RefUpdate{}, fmt.Errorf("jsonl: %s line %d does not read: %w", refsLog, n+1, err))
				return
			}
			if rec.Name != name {
				continue
			}
			if !yield(rec.update(), nil) {
				return
			}
		}
	}
}

// refsLocked runs f holding refs.json.lock, waiting for its holder
// until ctx ends. The lock is taken over when its holder was a process
// on this host that is gone.
func (s *Store) refsLocked(ctx context.Context, f func() error) error {
	s.refMu.Lock()
	defer s.refMu.Unlock()
	path := s.refsPath()
	for wait := time.Millisecond; ; wait = min(2*wait, 20*time.Millisecond) {
		err := acquireLock(path, s.staleReport)
		if err == nil {
			break
		}
		if !errors.Is(err, ErrSessionLocked) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	defer releaseLock(path)
	return f()
}

// UpdateRef implements [agentsession.RefStore].
func (s *Store) UpdateRef(ctx context.Context, name string, expected, next agentsession.RefTarget, reason string) error {
	if err := agentsession.ValidRefName(name); err != nil {
		return err
	}
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.refsLocked(ctx, func() error {
		doc, err := s.readRefsDoc()
		if err != nil {
			return err
		}
		tail, err := s.readRefLogTail()
		if err != nil {
			return err
		}
		// A record the file does not hold yet is made first.
		if tail.last != nil && tail.last.Seq > doc.Seq {
			applyRecord(&doc, *tail.last)
			if err := s.writeRefsDoc(doc); err != nil {
				return err
			}
		}
		refStep("read")
		curVal := doc.Refs[name]
		cur := curVal.get()
		if cur != expected {
			return &agentsession.RefMovedError{Name: name, Current: cur}
		}
		var nextIdent string
		if !next.IsZero() {
			var found bool
			if nextIdent, found = s.incarnation(next.Session); !found {
				return fmt.Errorf("%w: %s", agentsession.ErrNoSession, next.Session)
			}
			if next.Entry != "" {
				sess, err := s.Read(ctx, next.Session)
				if err != nil {
					return err
				}
				if _, ok := sess.Entry(next.Entry); !ok {
					return fmt.Errorf("%w: %s in session %s", agentsession.ErrNoEntry, next.Entry, next.Session)
				}
			}
		}
		// Setting a ref to the target it holds changes nothing, unless
		// the session under that ID is another since: then it adopts it.
		if cur == next && curVal.Ident == nextIdent {
			return nil
		}
		if cur.IsZero() {
			names := make([]string, 0, len(doc.Refs))
			for n := range doc.Refs {
				names = append(names, n)
			}
			if err := agentsession.RefConflict(name, names); err != nil {
				return err
			}
		}
		refStep("checked")
		rec := refRecord{
			Seq: doc.Seq + 1, Name: name, Old: asRef(cur, curVal.Ident), New: asRef(next, nextIdent),
			At: time.Now().UTC().Format(time.RFC3339Nano), Reason: reason,
		}
		if err := s.appendRefRecord(tail, rec); err != nil {
			return err
		}
		applyRecord(&doc, rec)
		if err := s.writeRefsDoc(doc); err != nil {
			// The record is cut back, so an update that failed is not one
			// the log says was made; where that fails too, the next
			// update makes what the record says.
			if terr := os.Truncate(s.refLogPath(), tail.whole); terr != nil {
				return fmt.Errorf("%w; and cutting its record back: %v", err, terr)
			}
			return err
		}
		return nil
	})
}

// appendRefRecord appends rec to refs.log and fsyncs it, first cutting
// a last line a crash left cut.
func (s *Store) appendRefRecord(tail refLogTail, rec refRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	f, err := os.OpenFile(s.refLogPath(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("jsonl: refs log: %w", err)
	}
	defer f.Close()
	if tail.whole < tail.size {
		if err := f.Truncate(tail.whole); err != nil {
			return fmt.Errorf("jsonl: refs log: %w", err)
		}
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if err != nil {
		if terr := f.Truncate(tail.whole); terr != nil {
			return fmt.Errorf("jsonl: refs log: %w; and cutting it back: %v", err, terr)
		}
		return fmt.Errorf("jsonl: refs log: %w", err)
	}
	if tail.size == 0 {
		syncDir(s.root)
	}
	return nil
}

// writeRefsDoc replaces refs.json by a temporary file and a rename.
func (s *Store) writeRefsDoc(doc refsDoc) error {
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.root, ".refs-*")
	if err != nil {
		return fmt.Errorf("jsonl: refs: %w", err)
	}
	_, werr := tmp.Write(append(data, '\n'))
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Rename(tmp.Name(), s.refsPath())
	}
	if werr != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("jsonl: refs: %w", werr)
	}
	syncDir(s.root)
	return nil
}

var _ agentsession.RefStore = (*Store)(nil)
