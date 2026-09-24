module github.com/ChristopherDavenport/agentsession/sqlite

go 1.25.0

// Every module in the repository is released at one version, from one
// commit, and requires its first-party siblings at exactly that version.
// The replace below is what makes that possible: go mod tidy ignores
// go.work, so without it tidy would resolve the version being released
// from the proxy, where it does not exist until the tag is pushed.
// Consumers ignore a replace in a dependency, and get the require —
// which names the commit this module was built against, by construction.
require (
	github.com/ChristopherDavenport/agentsession v0.0.7
	github.com/ChristopherDavenport/openresponses v0.0.12
	modernc.org/sqlite v1.59.0
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.24 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/sys v0.47.0 // indirect
	modernc.org/libc v1.75.7 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.12.1 // indirect
)

replace github.com/ChristopherDavenport/agentsession => ../
