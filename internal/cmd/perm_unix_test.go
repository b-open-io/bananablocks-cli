//go:build !windows

package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

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

// A FIFO with no reader does not hang the command: cancelling the context
// (Ctrl-C) ends the wait, and a stalled write is interrupted too.
func TestPayloadFIFOCancellable(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	cmd.SetContext(ctx)
	done := make(chan error, 1)
	go func() { done <- writePayloadTo(cmd, fifo, []byte("x"), false) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no-reader FIFO open did not respond to cancellation")
	}

	// A reader that opens but never drains: the write stalls once the pipe
	// buffer fills, and cancellation must still end it.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cmd.SetContext(ctx2)
	r, err := os.OpenFile(fifo, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	big := make([]byte, 4<<20)
	go func() { done <- writePayloadTo(cmd, fifo, big, true) }()
	time.Sleep(200 * time.Millisecond)
	cancel2()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stalled write: got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stalled FIFO write did not respond to cancellation")
	}
}

// A link that resolves to a pipe (as /dev/stdout does) is streamed through,
// not rejected or replaced.
func TestPayloadSymlinkToFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	link := filepath.Join(dir, "out")
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
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
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, link, []byte("hi"), true); err != nil {
		t.Fatal(err)
	}
	if b := <-got; string(b) != "hi" {
		t.Fatalf("reader got %q", b)
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the link was replaced")
	}
}

// The kernel decides where a link with a symlinked directory followed by
// ".." leads; bb must not reroute it by reading the path text.
func TestDestinationSymlinkDotDot(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"a", "b/subdir"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../b/subdir", filepath.Join(dir, "a", "hop")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/dev/stdout", filepath.Join(dir, "a", "result")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b", "result"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "a", "out")
	if err := os.Symlink("hop/../result", out); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, out, []byte("new"), true); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("payload rerouted to stdout: %q", stdout.String())
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "b", "result")); string(raw) != "new" {
		t.Fatalf("the file the kernel resolves to holds %q", raw)
	}
}

