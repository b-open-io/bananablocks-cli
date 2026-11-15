//go:build windows

package x402

// syncDir is a no-op on Windows, where directories cannot be opened for
// flushing; NTFS journals the metadata of a completed rename.
func syncDir(string) error { return nil }
