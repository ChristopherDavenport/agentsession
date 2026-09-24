module github.com/ChristopherDavenport/agentsession/otel

go 1.25.0

// Every module in the repository is released at one version, from one
// commit, and requires its first-party siblings at exactly that version.
// The replace below is what makes that possible: go mod tidy ignores
// go.work, so without it tidy would resolve the version being released
// from the proxy, where it does not exist until the tag is pushed.
// Consumers ignore a replace in a dependency, and get the require —
// which names the commit this module was built against, by construction.
require (
	github.com/ChristopherDavenport/agentsession v0.0.6
	github.com/ChristopherDavenport/openresponses v0.0.10
	go.opentelemetry.io/otel v1.46.0
	go.opentelemetry.io/otel/sdk v1.46.0
	go.opentelemetry.io/otel/trace v1.46.0
)

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/metric v1.46.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

replace github.com/ChristopherDavenport/agentsession => ../
