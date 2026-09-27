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
	"strconv"
	"strings"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
	"github.com/bsv-blockchain/go-sdk/transaction"
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
	upgradeTier    string
	upgradeWIF     string
	upgradeWIFFile string
	upgradeYes     bool
	upgradeDryRun  bool
	upgradeFeeRate uint64
	upgradeTestnet bool
	upgradeWait    time.Duration
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
Use --dry-run to fetch and inspect the challenge without paying.

The server can answer a submitted proof with 202 (pending): it broadcast the
payment but the network has not accepted it yet. That grants nothing. bb then
resubmits the SAME proof every Retry-After seconds until it settles, for up to
--wait (default 10m; --timeout still bounds each request). The proof is saved
to <user config dir>/bb/pending-upgrades.json (mode 0600) before the first
submit. If it is still pending when --wait runs out, or the run is
interrupted, rerun bb key upgrade: it resubmits the saved proof instead of
paying again, even after the challenge has expired.`,
	Example: `  bb key upgrade --dry-run
  bb key upgrade --tier pro --wif-file ~/.keys/funding.wif
  bb key upgrade --tier pro --wif-file ~/.keys/funding.wif --yes`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if apiKey() == "" {
			return errors.New("an API key is required: set --api-key or BB_API_KEY")
		}
		if upgradeWait < 0 {
			return errors.New("--wait must not be negative")
		}
		c := client()
		ctx := cmd.Context()
		errw := cmd.ErrOrStderr()

		store, err := openPendingStore()
		if err != nil {
			return err
		}
		fp := x402.KeyFingerprint(c.BaseURL, c.APIKey)
		saved, err := store.ForKey(fp)
		if err != nil {
			return err
		}
		// A saved proof means a payment may already be spent. Resume it and
		// never build another, whatever challenge the server would hand out
		// now: once the saved challenge expires, a proof-less request gets a
		// NEW challenge id, so matching on the id alone would pay twice.
		if len(saved) > 0 && !upgradeDryRun {
			return resumeSaved(cmd, c, store, saved)
		}
		for _, e := range saved {
			fmt.Fprintf(errw, "saved payment %s for challenge %s (tier %q) has not settled; a run without --dry-run resubmits it instead of paying\n",
				e.Txid, e.ChallengeID, e.Tier)
		}

		ch, err := fetchChallenge(cmd, c)
		if err != nil {
			return err
		}
		// The server re-serves an open challenge, and a pending payment keeps
		// its challenge open. A proof saved for this challenge id under another
		// fingerprint (the host or key spelled differently) is still ours: the
		// server only serves a key its own challenges. Resume it; never pay it
		// again.
		same, err := store.ForChallenge(ch.ChallengeID)
		if err != nil {
			return err
		}
		if len(same) > 0 {
			if !upgradeDryRun {
				return resumeSaved(cmd, c, store, same)
			}
			fmt.Fprintf(errw, "saved payment %s is for this challenge (%s) and has not settled; a run without --dry-run resubmits it instead of paying\n",
				same[0].Txid, ch.ChallengeID)
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

		tx, err := buildPayment(ctx, wallet, c, ch, upgradeFeeRate)
		if err != nil {
			return err
		}
		fmt.Fprintf(errw, "built payment %s (%d bytes); submitting proof...\n", tx.TxID().String(), tx.Size())

		e, err := savePending(store, fp, c.BaseURL, ch, tx.Bytes(), tx.TxID().String())
		if err != nil {
			return err
		}
		res, err := settleProof(ctx, c, errw, store, e, upgradeWait)
		if err != nil {
			return err
		}
		fmt.Fprintln(errw, settledLine(res))
		return render.JSON(cmd.OutOrStdout(), res)
	},
}

// resumeSaved resubmits the oldest of the given saved proofs instead of paying
// anything new. Resubmitting is valid even past the challenge's expires_at, so
// ensureNotExpired is deliberately not consulted here.
func resumeSaved(cmd *cobra.Command, c *api.Client, store *x402.PendingStore, saved []x402.PendingUpgrade) error {
	errw := cmd.ErrOrStderr()
	e := saved[0]
	fmt.Fprintf(errw, "resuming saved payment %s for challenge %s (tier %q, saved %s): resubmitting its proof; no new payment is made\n",
		e.Txid, e.ChallengeID, e.Tier, e.CreatedAt.Format(time.RFC3339))
	if upgradeTier != "" && upgradeTier != e.Tier {
		fmt.Fprintf(errw, "--tier %s waits until the saved payment for %q is resolved\n", upgradeTier, e.Tier)
	}
	res, err := settleProof(cmd.Context(), c, errw, store, &e, upgradeWait)
	if err != nil {
		return err
	}
	fmt.Fprintln(errw, settledLine(res))
	if n := len(saved) - 1; n > 0 {
		fmt.Fprintf(errw, "%d more saved payment(s) for this key; rerun `bb key upgrade` to resume them\n", n)
	}
	return render.JSON(cmd.OutOrStdout(), res)
}

// settledLine describes a settled upgrade. The expiry is unknown when the
// settlement was confirmed from the key's tier rather than a 200 body that
// carried one.
func settledLine(res *x402.UpgradeResult) string {
	until := "an unreported expiry"
	if !res.TierExpiresAt.IsZero() {
		until = res.TierExpiresAt.Format(time.RFC3339)
	}
	return fmt.Sprintf("upgrade settled: tier %q until %s (payment txid %s)", res.Tier, until, res.Txid)
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

// Seams for tests: building the payment (so a test can count or refuse it),
// the clock, and the wait between pending resubmits.
var (
	buildPayment = func(ctx context.Context, w *x402.Wallet, c *api.Client, ch *x402.Challenge, feeRate uint64) (*transaction.Transaction, error) {
		return w.BuildPayment(ctx, c, ch, feeRate)
	}
	nowFn    = time.Now
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			return nil
		}
	}
	pendingStorePath = x402.DefaultPendingPath
)

func openPendingStore() (*x402.PendingStore, error) {
	p, err := pendingStorePath()
	if err != nil {
		return nil, err
	}
	return &x402.PendingStore{Path: p}, nil
}

// savePending encodes the proof and writes it to the store BEFORE the first
// submit: the server broadcasts on that submit, so a crash, Ctrl-C or lost
// response afterwards must leave the proof on disk for the next run to
// resubmit. If it cannot be saved nothing is submitted, and since bb never
// broadcasts, nothing is paid.
func savePending(store *x402.PendingStore, fp, host string, ch *x402.Challenge, rawTx []byte, txid string) (*x402.PendingUpgrade, error) {
	hdr, err := x402.EncodeProofHeader(&x402.Proof{
		Version:     x402.Version,
		ChallengeID: ch.ChallengeID,
		RawTxBase64: base64.StdEncoding.EncodeToString(rawTx),
		Txid:        txid,
	})
	if err != nil {
		return nil, err
	}
	e := x402.PendingUpgrade{
		KeyFingerprint: fp,
		Host:           host,
		ChallengeID:    ch.ChallengeID,
		Tier:           ch.Tier,
		AmountSats:     ch.AmountSats,
		Txid:           txid,
		Proof:          hdr,
		PayURL:         ch.PayURL,
		CreatedAt:      nowFn().UTC(),
	}
	if err := store.Put(e); err != nil {
		return nil, fmt.Errorf("saving payment %s to %s before submitting it failed, so nothing was submitted or paid: %w", txid, store.Path, err)
	}
	return &e, nil
}

// pendingReply is a 202 from a proof submit: the server broadcast the payment
// but the network has not accepted it yet. It grants nothing.
type pendingReply struct {
	RetryAfter time.Duration
	Message    string
}

const (
	defaultPendingRetry = 10 * time.Second
	minPendingRetry     = time.Second
	maxPendingRetry     = 60 * time.Second
)

// parseRetryAfter reads a Retry-After of integer seconds, clamped to
// [minPendingRetry, maxPendingRetry]. Missing or unparsable values (including
// the HTTP-date form, which the upgrade endpoint does not send) wait
// defaultPendingRetry.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		if errors.Is(err, strconv.ErrRange) && !strings.HasPrefix(v, "-") {
			return maxPendingRetry
		}
		return defaultPendingRetry
	}
	switch {
	case n < int64(minPendingRetry/time.Second):
		return minPendingRetry
	case n > int64(maxPendingRetry/time.Second):
		return maxPendingRetry
	}
	return time.Duration(n) * time.Second
}

// submitProof POSTs one proof submit. It returns the settlement on 200, a
// pendingReply on 202, and otherwise the error (an *api.Error for non-2xx
// statuses, left for settleProof to classify).
func submitProof(ctx context.Context, c *api.Client, payURL, proofHdr string) (*x402.UpgradeResult, *pendingReply, error) {
	req, err := c.NewRequest(ctx, http.MethodPost, submitPath(payURL, c.BaseURL), nil, "", nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X402-Proof", proofHdr)
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	// Bound the body like the client's JSON/Bytes paths do, rather than decoding
	// straight from an unbounded network stream.
	raw, err := c.ReadBody(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	if resp.StatusCode == http.StatusAccepted {
		var body struct {
			Error string `json:"error"`
		}
		msg := "payment not yet accepted by the network"
		if json.Unmarshal(raw, &body) == nil && body.Error != "" {
			msg = body.Error
		}
		return nil, &pendingReply{RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After")), Message: msg}, nil
	}
	var res x402.UpgradeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, nil, fmt.Errorf("decoding upgrade result: %w", err)
	}
	// A 2xx body that unmarshals cleanly but carries no settlement data (e.g. an
	// empty object or an error payload) must not be reported as a settled
	// upgrade — require the fields that prove settlement.
	if res.Tier == "" && res.Txid == "" {
		return nil, nil, fmt.Errorf("upgrade response contained no settlement details: %s", strings.TrimSpace(string(raw)))
	}
	return &res, nil, nil
}

// settleProof submits a saved proof and, while the server answers 202,
// resubmits the identical proof after each Retry-After until it settles, a
// terminal answer arrives, ctx ends, or the next wait would overrun the wait
// budget. Every attempt is bounded by the client's per-request timeout on its
// own. The saved entry is removed only when the proof has settled or can no
// longer settle; on every other exit it stays for the next run to resume.
func settleProof(ctx context.Context, c *api.Client, errw io.Writer, store *x402.PendingStore, e *x402.PendingUpgrade, wait time.Duration) (*x402.UpgradeResult, error) {
	start := nowFn()
	deadline := start.Add(wait)
	for {
		prevLast, prevSubmit := e.LastSubmitAt, e.LastActivity()
		markSubmit(errw, store, e)
		res, pending, err := submitProof(ctx, c, e.PayURL, e.Proof)
		if err == nil && pending == nil {
			forgetPending(errw, store, e)
			return res, nil
		}
		if err != nil {
			var apiErr *api.Error
			if !errors.As(err, &apiErr) {
				return nil, fmt.Errorf("submitting payment %s: %w; %s", e.Txid, err, resumeHint(store))
			}
			switch {
			case isChallengeConsumed(apiErr):
				// The server answers "consumed" without settling anything, so
				// this submit cannot have settled the challenge. Un-record it:
				// left in place, every rerun the keep path asks for would
				// restart consumedTierGrace and the entry would never go.
				unmarkSubmit(errw, store, e, prevLast)
				return confirmConsumed(ctx, c, errw, store, e, prevSubmit)
			case apiErr.Status == http.StatusTooManyRequests:
				pending = &pendingReply{RetryAfter: parseRetryAfter(apiErr.Header.Get("Retry-After")), Message: apiErr.Message}
			case proofCannotSettle(apiErr):
				forgetPending(errw, store, e)
				return nil, upgradeError(apiErr)
			default:
				return nil, fmt.Errorf("submitting payment %s: %w; %s", e.Txid, upgradeError(apiErr), resumeHint(store))
			}
		}
		if nowFn().Add(pending.RetryAfter).After(deadline) {
			return nil, fmt.Errorf("payment %s is still not settled after waiting %s (%s); %s",
				e.Txid, nowFn().Sub(start).Round(time.Second), pending.Message, resumeHint(store))
		}
		fmt.Fprintf(errw, "payment %s not settled yet (%s); resubmitting the same proof in %s\n", e.Txid, pending.Message, pending.RetryAfter)
		if err := sleepCtx(ctx, pending.RetryAfter); err != nil {
			return nil, fmt.Errorf("stopped waiting for payment %s: %w; %s", e.Txid, err, resumeHint(store))
		}
	}
}

func resumeHint(store *x402.PendingStore) string {
	return fmt.Sprintf("your payment is saved in %s; rerun `bb key upgrade` to resume it — do not pay again", store.Path)
}

// markSubmit records on the saved entry that its proof is being sent now, so
// a later "challenge already consumed" can tell a settle that may still be
// propagating from a stale entry. Failing to record it is only a warning: the
// submit itself matters more.
func markSubmit(errw io.Writer, store *x402.PendingStore, e *x402.PendingUpgrade) {
	e.LastSubmitAt = nowFn().UTC()
	if err := store.Put(*e); err != nil {
		fmt.Fprintf(errw, "warning: could not record the submit of payment %s in %s: %v\n", e.Txid, store.Path, err)
	}
}

// unmarkSubmit restores the LastSubmitAt a submit that provably settled
// nothing overwrote. Failing to restore it is only a warning: the entry then
// waits one more consumedTierGrace before it can be dropped.
func unmarkSubmit(errw io.Writer, store *x402.PendingStore, e *x402.PendingUpgrade, prev time.Time) {
	e.LastSubmitAt = prev
	if err := store.Put(*e); err != nil {
		fmt.Fprintf(errw, "warning: could not restore the last submit time of payment %s in %s: %v\n", e.Txid, store.Path, err)
	}
}

// forgetPending drops a resolved entry. Failing to drop it is only a warning:
// the next run resubmits the proof, gets "challenge already consumed" (or the
// same terminal error), and resolves it then.
func forgetPending(errw io.Writer, store *x402.PendingStore, e *x402.PendingUpgrade) {
	if err := store.Delete(e.KeyFingerprint, e.ChallengeID); err != nil {
		fmt.Fprintf(errw, "warning: could not remove resolved payment %s from %s: %v\n", e.Txid, store.Path, err)
	}
}

func isChallengeConsumed(apiErr *api.Error) bool {
	return apiErr.Status == http.StatusConflict && strings.Contains(apiErr.Message, "challenge already consumed")
}

// proofCannotSettle reports the answers after which resubmitting the same
// proof can never succeed: malformed (400), wrong script or underpaid (402),
// unknown challenge (404 "challenge not found"), payment txid already used or
// key already at the tier (409, other than "challenge already consumed"),
// expired with no payment the network holds (410), and rejected by broadcast
// (422).
//
// A 404 counts only with the upgrade handler's own body. Any other 404 (a node
// where the route is not mounted answers a bare "404 page not found") says
// nothing about the proof, and dropping it there would let the next run pay
// the challenge again.
func proofCannotSettle(apiErr *api.Error) bool {
	switch apiErr.Status {
	case http.StatusNotFound:
		return isChallengeNotFound(apiErr)
	case http.StatusBadRequest, http.StatusPaymentRequired,
		http.StatusConflict, http.StatusGone, http.StatusUnprocessableEntity:
		return true
	}
	return false
}

func isChallengeNotFound(apiErr *api.Error) bool {
	return apiErr.Status == http.StatusNotFound && strings.Contains(apiErr.Message, "challenge not found")
}

// keyUsage is the part of GET /api/v1/key/usage the lost-200 check reads.
type keyUsage struct {
	Tier          string     `json:"tier"`
	TierExpiresAt *time.Time `json:"tier_expires_at"`
}

// consumedTierGrace is how long after its last earlier submit a saved proof
// whose challenge is consumed is kept while the key's tier does not show the
// purchase. A settle happens only on a submit, and a new tier takes about a
// minute to reach every server, so this covers a tier read that lags a fresh
// settle. Past it the entry can only be stale (the tier lapsed or was changed
// since), and since a consumed challenge never takes the proof again, keeping
// it would block every later upgrade.
const consumedTierGrace = time.Hour

// confirmConsumed handles "challenge already consumed" for a proof we saved:
// most likely an earlier submit of it settled and its 200 was lost. The key's
// tier decides. When the tier does not confirm it, the entry is kept if an
// earlier submit (prevSubmit, the latest time it could have settled) was
// within consumedTierGrace, because other servers behind the load balancer can
// report the old tier for a while after a settle; otherwise it is removed.
func confirmConsumed(ctx context.Context, c *api.Client, errw io.Writer, store *x402.PendingStore, e *x402.PendingUpgrade, prevSubmit time.Time) (*x402.UpgradeResult, error) {
	var u keyUsage
	if err := c.GetJSON(ctx, "/api/v1/key/usage", nil, &u); err != nil {
		return nil, fmt.Errorf("challenge %s is already consumed and reading the key's tier to confirm payment %s failed: %w; %s",
			e.ChallengeID, e.Txid, err, resumeHint(store))
	}
	if !tierAtLeast(u.Tier, e.Tier) {
		if age := nowFn().Sub(prevSubmit); age > consumedTierGrace {
			forgetPending(errw, store, e)
			return nil, fmt.Errorf("challenge %s is already consumed but the key is on tier %q, not %q; payment %s was last submitted %s ago and can never settle again, so it has been removed from %s — rerun `bb key upgrade` to buy a new upgrade",
				e.ChallengeID, u.Tier, e.Tier, e.Txid, age.Round(time.Minute), store.Path)
		}
		return nil, fmt.Errorf("challenge %s is already consumed but the key is on tier %q, not %q; a new tier can take a minute to show on every server — %s",
			e.ChallengeID, u.Tier, e.Tier, resumeHint(store))
	}
	fmt.Fprintf(errw, "challenge %s was already settled and the key is on tier %q: an earlier submit of payment %s went through\n",
		e.ChallengeID, u.Tier, e.Txid)
	forgetPending(errw, store, e)
	res := &x402.UpgradeResult{Tier: u.Tier, Txid: e.Txid, AmountSats: e.AmountSats}
	if u.TierExpiresAt != nil {
		res.TierExpiresAt = *u.TierExpiresAt
	}
	return res, nil
}

// tierRank orders the server's standard tiers. A tier outside it only matches
// itself.
var tierRank = map[string]int{"free": 0, "pro": 1, "enterprise": 2}

func tierAtLeast(current, target string) bool {
	if current == "" {
		return false
	}
	if current == target {
		return true
	}
	cr, okC := tierRank[current]
	tr, okT := tierRank[target]
	return okC && okT && cr >= tr
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

// upgradeError maps the server's x402 error statuses onto actionable messages.
func upgradeError(apiErr *api.Error) error {
	switch apiErr.Status {
	case http.StatusPaymentRequired:
		if ch, err := challengeFromResponse(apiErr); err == nil {
			return fmt.Errorf("payment insufficient: the challenge wants %d sats to %s (challenge still open — rerun to retry)",
				ch.AmountSats, ch.PayeeDisplay())
		}
		return fmt.Errorf("payment insufficient: %s", apiErr.Message)
	case http.StatusConflict:
		// No claim about whether the tx was broadcast: the server raises
		// "payment txid already used" and a settle-time "already at or above
		// this tier" AFTER broadcasting it.
		return fmt.Errorf("upgrade refused: %s", apiErr.Message)
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
	c := client()
	store, err := openPendingStore()
	if err != nil {
		return err
	}
	fp := x402.KeyFingerprint(c.BaseURL, c.APIKey)
	saved, err := store.ForKey(fp)
	if err != nil {
		return err
	}
	// A proof saved for this very challenge id belongs to this key whatever
	// fingerprint it was saved under (the server scopes challenges to keys).
	same, err := store.ForChallenge(ch.ChallengeID)
	if err != nil {
		return err
	}
	saved = append(saved, same...)
	// A saved proof may already have spent coins; `bb key upgrade` resumes
	// it. Never offer to pay again from here.
	if len(saved) > 0 {
		return fmt.Errorf("payment %s for this key (tier %q) has not settled yet; run `bb key upgrade` to resume it — do not pay again",
			saved[0].Txid, saved[0].Tier)
	}
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
	fmt.Fprintf(errw, "  pay to:  %s\n", ch.PayeeDisplay())
	fmt.Fprintf(errw, "  from:    %s\n", wallet.Address)
	if !confirmIO(os.Stdin, errw, fmt.Sprintf("Pay %d sats now to upgrade to %q?", ch.AmountSats, ch.Tier)) {
		return errDeclined
	}
	// Derive from the signal-aware context so Ctrl-C cancels the in-flight
	// UTXO fetch and proof submission, not just the process at exit. The
	// build gets the per-request timeout; the proof submits are bounded by
	// the wait budget instead, each attempt by the client's own timeout.
	buildCtx, cancel := context.WithTimeout(ctx, flagTimeout)
	defer cancel()
	tx, err := buildPayment(buildCtx, wallet, c, ch, upgradeFeeRate)
	if err != nil {
		return err
	}
	e, err := savePending(store, fp, c.BaseURL, ch, tx.Bytes(), tx.TxID().String())
	if err != nil {
		return err
	}
	res, err := settleProof(ctx, c, errw, store, e, upgradeWait)
	if err != nil {
		return err
	}
	fmt.Fprintln(errw, settledLine(res)+" — rerun your command")
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
	f.DurationVar(&upgradeWait, "wait", 10*time.Minute, "how long to keep resubmitting a payment the network has not accepted yet (separate from --timeout, which bounds each request)")
	keyCmd.AddCommand(keyUsageCmd, keyUpgradeCmd)
	rootCmd.AddCommand(keyCmd)
}
