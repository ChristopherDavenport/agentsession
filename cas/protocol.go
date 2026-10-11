package cas

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/ChristopherDavenport/agentsession"
)

// This file is what the store offers an exchange that does not hold a
// second Store in the same process: RFC 0003's server, which takes a
// push over the wire, and its push helper, which sends one. Push and
// Fetch between two Stores are the same steps without the wire.

// The object spaces as RFC 0003 names them. Envelopes are kept apart
// from contents (RFC 0002, Entries). A media blob is a content object
// here, held once, but a caller names its space, and a server checks
// what a caller may read per space.
const (
	SpaceEntries  = "entries"
	SpaceContents = "contents"
	SpaceMedia    = "media"
)

// ErrNoObject is an object the store does not hold.
var ErrNoObject = errors.New("cas: no such object")

// ErrBadObject is bytes that do not hash to the name they were given.
var ErrBadObject = errors.New("cas: the object does not hash to its name")

// spaceNamed is the store's space for an RFC 0003 space name.
func spaceNamed(name string) (space, error) {
	switch name {
	case SpaceEntries:
		return spaceEntries, nil
	case SpaceContents, SpaceMedia:
		return spaceContents, nil
	}
	return 0, fmt.Errorf("cas: no object space %q", name)
}

// Object returns the bytes of an object the store holds, checked
// against its name: an envelope, a content, or a media blob.
func (s *Store) Object(ctx context.Context, spaceName, hash string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sp, err := spaceNamed(spaceName)
	if err != nil {
		return nil, err
	}
	if !agentsession.ValidHash(hash) {
		return nil, fmt.Errorf("%w: %s %q", ErrNoObject, spaceName, hash)
	}
	data, err := s.objs.read(sp, hash)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s %s", ErrNoObject, spaceName, hash)
	}
	return data, err
}

