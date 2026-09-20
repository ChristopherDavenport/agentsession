package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/atif"
	"github.com/ChristopherDavenport/agentsession/export"
)

func exportCmd(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("export", "<file> -out dir [-redact-home] [-redact-env] [-secret VALUE]...", stderr)
	out := fs.String("out", "", "directory to write the ATIF documents into (required)")
	redactHome := fs.Bool("redact-home", false, "replace the home directory in paths and text")
	redactEnv := fs.Bool("redact-env", false, "drop environment snapshots from the documents")
	var secrets stringList
	fs.Var(&secrets, "secret", "a value to redact wherever it appears; repeatable")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	path, err := onePath(fs, positional, "session file")
	if err != nil {
		return err
	}
	if *out == "" {
		fs.Usage()
		return fmt.Errorf("%w: -out is required", errUsage)
	}
	s, err := readSession(path)
	if err != nil {
		return err
	}
	opts := export.Options{Subsessions: siblingResolver(path)}
	if len(secrets) > 0 {
		opts.Redactors = append(opts.Redactors, export.Secrets(secrets...))
	}
	if *redactHome {
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("-redact-home: %w", err)
		}
		opts.Redactors = append(opts.Redactors, export.HomePaths(home))
	}
	if *redactEnv {
		opts.Redactors = append(opts.Redactors, export.Environment())
	}

	var docs []*atif.Trajectory
	for tr, err := range export.Trajectories(s) {
		if err != nil {
			return err
		}
		doc, err := export.ToATIF(tr, opts)
		if err != nil {
			return fmt.Errorf("leaf %s: %w", tr.LeafID, err)
		}
		docs = append(docs, doc)
	}
	if err := export.WriteATIF(*out, func(yield func(*atif.Trajectory) bool) {
		for _, d := range docs {
			if !yield(d) {
				return
			}
		}
	}); err != nil {
		return err
	}
	for _, d := range docs {
		fmt.Fprintln(stdout, filepath.Join(*out, export.DocumentName(d)))
	}
	return nil
}

// siblingResolver finds a subsession's file near the exported one: in
// the same directory, or under a sibling project directory when the
// file lives in a jsonl store root, so a linked child is embedded
// rather than referenced.
func siblingResolver(path string) func(id string) (*agentsession.Session, error) {
	dir := filepath.Dir(path)
	return func(id string) (*agentsession.Session, error) {
		for _, pattern := range []string{
			filepath.Join(dir, "*_"+id+".jsonl"),
			filepath.Join(dir, "..", "*", "*_"+id+".jsonl"),
		} {
			matches, err := filepath.Glob(pattern)
			if err != nil || len(matches) == 0 {
				continue
			}
			return readSession(matches[0])
		}
		return nil, nil
	}
}
