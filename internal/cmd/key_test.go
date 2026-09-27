package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

const testWIF = "KwDiBf89QgGbjEhKnhXJuH7LrciVrZi3qYjgd9M7rFU73sVHnoWn"

// TestMain points the saved-payment store at a throwaway directory so no test
// reads or writes the real user config dir.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "bb-cmd-test-")
	if err != nil {
		panic(err)
	}
	pendingStorePath = func() (string, error) { return filepath.Join(dir, "pending-upgrades.json"), nil }
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// useTempStore gives one test its own saved-payment store.
func useTempStore(t *testing.T) *x402.PendingStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bb", "pending-upgrades.json")
	old := pendingStorePath
	pendingStorePath = func() (string, error) { return path, nil }
	t.Cleanup(func() { pendingStorePath = old })
	return &x402.PendingStore{Path: path}
}

// newX402Server mimics the parent server's /api/v1/key/upgrade contract:
// proof-less POST → 402 challenge (header + body); POST with X402-Proof →
// decode exactly like service.DecodeProofHeader, verify the payment output,
// and settle. It also serves the UTXOs the wallet funds the payment from.
func newX402Server(t *testing.T, wallet *x402.Wallet, ch *x402.Challenge) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/utxos"):
			json.NewEncoder(rw).Encode([]map[string]any{
				{"txid": strings.Repeat("44", 32), "vout": 0, "value": int64(100000), "script_type": "p2pkh"},
			})

		case r.URL.Path == x402.UpgradePath && r.Method == http.MethodPost:
			proofHdr := strings.TrimSpace(r.Header.Get("X402-Proof"))
			if proofHdr == "" {
				if got := r.URL.Query().Get("tier"); got != ch.Tier {
					t.Errorf("challenge requested for tier %q, want %q", got, ch.Tier)
				}
				raw, _ := json.Marshal(ch)
				rw.Header().Set("X402-Challenge", base64.RawURLEncoding.EncodeToString(raw))
				rw.Header().Set("Content-Type", "application/json")
				rw.WriteHeader(http.StatusPaymentRequired)
				json.NewEncoder(rw).Encode(map[string]any{"error": "payment required", "challenge": ch})
				return
			}
			// Decode as the server does.
			raw, err := base64.RawURLEncoding.DecodeString(proofHdr)
			if err != nil {
				t.Errorf("proof header is not raw base64url: %v", err)
				http.Error(rw, `{"error":"malformed X402-Proof header"}`, http.StatusBadRequest)
				return
			}
			var proof x402.Proof
			if err := json.Unmarshal(raw, &proof); err != nil || proof.ChallengeID != ch.ChallengeID {
				http.Error(rw, `{"error":"challenge not found"}`, http.StatusNotFound)
				return
			}
			rawTx, err := base64.StdEncoding.DecodeString(proof.RawTxBase64)
			if err != nil {
				http.Error(rw, `{"error":"malformed payment proof"}`, http.StatusBadRequest)
				return
			}
			tx, err := transaction.NewTransactionFromBytes(rawTx)
			if err != nil {
				http.Error(rw, `{"error":"malformed payment proof"}`, http.StatusBadRequest)
				return
			}
			var paid int64
			for _, out := range tx.Outputs {
				if out.LockingScript.String() == ch.PayeeLockingScriptHex {
					paid += int64(out.Satoshis)
				}
			}
			if paid < ch.AmountSats {
				http.Error(rw, `{"error":"payment insufficient"}`, http.StatusPaymentRequired)
				return
			}
			json.NewEncoder(rw).Encode(x402.UpgradeResult{
				Tier:          ch.Tier,
				TierExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC(),
				Txid:          tx.TxID().String(),
				AmountSats:    ch.AmountSats,
			})

		default:
			http.NotFound(rw, r)
		}
	}))
}

// TestUpgradeFlow drives challenge fetch → payment build → proof submit
// against a mock server speaking the parent repo's exact wire contract.
func TestUpgradeFlow(t *testing.T) {
	wallet, err := x402.NewWallet(testWIF, true)
	if err != nil {
		t.Fatal(err)
	}
	ch := &x402.Challenge{
		Version:               x402.Version,
		ChallengeID:           "ch-test-1",
		Tier:                  "pro",
		DurationDays:          30,
		AmountSats:            50000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
		PayeeAddress:          "1111111111111111111114oLvT2",
		ExpiresAt:             time.Now().Add(15 * time.Minute).UTC(),
		PayURL:                x402.UpgradePath,
	}
	srv := newX402Server(t, wallet, ch)
	defer srv.Close()

	c := &api.Client{BaseURL: srv.URL, APIKey: "bb_live_test", HTTP: srv.Client()}

	// Challenge fetch (mirrors fetchChallenge without the cobra plumbing).
	upgradeTier = ch.Tier
	defer func() { upgradeTier = "" }()
	err = c.JSON(context.Background(), http.MethodPost, x402.UpgradePath+"?tier="+ch.Tier, nil, "", nil, nil)
	apiErr, ok := err.(*api.Error)
	if !ok || apiErr.Status != http.StatusPaymentRequired {
		t.Fatalf("expected a 402, got %v", err)
	}
	got, err := challengeFromResponse(apiErr)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChallengeID != ch.ChallengeID || got.AmountSats != ch.AmountSats {
		t.Fatalf("challenge mismatch: %+v", got)
	}

	tx, err := wallet.BuildPayment(context.Background(), c, got, 1)
	if err != nil {
		t.Fatal(err)
	}

	store := useTempStore(t)
	e, err := savePending(store, x402.KeyFingerprint(c.BaseURL, c.APIKey), c.BaseURL, got, tx.Bytes(), tx.TxID().String())
	if err != nil {
		t.Fatal(err)
	}
	res, err := settleProof(context.Background(), c, io.Discard, store, e, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != ch.Tier || res.Txid != tx.TxID().String() {
		t.Fatalf("unexpected settle result: %+v", res)
	}
}

// TestChallengeFromResponseHeaderFallback drops the body challenge and makes
// sure the X402-Challenge header path still yields it.
func TestChallengeFromResponseHeaderFallback(t *testing.T) {
	ch := x402.Challenge{ChallengeID: "hdr-only", AmountSats: 123}
	raw, _ := json.Marshal(ch)
	hdr := http.Header{}
	hdr.Set("X402-Challenge", base64.RawURLEncoding.EncodeToString(raw))
	apiErr := &api.Error{
		Status: http.StatusPaymentRequired,
		Header: hdr,
		Body:   []byte(`{"error":"payment required"}`),
	}
	got, err := challengeFromResponse(apiErr)
	if err != nil {
		t.Fatal(err)
	}
	if got.ChallengeID != "hdr-only" {
		t.Fatalf("got %+v", got)
	}
}

// TestUpgradeErrorMapping checks the status → message taxonomy.
func TestUpgradeErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		want   string
	}{
		{http.StatusConflict, "payment txid already used"},
		{http.StatusGone, "expired"},
		{http.StatusUnprocessableEntity, "rejected"},
	}
	for _, tc := range cases {
		err := upgradeError(&api.Error{Status: tc.status, Message: "payment txid already used"})
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d → %q, want it to mention %q", tc.status, err, tc.want)
		}
	}
	// The server raises a replay 409 after broadcasting the tx, so the message
	// must not tell the payer it was not broadcast.
	if err := upgradeError(&api.Error{Status: http.StatusConflict, Message: "payment txid already used"}); strings.Contains(err.Error(), "not broadcast") {
		t.Errorf("409 message claims the tx was not broadcast: %q", err)
	}
}

