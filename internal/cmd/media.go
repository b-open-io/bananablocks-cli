package cmd

import (
	"fmt"
	"io"
	"mime"
	"net/url"
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
		if mediaOutFile == "" {
			return saveGenerated(cmd, body, defaultName+extForContentType(contentType), contentType)
		}
		return saveStream(cmd, body, mediaOutFile, contentType)
	},
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
