// Package origin picks a fork's origin from the sessions a store found
// holding its base, asking the caller's read check (agentsession's
// WithReadable) only once the store has let go of its own locks: the
// check is a program's authorizer, which may read the store.
package origin

// Candidates are the sessions that may be the origin of a fork at a
// base, gathered by a store under its lock, in the store's order.
type Candidates struct {
	// Chain is the session the fork's parent_session names, and those
	// up its parent_session line, each holding the base, as far as the
	// walk got: it stops at a session whose own entries include the base
	// or that names no parent, Ends then set, or at one the store does
	// not hold or that does not hold the base, Ends unset.
	Chain []string
	Ends  bool
	// Owners are the sessions whose own entries include the base, and
	// OnPrefix those that hold it on their prefix, each in ID order.
	Owners, OnPrefix []string
}

// Pick returns the origin among the sessions readable admits, as the
// store would find it holding none it does not: up the chain while each
// is admitted, to its end; else the first admitted owner; else the first
// admitted session holding the base on its prefix. It asks readable in
// that order, of each session once, and of none after the first it
// admits, and returns "" when none is admitted.
func (c Candidates) Pick(readable func(string) bool) string {
	asked := map[string]bool{}
	ok := func(id string) bool {
		v, seen := asked[id]
		if !seen {
			v = readable(id)
			asked[id] = v
		}
		return v
	}
	for i, id := range c.Chain {
		if !ok(id) {
			break
		}
		if i == len(c.Chain)-1 && c.Ends {
			return id
		}
	}
	for _, id := range c.Owners {
		if ok(id) {
			return id
		}
	}
	for _, id := range c.OnPrefix {
		if ok(id) {
			return id
		}
	}
	return ""
}
