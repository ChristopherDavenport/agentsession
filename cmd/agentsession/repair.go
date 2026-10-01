package main

import (
	"context"
	"fmt"
	"io"

	"github.com/ChristopherDavenport/agentsession/cas"
)

// repair rewrites a cas session's damaged log from the records that
// still read, as cas.Store.Repair does, and reports what it kept and
// dropped. It is the one command that writes; with -dry-run it opens
// the store read-only and writes nothing.
func repair(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("repair", "<cas-root> <id> [-dry-run]", stderr)
	dry := fs.Bool("dry-run", false, "report what a repair would keep and drop, writing nothing")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 || !cas.IsStore(positional[0]) {
		fs.Usage()
		return fmt.Errorf("%w: expected a cas store's root and a session id", errUsage)
	}
	root, id := positional[0], positional[1]
	var opts []cas.Option
	if *dry {
		opts = append(opts, cas.WithReadOnly())
	}
	st, err := cas.Open(root, opts...)
	if err != nil {
		return err
	}
	defer st.Close()
	rep, err := st.Repair(context.Background(), id, cas.RepairOptions{DryRun: *dry})
	for _, d := range rep.Damage {
		fmt.Fprintf(stdout, "damaged  line %d (offset %d): %v\n", d.Line, d.Offset, d.Err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "kept     %d entries\n", len(rep.Kept))
	for _, d := range rep.Dropped {
		fmt.Fprintf(stdout, "dropped  %s: %v\n", shortID(d.Entry), d.Err)
	}
	if rep.Head == rep.Named {
		fmt.Fprintf(stdout, "head     %s, as the log last named it\n", orNone(shortID(rep.Head)))
	} else {
		fmt.Fprintf(stdout, "head     %s, the latest kept leaf; the log last named %s, which is not kept\n", orNone(shortID(rep.Head)), shortID(rep.Named))
	}
	fmt.Fprintf(stdout, "mark     %s\n", rep.Mark)
	if *dry {
		fmt.Fprintln(stdout, "dry run: nothing written")
		return nil
	}
	fmt.Fprintf(stdout, "repaired; the damaged log is kept as %s until you remove it\n", rep.DamagedLog)
	return nil
}

// orNone shows an empty head as none.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
