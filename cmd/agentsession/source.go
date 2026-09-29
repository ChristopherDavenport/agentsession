package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
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

// casResolver finds a linked subsession in the same cas store.
func casResolver(root string) func(id string) (*agentsession.Session, error) {
	return func(id string) (*agentsession.Session, error) {
		s, err := readSource(source{path: root, id: id})
		if errors.Is(err, agentsession.ErrNoSession) || errors.Is(err, cas.ErrBadName) {
			return nil, nil
		}
		return s, err
	}
}

// verifyStore checks a whole cas store as git fsck does: journal,
// objects, packs and every session's entries.
func verifyStore(root string, stdout io.Writer) error {
	st, err := cas.Open(root, cas.WithReadOnly())
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.Verify(context.Background())
	if err != nil {
		return err
	}
	for _, p := range rep.Problems {
		fmt.Fprintf(stdout, "%s\n", p)
	}
	fmt.Fprintf(stdout, "%d sessions, %d entries, %d objects checked, %d problems\n", rep.Sessions, rep.Entries, rep.Objects, len(rep.Problems))
	if rep.Sessions == 0 && rep.OK() {
		fmt.Fprintf(stdout, "nothing to verify: %s holds no sessions\n", root)
		return errFailed
	}
	if !rep.OK() {
		return errFailed
	}
	return nil
}
