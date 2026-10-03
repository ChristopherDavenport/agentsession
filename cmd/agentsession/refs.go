package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/cas"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

// refPrefix introduces a ref where a command takes a session id:
// "show <cas-root> ref:conversations/slack/C123".
const refPrefix = "ref:"

// openRefs opens the store at root read-only, for the refs it keeps: a
// cas store or a jsonl store's layout. The sqlite store is a module of
// its own and is read through its own package.
func openRefs(root string) (interface {
	agentsession.RefStore
	Close() error
}, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", root)
	}
	if cas.IsStore(root) {
		return cas.Open(root, cas.WithReadOnly())
	}
	return jsonl.Open(root, jsonl.WithReadOnly())
}

// resolveRefSource turns the session id of a source that names a ref
// into the session the ref points at, noting it on stderr. A ref whose
// session is gone is an error naming it.
func resolveRefSource(src source, stderr io.Writer) (source, error) {
	name, ok := strings.CutPrefix(src.id, refPrefix)
	if !ok {
		return src, nil
	}
	st, err := cas.Open(src.path, cas.WithReadOnly())
	if err != nil {
		return src, err
	}
	defer st.Close()
	t, err := st.ResolveRef(context.Background(), name)
	if err != nil {
		return src, fmt.Errorf("ref %s: %w", name, err)
	}
	fmt.Fprintf(stderr, "agentsession: %s%s is session %s", refPrefix, name, t.Session)
	if t.Entry != "" {
		fmt.Fprintf(stderr, ", pinned at %s", shortID(t.Entry))
	}
	fmt.Fprintln(stderr)
	src.id = t.Session
	return src, nil
}

// refs lists the refs a store holds.
func refs(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("refs", "<root> [prefix]", stderr)
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) < 1 || len(positional) > 2 {
		fs.Usage()
		return fmt.Errorf("%w: expected a store root and at most a prefix, got %d arguments", errUsage, len(positional))
	}
	prefix := ""
	if len(positional) == 2 {
		prefix = positional[1]
	}
	st, err := openRefs(positional[0])
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSESSION\tENTRY\tSTATE")
	var failed error
	for r, err := range st.ListRefs(ctx, prefix) {
		if err != nil {
			failed = err
			break
		}
		state := "ok"
		if _, err := st.ResolveRef(ctx, r.Name); errors.Is(err, agentsession.ErrNoSession) {
			state = "dangling"
		} else if err != nil {
			state = err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Name, r.Target.Session, orDash(shortID(r.Target.Entry)), state)
	}
	tw.Flush()
	return failed
}

// refCmd prints one ref, and with -log the updates that moved it.
func refCmd(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("ref", "<root> <name> [-log]", stderr)
	showLog := fs.Bool("log", false, "also print every update of the ref, newest first")
	positional, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 2 {
		fs.Usage()
		return fmt.Errorf("%w: expected a store root and a ref name, got %d arguments", errUsage, len(positional))
	}
	name := strings.TrimPrefix(positional[1], refPrefix)
	st, err := openRefs(positional[0])
	if err != nil {
		return err
	}
	defer st.Close()
	ctx := context.Background()
	t, err := st.ResolveRef(ctx, name)
	state := "ok"
	switch {
	case errors.Is(err, agentsession.ErrNoRef) && *showLog:
		// A deleted ref still has a log.
		state = "none"
	case errors.Is(err, agentsession.ErrNoSession):
		state = "dangling: the session is not in the store"
	case err != nil:
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "ref\t%s\n", name)
	if !t.IsZero() {
		fmt.Fprintf(tw, "session\t%s\n", t.Session)
		if t.Entry != "" {
			fmt.Fprintf(tw, "entry\t%s\n", t.Entry)
		}
	}
	fmt.Fprintf(tw, "state\t%s\n", state)
	tw.Flush()
	if !*showLog {
		return nil
	}
	fmt.Fprintln(stdout)
	tw = tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TIME\tFROM\tTO\tREASON")
	for u, err := range st.RefLog(ctx, name) {
		if err != nil {
			tw.Flush()
			return err
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", u.Time.UTC().Format(time.RFC3339), orDash(describeTarget(u.Old)), orDash(describeTarget(u.New)), orDash(u.Reason))
	}
	return tw.Flush()
}

// describeTarget abbreviates a target as session@entry.
func describeTarget(t agentsession.RefTarget) string {
	if t.Entry == "" {
		return t.Session
	}
	return t.Session + "@" + shortID(t.Entry)
}
