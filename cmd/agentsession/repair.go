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
// the store read-only and writes nothing. With -ref <name> in place of
// a session id it repairs that ref instead, as cas.Store.RepairRef
// does: its damaged log is set aside and restarted from the ref's
// target. (An id of the form ref:<name> names the session a ref points
// to, as in every other command.)
func repair(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("repair", "<cas-root> <id> | <cas-root> -ref <name> [-dry-run]", stderr)
	dry := fs.Bool("dry-run", false, "report what a repair would keep and drop, writing nothing")
	ref := fs.String("ref", "", "repair this ref's damaged log instead of a session's")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if *ref != "" {
		if len(positional) != 1 || !cas.IsStore(positional[0]) {
			fs.Usage()
			return fmt.Errorf("%w: expected a cas store's root and -ref <name>", errUsage)
		}
		return repairRef(positional[0], *ref, *dry, stdout)
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

// repairRef restarts a damaged ref's log, or with dry set reports the
// damage and writes nothing.
func repairRef(root, name string, dry bool, stdout io.Writer) error {
	var opts []cas.Option
	if dry {
		opts = append(opts, cas.WithReadOnly())
	}
	st, err := cas.Open(root, opts...)
	if err != nil {
		return err
	}
	defer st.Close()
	if dry {
		rep, err := st.Verify(context.Background())
		if err != nil {
			return err
		}
		for _, p := range rep.Problems {
			if p.Kind == cas.KindRef && p.Ref == name {
				fmt.Fprintf(stdout, "damaged  %v\n", p.Err)
			}
		}
		fmt.Fprintln(stdout, "dry run: nothing written")
		return nil
	}
	rep, err := st.RepairRef(context.Background(), name)
	for _, d := range rep.Damage {
		fmt.Fprintf(stdout, "damaged  %v\n", d)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "target   %s\n", orNone(rep.Target.String()))
	if rep.DamagedLog != "" {
		fmt.Fprintf(stdout, "repaired; the damaged log is kept as %s until you remove it\n", rep.DamagedLog)
	}
	return nil
}

// orNone shows an empty head as none.
func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