// TestOfferUpgradeOn402 covers the rate-limit 402 → offer path: terms plus a
// `bb key upgrade` hint when no funding key is at hand, and silence on
// non-402 errors or challenge-less (anonymous) 402s.
func TestOfferUpgradeOn402(t *testing.T) {
	t.Setenv("BB_WIF", "")
	ch := x402.Challenge{ChallengeID: "rl-1", Tier: "pro", DurationDays: 30, AmountSats: 100000}
	body, _ := json.Marshal(map[string]any{"error": "rate limit exceeded", "challenge": ch})

	var buf bytes.Buffer
	offerUpgradeOn402(context.Background(), &buf, &api.Error{Status: http.StatusPaymentRequired, Body: body})
	out := buf.String()
	if !strings.Contains(out, "100000 sats") || !strings.Contains(out, "bb key upgrade --tier pro") {
		t.Fatalf("offer output missing terms or hint: %q", out)
	}

	buf.Reset()
	offerUpgradeOn402(context.Background(), &buf, &api.Error{Status: http.StatusTooManyRequests, Body: []byte(`{"error":"slow down"}`)})
	if buf.Len() != 0 {
		t.Fatalf("non-402 must not offer, got %q", buf.String())
	}

	buf.Reset()
	offerUpgradeOn402(context.Background(), &buf, &api.Error{Status: http.StatusPaymentRequired, Body: []byte(`{"error":"anonymous"}`)})
	if buf.Len() != 0 {
		t.Fatalf("challenge-less 402 must not offer, got %q", buf.String())
	}
}

// TestPayChallengeSettlesInline drives the rate-limit offer's inline payment
// (confirm → build → proof submit) against the mock x402 server.
func TestPayChallengeSettlesInline(t *testing.T) {
	wallet, err := x402.NewWallet(testWIF, true)
	if err != nil {
		t.Fatal(err)
	}
	ch := &x402.Challenge{
		Version:               x402.Version,
		ChallengeID:           "ch-inline-1",
		Tier:                  "pro",
		DurationDays:          30,
		AmountSats:            50000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
		PayeeAddress:          "1111111111111111111114oLvT2",
		ExpiresAt:             time.Now().Add(15 * time.Minute).UTC(),
		PayURL:                x402.UpgradePath,
	}
	srv := newX402Server(t, wallet, ch)
	defer srv.Close()

	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = srv.URL, "bb_live_test"
	defer func() { flagHost, flagAPIKey = oldHost, oldKey }()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("y\n")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()

	var buf bytes.Buffer
	if err := payChallenge(context.Background(), &buf, testWIF, ch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "upgrade settled") {
		t.Fatalf("missing settle notice: %q", buf.String())
	}
}

// TestEnsureNotExpired confirms an already-expired challenge is rejected before
// any signing/submission, while a still-valid (or expiry-less) one passes.
func TestEnsureNotExpired(t *testing.T) {
	expired := &x402.Challenge{ExpiresAt: time.Now().Add(-time.Minute)}
	if err := ensureNotExpired(expired); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired challenge should be rejected, got %v", err)
	}
	if err := ensureNotExpired(&x402.Challenge{ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatalf("valid challenge should pass, got %v", err)
	}
	if err := ensureNotExpired(&x402.Challenge{}); err != nil {
		t.Fatalf("zero-expiry challenge should pass, got %v", err)
	}
}

// TestSubmitPath covers pay_url resolution: empty falls back to the canonical
// path, same-host relative/absolute URLs (with query) are honored, and a
// cross-origin absolute URL is refused so the Bearer key can't leak.
func TestSubmitPath(t *testing.T) {
	const base = "https://bananablocks.com"
	cases := []struct {
		payURL string
		want   string
	}{
		{"", x402.UpgradePath},
		{"/api/v1/key/upgrade", "/api/v1/key/upgrade"},
		{"/api/v2/pay?cid=abc123", "/api/v2/pay?cid=abc123"},
		{"https://bananablocks.com/api/v2/pay?cid=abc", "/api/v2/pay?cid=abc"},
		{"https://evil.example/steal", x402.UpgradePath}, // cross-origin → refused
		{"://nonsense", x402.UpgradePath},
	}
	for _, tc := range cases {
		if got := submitPath(tc.payURL, base); got != tc.want {
			t.Errorf("submitPath(%q) = %q, want %q", tc.payURL, got, tc.want)
		}
	}
}

