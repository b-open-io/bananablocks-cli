package cmd

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
)

// x402 proof settlement.
//
// Once a payment proof has been submitted the server may have broadcast its
// transaction, and a second payment to the same challenge is never credited.
// So the client's only retry is to resubmit the SAME proof: it is saved to
// disk before the first submit, resubmitted on every answer the server
// documents as retryable (202 pending, 422 before expiry, 429, 5xx, transport
// failures), and a later `bb key upgrade` resubmits a still-saved proof
// instead of paying again.

const (
	defaultProofRetry = 10 * time.Second
	maxProofRetry     = 60 * time.Second
	// max422Retries bounds resubmits after "transaction rejected by
	// broadcast": the docs call it not always final, but a proof whose inputs
	// were spent elsewhere answers it forever.
	max422Retries = 3
)

// sleepCtx waits d or until ctx is done. A var so tests can skip real waits.
var sleepCtx = func(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// retryAfter parses a Retry-After header (seconds or HTTP date), falling back
// to def when absent or malformed and clamping to [0, maxProofRetry].
func retryAfter(h http.Header, def time.Duration) time.Duration {
	d := def
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			d = time.Duration(secs) * time.Second
		} else if at, err := http.ParseTime(v); err == nil {
			d = time.Until(at)
		}
	}
	if d < 0 {
		d = 0
	}
	if d > maxProofRetry {
		d = maxProofRetry
	}
	return d
}

// newSavedProof builds the record persisted before a proof's first submit.
func newSavedProof(c *api.Client, ch *x402.Challenge, rawTx []byte, txid string) (*x402.SavedProof, error) {
	hdr, err := x402.EncodeProofHeader(&x402.Proof{
		Version:     x402.Version,
		ChallengeID: ch.ChallengeID,
		RawTxBase64: base64.StdEncoding.EncodeToString(rawTx),
		Txid:        txid,
	})
	if err != nil {
		return nil, err
	}
	return &x402.SavedProof{
		Host:        c.BaseURL,
		ChallengeID: ch.ChallengeID,
		Tier:        ch.Tier,
		AmountSats:  ch.AmountSats,
		Txid:        txid,
		ProofHeader: hdr,
		SubmitPath:  submitPath(ch.PayURL, c.BaseURL),
		ExpiresAt:   ch.ExpiresAt,
		SavedAt:     time.Now().UTC(),
	}, nil
}

// proofStore is the pending-proof store for the current host and API key.
func proofStore() (*x402.ProofStore, error) {
	dir, err := x402.DefaultStateDir()
	if err != nil {
		return nil, fmt.Errorf("locating a state directory for payment proofs (set BB_STATE_DIR to an absolute path): %w", err)
	}
	return x402.NewProofStore(dir, apiKey()), nil
}

// settleProof submits sp's proof until the server settles it, gives a
// terminal answer, or the wait budget runs out. A proof that may still settle
// stays saved; one that never can is removed.
func settleProof(ctx context.Context, c *api.Client, errw io.Writer, store *x402.ProofStore, sp *x402.SavedProof, wait time.Duration) (*x402.UpgradeResult, error) {
	if sp.Host != "" && x402.CanonicalOrigin(sp.Host) != x402.CanonicalOrigin(c.BaseURL) {
		return nil, fmt.Errorf("the saved proof for payment %s (challenge %s) belongs to %s, not %s; rerun with --host %s to resubmit it. Do NOT pay again",
			sp.Txid, sp.ChallengeID, sp.Host, c.BaseURL, sp.Host)
	}
	deadline := time.Now().Add(wait)
	rejected := 0
	for {
		status, hdr, body, err := postProof(ctx, c, sp)
		var delay time.Duration
		var reason string
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return nil, pendingProofError(store, sp, "interrupted")
			}
			delay, reason = 5*time.Second, err.Error()
		case status == http.StatusAccepted:
			delay, reason = retryAfter(hdr, defaultProofRetry), "payment broadcast but not yet accepted by the network"
		case status >= 200 && status < 300:
			res, perr := parseUpgradeResult(body)
			if perr != nil {
				// A 2xx we cannot read may still be a settlement; keep the
				// proof so a rerun resubmits it rather than paying again.
				return nil, fmt.Errorf("%w; %s", perr, keepProofAdvice(store, sp))
			}
			if aerr := store.Archive(sp.ChallengeID, sp.Txid, "settled"); aerr != nil {
				fmt.Fprintf(errw, "warning: settled, but could not archive the saved proof %s: %v\n", store.Path(sp.ChallengeID), aerr)
			}
			return res, nil
		case status == http.StatusUnprocessableEntity:
			rejected++
			reason = serverMessage(body)
			if rejected > max422Retries || (!sp.ExpiresAt.IsZero() && time.Now().After(sp.ExpiresAt)) {
				return nil, pendingProofError(store, sp, "the broadcast of the payment transaction was rejected: "+reason)
			}
			delay = defaultProofRetry
		case status == http.StatusTooManyRequests || status >= 500:
			delay, reason = retryAfter(hdr, defaultProofRetry), fmt.Sprintf("HTTP %d: %s", status, serverMessage(body))
		default:
			return nil, terminalProofError(errw, store, sp, status, serverMessage(body))
		}
		if time.Now().Add(delay).After(deadline) {
			return nil, pendingProofError(store, sp, reason)
		}
		fmt.Fprintf(errw, "payment %s not settled yet (%s); resubmitting the same proof in %s...\n", sp.Txid, reason, delay)
		if sleepCtx(ctx, delay) != nil {
			return nil, pendingProofError(store, sp, "interrupted")
		}
	}
}

