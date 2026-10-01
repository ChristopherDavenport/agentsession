// Command agentsession inspects, verifies, exports and lists Agent
// Session Format files, and the sessions of a cas store, from a shell.
//
//	agentsession show <file> | <cas-root> <id> [-leaf id] [-v]
//	agentsession verify <file> | <cas-root> [id]
//	agentsession export <file> | <cas-root> <id> -out dir [-redact-home] [-redact-env] [-secret VALUE]...
//	agentsession list <root> [-cwd path] [-parent id] [-limit n]
//
// Every command opens what it reads read-only. show, verify and
// export read a file directly and never take its lock; given a cas
// store and a session id, they open the store read-only and read the
// session as the file its projection is. A path inside a cas store's
// sessions/<id> directory, its one-line header file included, is read
// as that session through the store. verify of a cas root with no id
// checks the whole store as git fsck does, and then each session's
// request hashes and records as verify of that session would, printing
// only what fails. list opens a jsonl or cas
// store root read-only and prints its sessions, newest first. All four
// are safe to run beside a harness that is writing, which is when an
// operator most wants them.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
)

const usage = `usage: agentsession <command> [flags] <arguments>

commands:
  show    <session> [-leaf id] [-v]  print the entries and the context at a leaf
  verify  <session>                  check every entry hash and recorded request hash
  verify  <cas-root>                 check a whole cas store: logs, objects, sessions, records
  export  <session> -out dir         write ATIF documents for every leaf
  list    <root>                     list the sessions of a jsonl or cas store

A <session> is a session file, or a cas store's root and a session id:
"agentsession show ~/.agent/cas 01995b2a-...". Every command reads
without taking a lock.

Run "agentsession <command> -h" for a command's flags.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one invocation and returns the exit status: 0 on
// success, 1 when the command found a problem to report, 2 on bad
// usage.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	var err error
	switch args[0] {
	case "show":
		err = show(args[1:], stdout, stderr)
	case "verify":
		err = verify(args[1:], stdout, stderr)
	case "export":
		err = exportCmd(args[1:], stdout, stderr)
	case "list":
		err = list(args[1:], stdout, stderr)
	case "-h", "-help", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "agentsession: unknown command %q\n%s", args[0], usage)
		return 2
	}
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.Is(err, errUsage):
		fmt.Fprintf(stderr, "agentsession: %v\n", err)
		return 2
	case errors.Is(err, errFailed):
		// The command has already reported what it found.
		return 1
	default:
		fmt.Fprintf(stderr, "agentsession: %v\n", err)
		return 1
	}
}

// errUsage marks an argument error; errFailed marks a check that the
// command reported on stdout, such as a hash mismatch.
var (
	errUsage  = errors.New("usage")
	errFailed = errors.New("failed")
)

// newFlags returns a flag set that writes its usage to w and returns
// errors instead of exiting.
func newFlags(name, args string, w io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(w)
	fs.Usage = func() {
		fmt.Fprintf(w, "usage: agentsession %s %s\n", name, args)
		fs.PrintDefaults()
	}
	return fs
}

// parse reads flags and positional arguments in any order, so
// "export file -out dir" works as well as "export -out dir file", and
// returns the positionals.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return positional, nil
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

// onePath returns the single positional argument or a usage error.
func onePath(fs *flag.FlagSet, positional []string, what string) (string, error) {
	if len(positional) != 1 {
		fs.Usage()
		return "", fmt.Errorf("%w: expected one %s, got %d arguments", errUsage, what, len(positional))
	}
	return positional[0], nil
}

// readSession loads a session file without a store, so nothing is
// locked or modified.
func readSession(path string) (*agentsession.Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// shortID abbreviates an entry hash for display: the first twelve hex
// characters of the digest, as git shows a commit. Anything that is not
// a hash is returned as it is.
func shortID(id string) string {
	if strings.HasPrefix(id, agentsession.HashPrefix) && len(id) > len(agentsession.HashPrefix)+12 {
		return id[len(agentsession.HashPrefix) : len(agentsession.HashPrefix)+12]
	}
	return id
}

// resolveEntry finds the entry a user named: by its full id, by a
// unique prefix of its digest, with or without the "sha256:" prefix, or
// by the legacy id a migrated entry carries.
func resolveEntry(s *agentsession.Session, arg string) (string, error) {
	if id, ok := s.Resolve(arg); ok {
		return id, nil
	}
	want := strings.TrimPrefix(arg, agentsession.HashPrefix)
	var found []string
	for _, e := range s.Entries() {
		b := e.Base()
		if want != "" && strings.HasPrefix(strings.TrimPrefix(b.ID, agentsession.HashPrefix), want) {
			found = append(found, b.ID)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("%w: %s", agentsession.ErrNoEntry, arg)
	default:
		return "", fmt.Errorf("%s names %d entries; give more of the hash", arg, len(found))
	}
}

// stringList collects a repeatable flag.
type stringList []string

func (l *stringList) String() string { return fmt.Sprint([]string(*l)) }

func (l *stringList) Set(v string) error {
	*l = append(*l, v)
	return nil
}
