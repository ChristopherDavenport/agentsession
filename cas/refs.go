package cas

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

// Refs are the store's names for sessions, RFC 0002's refs section. A
// name is slash-separated, and the store keeps each as one file in a
// flat directory, the name with "/" written "%", a character no name
// has, so a ref and a ref that was once under it cannot meet as a file
// and a directory, and a name needs no directory tree to be removed:
//
//	refs/<name>            the ref: "<session>\n", or "<session> <entry>\n" when it pins an entry
//	logs/refs/<name>       the ref's log, a record a line, each with a checksum
//	reflocks/<name>        the lock of the ref's update
//	refs.lock              the lock of a creation, which looks at every name
//
// An update takes the ref's lock, and a creation takes refs.lock first.
// Under them it reads the ref, compares it with the expected target,
// appends the update's record to the log and fsyncs it, and then
// replaces the ref file by rename, durably: the log is written ahead,
// so an update the store cannot log is not accepted, and one that is
// recorded is made. A crash between the two steps leaves the ref one
// record behind its log; the next update of the ref under its lock
// finishes it, and a read of the ref sees the record's target at once,
// as the log says. A ref file changed with no record, by a crash with
// no write-ahead or by hand, is logged by the next update.

// refStep is called at the steps of an update that a test holds still
// to make another caller's arrive between them.
var refStep = func(name, stage string) {}

const (
	refsDir     = "refs"
	refLogsDir  = "logs/refs"
	refLocksDir = "reflocks"
	refsLock    = "refs.lock"
	refOp       = "ref"
)

// refFile is a ref's name as a file name.
func refFile(name string) string { return strings.ReplaceAll(name, "/", "%") }

// refName is a file name as a ref's name.
func refName(file string) string { return strings.ReplaceAll(file, "%", "/") }

// refRecord is one line of a ref's log, which carries a checksum as a
// session's log records do.
type refRecord struct {
	Op         string `json:"op"`
	Name       string `json:"name"`
	OldSession string `json:"old_session,omitempty"`
	OldEntry   string `json:"old_entry,omitempty"`
	NewSession string `json:"new_session,omitempty"`
	NewEntry   string `json:"new_entry,omitempty"`
	// OldIdent and NewIdent are the targets' sessions' HeaderIdent when
	// each was set, which the store compares at resolution and no
	// caller sees.
	OldIdent string `json:"old_ident,omitempty"`
	NewIdent string `json:"new_ident,omitempty"`
	At       string `json:"at"`
	Reason   string `json:"reason,omitempty"`
}

func (r refRecord) old() agentsession.RefTarget {
	return agentsession.RefTarget{Session: r.OldSession, Entry: r.OldEntry}
}

func (r refRecord) next() agentsession.RefTarget {
	return agentsession.RefTarget{Session: r.NewSession, Entry: r.NewEntry}
}

// refVal is a ref's target with the identity of its session's
// incarnation, which only the store keeps and compares.
type refVal struct {
	agentsession.RefTarget
	Ident string
}

func (r refRecord) nextVal() refVal { return refVal{r.next(), r.NewIdent} }

func newRefRecord(name string, old, next agentsession.RefTarget, at time.Time, reason string) refRecord {
	return refRecord{
		Op: refOp, Name: name,
		OldSession: old.Session, OldEntry: old.Entry,
		NewSession: next.Session, NewEntry: next.Entry,
		At: at.UTC().Format(time.RFC3339Nano), Reason: reason,
	}
}

// encode renders the record as a log line, ending in a CRC-32C of the
// bytes before it.
func (r refRecord) encode() ([]byte, error) {
	data, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	body := data[:len(data)-1]
	line := append([]byte(nil), body...)
	line = append(line, crcMember...)
	line = strconv.AppendUint(line, uint64(crc32.Checksum(body, crcTable)), 16)
	return append(line, '"', '}', '\n'), nil
}

