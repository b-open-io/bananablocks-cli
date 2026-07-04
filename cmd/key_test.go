package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

const testWIF = "KwDiBf89QgGbjEhKnhXJuH7LrciVrZi3qYjgd9M7rFU73sVHnoWn"

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

	res, err := submitProof(context.Background(), c, got, tx.Bytes(), tx.TxID().String())
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
		{http.StatusConflict, "not broadcast"},
		{http.StatusGone, "expired"},
		{http.StatusUnprocessableEntity, "rejected"},
	}
	for _, tc := range cases {
		err := upgradeError(&api.Error{Status: tc.status, Message: "m"})
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("status %d → %q, want it to mention %q", tc.status, err, tc.want)
		}
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
