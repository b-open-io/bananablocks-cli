//go:build !windows

package cmd

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
)

// A replaced file keeps its permissions, and a new one honors the umask; the
// temp file is never world-readable while being written.
func TestPayloadFilePermissions(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)

	private := filepath.Join(dir, "private.beef")
	if err := os.WriteFile(private, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writePayloadTo(cmd, private, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(private); st.Mode().Perm() != 0o600 {
		t.Fatalf("replaced file mode %v, want 0600", st.Mode().Perm())
	}

	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	fresh := filepath.Join(dir, "fresh.beef")
	if err := writePayloadTo(cmd, fresh, []byte("x"), false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(fresh); st.Mode().Perm() != 0o600 {
		t.Fatalf("new file under umask 077 has mode %v, want 0600", st.Mode().Perm())
	}
}
