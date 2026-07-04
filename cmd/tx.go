package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/b-open-io/bananablocks-cli/internal/spv"
	"github.com/spf13/cobra"
)

var (
	txHex     bool
	txBeef    bool
	txProof   bool
	txVerify  bool
	txOutFile string
)

var txCmd = &cobra.Command{
	Use:   "tx <txid>",
	Short: "Show a transaction; fetch hex/BEEF/merkle proof; verify SPV proofs",
	Long: `Fetch a transaction as JSON (default), raw hex, BEEF, or a TSC merkle
proof.

--verify recomputes merkle roots locally and checks them against block
headers fetched from the server: combined with --proof it verifies the TSC
path; otherwise it fetches the transaction's BEEF and verifies every BUMP
in it.`,
	Example: `  bb tx 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b
  bb tx <txid> --hex
  bb tx <txid> --beef -o tx.beef
  bb tx <txid> --proof --verify
  bb tx <txid> --verify`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		txid := strings.ToLower(strings.TrimSpace(args[0]))
		set := 0
		for _, b := range []bool{txHex, txBeef, txProof} {
			if b {
				set++
			}
		}
		if set > 1 {
			return errors.New("pick at most one of --hex, --beef, --proof")
		}
		c := client()
		ctx := cmd.Context()
		base := "/api/v1/tx/" + url.PathEscape(txid)

		switch {
		case txHex:
			raw, _, err := c.Bytes(ctx, base+"/hex", nil)
			if err != nil {
				return err
			}
			return writePayload(cmd, raw, true)

		case txBeef:
			raw, hdr, err := c.Bytes(ctx, base+"/beef", nil)
			if err != nil {
				return err
			}
			truncated := hdr.Get("X-BEEF-Truncated") == "true"
			if truncated {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: BEEF was truncated by the server (partial ancestry)")
			}
			if txVerify {
				return verifyBeefBytes(cmd, txid, raw, truncated)
			}
			return writePayload(cmd, raw, false)

		case txProof:
			var proof spv.TSCProof
			if err := c.GetJSON(ctx, base+"/proof", nil, &proof); err != nil {
				return err
			}
			if txVerify {
				res, err := spv.VerifyTSC(ctx, c, txid, &proof)
				if err != nil {
					return err
				}
				return renderVerify(cmd, []*spv.Result{res})
			}
			return render.JSON(cmd.OutOrStdout(), proof)

		case txVerify:
			raw, hdr, err := c.Bytes(ctx, base+"/beef", nil)
			if err != nil {
				return err
			}
			return verifyBeefBytes(cmd, txid, raw, hdr.Get("X-BEEF-Truncated") == "true")

		default:
			var out json.RawMessage
			if err := c.GetJSON(ctx, base, nil, &out); err != nil {
				return err
			}
			return render.JSON(cmd.OutOrStdout(), out)
		}
	},
}

// writePayload writes raw to --output when set; otherwise to stdout, hex-
// encoding binary payloads so they stay terminal-safe.
func writePayload(cmd *cobra.Command, raw []byte, isText bool) error {
	if txOutFile != "" && txOutFile != "-" {
		if err := os.WriteFile(txOutFile, raw, 0o644); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", len(raw), txOutFile)
		return nil
	}
	if isText {
		fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(raw)))
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%x\n", raw)
	return nil
}

// verifyBeefBytes runs BEEF verification, turning the no-proofs case into an
// actionable message when the server admitted the BEEF is truncated.
func verifyBeefBytes(cmd *cobra.Command, txid string, raw []byte, truncated bool) error {
	results, err := spv.VerifyBEEF(cmd.Context(), client(), txid, raw)
	if err != nil {
		if truncated {
			return fmt.Errorf("%w (the server returned a truncated BEEF without the target merkle proof — retry later, or verify via the TSC path: bb tx %s --proof --verify)", err, txid)
		}
		return err
	}
	return renderVerify(cmd, results)
}

// renderVerify prints verification results and exits non-zero on any failure.
func renderVerify(cmd *cobra.Command, results []*spv.Result) error {
	if err := render.JSON(cmd.OutOrStdout(), results); err != nil {
		return err
	}
	for _, r := range results {
		if !r.Valid {
			return fmt.Errorf("proof INVALID: computed root %s does not match header root %s", r.ComputedRoot, r.HeaderRoot)
		}
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "proof OK: computed merkle root(s) match the block header(s)")
	return nil
}

func init() {
	f := txCmd.Flags()
	f.BoolVar(&txHex, "hex", false, "output raw transaction hex")
	f.BoolVar(&txBeef, "beef", false, "output BEEF (binary with -o, hex to stdout)")
	f.BoolVar(&txProof, "proof", false, "output TSC merkle proof JSON")
	f.BoolVar(&txVerify, "verify", false, "verify the merkle proof locally against block headers")
	f.StringVarP(&txOutFile, "output", "o", "", "write payload to file instead of stdout")
	rootCmd.AddCommand(txCmd)
}