var errRefTorn = errors.New("cut short")

// decodeRefRecord reads one line, newline included or not.
func decodeRefRecord(line []byte) (refRecord, error) {
	var r refRecord
	line = bytes.TrimRight(line, "\n")
	if len(line) == 0 || line[len(line)-1] != '}' {
		return r, errRefTorn
	}
	k := bytes.LastIndex(line, []byte(crcMember))
	if k < 0 {
		return r, errors.New("no checksum")
	}
	tail := line[k+len(crcMember):]
	if len(tail) < 3 || tail[len(tail)-2] != '"' {
		return r, errors.New("malformed checksum")
	}
	want, err := strconv.ParseUint(string(tail[:len(tail)-2]), 16, 32)
	if err != nil {
		return r, errors.New("malformed checksum")
	}
	if crc32.Checksum(line[:k], crcTable) != uint32(want) {
		return r, errors.New("checksum mismatch")
	}
	if err := json.Unmarshal(line, &r); err != nil {
		return r, err
	}
	if r.Op != refOp || r.Name == "" {
		return r, errors.New("record names no ref")
	}
	return r, nil
}

func (r refRecord) update() agentsession.RefUpdate {
	at, _ := time.Parse(time.RFC3339Nano, r.At)
	return agentsession.RefUpdate{Name: r.Name, Old: r.old(), New: r.next(), Time: at, Reason: r.Reason}
}

// encodeRefTarget renders a ref file: the session, the pinned entry or
// "-", and the incarnation's identity or "-", separated by spaces.
func encodeRefTarget(v refVal) []byte {
	return []byte(v.Session + " " + orDashRef(v.Entry) + " " + orDashRef(v.Ident) + "\n")
}

