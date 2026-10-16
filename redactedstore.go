package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
)

// ErrNotRedacted is returned by a store set to hold only redacted
// entries for what was not redacted: a session whose header does not
// carry redacted, at its create, its import, or a push or fetch it
// receives, and an entry that carries no redacted member ([RedactedOf]
// false), however it arrives, an append, an import, a push or a fetch,
// a prefix entry included. A fork of a redacted session and an entry a
// writer appended to one natively are among them, since neither was
// redacted. [Original] returns it for a session that is not redacted.
var ErrNotRedacted = errors.New("agentsession: not redacted")

// ErrStaleOriginal is returned by [Original] when a redacted entry names
// a position past the entries the store holds of the original: a copy,
// a mirror say, that has not reached the version redacted. Fetching the
// original and asking again may answer.
var ErrStaleOriginal = errors.New("agentsession: the store's copy of the original is behind the version redacted")

// CheckCreate is the refusal a store makes of a create for what the
// redaction rules forbid: a redacted_from RFC 0001 calls malformed
// ([ErrRedactedFrom]), one on a session that is not redacted, whether
// typed or held in Extra as a header decoded without a format holds
// it, one naming no session, or one naming the session itself, as
// [Header.Validate] refuses it on a read; and, in a store that holds
// only redacted entries, a header that does not carry redacted
// ([ErrNotRedacted]). A store calls it before it creates anything; nil
// means it may. A header whose ID the store fills names no session
// yet, so cannot name itself. A redacted session is created as any
// other is.
func CheckCreate(h Header, redactedOnly bool) error {
	r := h.RedactedFrom
	if r == nil {
		r = extraRedactedFrom(h)
	}
	switch {
	case !h.Redacted && r != nil:
		return fmt.Errorf("%w: create %s: on a session that is not redacted", ErrRedactedFrom, h.ID)
	case r != nil && r.Session == "":
		return fmt.Errorf("%w: create %s: names no session", ErrRedactedFrom, h.ID)
	case r != nil && h.ID != "" && r.Session == h.ID:
		return fmt.Errorf("%w: create %s: names the session itself", ErrRedactedFrom, h.ID)
	case redactedOnly && !h.Redacted:
		return fmt.Errorf("%w: create %s in a store that holds only redacted entries", ErrNotRedacted, h.ID)
	}
	return nil
}

// CheckRedacted is the refusal a store that holds only redacted entries
// makes, with [ErrNotRedacted], of a session that would bring it
// anything not redacted: a header that does not carry redacted, or an
// entry, the prefix's included, that carries no redacted member. A
// store calls it on what an import, a push or a fetch delivers, and,
// with the session's header and the one entry, on an append.
func CheckRedacted(h Header, entries []Entry) error {
	if !h.Redacted {
		return fmt.Errorf("%w: session %s in a store that holds only redacted entries", ErrNotRedacted, h.ID)
	}
	for _, e := range entries {
		if _, ok := RedactedOf(e); !ok {
			return fmt.Errorf("%w: entry %s of %s carries no redacted, in a store that holds only redacted entries", ErrNotRedacted, e.Base().ID, h.ID)
		}
	}
	return nil
}

// CheckHoldsRedacted reads every session st lists and refuses, with
// [ErrNotRedacted], the first whose header does not carry redacted or
// that holds an entry without it, as [CheckRedacted] does: what a store
// set to hold only redacted entries does at its open, so it is not set
// that way over what it already holds. A session it cannot read, or a
// listing error, refuses too.
func CheckHoldsRedacted(ctx context.Context, st Store) error {
	for sum, err := range st.List(ctx, ListFilter{}) {
		if err != nil {
			return err
		}
		s, err := readHeld(ctx, st, sum.Header.ID)
		if err != nil {
			return fmt.Errorf("agentsession: checking %s holds only redacted entries: %w", sum.Header.ID, err)
		}
		if err := CheckRedacted(s.Header(), s.Entries()); err != nil {
			return err
		}
	}
	return nil
}

// extraRedactedFrom is the redacted_from a header holds in Extra that
// its file would decode as the object, which a header decoded without a
// format holds there, or nil: a store writes a header with no format at
// this package's, which defines the member, so Read would then hold it
// to the member's rules. A member in another form is kept as written,
// as a reader keeps it.
func extraRedactedFrom(h Header) *RedactedFrom {
	raw, ok := h.Extra["redacted_from"]
	format := h.Format
	if format == "" {
		format = Format
	}
	if !ok || !definesRedactedFrom(format) {
		return nil
	}
	var r RedactedFrom
	if json.Unmarshal(raw, &r) != nil {
		return nil
	}
	return &r
}

