module github.com/saanuregh/agent-cli-connector/bindings/go

go 1.26

require (
	github.com/ebitengine/purego v0.11.2
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-amd64 v0.4.0
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-arm64 v0.4.0
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-amd64 v0.4.0
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-arm64 v0.4.0
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/windows-amd64 v0.4.0
)

// The platform modules live in this repository. Replace directives apply
// only here; users get the tagged bindings/go/lib/<platform> versions.
replace (
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-amd64 => ./lib/darwin-amd64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-arm64 => ./lib/darwin-arm64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-amd64 => ./lib/linux-amd64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-arm64 => ./lib/linux-arm64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/windows-amd64 => ./lib/windows-amd64
)
