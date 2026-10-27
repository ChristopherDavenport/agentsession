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
//
// A file in refs/ or logs/refs/ whose name begins %tmp- is crash
// debris: a write killed between its temporary file and the rename.
// Verify reports it as a leftover and Sweep removes it once older than
// its grace. One whose name begins %damaged- is a damaged log
// [Store.RepairRef] kept, which Verify reports until it is removed.

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

// oldVal is the state a record moved a ref from.
func (r refRecord) oldVal() refVal { return refVal{r.old(), r.OldIdent} }

// same reports whether two refs hold one target of one incarnation. A
// side with no identity, a ref or a record written without one, matches
// any.
func (v refVal) same(o refVal) bool {
	return v.RefTarget == o.RefTarget && (v.Ident == "" || o.Ident == "" || v.Ident == o.Ident)
}

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
	if last := tail.last; last != nil && last.oldVal().same(file) && !last.nextVal().same(file) {
		return last.nextVal(), tail, nil
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
	if t.Entry != "" {
		held, err := s.holdsPin(ctx, t)
		if err != nil {
			return t, err
		}
		if !held {
			return t, fmt.Errorf("%w: %s, which ref %s pins, is no longer in session %s", agentsession.ErrNoEntry, t.Entry, name, t.Session)
		}
	}
	return t, nil
}

