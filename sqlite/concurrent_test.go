package sqlite_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
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
