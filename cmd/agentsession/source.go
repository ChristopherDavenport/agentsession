package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
)

// source is where a session is read from: a file, or a session a cas
// store holds.
type source struct {
	path string // the file, or the cas store's root
	id   string // the session, when path is a cas store
}

func (src source) String() string {
	if src.id == "" {
		return src.path
	}
	return src.path + " " + src.id
}

// isStore reports whether the source is a whole cas store rather than
// one session.
func (src source) isStore() bool { return src.id == "" && cas.IsStore(src.path) }

// sessionSource reads a command's positional arguments as a session:
// "<file>", or "<cas-root> <id>". A path inside a cas store's
// sessions/<id> directory is read as that session through the store,
// since the header file there is a valid session file holding no
// entries, and reading it as one would report on nothing. A lone cas
// root is returned as it is; the command decides what it means.
func sessionSource(fs *flag.FlagSet, positional []string, stderr io.Writer) (source, error) {
	switch len(positional) {
	case 1:
		p := positional[0]
		if cas.IsStore(p) {
			return source{path: p}, nil
		}
		if root, id, ok := casSession(p); ok {
			fmt.Fprintf(stderr, "agentsession: %s is in the cas store %s; reading session %s through the store\n", p, root, id)
			return source{path: root, id: id}, nil
		}
		return source{path: p}, nil
	case 2:
		if !cas.IsStore(positional[0]) {
			fs.Usage()
			return source{}, fmt.Errorf("%w: %s is not a cas store; a session id follows only a store's root", errUsage, positional[0])
		}
		return source{path: positional[0], id: positional[1]}, nil
	}
	fs.Usage()
	return source{}, fmt.Errorf("%w: expected a session file, or a cas store and a session id, got %d arguments", errUsage, len(positional))
}

// casSession finds the cas store a path lies in and the session whose
// directory holds it: root/sessions/<id> itself, or anything under it.
func casSession(p string) (root, id string, ok bool) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", "", false
	}
	for dir := filepath.Dir(abs); ; dir = filepath.Dir(dir) {
		if cas.IsStore(dir) {
			rel, err := filepath.Rel(dir, abs)
			if err != nil {
				return "", "", false
			}
			parts := strings.Split(rel, string(filepath.Separator))
			if len(parts) < 2 || parts[0] != "sessions" {
				return "", "", false
			}
			return dir, parts[1], true
		}
		if filepath.Dir(dir) == dir {
			return "", "", false
		}
	}
}

// requireSession refuses a whole store where a command wants one
// session, naming the invocation that would work.
func requireSession(fs *flag.FlagSet, src source, command string) error {
	if !src.isStore() {
		return nil
	}
	fs.Usage()
	return fmt.Errorf("%w: %s is a cas store; name a session: agentsession %s %s <id>", errUsage, src.path, command, src.path)
}

// readSource loads the session a source names without taking a lock or
// writing anything. A cas session is projected into memory and read as
// the file the projection is, so every command reports on it exactly
// as it would on the file ProjectDir writes.
func readSource(src source) (*agentsession.Session, error) {
	if src.id == "" {
		return readSession(src.path)
	}
	st, err := cas.Open(src.path, cas.WithReadOnly())
	if err != nil {
		return nil, err
	}
	defer st.Close()
	return readFrom(st, src)
}

// readDeclared loads the session a source names, as readSource does,
// and the format its header declares where it is stored, which is what
// verify's notes turn on. For a file that is the session's
// DeclaredFormat. For a session a cas store holds it is not: the
// session is read from its projection, which is the file this release
// would write and so declares this release's format, as any file names
// its writer's; the header the store keeps declares the format the
// session was written under, raised only when this release appends to
// it. The store exposes that header through Store.Read, whose session
// declares it, and through List; one session is read again here,
// since the store offers no cheaper read of its header alone.
func readDeclared(src source) (*agentsession.Session, string, error) {
	if src.id == "" {
		s, err := readSession(src.path)
		if err != nil {
			return nil, "", err
		}
		return s, s.DeclaredFormat(), nil
	}
	st, err := cas.Open(src.path, cas.WithReadOnly())
	if err != nil {
		return nil, "", err
	}
	defer st.Close()
	s, err := readFrom(st, src)
	if err != nil {
		return nil, "", err
	}
	stored, err := st.Read(context.Background(), src.id)
	if err != nil {
		return nil, "", fmt.Errorf("%s: %w", src, err)
	}
	return s, stored.DeclaredFormat(), nil
}

