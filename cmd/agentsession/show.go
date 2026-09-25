package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
)

func show(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("show", "<file> [-leaf id] [-v]", stderr)
	leaf := fs.String("leaf", "", "entry whose context to print; the current leaf by default")
	full := fs.Bool("v", false, "print the data of custom and extension entries instead of its size")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	path, err := onePath(fs, positional, "session file")
	if err != nil {
		return err
	}
	s, err := readSession(path)
	if err != nil {
		return err
	}
	printHeader(stdout, s)
	printEntries(stdout, s, *full)
	at := *leaf
	if at == "" {
		at = s.Leaf()
	}
	if at == "" {
		return nil
	}
	fmt.Fprintln(stdout)
	if err := printContext(stdout, s, at); err != nil {
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
	fmt.Fprintf(tw, "entries\t%d in %d root(s), %d leaf(s), leaf %s\n", s.Len(), len(s.Roots()), len(s.Leaves()), orDash(s.Leaf()))
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
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", b.ID, orDash(b.Parent), e.EntryType(), summary)
	}
	tw.Flush()
}

func printContext(w io.Writer, s *agentsession.Session, at string) error {
	ctx, err := s.ContextAt(at)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "context at %s: %d item(s) from %d entries\n", at, len(ctx.Items), len(ctx.Entries))
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	st := ctx.Settings
	fmt.Fprintf(tw, "  model\t%s\n", orDash(st.Model))
	fmt.Fprintf(tw, "  instructions\t%s\n", describeText(st.Instructions))
	if len(st.InstructionsParts) > 0 {
		names := make([]string, 0, len(st.InstructionsParts))
		for _, p := range st.InstructionsParts {
			name := fmt.Sprintf("%s %dB", p.ID, len(p.Text))
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
