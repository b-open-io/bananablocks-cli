//go:build !windows

package cmd

import (
	"os"
	"syscall"
)

// currentUmask reads the process umask. syscall.Umask can only be read by
// setting it, so it is restored at once; the CLI does not create files
// concurrently with this call.
func currentUmask() os.FileMode {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return os.FileMode(m)
}
