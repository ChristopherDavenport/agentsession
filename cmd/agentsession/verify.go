package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
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
	// A redacted file's ids are hashes of what was written to it, by the
	// redactor or natively after, so they verify it and never its
	// original, which the line has to say rather than leave a reader to
	// assume (RFC 0001, Redaction).
	redacted := s.Header().Redacted
	switch migrated, _ := s.Migrated(); {
	case migrated:
		fmt.Fprintf(stdout, "%d entries read, migrated from an earlier format: ids assigned, not checked\n", s.Len())
	case redacted:
		fmt.Fprintf(stdout, "%d entries read, each id checked against its hash: the ids verify this redacted file, not its original\n", s.Len())
	default:
		fmt.Fprintf(stdout, "%d entries read, each id checked against its hash\n", s.Len())
	}
	if redacted {
		fmt.Fprintf(stdout, "redacted_from %s\n", describeRedactedFrom(s.Header()))
	}
	if n, native := countRedacted(s, declared); redacted || n > 0 {
		line := fmt.Sprintf("%d redacted entries, %d native", n, native)
		if legacyRedacted(s, declared) {
			line += ": a redacted file before 0.13 marks no entry and took no appends, so every entry is the redaction's"
		}
		fmt.Fprintln(stdout, line)
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

// describeRedactedFrom describes a redacted session's link to its
// original, from its header's redacted_from: the original's session,
// the profile when present, and the names of the members a redaction
// recorded about itself. A redacted file without the member, as 0.12
// wrote one, names no original, and one whose member does not read as
// the link, kept as written, names none either.
func describeRedactedFrom(h agentsession.Header) string {
	from := h.RedactedFrom
	if from == nil {
		if raw, ok := h.Extra["redacted_from"]; ok {
			return fmt.Sprintf("%s, kept as written and not read as a link: this file names no original", raw)
		}
		return "absent: this file names no original"
	}
	text := "session " + from.Session
	if from.Profile != "" {
		text += ", profile " + from.Profile
	}
	if len(from.Extra) > 0 {
		text += ", also " + strings.Join(slices.Sorted(maps.Keys(from.Extra)), ", ")
	}
	return text
}

// legacyRedacted reports whether s is a redacted file of a minor before
// 0.13, as its header declared: such a file marks no entry redacted,
// since the member is not defined there, and its writer could not
// append to it, so every entry it holds is the redaction's, and a
// request_hash in it may be the original's, which 0.12 allowed.
func legacyRedacted(s *agentsession.Session, declared string) bool {
	major, minor, err := agentsession.ParseFormat(declared)
	return s.Header().Redacted && err == nil && major == 0 && minor < 13
}

// isRedacted reports whether e is an entry a redaction made: one that
// carries redacted, or any entry of a redacted file before 0.13; see
// legacyRedacted.
func isRedacted(e agentsession.Entry, legacy bool) bool {
	_, ok := agentsession.RedactedOf(e)
	return ok || legacy
}

// countRedacted counts the entries of s a redaction made and the ones
// appended natively, as isRedacted tells them apart.
func countRedacted(s *agentsession.Session, declared string) (redacted, native int) {
	legacy := legacyRedacted(s, declared)
	for _, e := range s.Entries() {
		if isRedacted(e, legacy) {
			redacted++
		} else {
			native++
		}
	}
	return redacted, native
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
// rather than returned. A redacted session is held to the rule as any
// other: a 0.13 redactor gives each response the request_hash of the
// request the redacted session rebuilds, so a mismatch on a redacted
// entry is a fault as on a native one (RFC 0001, Redaction). Only a
// redacted file before 0.13, whose request hashes may be the
// original's, has a mismatch that fails nothing: it is counted apart,
// as expected.
func checkSession(s *agentsession.Session, declared, prefix string, stdout io.Writer, all bool) (problem bool, notes []string) {
	var checked, unhashed, differ, failed int
	legacy := legacyRedacted(s, declared)
	for _, e := range s.Entries() {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		id := prefix + shortID(r.ID)
		err := s.Verify(r.ID)
		switch {
		case legacy && (errors.Is(err, agentsession.ErrHashMismatch) || errors.Is(err, agentsession.ErrOmitDivergence)):
			// A redacted file before 0.13 may hold the original's
			// request_hash, which need not match the request the
			// redacted bodies rebuild, and the format reads such a
			// file as it stands.
			differ++
			if all {
				fmt.Fprintf(stdout, "%s  differs from recorded %s, as a request_hash in a redacted file before 0.13 may\n", id, r.RequestHash)
			}
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
		if legacy {
			fmt.Fprintf(stdout, "%d verified, %d without hash, %d redacted before 0.13 differ as expected, %d failed\n", checked, unhashed, differ, failed)
		} else {
			fmt.Fprintf(stdout, "%d verified, %d without hash, %d failed\n", checked, unhashed, failed)
		}
	}
	problem = failed > 0
	noted := map[string]bool{}
	for _, leaf := range s.Leaves() {
		unresolved, _ := s.UnresolvedOf(leaf)
		for _, c := range unresolved {
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
const unresolvedOfNote = "note: config %s names an omitted list by an of its path cannot resolve; the element is kept as written, which is not corruption: the list reaches no request"
