package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	mediaKind    string
	mediaOutFile string
	mediaStdout  bool
)

var mediaKinds = map[string]string{
	"media":       "media",
	"inscription": "inscription",
	"nft":         "nft",
	"bfile":       "bfile",
}

var mediaCmd = &cobra.Command{
	Use:   "media <txid>:<vout> | <txid> --kind bfile",
	Short: "Download on-chain content (inscriptions, NFT renders, B:// files)",
	Long: `Download the content embedded at a transaction output. By default the
server's unified media endpoint is used; --kind selects a specific decoder
(inscription, nft, bfile).

Output goes to --output when given, otherwise to a file named
<txid>_<vout><ext> derived from the returned Content-Type. Use --stdout to
stream bytes to stdout instead.

With --kind bfile the output index may be omitted: the server then returns
the transaction's first B:// file, which is what a bare b://<txid> resolves
to.`,
	Example: `  bb media 6a8f...c1:0
  bb media 6a8f...c1:0 --kind inscription -o art.png
  bb media 6a8f...c1:0 --stdout | file -
  bb media 6a8f...c1 --kind bfile`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		kind, ok := mediaKinds[mediaKind]
		if !ok {
			return fmt.Errorf("unknown --kind %q (want media, inscription, nft, or bfile)", mediaKind)
		}
		var path, defaultName string
		if kind == "bfile" && !strings.Contains(args[0], ":") {
			// A bare b://<txid>: the server finds the first B:// file, which is
			// not always at output 0.
			txid, err := normalizeTxID(args[0])
			if err != nil {
				return err
			}
			path = "/api/v1/tx/" + url.PathEscape(txid) + "/bfile"
			defaultName = txid
		} else {
			txid, vout, err := parseOutpoint(args[0])
			if err != nil {
				return err
			}
			path = "/api/v1/tx/" + url.PathEscape(txid) + "/" + kind + "/" + strconv.Itoa(vout)
			defaultName = fmt.Sprintf("%s_%d", txid, vout)
		}
		// Stream the content straight to its sink; on-chain media can be large,
		// so it is never fully buffered in memory. streamClient omits the overall
		// HTTP timeout, which would otherwise abort a long download mid-copy.
		body, hdr, err := streamClient().Stream(cmd.Context(), path, nil)
		if err != nil {
			return err
		}
		defer body.Close()
		contentType := hdr.Get("Content-Type")

		// --stdout, or -o - (stdout sentinel, consistent with `bb tx -o -`).
		if mediaStdout || mediaOutFile == "-" {
			_, err := io.Copy(cmd.OutOrStdout(), body)
			return err
		}
		out := mediaOutFile
		if out == "" {
			out = defaultName + extForContentType(contentType)
		}
		return saveStream(cmd, body, out, contentType)
	},
}

// saveStream copies body to out through a temp file in the same directory,
// renamed into place only once the copy fully succeeds: writing the target
// directly would truncate (and, on a mid-download error, delete) a
// pre-existing file at that path even though the download never completed.
//
// A symlink destination that resolves has its target replaced (the link
// stays; a dangling one is refused), and a
// non-regular destination such as a FIFO or a device is written in place,
// since replacing it would cut off whatever reads from it.
func saveStream(cmd *cobra.Command, body io.Reader, out, contentType string) error {
	if w, closeFn, ok, err := destinationDescriptor(cmd, out); ok {
		if err != nil {
			return err
		}
		defer closeFn()
		// A path naming one of this process's descriptors (/dev/stdout,
		// /dev/fd/N, a process substitution): write through the descriptor
		// itself, keeping its offset and append mode. Reopening or replacing
		// the file behind it would clobber what a `>>` redirect accumulated.
		n, err := io.Copy(w, body)
		if err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", n, out)
		return nil
	}
	return saveStreamDepth(cmd, body, out, contentType, 0)
}

// destinationDescriptor reports whether out leads to one of this process's
// descriptors, directly (/dev/stdout, /dev/fd/N), through symlinks whose
// targets name one (sink -> /dev/stdout), or by being the very file stdout or
// stderr is redirected to. Such a destination is written through the
// descriptor: following it to the backing file and replacing that would
// erase what a `>>` redirect accumulated.
func destinationDescriptor(cmd *cobra.Command, out string) (io.Writer, func(), bool, error) {
	if w, c, ok, err := descriptorWriter(cmd, out); ok {
		return w, c, ok, err
	}
	cur := out
	for i := 0; i < 40; i++ {
		st, err := os.Lstat(cur)
		if err != nil || st.Mode()&os.ModeSymlink == 0 {
			break
		}
		txt, err := os.Readlink(cur)
		if err != nil {
			break
		}
		if !filepath.IsAbs(txt) {
			txt = filepath.Join(filepath.Dir(cur), txt)
		}
		if w, c, ok, err := descriptorWriter(cmd, txt); ok {
			return w, c, ok, err
		}
		cur = txt
	}
	if fst, err := os.Stat(out); err == nil {
		for _, std := range []struct {
			f *os.File
			w io.Writer
		}{{os.Stdout, cmd.OutOrStdout()}, {os.Stderr, cmd.ErrOrStderr()}} {
			if sst, err := std.f.Stat(); err == nil && os.SameFile(fst, sst) {
				return std.w, func() {}, true, nil
			}
		}
	}
	return nil, nil, false, nil
}

