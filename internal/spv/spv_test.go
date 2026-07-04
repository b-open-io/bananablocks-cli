package spv

import "testing"

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
