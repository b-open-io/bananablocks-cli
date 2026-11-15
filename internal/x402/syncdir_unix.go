//go:build !windows

package x402

import "os"

// syncDir flushes a directory's entries (new or renamed names) to disk.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	serr := d.Sync()
	if cerr := d.Close(); serr == nil {
		serr = cerr
	}
	return serr
}

// renameDurable renames; the caller then syncs the directory.
func renameDurable(from, to string) error { return os.Rename(from, to) }
