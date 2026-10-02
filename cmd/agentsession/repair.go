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
	for _, d := range rep.Dropped {
		fmt.Fprintf(stdout, "dropped  %s: %v\n", shortID(d.Entry), d.Err)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "kept     %d entries, %d of them recovered from what the damage hid\n", len(rep.Kept), len(rep.Hidden)+len(rep.Salvaged))
	for _, e := range rep.Salvaged {
		place := "not the head"
		if e == rep.Head {
			place = "the head"
		}
		fmt.Fprintf(stdout, "salvaged %s: named only by a damaged record, and its objects whole; %s\n", shortID(e), place)
	}
	switch {
	case rep.Unread:
		fmt.Fprintf(stdout, "head     %s, the last head the repair could read; a damaged line after it may have moved the head or appended entries the repair cannot name\n", orNone(shortID(rep.Head)))
	case rep.Head == rep.Named:
		fmt.Fprintf(stdout, "head     %s, as the log last named it\n", orNone(shortID(rep.Head)))
	default:
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
