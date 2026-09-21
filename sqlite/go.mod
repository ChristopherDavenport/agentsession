module github.com/ChristopherDavenport/agentsession/sqlite

go 1.25.0

// The root requirement names the released version a consumer fetches.
// The workspace builds this module against the tree instead; there is
// deliberately no replace, so release-check can build it the way a
// consumer does and fail while the version named here is too old.
require (
	github.com/ChristopherDavenport/agentsession v0.0.6
	github.com/ChristopherDavenport/openresponses v0.0.10
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
