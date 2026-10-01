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
	problem, early := checkSession(s, "", stdout, true)
	if early {
		fmt.Fprintln(stdout, earlyNote)
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

// earlyNote follows a records error a 0.9 file of an early writer may
// carry; see amended09.
const earlyNote = "note: draft 0.9 gained these rules after v0.0.12, v0.0.13 and v0.0.14 wrote it; a 0.9 file one of them wrote, or appended to, may break them without being corrupt"

// checkSession checks each response's request hash and the records to
// each leaf, printing each line under prefix: every response's result
// when all is set, else only the failures. It reports whether anything
// failed, and whether a failure is one an early 0.9 writer may have
// left.
func checkSession(s *agentsession.Session, prefix string, stdout io.Writer, all bool) (problem, early bool) {
	var checked, unhashed, failed int
	for _, e := range s.Entries() {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		id := prefix + shortID(r.ID)
		switch err := s.Verify(r.ID); {
		case errors.Is(err, agentsession.ErrNoHash):
			unhashed++
			if all {
				fmt.Fprintf(stdout, "%s  no hash recorded\n", id)
			}
		case err == nil:
			checked++
			if all {
				fmt.Fprintf(stdout, "%s  ok\n", id)
			}
		case errors.Is(err, agentsession.ErrHashMismatch):
			failed++
			fmt.Fprintf(stdout, "%s  MISMATCH recorded %s\n", id, r.RequestHash)
		default:
			failed++
			fmt.Fprintf(stdout, "%s  ERROR %v\n", id, err)
		}
	}
	if all {
		fmt.Fprintf(stdout, "%d verified, %d without hash, %d failed\n", checked, unhashed, failed)
	}
	problem = failed > 0
	for _, leaf := range s.Leaves() {
		if err := s.VerifyRecords(leaf); err != nil {
			problem = true
			fmt.Fprintf(stdout, "%srecords to %s  ERROR %v\n", prefix, shortID(leaf), err)
			early = early || amended09(s.DeclaredFormat(), err)
		}
	}
	return problem, early
}

// amended09 reports whether err breaks a rule draft 0.9 gained after
// v0.0.12, v0.0.13 and v0.0.14 wrote it, in a file whose header said
// 0.9 when it was read: such a file may be one of theirs, or one they
// appended to, rather than corrupt. A file of an earlier minor, which
// Read brings up to the current one, never had these rules.
func amended09(declared string, err error) bool {
	if declared != "agentsession/0.9" {
		return false
	}
	for _, e := range []error{
		agentsession.ErrReasonMismatch,
		agentsession.ErrCallRejected,
		agentsession.ErrRejectDispatched,
		agentsession.ErrCallCompleted,
		agentsession.ErrCallIDRepeated,
		agentsession.ErrCallIDEmpty,
		agentsession.ErrSourceMismatch,
		agentsession.ErrBadTarget,
	} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
