//go:build cgo && !agent_use_lib && linux && amd64

package agent

import _ "github.com/saanuregh/agent-cli-connector/bindings/go/lib/linux-amd64"
