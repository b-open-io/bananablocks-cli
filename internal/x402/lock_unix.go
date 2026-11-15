//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package x402

import (
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes a non-blocking exclusive advisory lock on f. The kernel
// releases it when the process exits, so a crashed run never leaves it held.
func tryLock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}
