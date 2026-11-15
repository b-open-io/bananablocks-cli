//go:build windows

package x402

import "golang.org/x/sys/windows"

// syncDir is a no-op on Windows, where a directory cannot be opened for
// flushing; renameDurable carries the durability instead.
func syncDir(string) error { return nil }

// renameDurable renames with MOVEFILE_WRITE_THROUGH, which does not return
// until the move has been flushed to disk.
func renameDurable(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
