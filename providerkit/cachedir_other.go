//go:build !unix

package providerkit

import "os"

// fileOwner reports no owner where file ownership cannot be read.
func fileOwner(os.FileInfo) (int, bool) { return 0, false }
