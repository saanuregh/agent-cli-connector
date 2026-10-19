//go:build !unix

package providerkit

import "os"

// unixModes reports whether file modes control access, so that a mode of
// 0700 makes a directory private.
const unixModes = false

// fileOwner reports no owner where file ownership cannot be read.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
