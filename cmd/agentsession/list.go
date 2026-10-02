package main

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

func list(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("list", "<root> [-cwd path] [-parent id] [-harness name] [-top-level] [-limit n] [-current]", stderr)
	cwd := fs.String("cwd", "", "only sessions with this working directory")
	parent := fs.String("parent", "", "only sessions forked or spawned from this session")
	harness := fs.String("harness", "", "only sessions whose header names this harness")
	topLevel := fs.Bool("top-level", false, "leave out the subsessions a call spawned")
	limit := fs.Int("limit", 0, "at most this many sessions; 0 means all")
	current := fs.Bool("current", false, "leave out sessions that were continued in a successor")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	root, err := onePath(fs, positional, "store root")
	if err != nil {
		return err
	}
	// jsonl.Open would create a missing root, which a listing must not.
	if info, err := os.Stat(root); err != nil {
		return err
	} else if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", root)
	}
	// Read-only: a listing takes no session's lock, so it says what
	// is there while an agent writes.
	var st interface {
		List(context.Context, agentsession.ListFilter) iter.Seq2[agentsession.Summary, error]
		Close() error
	}
	if cas.IsStore(root) {
		st, err = cas.Open(root, cas.WithReadOnly())
	} else {
		st, err = jsonl.Open(root, jsonl.WithReadOnly())
	}
	if err != nil {
		return err
	}
	defer st.Close()

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREATED\tID\tNAME\tSIZE\tCWD\tCONTINUED IN\tPATH")
	var problems []error
	f := agentsession.ListFilter{CWD: *cwd, ParentSession: *parent, Harness: *harness, TopLevel: *topLevel, Limit: *limit, WithNames: true, Current: *current}
	for sum, err := range st.List(context.Background(), f) {
		if err != nil {
			problems = append(problems, err)
			continue
		}
		h := sum.Header
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\t%s\n", h.CreatedAt.UTC().Format(time.RFC3339), h.ID, orDash(sum.Name), sum.Size, orDash(h.CWD), orDash(sum.SupersededBy), sum.Path)
	}
	tw.Flush()
	for _, err := range problems {
		fmt.Fprintf(stderr, "agentsession: %v\n", err)
	}
	if len(problems) > 0 {
		return errFailed
	}
	return nil
}
