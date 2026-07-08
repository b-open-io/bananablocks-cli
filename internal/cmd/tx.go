package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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

	flagHeadersURL   string
	flagHeadersToken string
	flagNoHeaders    bool
)

var txCmd = &cobra.Command{
	Use:   "tx <txid>",
	Short: "Show a transaction; fetch hex/BEEF/merkle proof; verify SPV proofs",
	Long: `Fetch a transaction as JSON (default), raw hex, BEEF, or a TSC merkle
proof.

--verify recomputes merkle roots locally and confirms them against an
independent block-headers service (default per --chain; --headers-url to
override, --no-headers to fall back to the serving API's blocks). Combined
with --proof it verifies the TSC path; otherwise it fetches the
transaction's BEEF and verifies every BUMP in it.`,
	Example: `  bb tx 4a5e1e4baab89f3a32518a88c31bc87f618f76673e2cc77ab2127b7afdeda33b
  bb tx <txid> --hex
  bb tx <txid> --beef -o tx.beef
  bb tx <txid> --proof --verify
  bb tx <txid> --verify`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		txid, err := normalizeTxID(args[0])
		if err != nil {
			return err
		}
		set := 0
		for _, b := range []bool{txHex, txBeef, txProof} {
			if b {
				set++
			}
		}
		if set > 1 {
			return errors.New("pick at most one of --hex, --beef, --proof")
		}
		// --hex just emits raw hex and returns before any verification runs, so
		// combining it with --verify would silently do nothing (a false sense of
		// verification). Reject the combination explicitly; --verify applies to
		// --proof or the default BEEF path.
		if txVerify && txHex {
			return errors.New("--verify cannot be combined with --hex (use --proof, or omit --hex to verify the BEEF)")
		}
		// Fail fast on an unusable verification config before any network I/O, so
		// the config error isn't mistaken for (or wrapped as) a fetch/proof error.
		if txVerify {
			if err := headersConfigError(); err != nil {
				return err
			}
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
				// Honor -o even when verifying: write the BEEF to the file
				// first, then verify.
				switch {
				case txOutFile == "-":
					// Binary BEEF on stdout would corrupt the JSON verification
					// results that also go to stdout, so -o - can't be honored
					// here. Warn instead of silently dropping the payload.
					fmt.Fprintln(cmd.ErrOrStderr(), "warning: -o - is ignored with --verify; rerun without --verify to emit BEEF to stdout")
				case txOutFile != "":
					if err := os.WriteFile(txOutFile, raw, 0o644); err != nil {
						return err
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "wrote %d bytes to %s\n", len(raw), txOutFile)
				}
				return verifyBeefBytes(cmd, txid, raw, truncated)
			}
			return writePayload(cmd, raw, false)

		case txProof:
			var proof spv.TSCProof
			if err := c.GetJSON(ctx, base+"/proof", nil, &proof); err != nil {
				return err
			}
			if txVerify {
				results, source, err := runVerify(cmd, func(v spv.RootVerifier) ([]*spv.Result, error) {
					res, err := spv.VerifyTSC(ctx, c, v, txid, &proof)
					if err != nil {
						return nil, err
					}
					return []*spv.Result{res}, nil
				})
				if err != nil {
					return err
				}
				return renderVerify(cmd, results, source)
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
	results, source, err := runVerify(cmd, func(v spv.RootVerifier) ([]*spv.Result, error) {
		return spv.VerifyBEEF(cmd.Context(), v, txid, raw)
	})
	if err != nil {
		if truncated {
			return fmt.Errorf("%w (the server returned a truncated BEEF without the target merkle proof — retry later, or verify via the TSC path: bb tx %s --proof --verify)", err, txid)
		}
		return err
	}
	return renderVerify(cmd, results, source)
}

// runVerify runs a verification closure against the strong header service when
// one is configured (the default for main/test chains), falling back to the
// weaker indexer cross-check only if the header service is unreachable (a
// service error, not a negative verdict). It returns the results and a human
// phrase naming the source that produced them.
func runVerify(cmd *cobra.Command, run func(v spv.RootVerifier) ([]*spv.Result, error)) ([]*spv.Result, string, error) {
	indexer := func() spv.RootVerifier { return spv.NewIndexerVerifier(client()) }

	if flagNoHeaders {
		// The user explicitly opted out of the independent source.
		results, err := run(indexer())
		if err != nil {
			return nil, "", err
		}
		return results, weakSource, nil
	}

	u := headersURL()
	if u == "" {
		// Strong verification is the default, but no header service is known for
		// this --chain. Refuse to silently downgrade to the weaker cross-check.
		return nil, "", headersConfigError()
	}

	// Verify against the header service, falling back to the serving API only if
	// the header service is unreachable (a *HeadersServiceError). The fallback
	// happens inside FallbackVerifier at the ConfirmRoots seam, so the proof is
	// parsed and its roots computed only once regardless of fallback.
	fellBack := false
	v := &spv.FallbackVerifier{
		Primary:   spv.NewHeadersVerifier(u, headersToken(), &http.Client{Timeout: flagTimeout}),
		Secondary: indexer(),
		OnFallback: func(err error) {
			fellBack = true
			fmt.Fprintf(cmd.ErrOrStderr(), "warning: header service unavailable (%v); falling back to the serving API cross-check, which is weaker\n", err)
		},
	}
	results, err := run(v)
	if err != nil {
		return nil, "", err
	}
	if fellBack {
		return results, weakSource, nil
	}
	return results, "an independent header service (" + u + ")", nil
}

const weakSource = "the serving API (weaker: not an independent source)"

// renderVerify prints verification results and exits non-zero on any failure.
// It distinguishes a rejected root (fraud) from one the source could not yet
// confirm (inconclusive), so a too-recent header is not reported as invalid,
// and it only cites a block height / source when the chain was actually
// consulted for that result.
func renderVerify(cmd *cobra.Command, results []*spv.Result, source string) error {
	if err := render.JSON(cmd.OutOrStdout(), results); err != nil {
		return err
	}
	for _, r := range results {
		if r.State == spv.ConfInvalid {
			return fmt.Errorf("proof INVALID: %s", invalidDetail(r, source))
		}
	}
	for _, r := range results {
		if r.State == spv.ConfUnknown {
			return fmt.Errorf("proof UNVERIFIED: %s", unverifiedDetail(r, source))
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "proof OK: computed merkle root(s) confirmed against %s\n", source)
	return nil
}

// invalidDetail explains an INVALID verdict using only the facts that apply to
// that result: a proof-internal inconsistency and a transitive-ancestor failure
// are decided without consulting the chain, so neither cites a block or source.
func invalidDetail(r *spv.Result, source string) string {
	switch {
	case r.BlockHeight == 0 && r.HeaderRoot != "":
		return fmt.Sprintf("the proof's target %s does not match its computed root %s", r.HeaderRoot, r.ComputedRoot)
	case r.BlockHeight == 0:
		return fmt.Sprintf("%s is proven only via linked ancestors and an ancestor proof was rejected", r.Txid)
	default:
		return fmt.Sprintf("computed root %s for %s was rejected for block %d by %s", r.ComputedRoot, r.Txid, r.BlockHeight, source)
	}
}

// unverifiedDetail explains an inconclusive verdict.
func unverifiedDetail(r *spv.Result, source string) string {
	if r.BlockHeight == 0 {
		return fmt.Sprintf("%s is proven only via linked ancestors and an ancestor could not be confirmed by %s", r.Txid, source)
	}
	return fmt.Sprintf("%s could not confirm block %d yet (it may be too recent or not yet synced); retry later", source, r.BlockHeight)
}

// defaultHeadersHost returns the GorillaPool block-headers service for a known
// chain, or "" when no independent header source is known for it (callers then
// require the user to pass --headers-url or --no-headers rather than silently
// downgrading).
func defaultHeadersHost(chain string) string {
	switch chain {
	case "main", "mainnet":
		return "https://mainnet.headers.gorillapool.io"
	case "test", "testnet":
		return "https://testnet.headers.gorillapool.io"
	}
	return ""
}

// headersURL resolves the block-headers service to verify against: flag, then
// env, then the per-chain default. An empty result means no independent source
// is known (--no-headers is handled by the caller). A bare host is assumed
// https unless it already carries a scheme.
func headersURL() string {
	if flagHeadersURL != "" {
		return normalizeHost(flagHeadersURL)
	}
	if h := os.Getenv("BB_HEADERS_URL"); h != "" {
		return normalizeHost(h)
	}
	return defaultHeadersHost(flagChain)
}

// headersConfigError reports when strong verification is requested (not
// --no-headers) but no header service is configured for the chain, so callers
// can surface it as a clean config error rather than a verification failure.
func headersConfigError() error {
	if flagNoHeaders || headersURL() != "" {
		return nil
	}
	return fmt.Errorf("no block-headers service is configured for --chain %q; pass --headers-url <url> to verify against an independent source, or --no-headers to cross-check against the serving API only", flagChain)
}

func headersToken() string {
	if flagHeadersToken != "" {
		return flagHeadersToken
	}
	return os.Getenv("BB_HEADERS_TOKEN")
}

func init() {
	f := txCmd.Flags()
	f.BoolVar(&txHex, "hex", false, "output raw transaction hex")
	f.BoolVar(&txBeef, "beef", false, "output BEEF (binary with -o, hex to stdout)")
	f.BoolVar(&txProof, "proof", false, "output TSC merkle proof JSON")
	f.BoolVar(&txVerify, "verify", false, "verify the merkle proof locally against block headers")
	f.StringVarP(&txOutFile, "output", "o", "", "write payload to file instead of stdout")
	f.StringVar(&flagHeadersURL, "headers-url", "", "block-headers-service base URL for independent proof verification (env BB_HEADERS_URL; default per --chain)")
	f.StringVar(&flagHeadersToken, "headers-token", "", "bearer token for the headers service if it requires auth (env BB_HEADERS_TOKEN)")
	f.BoolVar(&flagNoHeaders, "no-headers", false, "verify against the serving API only, skipping the independent header service")
	rootCmd.AddCommand(txCmd)
}
