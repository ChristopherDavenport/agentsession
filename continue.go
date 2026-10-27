package agentsession

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ChristopherDavenport/openresponses"
)

// Continue rolls a session over into a successor, for a conversation
// that has outgrown its file: a months-long unattended session, or one
// whose compaction has folded the context many times over while the
// file kept growing. It does the four steps in the order the format
// wants them: the successor is created with parent_session naming id
// and the old header's harness, working directory, records and media
// mode; its first entry is a full config carrying the settings in
// force at the old leaf, so the first request on the new root rebuilds
// and a recorder finds a config on the path; summary, when not nil, is
// appended as the item that carries the conversation across; the old
// session's display name is carried over; and the old session gets a
// continued_in link naming the successor, which is what a store reads
// to mark it superseded. It returns the successor.
//
// A session continues once (RFC 0002, Refs and continuation): one that
// already holds a continued_in link is refused with [ErrSuperseded],
// since a second successor would fork the chain. The store refuses the
// link itself under the lock it appends under, so of two writers
// continuing at once one lands its link and the other is refused, its
// successor deleted. A caller continuing a
// named conversation uses [ContinueRef], which continues the session at
// the end of the chain.
func Continue(ctx context.Context, store Store, id string, summary openresponses.Item) (*Session, error) {
	old, err := store.Open(ctx, id)
	if err != nil {
		return nil, err
	}
	if next := old.SupersededBy(); next != "" {
		return nil, fmt.Errorf("%w: %s was continued in %s", ErrSuperseded, id, next)
	}
	cx, err := old.Context()
	if err != nil {
		return nil, fmt.Errorf("agentsession: continue %s: %w", id, err)
	}
	req, err := cx.Settings.Request(nil)
	if err != nil {
		return nil, fmt.Errorf("agentsession: continue %s: %w", id, err)
	}
	cfg, err := ConfigFromRequest(req)
	if err != nil {
		return nil, err
	}
	// The parts left out stay in force across the rollover, as they do
	// across a compaction.
	cfg.InstructionsOmitted = cloneOmitted(cx.Settings.InstructionsOmitted)
	// And so does what the request leaves out, as a checkpoint carries
	// it: the rule keeps covering a later switch of model, and the
	// entries listed name nothing in the successor, so they leave
	// nothing out there.
	if o := cx.Settings.Omit; !o.IsZero() {
		o.Items = slices.Clone(o.Items)
		cfg.Omit = &o
	}
	h := old.Header()
	next, err := store.Create(ctx, Header{
		Harness:       h.Harness,
		Records:       h.Records,
		CWD:           h.CWD,
		ParentSession: h.ID,
		Media:         h.Media,
	})
	if err != nil {
		return nil, err
	}
	nextID := next.ID()
	if _, err := store.Append(ctx, nextID, cfg); err != nil {
		return nil, fmt.Errorf("agentsession: continue %s: config: %w", id, err)
	}
	if summary != nil {
		if _, err := store.Append(ctx, nextID, NewItemEntry(summary)); err != nil {
			return nil, fmt.Errorf("agentsession: continue %s: summary: %w", id, err)
		}
	}
	if name := old.Name(); name != "" {
		if _, err := store.Append(ctx, nextID, &InfoEntry{Name: name}); err != nil {
			return nil, fmt.Errorf("agentsession: continue %s: name: %w", id, err)
		}
	}
	if _, err := store.Append(ctx, id, NewLinkEntry(RelContinuedIn, nextID)); err != nil {
		if errors.Is(err, ErrSuperseded) {
			// Another writer continued the session first, and its link
			// is the one the chain follows: this successor is no one's.
			_ = store.Delete(ctx, nextID)
		}
		return nil, fmt.Errorf("agentsession: continue %s: link: %w", id, err)
	}
	return next, nil
}

// ErrSuperseded is returned by [Continue] for a session a continued_in
// link has already retired, and by a store that refuses to append to
// such a session. No store in this module refuses the append.
var ErrSuperseded = errors.New("agentsession: session was continued in a successor")

// SupersededBy returns the successor named by the last continued_in
// link in the session, or "".
func (s *Session) SupersededBy() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	next := ""
	for _, e := range s.entries {
		if l, ok := e.(*LinkEntry); ok && l.Rel == RelContinuedIn && l.Session != "" {
			next = l.Session
		}
	}
	return next
}