// TestFundingWIF covers key-source precedence: --wif-file beats --wif beats
// BB_WIF, and a missing source errors mentioning all three.
func TestFundingWIF(t *testing.T) {
	reset := func() { upgradeWIF, upgradeWIFFile = "", "" }
	t.Cleanup(reset)

	file := filepath.Join(t.TempDir(), "f.wif")
	if err := os.WriteFile(file, []byte("  file-wif\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// --wif-file wins over --wif.
	reset()
	t.Setenv("BB_WIF", "env-wif")
	upgradeWIFFile, upgradeWIF = file, "flag-wif"
	if got, err := fundingWIF(); err != nil || got != "file-wif" {
		t.Fatalf("file should win: got %q, %v", got, err)
	}

	// --wif beats env.
	reset()
	upgradeWIF = "flag-wif"
	if got, err := fundingWIF(); err != nil || got != "flag-wif" {
		t.Fatalf("flag should beat env: got %q, %v", got, err)
	}

	// env as last resort.
	reset()
	if got, err := fundingWIF(); err != nil || got != "env-wif" {
		t.Fatalf("env fallback: got %q, %v", got, err)
	}

	// nothing set → error naming every source.
	reset()
	t.Setenv("BB_WIF", "")
	_, err := fundingWIF()
	if err == nil || !strings.Contains(err.Error(), "--wif-file") || !strings.Contains(err.Error(), "BB_WIF") {
		t.Fatalf("missing-source error should name all sources, got %v", err)
	}

	// fundingWIFExplicit ignores env entirely.
	reset()
	t.Setenv("BB_WIF", "env-wif")
	if got, err := fundingWIFExplicit(); err != nil || got != "" {
		t.Fatalf("explicit must ignore BB_WIF: got %q, %v", got, err)
	}
}

// TestOfferUpgradeEnvWIFNoInline verifies an inherited BB_WIF does not trigger
// an inline payment offer — the hint is printed instead.
func TestOfferUpgradeEnvWIFNoInline(t *testing.T) {
	upgradeWIF, upgradeWIFFile = "", ""
	t.Setenv("BB_WIF", "env-wif")
	ch := x402.Challenge{ChallengeID: "rl", Tier: "pro", DurationDays: 30, AmountSats: 5_000_000}
	body, _ := json.Marshal(map[string]any{"error": "rate limit", "challenge": ch})

	var buf bytes.Buffer
	offerUpgradeOn402(context.Background(), &buf, &api.Error{Status: http.StatusPaymentRequired, Body: body})
	out := buf.String()
	if !strings.Contains(out, "bb key upgrade --tier pro") {
		t.Fatalf("env-only WIF should print the hint, got %q", out)
	}
	if strings.Contains(out, "Pay ") {
		t.Fatalf("env-only WIF must not prompt to pay inline, got %q", out)
	}
}

// TestConfirmReadsStdin covers the interactive prompt's yes/no parsing.
func TestConfirmReadsStdin(t *testing.T) {
	cmd := keyUpgradeCmd
	var errBuf bytes.Buffer
	cmd.SetErr(&errBuf)
	cmd.SetIn(strings.NewReader("y\n"))
	if !confirm(cmd, "pay?") {
		t.Fatal(`"y" should confirm`)
	}
	cmd.SetIn(strings.NewReader("\n"))
	if confirm(cmd, "pay?") {
		t.Fatal("empty answer must NOT confirm")
	}
}

// TestChallengeFromResponseHeaderRejectsEmptyID checks that the X402-Challenge
// header fallback rejects a challenge carrying no challenge_id (the JSON-body
// path already does), so bb never builds a payment the server cannot settle.
func TestChallengeFromResponseHeaderRejectsEmptyID(t *testing.T) {
	ch := x402.Challenge{AmountSats: 123} // no ChallengeID
	raw, _ := json.Marshal(ch)
	hdr := http.Header{}
	hdr.Set("X402-Challenge", base64.RawURLEncoding.EncodeToString(raw))
	apiErr := &api.Error{Status: http.StatusPaymentRequired, Header: hdr, Body: []byte(`{}`)}
	if _, err := challengeFromResponse(apiErr); err == nil {
		t.Fatal("a header challenge with no challenge_id must be rejected")
	}
}

// TestFundingWIFExplicitEmptyFile checks that an explicitly named but empty
// --wif-file errors rather than silently falling through to BB_WIF, which would
// fund the payment from an unintended key.
func TestFundingWIFExplicitEmptyFile(t *testing.T) {
	defer func(f, w string) { upgradeWIFFile, upgradeWIF = f, w }(upgradeWIFFile, upgradeWIF)
	upgradeWIF = ""
	f := filepath.Join(t.TempDir(), "empty.wif")
	if err := os.WriteFile(f, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	upgradeWIFFile = f
	if _, err := fundingWIFExplicit(); err == nil {
		t.Fatal("an empty --wif-file must error, not resolve to an empty key")
	}
}

// --- x402 202 pending, saved-proof resume, lost-200 (OPL-5278) ---

// proofReply is one canned answer to a proof submit.
type proofReply struct {
	status     int
	retryAfter string // Retry-After header, "" = none
	body       string // "" = a 200 settlement for the submitted txid
	ctype      string // Content-Type, "" = application/json
	hangup     bool   // close the connection without answering
	truncate   bool   // start a 200 and drop the connection mid-body
}

// fakeUpgradeServer speaks the server's upgrade contract (bananablocks
// internal/server/x402.go): a proof-less POST gets a 402 challenge, a proof
// submit gets the next canned reply (the last one repeats), and
// /api/v1/key/usage reports usageTier.
type fakeUpgradeServer struct {
	t         *testing.T
	mu        sync.Mutex
	ch        *x402.Challenge
	replies   []proofReply
	usageTier string

	proofs           []string
	challengeFetches int
	onProof          func(proof string)
}

func (f *fakeUpgradeServer) setReplies(r ...proofReply) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replies = r
}

func (f *fakeUpgradeServer) snapshot() ([]string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.proofs...), f.challengeFetches
}

func (f *fakeUpgradeServer) start() *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.HasSuffix(r.URL.Path, "/utxos"):
			json.NewEncoder(rw).Encode([]map[string]any{
				{"txid": strings.Repeat("44", 32), "vout": 0, "value": int64(100000), "script_type": "p2pkh"},
			})
		case r.URL.Path == "/api/v1/key/usage":
			json.NewEncoder(rw).Encode(map[string]any{"key_prefix": "bb_live_te", "tier": f.usageTier, "tier_expires_at": "2026-10-26T00:00:00Z"})
		case r.URL.Path == x402.UpgradePath && r.Method == http.MethodPost:
			proof := r.Header.Get("X402-Proof")
			if proof == "" {
				f.challengeFetches++
				rw.Header().Set("Content-Type", "application/json")
				rw.WriteHeader(http.StatusPaymentRequired)
				json.NewEncoder(rw).Encode(map[string]any{"error": "payment required", "challenge": f.ch})
				return
			}
			if f.onProof != nil {
				f.onProof(proof)
			}
			f.proofs = append(f.proofs, proof)
			i := len(f.proofs) - 1
			if i >= len(f.replies) {
				i = len(f.replies) - 1
			}
			rep := f.replies[i]
			if rep.hangup {
				conn, _, err := rw.(http.Hijacker).Hijack()
				if err != nil {
					f.t.Errorf("hijacking the proof submit: %v", err)
					return
				}
				conn.Close()
				return
			}
			if rep.truncate {
				rw.Header().Set("Content-Length", "1000")
				rw.WriteHeader(http.StatusOK)
				io.WriteString(rw, `{"tier":`)
				rw.(http.Flusher).Flush()
				panic(http.ErrAbortHandler) // the server drops the connection; the deferred Unlock still runs
			}
			if rep.retryAfter != "" {
				rw.Header().Set("Retry-After", rep.retryAfter)
			}
			if rep.status == http.StatusOK && rep.body == "" {
				json.NewEncoder(rw).Encode(x402.UpgradeResult{
					Tier: f.ch.Tier, TierExpiresAt: time.Now().Add(30 * 24 * time.Hour).UTC(),
					Txid: proofTxid(f.t, proof), AmountSats: f.ch.AmountSats,
				})
				return
			}
			ctype := rep.ctype
			if ctype == "" {
				ctype = "application/json"
			}
			rw.Header().Set("Content-Type", ctype)
			rw.WriteHeader(rep.status)
			io.WriteString(rw, rep.body)
		default:
			http.NotFound(rw, r)
		}
	}))
	f.t.Cleanup(srv.Close)
	return srv
}

// proofTxid pulls the txid out of an X402-Proof header ("" for a fake one).
func proofTxid(t *testing.T, hdr string) string {
	raw, err := base64.RawURLEncoding.DecodeString(hdr)
	if err != nil {
		return ""
	}
	var p x402.Proof
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.Txid
}

const pendingBody = `{"status":"pending","error":"payment broadcast but not yet accepted by the network; resubmit the same proof"}`

func testChallenge(id string) *x402.Challenge {
	return &x402.Challenge{
		Version:               x402.Version,
		ChallengeID:           id,
		Tier:                  "pro",
		DurationDays:          30,
		AmountSats:            50000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
		PayeeAddress:          "1111111111111111111114oLvT2",
		ExpiresAt:             time.Now().Add(10 * time.Minute).UTC(),
		PayURL:                x402.UpgradePath,
	}
}

// fakeClock replaces nowFn/sleepCtx: sleeping advances the clock instantly
// and records each wait.
func fakeClock(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	oldNow, oldSleep := nowFn, sleepCtx
	nowFn = func() time.Time { return now }
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		slept = append(slept, d)
		now = now.Add(d)
		return nil
	}
	t.Cleanup(func() { nowFn, sleepCtx = oldNow, oldSleep })
	return &slept
}

// countBuilds wraps buildPayment and counts real payment builds.
func countBuilds(t *testing.T) *int {
	t.Helper()
	n := 0
	old := buildPayment
	buildPayment = func(ctx context.Context, w *x402.Wallet, c *api.Client, ch *x402.Challenge, feeRate uint64) (*transaction.Transaction, error) {
		n++
		return old(ctx, w, c, ch, feeRate)
	}
	t.Cleanup(func() { buildPayment = old })
	return &n
}

// runUpgrade runs `bb key upgrade --tier pro --wif <test> --yes --wait <wait>`
// against srvURL and returns stdout, stderr and the error.
func runUpgrade(t *testing.T, ctx context.Context, srvURL string, wait time.Duration) (string, string, error) {
	t.Helper()
	return runUpgradeMode(t, ctx, srvURL, wait, false)
}

