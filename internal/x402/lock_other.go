//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package x402

import "os"

// tryLock is a no-op where no advisory lock is available; release builds
// target linux, darwin and windows only.
func tryLock(*os.File) error { return nil }
