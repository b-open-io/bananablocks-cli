//go:build windows

package cmd

import (
	"context"
	"io"
	"os"
)

// openForWriteCtx opens an existing file for writing.
func openForWriteCtx(_ context.Context, path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY, 0)
}

func newCtxWriter(_ context.Context, f *os.File) io.Writer { return f }
