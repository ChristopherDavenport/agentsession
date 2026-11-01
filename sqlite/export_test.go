package sqlite

// SetLoadHook sets the hook told of each session the store loads, and
// returns a function that clears it.
func SetLoadHook(f func(id string)) func() {
	loadHook = f
	return func() { loadHook = nil }
}
