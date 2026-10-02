package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

func show(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("show", "<file> | <cas-root> <id> [-leaf id] [-v]", stderr)
	leaf := fs.String("leaf", "", "entry whose context to print; the current leaf by default")
	full := fs.Bool("v", false, "print the data of custom and extension entries instead of its size")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	src, err := sessionSource(fs, positional, stderr)
	if err != nil {
		return err
	}
	if err := requireSession(fs, src, "show"); err != nil {
		return err
	}
	// A session read through a cas store keeps the store open: a call
	// in a fork's prefix has its dispatch in the session the fork was
	// made from, which the store holds and a file does not.
	var (
		s      *agentsession.Session
		reader agentsession.Reader
	)
	if src.id == "" {
		s, err = readSession(src.path)
	} else {
		st, oerr := cas.Open(src.path, cas.WithReadOnly())
		if oerr != nil {
			return oerr
		}
		defer st.Close()
		s, err = readFrom(st, src)
		reader = st
	}
	if err != nil {
		return err
	}
	printHeader(stdout, s)
	printEntries(stdout, s, *full)
	at := *leaf
	if at == "" {
		at = s.Leaf()
	} else if at, err = resolveEntry(s, at); err != nil {
		return err
	}
	if at == "" {
		return nil
	}
	fmt.Fprintln(stdout)
	if err := printContext(stdout, s, at); err != nil {
		return err
	}
	if err := printPending(stdout, s, at, reader); err != nil {
		return err
	}
	if t := s.Truncated(); t != nil {
		fmt.Fprintf(stdout, "\ntruncated: line %d was cut short and is not loaded: %v\n", t.Line, t.Err)
	}
	return nil
}

func printHeader(w io.Writer, s *agentsession.Session) {
	h := s.Header()
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "session\t%s\n", h.ID)
	fmt.Fprintf(tw, "created\t%s\n", h.CreatedAt.UTC().Format(time.RFC3339Nano))
	fmt.Fprintf(tw, "format\t%s (%s)\n", h.Format, h.Payload)
	if h.Harness != nil {
		fmt.Fprintf(tw, "harness\t%s\n", strings.TrimSpace(h.Harness.Name+" "+h.Harness.Version))
	}
	if h.CWD != "" {
		fmt.Fprintf(tw, "cwd\t%s\n", h.CWD)
	}
	if h.ParentSession != "" {
		fmt.Fprintf(tw, "parent\t%s\n", h.ParentSession)
	}
	if h.SpawnedBy != "" {
		fmt.Fprintf(tw, "spawned by\t%s\n", h.SpawnedBy)
	}
	if len(h.Records) > 0 {
		fmt.Fprintf(tw, "records\t%s\n", strings.Join(h.Records, ", "))
	}
	if h.Media != "" {
		fmt.Fprintf(tw, "media\t%s\n", h.Media)
	}
	if name := s.Name(); name != "" {
		fmt.Fprintf(tw, "name\t%s\n", name)
	}
	fmt.Fprintf(tw, "entries\t%d in %d root(s), %d leaf(s), leaf %s\n", s.Len(), len(s.Roots()), len(s.Leaves()), orDash(shortID(s.Leaf())))
	tw.Flush()
}