func orDashRef(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func decodeRefTarget(data []byte) (refVal, error) {
	f := strings.Fields(string(data))
	bad := func() (refVal, error) {
		return refVal{}, fmt.Errorf("%w: ref file holds %q", ErrCorrupt, strings.TrimSpace(string(data)))
	}
	if len(f) < 1 || len(f) > 3 || !validSessionID(f[0]) {
		return bad()
	}
	v := refVal{RefTarget: agentsession.RefTarget{Session: f[0]}}
	if len(f) > 1 && f[1] != "-" {
		if !agentsession.ValidHash(f[1]) {
			return bad()
		}
		v.Entry = f[1]
	}
	if len(f) > 2 && f[2] != "-" {
		v.Ident = f[2]
	}
	return v, nil
}

// readRefFile reads a ref's file: the zero target when there is none.
func (s *Store) readRefFile(name string) (refVal, error) {
	data, err := os.ReadFile(filepath.Join(s.root, refsDir, refFile(name)))
	if errors.Is(err, os.ErrNotExist) {
		return refVal{}, nil
	}
	if err != nil {
		return refVal{}, fmt.Errorf("cas: ref %s: %w", name, err)
	}
	t, err := decodeRefTarget(data)
	if err != nil {
		return t, fmt.Errorf("cas: ref %s: %w", name, err)
	}
	return t, nil
}

func (s *Store) refLogPath(name string) string {
	return filepath.Join(s.root, refLogsDir, refFile(name))
}

// refTail is the last record of a ref's log, and where its whole lines
// end: a log that ends in a cut line is cut back to there by an
// updater.
type refTail struct {
	last  *refRecord
	whole int64
	size  int64
}

// readRefTail reads the end of the ref's log, enough of it to hold the
// last whole record. A line that is whole and does not read is damage.
func (s *Store) readRefTail(name string) (refTail, error) {
	var t refTail
	f, err := os.Open(s.refLogPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return t, nil
	}
	if err != nil {
		return t, fmt.Errorf("cas: ref %s log: %w", name, err)
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
		// Drop a cut last line: it is a record being written, or one a
		// crash cut.
		end := len(buf)
		if i := bytes.LastIndexByte(buf, '\n'); i < 0 {
			end = 0
		} else if i != len(buf)-1 {
			end = i + 1
		}
		t.whole = start + int64(end)
		buf = buf[:end]
		lines := bytes.Split(bytes.TrimRight(buf, "\n"), []byte{'\n'})
		// The first line may be the middle of one when the read began
		// after the start of the file.
		if start > 0 {
			lines = lines[1:]
		}
		if len(lines) == 0 || (len(lines) == 1 && len(lines[0]) == 0) {
			if start == 0 {
				return t, nil
			}
			continue
		}
		rec, err := decodeRefRecord(lines[len(lines)-1])
		if err != nil {
			return t, fmt.Errorf("%w: ref %s log: %v", ErrCorrupt, name, err)
		}
		t.last = &rec
		return t, nil
	}
}

// effectiveRef is the target a ref holds as the file and the end of
// its log say it does together, with the log's tail. The ref file is
// read first. When the log's last record moves the target the file
// holds, the record is the update's commit and the file lags it, so
// the record's target is the ref's.
func (s *Store) effectiveRef(name string) (refVal, refTail, error) {
	file, err := s.readRefFile(name)
	if err != nil {
		return file, refTail{}, err
	}
	tail, err := s.readRefTail(name)
	if err != nil {
		return file, tail, err
	}
	if tail.last != nil && tail.last.old() == file.RefTarget && tail.last.next() != file.RefTarget {
		return tail.last.nextVal(), tail, nil
	}
	return file, tail, nil
}

// ResolveRef implements [agentsession.RefStore].
func (s *Store) ResolveRef(ctx context.Context, name string) (agentsession.RefTarget, error) {
	if err := agentsession.ValidRefName(name); err != nil {
		return agentsession.RefTarget{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentsession.RefTarget{}, err
	}
	v, _, err := s.effectiveRef(name)
	if err != nil {
		return agentsession.RefTarget{}, err
	}
	t := v.RefTarget
	if t.IsZero() {
		return t, fmt.Errorf("%w: %s", agentsession.ErrNoRef, name)
	}
	ident, ok := s.incarnation(t.Session)
	if !ok {
		return t, fmt.Errorf("%w: %s, which ref %s names", agentsession.ErrNoSession, t.Session, name)
	}
	if v.Ident != "" && ident != v.Ident {
		return t, fmt.Errorf("%w: %s was created again after ref %s was set", agentsession.ErrNoSession, t.Session, name)
	}
	return t, nil
}

// incarnation is the identity of the session now under id, and false
// when none is.
func (s *Store) incarnation(id string) (string, bool) {
	dir, err := s.sessionDir(id)
	if err != nil {
		return "", false
	}
	h, err := readHeader(dir)
	if err != nil {
		return "", false
	}
	ident, err := agentsession.HeaderIdent(h)
	return ident, err == nil
}

// refNames lists the names of the refs the store holds: every name with
// a ref file or a log, kept when the ref is not none as the file and the
// log's last record say together. A creation recorded and not yet made
// is a ref, so it is listed, conflicts and keeps its pin through a
// sweep; a deletion recorded and not yet made is not, though its file
// is still there. A name is any a ref may have: files this store makes
// for its own use carry a "%" and are no names.
func (s *Store) refNames() ([]string, error) {
	seen := map[string]bool{}
	for _, dir := range []string{refsDir, refLogsDir} {
		ents, err := os.ReadDir(filepath.Join(s.root, dir))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("cas: refs: %w", err)
		}
		for _, e := range ents {
			if n := refName(e.Name()); !e.IsDir() && agentsession.ValidRefName(n) == nil {
				seen[n] = true
			}
		}
	}
	var out []string
	for n := range seen {
		v, _, err := s.effectiveRef(n)
		if err != nil {
			return nil, err
		}
		if !v.IsZero() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// ListRefs implements [agentsession.RefStore].
func (s *Store) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	return func(yield func(agentsession.Ref, error) bool) {
		if err := agentsession.ValidRefPrefix(prefix); err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		names, err := s.refNames()
		if err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		for _, n := range names {
			if !strings.HasPrefix(n, prefix) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(agentsession.Ref{}, err)
				return
			}
			v, _, err := s.effectiveRef(n)
			if err != nil {
				yield(agentsession.Ref{}, err)
				return
			}
			if v.IsZero() {
				continue // deleted since the directory was read
			}
			if !yield(agentsession.Ref{Name: n, Target: v.RefTarget}, nil) {
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
		data, err := os.ReadFile(s.refLogPath(name))
		if errors.Is(err, os.ErrNotExist) {
			return
		}
		if err != nil {
			yield(agentsession.RefUpdate{}, fmt.Errorf("cas: ref %s log: %w", name, err))
			return
		}
		// A last line with no newline is a record being written.
		if i := bytes.LastIndexByte(data, '\n'); i < 0 {
			return
		} else {
			data = data[:i+1]
		}
		lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
		for i := len(lines) - 1; i >= 0; i-- {
			rec, err := decodeRefRecord(lines[i])
			if err != nil {
				yield(agentsession.RefUpdate{}, fmt.Errorf("%w: ref %s log line %d: %v", ErrCorrupt, name, i+1, err))
				return
			}
			if err := ctx.Err(); err != nil {
				yield(agentsession.RefUpdate{}, err)
				return
			}
			if !yield(rec.update(), nil) {
				return
			}
		}
	}
}

// UpdateRef implements [agentsession.RefStore].
func (s *Store) UpdateRef(ctx context.Context, name string, expected, next agentsession.RefTarget, reason string) error {
	if err := agentsession.ValidRefName(name); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	if next.Session != "" && !validSessionID(next.Session) {
		return fmt.Errorf("%w: session id %q", ErrBadName, next.Session)
	}
	if next.Session == "" && next.Entry != "" {
		return fmt.Errorf("%w: a pinned entry without a session", ErrBadName)
	}
	for _, d := range []string{refsDir, refLogsDir, refLocksDir} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			return fmt.Errorf("cas: refs: %w", err)
		}
	}
	creating := expected.IsZero() && !next.IsZero()
	if creating {
		lk, err := lockBlocking(ctx, filepath.Join(s.root, refsLock))
		if err != nil {
			return err
		}
		defer lk.release()
	}
	lk, err := lockBlocking(ctx, filepath.Join(s.root, refLocksDir, refFile(name)))
	if err != nil {
		return err
	}
	defer lk.release()
	if next.Entry != "" {
		// A sweep's last step does not run between the check that the
		// session holds the entry and the record that names it.
		g, err := s.writeGuard(ctx)
		if err != nil {
			return err
		}
		defer g.release()
	}

	cur, err := s.mendRef(name)
	if err != nil {
		return err
	}
	refStep(name, "read")
	if cur.RefTarget != expected {
		return &agentsession.RefMovedError{Name: name, Current: cur.RefTarget}
	}
	var nextIdent string
	if !next.IsZero() {
		var ok bool
		if nextIdent, ok = s.incarnation(next.Session); !ok {
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
	// Setting a ref to the target it holds changes nothing, unless the
	// session under that ID is another since: then it adopts it.
	if cur.RefTarget == next && cur.Ident == nextIdent {
		return nil
	}
	if cur.IsZero() {
		names, err := s.refNames()
		if err != nil {
			return err
		}
		if err := agentsession.RefConflict(name, names); err != nil {
			return err
		}
	}
	refStep(name, "checked")
	rec := newRefRecord(name, cur.RefTarget, next, time.Now(), reason)
	rec.OldIdent, rec.NewIdent = cur.Ident, nextIdent
	tail, err := s.readRefTail(name)
	if err != nil {
		return err
	}
	if err := s.appendRefRecord(name, tail, rec); err != nil {
		return err
	}
	if err := s.placeRef(name, refVal{next, nextIdent}); err != nil {
		// The record is cut back, so an update that failed is not one
		// the log says was made; where that fails too, the next update
		// finishes what the record says.
		if terr := os.Truncate(s.refLogPath(name), tail.whole); terr != nil {
			return fmt.Errorf("cas: ref %s: %w; and cutting its record back: %v", name, err, terr)
		}
		return err
	}
	return nil
}

// mendRef brings a ref and its log into step, under the ref's lock, and
// returns the target the ref holds. A log whose last record moves the
// ref from where the file is to another target is an update that was
// recorded and not made, and is made. A file the last record does not
// account for is an update that was made and not recorded, and is
// recorded.
func (s *Store) mendRef(name string) (refVal, error) {
	file, err := s.readRefFile(name)
	if err != nil {
		return file, err
	}
	tail, err := s.readRefTail(name)
	if err != nil {
		return file, err
	}
	switch last := tail.last; {
	case last == nil && file.IsZero(), last != nil && last.next() == file.RefTarget:
		return file, nil
	case last != nil && last.old() == file.RefTarget:
		// Recorded, not made.
		return last.nextVal(), s.placeRef(name, last.nextVal())
	}
	var from refVal
	if tail.last != nil {
		from = tail.last.nextVal()
	}
	rec := newRefRecord(name, from.RefTarget, file.RefTarget, time.Now(), "recovered: the ref changed with no record of it")
	rec.OldIdent, rec.NewIdent = from.Ident, file.Ident
	return file, s.appendRefRecord(name, tail, rec)
}

// appendRefRecord appends rec to the ref's log durably, first cutting a
// last line a crash left cut.
func (s *Store) appendRefRecord(name string, tail refTail, rec refRecord) error {
	data, err := rec.encode()
	if err != nil {
		return err
	}
	path := s.refLogPath(name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("cas: ref %s log: %w", name, err)
	}
	defer f.Close()
	if tail.whole < tail.size {
		if err := f.Truncate(tail.whole); err != nil {
			return fmt.Errorf("cas: ref %s log: %w", name, err)
		}
	}
	before := tail.whole
	_, err = f.Write(data)
	if err == nil {
		err = s.objs.fsync(path, f, syncLog)
	}
	if err != nil {
		if terr := f.Truncate(before); terr != nil {
			return fmt.Errorf("cas: ref %s log: %w; and cutting it back: %v", name, err, terr)
		}
		return fmt.Errorf("cas: ref %s log: %w", name, err)
	}
	if tail.size == 0 {
		return s.objs.fsyncDir(filepath.Dir(path))
	}
	return nil
}

// placeRef makes the ref file hold t, durably: replaced by rename, or
// removed for none.
func (s *Store) placeRef(name string, t refVal) error {
	path := filepath.Join(s.root, refsDir, refFile(name))
	if t.IsZero() {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("cas: ref %s: %w", name, err)
		}
		return s.objs.fsyncDir(filepath.Dir(path))
	}
	if err := s.writeRefFile(path, encodeRefTarget(t)); err != nil {
		return fmt.Errorf("cas: ref %s: %w", name, err)
	}
	return nil
}

// refTmp prefixes the temporary files a ref is written through. A ref
// name cannot hold a "%", which is how a name's "/" is written, so no
// temporary file is taken for a ref and no ref for a temporary file.
const refTmp = "%tmp-"

// writeRefFile replaces the file at path with data through a temporary
// file in its directory, fsynced before the rename and the directory
// after.
func (s *Store) writeRefFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, refTmp+"*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if err == nil {
		err = s.objs.fsync(tmp.Name(), tmp, syncFile)
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return s.objs.fsyncDir(dir)
}

var _ agentsession.RefStore = (*Store)(nil)
