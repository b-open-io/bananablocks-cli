package x402

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func entry(fp, ch string) PendingUpgrade {
	return PendingUpgrade{
		KeyFingerprint: fp,
		Host:           "https://bb.test",
		ChallengeID:    ch,
		Tier:           "pro",
		Txid:           "tx-" + ch,
		Proof:          "proof-" + ch,
		PayURL:         UpgradePath,
		CreatedAt:      time.Unix(1_700_000_000, 0).UTC(),
	}
}

func TestPendingStoreMissingFile(t *testing.T) {
	s := &PendingStore{Path: filepath.Join(t.TempDir(), "bb", "pending-upgrades.json")}
	got, err := s.ForKey("fp")
	if err != nil || len(got) != 0 {
		t.Fatalf("missing file: got %v, %v; want no entries and no error", got, err)
	}
	if err := s.Delete("fp", "ch"); err != nil {
		t.Fatalf("deleting from a missing file: %v", err)
	}
}

func TestPendingStorePutForKeyDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bb")
	s := &PendingStore{Path: filepath.Join(dir, "pending-upgrades.json")}

	if err := s.Put(entry("fpA", "ch1")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 600", perm)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Fatalf("dir mode = %o, want 700", perm)
	}

	if err := s.Put(entry("fpA", "ch2")); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(entry("fpB", "ch3")); err != nil {
		t.Fatal(err)
	}
	// Same key + challenge replaces rather than duplicates.
	updated := entry("fpA", "ch1")
	updated.Proof = "proof-ch1-v2"
	if err := s.Put(updated); err != nil {
		t.Fatal(err)
	}

	a, err := s.ForKey("fpA")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != 2 || a[0].ChallengeID != "ch1" || a[0].Proof != "proof-ch1-v2" || a[1].ChallengeID != "ch2" {
		t.Fatalf("ForKey(fpA) = %+v", a)
	}
	b, _ := s.ForKey("fpB")
	if len(b) != 1 || b[0].ChallengeID != "ch3" {
		t.Fatalf("ForKey(fpB) = %+v", b)
	}

	if err := s.Delete("fpA", "ch1"); err != nil {
		t.Fatal(err)
	}
	a, _ = s.ForKey("fpA")
	if len(a) != 1 || a[0].ChallengeID != "ch2" {
		t.Fatalf("after delete ForKey(fpA) = %+v", a)
	}
	if b, _ := s.ForKey("fpB"); len(b) != 1 {
		t.Fatalf("delete touched another key: %+v", b)
	}

	// Removing the last entry removes the file.
	_ = s.Delete("fpA", "ch2")
	_ = s.Delete("fpB", "ch3")
	if _, err := os.Stat(s.Path); !os.IsNotExist(err) {
		t.Fatalf("empty store should remove the file, stat err = %v", err)
	}
}

// A file written with loose permissions (e.g. by hand) is tightened to 0600
// on the next write.
func TestPendingStoreTightensPermissions(t *testing.T) {
	s := &PendingStore{Path: filepath.Join(t.TempDir(), "pending-upgrades.json")}
	if err := os.WriteFile(s.Path, []byte(`{"version":1,"pending":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(entry("fp", "ch")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file mode = %o, want 600", perm)
	}
}

// A malformed file fails closed: it may hold a payment that is still
// settling, so it is neither read as empty nor overwritten.
func TestPendingStoreMalformedFile(t *testing.T) {
	s := &PendingStore{Path: filepath.Join(t.TempDir(), "pending-upgrades.json")}
	const junk = `{"version":1,"pending":[{"challenge_id":`
	if err := os.WriteFile(s.Path, []byte(junk), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ForKey("fp"); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("malformed file: want a parse error, got %v", err)
	}
	if err := s.Put(entry("fp", "ch")); err == nil {
		t.Fatal("Put over a malformed file must fail, not overwrite it")
	}
	if err := s.Delete("fp", "ch"); err == nil {
		t.Fatal("Delete over a malformed file must fail, not overwrite it")
	}
	raw, _ := os.ReadFile(s.Path)
	if string(raw) != junk {
		t.Fatalf("malformed file was modified: %q", raw)
	}
}

func TestPendingStoreEmptyFile(t *testing.T) {
	s := &PendingStore{Path: filepath.Join(t.TempDir(), "pending-upgrades.json")}
	if err := os.WriteFile(s.Path, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ForKey("fp"); err != nil || len(got) != 0 {
		t.Fatalf("empty file: got %v, %v", got, err)
	}
}

func TestPendingStorePutRequiresProof(t *testing.T) {
	s := &PendingStore{Path: filepath.Join(t.TempDir(), "pending-upgrades.json")}
	e := entry("fp", "ch")
	e.Proof = ""
	if err := s.Put(e); err == nil {
		t.Fatal("an entry without a proof cannot be resumed and must be refused")
	}
}

func TestKeyFingerprint(t *testing.T) {
	const key = "bb_live_secret123"
	fp := KeyFingerprint("https://bananablocks.com", key)
	if strings.Contains(fp, "secret") || len(fp) != 32 {
		t.Fatalf("fingerprint %q leaks the key or has the wrong length", fp)
	}
	if fp != KeyFingerprint("https://bananablocks.com", key) {
		t.Fatal("fingerprint is not stable")
	}
	if fp == KeyFingerprint("https://other.example", key) || fp == KeyFingerprint("https://bananablocks.com", key+"x") {
		t.Fatal("fingerprint must differ by host and by key")
	}
}