// descriptorWriter maps a path that names one of this process's open
// descriptors to a writer on that descriptor: /dev/stdout, /dev/stderr,
// /dev/fd/N and /proc/self/fd/N (N >= 0). Other paths return ok=false. For
// another fd it writes through a duplicate, closed by closeFn: wrapping the
// borrowed fd itself would let the GC close it (and whatever later reuses
// its number).
func descriptorWriter(cmd *cobra.Command, out string) (w io.Writer, closeFn func(), ok bool, err error) {
	noop := func() {}
	clean := filepath.Clean(out)
	switch clean {
	case "/dev/stdout":
		return cmd.OutOrStdout(), noop, true, nil
	case "/dev/stderr":
		return cmd.ErrOrStderr(), noop, true, nil
	}
	var rest string
	switch {
	case strings.HasPrefix(clean, "/dev/fd/"):
		rest = strings.TrimPrefix(clean, "/dev/fd/")
	case strings.HasPrefix(clean, "/proc/self/fd/"):
		rest = strings.TrimPrefix(clean, "/proc/self/fd/")
	default:
		return nil, nil, false, nil
	}
	fd, convErr := strconv.Atoi(rest)
	if convErr != nil || fd < 0 {
		return nil, nil, false, nil
	}
	switch fd {
	case 1:
		return cmd.OutOrStdout(), noop, true, nil
	case 2:
		return cmd.ErrOrStderr(), noop, true, nil
	}
	f, err := dupFile(fd, clean)
	if err != nil {
		return nil, nil, true, fmt.Errorf("%s: %w", clean, err)
	}
	return f, func() { _ = f.Close() }, true, nil
}

func saveStreamDepth(cmd *cobra.Command, body io.Reader, out, contentType string, depth int) error {
	if depth > 40 {
		return fmt.Errorf("%s: too many levels of symbolic links", out)
	}
	if st, err := os.Lstat(out); err == nil {
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			// What the link leads to, as the kernel resolves it. This also
			// covers descriptor magic links such as Linux /dev/stdout ->
			// /proc/self/fd/1 -> pipe:[N], which no path walk can follow.
			fst, err := os.Stat(out)
			if err != nil {
				// Dangling: guessing where it points could land on an
				// unrelated file.
				return fmt.Errorf("%s is a symbolic link that does not resolve (%v); pass the real destination path", out, err)
			}
			if fst.IsDir() {
				return fmt.Errorf("%s is a directory", out)
			}
			if !fst.Mode().IsRegular() {
				// A pipe, FIFO, terminal or device: stream through the
				// original path.
				return writeInPlace(cmd, body, out, contentType)
			}
			target, err := filepath.EvalSymlinks(out)
			if err != nil {
				// A regular file reachable only through a magic link:
				// write it in place rather than guess a path.
				return writeInPlace(cmd, body, out, contentType)
			}
			return saveStreamDepth(cmd, body, target, contentType, depth+1)
		case !st.Mode().IsRegular():
			if st.IsDir() {
				return fmt.Errorf("%s is a directory", out)
			}
			return writeInPlace(cmd, body, out, contentType)
		}
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".bb-download-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// The temp file is created 0600 and stays private while it is written;
	// it gets its final mode only once complete.
	n, err := io.Copy(f, body)
	if err == nil {
		err = f.Chmod(outputMode(out))
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// Discard the partial download; the target file is untouched.
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, out); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if contentType != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes (%s) to %s\n", n, contentType, out)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", n, out)
	}
	return nil
}

// parseOutpoint accepts "txid:vout".
func parseOutpoint(s string) (string, int, error) {
	txidRaw, voutStr, found := strings.Cut(s, ":")
	if !found {
		return "", 0, fmt.Errorf("expected <txid>:<vout>, got %q", s)
	}
	txid, err := normalizeTxID(txidRaw)
	if err != nil {
		return "", 0, err
	}
	vout, err := strconv.Atoi(strings.TrimSpace(voutStr))
	if err != nil || vout < 0 {
		return "", 0, fmt.Errorf("invalid vout %q", voutStr)
	}
	return txid, vout, nil
}

func extForContentType(ct string) string {
	if ct == "" {
		return ".bin"
	}
	if mt, _, err := mime.ParseMediaType(ct); err == nil {
		ct = mt
	}
	// Prefer well-known extensions; mime.ExtensionsByType ordering is
	// platform-dependent.
	switch ct {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "text/plain":
		return ".txt"
	case "text/html":
		return ".html"
	case "application/json":
		return ".json"
	case "video/mp4":
		return ".mp4"
	}
	if exts, err := mime.ExtensionsByType(ct); err == nil && len(exts) > 0 {
		return exts[0]
	}
	return ".bin"
}

func init() {
	f := mediaCmd.Flags()
	f.StringVar(&mediaKind, "kind", "media", "content decoder: media, inscription, nft, or bfile")
	f.StringVarP(&mediaOutFile, "output", "o", "", "output file (default <txid>_<vout>.<ext>, or <txid>.<ext> for a bare bfile)")
	f.BoolVar(&mediaStdout, "stdout", false, "write content bytes to stdout")
	rootCmd.AddCommand(mediaCmd)
}

// writeInPlace streams body into an existing non-regular file (a FIFO, a
// device such as /dev/stdout) without replacing it.
func writeInPlace(cmd *cobra.Command, body io.Reader, out, contentType string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	f, err := openForWriteCtx(ctx, out)
	if err != nil {
		return err
	}
	// Ctrl-C cancels ctx; closing the file then unblocks a write stalled on
	// a reader that stopped draining (pipes and FIFOs are pollable).
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
	if closeErr := f.Close(); err == nil && !errors.Is(closeErr, os.ErrClosed) {
		err = closeErr
	}
	if ctx.Err() != nil {
		return fmt.Errorf("writing %s: %w", out, ctx.Err())
	}
	if err != nil {
		return err
	}
	if contentType != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes (%s) to %s\n", n, contentType, out)
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", n, out)
	}
	return nil
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