// printEntries lists every entry in file order. Forks and leaves are
// marked, since they are what a reader looks for in a tree.
func printEntries(w io.Writer, s *agentsession.Session, full bool) {
	labels := s.Labels()
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tPARENT\tTYPE\tSUMMARY")
	for _, e := range s.Entries() {
		b := e.Base()
		var marks []string
		if n := len(s.Children(b.ID)); n > 1 {
			marks = append(marks, fmt.Sprintf("fork×%d", n))
		} else if n == 0 {
			marks = append(marks, "leaf")
		}
		if l, ok := labels[b.ID]; ok {
			marks = append(marks, "["+l+"]")
		}
		if n := len(b.Parents); n > 0 {
			// Provenance, not a second parent: the PARENT column is still
			// the whole of this entry's line of descent.
			marks = append(marks, fmt.Sprintf("converges %d", n))
		}
		summary := describeEntry(e, full)
		if len(marks) > 0 {
			summary += "  " + strings.Join(marks, " ")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", shortID(b.ID), orDash(shortID(b.Parent)), e.EntryType(), summary)
	}
	tw.Flush()
}

func printContext(w io.Writer, s *agentsession.Session, at string) error {
	ctx, err := s.ContextAt(at)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "context at %s: %d item(s) from %d entries\n", shortID(at), len(ctx.Items), len(ctx.Entries))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	st := ctx.Settings
	fmt.Fprintf(tw, "  model\t%s\n", orDash(st.Model))
	fmt.Fprintf(tw, "  instructions\t%s\n", describeText(st.Instructions))
	if len(st.InstructionsParts) > 0 {
		names := make([]string, 0, len(st.InstructionsParts))
		for _, p := range st.InstructionsParts {
			name := fmt.Sprintf("%s %dB", p.ID, len(p.Text))
			if p.Unresolved() {
				name = orDash(p.ID) + " unresolved"
			}
			if p.Source != "" {
				name += " from " + p.Source
			}
			names = append(names, name)
		}
		fmt.Fprintf(tw, "  parts\t%s\n", strings.Join(names, ", "))
	}
	if omitted := ctx.InstructionsOmitted(); len(omitted) > 0 {
		names := make([]string, 0, len(omitted))
		for _, o := range omitted {
			if o.Unresolved() {
				// A keep the path could not satisfy, or an element
				// naming nothing.
				name := "- unresolved"
				if o.Keep > 0 {
					name = fmt.Sprintf("keep %d unresolved", o.Keep)
				}
				names = append(names, name)
				continue
			}
			names = append(names, fmt.Sprintf("%s %dB (%s)", o.ID, o.Size, o.Reason))
		}
		fmt.Fprintf(tw, "  omitted\t%s\n", strings.Join(names, ", "))
	}
	if len(st.Tools) > 0 {
		names := make([]string, 0, len(st.Tools))
		for _, t := range st.Tools {
			names = append(names, orDash(agentsession.ToolName(t)))
		}
		fmt.Fprintf(tw, "  tools\t%s\n", strings.Join(names, ", "))
	}
	if len(st.Extra) > 0 {
		fmt.Fprintf(tw, "  extra\t%s\n", strings.Join(sortedKeys(st.Extra), ", "))
	}
	tw.Flush()
	for i, it := range ctx.Items {
		fmt.Fprintf(w, "  %3d  %s\n", i+1, describeItem(it))
	}
	return nil
}

// printPending lists the calls pending at the entry at, with the state
// the path reads for each, and where a call with no dispatch on its
// path was handed to its tool, when it was: on another branch of the
// session, which a rebase leaves, or, for a call in a fork's prefix, in
// the session the fork was made from, read through r when the session
// came from a store. A file is read alone, so a prefix call's dispatch
// is not found from one. Nothing is printed when no call is pending.
func printPending(w io.Writer, s *agentsession.Session, at string, r agentsession.Reader) error {
	calls, err := s.PendingCalls(at)
	if err != nil {
		return err
	}
	if len(calls) == 0 {
		return nil
	}
	h := s.Header()
	fmt.Fprintf(w, "\npending at %s: %d call(s)\n", shortID(at), len(calls))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, c := range calls {
		var note string
		switch {
		case len(c.Dispatches) > 0:
			if key := c.IdempotencyKey(); key != "" {
				note = "key " + describeText(key)
			}
		default:
			origin, ds, err := agentsession.OriginDispatches(context.Background(), r, s, c.Entry.ID)
			switch {
			case err != nil:
				note = "origin not read: " + err.Error()
			case len(ds) > 0:
				d := ds[len(ds)-1]
				note = "dispatched off the path"
				if origin != s {
					note = "dispatched in session " + origin.ID()
				}
				if d.IdempotencyKey != "" {
					note += ", key " + describeText(d.IdempotencyKey)
				}
			}
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.ID(), c.Call.Name, c.State(h), note)
	}
	return tw.Flush()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
