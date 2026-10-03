package sqlite_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/sqlite"
)

// TestRefTablesOnAnOldDatabase: a database an earlier release wrote has
// no ref tables, and gains them at its first open, with its sessions
// as they were.
func TestRefTablesOnAnOldDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.ID()
	st.Close()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`DROP TABLE ref_log`, `DROP TABLE refs`} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err = sqlite.Open(path)
	if err != nil {
		t.Fatalf("opening a database with no ref tables: %v", err)
	}
	defer st.Close()
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: id}, "after migration"); err != nil {
		t.Fatal(err)
	}
	if got, err := st.ResolveRef(ctx, "r"); err != nil || got.Session != id {
		t.Errorf("ResolveRef = %v, %v; want %s", got, err, id)
	}
}

// TestRefTableOfAnotherProgram: a refs table that is not ours is
// refused with nothing written, as the store's other tables are.
func TestRefTableOfAnotherProgram(t *testing.T) {
	path := filepath.Join(t.TempDir(), "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE refs (branch TEXT, sha TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if st, err := sqlite.Open(path); err == nil {
		st.Close()
		t.Fatal("Open accepted a database whose refs table is another program's")
	} else if !strings.Contains(err.Error(), "another program") {
		t.Errorf("Open: %v, want the refusal for another program's table", err)
	}
}

// TestRefDeleteSessionKeepsRef: deleting a session leaves its refs,
// and the foreign keys of the other tables do not reach them.
func TestRefDeleteSessionKeepsRef(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := st.Create(ctx, agentsession.Header{})
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID()}, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Delete(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	var n int
	for _, err := range st.ListRefs(ctx, "") {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 1 {
		t.Errorf("%d refs after deleting the session, want 1", n)
	}
}

const refRaceEnv = "SQLITE_REFS_RACE_PATH"

// TestRefsRaceHelper is the body of one process of TestRefsProcessRace.
func TestRefsRaceHelper(t *testing.T) {
	path := os.Getenv(refRaceEnv)
	if path == "" {
		t.Skip("helper process of TestRefsProcessRace")
	}
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for {
		if _, err := os.Stat(path + ".go"); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	s, err := agentsession.SessionFor(context.Background(), st, "race/name", agentsession.Header{})
	switch {
	case err == nil:
		fmt.Printf("RESULT session %s\n", s.ID())
		time.Sleep(200 * time.Millisecond)
	case errors.Is(err, agentsession.ErrSessionLocked):
		fmt.Println("RESULT locked")
	default:
		fmt.Printf("RESULT error %v\n", err)
	}
}

// TestRefsProcessRace runs SessionFor for one name in several processes
// on one database at once: one session, one ref, and every process that
// got a session got that one.
func TestRefsProcessRace(t *testing.T) {
	if testing.Short() {
		t.Skip("starts processes")
	}
	ctx := context.Background()
	const procs = 8
	for round := range 3 {
		path := filepath.Join(t.TempDir(), "sessions.db")
		st, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
		var cmds []*exec.Cmd
		var outs []*bytes.Buffer
		for range procs {
			cmd := exec.Command(os.Args[0], "-test.run=^TestRefsRaceHelper$", "-test.v")
			cmd.Env = append(os.Environ(), refRaceEnv+"="+path)
			out := &bytes.Buffer{}
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			cmds, outs = append(cmds, cmd), append(outs, out)
		}
		time.Sleep(300 * time.Millisecond)
		if err := os.WriteFile(path+".go", nil, 0o644); err != nil {
			t.Fatal(err)
		}
		for i, cmd := range cmds {
			if err := cmd.Wait(); err != nil {
				t.Fatalf("round %d: process %d: %v\n%s", round, i, err, outs[i])
			}
		}
		ids := map[string]int{}
		for i, out := range outs {
			var res string
			for line := range strings.SplitSeq(out.String(), "\n") {
				if rest, ok := strings.CutPrefix(line, "RESULT "); ok {
					res = rest
				}
			}
			switch {
			case strings.HasPrefix(res, "session "):
				ids[strings.TrimPrefix(res, "session ")]++
			case res == "locked":
			default:
				t.Errorf("round %d: process %d: %q\n%s", round, i, res, out)
			}
		}
		check, err := sqlite.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ref, err := check.ResolveRef(ctx, "race/name")
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if len(ids) > 1 || (len(ids) == 1 && ids[ref.Session] == 0) {
			t.Errorf("round %d: processes got sessions %v, the ref names %s", round, ids, ref.Session)
		}
		n := 0
		for sum, err := range check.List(ctx, agentsession.ListFilter{}) {
			if err != nil {
				t.Fatal(err)
			}
			n++
			if sum.Header.ID != ref.Session {
				t.Errorf("round %d: session %s left behind by a loser", round, sum.Header.ID)
			}
		}
		if n != 1 {
			t.Errorf("round %d: %d sessions, want 1", round, n)
		}
		check.Close()
	}
}

// TestRefLogRecordsIdentity: ref_log carries the identity of the session
// each target named, which the store compares and callers never see.
func TestRefLogRecordsIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s, _ := st.Create(ctx, agentsession.Header{})
	if err := st.UpdateRef(ctx, "r", agentsession.RefTarget{}, agentsession.RefTarget{Session: s.ID()}, ""); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ident string
	if err := db.QueryRow(`SELECT new_ident FROM ref_log WHERE name = 'r'`).Scan(&ident); err != nil || ident == "" {
		t.Errorf("ref_log new_ident = %q, %v", ident, err)
	}
}
