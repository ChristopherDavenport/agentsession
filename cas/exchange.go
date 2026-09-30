package cas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/ChristopherDavenport/agentsession"
)

// ErrNotRecord is returned by Push from a store that is not the record
// for the session: RFC 0002 lets only the record initiate a push.
var ErrNotRecord = errors.New("cas: only the record may push a session")

// ErrHeaderDiffers is returned when an exchange names a session the
// receiver holds under a different header: two sessions alike only in
// ID are not one session.
var ErrHeaderDiffers = errors.New("cas: the receiver holds a different session under this ID")

// Exchange reports what a push or a fetch did at the receiver.
type Exchange struct {
	// Created is set when the receiver lacked the session.
	Created bool
	// Admitted is how many of the session's own entries the receiver
	// took in; the rest it held already.
	Admitted int
	// Head is the receiver's head afterwards, and HeadMoved whether the
	// exchange moved it. Forced is set when a push moved it although
	// the head was not the expected one.
	Head      string
	HeadMoved bool
	Forced    bool
	// Why says why the head did not move, when it did not.
	Why string
	// Handover is set when a push made the receiver the record and the
	// sender a mirror.
	Handover bool
}

// PushOptions are a push's compare-and-swap and its overrides.
type PushOptions struct {
	// Expected is the receiver's head as the sender last saw it: ""
	// when the sender believes the receiver lacks the session or holds
	// it with no head, or the base, which is the head a fork the
	// receiver lacked starts with. A push whose expected value is wrong
	// lands its entries as a branch and leaves the head, which is what
	// reveals two stores that each believed they were the record.
	Expected string
	// Force moves the head whatever it is, and is reported as such.
	Force bool
	// Handover makes the receiver the record and then this store a
	// mirror, each in its own commit and in that order, so a failure
	// between the two leaves two records and never none.
	Handover bool
}

// bundle is what an exchange carries: the closure of one session.
type bundle struct {
	header agentsession.Header
	mark   string
	head   string
	prefix []agentsession.Entry // the path to the base, root first
	own    []agentsession.Entry // in the sender's log order
	blobs  map[string][]byte
}

// bundleOf reads a session's closure: its own entries and their
// contents, its prefix and its media blobs, as RFC 0002's push carries.
func (s *Store) bundleOf(ctx context.Context, id string) (*bundle, error) {
	h, err := s.hold(id)
	if err != nil {
		return nil, err
	}
	defer h.mu.Unlock()
	if err := s.untouched(h); err != nil {
		return nil, err
	}
	// A sender commits the session before it pushes or serves it, so a
	// receiver never holds what a crash here could take back. A
	// read-only store cannot commit another process's working state, and
	// serves what that process's log shows committed: a durable append
	// whose fsync is still in flight reads as committed, and the writer
	// takes it back if the fsync fails.
	sess, head, mark := h.session, h.head, h.mark
	if !s.readOnly {
		if err := s.commitHandle(id, h); err != nil {
			return nil, err
		}
	} else {
		v, err := s.committedView(id, h.dir)
		if err != nil {
			return nil, err
		}
		if sess, _, err = s.build(id, h.dir, v); err != nil {
			return nil, err
		}
		head, mark = v.head, v.mark
	}
	hdr, err := readHeader(h.dir)
	if err != nil {
		return nil, err
	}
	b := &bundle{header: hdr, mark: mark, head: head, blobs: map[string][]byte{}}
	for _, e := range sess.Entries() {
		if sess.Prefix(e.Base().ID) {
			b.prefix = append(b.prefix, e)
		} else {
			b.own = append(b.own, e)
		}
		_, body, err := split(e)
		if err != nil {
			return nil, err
		}
		for _, hash := range blobsNamedBy(body) {
			if _, ok := b.blobs[hash]; ok {
				continue
			}
			data, err := s.Blob(ctx, hash)
			if err != nil {
				return nil, err
			}
			b.blobs[hash] = data
		}
	}
	return b, nil
}

// Push sends a session this store is the record for to another store,
// as RFC 0002's exchange section describes. The receiver admits what it
// lacks, parent first, and merges the log as a set; its head then moves
// by compare-and-swap from opts.Expected, or by force. The entries land
// whether or not the head moves. A receiver that lacks the session
// creates it from the pushed header as a mirror, or as the record for a
// handover. The two stores' mutexes are never held together. A session
// the receiver has open is rebuilt from what it holds, so a caller
// holding it there opens it again.
func (s *Store) Push(ctx context.Context, to *Store, id string, opts PushOptions) (Exchange, error) {
	if err := ctx.Err(); err != nil {
		return Exchange{}, err
	}
	if s.sameStore(to) {
		return Exchange{}, ErrSameStore
	}
	b, err := s.bundleOf(ctx, id)
	if err != nil {
		return Exchange{}, err
	}
	if b.mark != MarkRecord {
		return Exchange{}, fmt.Errorf("%w: this store holds %s as a mirror", ErrNotRecord, id)
	}
	x, err := to.receive(ctx, b, receiveOptions{push: true, expected: opts.Expected, force: opts.Force, handover: opts.Handover})
	if err != nil || !opts.Handover {
		return x, err
	}
	// The receiver is the record now; clear the mark here.
	h, err := s.hold(id)
	if err != nil {
		return x, fmt.Errorf("cas: handover: the receiver is the record, and this store could not clear its mark: %w", err)
	}
	defer h.mu.Unlock()
	if err := s.setMark(h, id, MarkMirror); err != nil {
		return x, fmt.Errorf("cas: handover: the receiver is the record, and this store could not clear its mark: %w", err)
	}
	x.Handover = true
	return x, nil
}

