//go:build !unix

package cas

// dirIdentity reports false where no identity is read: no object
// directory is known, and none is pruned. A directory is not fsynced
// here, so nothing is paid for it.
func dirIdentity(string) (dirID, bool) { return dirID{}, false }
