package main

import (
	"fmt"
	"io"

	"github.com/ChristopherDavenport/agentsession/cas"
)

// migrate opens a cas store for writing, which migrates a store from
// before per-session logs, and closes it. It is the open a writer of
// this release would make, without starting one; it refuses while a
// writer of the earlier release holds a session.
func migrate(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("migrate", "<cas-root>", stderr)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	root, err := onePath(fs, positional, "cas store root")
	if err != nil {
		return err
	}
	if !cas.IsStore(root) {
		fs.Usage()
		return fmt.Errorf("%w: %s is not a cas store", errUsage, root)
	}
	legacy := cas.NeedsMigration(root)
	st, err := cas.Open(root)
	if err != nil {
		return err
	}
	if err := st.Close(); err != nil {
		return err
	}
	switch {
	case cas.NeedsMigration(root):
		fmt.Fprintf(stdout, "a session failed to migrate, and the journal is kept for the next writing open to try again; agentsession verify %s names it\n", root)
		return errFailed
	case legacy:
		fmt.Fprintln(stdout, "migrated")
	default:
		fmt.Fprintln(stdout, "nothing to migrate")
	}
	return nil
}