// FileOrder returns a session's entries in file order, as RFC 0002
// projects a session to a file and as a redacted entry's positions
// count them: the prefix, root first, then the session's own entries
// in the order the session holds them, which for a store's session is
// its log order. A projection's synthetic leaf marker is not an entry
// and is not there.
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

// readHeld reads a session from st without a hold where st can.
func readHeld(ctx context.Context, st Store, id string) (*Session, error) {
	if r, ok := st.(Reader); ok {
		return r.Read(ctx, id)
	}
	return st.Open(ctx, id)
}

// Original returns the session s was redacted from, which s's header
// names in redacted_from, as st holds it, read through [Reader] where
// st implements it, so no hold is taken, and opened otherwise; and the
// entries each of s's redacted entries was made from. A store that does
// not hold the original reports [ErrNoSession], as does a redacted
// session that names no original, as one written before 0.13 may; a
// session that is not redacted is [ErrNotRedacted].
//
// The mapping takes each redacted entry's ID to the IDs of the
// original's entries its positions name, ranges expanded, in the order
// they are named, by position in the original's [FileOrder] counting
// from 1. An entry a writer appended natively, which carries no
// redacted, and an entry the redaction added, which carries [], are
// made from nothing in the original and are not in the mapping;
// [RedactedOf] tells them apart. A position past the entries the store
// holds of the original is [ErrStaleOriginal]: its copy has not reached
// the version redacted.
//
// A position counts in the original of the session whose own entry it
// is (RFC 0001, Redacted entries). For s a fork, its prefix is another
// session's own entries: those are resolved in the original that
// session's redacted_from names, following parent_session to the
// session that owns them, where st holds it and it is redacted, and are
// left out of the mapping where it does not. The session returned is
// s's own original.
func Original(ctx context.Context, st Store, s *Session) (*Session, map[string][]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	h := s.Header()
	if !h.Redacted {
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRedacted, h.ID)
	}
	if h.RedactedFrom == nil {
		return nil, nil, fmt.Errorf("%w: %s names no original in redacted_from", ErrNoSession, h.ID)
	}
	orig, err := readHeld(ctx, st, h.RedactedFrom.Session)
	if err != nil {
		return nil, nil, err
	}
	m := map[string][]string{}
	var own, prefix []Entry
	for _, e := range s.Entries() {
		if s.Prefix(e.Base().ID) {
			prefix = append(prefix, e)
		} else {
			own = append(own, e)
		}
	}
	if err := mapPositions(m, own, orig); err != nil {
		return nil, nil, err
	}
	// The prefix: up parent_session, each session's own entries mapped
	// in its own original, as far as the store holds the line.
	at := h
	for seen := 0; len(prefix) > 0 && at.ParentSession != "" && seen < 64; seen++ {
		parent, err := readHeld(ctx, st, at.ParentSession)
		if err != nil {
			break
		}
		var mine, rest []Entry
		for _, e := range prefix {
			if _, held := parent.Entry(e.Base().ID); held && !parent.Prefix(e.Base().ID) {
				mine = append(mine, e)
			} else {
				rest = append(rest, e)
			}
		}
		if ph := parent.Header(); ph.Redacted && ph.RedactedFrom != nil && len(mine) > 0 {
			if po, err := readHeld(ctx, st, ph.RedactedFrom.Session); err == nil {
				if err := mapPositions(m, mine, po); err != nil {
					return nil, nil, err
				}
			}
		}
		prefix, at = rest, parent.Header()
	}
	return orig, m, nil
}

// mapPositions adds to m, for each redacted entry of entries that names
// positions, the IDs of the entries of orig they name in its file
// order.
func mapPositions(m map[string][]string, entries []Entry, orig *Session) error {
	order := FileOrder(orig)
	for _, e := range entries {
		spans, ok := RedactedOf(e)
		if !ok || len(spans) == 0 {
			continue
		}
		var ids []string
		for _, sp := range spans {
			if sp.To > len(order) {
				return fmt.Errorf("%w: entry %s names position %d of %s, which the store holds %d entries of", ErrStaleOriginal, e.Base().ID, sp.To, orig.ID(), len(order))
			}
			for p := sp.From; p <= sp.To; p++ {
				ids = append(ids, order[p-1].Base().ID)
			}
		}
		m[e.Base().ID] = ids
	}
	return nil
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
