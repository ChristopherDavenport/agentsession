package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"text/tabwriter"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

// followContext is the context a follow runs under: cancelled by an
// interrupt, which is how an operator ends it. A test replaces it.
var followContext = func() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

// followShow is show -f: the session as show prints it, then each
// entry as the store accepts it, until the operator interrupts or the
// session is deleted. It reads through the store's Follower, read-only
// and holding nothing, so it runs beside the harness that is writing.
//
// A reset, which a store makes when it replaces the log it was
// following, prints the session again from the top, since what was
// printed before may not be in it.
func followShow(src source, full bool, stdout, stderr io.Writer) error {
	f, id, reader, closeStore, err := followerFor(src)
	if err != nil {
		return err
	}
	defer closeStore()
	ctx, stop := followContext()
	defer stop()
	fmt.Fprintf(stderr, "agentsession: following %s; interrupt to stop\n", id)
	for c, err := range f.Follow(ctx, id, "") {
		if err != nil {
			if errors.Is(err, agentsession.ErrNoSession) {
				return fmt.Errorf("session %s was deleted", id)
			}
			return err
		}
		switch c.Kind {
		case agentsession.Snapshot:
			if err := printSession(stdout, c.Session, "", full, reader); err != nil {
				return err
			}
			fmt.Fprintln(stdout)
		case agentsession.Reset:
			fmt.Fprintf(stdout, "\nreset: the log was replaced; the session is now %d entries\n\n", c.Session.Len())
			if err := printSession(stdout, c.Session, "", full, reader); err != nil {
				return err
			}
			fmt.Fprintln(stdout)
		case agentsession.Appended:
			printAppended(stdout, c.Session, c.Entry, full)
		case agentsession.Head:
			fmt.Fprintf(stdout, "head -> %s\n", shortID(c.Leaf))
		}
	}
	return nil
}

// printAppended prints one entry as a line of the entries table, in the
// same columns. The marks that depend on the rest of the tree, a fork
// or a leaf, are not known for an entry that has just landed.
func printAppended(w io.Writer, s *agentsession.Session, e agentsession.Entry, full bool) {
	b := e.Base()
	summary := describeEntry(e, full)
	if l, ok := s.Labels()[b.ID]; ok {
		summary += "  [" + l + "]"
	}
	tw := tabwriter.NewWriter(w, 12, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(b.ID), orDash(shortID(b.Parent)), e.EntryType(), summary)
	tw.Flush()
}

// followerFor opens what follows a source: a cas store's session
// through the store, read-only; a session file through a jsonl store
// read-only over the directories the file's layout names, which holds
// when the file is where a jsonl store keeps it,
// <root>/<project>/<time>_<id>.jsonl.
func followerFor(src source) (agentsession.Follower, string, agentsession.Reader, func(), error) {
	if src.id != "" {
		st, err := cas.Open(src.path, cas.WithReadOnly())
		if err != nil {
			return nil, "", nil, nil, err
		}
		return st, src.id, st, func() { st.Close() }, nil
	}
	s, err := readSession(src.path)
	if err != nil {
		return nil, "", nil, nil, err
	}
	abs, err := filepath.Abs(src.path)
	if err != nil {
		return nil, "", nil, nil, err
	}
	st, err := jsonl.Open(filepath.Dir(filepath.Dir(abs)), jsonl.WithReadOnly())
	if err != nil {
		return nil, "", nil, nil, err
	}
	if found, err := st.Path(s.ID()); err != nil || !sameFile(found, abs) {
		st.Close()
		return nil, "", nil, nil, fmt.Errorf("%w: %s cannot be followed as a file: only a session where a jsonl store keeps it, <root>/<project>/<time>_<id>.jsonl, or in a cas store", errUsage, src.path)
	}
	return st, s.ID(), nil, func() { st.Close() }, nil
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}
