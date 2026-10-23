//go:build cgo && !agent_use_lib && windows && amd64

package agent

import _ "github.com/saanuregh/agent-cli-connector/bindings/go/lib/windows-amd64"
