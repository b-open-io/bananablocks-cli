package cmd

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
	"github.com/spf13/cobra"
)

var keyCmd = &cobra.Command{
	Use:   "key",
	Short: "API-key operations: usage and x402 pay-to-upgrade",
}

var keyUsageCmd = &cobra.Command{
	Use:     "usage",
	Short:   "Show the calling key's tier, rate limit, and today's request count",
	Example: `  BB_API_KEY=bb_live_... bb key usage`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if apiKey() == "" {
			return errors.New("an API key is required: set --api-key or BB_API_KEY")
		}
		var out json.RawMessage
		if err := client().GetJSON(cmd.Context(), "/api/v1/key/usage", nil, &out); err != nil {
			return err
		}
		return render.JSON(cmd.OutOrStdout(), out)
	},
}

var (
	upgradeTier    string
	upgradeWIF     string
	upgradeWIFFile string
	upgradeYes     bool
	upgradeDryRun  bool
	upgradeFeeRate uint64
	upgradeTestnet bool
)

var keyUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Buy a rate-limit tier upgrade with an on-chain BSV payment (x402)",
	Long: `Upgrade the calling API key's rate-limit tier by paying the server's x402
(bsv-tx-v1) challenge on-chain.

The flow: the server answers with a 402 challenge naming a price and a
per-challenge payment address; bb builds and signs a transaction paying it
from the funding key's UTXOs; the signed transaction is submitted back as
payment proof. The SERVER broadcasts the transaction — bb never does — so
no payment leaves your wallet if the upgrade is rejected.

The funding key is a WIF private key. Prefer --wif-file, which keeps the key
out of shell history and inherited environments; --wif and the BB_WIF
environment variable also work but are less safe. When more than one is set,
--wif-file wins, then --wif, then BB_WIF. The key's P2PKH address must hold
enough confirmed satoshis to cover the challenge price plus a miner fee.

Without --tier the server picks the next tier above the key's current one.
Use --dry-run to fetch and inspect the challenge without paying.`,
	Example: `  bb key upgrade --dry-run
  bb key upgrade --tier pro --wif-file ~/.keys/funding.wif
  bb key upgrade --tier pro --wif-file ~/.keys/funding.wif --yes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if apiKey() == "" {
			return errors.New("an API key is required: set --api-key or BB_API_KEY")
		}
		c := client()
		ctx := cmd.Context()

		ch, err := fetchChallenge(cmd, c)
		if err != nil {
			return err
		}
		if err := ch.VerifyPayee(); err != nil {
			return err
		}

		errw := cmd.ErrOrStderr()
		fmt.Fprintf(errw, "x402 challenge:\n")
		fmt.Fprintf(errw, "  tier:      %s (%d days)\n", ch.Tier, ch.DurationDays)
		fmt.Fprintf(errw, "  price:     %d sats\n", ch.AmountSats)
		fmt.Fprintf(errw, "  pay to:    %s\n", ch.PayeeAddress)
		// A zero ExpiresAt (server omitted the field) would print a bogus
		// 0001-01-01 timestamp and a huge negative "in" duration — skip it.
		if !ch.ExpiresAt.IsZero() {
			fmt.Fprintf(errw, "  expires:   %s (in %s)\n", ch.ExpiresAt.Format(time.RFC3339), time.Until(ch.ExpiresAt).Round(time.Second))
		}

		if upgradeDryRun {
			return render.JSON(cmd.OutOrStdout(), ch)
		}

		if err := ensureNotExpired(ch); err != nil {
			return err
		}

		wif, err := fundingWIF()
		if err != nil {
			return err
		}
		wallet, err := x402.NewWallet(wif, !upgradeTestnet)
		if err != nil {
			return err
		}
		fmt.Fprintf(errw, "  from:      %s\n", wallet.Address)

		if !upgradeYes && !confirm(cmd, fmt.Sprintf("Pay %d sats to upgrade to %q?", ch.AmountSats, ch.Tier)) {
			return errors.New("aborted")
		}

		tx, err := wallet.BuildPayment(ctx, c, ch, upgradeFeeRate)
		if err != nil {
			return err
		}
		fmt.Fprintf(errw, "built payment %s (%d bytes); submitting proof...\n", tx.TxID().String(), tx.Size())

		res, err := submitProof(ctx, c, ch, tx.Bytes(), tx.TxID().String())
		if err != nil {
			return err
		}
		fmt.Fprintf(errw, "upgrade settled: tier %q until %s (payment txid %s)\n",
			res.Tier, res.TierExpiresAt.Format(time.RFC3339), res.Txid)
		return render.JSON(cmd.OutOrStdout(), res)
	},
}

// fetchChallenge POSTs the upgrade endpoint without a proof and extracts the
// challenge from the expected 402 response.
func fetchChallenge(cmd *cobra.Command, c *api.Client) (*x402.Challenge, error) {
	q := url.Values{}
	if upgradeTier != "" {
		q.Set("tier", upgradeTier)
	}
	err := c.JSON(cmd.Context(), http.MethodPost, x402.UpgradePath, q, "", nil, nil)
	if err == nil {
		return nil, errors.New("server did not issue a 402 challenge (unexpected success)")
	}
	var apiErr *api.Error
	if !errors.As(err, &apiErr) {
		return nil, err
	}
	if apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusMethodNotAllowed {
		return nil, fmt.Errorf("%w — x402 pay-to-upgrade does not appear to be enabled on %s", apiErr, host())
	}
	if apiErr.Status != http.StatusPaymentRequired {
		return nil, apiErr
	}
	return challengeFromResponse(apiErr)
}

// challengeFromResponse pulls the challenge out of a 402: JSON body first,
// X402-Challenge header as fallback.
func challengeFromResponse(apiErr *api.Error) (*x402.Challenge, error) {
	var body x402.ChallengeBody
	if json.Unmarshal(apiErr.Body, &body) == nil && body.Challenge != nil && body.Challenge.ChallengeID != "" {
		return body.Challenge, nil
	}
	if h := apiErr.Header.Get("X402-Challenge"); h != "" {
		return x402.DecodeChallengeHeader(h)
	}
	return nil, fmt.Errorf("402 response carried no challenge: %s", apiErr.Message)
}

// ensureNotExpired rejects an already-expired challenge before any UTXO fetch,
// signing, or payment prompt, rather than doing that work only for the server
// to reject the submission with a 410.
func ensureNotExpired(ch *x402.Challenge) error {
	if !ch.ExpiresAt.IsZero() && time.Now().After(ch.ExpiresAt) {
		return fmt.Errorf("challenge expired at %s; rerun to request a fresh one", ch.ExpiresAt.Format(time.RFC3339))
	}
	return nil
}

// submitProof POSTs the signed payment as an X402-Proof header and decodes
// the settlement result.
func submitProof(ctx context.Context, c *api.Client, ch *x402.Challenge, rawTx []byte, txid string) (*x402.UpgradeResult, error) {
	hdr, err := x402.EncodeProofHeader(&x402.Proof{
		Version:     x402.Version,
		ChallengeID: ch.ChallengeID,
		RawTxBase64: base64.StdEncoding.EncodeToString(rawTx),
		Txid:        txid,
	})
	if err != nil {
		return nil, err
	}
	req, err := c.NewRequest(ctx, http.MethodPost, submitPath(ch.PayURL, c.BaseURL), nil, "", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X402-Proof", hdr)
	resp, err := c.Do(req)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return nil, upgradeError(apiErr)
		}
		return nil, err
	}
	defer resp.Body.Close()
	// Bound the body like the client's JSON/Bytes paths do, rather than decoding
	// straight from an unbounded network stream.
	raw, err := c.ReadBody(resp.Body)
	if err != nil {
		return nil, err
	}
	var res x402.UpgradeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decoding upgrade result: %w", err)
	}
	// A 2xx body that unmarshals cleanly but carries no settlement data (e.g. an
	// empty object or an error payload) must not be reported as a settled
	// upgrade — require the fields that prove settlement.
	if res.Tier == "" && res.Txid == "" {
		return nil, fmt.Errorf("upgrade response contained no settlement details: %s", strings.TrimSpace(string(raw)))
	}
	return &res, nil
}

// submitPath resolves where to POST the payment proof. The challenge's pay_url
// takes precedence (a challenge issued off a rate-limited endpoint may name a
// different path or carry query params), falling back to the canonical upgrade
// path. An absolute pay_url is only honored when it targets the same host as
// the client — the request carries the Bearer API key, so it must never be
// sent cross-origin on the server's say-so.
func submitPath(payURL, baseURL string) string {
	if payURL == "" {
		return x402.UpgradePath
	}
	u, err := url.Parse(payURL)
	if err != nil {
		return x402.UpgradePath
	}
	if u.IsAbs() {
		base, err := url.Parse(baseURL)
		if err != nil || !strings.EqualFold(u.Host, base.Host) {
			return x402.UpgradePath
		}
	}
	p := u.EscapedPath()
	if p == "" {
		return x402.UpgradePath
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
}

// upgradeError maps the server's x402 error statuses onto actionable messages.
func upgradeError(apiErr *api.Error) error {
	switch apiErr.Status {
	case http.StatusPaymentRequired:
		if ch, err := challengeFromResponse(apiErr); err == nil {
			return fmt.Errorf("payment insufficient: the challenge wants %d sats to %s (challenge still open — rerun to retry)",
				ch.AmountSats, ch.PayeeAddress)
		}
		return fmt.Errorf("payment insufficient: %s", apiErr.Message)
	case http.StatusConflict:
		return fmt.Errorf("%s (the payment tx was not broadcast by this attempt)", apiErr.Message)
	case http.StatusGone:
		return errors.New("challenge expired; rerun to request a fresh one")
	case http.StatusUnprocessableEntity:
		return fmt.Errorf("broadcast rejected the payment transaction: %s — check the funding UTXOs are unspent", apiErr.Message)
	default:
		return apiErr
	}
}

// fundingWIFExplicit resolves a funding key set explicitly for this command
// via --wif-file (preferred) or --wif. It deliberately ignores BB_WIF: an
// inherited environment variable should not silently fund a payment. An empty
// string with a nil error means no explicit key was given.
func fundingWIFExplicit() (string, error) {
	if upgradeWIFFile != "" {
		raw, err := os.ReadFile(upgradeWIFFile)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(raw)), nil
	}
	if upgradeWIF != "" {
		return strings.TrimSpace(upgradeWIF), nil
	}
	return "", nil
}

// fundingWIF resolves the payment key, preferring an explicit --wif-file/--wif
// over the BB_WIF environment variable.
func fundingWIF() (string, error) {
	if w, err := fundingWIFExplicit(); err != nil || w != "" {
		return w, err
	}
	if w := strings.TrimSpace(os.Getenv("BB_WIF")); w != "" {
		return w, nil
	}
	return "", errors.New("a funding key is required: set --wif-file, --wif, or BB_WIF")
}

// confirm prompts on stderr and reads a y/N answer from stdin.
func confirm(cmd *cobra.Command, prompt string) bool {
	return confirmIO(cmd.InOrStdin(), cmd.ErrOrStderr(), prompt)
}

func confirmIO(in io.Reader, errw io.Writer, prompt string) bool {
	fmt.Fprintf(errw, "%s [y/N]: ", prompt)
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// offerUpgradeOn402 runs after a command fails with a rate-limit 402 (issued
// because the API client advertises X-Payment-Accept: x402). If the response
// carries a challenge it prints the upgrade terms and, when the session is
// interactive and a funding key is already at hand, offers to pay on the
// spot; otherwise it points at `bb key upgrade`. Anonymous 402s carry no
// challenge — the server's "get an API key" hint is already in the error.
func offerUpgradeOn402(errw io.Writer, err error) {
	var apiErr *api.Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusPaymentRequired {
		return
	}
	ch, chErr := challengeFromResponse(apiErr)
	if chErr != nil {
		return
	}
	fmt.Fprintf(errw, "\nThis API key can be upgraded on-chain (x402):\n")
	fmt.Fprintf(errw, "  tier:    %s (%d days)\n", ch.Tier, ch.DurationDays)
	fmt.Fprintf(errw, "  price:   %d sats\n", ch.AmountSats)

	hint := fmt.Sprintf("Run: bb key upgrade --tier %s\n", ch.Tier)
	// Only offer to pay inline from a key set explicitly for this command; an
	// inherited BB_WIF must not be spent from an unrelated rate-limited call.
	wif, wifErr := fundingWIFExplicit()
	if wifErr != nil || wif == "" || !interactive() {
		fmt.Fprint(errw, hint)
		return
	}
	if payErr := payChallenge(errw, wif, ch); payErr != nil {
		if !errors.Is(payErr, errDeclined) {
			fmt.Fprintln(errw, "upgrade failed:", payErr)
		}
		fmt.Fprint(errw, hint)
	}
}

var errDeclined = errors.New("declined")

// payChallenge settles an already-issued challenge with the funding key,
// mirroring `bb key upgrade` minus the challenge fetch.
func payChallenge(errw io.Writer, wif string, ch *x402.Challenge) error {
	if err := ch.VerifyPayee(); err != nil {
		return err
	}
	if err := ensureNotExpired(ch); err != nil {
		return err
	}
	wallet, err := x402.NewWallet(wif, !upgradeTestnet)
	if err != nil {
		return err
	}
	fmt.Fprintf(errw, "  pay to:  %s\n", ch.PayeeAddress)
	fmt.Fprintf(errw, "  from:    %s\n", wallet.Address)
	if !confirmIO(os.Stdin, errw, fmt.Sprintf("Pay %d sats now to upgrade to %q?", ch.AmountSats, ch.Tier)) {
		return errDeclined
	}
	ctx, cancel := context.WithTimeout(context.Background(), flagTimeout)
	defer cancel()
	c := client()
	tx, err := wallet.BuildPayment(ctx, c, ch, upgradeFeeRate)
	if err != nil {
		return err
	}
	res, err := submitProof(ctx, c, ch, tx.Bytes(), tx.TxID().String())
	if err != nil {
		return err
	}
	fmt.Fprintf(errw, "upgrade settled: tier %q until %s (payment txid %s) — rerun your command\n",
		res.Tier, res.TierExpiresAt.Format(time.RFC3339), res.Txid)
	return nil
}

// interactive reports whether both stdin and stderr are terminals, i.e. a
// human can answer a prompt.
func interactive() bool {
	for _, f := range []*os.File{os.Stdin, os.Stderr} {
		fi, err := f.Stat()
		if err != nil || fi.Mode()&os.ModeCharDevice == 0 {
			return false
		}
	}
	return true
}

func init() {
	f := keyUpgradeCmd.Flags()
	f.StringVar(&upgradeTier, "tier", "", "target tier (default: next tier above the key's current one)")
	f.StringVar(&upgradeWIF, "wif", "", "funding private key (WIF); less safe than --wif-file")
	f.StringVar(&upgradeWIFFile, "wif-file", "", "file containing the funding WIF (recommended)")
	f.BoolVar(&upgradeYes, "yes", false, "skip the payment confirmation prompt")
	f.BoolVar(&upgradeDryRun, "dry-run", false, "fetch and print the challenge without paying")
	f.Uint64Var(&upgradeFeeRate, "fee-rate", 1, "miner fee rate in sat/kB")
	f.BoolVar(&upgradeTestnet, "testnet", false, "derive the funding address for testnet")
	keyCmd.AddCommand(keyUsageCmd, keyUpgradeCmd)
	rootCmd.AddCommand(keyCmd)
}
