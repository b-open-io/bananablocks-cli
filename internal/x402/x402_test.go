package x402

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// TestVerifyPayee is the P2 guard: the confirmation prompt's displayed address
// must match the script the payment actually pays. A consistent challenge
// passes; a mismatched one is refused; an empty address is allowed.
func TestVerifyPayee(t *testing.T) {
	addr, err := script.NewAddressFromString("1BgGZ9tcN4rm9KBzDn7KprQz87SZ26SAMH")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		t.Fatal(err)
	}

	ok := &Challenge{PayeeAddress: addr.AddressString, PayeeLockingScriptHex: lock.String()}
	if err := ok.VerifyPayee(); err != nil {
		t.Fatalf("consistent challenge rejected: %v", err)
	}

	// Same script, but a different address displayed to the user → refuse.
	bad := &Challenge{PayeeAddress: "1111111111111111111114oLvT2", PayeeLockingScriptHex: lock.String()}
	if err := bad.VerifyPayee(); err == nil {
		t.Fatal("mismatched payee address must be refused")
	}

	// No address claimed → nothing to attest, allowed.
	none := &Challenge{PayeeLockingScriptHex: lock.String()}
	if err := none.VerifyPayee(); err != nil {
		t.Fatalf("empty payee address should be allowed: %v", err)
	}
}

func TestProofHeaderRoundTrip(t *testing.T) {
	p := &Proof{
		Version:     Version,
		ChallengeID: "abc123",
		RawTxBase64: base64.StdEncoding.EncodeToString([]byte{1, 2, 3}),
		Txid:        "00" + strings.Repeat("ab", 31),
	}
	hdr, err := EncodeProofHeader(p)
	if err != nil {
		t.Fatal(err)
	}
	// The server decodes with base64.RawURLEncoding then json.Unmarshal —
	// mirror that exactly.
	raw, err := base64.RawURLEncoding.DecodeString(hdr)
	if err != nil {
		t.Fatalf("header is not raw base64url: %v", err)
	}
	var back Proof
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("header payload is not JSON: %v", err)
	}
	if back != *p {
		t.Fatalf("round trip mismatch: %+v != %+v", back, *p)
	}
}

func TestDecodeChallengeHeader(t *testing.T) {
	ch := Challenge{
		Version:     Version,
		ChallengeID: "deadbeef",
		Tier:        "pro",
		AmountSats:  50000,
		ExpiresAt:   time.Now().UTC().Truncate(time.Second),
	}
	raw, _ := json.Marshal(ch)
	got, err := DecodeChallengeHeader(base64.RawURLEncoding.EncodeToString(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got.ChallengeID != ch.ChallengeID || got.AmountSats != ch.AmountSats {
		t.Fatalf("decoded %+v, want %+v", got, ch)
	}
	// Padded base64url from less careful encoders must also decode.
	if _, err := DecodeChallengeHeader(base64.URLEncoding.EncodeToString(raw)); err != nil {
		t.Fatalf("padded header rejected: %v", err)
	}
}

// Test WIF (throwaway key, mainnet-encoded): the classic all-ones private key.
const testWIF = "KwDiBf89QgGbjEhKnhXJuH7LrciVrZi3qYjgd9M7rFU73sVHnoWn"

// newUTXOServer serves the address-UTXOs endpoint with the given UTXOs
// locked to the wallet's own script.
func newUTXOServer(t *testing.T, w *Wallet, values []int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/address/"+w.Address+"/utxos") {
			http.NotFound(rw, r)
			return
		}
		if r.URL.Query().Get("page") != "0" {
			rw.Write([]byte("[]"))
			return
		}
		out := make([]map[string]any, 0, len(values))
		for i, v := range values {
			out = append(out, map[string]any{
				"txid":        strings.Repeat("11", 31) + hex.EncodeToString([]byte{byte(i + 1)}),
				"vout":        0,
				"value":       v,
				"script_type": "p2pkh",
			})
		}
		json.NewEncoder(rw).Encode(out)
	}))
}

func TestBuildPayment(t *testing.T) {
	w, err := NewWallet(testWIF, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := newUTXOServer(t, w, []int64{600, 700})
	defer srv.Close()
	c := &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	ch := &Challenge{
		Version:               Version,
		ChallengeID:           "c1",
		Tier:                  "pro",
		AmountSats:            1000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
		ExpiresAt:             time.Now().Add(10 * time.Minute),
	}
	tx, err := w.BuildPayment(context.Background(), c, ch, 1)
	if err != nil {
		t.Fatal(err)
	}

	if len(tx.Inputs) != 2 {
		t.Fatalf("selected %d inputs, want 2", len(tx.Inputs))
	}
	for i, in := range tx.Inputs {
		if in.UnlockingScript == nil || len(*in.UnlockingScript) == 0 {
			t.Fatalf("input %d is unsigned", i)
		}
	}

	var paid, change uint64
	for _, out := range tx.Outputs {
		if out.LockingScript.String() == ch.PayeeLockingScriptHex {
			paid += out.Satoshis
		} else {
			change += out.Satoshis
		}
	}
	if paid != uint64(ch.AmountSats) {
		t.Fatalf("payment output = %d sats, want %d", paid, ch.AmountSats)
	}
	fee := uint64(600+700) - paid - change
	if fee < 1 || fee > 100 {
		t.Fatalf("fee %d out of sane range for a ~%d-byte tx at 1 sat/kB", fee, tx.Size())
	}

	// The proof payload the server parses must round trip to the same txid.
	back, err := transaction.NewTransactionFromBytes(tx.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if back.TxID().String() != tx.TxID().String() {
		t.Fatal("serialized tx does not round trip")
	}
}

func TestBuildPaymentInsufficientFunds(t *testing.T) {
	w, err := NewWallet(testWIF, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := newUTXOServer(t, w, []int64{100})
	defer srv.Close()
	c := &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	ch := &Challenge{
		AmountSats:            1000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
	}
	if _, err := w.BuildPayment(context.Background(), c, ch, 1); err == nil ||
		!strings.Contains(err.Error(), "insufficient funds") {
		t.Fatalf("want insufficient-funds error, got %v", err)
	}
}

func TestBuildPaymentSkipsNonP2PKH(t *testing.T) {
	w, err := NewWallet(testWIF, true)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") != "0" {
			rw.Write([]byte("[]"))
			return
		}
		json.NewEncoder(rw).Encode([]map[string]any{
			{"txid": strings.Repeat("22", 32), "vout": 0, "value": int64(100000), "script_type": "ordinal"},
			{"txid": strings.Repeat("33", 32), "vout": 1, "value": int64(5000), "script_type": "p2pkh"},
		})
	}))
	defer srv.Close()
	c := &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}

	ch := &Challenge{
		AmountSats:            1000,
		PayeeLockingScriptHex: "76a914000000000000000000000000000000000000000088ac",
	}
	tx, err := w.BuildPayment(context.Background(), c, ch, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Inputs) != 1 {
		t.Fatalf("selected %d inputs, want only the p2pkh one", len(tx.Inputs))
	}
	if got := tx.Inputs[0].SourceTXID.String(); got != strings.Repeat("33", 32) {
		t.Fatalf("selected wrong utxo %s", got)
	}
}
