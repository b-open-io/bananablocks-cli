package cmd

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/spf13/cobra"
)

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

var broadcastCmd = &cobra.Command{
	Use:   "broadcast <hex|file|-> [more files...]",
	Short: "Broadcast raw transaction(s) to the network",
	Long: `Broadcast one or more raw transactions. Each argument is a hex string, a
path to a file containing hex, or "-" for stdin. With multiple arguments
the transactions are submitted in one batch to the multi-broadcast
endpoint.`,
	Example: `  bb broadcast 01000000...
  bb broadcast tx.hex
  cat tx.hex | bb broadcast -
  bb broadcast tx1.hex tx2.hex tx3.hex`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var hexes []string
		for _, a := range args {
			h, err := readTxHexArg(a)
			if err != nil {
				return err
			}
			hexes = append(hexes, h)
		}
		c := client()
		ctx := cmd.Context()

		if len(hexes) == 1 {
			var out json.RawMessage
			err := c.JSON(ctx, http.MethodPost, wocPath("/tx/broadcast"), nil,
				"text/plain", strings.NewReader(hexes[0]), &out)
			if err != nil {
				return err
			}
			return render.JSON(cmd.OutOrStdout(), out)
		}

		batch := make([]map[string]string, 0, len(hexes))
		for _, h := range hexes {
			batch = append(batch, map[string]string{"txhex": h})
		}
		var out json.RawMessage
		if err := c.PostJSONBody(ctx, wocPath("/tx/broadcast/multi"), nil, batch, &out); err != nil {
			return err
		}
		return render.JSON(cmd.OutOrStdout(), out)
	},
}

// readTxHexArg resolves an argument into validated transaction hex.
func readTxHexArg(arg string) (string, error) {
	var raw []byte
	switch {
	case arg == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		raw = b
	case hexRe.MatchString(arg) && len(arg)%2 == 0:
		// An argument that is itself valid, even-length hex is transaction hex —
		// take it literally even if a file of the same name exists, so a hex
		// string is never silently read from a colliding filename. (Real tx hex
		// is always even-length; odd-length all-hex args fall through so a file
		// named e.g. "abc" is still readable.)
		raw = []byte(arg)
	case fileExists(arg):
		b, err := os.ReadFile(arg)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		raw = []byte(arg)
	}
	// Collapse all ASCII whitespace so line-wrapped hex (e.g. from `xxd -p`,
	// which wraps every 60 chars) is recognized as hex rather than mistaken
	// for binary and double-encoded.
	s := strings.Join(strings.Fields(string(raw)), "")
	if s == "" {
		return "", fmt.Errorf("%s: empty transaction payload", arg)
	}
	// All-hex content that is odd-length is malformed (truncated/corrupt) hex,
	// not binary — flag it precisely rather than double-encoding it as bytes.
	// This check applies to files and stdin too, where an odd-length hex payload
	// would otherwise be silently re-encoded and broadcast as a wrong tx.
	if hexRe.MatchString(s) {
		if len(s)%2 == 0 {
			return s, nil
		}
		return "", fmt.Errorf("%q looks like hex but has an odd number of digits (%d)", truncateArg(arg), len(s))
	}
	// Not hex at all: a real file (or stdin) may hold raw binary — encode it. The
	// original bytes are encoded, not the whitespace-stripped view.
	if arg == "-" || fileExists(arg) {
		return hex.EncodeToString(raw), nil
	}
	return "", fmt.Errorf("%q is neither valid hex nor an existing file", truncateArg(arg))
}

func truncateArg(a string) string {
	if len(a) > 40 {
		return a[:40] + "..."
	}
	return a
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func init() {
	rootCmd.AddCommand(broadcastCmd)
}