// holdsPin reports whether the session a target names still holds the
// entry it pins. A ref written before the store committed pins first,
// or one whose pin a crash took with the session's working state, can
// name an entry recovery dropped: the ref then dangles as a ref to a
// deleted session does. The session's log answers without building the
// session when it records the entry's append as committed, or the
// entry is on its prefix; anything else is decided by a read.
func (s *Store) holdsPin(ctx context.Context, t agentsession.RefTarget) (bool, error) {
	if dir, err := s.sessionDir(t.Session); err == nil {
		if l, err := readSessionLog(dir, 0); err == nil && !l.legacy {
			synced, lostAt := -1, -1
			for i, r := range l.recs {
				switch {
				case r.Op == opSync:
					synced = i
				case r.Op == opLost && r.Entry == t.Entry:
					lostAt = i
				}
			}
			for i, r := range l.recs {
				if r.Op == opAppend && r.Entry == t.Entry && lostAt < i && (!r.Lazy || i < synced) {
					return true, nil
				}
			}
			if hdr, err := readHeader(dir); err == nil && hdr.Base != "" {
				if on, err := s.onPath(t.Entry, hdr.Base); err == nil && on {
					return true, nil
				}
			}
		}
	}
	sess, err := s.Read(ctx, t.Session)
	if err != nil {
		return false, err
	}
	_, ok := sess.Entry(t.Entry)
	return ok, nil
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

// refName is a ref the store holds, or one it cannot read: Err is set
// for a ref whose file or log's last record does not read, which is
// listed by name, so that it conflicts with a creation as a ref does
// and is reported rather than skipped.
type namedRef struct {
	Name string
	Val  refVal
	Err  error
}

// refNames lists the refs the store holds, in name order: every name
// with a ref file or a log, kept when the ref is not none as the file
// and the log's last record say together, or when it cannot be read. A
// creation recorded and not yet made is a ref, so it is listed,
// conflicts and keeps its pin through a sweep; a deletion recorded and
// not yet made is not, though its file is still there. A name is any a
// ref may have: files this store makes for its own use carry a "%" and
// are no names. One unreadable ref does not stop the listing.
func (s *Store) refNames() ([]namedRef, error) {
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
	var out []namedRef
	for n := range seen {
		v, _, err := s.effectiveRef(n)
		if err != nil || !v.IsZero() {
			out = append(out, namedRef{Name: n, Val: v, Err: err})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListRefs implements [agentsession.RefStore]. A ref that cannot be
// read is reported by name, with its error, in its place in the
// listing, and the listing goes on past it.
func (s *Store) ListRefs(ctx context.Context, prefix string) iter.Seq2[agentsession.Ref, error] {
	return func(yield func(agentsession.Ref, error) bool) {
		if err := agentsession.ValidRefPrefix(prefix); err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		refs, err := s.refNames()
		if err != nil {
			yield(agentsession.Ref{}, err)
			return
		}
		for _, r := range refs {
			n := r.Name
			if !strings.HasPrefix(n, prefix) {
				continue
			}
			if err := ctx.Err(); err != nil {
				yield(agentsession.Ref{}, err)
				return
			}
			v, _, err := s.effectiveRef(n)
			if err != nil {
				if !yield(agentsession.Ref{Name: n}, err) {
					return
				}
				continue
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

// lastNamed is what an unreadable ref last named, as far as anything of
// it reads: the target its file holds, and that of the last record of
// its log that reads. A sweep keeps both, since either may be the ref's.
func (s *Store) lastNamed(name string) []agentsession.RefTarget {
	var out []agentsession.RefTarget
	if v, err := s.readRefFile(name); err == nil && !v.IsZero() {
		out = append(out, v.RefTarget)
	}
	data, _ := os.ReadFile(s.refLogPath(name))
	lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
	for i := len(lines) - 1; i >= 0; i-- {
		if rec, err := decodeRefRecord(lines[i]); err == nil {
			if t := rec.next(); !t.IsZero() {
				out = append(out, t)
			}
			break
		}
	}
	return out
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
		// A ref is durable once placed, so the append of the entry it
		// pins is committed first: a crash cannot then leave the ref
		// pinning an entry its session no longer holds. That is done
		// before the sweep's lock is taken, as a fork commits its
		// base's origin.
		if err := s.commitPin(next); err != nil {
			return err
		}
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
	// Setting a ref to the target it holds changes nothing, and is
	// refused while the session under that ID is another since: a ref
	// is re-adopted by deleting it and setting it again.
	if cur.RefTarget == next {
		if !next.IsZero() && cur.Ident != "" && cur.Ident != nextIdent {
			return fmt.Errorf("%w: ref %s holds %s, which was created again after the ref was set; delete the ref and set it again to adopt the session now there", agentsession.ErrNoSession, name, next.Session)
		}
		return nil
	}
	if cur.IsZero() {
		refs, err := s.refNames()
		if err != nil {
			return err
		}
		names := make([]string, len(refs))
		for i, r := range refs {
			names[i] = r.Name
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

// commitPin commits the session whose log holds the entry a target
// pins, through that entry, or fails. An entry no session holds is
// left to the check that follows, which refuses it.
func (s *Store) commitPin(t agentsession.RefTarget) error {
	owner, held, err := s.holderOf(t.Entry, t.Session)
	if err != nil || !held {
		return err
	}
	if err := s.commitOrigin(owner, t.Entry); err != nil {
		return fmt.Errorf("cas: ref pins %s, whose append could not be committed: %w", t.Entry, err)
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
	case last == nil && file.IsZero(), last != nil && last.nextVal().same(file):
		return file, nil
	case last != nil && last.oldVal().same(file):
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

// refDamaged prefixes a damaged ref log a repair set aside in
// logs/refs, followed by the ref's file name and the time.
const refDamaged = "%damaged-"

// refDebris lists the temporary files a crash left in refs/ and
// logs/refs/ between a write and its rename. They are no refs, since a
// "%" is no name character, and a sweep removes them once older than
// its grace.
func (s *Store) refDebris() []string {
	var out []string
	for _, dir := range []string{refsDir, refLogsDir} {
		m, _ := filepath.Glob(filepath.Join(s.root, dir, refTmp+"*"))
		out = append(out, m...)
	}
	return out
}

// refLogDamage reads a ref's whole log and returns the damage in it: a
// whole line that fails its checksum or names no ref. A last line with
// no newline is a record being written, or one a crash cut, and no
// damage.
func (s *Store) refLogDamage(name string) ([]error, []byte, error) {
	data, err := os.ReadFile(s.refLogPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("cas: ref %s log: %w", name, err)
	}
	whole := data
	if i := bytes.LastIndexByte(whole, '\n'); i < 0 {
		whole = nil
	} else {
		whole = whole[:i+1]
	}
	var damage []error
	for i, line := range bytes.Split(bytes.TrimRight(whole, "\n"), []byte{'\n'}) {
		if len(whole) == 0 {
			break
		}
		if _, err := decodeRefRecord(line); err != nil {
			damage = append(damage, fmt.Errorf("%w: ref %s log line %d: %v", ErrCorrupt, name, i+1, err))
		}
	}
	return damage, data, nil
}

// ErrRefNotDamaged is returned by RepairRef for a ref whose file and
// log read whole: there is nothing to repair.
var ErrRefNotDamaged = errors.New("cas: the ref and its log are not damaged; there is nothing to repair")

// RefRepair is what RepairRef did.
type RefRepair struct {
	// Damage is each damaged line of the ref's log, and the ref file's
	// own damage when it did not read.
	Damage []error
	// Target is the target the ref holds after the repair, which the
	// restarted log's one record names; zero when the ref is none.
	Target agentsession.RefTarget
	// DamagedLog is where the damaged log is kept, in logs/refs; empty
	// when the ref had no log.
	DamagedLog string
}

// RepairRef restarts a damaged ref's log from the ref's current target,
// RFC 0002's repair of a ref. The current target is what the ref's file
// holds, which was the ref's until the damaged record, or, when the file
// does not read, what the log's last readable record names. The damaged
// log is kept in logs/refs under a name no ref has, %damaged-<ref>-<time>,
// which Verify reports until someone removes it, and the new log holds
// one record, from no target to the current one, whose reason says it
// was recovered; the ref file is written again when it did not read. The
// ref's history up to the damage is in the kept log alone.
//
// It holds the ref's lock, refuses a read-only or stopped store, and
// returns ErrRefNotDamaged for a ref whose file and log read whole.
func (s *Store) RepairRef(ctx context.Context, name string) (RefRepair, error) {
	var rep RefRepair
	if err := agentsession.ValidRefName(name); err != nil {
		return rep, err
	}
	if err := ctx.Err(); err != nil {
		return rep, err
	}
	if err := s.writable(); err != nil {
		return rep, err
	}
	for _, d := range []string{refsDir, refLogsDir, refLocksDir} {
		if err := os.MkdirAll(filepath.Join(s.root, d), 0o755); err != nil {
			return rep, fmt.Errorf("cas: refs: %w", err)
		}
	}
	lk, err := lockBlocking(ctx, filepath.Join(s.root, refLocksDir, refFile(name)))
	if err != nil {
		return rep, err
	}
	defer lk.release()
	damage, data, err := s.refLogDamage(name)
	if err != nil {
		return rep, err
	}
	file, ferr := s.readRefFile(name)
	if ferr != nil {
		damage = append(damage, ferr)
	}
	rep.Damage = damage
	if len(damage) == 0 {
		return rep, fmt.Errorf("%w: %s", ErrRefNotDamaged, name)
	}
	cur := file
	if ferr != nil {
		cur = refVal{}
		lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte{'\n'})
		for i := len(lines) - 1; i >= 0; i-- {
			if rec, err := decodeRefRecord(lines[i]); err == nil {
				cur = rec.nextVal()
				break
			}
		}
	}
	rep.Target = cur.RefTarget
	logDir := filepath.Join(s.root, refLogsDir)
	if data != nil {
		aside := filepath.Join(logDir, fmt.Sprintf("%s%s-%d", refDamaged, refFile(name), time.Now().UnixNano()))
		if err := s.writeRefFile(aside, data); err != nil {
			return rep, fmt.Errorf("cas: ref %s: keeping its damaged log: %w", name, err)
		}
		rep.DamagedLog = aside
	}
	var fresh []byte
	if !cur.IsZero() {
		rec := newRefRecord(name, agentsession.RefTarget{}, cur.RefTarget, time.Now(), "recovered: the ref's log was damaged and restarted from the ref's target")
		rec.NewIdent = cur.Ident
		if fresh, err = rec.encode(); err != nil {
			return rep, err
		}
	}
	if err := s.writeRefFile(s.refLogPath(name), fresh); err != nil {
		return rep, fmt.Errorf("cas: ref %s log: %w", name, err)
	}
	if ferr != nil {
		if err := s.placeRef(name, cur); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

var _ agentsession.RefStore = (*Store)(nil)
