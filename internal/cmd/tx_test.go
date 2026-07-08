package cmd

import (
	"strings"
	"testing"

	"github.com/b-open-io/bananablocks-cli/internal/spv"
)

// TestInvalidDetail checks that a locally-decided INVALID (proof-internal
// inconsistency or a failed transitive ancestor) does not cite "block 0" or
// attribute the verdict to a header service that was never consulted; only a
// chain-checked leaf names the block and source.
func TestInvalidDetail(t *testing.T) {
	const source = "an independent header service (https://example)"

	// TSC target mismatch: BlockHeight 0, HeaderRoot = claimed target.
	got := invalidDetail(&spv.Result{ComputedRoot: "abc", HeaderRoot: "def"}, source)
	if strings.Contains(got, "block 0") || strings.Contains(got, source) {
		t.Errorf("target-mismatch detail should not cite block 0 or the source: %q", got)
	}
	if !strings.Contains(got, "target") || !strings.Contains(got, "abc") {
		t.Errorf("target-mismatch detail should compare target and computed root: %q", got)
	}

	// Transitive subject: BlockHeight 0, no HeaderRoot.
	got = invalidDetail(&spv.Result{Txid: "tx1", ComputedRoot: "(no direct merkle path; proven via linked ancestors)"}, source)
	if strings.Contains(got, "block 0") {
		t.Errorf("transitive detail should not cite block 0: %q", got)
	}
	if !strings.Contains(got, "ancestor") {
		t.Errorf("transitive detail should mention linked ancestors: %q", got)
	}

	// Chain-checked leaf: real height → names block and source.
	got = invalidDetail(&spv.Result{Txid: "tx2", ComputedRoot: "abc", BlockHeight: 170}, source)
	if !strings.Contains(got, "block 170") || !strings.Contains(got, source) {
		t.Errorf("chain-checked detail should cite the real block and source: %q", got)
	}
}

func TestUnverifiedDetail(t *testing.T) {
	const source = "an independent header service (https://example)"
	if got := unverifiedDetail(&spv.Result{Txid: "tx1"}, source); strings.Contains(got, "block 0") {
		t.Errorf("transitive unverified detail should not cite block 0: %q", got)
	}
	if got := unverifiedDetail(&spv.Result{BlockHeight: 999}, source); !strings.Contains(got, "999") {
		t.Errorf("chain-checked unverified detail should cite the block: %q", got)
	}
}

// TestHeadersConfigError checks that an unknown chain surfaces a clean config
// error instead of silently downgrading, while --no-headers and a known chain
// are both fine.
func TestHeadersConfigError(t *testing.T) {
	defer func(chain, url string, no bool) {
		flagChain, flagHeadersURL, flagNoHeaders = chain, url, no
	}(flagChain, flagHeadersURL, flagNoHeaders)
	t.Setenv("BB_HEADERS_URL", "")

	flagHeadersURL, flagNoHeaders = "", false

	flagChain = "regtest"
	if err := headersConfigError(); err == nil {
		t.Error("an unknown chain with no --headers-url should error, not downgrade")
	}

	flagChain, flagNoHeaders = "regtest", true
	if err := headersConfigError(); err != nil {
		t.Errorf("--no-headers should make an unknown chain fine, got %v", err)
	}

	flagChain, flagNoHeaders = "main", false
	if err := headersConfigError(); err != nil {
		t.Errorf("a known chain should have a default header service, got %v", err)
	}
}

// TestVerifyRejectedWithHex checks that --verify combined with --hex is
// rejected up front, since --hex returns raw hex before any verification would
// run (the combination would otherwise silently do nothing).
func TestVerifyRejectedWithHex(t *testing.T) {
	defer func(h, v bool) { txHex, txVerify = h, v }(txHex, txVerify)
	txHex, txVerify = true, true
	err := txCmd.RunE(txCmd, []string{strings.Repeat("ab", 32)})
	if err == nil || !strings.Contains(err.Error(), "--verify cannot be combined with --hex") {
		t.Fatalf("want hex+verify rejection, got %v", err)
	}
}
