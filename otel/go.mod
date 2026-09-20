module github.com/ChristopherDavenport/agentsession/otel

go 1.25.0

require (
	github.com/ChristopherDavenport/agentsession v0.0.4
	github.com/ChristopherDavenport/openresponses v0.0.9
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

// The require names the released root a consumer fetches; the replace
// builds against the tree.
replace github.com/ChristopherDavenport/agentsession => ../
