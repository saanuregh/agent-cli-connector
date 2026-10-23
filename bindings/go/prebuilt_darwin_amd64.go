//go:build cgo && !agent_use_lib && darwin && amd64

package agent

import _ "github.com/saanuregh/agent-cli-connector/bindings/go/lib/darwin-amd64"
