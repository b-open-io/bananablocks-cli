//go:build windows

package x402

import (
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes a non-blocking exclusive lock on f, released when the
// handle is closed or the process exits.
func tryLock(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
}