// Fetch takes a session from another store that holds it, a mirror
// included. What it lacks is admitted as a push's entries are. The head
// moves only when this store is a mirror of the session and the fetched
// head descends from its own, or it has none; a record's head moves
// only by its own writers, and a fetch from a stale mirror moves
// nothing. A session this store lacked is created as a mirror.
func (s *Store) Fetch(ctx context.Context, from *Store, id string) (Exchange, error) {
	if err := ctx.Err(); err != nil {
		return Exchange{}, err
	}
	if s.sameStore(from) {
		return Exchange{}, ErrSameStore
	}
	b, err := from.bundleOf(ctx, id)
	if err != nil {
		return Exchange{}, err
	}
	return s.receive(ctx, b, receiveOptions{})
}

// ErrSameStore is an exchange between a store and itself, whether one
// Store or two opened on one directory: it has one session to both send
// and receive, and a handover would leave no store the record.
var ErrSameStore = errors.New("cas: an exchange needs two stores; this one is both")

// sameStore reports whether two Stores are one store.
func (s *Store) sameStore(o *Store) bool {
	if s == o {
		return true
	}
	a, err1 := os.Stat(s.root)
	b, err2 := os.Stat(o.root)
	return err1 == nil && err2 == nil && os.SameFile(a, b)
}

type receiveOptions struct {
	push     bool
	expected string
	force    bool
	handover bool
}

// sameSession compares two headers apart from format, which a later
// minor raises.
func sameSession(a, b agentsession.Header) bool {
	a.Format, b.Format = "", ""
	ja, err1 := json.Marshal(a)
	jb, err2 := json.Marshal(b)
	return err1 == nil && err2 == nil && reflect.DeepEqual(ja, jb)
}

// laterFormat reports whether format a names a later minor than b.
func laterFormat(a, b string) bool {
	_, ma, err1 := agentsession.ParseFormat(a)
	_, mb, err2 := agentsession.ParseFormat(b)
	return err1 == nil && (err2 != nil || ma > mb)
}

// descends reports whether the path to head passes through anc.
func descends(entries map[string]agentsession.Entry, head, anc string) bool {
	for id := head; id != ""; {
		if id == anc {
			return true
		}
		e, ok := entries[id]
		if !ok {
			return false
		}
		id = e.Base().Parent
	}
	return false
}

