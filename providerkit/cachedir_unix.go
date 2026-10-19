//go:build unix

package providerkit

import (
	"os"
	"syscall"
)

// unixModes reports whether file modes control access, so that a mode of
// 0700 makes a directory private.
const unixModes = true

// fileOwner returns the user ID owning info's file.
func fileOwner(info os.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}
