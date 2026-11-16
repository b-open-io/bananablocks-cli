//go:build windows

package cmd

import "os"

// currentUmask: Windows has no umask; file modes only toggle read-only.
func currentUmask() os.FileMode { return 0 }
