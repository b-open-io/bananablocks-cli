package cmd

import (
	"strings"
	"testing"
)

func TestNormalizeTxID(t *testing.T) {
	valid := "4A5E1E4BAAB89F3A32518A88C31BC87F618F76673E2CC77AB2127B7AFDEDA33B"
	got, err := normalizeTxID("  " + valid + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != strings.ToLower(valid) {
		t.Fatalf("got %q, want lowercase", got)
	}

	for _, bad := range []string{
		"",
		strings.Repeat("g", 64), // right length, non-hex
		strings.Repeat("a", 63), // too short
		strings.Repeat("a", 65), // too long
		"not-a-txid",
	} {
		if _, err := normalizeTxID(bad); err == nil {
			t.Errorf("expected %q to be rejected", bad)
		}
	}
}

// TestValidateHostScheme confirms an unusable host scheme is rejected up front
// with a clear message, while plain http/https (and bare hosts, which default
// to https) pass.
func TestValidateHostScheme(t *testing.T) {
	old := flagHost
	t.Cleanup(func() { flagHost = old })

	for _, bad := range []string{"ftp://example.com", "ws://example.com", "file:///etc/passwd"} {
		flagHost = bad
		if err := validateHost(); err == nil || !strings.Contains(err.Error(), "unsupported host scheme") {
			t.Errorf("host %q should be rejected, got %v", bad, err)
		}
	}

	for _, ok := range []string{"http://example.com", "https://example.com", "example.com"} {
		flagHost = ok
		if err := validateHost(); err != nil {
			t.Errorf("host %q should be accepted, got %v", ok, err)
		}
	}
}

func TestParseOutpointValidatesTxID(t *testing.T) {
	// Non-hex 64-char txid is rejected.
	if _, _, err := parseOutpoint(strings.Repeat("z", 64) + ":0"); err == nil {
		t.Fatal("expected non-hex txid to be rejected")
	}
	// Missing colon.
	if _, _, err := parseOutpoint(strings.Repeat("a", 64)); err == nil {
		t.Fatal("expected missing-colon error")
	}
	// Negative vout.
	if _, _, err := parseOutpoint(strings.Repeat("a", 64) + ":-1"); err == nil {
		t.Fatal("expected negative-vout error")
	}
	// Valid.
	txid, vout, err := parseOutpoint(strings.Repeat("a", 64) + ":3")
	if err != nil || vout != 3 || txid != strings.Repeat("a", 64) {
		t.Fatalf("got %q %d %v", txid, vout, err)
	}
}
