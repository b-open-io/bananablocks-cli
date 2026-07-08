package cmd

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReadTxHexArgWrappedHex is the P3 guard: line-wrapped hex (as produced by
// `xxd -p`) must be read as the same transaction as its single-line form, not
// hex-encoded again as if it were binary.
func TestReadTxHexArgWrappedHex(t *testing.T) {
	const flat = "0100beef0000ff00ff00"
	wrapped := "0100beef00\n00ff00ff00\n"
	f := filepath.Join(t.TempDir(), "wrapped.hex")
	if err := os.WriteFile(f, []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTxHexArg(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != flat {
		t.Fatalf("wrapped hex = %q, want %q", got, flat)
	}
}

// TestReadTxHexArgSpacedInline covers a hex string argument with spaces.
func TestReadTxHexArgSpacedInline(t *testing.T) {
	got, err := readTxHexArg("0100 beef 00")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0100beef00" {
		t.Fatalf("got %q", got)
	}
}

// TestReadTxHexArgBinaryFile confirms a genuinely binary file (non-hex bytes)
// is still hex-encoded from its original bytes.
func TestReadTxHexArgBinaryFile(t *testing.T) {
	bin := []byte{0x01, 0x00, 0xbe, 0xef, 0x80, 0x0a} // 0x80 is not a hex digit
	f := filepath.Join(t.TempDir(), "tx.bin")
	if err := os.WriteFile(f, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTxHexArg(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(bin) {
		t.Fatalf("binary file = %q, want %q", got, hex.EncodeToString(bin))
	}
}

// TestReadTxHexArgRejectsNonFile keeps the "not hex, not a file" error.
func TestReadTxHexArgRejectsNonFile(t *testing.T) {
	_, err := readTxHexArg("not-hex-and-not-a-file.zzz")
	if err == nil || !strings.Contains(err.Error(), "neither valid hex nor an existing file") {
		t.Fatalf("got %v", err)
	}
}

// TestReadTxHexArgOddLengthHex confirms a plain-string all-hex argument with an
// odd number of digits reports that precisely rather than the misleading
// "neither valid hex nor an existing file".
func TestReadTxHexArgOddLengthHex(t *testing.T) {
	_, err := readTxHexArg("0100beef0") // 9 hex digits
	if err == nil || !strings.Contains(err.Error(), "odd number of digits") {
		t.Fatalf("got %v, want an odd-length-hex error", err)
	}
}
