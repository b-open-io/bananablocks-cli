//go:build !windows

package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// openForWriteCtx opens an existing non-regular file for writing without
// blocking: opening a FIFO with no reader would otherwise block in the
// kernel, out of reach of Ctrl-C. It waits for a reader in short polls until
// ctx is cancelled. The fd stays non-blocking; writes go through
// newCtxWriter, which waits out EAGAIN while watching ctx.
func openForWriteCtx(ctx context.Context, path string) (*os.File, error) {
	for {
		f, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.ENXIO) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// ctxWriter writes to a non-blocking file, retrying on EAGAIN (a full pipe)
// until the data is written or ctx is cancelled. Where Go's poller does take
// the fd, Write blocks there instead, and writeInPlace closes the file on
// cancellation to end it.
type ctxWriter struct {
	ctx context.Context
	f   *os.File
}

func newCtxWriter(ctx context.Context, f *os.File) io.Writer { return &ctxWriter{ctx: ctx, f: f} }

func (w *ctxWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n, err := w.f.Write(p)
		total += n
		p = p[n:]
		if err == nil {
			continue
		}
		if !errors.Is(err, syscall.EAGAIN) {
			return total, err
		}
		select {
		case <-w.ctx.Done():
			return total, w.ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return total, nil
}

// dupFile returns a new *os.File on a duplicate of fd, so closing it (or its
// GC finalizer) never closes fd itself.
func dupFile(fd int, name string) (*os.File, error) {
	nfd, err := syscall.Dup(fd)
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(nfd)
	return os.NewFile(uintptr(nfd), name), nil
}
