package cmd

import (
	"bufio"
	"context"
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
		return getRender(cmd, "/api/v1/key/usage", nil)
	},
}

var (
	upgradeTier       string
	upgradeWIF        string
	upgradeWIFFile    string
	upgradeYes        bool
	upgradeDryRun     bool
	upgradeFeeRate    uint64
	upgradeTestnet    bool
	upgradeWait       time.Duration
	upgradeNewPayment bool
)

var keyUpgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Buy a rate-limit tier upgrade with an on-chain BSV payment (x402)",
	Long: `Upgrade the calling API key's rate-limit tier by paying the server's x402
(bsv-tx-v1) challenge on-chain.

The flow: the server answers with a 402 challenge naming a price and a
per-challenge payment address; bb builds and signs a transaction paying it
from the funding key's UTXOs; the signed transaction is submitted back as
payment proof. The SERVER broadcasts the transaction — bb never does.

The proof is saved (under BB_STATE_DIR, default the user config dir) before
it is first submitted, and bb resubmits that SAME proof while the server
reports the payment as pending, up to --wait. Once a proof has been
submitted its payment may be on the network, and a second payment to the
same challenge is never credited, so a later run resubmits any saved proof
instead of paying again. --new-payment discards saved proofs; use it only
after confirming their transactions were not mined.

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
		errw := cmd.ErrOrStderr()

		store, err := proofStore()
		if err != nil {
			return err
		}
		saved, err := store.List()
		if err != nil {
			return err
		}
		if len(saved) > 0 {
			switch {
			case upgradeDryRun:
				fmt.Fprintf(errw, "%d saved payment proof(s) from an earlier run are pending; `bb key upgrade` will resubmit them instead of paying again\n", len(saved))
			case upgradeNewPayment:
				for _, sp := range saved {
					fmt.Fprintf(errw, "warning: discarding the saved proof for payment %s (challenge %s); a mined copy of it is NOT credited by a new payment\n", sp.Txid, sp.ChallengeID)
					if err := store.Delete(sp.ChallengeID); err != nil {
						return err
					}
				}
			default:
				// Never pay over an unresolved proof: its payment may already be
				// broadcast, and a second payment to its challenge is not credited.
				res, err := resubmitSaved(ctx, c, errw, store, saved, upgradeWait)
				if err != nil {
					return err
				}
				return render.JSON(cmd.OutOrStdout(), res)
			}
		}

		ch, err := fetchChallenge(cmd, c)
		if err != nil {
			return err
		}
		if err := ch.VerifyPayee(); err != nil {
			return err
		}

		fmt.Fprintf(errw, "x402 challenge:\n")
		fmt.Fprintf(errw, "  tier:      %s (%d days)\n", ch.Tier, ch.DurationDays)
		fmt.Fprintf(errw, "  price:     %d sats\n", ch.AmountSats)
		fmt.Fprintf(errw, "  pay to:    %s\n", ch.PayeeDisplay())
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

		res, err := payAndSettle(ctx, c, errw, store, wallet, ch)
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
		ch, err := x402.DecodeChallengeHeader(h)
		if err != nil {
			return nil, err
		}
		// Require a challenge_id on the header path too (the JSON-body path above
		// already does). Without it the settlement can never be matched, so we
		// would build and submit a payment the server cannot credit.
		if ch.ChallengeID == "" {
			return nil, fmt.Errorf("402 response carried a challenge with no challenge_id: %s", apiErr.Message)
		}
		return ch, nil
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
	// A relative pay_url (e.g. "key/upgrade") parses to a path without a leading
	// slash, which NewRequest would concatenate onto the host as "hostkey/...".
	// Force it absolute.
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return p
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
		w := strings.TrimSpace(string(raw))
		if w == "" {
			// The user named a file explicitly; an empty/whitespace one is a
			// misconfiguration. Fail rather than silently falling through to
			// BB_WIF, which would fund the payment from an unintended key.
			return "", fmt.Errorf("--wif-file %s contains no WIF key", upgradeWIFFile)
		}
		return w, nil
	}
	if upgradeWIF != "" {
		w := strings.TrimSpace(upgradeWIF)
		if w == "" {
			return "", errors.New("--wif is set but empty")
		}
		return w, nil
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
func offerUpgradeOn402(ctx context.Context, errw io.Writer, err error) {
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
	if payErr := payChallenge(ctx, errw, wif, ch); payErr != nil {
		if !errors.Is(payErr, errDeclined) {
			fmt.Fprintln(errw, "upgrade failed:", payErr)
		}
		fmt.Fprint(errw, hint)
	}
}

var errDeclined = errors.New("declined")

// payChallenge settles an already-issued challenge with the funding key,
// mirroring `bb key upgrade` minus the challenge fetch.
func payChallenge(ctx context.Context, errw io.Writer, wif string, ch *x402.Challenge) error {
	if err := ch.VerifyPayee(); err != nil {
		return err
	}
	if err := ensureNotExpired(ch); err != nil {
		return err
	}
	store, err := proofStore()
	if err != nil {
		return err
	}
	saved, err := store.List()
	if err != nil {
		return err
	}
	if len(saved) > 0 {
		return fmt.Errorf("a payment proof from an earlier run is still pending (payment %s); run `bb key upgrade` to resubmit it instead of paying again", saved[0].Txid)
	}
	wallet, err := x402.NewWallet(wif, !upgradeTestnet)
	if err != nil {
		return err
	}
	fmt.Fprintf(errw, "  pay to:  %s\n", ch.PayeeDisplay())
	fmt.Fprintf(errw, "  from:    %s\n", wallet.Address)
	if !confirmIO(os.Stdin, errw, fmt.Sprintf("Pay %d sats now to upgrade to %q?", ch.AmountSats, ch.Tier)) {
		return errDeclined
	}
	c := client()
	res, err := payAndSettle(ctx, c, errw, store, wallet, ch)
	if err != nil {
		return err
	}
	fmt.Fprintf(errw, "upgrade settled: tier %q until %s (payment txid %s) — rerun your command\n",
		res.Tier, res.TierExpiresAt.Format(time.RFC3339), res.Txid)
	return nil
}

// payAndSettle builds and signs a payment for ch, saves its proof, and only
// then submits it, so a payment the server may broadcast is never lost.
func payAndSettle(ctx context.Context, c *api.Client, errw io.Writer, store *x402.ProofStore, wallet *x402.Wallet, ch *x402.Challenge) (*x402.UpgradeResult, error) {
	tx, err := wallet.BuildPayment(ctx, c, ch, upgradeFeeRate)
	if err != nil {
		return nil, err
	}
	txid := tx.TxID().String()
	sp, err := newSavedProof(c, ch, tx.Bytes(), txid)
	if err != nil {
		return nil, err
	}
	if err := store.Save(sp); err != nil {
		return nil, fmt.Errorf("saving the payment proof before submitting it (nothing was paid): %w", err)
	}
	fmt.Fprintf(errw, "built payment %s (%d bytes); proof saved to %s; submitting...\n", txid, tx.Size(), store.Path(sp.ChallengeID))
	return settleProof(ctx, c, errw, store, sp, upgradeWait)
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
	f.DurationVar(&upgradeWait, "wait", 2*time.Minute, "how long to keep resubmitting a pending payment proof before giving up (the proof stays saved)")
	f.BoolVar(&upgradeNewPayment, "new-payment", false, "discard saved pending payment proofs and pay a new challenge (only once you have confirmed they were not mined)")
	keyCmd.AddCommand(keyUsageCmd, keyUpgradeCmd)
	rootCmd.AddCommand(keyCmd)
}
