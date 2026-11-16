package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// Output destinations for -o.
//
// Where a destination leads is decided by the kernel, never by walking path
// text: os.Stat resolves symlinks (including descriptor magic links such as
// /dev/stdout) exactly as an open would, and os.SameFile compares the result
// with what stdout and stderr are. So:
//
//   - "-" or anything that is the same file as stdout is written to stdout,
//     keeping a `>>` redirect's offset and append mode;
//   - anything that is the same file as stderr (and not stdout) is refused,
//     since bb's own messages go there and would corrupt the payload;
//   - a path naming another of this process's descriptors (/dev/fd/N,
//     /proc/self/fd/N), or any path to a file one of them has open for
//     writing, is written through that descriptor;
//   - any other existing non-regular file (a FIFO, a device) is written in
//     place, since replacing it would cut off whatever reads from it;
//   - a regular file, or a new path, is replaced atomically through a temp
//     file in the directory it really lives in. A symlink to one has its
//     target replaced and stays a link; a dangling symlink is refused.

// destKind classifies a destination.
type destKind int

const (
	destStdout destKind = iota
	destFD
	destInPlace
	destFile
)

type destination struct {
	kind destKind
	fd   int    // destFD
	path string // destFile: the real file to replace; destInPlace: path to open
}

var errStderrDest = errors.New("leads to stderr, where bb also writes its messages; use -o - (stdout) or a file")

// stdStreams returns the files stdout and stderr are; a var so tests can
// stand in files they control.
var stdStreams = func() (stdout, stderr *os.File) { return os.Stdout, os.Stderr }

// resolveDestination classifies out. It runs before a command prints
// anything (see the root PersistentPreRunE), so a refused destination never
// receives a byte.
func resolveDestination(out string) (destination, error) {
	if out == "-" {
		return destination{kind: destStdout}, nil
	}
	// A path naming a descriptor, directly or through symlinks resolved
	// component by component (macOS /dev/stdout -> /dev/fd/1), is judged by
	// that descriptor: fstat of a pipe matches, where stat of the path may not.
	fd, isFD := descriptorPath(out)
	real, evalErr := filepath.EvalSymlinks(out)
	if !isFD && evalErr == nil {
		fd, isFD = descriptorPath(real)
	}
	var fst os.FileInfo
	if isFD {
		f, err := dupFile(fd, out)
		if err != nil {
			return destination{}, fmt.Errorf("-o %s: %w", out, err)
		}
		fst, err = f.Stat()
		_ = f.Close()
		if err != nil {
			return destination{}, fmt.Errorf("-o %s: %w", out, err)
		}
	} else {
		var err error
		fst, err = os.Stat(out)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return destination{}, err
			}
			if lst, lerr := os.Lstat(out); lerr == nil && lst.Mode()&os.ModeSymlink != 0 {
				return destination{}, fmt.Errorf("-o %s is a symbolic link that does not resolve; pass the real destination path", out)
			}
			return destination{kind: destFile, path: out}, nil
		}
	}
	if fst.IsDir() {
		return destination{}, fmt.Errorf("-o %s is a directory", out)
	}
	stdout, stderr := stdStreams()
	isStdout := sameAs(fst, stdout)
	if sameAs(fst, stderr) && !isStdout {
		return destination{}, fmt.Errorf("-o %s %w", out, errStderrDest)
	}
	if isStdout {
		return destination{kind: destStdout}, nil
	}
	if isFD {
		return destination{kind: destFD, fd: fd}, nil
	}
	if !fst.Mode().IsRegular() {
		return destination{kind: destInPlace, path: out}, nil
	}
	// A file this process already has open for writing (an inherited
	// `3>>file`, reached through any alias) is written through that
	// descriptor, keeping its offset and append mode.
	if wfd, ok := writableFDFor(fst); ok {
		return destination{kind: destFD, fd: wfd}, nil
	}
	if evalErr != nil {
		return destination{}, fmt.Errorf("-o %s: %w", out, evalErr)
	}
	// Confirm the resolved path is the very file the kernel opens before
	// replacing anything.
	rst, err := os.Stat(real)
	if err != nil || !os.SameFile(rst, fst) {
		return destination{}, fmt.Errorf("-o %s: cannot determine which file it names; pass the real destination path", out)
	}
	return destination{kind: destFile, path: real}, nil
}

func sameAs(fi os.FileInfo, f *os.File) bool {
	if f == nil {
		return false
	}
	st, err := f.Stat()
	return err == nil && os.SameFile(fi, st)
}

