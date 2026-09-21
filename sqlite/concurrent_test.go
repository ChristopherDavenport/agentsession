package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/sqlite"
	"github.com/ChristopherDavenport/openresponses"
)

// TestSessionHold: a session open in one store is refused to another
// over the same file until it is released, as two processes would see.
func TestSessionHold(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.Create(ctx, agentsession.Header{ID: "shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ctx, "shared"); !errors.Is(err, sqlite.ErrSessionLocked) {
		t.Fatalf("b.Open while a holds = %v, want ErrSessionLocked", err)
	}
	if _, err := b.Append(ctx, "shared", &agentsession.InfoEntry{Name: "n"}); !errors.Is(err, sqlite.ErrSessionLocked) {
		t.Errorf("b.Append while a holds = %v, want ErrSessionLocked", err)
	}
	holder, err := a.LockHolder(ctx, "shared")
	if err != nil || holder == nil || holder.PID != os.Getpid() {
		t.Errorf("LockHolder = %+v, %v", holder, err)
	}
	// Reopening in the holding store is fine.
	if _, err := a.Open(ctx, "shared"); err != nil {
		t.Errorf("a.Open of its own session: %v", err)
	}
	a.Release("shared")
	if holder, _ := a.LockHolder(ctx, "shared"); holder != nil {
		t.Errorf("holder after Release = %+v", holder)
	}
	if _, err := b.Open(ctx, "shared"); err != nil {
		t.Fatalf("b.Open after release: %v", err)
	}
	if _, err := b.Append(ctx, "shared", &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Errorf("b.Append after release: %v", err)
	}
}

// TestHoldLost: an operator breaks a live hold and a second process
// takes the session; the first process's next append fails loudly and
// nothing is re-parented, as the format's one-writer rule needs.
func TestHoldLost(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, _ := sqlite.Open(path)
	defer a.Close()
	b, _ := sqlite.Open(path)
	defer b.Close()
	if _, err := a.Create(ctx, agentsession.Header{ID: "shared"}); err != nil {
		t.Fatal(err)
	}
	if err := b.BreakLock(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Open(ctx, "shared"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Append(ctx, "shared", agentsession.NewItemEntry(openresponses.UserText("from b"))); err != nil {
		t.Fatal(err)
	}
	_, err := a.Append(ctx, "shared", agentsession.NewItemEntry(openresponses.UserText("from a")))
	if !errors.Is(err, sqlite.ErrSessionLocked) {
		t.Fatalf("a.Append after losing the hold = %v, want ErrSessionLocked", err)
	}
	if _, err := a.Append(ctx, "shared", agentsession.NewItemEntry(openresponses.UserText("again"))); !errors.Is(err, sqlite.ErrSessionLocked) {
		t.Errorf("second a.Append = %v, want still refused", err)
	}
	b.Release("shared")
	// After an explicit reload a sees b's entry and continues.
	a.Release("shared")
	reloaded, err := a.Open(ctx, "shared")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Len() != 1 {
		t.Fatalf("reloaded has %d entries, want 1", reloaded.Len())
	}
	if _, err := a.Append(ctx, "shared", agentsession.NewItemEntry(openresponses.UserText("after reload"))); err != nil {
		t.Errorf("append after reload: %v", err)
	}
}

// TestStaleHold leaves a holders row naming a process that cannot
// exist on this host and expects Open to take it over and say so.
func TestStaleHold(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, _ := sqlite.Open(path)
	if _, err := a.Create(ctx, agentsession.Header{ID: "s"}); err != nil {
		t.Fatal(err)
	}
	a.Close()
	host, _ := os.Hostname()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	since := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO holders (session_id, pid, host, token, since, heartbeat) VALUES (?, ?, ?, ?, ?, ?)`, "s", 2147483000, host, "dead", since, since); err != nil {
		t.Fatal(err)
	}
	db.Close()
	var reported []sqlite.LockInfo
	b, err := sqlite.Open(path, sqlite.WithStaleLockReport(func(l sqlite.LockInfo) { reported = append(reported, l) }))
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := b.Open(ctx, "s"); err != nil {
		t.Fatalf("Open over a stale hold: %v", err)
	}
	if len(reported) != 1 || reported[0].PID != 2147483000 || reported[0].Since.Format(time.RFC3339Nano) != since {
		t.Errorf("reported = %+v", reported)
	}
	holder, _ := b.LockHolder(ctx, "s")
	if holder == nil || holder.PID != os.Getpid() {
		t.Errorf("holder after takeover = %+v", holder)
	}
}

// TestReadOnlyStore: a read-only store reads a session another store
// holds, takes no hold of its own and refuses every write. It is what
// an operator verifying a session while the daemon runs gets.
func TestReadOnlyStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	writer, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Create(ctx, agentsession.Header{ID: "held"}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Append(ctx, "held", &agentsession.InfoEntry{Name: "live"}); err != nil {
		t.Fatal(err)
	}

	reader, err := sqlite.Open(path, sqlite.WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	sess, err := reader.Open(ctx, "held")
	if err != nil {
		t.Fatalf("read-only Open of a held session = %v", err)
	}
	if sess.Name() != "live" {
		t.Errorf("name = %q, want the one the holder wrote", sess.Name())
	}
	holder, err := reader.LockHolder(ctx, "held")
	if err != nil || holder == nil || holder.PID != os.Getpid() {
		t.Errorf("the hold moved: %+v, %v", holder, err)
	}
	writes := map[string]error{
		"Create": func() error { _, err := reader.Create(ctx, agentsession.Header{ID: "new"}); return err }(),
		"Append": func() error {
			_, err := reader.Append(ctx, "held", &agentsession.InfoEntry{Name: "no"})
			return err
		}(),
		"Delete": reader.Delete(ctx, "held"),
	}
	for name, err := range writes {
		if !errors.Is(err, agentsession.ErrReadOnly) {
			t.Errorf("read-only %s = %v, want ErrReadOnly", name, err)
		}
	}
	// The holder keeps appending, and a reopened read-only session
	// sees it.
	if _, err := writer.Append(ctx, "held", &agentsession.InfoEntry{Name: "still writing"}); err != nil {
		t.Errorf("the holder's append after a read-only open: %v", err)
	}
	reader.Release("held")
	again, err := reader.Open(ctx, "held")
	if err != nil {
		t.Fatal(err)
	}
	if again.Name() != "still writing" {
		t.Errorf("name after reopen = %q", again.Name())
	}
	// Releasing a read-only session left the holder's row alone.
	if holder, err := writer.LockHolder(ctx, "held"); err != nil || holder == nil {
		t.Errorf("hold after a read-only Release = %+v, %v", holder, err)
	}
}

// TestHolderTimestamps: the message a refused caller sees carries the
// times the holder has held the session since and last appended, not
// the zero time.
func TestHolderTimestamps(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	a, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if _, err := a.Create(ctx, agentsession.Header{ID: "shared"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Append(ctx, "shared", &agentsession.InfoEntry{Name: "n"}); err != nil {
		t.Fatal(err)
	}
	_, err = b.Open(ctx, "shared")
	if !errors.Is(err, agentsession.ErrSessionLocked) {
		t.Fatalf("Open of a held session = %v", err)
	}
	if strings.Contains(err.Error(), "0001-01-01") {
		t.Errorf("the refusal reports a zero timestamp: %v", err)
	}
	year := time.Now().UTC().Format("2006")
	if !strings.Contains(err.Error(), "since "+year) {
		t.Errorf("the refusal does not say since when: %v", err)
	}
	holder, err := b.LockHolder(ctx, "shared")
	if err != nil || holder == nil {
		t.Fatalf("LockHolder = %+v, %v", holder, err)
	}
	if holder.Since.IsZero() || holder.Heartbeat.IsZero() {
		t.Errorf("LockInfo = %+v, want both times", holder)
	}
	if holder.Heartbeat.Before(holder.Since) {
		t.Errorf("heartbeat %s is before since %s", holder.Heartbeat, holder.Since)
	}
}
