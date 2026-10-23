module github.com/saanuregh/agent-cli-connector/examples

go 1.26

require github.com/saanuregh/agent-cli-connector/bindings/go v0.0.0

require (
	github.com/ebitengine/purego v0.11.2 // indirect
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-amd64 v0.4.1 // indirect
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-arm64 v0.4.1 // indirect
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-amd64 v0.4.1 // indirect
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-arm64 v0.4.1 // indirect
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/windows-amd64 v0.4.1 // indirect
)

replace github.com/saanuregh/agent-cli-connector/bindings/go => ../bindings/go

replace (
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-amd64 => ../bindings/go/lib/darwin-amd64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-arm64 => ../bindings/go/lib/darwin-arm64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-amd64 => ../bindings/go/lib/linux-amd64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-arm64 => ../bindings/go/lib/linux-arm64
	github.com/saanuregh/agent-cli-connector/bindings/go/lib/windows-amd64 => ../bindings/go/lib/windows-amd64
)