// runUpgradeMode is runUpgrade with --dry-run set to dryRun.
func runUpgradeMode(t *testing.T, ctx context.Context, srvURL string, wait time.Duration, dryRun bool) (string, string, error) {
	t.Helper()
	oldHost, oldKey := flagHost, flagAPIKey
	oldTier, oldWIF, oldWIFFile, oldYes, oldDry, oldWait := upgradeTier, upgradeWIF, upgradeWIFFile, upgradeYes, upgradeDryRun, upgradeWait
	t.Cleanup(func() {
		flagHost, flagAPIKey = oldHost, oldKey
		upgradeTier, upgradeWIF, upgradeWIFFile, upgradeYes, upgradeDryRun, upgradeWait = oldTier, oldWIF, oldWIFFile, oldYes, oldDry, oldWait
	})
	flagHost, flagAPIKey = srvURL, "bb_live_test"
	upgradeTier, upgradeWIF, upgradeWIFFile, upgradeYes, upgradeDryRun, upgradeWait = "pro", testWIF, "", true, dryRun, wait

	var out, errb bytes.Buffer
	cmd := keyUpgradeCmd
	cmd.SetContext(ctx)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err := cmd.RunE(cmd, nil)
	return out.String(), errb.String(), err
}

// seedSaved stores a pending proof for the test key on srvURL, as a previous
// run that got a 202 would have, saved at nowFn().
func seedSaved(t *testing.T, store *x402.PendingStore, srvURL, challengeID, proof string) {
	t.Helper()
	seedSavedAt(t, store, srvURL, challengeID, proof, nowFn().UTC())
}