// HasObject reports whether the store holds an object.
func (s *Store) HasObject(ctx context.Context, spaceName, hash string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	sp, err := spaceNamed(spaceName)
	if err != nil {
		return false, err
	}
	if !agentsession.ValidHash(hash) {
		return false, nil
	}
	if _, _, _, _, err := s.objs.locate(sp, hash, true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// PutObject stores an object under its hash, durably, after checking
// the bytes hash to it (ErrBadObject otherwise). An object the store
// holds already is a write that freshens it. What it puts is reachable
// from no session until an exchange names it, and a Sweep collects it
// once it is older than the sweep's grace, as any unreferenced object.
func (s *Store) PutObject(ctx context.Context, spaceName, hash string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	sp, err := spaceNamed(spaceName)
	if err != nil {
		return err
	}
	if !agentsession.ValidHash(hash) || hashBytes(data) != hash {
		return fmt.Errorf("%w: %s %s", ErrBadObject, spaceName, hash)
	}
	guard, err := s.writeGuard(ctx)
	if err != nil {
		return err
	}
	defer guard.release()
	pend := newPendSet()
	if err := s.objs.writeTo(sp, hash, data, true, pend); err != nil {
		return fmt.Errorf("cas: store object: %w", err)
	}
	return s.objs.flushSet(pend)
}

// SplitEntry takes an entry apart into its two objects, canonical: the
// envelope its ID hashes and the content its content hash names.
func SplitEntry(e agentsession.Entry) (envelope, content []byte, err error) {
	return split(e)
}

// JoinLine rebuilds an entry's RFC 0001 line from its envelope and
// content objects, and checks it hashes to id. A reader of the line
// (agentsession.Read, Session.Extend) decodes and hashes it.
func JoinLine(id string, envelope, content []byte) ([]byte, error) {
	line, err := join(id, envelope, content)
	if err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	got, _, err := agentsession.EntryHashes(line)
	if err != nil {
		return nil, fmt.Errorf("cas: entry %s: %w", id, err)
	}
	if got != id {
		return nil, fmt.Errorf("cas: entry %s: %w: its objects hash to %s", id, agentsession.ErrBadID, got)
	}
	return line, nil
}

// ContentOf is the content hash an envelope names.
func ContentOf(envelope []byte) (string, bool) { return envelopeContent(envelope) }

// BlobsNamedBy returns the media blobs a content names through sidecar
// references, as hashes.
func BlobsNamedBy(content []byte) []string { return blobsNamedBy(content) }

// Closure is what an exchange carries: one session's header, mark and
// head, its prefix (the path to its base, root first), its own entries
// in log order, and the media blobs their contents name, by hash.
type Closure struct {
	Header agentsession.Header
	Mark   string
	Head   string
	Prefix []agentsession.Entry
	Own    []agentsession.Entry
	Blobs  map[string][]byte
}

// Closure reads a session's closure as a push sends it, after committing
// the session, so a receiver never holds what a crash could take back.
func (s *Store) Closure(ctx context.Context, id string) (Closure, error) {
	b, err := s.bundleOf(ctx, id)
	if err != nil {
		return Closure{}, err
	}
	return Closure{Header: b.header, Mark: b.mark, Head: b.head, Prefix: b.prefix, Own: b.own, Blobs: b.blobs}, nil
}

// ReceiveOptions are a received push's compare-and-swap and handover,
// as PushOptions are the sender's.
type ReceiveOptions struct {
	// Expected is the receiver's head as the sender last saw it.
	Expected string
	// Force moves the head whatever it is, and is reported as such.
	Force bool
	// Handover makes this store the record.
	Handover bool
	// Fetch admits c as Fetch does rather than as a push: c need not be
	// the record's, and the head moves only when this store is a mirror
	// of the session and c's head descends from its own, or it has none;
	// a session this store lacked is created as a mirror. Expected, Force
	// and Handover are not read. It is how a client admits a session it
	// read from a server (RFC 0003's fetch).
	Fetch bool
}

// Receive admits a push of c from the record, as Push does at its
// receiver: what this store lacks lands parent first, the log merged
// as a set; the head moves by compare-and-swap from o.Expected, or by
// force; a session this store lacked is created as a mirror, or as the
// record for a handover. Everything it admits, and a mark it sets, is
// committed before it returns. The sender is the record by definition:
// c.Mark is not read. A handover whose head does not move is refused
// with ErrHeadMoved, before anything lands for a session this store
// lacked and after the entries land for one it holds, with the Exchange
// saying what landed. With o.Fetch it admits c as Fetch admits a
// session, as ReceiveOptions says.
func (s *Store) Receive(ctx context.Context, c Closure, o ReceiveOptions) (Exchange, error) {
	if err := ctx.Err(); err != nil {
		return Exchange{}, err
	}
	for _, e := range append(append([]agentsession.Entry(nil), c.Prefix...), c.Own...) {
		if err := checkEntry(e); err != nil {
			return Exchange{}, err
		}
	}
	for h, data := range c.Blobs {
		if hashBytes(data) != h {
			return Exchange{}, fmt.Errorf("%w: media %s", ErrBadObject, h)
		}
	}
	b := &bundle{header: c.Header, mark: MarkRecord, head: c.Head, prefix: c.Prefix, own: c.Own, blobs: c.Blobs}
	if b.blobs == nil {
		b.blobs = map[string][]byte{}
	}
	if o.Fetch {
		b.mark = c.Mark
		return s.receive(ctx, b, receiveOptions{})
	}
	return s.receive(ctx, b, receiveOptions{push: true, expected: o.Expected, force: o.Force, handover: o.Handover})
}

// checkEntry checks an entry's line hashes to the ID it carries.
func checkEntry(e agentsession.Entry) error {
	line, err := agentsession.MarshalEntry(e)
	if err != nil {
		return err
	}
	id, _, err := agentsession.EntryHashes(line)
	if err != nil {
		return err
	}
	if id != e.Base().ID {
		return fmt.Errorf("cas: entry %s: %w: its line hashes to %s", e.Base().ID, agentsession.ErrBadID, id)
	}
	return nil
}

// MarkOf returns the session's record mark and head as its committed
// log says, without holding the session: what a server reports of a
// session it does not hold, and what it checks before it leases one.
func (s *Store) MarkOf(ctx context.Context, sessionID string) (mark, head string, err error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", "", fmt.Errorf("%w: %s", agentsession.ErrNoSession, sessionID)
	}
	v, err := s.committedView(sessionID, dir)
	if err != nil {
		return "", "", err
	}
	if !v.exists {
		return "", "", fmt.Errorf("%w: %s", agentsession.ErrNoSession, sessionID)
	}
	return v.mark, v.head, nil
}

// Head returns the session's committed head, "" for none.
func (s *Store) Head(ctx context.Context, sessionID string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return "", err
	}
	defer h.mu.Unlock()
	return h.head, nil
}

// DeclareRecordAt is DeclareRecord as a compare-and-swap: it refuses
// with ErrHeadMoved when the session's head is not head, the head the
// caller believes this store holds, which is what reveals two records.
func (s *Store) DeclareRecordAt(ctx context.Context, sessionID, head string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if h.head != head {
		return fmt.Errorf("%w: the head is %s, not %s", ErrHeadMoved, orNone(h.head), orNone(head))
	}
	if h.mark == MarkRecord {
		return nil
	}
	if err := s.setMark(h, sessionID, MarkRecord); err != nil {
		return err
	}
	if h.head != "" {
		return h.session.Branch(h.head)
	}
	if h.session.Header().Base == "" {
		h.session.ResetLeaf()
	}
	return nil
}

// HandOver makes this store a mirror of a session it is the record for,
// when the session's head is head: this store's side of handing the
// session to another store, which then declares itself the record. It
// is refused with ErrMirror when this store is not the record, and with
// ErrHeadMoved when the head is another. The mark is committed before
// it returns.
func (s *Store) HandOver(ctx context.Context, sessionID, head string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.writable(); err != nil {
		return err
	}
	h, err := s.hold(sessionID)
	if err != nil {
		return err
	}
	defer h.mu.Unlock()
	if h.mark != MarkRecord {
		return fmt.Errorf("%w: %s", ErrMirror, sessionID)
	}
	if h.head != head {
		return fmt.Errorf("%w: the head is %s, not %s", ErrHeadMoved, orNone(h.head), orNone(head))
	}
	return s.setMark(h, sessionID, MarkMirror)
}
