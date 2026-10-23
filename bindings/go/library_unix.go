//go:build (darwin || freebsd || linux || netbsd) && !(cgo && !agent_use_lib && ((linux && (amd64 || arm64)) || (darwin && (amd64 || arm64)) || (windows && amd64)))

package agent

import "github.com/ebitengine/purego"

func openLibrary(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_LOCAL)
}