// receive admits a bundle.
func (s *Store) receive(ctx context.Context, b *bundle, o receiveOptions) (Exchange, error) {
	if s.readOnly {
		return Exchange{}, errReadOnly()
	}
	if o.push && b.mark != MarkRecord {
		return Exchange{}, ErrNotRecord
	}
	if laterFormat(b.header.Format, agentsession.Format) {
		return Exchange{}, fmt.Errorf("%w: %s is later than this store's %s", agentsession.ErrUnsupportedFormat, b.header.Format, agentsession.Format)
	}
	id := b.header.ID
	dir, err := s.sessionDir(id)
	if err != nil {
		return Exchange{}, err
	}
	all := map[string]agentsession.Entry{}
	for _, e := range append(append([]agentsession.Entry(nil), b.prefix...), b.own...) {
		all[e.Base().ID] = e
	}
	h, err := s.hold(id)
	if errors.Is(err, agentsession.ErrNoSession) {
		mark := MarkMirror
		// A fresh session's head is its base, or none: the value a push's
		// compare-and-swap is held to.
		fresh := b.header.Base
		x := Exchange{Created: true, Admitted: len(b.own), Head: fresh}
		head := fresh
		switch {
		case b.head == fresh:
		case !o.push || o.expected == fresh:
			head, x.HeadMoved = b.head, true
		case o.force:
			head, x.HeadMoved, x.Forced = b.head, true, true
		default:
			x.Why = fmt.Sprintf("the receiver's head is %s, not the expected %s", orNone(fresh), orNone(o.expected))
		}
		if o.handover {
			if b.head != head {
				return Exchange{}, fmt.Errorf("%w: a handover whose head does not move is refused: %s", ErrHeadMoved, x.Why)
			}
			mark = MarkRecord
		}
		stored := append(append([]agentsession.Entry(nil), b.prefix...), b.own...)
		slot, held := s.claim(id)
		if held {
			slot.mu.Unlock()
			return Exchange{}, fmt.Errorf("%w: %s", agentsession.ErrSessionExists, id)
		}
		n, err := s.admitNew(ctx, dir, b.header, mark, stored, b.own, b.blobs, head)
		if err != nil {
			s.abandon(id, slot)
			return Exchange{}, err
		}
		slot.take(n)
		slot.mu.Unlock()
		x.Head = head
		return x, nil
	}
	if err != nil {
		return Exchange{}, err
	}
	defer h.mu.Unlock()
	hdr, err := readHeader(h.dir)
	if err != nil {
		return Exchange{}, err
	}
	if !sameSession(hdr, b.header) {
		return Exchange{}, fmt.Errorf("%w: %s", ErrHeaderDiffers, id)
	}
	if err := s.untouched(h); err != nil {
		return Exchange{}, err
	}
	// The log merges as a set, parent first: each own entry the receiver
	// lacks must hang from the base, an own entry of the union, or null
	// in a session with no base.
	held := map[string]bool{}
	for _, e := range h.session.Entries() {
		if !h.session.Prefix(e.Base().ID) {
			held[e.Base().ID] = true
		}
	}
	var fresh []agentsession.Entry
	for _, e := range b.own {
		eid := e.Base().ID
		if held[eid] {
			continue
		}
		p := e.Base().Parent
		switch {
		case p == "" && b.header.Base == "":
		case p != "" && (p == b.header.Base || held[p]):
		default:
			return Exchange{}, fmt.Errorf("cas: exchange: own entry %s hangs from %s, which is neither the base nor an own entry", eid, orNone(p))
		}
		held[eid] = true
		fresh = append(fresh, e)
	}
	x := Exchange{Admitted: len(fresh), Head: h.head}
	failedHandover := false
	newHead := h.head
	switch {
	case b.head == h.head:
	case o.push && (o.expected == h.head || o.force):
		newHead, x.HeadMoved, x.Forced = b.head, true, o.expected != h.head
	case o.push:
		x.Why = fmt.Sprintf("the receiver's head is %s, not the expected %s", orNone(h.head), orNone(o.expected))
	case h.mark == MarkRecord:
		x.Why = "this store is the record; its head moves only by its own writers"
	case h.head == "" || descends(all, b.head, h.head):
		newHead, x.HeadMoved = b.head, true
	default:
		x.Why = "the fetched head does not descend from this store's; the source is stale or the record moved back"
	}
	if o.handover && newHead != b.head {
		// The compare-and-swap is what reveals two records; a handover
		// that fails it changes no mark, and its entries still land.
		o.handover = false
		failedHandover = true
	}
	if x.HeadMoved {
		target, ok := all[newHead]
		if l, isLabel := target.(*agentsession.LabelEntry); ok && isLabel && l.Label != nil && *l.Label == agentsession.LeafLabel {
			return Exchange{}, errors.New("cas: exchange: the head never rests on a leaf label")
		}
	}
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return Exchange{}, err
	}
	defer guard.release()
	pend := newPendSet()
	sizes, err := s.packEntries(append(append([]agentsession.Entry(nil), b.prefix...), fresh...), b.blobs, pend)
	if err != nil {
		return Exchange{}, err
	}
	if laterFormat(b.header.Format, hdr.Format) {
		hdr.Format = b.header.Format
		if err := writeHeader(h.dir, hdr); err != nil {
			return Exchange{}, err
		}
	}
	var recs []logRecord
	var hashes []string
	seq := h.session.Len() - len(b.prefix)
	for _, e := range fresh {
		seq++
		eid := e.Base().ID
		hashes = append(hashes, eid)
		recs = append(recs, logRecord{Op: opAppend, Session: id, Entry: eid, Seq: seq, Size: sizes[eid]})
	}
	if x.HeadMoved {
		recs = append(recs, logRecord{Op: opHead, Session: id, Head: newHead, Seq: seq})
	}
	if o.handover {
		recs = append(recs, logRecord{Op: opMark, Session: id, Mark: MarkRecord})
	}
	if len(recs) > 0 {
		// Committed before it is acknowledged, as RFC 0002 requires of
		// what a receiver admits and a mark it sets: the objects
		// packEntries left loose first, then the log.
		if err := s.objs.flushSet(pend); err != nil {
			return Exchange{}, err
		}
		if err := s.appendRecords(h, h.dir, true, s.withSync(h, id, recs...)...); err != nil {
			return Exchange{}, err
		}
		h.lazy = false
	}
	// Committed. The indexes follow, and the session is rebuilt from
	// what the store holds on its next open.
	for _, eid := range hashes {
		s.own(eid, id)
	}
	if x.HeadMoved {
		_ = writeHead(h.dir, newHead)
		x.Head = newHead
	}
	if o.handover {
		_ = writeIndex(filepath.Join(h.dir, "record"), []byte(MarkRecord+"\n"))
	}
	if err := s.reopen(id, h); err != nil {
		return x, fmt.Errorf("cas: exchange committed, and the session could not be reopened: %w", err)
	}
	if failedHandover {
		return x, fmt.Errorf("%w: the entries landed and no mark changed: %s", ErrHeadMoved, x.Why)
	}
	return x, nil
}
