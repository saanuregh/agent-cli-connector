//go:build linux && amd64

// Package lib links the agent-cli-connector static library for linux/amd64.
// The agent package imports it; it has no API of its own.
package lib

// #cgo LDFLAGS: -L${SRCDIR} -lagent-cli-connector -lpthread
import "C"
