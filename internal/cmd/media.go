package cmd

import (
	"fmt"
	"mime"
	"net/url"
	"os"
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
	Use:   "media <txid>:<vout>",
	Short: "Download on-chain content (inscriptions, NFT renders, B:// files)",
	Long: `Download the content embedded at a transaction output. By default the
server's unified media endpoint is used; --kind selects a specific decoder
(inscription, nft, bfile).

Output goes to --output when given, otherwise to a file named
<txid>_<vout><ext> derived from the returned Content-Type. Use --stdout to
stream bytes to stdout instead.`,
	Example: `  bb media 6a8f...c1:0
  bb media 6a8f...c1:0 --kind inscription -o art.png
  bb media 6a8f...c1:0 --stdout | file -`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		txid, vout, err := parseOutpoint(args[0])
		if err != nil {
			return err
		}
		kind, ok := mediaKinds[mediaKind]
		if !ok {
			return fmt.Errorf("unknown --kind %q (want media, inscription, nft, or bfile)", mediaKind)
		}
		path := "/api/v1/tx/" + url.PathEscape(txid) + "/" + kind + "/" + strconv.Itoa(vout)
		raw, hdr, err := client().Bytes(cmd.Context(), path, nil)
		if err != nil {
			return err
		}
		contentType := hdr.Get("Content-Type")

		if mediaStdout {
			_, err := cmd.OutOrStdout().Write(raw)
			return err
		}
		out := mediaOutFile
		if out == "" {
			out = fmt.Sprintf("%s_%d%s", txid, vout, extForContentType(contentType))
		}
		if err := os.WriteFile(out, raw, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes (%s) to %s\n", len(raw), contentType, out)
		return nil
	},
}

// parseOutpoint accepts "txid:vout".
func parseOutpoint(s string) (string, int, error) {
	txid, voutStr, found := strings.Cut(s, ":")
	if !found {
		return "", 0, fmt.Errorf("expected <txid>:<vout>, got %q", s)
	}
	txid = strings.ToLower(strings.TrimSpace(txid))
	if len(txid) != 64 {
		return "", 0, fmt.Errorf("invalid txid %q", txid)
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
	f.StringVarP(&mediaOutFile, "output", "o", "", "output file (default <txid>_<vout>.<ext>)")
	f.BoolVar(&mediaStdout, "stdout", false, "write content bytes to stdout")
	rootCmd.AddCommand(mediaCmd)
}
