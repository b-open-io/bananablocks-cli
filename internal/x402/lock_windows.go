//go:build windows

package x402

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on f, waiting for it unless nonBlocking.
// It is released when the handle is closed or the process exits.
func lockFile(f *os.File, nonBlocking bool) error {
	flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK)
	if nonBlocking {
		flags |= windows.LOCKFILE_FAIL_IMMEDIATELY
	}
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, &ol)
}
