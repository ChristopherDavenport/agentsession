//go:build unix

package cas

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
)

// oldCreate does on disk what a writer of the release before per-session
// logs did to create a session and append one entry to it durably, in
// that release's order: the session's lock and directory, then, under
// the sweep's lock shared, the journal records, the commit point, then
// the session's files. It returns nil only for a create that release
// acknowledged, and, like that release, removes the directory of one it
// did not.
func oldCreate(root, id string, e agentsession.Entry, guard func() (*dirLock, error)) error {
	lk, err := lockFile(filepath.Join(root, "locks", id))
	if err != nil {
		return err
	}
	defer lk.release()
	dir := filepath.Join(root, "sessions", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	g, err := guard()
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	defer g.release()
	size, _, err := (&Store{root: root, objs: newObjects(root)}).storeEntry(e, true, nil)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	var recs []byte
	for _, r := range []logRecord{
		{Op: "create", Session: id},
		{Op: "mark", Session: id, Mark: MarkRecord},
		{Op: "append", Session: id, Entry: e.Base().ID, Head: e.Base().ID, Seq: 1, Size: size},
	} {
		line, err := r.encode()
		if err != nil {
			return err
		}
		recs = append(recs, line...)
	}
	f, err := os.OpenFile(filepath.Join(root, journalFile), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		os.RemoveAll(dir)
		return fmt.Errorf("cas: journal: %w", err)
	}
	_, err = f.Write(recs)
	if err == nil {
		err = f.Sync()
	}
	f.Close()
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	// Committed: from here the release acknowledged the create and the
	// append, and wrote its indexes of them without syncing them.
	os.WriteFile(filepath.Join(dir, "record"), []byte(MarkRecord+"\n"), 0o600)
	if err := writeHeader(nil, dir, agentsession.New(agentsession.Header{ID: id}).Header()); err != nil {
		return err
	}
	os.WriteFile(filepath.Join(dir, logName), fmt.Appendf(nil, "%s %d\n", e.Base().ID, size), 0o600)
	os.WriteFile(filepath.Join(dir, "HEAD"), []byte(e.Base().ID+"\n"), 0o600)
	return nil
}

const lockSH = syscall.LOCK_SH

var errLockHeld = errors.New("the sweep's lock is held")

// TestMigrateOldWriterMidway: a writer of the earlier version that
// holds no session when the migration starts, and creates one while it
// runs, either has its create refused or finds it readable afterwards:
// nothing it acknowledged is left where no open reads it, and nothing
// it writes after the migration lands in a store that cannot read it.
func TestMigrateOldWriterMidway(t *testing.T) {
	ctx := context.Background()
	root, want := legacyStore(t)
	e := item("midway")
	if _, err := agentsession.New(agentsession.Header{ID: "midway"}).Append(e); err != nil {
		t.Fatal(err)
	}
	// The writer starts its create after the migration has read the
	// journal and listed the sessions. It takes the sweep's lock if it
	// can; if the migration holds it, the writer waits for it as that
	// release did, after the migration.
	var late chan error
	acked := false
	migrating = func() {
		migrating = nil
		guarded := false
		err := oldCreate(root, "midway", e, func() (*dirLock, error) {
			lk, ok, err := lockTry(filepath.Join(root, "sweep.lock"), lockSH)
			if err == nil && !ok {
				err = errLockHeld
			}
			guarded = err == nil
			return lk, err
		})
		switch {
		case err == nil:
			acked = true
		case !guarded && errors.Is(err, errLockHeld):
			late = make(chan error, 1)
			go func() {
				late <- oldCreate(root, "midway", e, func() (*dirLock, error) {
					return lockShared(ctx, filepath.Join(root, "sweep.lock"))
				})
			}()
		default:
			t.Errorf("the earlier writer's create: %v", err)
		}
	}
	defer func() { migrating = nil }()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if late == nil {
		t.Error("the earlier writer took the sweep's lock while the migration ran")
	} else if err := <-late; err == nil {
		acked = true
		t.Error("the earlier writer committed to the store after the migration")
	} else if !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("the earlier writer's create, after the migration: %v", err)
	}
	// A release from v0.0.16 to v0.0.18 takes the tombstone for no
	// journal.
	if _, err := os.Stat(filepath.Join(root, journalFile)); err == nil {
		t.Error("the tombstone stats as a journal")
	}
	st, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	checkMigrated(t, root, want)
	if acked {
		s, err := st.Open(ctx, "midway")
		if err != nil || s.Len() != 1 {
			t.Fatalf("a create the earlier writer acknowledged during the migration, opened after it: %v", err)
		}
	} else if _, err := os.Stat(filepath.Join(root, "sessions", "midway")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused create left its directory: %v", err)
	}
	if rep, err := st.Verify(ctx); err != nil || !rep.OK() {
		t.Errorf("verify: %v %v", err, rep.Problems)
	}
}

// TestTombstoneOnOpen: a writing open puts the tombstone in place in a
// store that has its layout and no journal, as one an earlier release
// migrated has, and a read-only open does not.
func TestTombstoneOnOpen(t *testing.T) {
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	journal := filepath.Join(root, journalFile)
	if got, err := os.Readlink(journal); err != nil || got != tombstone {
		t.Fatalf("a new store's tombstone: %q %v", got, err)
	}
	os.Remove(journal)
	ro, err := Open(root, WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	ro.Close()
	if _, err := os.Lstat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a read-only open wrote the tombstone: %v", err)
	}
	st, err = Open(root)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if got, err := os.Readlink(journal); err != nil || got != tombstone {
		t.Errorf("the tombstone of a store migrated without one: %q %v", got, err)
	}
	// The earlier release's commit through it fails.
	if _, err := os.OpenFile(journal, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600); !errors.Is(err, syscall.ENOTDIR) {
		t.Errorf("a commit through the tombstone: %v", err)
	}
}
