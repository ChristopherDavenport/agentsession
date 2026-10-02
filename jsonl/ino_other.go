//go:build !unix

package jsonl

import "os"

// inode is 0 where the platform has no inode number to read: a
// follower there tells a replaced file by its header and its size.
func inode(os.FileInfo) uint64 { return 0 }
