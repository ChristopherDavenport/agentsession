//go:build !unix

package jsonl

// guarded on a platform without flock runs f unguarded: a taker that
// finds a stale lock can still remove a fresh one that another took
// over it, as before. The unix build keeps takers in turn.
func guarded(lockFile string, f func() error) error { return f() }