func seedSavedAt(t *testing.T, store *x402.PendingStore, srvURL, challengeID, proof string, created time.Time) {
	t.Helper()
	err := store.Put(x402.PendingUpgrade{
		KeyFingerprint: x402.KeyFingerprint(srvURL, "bb_live_test"),
		Host:           srvURL,
		ChallengeID:    challengeID,
		Tier:           "pro",
		AmountSats:     50000,
		Txid:           "aa" + strings.Repeat("0", 62),
		Proof:          proof,
		PayURL:         x402.UpgradePath,
		CreatedAt:      created,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func savedFor(t *testing.T, store *x402.PendingStore, srvURL string) []x402.PendingUpgrade {
	t.Helper()
	got, err := store.ForKey(x402.KeyFingerprint(srvURL, "bb_live_test"))
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// 202, 202, 200: the same proof is resubmitted after each Retry-After, the
// proof is on disk before the first submit, and it is removed once settled.
func TestUpgradePendingThenSettles(t *testing.T) {
	store := useTempStore(t)
	slept := fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-pend-1")}
	f.setReplies(
		proofReply{status: http.StatusAccepted, retryAfter: "7", body: pendingBody},
		proofReply{status: http.StatusAccepted, body: pendingBody}, // no Retry-After → 10s
		proofReply{status: http.StatusOK},
	)
	srv := f.start()
	savedBeforeSubmit := 0
	f.onProof = func(proof string) {
		for _, e := range savedFor(t, store, srv.URL) {
			if e.Proof == proof {
				savedBeforeSubmit++
			}
		}
	}

	out, errOut, err := runUpgrade(t, context.Background(), srv.URL, 10*time.Minute)
	if err != nil {
		t.Fatalf("upgrade failed: %v\nstderr: %s", err, errOut)
	}
	proofs, _ := f.snapshot()
	if len(proofs) != 3 {
		t.Fatalf("want 3 proof submits, got %d", len(proofs))
	}
	for _, p := range proofs[1:] {
		if p != proofs[0] {
			t.Fatal("a resubmit carried a different X402-Proof; it must resend the identical proof")
		}
	}
	if savedBeforeSubmit != 3 {
		t.Fatalf("the proof must be saved before every submit, was saved for %d of 3", savedBeforeSubmit)
	}
	if want := []time.Duration{7 * time.Second, 10 * time.Second}; !equalDurations(*slept, want) {
		t.Fatalf("waits = %v, want %v", *slept, want)
	}
	if *builds != 1 {
		t.Fatalf("built %d payments, want 1", *builds)
	}
	if n := strings.Count(errOut, "not settled yet"); n != 2 {
		t.Fatalf("want one progress line per 202, got %d in %q", n, errOut)
	}
	if !strings.Contains(out, `"tier": "pro"`) {
		t.Fatalf("stdout lacks the settlement: %q", out)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

func equalDurations(a, b []time.Duration) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Pending past the wait budget: exit with a resume message and keep the
// proof. The rerun resubmits it without paying again, even though the server
// now hands out a different challenge id (the saved one expired, and the
// server re-serves only an unexpired challenge).
func TestUpgradePendingBudgetThenResume(t *testing.T) {
	store := useTempStore(t)
	slept := fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-budget-1")}
	f.setReplies(proofReply{status: http.StatusAccepted, retryAfter: "10", body: pendingBody})
	srv := f.start()

	_, _, err := runUpgrade(t, context.Background(), srv.URL, 25*time.Second)
	if err == nil {
		t.Fatal("a payment still pending past --wait must exit non-zero")
	}
	for _, want := range []string{"still not settled", "rerun `bb key upgrade`", "do not pay again"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	proofs, fetches := f.snapshot()
	if len(proofs) != 3 || !equalDurations(*slept, []time.Duration{10 * time.Second, 10 * time.Second}) {
		t.Fatalf("want submits at 0s/10s/20s within a 25s budget, got %d submits, waits %v", len(proofs), *slept)
	}
	saved := savedFor(t, store, srv.URL)
	if len(saved) != 1 || saved[0].Proof != proofs[0] || saved[0].ChallengeID != "ch-budget-1" {
		t.Fatalf("the pending proof must stay saved, got %+v", saved)
	}
	if !strings.Contains(err.Error(), saved[0].Txid) {
		t.Fatalf("error %q should name the payment txid %s", err, saved[0].Txid)
	}

	// Rerun: the server now issues a fresh challenge and accepts the payment.
	f.mu.Lock()
	f.ch = testChallenge("ch-budget-2")
	f.mu.Unlock()
	f.setReplies(proofReply{status: http.StatusOK})
	_, errOut, err := runUpgrade(t, context.Background(), srv.URL, 25*time.Second)
	if err != nil {
		t.Fatalf("resume failed: %v\nstderr: %s", err, errOut)
	}
	if *builds != 1 {
		t.Fatalf("resume built a second payment (%d builds)", *builds)
	}
	proofs2, fetches2 := f.snapshot()
	if len(proofs2) != 4 || proofs2[3] != proofs[0] {
		t.Fatal("resume must resubmit the saved proof")
	}
	if fetches2 != fetches {
		t.Fatalf("resume fetched a challenge (%d → %d); it needs none", fetches, fetches2)
	}
	if !strings.Contains(errOut, "resuming saved payment") {
		t.Fatalf("resume should say so, stderr: %q", errOut)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// The spec's rerun case: the server re-serves the SAME (now expired)
// challenge id. The saved proof is resubmitted and no payment is built. The
// proof is found by key fingerprint, which resumes before any challenge is
// fetched, so this test cannot see ensureNotExpired;
// TestUpgradeResumesSavedProofByChallengeID covers the expiry on the path
// that fetches the challenge first.
func TestUpgradeResumesSavedProofForSameChallenge(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	old := buildPayment
	buildPayment = func(context.Context, *x402.Wallet, *api.Client, *x402.Challenge, uint64) (*transaction.Transaction, error) {
		t.Error("BuildPayment called while a saved proof exists")
		return nil, errors.New("must not build")
	}
	t.Cleanup(func() { buildPayment = old })

	ch := testChallenge("ch-same-1")
	ch.ExpiresAt = time.Now().Add(-time.Minute).UTC()
	f := &fakeUpgradeServer{t: t, ch: ch}
	f.setReplies(proofReply{status: http.StatusOK})
	srv := f.start()
	seedSaved(t, store, srv.URL, "ch-same-1", "saved-proof-header")

	_, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err != nil {
		t.Fatalf("resume failed: %v\nstderr: %s", err, errOut)
	}
	proofs, _ := f.snapshot()
	if len(proofs) != 1 || proofs[0] != "saved-proof-header" {
		t.Fatalf("want the saved proof resubmitted once, got %q", proofs)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// 409 "challenge already consumed" for our saved proof: the 200 was lost.
// The key's tier decides; an unconfirmed tier keeps the proof for a rerun.
func TestUpgradeConsumedChecksTier(t *testing.T) {
	cases := []struct {
		usageTier string
		wantOK    bool
	}{
		{"pro", true},
		{"enterprise", true},
		{"free", false},
		{"", false},
	}
	for _, tc := range cases {
		t.Run("tier="+tc.usageTier, func(t *testing.T) {
			store := useTempStore(t)
			fakeClock(t)
			builds := countBuilds(t)
			f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-cons-1"), usageTier: tc.usageTier}
			f.setReplies(proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
			srv := f.start()
			seedSaved(t, store, srv.URL, "ch-cons-1", "saved-proof-header")

			out, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
			left := savedFor(t, store, srv.URL)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("consumed + tier %q should report success, got %v\nstderr: %s", tc.usageTier, err, errOut)
				}
				if len(left) != 0 {
					t.Fatalf("confirmed payment still saved: %+v", left)
				}
				if !strings.Contains(out, `"tier": "`+tc.usageTier+`"`) {
					t.Fatalf("stdout should report the key's tier: %q", out)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "already consumed") {
					t.Fatalf("consumed + tier %q should fail, got %v", tc.usageTier, err)
				}
				if len(left) != 1 {
					t.Fatal("an unconfirmed consumed payment must stay saved for a rerun")
				}
			}
			if *builds != 0 {
				t.Fatalf("built %d payments during a resume", *builds)
			}
		})
	}
}

// Answers after which the proof can never settle drop it; transient ones
// keep it for a rerun.
func TestUpgradeTerminalAndTransientAnswers(t *testing.T) {
	cases := []struct {
		name        string
		reply       proofReply
		wantErr     string
		wantSaved   bool
		wantSubmits int // 0 = exactly 1
	}{
		{"422 rejected", proofReply{status: http.StatusUnprocessableEntity, body: `{"error":"transaction rejected by broadcast"}`}, "rejected", false, 0},
		{"409 txid used", proofReply{status: http.StatusConflict, body: `{"error":"payment txid already used"}`}, "txid already used", false, 0},
		{"400 malformed", proofReply{status: http.StatusBadRequest, body: `{"error":"malformed payment proof"}`}, "malformed", false, 0},
		{"402 insufficient", proofReply{status: http.StatusPaymentRequired, body: `{"error":"payment insufficient"}`}, "insufficient", false, 0},
		{"410 expired", proofReply{status: http.StatusGone, body: `{"error":"challenge expired; request a new one"}`}, "expired", false, 0},
		// A 5xx says nothing about the proof: it is resubmitted every 10s
		// (no Retry-After) for the whole 1m --wait, at 0s, 10s, ... 60s.
		{"500", proofReply{status: http.StatusInternalServerError, body: `{"error":"upgrade failed"}`}, "rerun `bb key upgrade`", true, 7},
		{"502 from the LB", proofReply{status: http.StatusBadGateway, body: "<html>502 Bad Gateway</html>", ctype: "text/html"}, "rerun `bb key upgrade`", true, 7},
		{"503 Retry-After", proofReply{status: http.StatusServiceUnavailable, retryAfter: "30", body: `{"error":"draining"}`}, "rerun `bb key upgrade`", true, 3},
		{"200 without settlement", proofReply{status: http.StatusOK, body: `{}`}, "no settlement details", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := useTempStore(t)
			fakeClock(t)
			f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-term-1")}
			f.setReplies(tc.reply)
			srv := f.start()

			_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
			if got := len(savedFor(t, store, srv.URL)) == 1; got != tc.wantSaved {
				t.Fatalf("saved after %s = %v, want %v", tc.name, got, tc.wantSaved)
			}
			want := tc.wantSubmits
			if want == 0 {
				want = 1
			}
			proofs, _ := f.snapshot()
			if len(proofs) != want {
				t.Fatalf("want %d submits after %s, got %d", want, tc.name, len(proofs))
			}
			for _, p := range proofs[1:] {
				if p != proofs[0] {
					t.Fatal("a resubmit carried a different X402-Proof; it must resend the identical proof")
				}
			}
		})
	}
}

// Ctrl-C while waiting between resubmits stops promptly and keeps the proof.
func TestUpgradeCancelWhilePending(t *testing.T) {
	store := useTempStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	oldSleep := sleepCtx
	sleepCtx = func(c context.Context, d time.Duration) error {
		cancel() // the user hits Ctrl-C during the wait
		return oldSleep(c, time.Hour)
	}
	t.Cleanup(func() { sleepCtx = oldSleep })
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-cancel-1")}
	f.setReplies(proofReply{status: http.StatusAccepted, body: pendingBody})
	srv := f.start()

	done := make(chan error, 1)
	go func() {
		_, _, err := runUpgrade(t, ctx, srv.URL, 10*time.Minute)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
			t.Fatalf("cancel should stop with a resume hint, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling did not stop the wait")
	}
	if len(savedFor(t, store, srv.URL)) != 1 {
		t.Fatal("a cancelled pending payment must stay saved")
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{
		"":                              10 * time.Second,
		"soon":                          10 * time.Second,
		"Wed, 21 Oct 2026 07:28:00 GMT": 10 * time.Second,
		"0":                             time.Second,
		"-5":                            time.Second,
		"7":                             7 * time.Second,
		" 12 ":                          12 * time.Second,
		"60":                            60 * time.Second,
		"61":                            60 * time.Second,
		"86400":                         60 * time.Second,
		"99999999999999999999":          60 * time.Second,
		"-99999999999999999999":         10 * time.Second,
	}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %s, want %s", in, got, want)
		}
	}
}

// The rate-limit offer never pays while a saved proof for the key is
// unsettled; it points at `bb key upgrade` instead.
func TestPayChallengeRefusesWhileSaved(t *testing.T) {
	store := useTempStore(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-offer-2")}
	f.setReplies(proofReply{status: http.StatusOK})
	srv := f.start()
	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = srv.URL, "bb_live_test"
	t.Cleanup(func() { flagHost, flagAPIKey = oldHost, oldKey })
	seedSaved(t, store, srv.URL, "ch-offer-1", "saved-proof-header")

	var buf bytes.Buffer
	err := payChallenge(context.Background(), &buf, testWIF, f.ch)
	if err == nil || !strings.Contains(err.Error(), "bb key upgrade") {
		t.Fatalf("want a pointer to `bb key upgrade`, got %v", err)
	}
	if strings.Contains(buf.String(), "Pay ") || *builds != 0 {
		t.Fatalf("must not prompt or build while a proof is saved (builds=%d, out=%q)", *builds, buf.String())
	}
}

// The rate-limit offer's inline payment waits through 202s too.
func TestPayChallengeInlineWaitsForPending(t *testing.T) {
	store := useTempStore(t)
	slept := fakeClock(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-inline-2")}
	f.setReplies(
		proofReply{status: http.StatusAccepted, retryAfter: "3", body: pendingBody},
		proofReply{status: http.StatusOK},
	)
	srv := f.start()
	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = srv.URL, "bb_live_test"
	t.Cleanup(func() { flagHost, flagAPIKey = oldHost, oldKey })
	oldWait := upgradeWait
	upgradeWait = time.Minute
	t.Cleanup(func() { upgradeWait = oldWait })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("y\n")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var buf bytes.Buffer
	if err := payChallenge(context.Background(), &buf, testWIF, f.ch); err != nil {
		t.Fatalf("inline payment failed: %v\n%s", err, buf.String())
	}
	if proofs, _ := f.snapshot(); len(proofs) != 2 || proofs[0] != proofs[1] {
		t.Fatalf("want the same proof submitted twice, got %d", len(proofs))
	}
	if !equalDurations(*slept, []time.Duration{3 * time.Second}) {
		t.Fatalf("waits = %v", *slept)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// sleepCtx returns as soon as its context ends.
func TestSleepCtxCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := sleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("sleepCtx ignored cancellation")
	}
}

// The upgrade route's own limiter answers 429 + Retry-After; that is a wait,
// not a failure, and the same proof is resubmitted.
func TestUpgradeRetriesAfter429(t *testing.T) {
	store := useTempStore(t)
	slept := fakeClock(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-429-1")}
	f.setReplies(
		proofReply{status: http.StatusTooManyRequests, retryAfter: "2", body: `{"error":"payment request limit exceeded"}`},
		proofReply{status: http.StatusOK},
	)
	srv := f.start()

	if _, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute); err != nil {
		t.Fatalf("upgrade failed: %v\nstderr: %s", err, errOut)
	}
	if proofs, _ := f.snapshot(); len(proofs) != 2 || proofs[0] != proofs[1] {
		t.Fatalf("want the same proof submitted twice, got %d", len(proofs))
	}
	if !equalDurations(*slept, []time.Duration{2 * time.Second}) {
		t.Fatalf("waits = %v", *slept)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// --- review round 1 (OPL-5278) ---

// A resubmit that lands on a node where the upgrade route is not mounted gets
// chi's bare "404 page not found". That says nothing about the proof: the
// entry stays and the error points at the resume. Only the upgrade handler's
// own {"error":"challenge not found"} drops it.
func TestUpgrade404OnlyChallengeNotFoundDropsProof(t *testing.T) {
	cases := []struct {
		name      string
		reply     proofReply
		wantErr   string
		wantSaved bool
	}{
		{"bare 404", proofReply{status: http.StatusNotFound, body: "404 page not found\n", ctype: "text/plain; charset=utf-8"}, "rerun `bb key upgrade`", true},
		{"challenge not found", proofReply{status: http.StatusNotFound, body: `{"error":"challenge not found"}`}, "challenge not found", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := useTempStore(t)
			fakeClock(t)
			builds := countBuilds(t)
			f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-404-1")}
			f.setReplies(tc.reply)
			srv := f.start()
			seedSaved(t, store, srv.URL, "ch-404-1", "saved-proof-header")

			_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.wantErr)
			}
			if got := len(savedFor(t, store, srv.URL)); (got == 1) != tc.wantSaved {
				t.Fatalf("saved entries after %s = %d, want saved=%v", tc.name, got, tc.wantSaved)
			}
			if *builds != 0 {
				t.Fatalf("built %d payments during a resume", *builds)
			}
		})
	}
}

// 409 consumed while the key's tier is below the saved entry's, long after
// the entry was saved (the tier lapsed, or an admin changed it): the proof can
// never settle, so the entry goes and the next run can buy a new upgrade
// instead of being refused forever.
func TestUpgradeConsumedStaleEntryIsDropped(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-stale-2"), usageTier: "free"}
	f.setReplies(proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-stale-1", "saved-proof-header", nowFn().Add(-31*24*time.Hour))

	_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "can never settle again") {
		t.Fatalf("a stale consumed entry should fail with an explanation, got %v", err)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("a stale consumed entry must be removed, got %+v", left)
	}
	if *builds != 0 {
		t.Fatalf("built %d payments while resolving a saved entry", *builds)
	}

	// The next run is no longer wedged: it fetches the open challenge and pays.
	f.setReplies(proofReply{status: http.StatusOK})
	if _, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute); err != nil {
		t.Fatalf("the run after a dropped stale entry failed: %v\nstderr: %s", err, errOut)
	}
	if _, fetches := f.snapshot(); fetches != 1 {
		t.Fatalf("want 1 challenge fetch, got %d", fetches)
	}
	if *builds != 1 {
		t.Fatalf("want 1 payment built after the stale entry was dropped, got %d", *builds)
	}
}

// Inside the one-hour grace window the entry is kept: a fresh settle can take
// a moment to show on every server.
func TestUpgradeConsumedYoungEntryIsKept(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-young-1"), usageTier: "free"}
	f.setReplies(proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-young-1", "saved-proof-header", nowFn().Add(-59*time.Minute))

	_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
		t.Fatalf("a young consumed entry should ask for a rerun, got %v", err)
	}
	if len(savedFor(t, store, srv.URL)) != 1 {
		t.Fatal("a young consumed entry must stay saved")
	}
}

// The grace runs from the proof's last earlier submit, not from when it was
// saved: a proof that stayed pending for hours and then settled on a submit
// seconds ago (its 200 lost, the tier read lagging) must not be dropped, or
// the user is told to pay again for a tier they just bought.
func TestUpgradeConsumedAfterRecentSubmitIsKept(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-late-1"), usageTier: "free"}
	f.setReplies(
		proofReply{status: http.StatusAccepted, retryAfter: "10", body: pendingBody},
		proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`},
	)
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-late-1", "saved-proof-header", nowFn().Add(-3*time.Hour))

	_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
		t.Fatalf("consumed 10s after the previous submit should ask for a rerun, got %v", err)
	}
	left := savedFor(t, store, srv.URL)
	if len(left) != 1 {
		t.Fatal("a proof submitted seconds before the consumed answer must stay saved")
	}
	// The consumed-answered submit settled nothing, so the recorded submit is
	// the 202 one, 10s (one Retry-After) earlier.
	if want := nowFn().Add(-10 * time.Second); !left[0].LastSubmitAt.Equal(want) {
		t.Fatalf("LastSubmitAt = %s, want the 202 submit at %s", left[0].LastSubmitAt, want)
	}
	if *builds != 0 {
		t.Fatalf("built %d payments during a resume", *builds)
	}
}

// Rerunning as the keep path asks must not extend the grace: a submit that
// got "challenge already consumed" settled nothing, so it is not recorded, and
// once the last real submit is over an hour old the entry is dropped instead
// of wedging every upgrade for as long as the user keeps rerunning.
func TestUpgradeConsumedRerunDoesNotExtendGrace(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-rerun-1"), usageTier: "free"}
	f.setReplies(proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-rerun-1", "saved-proof-header", nowFn().Add(-50*time.Minute))

	_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
		t.Fatalf("first run, 50m after the save, should keep the entry, got %v", err)
	}
	left := savedFor(t, store, srv.URL)
	if len(left) != 1 {
		t.Fatal("first run must keep the entry inside the grace")
	}
	if !left[0].LastSubmitAt.IsZero() {
		t.Fatalf("a consumed-answered submit was recorded: LastSubmitAt = %s", left[0].LastSubmitAt)
	}

	if err := sleepCtx(context.Background(), 30*time.Minute); err != nil {
		t.Fatal(err)
	}
	_, _, err = runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "can never settle again") {
		t.Fatalf("rerun 80m after the last real submit should drop the entry, got %v", err)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("entry still saved after the grace ran out: %+v", left)
	}
	if proofs, _ := f.snapshot(); len(proofs) != 2 {
		t.Fatalf("want one submit per run, got %d", len(proofs))
	}
	if *builds != 0 {
		t.Fatalf("built %d payments while resolving a saved entry", *builds)
	}
}

// --dry-run never resumes a saved proof: it only fetches and prints the
// challenge, whether the entry is found by key fingerprint or, under another
// spelling of the host, by challenge id.
func TestUpgradeDryRunDoesNotResumeSaved(t *testing.T) {
	for _, tc := range []struct {
		name  string
		runAt func(t *testing.T, srvURL string) string
	}{
		{"same fingerprint", func(_ *testing.T, u string) string { return u }},
		{"same challenge id", localhostURL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := useTempStore(t)
			fakeClock(t)
			builds := countBuilds(t)
			f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-dry-1")}
			f.setReplies(proofReply{status: http.StatusOK})
			srv := f.start()
			seedSaved(t, store, srv.URL, "ch-dry-1", "saved-proof-header")

			out, errOut, err := runUpgradeMode(t, context.Background(), tc.runAt(t, srv.URL), time.Minute, true)
			if err != nil {
				t.Fatalf("dry run failed: %v\nstderr: %s", err, errOut)
			}
			if proofs, _ := f.snapshot(); len(proofs) != 0 {
				t.Fatalf("--dry-run submitted %d proofs", len(proofs))
			}
			if *builds != 0 {
				t.Fatalf("--dry-run built %d payments", *builds)
			}
			if len(savedFor(t, store, srv.URL)) != 1 {
				t.Fatal("--dry-run must leave the saved proof in place")
			}
			if !strings.Contains(errOut, "has not settled") {
				t.Fatalf("--dry-run should mention the saved payment, stderr: %s", errOut)
			}
			var ch x402.Challenge
			if err := json.Unmarshal([]byte(out), &ch); err != nil || ch.ChallengeID != "ch-dry-1" {
				t.Fatalf("stdout should be the challenge JSON, got %q (err %v)", out, err)
			}
		})
	}
}

// localhostURL spells an httptest URL's host differently, so the key
// fingerprint differs while the server is the same.
func localhostURL(t *testing.T, srvURL string) string {
	t.Helper()
	if !strings.Contains(srvURL, "127.0.0.1") {
		t.Skipf("httptest URL %s is not on 127.0.0.1", srvURL)
	}
	return strings.Replace(srvURL, "127.0.0.1", "localhost", 1)
}

// A proof saved under another spelling of the host misses the fingerprint
// lookup, but the server re-serves the same challenge id: that match alone
// resumes the saved proof, and no payment is built. The challenge is past
// expires_at: a pending payment keeps it open, so the expiry guard (which only
// protects building a NEW payment) must not block the resume.
func TestUpgradeResumesSavedProofByChallengeID(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	builds := countBuilds(t)
	ch := testChallenge("ch-id-1")
	// time.Now, not nowFn: ensureNotExpired reads the real clock.
	ch.ExpiresAt = time.Now().Add(-time.Minute).UTC()
	f := &fakeUpgradeServer{t: t, ch: ch}
	f.setReplies(proofReply{status: http.StatusOK})
	srv := f.start()
	seedSaved(t, store, srv.URL, "ch-id-1", "saved-proof-header")
	other := localhostURL(t, srv.URL)
	if x402.KeyFingerprint(other, "bb_live_test") == x402.KeyFingerprint(srv.URL, "bb_live_test") {
		t.Fatal("test setup: the two host spellings must fingerprint differently")
	}

	_, errOut, err := runUpgrade(t, context.Background(), other, time.Minute)
	if err != nil {
		t.Fatalf("resume failed: %v\nstderr: %s", err, errOut)
	}
	if *builds != 0 {
		t.Fatalf("built %d payments for a challenge that already has a saved proof", *builds)
	}
	if proofs, _ := f.snapshot(); len(proofs) != 1 || proofs[0] != "saved-proof-header" {
		t.Fatalf("want the saved proof resubmitted once, got %q", proofs)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// The rate-limit offer refuses to pay a challenge that has a saved proof,
// whatever fingerprint it was saved under.
func TestPayChallengeRefusesSavedChallengeID(t *testing.T) {
	store := useTempStore(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-offer-id-1")}
	f.setReplies(proofReply{status: http.StatusOK})
	srv := f.start()
	seedSaved(t, store, srv.URL, "ch-offer-id-1", "saved-proof-header")
	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = localhostURL(t, srv.URL), "bb_live_test"
	t.Cleanup(func() { flagHost, flagAPIKey = oldHost, oldKey })

	var buf bytes.Buffer
	err := payChallenge(context.Background(), &buf, testWIF, f.ch)
	if err == nil || !strings.Contains(err.Error(), "bb key upgrade") {
		t.Fatalf("want a pointer to `bb key upgrade`, got %v", err)
	}
	if strings.Contains(buf.String(), "Pay ") || *builds != 0 {
		t.Fatalf("must not prompt or build while the challenge has a saved proof (builds=%d, out=%q)", *builds, buf.String())
	}
}

// The inline offer builds under --timeout but waits out 202s under --wait:
// the resubmit loop must not inherit the --timeout deadline.
func TestPayChallengeSettleNotBoundByTimeout(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	oldSleep := sleepCtx
	deadlines := 0
	sleepCtx = func(ctx context.Context, d time.Duration) error {
		if _, ok := ctx.Deadline(); ok {
			deadlines++
		}
		return oldSleep(ctx, d)
	}
	t.Cleanup(func() { sleepCtx = oldSleep })
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-inline-t")}
	f.setReplies(
		proofReply{status: http.StatusAccepted, retryAfter: "10", body: pendingBody},
		proofReply{status: http.StatusAccepted, retryAfter: "10", body: pendingBody},
		proofReply{status: http.StatusOK},
	)
	srv := f.start()
	oldHost, oldKey, oldTimeout, oldWait := flagHost, flagAPIKey, flagTimeout, upgradeWait
	flagHost, flagAPIKey, flagTimeout, upgradeWait = srv.URL, "bb_live_test", 5*time.Second, 10*time.Minute
	t.Cleanup(func() { flagHost, flagAPIKey, flagTimeout, upgradeWait = oldHost, oldKey, oldTimeout, oldWait })

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString("y\n")
	w.Close()
	oldStdin := os.Stdin
	os.Stdin = r
	t.Cleanup(func() { os.Stdin = oldStdin })

	var buf bytes.Buffer
	if err := payChallenge(context.Background(), &buf, testWIF, f.ch); err != nil {
		t.Fatalf("inline payment failed: %v\n%s", err, buf.String())
	}
	if deadlines != 0 {
		t.Fatalf("%d pending waits ran under a deadline; the settle loop must be bounded by --wait, not --timeout", deadlines)
	}
	if proofs, _ := f.snapshot(); len(proofs) != 3 {
		t.Fatalf("want 3 submits, got %d", len(proofs))
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// --- review round 3 (OPL-5278) ---

// A saved entry whose last submit is long past, answered "challenge already
// consumed" while the key already holds the tier (a lost 200 from weeks ago,
// then a renewal run): that old settle is not this run's result. The run
// fails, reports nothing on stdout, buys nothing, and drops the entry.
func TestUpgradeConsumedStaleAtTierIsNotThisRunsSettlement(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-new-1"), usageTier: "pro"}
	f.setReplies(proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-old", "saved-proof-header", nowFn().Add(-25*24*time.Hour))

	out, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil {
		t.Fatalf("a settle from 25 days ago must not be reported as this run's upgrade\nstdout: %s\nstderr: %s", out, errOut)
	}
	for _, want := range []string{"settled earlier", "bought nothing", "rerun `bb key upgrade`"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if out != "" {
		t.Fatalf("stdout must carry no settlement, got %q", out)
	}
	if strings.Contains(errOut, "upgrade settled") {
		t.Fatalf("stderr reports the old payment as settled by this run: %q", errOut)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("the long-settled entry must be removed, got %+v", left)
	}
	if proofs, fetches := f.snapshot(); len(proofs) != 1 || fetches != 0 || *builds != 0 {
		t.Fatalf("want 1 resubmit, 0 challenge fetches, 0 builds; got %d, %d, %d", len(proofs), fetches, *builds)
	}

	// The entry no longer takes over: the next run buys the renewal.
	f.setReplies(proofReply{status: http.StatusOK})
	if _, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute); err != nil {
		t.Fatalf("the run after the stale entry was dropped failed: %v\nstderr: %s", err, errOut)
	}
	if _, fetches := f.snapshot(); fetches != 1 || *builds != 1 {
		t.Fatalf("want 1 challenge fetch and 1 build for the renewal, got %d and %d", fetches, *builds)
	}
}

// A 429 is sent before the server reads the proof, so it settled nothing and
// must not move the entry's last submit time: a following "challenge already
// consumed" still sees a 25-day-old entry and refuses to report it as this
// run's upgrade.
func TestUpgradeRateLimitedResubmitDoesNotRefreshStaleEntry(t *testing.T) {
	store := useTempStore(t)
	slept := fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-new-1"), usageTier: "pro"}
	f.setReplies(
		proofReply{status: http.StatusTooManyRequests, retryAfter: "1", body: `{"error":"payment request limit exceeded"}`},
		proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`},
	)
	srv := f.start()
	seedSavedAt(t, store, srv.URL, "ch-old", "saved-proof-header", nowFn().Add(-25*24*time.Hour))

	out, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err == nil {
		t.Fatalf("a 429 then consumed on a 25-day-old entry must not be reported as this run's upgrade\nstdout: %s\nstderr: %s", out, errOut)
	}
	for _, want := range []string{"settled earlier", "bought nothing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
	if out != "" {
		t.Fatalf("stdout must carry no settlement, got %q", out)
	}
	if strings.Contains(errOut, "upgrade settled") {
		t.Fatalf("stderr reports the old payment as settled by this run: %q", errOut)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("the long-settled entry must be removed, got %+v", left)
	}
	if proofs, fetches := f.snapshot(); len(proofs) != 2 || fetches != 0 || *builds != 0 {
		t.Fatalf("want 2 resubmits, 0 challenge fetches, 0 builds; got %d, %d, %d", len(proofs), fetches, *builds)
	}
	if len(*slept) != 1 || (*slept)[0] != time.Second {
		t.Fatalf("want one 1s wait for the 429's Retry-After, got %v", *slept)
	}
}

// A submit that fails in transit (the connection drops before the answer, or
// in the middle of a 200's body) is resubmitted with the identical proof
// within the same run, under --wait.
func TestUpgradeRetriesAfterDroppedConnection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first proofReply
	}{
		{"before the answer", proofReply{hangup: true}},
		{"mid-body", proofReply{truncate: true}},
	} {
		t.Run(tc.name, func(t *testing.T) { testRetryAfterTransportError(t, tc.first) })
	}
}

func testRetryAfterTransportError(t *testing.T, first proofReply) {
	store := useTempStore(t)
	slept := fakeClock(t)
	builds := countBuilds(t)
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-drop-1")}
	f.setReplies(first, proofReply{status: http.StatusOK})
	srv := f.start()

	out, errOut, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
	if err != nil {
		t.Fatalf("upgrade failed: %v\nstderr: %s", err, errOut)
	}
	proofs, _ := f.snapshot()
	if len(proofs) != 2 || proofs[0] != proofs[1] {
		t.Fatalf("want the same proof submitted twice, got %d", len(proofs))
	}
	if !equalDurations(*slept, []time.Duration{defaultPendingRetry}) {
		t.Fatalf("waits = %v", *slept)
	}
	if *builds != 1 {
		t.Fatalf("built %d payments, want 1", *builds)
	}
	if !strings.Contains(out, `"tier": "pro"`) {
		t.Fatalf("stdout lacks the settlement: %q", out)
	}
	if left := savedFor(t, store, srv.URL); len(left) != 0 {
		t.Fatalf("settled payment still saved: %+v", left)
	}
}

// Ctrl-C during a submit that fails in transit stops at once rather than
// retrying.
func TestUpgradeTransientErrorAfterCancelStops(t *testing.T) {
	store := useTempStore(t)
	fakeClock(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-drop-2")}
	f.setReplies(proofReply{hangup: true})
	f.onProof = func(string) { cancel() }
	srv := f.start()

	_, errOut, err := runUpgrade(t, ctx, srv.URL, 10*time.Minute)
	if err == nil || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
		t.Fatalf("cancel should stop with a resume hint, got %v", err)
	}
	if strings.Contains(errOut, "resubmitting") {
		t.Fatalf("a cancelled run must not announce a resubmit: %q", errOut)
	}
	if proofs, _ := f.snapshot(); len(proofs) != 1 {
		t.Fatalf("a cancelled run must not resubmit, got %d submits", len(proofs))
	}
	if len(savedFor(t, store, srv.URL)) != 1 {
		t.Fatal("a cancelled payment must stay saved")
	}
}

// Only a "challenge already consumed" answer un-records its submit. A submit
// whose response was lost may have settled, so it stays recorded: a consumed
// answer to the next resubmit is then judged from it, not from the entry's
// save hours ago, and the entry is kept rather than dropped as stale (which
// would tell the user to pay again for a tier just bought).
func TestUpgradeLostResponseSubmitStaysRecorded(t *testing.T) {
	for _, tc := range []struct {
		name string
		lost proofReply
	}{
		{"dropped connection", proofReply{hangup: true}},
		{"500", proofReply{status: http.StatusInternalServerError, body: `{"error":"upgrade failed"}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := useTempStore(t)
			fakeClock(t)
			builds := countBuilds(t)
			f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-lost-1"), usageTier: "free"}
			f.setReplies(tc.lost, proofReply{status: http.StatusConflict, body: `{"error":"challenge already consumed"}`})
			srv := f.start()
			seedSavedAt(t, store, srv.URL, "ch-lost-1", "saved-proof-header", nowFn().Add(-3*time.Hour))
			lostAt := nowFn()

			_, _, err := runUpgrade(t, context.Background(), srv.URL, time.Minute)
			if err == nil || !strings.Contains(err.Error(), "rerun `bb key upgrade`") {
				t.Fatalf("consumed right after a lost submit should ask for a rerun, got %v", err)
			}
			left := savedFor(t, store, srv.URL)
			if len(left) != 1 {
				t.Fatal("a proof whose last submit may have just settled must stay saved")
			}
			if !left[0].LastSubmitAt.Equal(lostAt) {
				t.Fatalf("LastSubmitAt = %s, want the lost submit at %s", left[0].LastSubmitAt, lostAt)
			}
			if proofs, _ := f.snapshot(); len(proofs) != 2 {
				t.Fatalf("want 2 submits, got %d", len(proofs))
			}
			if *builds != 0 {
				t.Fatalf("built %d payments during a resume", *builds)
			}
		})
	}
}

// --wait defaults to the spec's 10m budget.
func TestUpgradeWaitFlagDefault(t *testing.T) {
	fl := keyUpgradeCmd.Flags().Lookup("wait")
	if fl == nil {
		t.Fatal("bb key upgrade has no --wait flag")
	}
	if fl.DefValue != "10m0s" {
		t.Fatalf("--wait default = %s, want 10m0s", fl.DefValue)
	}
}

// A negative --wait is rejected before any request or payment build.
func TestUpgradeRejectsNegativeWait(t *testing.T) {
	useTempStore(t)
	builds := countBuilds(t)
	var hits atomic.Int64
	f := &fakeUpgradeServer{t: t, ch: testChallenge("ch-neg-wait-1")}
	f.setReplies(proofReply{status: http.StatusOK})
	inner := f.start()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		inner.Config.Handler.ServeHTTP(rw, r)
	}))
	t.Cleanup(srv.Close)

	_, _, err := runUpgrade(t, context.Background(), srv.URL, -time.Second)
	if err == nil || !strings.Contains(err.Error(), "--wait must not be negative") {
		t.Fatalf("want a negative --wait rejected, got %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("sent %d requests despite a negative --wait", n)
	}
	if *builds != 0 {
		t.Fatalf("built %d payments despite a negative --wait", *builds)
	}
}
