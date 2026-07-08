package spv

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/b-open-io/bananablocks-cli/internal/api"
)

// mockHeaders spins a fake block-headers service whose per-root verdict is
// chosen by decide ("CONFIRMED" / "INVALID" / "UNABLE_TO_VERIFY"). It echoes the
// queried root and height back in each confirmation, mirroring the real service.
func mockHeaders(t *testing.T, decide func(root string, height uint32) string) *HeadersVerifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/chain/merkleroot/verify" {
			http.Error(rw, "not found", http.StatusNotFound)
			return
		}
		var reqs []struct {
			MerkleRoot  string `json:"merkleRoot"`
			BlockHeight uint32 `json:"blockHeight"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqs); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		type conf struct {
			BlockHash    string `json:"blockHash"`
			BlockHeight  uint32 `json:"blockHeight"`
			MerkleRoot   string `json:"merkleRoot"`
			Confirmation string `json:"confirmation"`
		}
		resp := struct {
			ConfirmationState string `json:"confirmationState"`
			Confirmations     []conf `json:"confirmations"`
		}{ConfirmationState: "CONFIRMED"}
		for _, q := range reqs {
			state := decide(q.MerkleRoot, q.BlockHeight)
			if state != "CONFIRMED" {
				resp.ConfirmationState = "INVALID"
			}
			hash := ""
			if state == "CONFIRMED" {
				hash = "hash-" + strconv.FormatUint(uint64(q.BlockHeight), 10)
			}
			resp.Confirmations = append(resp.Confirmations, conf{
				BlockHash: hash, BlockHeight: q.BlockHeight, MerkleRoot: q.MerkleRoot, Confirmation: state,
			})
		}
		json.NewEncoder(rw).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return NewHeadersVerifier(srv.URL, "", srv.Client())
}

// TestHeadersVerifierStates checks the three verdicts map to the right
// Confirmation, that a confirmed root carries block metadata, and that an
// INVALID root does not expose an authoritative header root.
func TestHeadersVerifierStates(t *testing.T) {
	confirmed := strings.Repeat("11", 32)
	invalid := strings.Repeat("22", 32)
	pending := strings.Repeat("33", 32)
	hv := mockHeaders(t, func(root string, _ uint32) string {
		switch root {
		case confirmed:
			return "CONFIRMED"
		case pending:
			return "UNABLE_TO_VERIFY"
		default:
			return "INVALID"
		}
	})
	got, err := hv.ConfirmRoots(context.Background(), []RootAtHeight{
		{Root: confirmed, Height: 100}, {Root: invalid, Height: 101}, {Root: pending, Height: 102},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != ConfConfirmed {
		t.Errorf("confirmed: state = %v, want confirmed", got[0].State)
	}
	if got[0].BlockHash == "" || got[0].HeaderRoot != confirmed {
		t.Errorf("confirmed root should carry blockHash and headerRoot, got %+v", got[0])
	}
	if got[1].State != ConfInvalid {
		t.Errorf("invalid: state = %v, want invalid", got[1].State)
	}
	if got[1].HeaderRoot != "" {
		t.Errorf("an INVALID root must not expose an authoritative header root, got %q", got[1].HeaderRoot)
	}
	if got[2].State != ConfUnknown {
		t.Errorf("unable-to-verify: state = %v, want unverified", got[2].State)
	}
}

// TestHeadersVerifierMissingEntryIsUnknown guards the mapping: a pair the
// service omits from its response must be reported unverified, never silently
// confirmed.
func TestHeadersVerifierMissingEntryIsUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		io.WriteString(rw, `{"confirmationState":"INVALID","confirmations":[]}`)
	}))
	t.Cleanup(srv.Close)
	hv := NewHeadersVerifier(srv.URL, "", srv.Client())
	got, err := hv.ConfirmRoots(context.Background(), []RootAtHeight{{Root: strings.Repeat("aa", 32), Height: 5}})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].State != ConfUnknown {
		t.Fatalf("omitted entry should be unverified, got %v", got[0].State)
	}
}

// TestHeadersVerifierServiceError verifies a transport/HTTP failure surfaces as
// a *HeadersServiceError, so callers can fall back to the weaker verifier rather
// than reporting the proof as fraudulent.
func TestHeadersVerifierServiceError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		http.Error(rw, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	hv := NewHeadersVerifier(srv.URL, "", srv.Client())
	_, err := hv.ConfirmRoots(context.Background(), []RootAtHeight{{Root: strings.Repeat("aa", 32), Height: 5}})
	if err == nil {
		t.Fatal("expected an error on HTTP 500")
	}
	var se *HeadersServiceError
	if !errors.As(err, &se) {
		t.Fatalf("error should be a *HeadersServiceError, got %T: %v", err, err)
	}
}

// TestVerifyBEEFWithHeaders runs the main BEEF path against the header service
// across all three verdicts: CONFIRMED → valid, INVALID → invalid, and
// UNABLE_TO_VERIFY → unverified (not reported as fraud).
func TestVerifyBEEFWithHeaders(t *testing.T) {
	raw, subject, other, root := compoundBUMPBeef(t)

	got, err := VerifyBEEF(context.Background(), mockHeaders(t, func(r string, h uint32) string {
		if r == root && h == 700000 {
			return "CONFIRMED"
		}
		return "INVALID"
	}), subject, raw)
	if err != nil {
		t.Fatal(err)
	}
	valid := map[string]bool{}
	for _, r := range got {
		valid[r.Txid] = r.Valid
		if r.State != ConfConfirmed {
			t.Errorf("%s: state = %s, want confirmed", r.Txid, r.State)
		}
	}
	if !valid[subject] || !valid[other] {
		t.Fatalf("both leaves should be valid, got %v", valid)
	}

	got, err = VerifyBEEF(context.Background(), mockHeaders(t, func(string, uint32) string { return "INVALID" }), subject, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Valid || r.State != ConfInvalid {
			t.Fatalf("expected an invalid result, got %+v", r)
		}
	}

	got, err = VerifyBEEF(context.Background(), mockHeaders(t, func(string, uint32) string { return "UNABLE_TO_VERIFY" }), subject, raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range got {
		if r.Valid || r.State != ConfUnknown {
			t.Fatalf("expected an unverified result, got %+v", r)
		}
	}
}

// TestVerifyTSCWithHeaders confirms the TSC path resolves the block height from
// the API, confirms the root against the header service, and short-circuits an
// internally inconsistent proof (target != computed root) without any network
// call.
func TestVerifyTSCWithHeaders(t *testing.T) {
	txid := "f4184fc596403b9d638783cf57adfe4c75c605f6356fbc91338530e9831e9e16"
	proof := &TSCProof{Index: 1, Nodes: []string{"b1fea52486ce0c62bb442b530a3f0132b826c74e473d1f2c220bfa78111c5082"}}
	root := "7dac2c5666815c17a3b36427de37bb9d2e2c5ccec3f8633eb91a4205cb4c10ff"

	txSrv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{"block_hash": "0000block170", "block_height": 170})
	}))
	t.Cleanup(txSrv.Close)
	c := &api.Client{BaseURL: txSrv.URL, HTTP: txSrv.Client()}

	hv := mockHeaders(t, func(r string, h uint32) string {
		if r == root && h == 170 {
			return "CONFIRMED"
		}
		return "INVALID"
	})
	res, err := VerifyTSC(context.Background(), c, hv, txid, proof)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Valid || res.State != ConfConfirmed {
		t.Fatalf("expected a confirmed valid result, got %+v", res)
	}
	if res.BlockHeight != 170 {
		t.Errorf("block height = %d, want 170", res.BlockHeight)
	}

	// Target mismatch → invalid, decided locally (nil client and verifier prove
	// no network call is reached).
	bad := &TSCProof{Index: 1, Target: strings.Repeat("00", 32), Nodes: proof.Nodes}
	res, err = VerifyTSC(context.Background(), nil, nil, txid, bad)
	if err != nil {
		t.Fatal(err)
	}
	if res.Valid || res.State != ConfInvalid {
		t.Fatalf("a target mismatch should be invalid without a network call, got %+v", res)
	}
}