// postProof sends one proof submit and returns the raw answer. Transport
// failures come back as err; every HTTP answer, 2xx or not, as status.
func postProof(ctx context.Context, c *api.Client, sp *x402.SavedProof) (int, http.Header, []byte, error) {
	path := sp.SubmitPath
	if path == "" {
		path = x402.UpgradePath
	}
	req, err := c.NewRequest(ctx, http.MethodPost, path, nil, "", nil)
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("X402-Proof", sp.ProofHeader)
	// Never follow a redirect with a proof: it would carry the signed payment
	// (and the API key) to whatever the Location names, and let that answer
	// drive settlement. A 3xx comes back as a non-retryable answer that keeps
	// the proof pending.
	nc := *c
	hc := *c.HTTP
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	nc.HTTP = &hc
	resp, err := nc.Do(req)
	if err != nil {
		var apiErr *api.Error
		if errors.As(err, &apiErr) {
			return apiErr.Status, apiErr.Header, apiErr.Body, nil
		}
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := c.ReadBody(resp.Body)
	if err != nil {
		return 0, nil, nil, err
	}
	return resp.StatusCode, resp.Header, raw, nil
}

// parseUpgradeResult decodes a settlement body, requiring the fields that
// prove settlement.
func parseUpgradeResult(raw []byte) (*x402.UpgradeResult, error) {
	var res x402.UpgradeResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decoding upgrade result: %w", err)
	}
	if res.Tier == "" || res.Txid == "" {
		return nil, fmt.Errorf("upgrade response contained no settlement details: %s", strings.TrimSpace(string(raw)))
	}
	return &res, nil
}

func serverMessage(body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	if s := strings.TrimSpace(string(body)); s != "" {
		return s
	}
	return "no message"
}

func keepProofAdvice(store *x402.ProofStore, sp *x402.SavedProof) string {
	return fmt.Sprintf("the signed proof for payment %s is saved at %s. Do NOT pay again: rerun `bb key upgrade` to resubmit the same proof", sp.Txid, store.Path(sp.ChallengeID))
}

func pendingProofError(store *x402.ProofStore, sp *x402.SavedProof, reason string) error {
	return fmt.Errorf("payment %s for challenge %s is not settled yet (%s); %s. Check the transaction's status before you reuse its inputs",
		sp.Txid, sp.ChallengeID, reason, keepProofAdvice(store, sp))
}

// settleGrace is how long after a challenge's expires_at the server still
// settles an accepted payment to it.
const settleGrace = 24 * time.Hour

