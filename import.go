package agentsession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Importer is implemented by a store that takes a session in whole from
// an RFC 0001 file, as RFC 0002's projection section has a store import
// a projection. It is how a redacted session enters a store, since no
// store creates one. Import verifies every line and refuses a file one
// of whose lines fails, a last line cut short, a header carrying
// redacted_from without redacted ([ErrRedactedFrom]), a session ID the
// store holds ([ErrSessionExists]), an own entry that does not hang
// from the base or another own entry, and a fork whose media differs
// from that of a held session whose own entries include the base; a
// store set to hold only redacted sessions refuses a file whose header
// does not carry redacted ([ErrNotRedacted]). What it imports is
// committed before it returns, and the session comes back as the store
// holds it.
//
// asRecord says the file is the only copy, so the store is the session's
// record and not a mirror of it, as RFC 0002's record mark has it. A
// store that keeps no record mark holds every session as its own, and
// does not read it.
type Importer interface {
	Import(ctx context.Context, r io.Reader, asRecord bool) (*Session, error)
}

// ErrSyntheticMarker is returned by [ReadForImport] for a file holding
// a projection's synthetic leaf marker: a label carrying synthetic,
// which RFC 0002 writes only where the resume rule over the lines before
// it would not name the session's head. A store that keeps a session's
// head only in its entries cannot hold that head without adding an
// entry, which would change the session, so it refuses the file.
var ErrSyntheticMarker = errors.New("agentsession: the file's head is named by a projection's synthetic leaf marker, which this store cannot keep")

// ReadForImport reads an RFC 0001 file a store is to import and holds
// it to the checks of RFC 0002's import that need nothing of the
// store: [Read]'s, every line verified and a header with redacted_from
// without redacted refused among them, and then a last line cut short,
// a migration from before 0.5 that left an entry unresolved, an own
// entry above the base, and a synthetic leaf marker
// ([ErrSyntheticMarker]) are refused. A store then checks the session
// ID, the media against a held session whose own entries include the
// base, and its own settings, and stores the session's entries in file
// order: the prefix, which joins no session's own entries, then its
// own.
func ReadForImport(r io.Reader) (*Session, error) {
	s, err := Read(r)
	if err != nil {
		return nil, err
	}
	if t := s.Truncated(); t != nil {
		return nil, fmt.Errorf("agentsession: import: line %d is cut short: %w", t.Line, t.Err)
	}
	if migrated, unresolved := s.Migrated(); migrated && len(unresolved) > 0 {
		return nil, ErrUnresolvedMigration
	}
	h := s.Header()
	for i, e := range s.Entries() {
		if l, ok := e.(*LabelEntry); ok && synthetic(l) {
			return nil, fmt.Errorf("%w: line %d", ErrSyntheticMarker, i+2)
		}
		if h.Base == "" || s.Prefix(e.Base().ID) {
			continue
		}
		if p := e.Base().Parent; p != h.Base && (p == "" || s.Prefix(p)) {
			return nil, NewBaseRuleError(fmt.Sprintf("import: own entry %s hangs from %q, above the base", e.Base().ID, p))
		}
	}
	return s, nil
}

// synthetic reports whether a label carries synthetic: true.
func synthetic(l *LabelEntry) bool {
	raw, ok := l.Unknown["synthetic"]
	if !ok {
		return false
	}
	var v bool
	return json.Unmarshal(raw, &v) == nil && v
}

// CheckImportMedia is the media check RFC 0002's import makes, for a
// store that has found origin, the session holding the base, or nil
// when it holds none: a fork whose media differs from origin's is
// refused when origin's own entries include the base, since the prefix
// was written in origin's form and a projection carries media in one.
func CheckImportMedia(h Header, origin *Session) error {
	if origin == nil || h.Base == "" || origin.Prefix(h.Base) {
		return nil
	}
	if om := origin.Header().Media; mediaForm(h.Media) != mediaForm(om) {
		return fmt.Errorf("agentsession: import: media %q differs from %q of %s, whose own entries include the base", mediaForm(h.Media), mediaForm(om), origin.ID())
	}
	return nil
}
