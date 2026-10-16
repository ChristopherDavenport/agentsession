package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
)

// ErrRedacted is returned for a write a redacted session never takes:
// a store's Create of a session whose header carries redacted, and an
// append to one, through [Session.Append] and [Session.Prepare] and so
// through every store's Append, whoever the writer. A redactor writes a
// redacted session whole, with [Session.Commit] and [Write], and it
// enters a store whole, by import or push; an exchange that would add
// entries to a held redacted session is refused with it too. Continuing
// from a redacted session is a fork, whose header does not carry
// redacted.
var ErrRedacted = errors.New("agentsession: a redacted session is written whole and never created empty or appended to")

// ErrNotRedacted is returned by a store set to hold only redacted
// sessions for a session whose header does not carry redacted, by
// whatever path it would enter: create, import or a push received. A
// fork of a redacted session is among them, since a fork's own entries
// were not redacted. [Original] returns it for a session that is not
// redacted.
var ErrNotRedacted = errors.New("agentsession: not a redacted session")

// CheckCreate is the refusal RFC 0002 makes of a create: [ErrRedacted]
// for a header that carries redacted, which no store creates,
// [ErrRedactedFrom] for one carrying redacted_from without it, which
// RFC 0001 calls malformed, and, in a store that holds only redacted
// sessions, [ErrNotRedacted] for every other. A store calls it before
// it creates anything; nil means it may.
func CheckCreate(h Header, redactedOnly bool) error {
	switch {
	case h.Redacted:
		return fmt.Errorf("%w: create %s", ErrRedacted, h.ID)
	case h.RedactedFrom != nil || unmarkedRedactedFrom(h):
		return fmt.Errorf("%w: create %s: on a session that is not redacted", ErrRedactedFrom, h.ID)
	case redactedOnly:
		return fmt.Errorf("%w: create %s in a store that holds only redacted sessions", ErrNotRedacted, h.ID)
	}
	return nil
}

// unmarkedRedactedFrom reports whether a header holds in Extra a
// redacted_from that its file would decode as the object, which a
// header decoded without a format holds there: a store writes a header
// with no format at this package's, which defines the member, so Read
// would then refuse the file. A member in another form is kept as
// written, as a reader keeps it.
func unmarkedRedactedFrom(h Header) bool {
	raw, ok := h.Extra["redacted_from"]
	format := h.Format
	if format == "" {
		format = Format
	}
	if !ok || !definesRedactedFrom(format) {
		return false
	}
	var r RedactedFrom
	return json.Unmarshal(raw, &r) == nil
}

// FileOrder returns a session's entries in file order, as RFC 0002
// projects a session to a file and as redacted_from counts them: the
// prefix, root first, then the session's own entries in the order the
// session holds them, which for a store's session is its log order. A
// projection's synthetic leaf marker is not an entry and is not there.
func FileOrder(s *Session) []Entry {
	all := s.Entries()
	if s.Header().Base == "" {
		return all
	}
	out := make([]Entry, 0, len(all))
	for _, e := range all {
		if s.Prefix(e.Base().ID) {
			out = append(out, e)
		}
	}
	for _, e := range all {
		if !s.Prefix(e.Base().ID) {
			out = append(out, e)
		}
	}
	return out
}

// Original returns the session s was redacted from, which s's header
// names in redacted_from, as st holds it: read through [Reader] where
// st implements it, so no hold is taken, and opened otherwise. A store
// that does not hold it reports [ErrNoSession], as does a redacted
// session that names no original, as one written before 0.13 may; a
// session that is not redacted is [ErrNotRedacted].
//
// For an aligned redaction it also returns the correspondence its
// entries have with the original's: each of s's entry IDs mapped to the
// ID of the original's entry in its place, by position in [FileOrder]
// over the first redacted_from.entries entries of the original, prefix
// included. That holds only when s has exactly that many entries, the
// original at least that many, and each pair is of one type, so a
// mismatch is refused with [ErrRedactedFrom] rather than mapped wrong:
// the claim is false, or the store holds another session under the
// original's ID, or a copy that has not reached the version redacted. A
// redaction that is not aligned gets no mapping; one that records its
// own keeps it in namespaced members of redacted_from, which
// [RedactedFrom].Extra holds.
func Original(ctx context.Context, st Store, s *Session) (*Session, map[string]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	h := s.Header()
	if !h.Redacted {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRedacted, h.ID)
	}
	from := h.RedactedFrom
	if from == nil {
		return nil, nil, fmt.Errorf("%w: %s names no original in redacted_from", ErrNoSession, h.ID)
	}
	var (
		orig *Session
		err  error
	)
	if r, ok := st.(Reader); ok {
		orig, err = r.Read(ctx, from.Session)
	} else {
		orig, err = st.Open(ctx, from.Session)
	}
	if err != nil {
		return nil, nil, err
	}
	if !from.Aligned {
		return orig, nil, nil
	}
	red, held := FileOrder(s), FileOrder(orig)
	switch {
	case len(red) != from.Entries:
		return nil, nil, fmt.Errorf("%w: %s is aligned to %d entries of %s and holds %d", ErrRedactedFrom, h.ID, from.Entries, from.Session, len(red))
	case len(held) < from.Entries:
		return nil, nil, fmt.Errorf("%w: %s is aligned to %d entries of %s, of which the store holds %d", ErrRedactedFrom, h.ID, from.Entries, from.Session, len(held))
	}
	m := make(map[string]string, len(red))
	for i, e := range red {
		o := held[i]
		if e.EntryType() != o.EntryType() {
			return nil, nil, fmt.Errorf("%w: %s is aligned to %s, and its entry %d is a %s where the original's is a %s", ErrRedactedFrom, h.ID, from.Session, i, e.EntryType(), o.EntryType())
		}
		m[e.Base().ID] = o.Base().ID
	}
	return orig, m, nil
}

// Redactions lists the sessions st holds that were redacted from the
// session id: those whose header's redacted_from names it, newest
// first, as [Store.List] gives them. Nothing is written into an
// original about what was made from it, so this is how its holder
// finds them. It reads every header the store lists; a header from
// before 0.13 holds no redacted_from this package reads, and is not
// one.
func Redactions(ctx context.Context, st Store, id string) iter.Seq2[Summary, error] {
	return func(yield func(Summary, error) bool) {
		for sum, err := range st.List(ctx, ListFilter{}) {
			if err != nil {
				if !yield(Summary{}, err) {
					return
				}
				continue
			}
			if from := sum.Header.RedactedFrom; from == nil || !sum.Header.Redacted || from.Session != id {
				continue
			}
			if !yield(sum, nil) {
				return
			}
		}
	}
}