// fakeStd stands in two temp files for stdout and stderr, so destination
// identity can be tested whatever the test binary's own streams are.
func fakeStd(t *testing.T) (stdout, stderr *os.File) {
	t.Helper()
	dir := t.TempDir()
	var err error
	if stdout, err = os.OpenFile(filepath.Join(dir, "stdout"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		t.Fatal(err)
	}
	if stderr, err = os.OpenFile(filepath.Join(dir, "stderr"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err != nil {
		t.Fatal(err)
	}
	old := stdStreams
	stdStreams = func() (*os.File, *os.File) { return stdout, stderr }
	t.Cleanup(func() { stdStreams = old; stdout.Close(); stderr.Close() })
	return stdout, stderr
}

func fdPath(f *os.File) string { return "/dev/fd/" + strconv.Itoa(int(f.Fd())) }

// Anything that is stdout — a descriptor path, a symlink to one, or the very
// file stdout is redirected to — is written through stdout, not replaced.
func TestDestinationStdoutAliases(t *testing.T) {
	stdout, _ := fakeStd(t)
	link := filepath.Join(t.TempDir(), "sink")
	if err := os.Symlink(fdPath(stdout), link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fdPath(stdout), link, stdout.Name()} {
		var buf bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&buf)
		cmd.SetErr(io.Discard)
		if err := writePayloadTo(cmd, p, []byte("abc"), true); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if buf.String() != "abc" {
			t.Fatalf("%s: stdout got %q", p, buf.String())
		}
	}
	if st, _ := os.Lstat(link); st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the alias was replaced")
	}
}

// Anything that is stderr — a descriptor path, a duplicate of it (3>&2), a
// symlink, or the file stderr is redirected to — is refused before writing.
func TestDestinationStderrAliasesRefused(t *testing.T) {
	_, stderr := fakeStd(t)
	dup, err := syscall.Dup(int(stderr.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(dup)
	link := filepath.Join(t.TempDir(), "sink")
	if err := os.Symlink(fdPath(stderr), link); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{fdPath(stderr), "/dev/fd/" + strconv.Itoa(dup), link, stderr.Name()} {
		if _, err := resolveDestination(p); !errors.Is(err, errStderrDest) {
			t.Fatalf("%s: got %v", p, err)
		}
	}
	rs := newRecordingServer(t, nil)
	if _, _, err := runBB(t, rs.srv.URL, "tx", strings.Repeat("ab", 32), "--bin", "-o", stderr.Name()); !errors.Is(err, errStderrDest) {
		t.Fatalf("command: got %v", err)
	}
	if len(rs.requests()) != 0 {
		t.Fatal("a refused destination still fetched")
	}
}

// Another descriptor is written through itself, keeping its append mode,
// and the caller's descriptor stays open.
func TestDestinationOtherDescriptorAppends(t *testing.T) {
	fakeStd(t)
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "hist"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("old\n")
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, fdPath(f), []byte("new\n"), true); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	if _, err := f.WriteString("more\n"); err != nil {
		t.Fatalf("the original descriptor was closed: %v", err)
	}
	if raw, _ := os.ReadFile(f.Name()); string(raw) != "old\nnew\nmore\n" {
		t.Fatalf("file %q", raw)
	}
}

// A symlink to an inherited append descriptor (sink -> /dev/fd/3 with
// 3>>history) appends through it instead of replacing the file under it.
func TestDestinationAliasToInheritedDescriptor(t *testing.T) {
	fakeStd(t)
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "history"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("old\n")
	before, _ := f.Stat()
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	for _, target := range []string{fdPath(f), "/proc/self/fd/" + strconv.Itoa(int(f.Fd()))} {
		if _, err := os.Stat(target); err != nil {
			continue // no /proc on this OS
		}
		sink := filepath.Join(t.TempDir(), "sink")
		if err := os.Symlink(target, sink); err != nil {
			t.Fatal(err)
		}
		if err := writePayloadTo(cmd, sink, []byte("new\n"), true); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	after, err := os.Stat(f.Name())
	if err != nil || !os.SameFile(before, after) {
		t.Fatal("the file under the descriptor was replaced")
	}
	if raw, _ := os.ReadFile(f.Name()); !strings.HasPrefix(string(raw), "old\nnew\n") {
		t.Fatalf("file %q", raw)
	}
}

// An ordinary path is always replaced atomically, even when an inherited
// descriptor has the file open: the export never inherits stale bytes or an
// append offset it was not asked for.
func TestOrdinaryPathIgnoresInheritedDescriptors(t *testing.T) {
	fakeStd(t)
	path := filepath.Join(t.TempDir(), "tx.bin")
	if err := os.WriteFile(path, []byte("a much longer previous payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	rw, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer rw.Close()
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	if err := writePayloadTo(cmd, path, []byte("short"), true); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); string(raw) != "short" {
		t.Fatalf("file %q, want exactly the new payload", raw)
	}
}

// A default download name never follows a planted symlink onto another file.
func TestGeneratedNameDoesNotFollowSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(dir, "config")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "payload")
	})
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	txid := strings.Repeat("ab", 32)
	if err := os.Symlink(victim, txid+"_0.txt"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, "tok_0.txt"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runBB(t, rs.srv.URL, "media", txid+":0"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runBB(t, rs.srv.URL, "token", "tok_0", "--icon"); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(victim); string(raw) != "keep" {
		t.Fatalf("symlink target overwritten: %q", raw)
	}
	for _, n := range []string{txid + "_0.txt", "tok_0.txt"} {
		st, err := os.Lstat(n)
		if err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s should now be a regular file", n)
		}
	}
}

// A relative symlink chain to an inherited descriptor (sink -> fdlink ->
// /dev/fd/N) is traced and appends through the descriptor.
func TestDestinationRelativeChainToDescriptor(t *testing.T) {
	fakeStd(t)
	dir := t.TempDir()
	f, err := os.OpenFile(filepath.Join(dir, "history"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	f.WriteString("old\n")
	before, _ := f.Stat()
	targets := []string{fdPath(f)}
	if _, err := os.Stat("/proc/self/fd"); err == nil {
		targets = append(targets, "/proc/self/fd/"+strconv.Itoa(int(f.Fd())))
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	for i, target := range targets {
		sub := filepath.Join(dir, "links"+strconv.Itoa(i))
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(sub, "fdlink")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("fdlink", filepath.Join(sub, "sink")); err != nil {
			t.Fatal(err)
		}
		if err := writePayloadTo(cmd, filepath.Join(sub, "sink"), []byte("new\n"), true); err != nil {
			t.Fatalf("%s: %v", target, err)
		}
	}
	after, _ := os.Stat(f.Name())
	if !os.SameFile(before, after) {
		t.Fatal("the file under the descriptor was replaced")
	}
	if raw, _ := os.ReadFile(f.Name()); !strings.HasPrefix(string(raw), "old\nnew\n") {
		t.Fatalf("file %q", raw)
	}
}

// A symlink to a file this process holds open for writing, but which bb
// cannot trace to a descriptor, is refused rather than replaced.
func TestDestinationUntraceableAliasRefused(t *testing.T) {
	fakeStd(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	link := filepath.Join(dir, "link")
	if err := os.Symlink("history", link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveDestination(link); err == nil || !strings.Contains(err.Error(), "open on descriptor") {
		t.Fatalf("got %v", err)
	}
	// The plain path is still replaced normally.
	if d, err := resolveDestination(path); err != nil || d.kind != destFile {
		t.Fatalf("plain path: %+v, %v", d, err)
	}
}
