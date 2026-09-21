package jsonl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/internal/procs"
)

// ErrSessionLocked is returned by Create, Open and Append when another
// process holds the session. The error's message names the holder;
// BreakLock removes a lock the caller has decided is stale.
//
// It is [agentsession.ErrSessionLocked], so a host that reads the
// store through the interface matches the same sentinel whichever
// store it was given.
var ErrSessionLocked = agentsession.ErrSessionLocked

// LockInfo describes the holder of a session lock.
type LockInfo struct {
	PID   int       `json:"pid"`
	Host  string    `json:"host,omitempty"`
	Since time.Time `json:"since"`
}

// lockPath is the advisory lock file beside a session file. The
// suffix keeps it out of the globs that find session files.
func lockPath(sessionPath string) string { return sessionPath + ".lock" }

// acquireLock takes the advisory lock for a session file. A lock left
// by a process on this host that no longer runs is taken over, and
// report, when not nil, is told whose it was; any other holder
// produces ErrSessionLocked.
func acquireLock(sessionPath string, report func(LockInfo)) error {
	path := lockPath(sessionPath)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			host, _ := os.Hostname()
			info := LockInfo{PID: os.Getpid(), Host: host, Since: time.Now().UTC().Round(0)}
			werr := json.NewEncoder(f).Encode(info)
			if cerr := f.Close(); werr == nil {
				werr = cerr
			}
			if werr != nil {
				os.Remove(path)
				return fmt.Errorf("jsonl: write lock %s: %w", path, werr)
			}
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("jsonl: create lock %s: %w", path, err)
		}
		holder, herr := readLock(path)
		if herr != nil {
			if errors.Is(herr, os.ErrNotExist) {
				continue // released between our attempt and the read
			}
			return fmt.Errorf("%w: %s (unreadable lock: %v)", ErrSessionLocked, path, herr)
		}
		if holder.stale() {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("jsonl: remove stale lock %s: %w", path, err)
			}
			if report != nil {
				report(holder)
			}
			continue
		}
		return fmt.Errorf("%w: %s held by pid %d on %s since %s", ErrSessionLocked, path, holder.PID, holder.Host, holder.Since.Format(time.RFC3339))
	}
	return fmt.Errorf("%w: %s", ErrSessionLocked, path)
}

// releaseLock removes the lock file. A missing file is not an error:
// the lock may already have been broken.
func releaseLock(sessionPath string) error {
	if err := os.Remove(lockPath(sessionPath)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("jsonl: remove lock: %w", err)
	}
	return nil
}

func readLock(path string) (LockInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return LockInfo{}, err
	}
	var info LockInfo
	if len(data) == 0 {
		// Created but not yet written by its holder.
		return info, errors.New("empty")
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return LockInfo{}, err
	}
	return info, nil
}

// stale reports whether the holder is a process on this host that no
// longer runs. A holder on another host is never stale: this process
// cannot tell, and the caller must break the lock deliberately.
func (l LockInfo) stale() bool {
	host, _ := os.Hostname()
	if l.Host != host || l.PID <= 0 {
		return false
	}
	return !procs.Alive(l.PID)
}

// LockHolder reports who holds a session's lock, or nil when it is
// free. It reads the lock file and does not consult this store's open
// sessions, so a session this store holds is reported as locked by
// this process.
func (s *Store) LockHolder(id string) (*LockInfo, error) {
	path, err := s.Path(id)
	if err != nil {
		return nil, err
	}
	info, err := readLock(lockPath(path))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("jsonl: read lock: %w", err)
	}
	return &info, nil
}

// BreakLock removes a session's lock whoever holds it. Use it when
// Open reports ErrSessionLocked and the caller has confirmed the holder
// is gone, for example a process on another host that crashed. Breaking
// the lock of a live writer lets two processes append to one file.
// A store opened with [WithReadOnly] refuses: it takes no lock, so it
// has no business dropping another process's.
func (s *Store) BreakLock(id string) error {
	if s.readOnly {
		return agentsession.ErrReadOnly
	}
	path, err := s.Path(id)
	if err != nil {
		return err
	}
	return releaseLock(path)
}
