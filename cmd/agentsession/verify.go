package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/ChristopherDavenport/agentsession"
)

func verify(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("verify", "<file> | <cas-root> [id]", stderr)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	src, err := sessionSource(fs, positional, stderr)
	if err != nil {
		return err
	}
	if src.isStore() {
		return verifyStore(src.path, stdout)
	}
	s, err := readSource(src)
	if err != nil {
		return err
	}
	// Reading checks each entry's id against its hash, except in a file
	// of an earlier minor, whose ids the migration assigns. A header
	// alone checks nothing, and "0 failed" of it would read as a pass.
	if s.Len() == 0 {
		fmt.Fprintf(stdout, "nothing to verify: %s holds a header and no entries\n", src)
		if t := s.Truncated(); t != nil {
			fmt.Fprintf(stdout, "truncated: line %d was cut short: %v\n", t.Line, t.Err)
		}
		return errFailed
	}
	if migrated, _ := s.Migrated(); migrated {
		fmt.Fprintf(stdout, "%d entries read, migrated from an earlier format: ids assigned, not checked\n", s.Len())
	} else {
		fmt.Fprintf(stdout, "%d entries read, each id checked against its hash\n", s.Len())
	}
	var checked, unhashed, failed int
	for _, e := range s.Entries() {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		id := shortID(r.ID)
		switch err := s.Verify(r.ID); {
		case errors.Is(err, agentsession.ErrNoHash):
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
	for _, leaf := range s.Leaves() {
		if err := s.VerifyRecords(leaf); err != nil {
			problem = true
			fmt.Fprintf(stdout, "records to %s  ERROR %v\n", shortID(leaf), err)
		}
	}
	if t := s.Truncated(); t != nil {
		problem = true
		fmt.Fprintf(stdout, "truncated: line %d was cut short: %v\n", t.Line, t.Err)
	}
	if problem {
		return errFailed
	}
	return nil
}
