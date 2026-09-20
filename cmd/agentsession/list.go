package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

func list(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("list", "<root> [-cwd path] [-parent id] [-limit n]", stderr)
	cwd := fs.String("cwd", "", "only sessions with this working directory")
	parent := fs.String("parent", "", "only sessions forked or spawned from this session")
	limit := fs.Int("limit", 0, "at most this many sessions; 0 means all")
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
	st, err := jsonl.Open(root)
	if err != nil {
		return err
	}
	defer st.Close()

	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CREATED\tID\tSIZE\tCWD\tPATH")
	var problems []error
	f := agentsession.ListFilter{CWD: *cwd, ParentSession: *parent, Limit: *limit}
	for sum, err := range st.List(context.Background(), f) {
		if err != nil {
			problems = append(problems, err)
			continue
		}
		h := sum.Header
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", h.CreatedAt.UTC().Format(time.RFC3339), h.ID, sum.Size, orDash(h.CWD), sum.Path)
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
