//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package x402

import (
	"os"

	"golang.org/x/sys/unix"
)

// lockFile takes an exclusive advisory lock on f, waiting for it unless
// nonBlocking. The kernel releases it when f is closed or the process exits,
// so a crashed run never leaves it held.
func lockFile(f *os.File, nonBlocking bool) error {
	how := unix.LOCK_EX
	if nonBlocking {
		how |= unix.LOCK_NB
	}
	return unix.Flock(int(f.Fd()), how)
}
