//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package x402

import "os"

// lockFile is a no-op where no advisory lock is available; release builds
// target linux, darwin and windows only.
func lockFile(*os.File, bool) error { return nil }
