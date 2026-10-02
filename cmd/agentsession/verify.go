package main

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

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
	s, declared, st, err := readDeclared(src)
	if err != nil {
		return err
	}
	if st != nil {
		defer st.Close()
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
	problem, notes := checkSession(s, declared, "", stdout, true)
	// A link's target is found through the store the session is in; a
	// file names no store, so its links go unchecked.
	if st != nil && checkLinks(s, resolveIn(st, src.path), "", stdout) {
		problem = true
	}
	printNotes(stdout, notes)
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

// sourceNote and reasonNote follow a run's source or end that
// disagrees with its segment, in a file of any minor: no release
// checks either as it is written, only VerifyRecords after, so the
// writer of the run, of whatever version, computed it otherwise.
const (
	sourceNote = "note: no release checks a run's source as it is written; the writer of this run computed it otherwise than ComputeSource"
	reasonNote = "note: no release checks a run's end as it is written; the writer of this run computed its reason or pending list otherwise than ComputeReason and Run.Pending"
)

// earlierNote follows such an error in a file of a minor before 0.9,
// which did not forbid what failed; see noteFor.
const earlierNote = "note: this file declares a minor before 0.9, which did not forbid what failed; it is checked here by 0.9's rules"

// checkSession checks each response's request hash and the records to
// each leaf, printing each line under prefix: every response's result
// when all is set, else only the failures. declared is the format the
// session's header declared where it is stored, which the notes turn
// on; for a file it is the session's DeclaredFormat, and for a session
// a cas store holds it is the stored header's, since the projection
// the session is read from declares this release's format. It reports
// whether anything failed, and the notes the failures earn, as noteFor
// gives them, distinct and in noteOrder. A config entry that names an
// omitted list by an of the path cannot resolve fails nothing, since
// the format keeps such an element as written and the list reaches no
// request, but it names the entry, so its note is printed under prefix
// rather than returned.
func checkSession(s *agentsession.Session, declared, prefix string, stdout io.Writer, all bool) (problem bool, notes []string) {
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
		case errors.Is(err, agentsession.ErrOmitDivergence):
			failed++
			fmt.Fprintf(stdout, "%s  MISMATCH recorded %s, the request with the items the omit setting leaves out\n", id, r.RequestHash)
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
	noted := map[string]bool{}
	for _, leaf := range s.Leaves() {
		for _, c := range unresolvedOf(s, leaf) {
			if !noted[c.ID] {
				noted[c.ID] = true
				fmt.Fprintf(stdout, "%s"+unresolvedOfNote+"\n", prefix, shortID(c.ID))
			}
		}
	}
	for _, leaf := range s.Leaves() {
		if err := s.VerifyRecords(leaf); err != nil {
			problem = true
			fmt.Fprintf(stdout, "%srecords to %s  ERROR %v\n", prefix, shortID(leaf), err)
			if n := noteFor(declared, err); n != "" {
				// noteFor joins the note on an earlier minor with the
				// writer's note; each is kept once.
				for _, line := range strings.Split(n, "\n") {
					if !slices.Contains(notes, line) {
						notes = append(notes, line)
					}
				}
			}
		}
	}
	return problem, notes
}

// checkLinks checks each subsession link against the header of the
// session it names, found through resolve, as VerifyLinks does, and
// prints the first failure under prefix: the link, the session it
// names, and what disagrees. It reports whether one failed. A link
// whose target names another parent or another call breaks the format,
// so it fails the command rather than warns; a target the store lacks
// is a child that never started and is no failure.
func checkLinks(s *agentsession.Session, resolve func(string) (*agentsession.Session, error), prefix string, stdout io.Writer) bool {
	err := s.VerifyLinks(resolve)
	if err == nil {
		return false
	}
	var le *agentsession.LinkError
	if errors.As(err, &le) {
		fmt.Fprintf(stdout, "%slink %s -> %s  ERROR %v\n", prefix, shortID(le.Entry), le.Session, le.Err)
	} else {
		fmt.Fprintf(stdout, "%slinks  ERROR %v\n", prefix, err)
	}
	return true
}

// noteOrder is the order the notes a check collects are printed in,
// whatever order the failures earned them: the note on the minor a
// file declared before the notes on what its writer computed, as
// noteFor joins them. A note not in it, which none is today, follows,
// sorted, so the order is stable whatever is added.
var noteOrder = []string{earlyNote, earlierNote, sourceNote, reasonNote}

// printNotes prints each distinct note once, in noteOrder.
func printNotes(stdout io.Writer, notes []string) {
	rest := slices.Clone(notes)
	for _, n := range noteOrder {
		if i := slices.Index(rest, n); i >= 0 {
			fmt.Fprintln(stdout, n)
			rest = slices.Delete(rest, i, i+1)
		}
	}
	slices.Sort(rest)
	for _, n := range slices.Compact(rest) {
		fmt.Fprintln(stdout, n)
	}
}

// gained09 reports whether err breaks a rule draft 0.9 gained, after
// 0.8 and after v0.0.12 first wrote 0.9.
func gained09(err error) bool {
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

// amended09 reports whether err breaks a rule draft 0.9 gained after
// v0.0.12, v0.0.13 and v0.0.14 wrote it, in a file whose header said
// 0.9 when it was read: such a file may be one of theirs, or one they
// appended to, rather than corrupt. A run's source and end are left
// out: no later release refuses them as written either; see
// writerNote.
func amended09(declared string, err error) bool {
	return declared == "agentsession/0.9" && gained09(err) && writerNote(err) == ""
}

// writerNote is the note for a rule no release checks as an entry is
// appended, or "".
func writerNote(err error) string {
	switch {
	case errors.Is(err, agentsession.ErrSourceMismatch):
		return sourceNote
	case errors.Is(err, agentsession.ErrReasonMismatch):
		return reasonNote
	}
	return ""
}

// noteFor is the note a records error in a file that declared the
// given format earns, or "": a 0.9 file may come from an early writer
// of it, and a file of an earlier minor was written before 0.9 forbade
// what failed. A run's source or end that disagrees with its segment
// earns its own note in a file of any minor, after the earlier
// minor's where that applies. Any other error in a file of the current
// minor earns none.
func noteFor(declared string, err error) string {
	if amended09(declared, err) {
		return earlyNote
	}
	note := writerNote(err)
	if _, minor, perr := agentsession.ParseFormat(declared); perr == nil && minor < 9 && gained09(err) {
		if note != "" {
			return earlierNote + "\n" + note
		}
		return earlierNote
	}
	return note
}

// unresolvedOfNote is printed on a line of its own for a config entry
// whose omitted list names an earlier entry's list the path does not
// hold.
const unresolvedOfNote = "note: config %s names an omitted list by an of that no config entry on its path puts in force; the element is kept as written, which is not corruption: the list reaches no request"

// unresolvedOf returns the config entries on the path to leaf that carry
// a keep with an of the path cannot resolve, in path order: replaying
// the path's settings leaves the element in the list as written.
func unresolvedOf(s *agentsession.Session, leaf string) []*agentsession.ConfigEntry {
	var out []*agentsession.ConfigEntry
	var settings agentsession.Settings
	for _, e := range s.Path(leaf) {
		c, ok := e.(*agentsession.ConfigEntry)
		if !ok {
			continue
		}
		settings = settings.Apply(c)
		for _, el := range c.InstructionsOmitted {
			if el.ID != "" || el.Keep == 0 || el.Of == "" {
				continue
			}
			if slices.ContainsFunc(settings.InstructionsOmitted, func(p agentsession.OmittedPart) bool {
				return p.ID == "" && p.Keep == el.Keep && p.Of == el.Of
			}) {
				out = append(out, c)
				break
			}
		}
	}
	return out
}
