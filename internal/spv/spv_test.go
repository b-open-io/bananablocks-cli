package spv

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// blockHeaderServer serves /api/v1/block/<height> with the given merkle root.
func blockHeaderServer(t *testing.T, merkleRoot string) *api.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(map[string]any{
			"hash": "0000header", "height": 814435, "merkle_root": merkleRoot,
		})
	}))
	t.Cleanup(srv.Close)
	return &api.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// TestVerifyBEEFChecksLeafAgainstHeader verifies the proven leaf's computed
// root is compared to the fetched block header: a matching header passes, a
// tampered one fails. Deriving the subject/root/height from the fixture keeps
// the assertions honest without hardcoded transcription.
func TestVerifyBEEFChecksLeafAgainstHeader(t *testing.T) {
	raw, err := hex.DecodeString(brc62BEEF)
	if err != nil {
		t.Fatal(err)
	}
	beef, err := transaction.NewBeefFromBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	bump := beef.BUMPs[0]
	var subject string
	var wantRoot string
	for _, el := range bump.Path[0] {
		if el.Hash != nil && el.Txid != nil && *el.Txid {
			subject = el.Hash.String()
			root, _ := bump.ComputeRoot(el.Hash)
			wantRoot = root.String()
		}
	}
	if subject == "" {
		t.Fatal("fixture has no txid leaf")
	}

	// Correct header → valid.
	got, err := VerifyBEEF(context.Background(), blockHeaderServer(t, wantRoot), subject, raw)
	if err != nil {
		t.Fatalf("valid BEEF rejected: %v", err)
	}
	for _, r := range got {
		if !r.Valid {
			t.Fatalf("expected all results valid, got %+v", r)
		}
	}

	// Tampered header → the leaf result is invalid.
	got, err = VerifyBEEF(context.Background(), blockHeaderServer(t, strings.Repeat("00", 32)), subject, raw)
	if err != nil {
		t.Fatal(err)
	}
	anyInvalid := false
	for _, r := range got {
		if !r.Valid {
			anyInvalid = true
		}
	}
	if !anyInvalid {
		t.Fatal("tampered header root should make a leaf result invalid")
	}
}

// A valid BRC-62 BEEF (from the go-sdk test vectors) that proves one specific
// transaction. Verifying a *different* txid against it must fail rather than
// report the unrelated proofs as OK.
const brc62BEEF = "0100beef01fe636d0c0007021400fe507c0c7aa754cef1f7889d5fd395cf1f785dd7de98eed895dbedfe4e5bc70d1502ac4e164f5bc16746bb0868404292ac8318bbac3800e4aad13a014da427adce3e010b00bc4ff395efd11719b277694cface5aa50d085a0bb81f613f70313acd28cf4557010400574b2d9142b8d28b61d88e3b2c3f44d858411356b49a28a4643b6d1a6a092a5201030051a05fc84d531b5d250c23f4f886f6812f9fe3f402d61607f977b4ecd2701c19010000fd781529d58fc2523cf396a7f25440b409857e7e221766c57214b1d38c7b481f01010062f542f45ea3660f86c013ced80534cb5fd4c19d66c56e7e8c5d4bf2d40acc5e010100b121e91836fd7cd5102b654e9f72f3cf6fdbfd0b161c53a9c54b12c841126331020100000001cd4e4cac3c7b56920d1e7655e7e260d31f29d9a388d04910f1bbd72304a79029010000006b483045022100e75279a205a547c445719420aa3138bf14743e3f42618e5f86a19bde14bb95f7022064777d34776b05d816daf1699493fcdf2ef5a5ab1ad710d9c97bfb5b8f7cef3641210263e2dee22b1ddc5e11f6fab8bcd2378bdd19580d640501ea956ec0e786f93e76ffffffff013e660000000000001976a9146bfd5c7fbe21529d45803dbcf0c87dd3c71efbc288ac0000000001000100000001ac4e164f5bc16746bb0868404292ac8318bbac3800e4aad13a014da427adce3e000000006a47304402203a61a2e931612b4bda08d541cfb980885173b8dcf64a3471238ae7abcd368d6402204cbf24f04b9aa2256d8901f0ed97866603d2be8324c2bfb7a37bf8fc90edd5b441210263e2dee22b1ddc5e11f6fab8bcd2378bdd19580d640501ea956ec0e786f93e76ffffffff013c660000000000001976a9146bfd5c7fbe21529d45803dbcf0c87dd3c71efbc288ac0000000000"

// Block 170's two-transaction tree: the first non-coinbase bitcoin
// transaction, proven against the block's known merkle root.
func TestComputeTSCRootBlock170(t *testing.T) {
	root, err := ComputeTSCRoot(
		"f4184fc596403b9d638783cf57adfe4c75c605f6356fbc91338530e9831e9e16",
		1,
		[]string{"b1fea52486ce0c62bb442b530a3f0132b826c74e473d1f2c220bfa78111c5082"},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "7dac2c5666815c17a3b36427de37bb9d2e2c5ccec3f8633eb91a4205cb4c10ff"
	if root != want {
		t.Fatalf("root = %s, want %s", root, want)
	}
}

func TestComputeTSCRootSingleTxBlock(t *testing.T) {
	// A one-transaction block: the txid IS the merkle root.
	txid := "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	root, err := ComputeTSCRoot(txid, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if root != txid {
		t.Fatalf("root = %s, want %s", root, txid)
	}
}

func TestComputeTSCRootDuplicateNode(t *testing.T) {
	// Odd trees duplicate the working hash; "*" must not error.
	if _, err := ComputeTSCRoot(
		"f4184fc596403b9d638783cf57adfe4c75c605f6356fbc91338530e9831e9e16",
		0,
		[]string{"*"},
	); err != nil {
		t.Fatal(err)
	}
}

func TestComputeTSCRootIndexOutOfRange(t *testing.T) {
	if _, err := ComputeTSCRoot(
		"f4184fc596403b9d638783cf57adfe4c75c605f6356fbc91338530e9831e9e16",
		4, // needs at least 3 levels, proof has 1
		[]string{"b1fea52486ce0c62bb442b530a3f0132b826c74e473d1f2c220bfa78111c5082"},
	); err == nil {
		t.Fatal("expected an error for an index deeper than the proof")
	}
}

// TestVerifyBEEFRejectsUnrelatedTxid is the core P1 guard: a valid BEEF that
// does not contain the requested transaction must fail before any header
// fetch, rather than reporting the unrelated proofs as valid. A nil client
// proves no network call is reached.
func TestVerifyBEEFRejectsUnrelatedTxid(t *testing.T) {
	raw, err := hex.DecodeString(brc62BEEF)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := "0000000000000000000000000000000000000000000000000000000000000001"
	_, err = VerifyBEEF(context.Background(), nil, unrelated, raw)
	if err == nil {
		t.Fatal("expected verification to fail for a txid absent from the BEEF")
	}
	if !strings.Contains(err.Error(), "does not contain") {
		t.Fatalf("error should explain the tx is absent, got: %v", err)
	}
}

// TestVerifyBEEFRejectsGarbage covers unparsable BEEF bytes.
func TestVerifyBEEFRejectsGarbage(t *testing.T) {
	if _, err := VerifyBEEF(context.Background(), nil, "ab", []byte{0x00, 0x01, 0x02}); err == nil {
		t.Fatal("expected an error parsing non-BEEF bytes")
	}
}
