//go:build unix

package procs

import (
	"errors"
	"os"
	"syscall"
)

// Alive reports whether a process with the given ID exists.
// Signal 0 performs the permission and existence checks without
// delivering anything; EPERM means the process exists but belongs to
// another user.
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
