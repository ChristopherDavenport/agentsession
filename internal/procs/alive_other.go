//go:build !unix

package procs

import "os"

// Alive reports whether a process with the given ID exists. On
// Windows FindProcess opens a handle and fails when there is none.
func Alive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	p.Release()
	return true
}
