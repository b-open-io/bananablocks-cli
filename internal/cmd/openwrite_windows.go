//go:build windows

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
)

// openForWriteCtx opens an existing file for writing.
func openForWriteCtx(_ context.Context, path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}

func newCtxWriter(_ context.Context, f *os.File) io.Writer { return f }

// dupFile: Windows has no /dev/fd paths to name a descriptor by.
func dupFile(fd int, name string) (*os.File, error) {
	return nil, fmt.Errorf("descriptor paths are not supported on Windows")
}

// writableFDFor: Windows has no descriptor paths to alias.
func writableFDFor(os.FileInfo) (int, bool) { return 0, false }