// readFrom loads a session from a cas store already open, as readSource
// does. The session declares this release's format whatever its stored
// header says; readDeclared is where the stored format comes from.
func readFrom(st *cas.Store, src source) (*agentsession.Session, error) {
	var buf bytes.Buffer
	if err := st.Project(context.Background(), &buf, src.id); err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	s, err := agentsession.Read(&buf)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", src, err)
	}
	return s, nil
}

// casResolver finds a linked subsession in the same cas store, opening
// the store read-only for each.
func casResolver(root string) func(id string) (*agentsession.Session, error) {
	return func(id string) (*agentsession.Session, error) {
		st, err := cas.Open(root, cas.WithReadOnly())
		if err != nil {
			return nil, err
		}
		defer st.Close()
		return resolveIn(st, root)(id)
	}
}

// resolveIn finds a linked subsession in a cas store already open. A
// session the store does not hold, or an id that names none, is nil: a
// subsession link is written when its call is dispatched, so a child
// the store lacks is one that never started.
func resolveIn(st *cas.Store, root string) func(id string) (*agentsession.Session, error) {
	return func(id string) (*agentsession.Session, error) {
		s, err := readFrom(st, source{path: root, id: id})
		if errors.Is(err, agentsession.ErrNoSession) || errors.Is(err, cas.ErrBadName) {
			return nil, nil
		}
		return s, err
	}
}

// verifyStore checks a whole cas store as git fsck does: logs,
// objects, packs and every session's entries, and then each session as
// verify of one session does, its request hashes, records and
// subsession links, printing only what fails.
func verifyStore(root string, stdout io.Writer) error {
	ctx := context.Background()
	st, err := cas.Open(root, cas.WithReadOnly())
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.Verify(ctx)
	if err != nil {
		return err
	}
	// A leftover is printed and is no failure: nothing reads it, and a
	// sweep removes it.
	leftovers := 0
	for _, p := range rep.Problems {
		fmt.Fprintf(stdout, "%s\n", p)
		if p.Kind == cas.KindLeftover {
			leftovers++
		}
	}
	fmt.Fprintf(stdout, "%d sessions, %d entries, %d objects checked, %d problems", rep.Sessions, rep.Entries, rep.Objects, len(rep.Problems)-leftovers)
	if leftovers > 0 {
		fmt.Fprintf(stdout, ", %d leftover objects nothing needs", leftovers)
	}
	fmt.Fprintln(stdout)
	if rep.Sessions == 0 && rep.OK() {
		fmt.Fprintf(stdout, "nothing to verify: %s holds no sessions\n", root)
		return errFailed
	}
	// A session that fails to list or to open is one the store's walk
	// has reported; it is named here as unchecked, and not counted
	// again.
	// The listing's header is the one the store keeps, so its format is
	// the one the session declared; see readDeclared.
	checked, failing, unchecked := 0, 0, 0
	var notes []string
	var listed []agentsession.Header
	for sum, err := range st.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			fmt.Fprintf(stdout, "records not checked: %v\n", err)
			unchecked++
			continue
		}
		listed = append(listed, sum.Header)
	}
	resolve := resolveIn(st, root)
	for _, h := range listed {
		s, err := readFrom(st, source{path: root, id: h.ID})
		if err != nil {
			fmt.Fprintf(stdout, "%s: records not checked: %v\n", h.ID, err)
			unchecked++
			continue
		}
		checked++
		problem, ns := checkSession(s, h.Format, h.ID+": ", stdout, false)
		if checkLinks(s, resolve, h.ID+": ", stdout) {
			problem = true
		}
		if problem {
			failing++
			for _, n := range ns {
				if !slices.Contains(notes, n) {
					notes = append(notes, n)
				}
			}
		}
	}
	// The notes that name no session are printed once for every
	// session that earned them; the one on a run that took up nothing
	// names its run and was printed beside that session's failure.
	printNotes(stdout, notes)
	fmt.Fprintf(stdout, "%d sessions' hashes and records checked, %d failed, %d not checked\n", checked, failing, unchecked)
	if cas.NeedsMigration(root) {
		fmt.Fprintf(stdout, "the store holds a journal from before per-session logs; stop every writer, take a copy, and run agentsession migrate %s\n", root)
	}
	if !rep.OK() || failing > 0 || unchecked > 0 {
		return errFailed
	}
	return nil
}