// descriptorPath reports the fd a path names: /dev/stdout (1), /dev/stderr
// (2), /dev/fd/N and /proc/self/fd/N.
func descriptorPath(out string) (int, bool) {
	clean := filepath.Clean(out)
	switch clean {
	case "/dev/stdout":
		return 1, true
	case "/dev/stderr":
		return 2, true
	}
	var rest string
	switch {
	case strings.HasPrefix(clean, "/dev/fd/"):
		rest = strings.TrimPrefix(clean, "/dev/fd/")
	case strings.HasPrefix(clean, "/proc/self/fd/"):
		rest = strings.TrimPrefix(clean, "/proc/self/fd/")
	default:
		return 0, false
	}
	fd, err := strconv.Atoi(rest)
	if err != nil || fd < 0 {
		return 0, false
	}
	return fd, true
}

// checkOutputDestination validates an -o value before the command runs.
func checkOutputDestination(out string) error {
	if out == "" {
		return nil
	}
	_, err := resolveDestination(out)
	return err
}

// saveGenerated saves a download under a name bb made up (no -o). Unlike an
// explicit destination it never follows what is already there: an existing
// entry of that name, symlink or not, is replaced as a directory entry, so a
// planted link cannot redirect the download onto another file.
func saveGenerated(cmd *cobra.Command, body io.Reader, name, contentType string) error {
	mode := 0o666 &^ currentUmask()
	if st, err := os.Lstat(name); err == nil {
		if st.IsDir() {
			return fmt.Errorf("%s is a directory; pass -o", name)
		}
		if st.Mode().IsRegular() {
			mode = st.Mode().Perm()
		}
	}
	n, err := replaceFile(body, name, mode)
	if err != nil {
		return err
	}
	desc := ""
	if contentType != "" {
		desc = " (" + contentType + ")"
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes%s to %s\n", n, desc, name); err != nil {
		return fmt.Errorf("reporting the write: %w", err)
	}
	return nil
}

// saveStream writes body to out as resolveDestination directs and reports
// the byte count on stderr.
func saveStream(cmd *cobra.Command, body io.Reader, out, contentType string) error {
	d, err := resolveDestination(out)
	if err != nil {
		return err
	}
	var n int64
	switch d.kind {
	case destStdout:
		n, err = io.Copy(cmd.OutOrStdout(), body)
	case destFD:
		var f *os.File
		if f, err = dupFile(d.fd, out); err == nil {
			n, err = io.Copy(f, body)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}
	case destInPlace:
		n, err = writeInPlace(cmd, body, d.path)
	case destFile:
		n, err = replaceFile(body, d.path, outputMode(d.path))
	}
	if err != nil {
		return err
	}
	desc := ""
	if contentType != "" {
		desc = " (" + contentType + ")"
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes%s to %s\n", n, desc, out); err != nil {
		return fmt.Errorf("reporting the write: %w", err)
	}
	return nil
}

// replaceFile copies body into a 0600 temp file next to path and renames it
// over path only once the copy fully succeeds, so a failed download never
// truncates an existing file. The finished file gets mode.
func replaceFile(body io.Reader, path string, mode os.FileMode) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".bb-download-*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	n, err := io.Copy(f, body)
	if err == nil {
		err = f.Chmod(mode)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// writeInPlace streams body into an existing non-regular file (a FIFO, a
// device) without replacing it. Opening and writing honor Ctrl-C: a FIFO
// with no reader, or one whose reader stopped draining, cannot hang bb.
func writeInPlace(cmd *cobra.Command, body io.Reader, path string) (int64, error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := openForWriteCtx(ctx, path)
	if err != nil {
		return 0, err
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = f.Close()
		case <-stop:
		}
	}()
	n, err := io.Copy(newCtxWriter(ctx, f), body)
	if cerr := f.Close(); err == nil && !errors.Is(cerr, os.ErrClosed) {
		err = cerr
	}
	if ctx.Err() != nil {
		return n, fmt.Errorf("writing %s: %w", path, ctx.Err())
	}
	return n, err
}

// outputMode is the mode a finished download gets: an existing destination
// keeps its permissions (a private 0600 file stays private), and a new one
// gets 0666 minus the process umask, as os.Create would give it.
func outputMode(out string) os.FileMode {
	if st, err := os.Stat(out); err == nil && st.Mode().IsRegular() {
		return st.Mode().Perm()
	}
	return 0o666 &^ currentUmask()
}

// copyOut streams body to the command's stdout.
func copyOut(cmd *cobra.Command, body io.Reader) (int64, error) {
	return io.Copy(cmd.OutOrStdout(), body)
}
