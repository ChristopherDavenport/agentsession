package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/ChristopherDavenport/agentsession"
)

func verify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", "<file>", stderr)
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
	var checked, unhashed, failed int
	for _, e := range s.Entries() {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		id := r.ID
		switch err := s.Verify(id); {
		case err == nil && r.RequestHash == "":
			unhashed++
			fmt.Fprintf(stdout, "%s  no hash recorded\n", id)
		case err == nil:
			checked++
			fmt.Fprintf(stdout, "%s  ok\n", id)
		case errors.Is(err, agentsession.ErrHashMismatch):
			failed++
			fmt.Fprintf(stdout, "%s  MISMATCH recorded %s\n", id, r.RequestHash)
		default:
			failed++
			fmt.Fprintf(stdout, "%s  ERROR %v\n", id, err)
		}
	}
	fmt.Fprintf(stdout, "%d verified, %d without hash, %d failed\n", checked, unhashed, failed)
	problem := failed > 0
	if t := s.Truncated(); t != nil {
		problem = true
		fmt.Fprintf(stdout, "truncated: line %d was cut short: %v\n", t.Line, t.Err)
	}
	if problem {
		return errFailed
	}
	return nil
}