// terminalProofError maps an answer that automatic retries cannot change.
// Proofs that can never settle are archived; the rest stay pending so a later
// run resubmits them instead of paying again.
func terminalProofError(errw io.Writer, store *x402.ProofStore, sp *x402.SavedProof, status int, msg string) error {
	drop := func(reason string) {
		if err := store.Archive(sp.ChallengeID, sp.Txid, reason); err != nil {
			fmt.Fprintf(errw, "warning: could not archive the saved proof %s: %v\n", store.Path(sp.ChallengeID), err)
		}
	}
	lower := strings.ToLower(msg)
	switch {
	case status == http.StatusConflict && (strings.Contains(lower, "challenge already consumed") || strings.Contains(lower, "payment txid already used")):
		drop("server: " + msg)
		return fmt.Errorf("%s (payment %s): this proof will not be retried. It may already have settled even if no confirmation arrived; check `bb key usage` before paying again", msg, sp.Txid)
	case status == http.StatusConflict && (strings.Contains(lower, "payment request closed") || strings.Contains(lower, "payment already refused")):
		drop("server: " + msg)
		return fmt.Errorf("%s (payment %s): resubmitting it can never settle. Run `bb key upgrade` for a new challenge if you still want the upgrade; check the transaction's status before you reuse its inputs, and contact the operator if it is mined", msg, sp.Txid)
	case status == http.StatusConflict && strings.Contains(lower, "already on a tier"):
		return fmt.Errorf("%s (payment %s): no tier was granted. Check `bb key usage`, and ask the operator to check payment and refusal records before expecting a credit or refund. The proof stays saved at %s so a later `bb key upgrade` can resubmit it to reconcile; do not make another payment",
			msg, sp.Txid, store.Path(sp.ChallengeID))
	case status == http.StatusGone:
		// A 410 is not proof the payment is lost: within the grace window an
		// expired challenge's payment can still settle once the network
		// accepts it, so the proof stays pending until that window closes.
		if !sp.ExpiresAt.IsZero() && time.Now().After(sp.ExpiresAt.Add(settleGrace)) {
			drop("server: " + msg)
			return fmt.Errorf("%s (payment %s): the settlement window has closed. If the network accepted this payment, ask the operator about a refund or manual credit. Check the transaction's status before you reuse its inputs", msg, sp.Txid)
		}
		until := "24 hours after the challenge expired"
		if !sp.ExpiresAt.IsZero() {
			until = sp.ExpiresAt.Add(settleGrace).Format(time.RFC3339)
		}
		return fmt.Errorf("%s (payment %s): if the network later accepts this payment it can still settle until %s; the proof stays saved at %s, so rerun `bb key upgrade` later to resubmit it rather than paying again. Check the transaction's status before you reuse its inputs",
			msg, sp.Txid, until, store.Path(sp.ChallengeID))
	case status == http.StatusNotFound && strings.Contains(lower, "challenge not found"):
		// Only the protocol's own answer means the server has no such
		// challenge; a routing 404 (x402 temporarily disabled) keeps the
		// proof pending for a later resubmit.
		drop("server: " + msg)
		return fmt.Errorf("%s (payment %s): the server does not know this challenge for this API key; check the transaction's status before you reuse its inputs", msg, sp.Txid)
	case status == http.StatusPaymentRequired:
		return fmt.Errorf("%s (payment %s): the server did not accept this payment for the challenge. Do NOT pay the challenge again; a second payment is not credited. Check the transaction's status and contact the operator. The proof stays saved at %s (--new-payment discards it)",
			msg, sp.Txid, store.Path(sp.ChallengeID))
	default:
		return fmt.Errorf("HTTP %d: %s; %s", status, msg, keepProofAdvice(store, sp))
	}
}

// resubmitSaved resubmits every saved proof for this host and key instead of
// making a new payment. It returns the last settlement, or an error naming
// the proofs still unresolved.
func resubmitSaved(ctx context.Context, c *api.Client, errw io.Writer, store *x402.ProofStore, saved []*x402.SavedProof, wait time.Duration) (*x402.UpgradeResult, error) {
	var last *x402.UpgradeResult
	var errs []error
	for _, sp := range saved {
		fmt.Fprintf(errw, "resubmitting the saved proof for payment %s (challenge %s, tier %q); no new payment is made\n", sp.Txid, sp.ChallengeID, sp.Tier)
		res, err := settleProof(ctx, c, errw, store, sp, wait)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(errw, "upgrade settled: tier %q until %s (payment txid %s)\n", res.Tier, res.TierExpiresAt.Format(time.RFC3339), res.Txid)
		last = res
	}
	if len(errs) > 0 {
		return last, errors.Join(errs...)
	}
	return last, nil
}
