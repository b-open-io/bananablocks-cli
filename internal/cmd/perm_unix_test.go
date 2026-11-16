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

// A symlink destination keeps the link and replaces its target; a FIFO is
// written in place, not replaced by a regular file.
func TestPayloadSymlinkAndFIFO(t *testing.T) {
	dir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)

	target := filepath.Join(dir, "real.beef")
	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.beef")
	if err := os.Symlink("real.beef", link); err != nil {
		t.Fatal(err)
	}
	if err := writePayloadTo(cmd, link, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	if raw, _ := os.ReadFile(target); string(raw) != "new" {
		t.Fatalf("link target holds %q", raw)
	}

	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	got := make(chan []byte, 1)
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			got <- nil
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		got <- b
	}()
	if err := writePayloadTo(cmd, fifo, []byte("piped"), false); err != nil {
		t.Fatal(err)
	}
	if b := <-got; string(b) != "piped" {
		t.Fatalf("reader got %q", b)
	}
	if st, _ := os.Lstat(fifo); st.Mode()&os.ModeNamedPipe == 0 {
		t.Fatal("the FIFO was replaced")
	}
}

func TestPayloadSymlinkLoop(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, a, []byte("x"), false); err == nil {
		t.Fatal("a symlink loop must error")
	}
}

// A dangling symlink is refused rather than resolved by guesswork, so the
// write can never land on an unrelated file.
func TestPayloadDanglingSymlinkRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "b", "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	unrelated := filepath.Join(dir, "a", "result")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../b/subdir", filepath.Join(dir, "a", "hop")); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "a", "out")
	if err := os.Symlink("hop/../result", out); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, out, []byte("x"), false); err == nil {
		t.Fatal("a dangling symlink must be refused")
	}
	if raw, _ := os.ReadFile(unrelated); string(raw) != "keep" {
		t.Fatalf("unrelated file overwritten: %q", raw)
	}
}
