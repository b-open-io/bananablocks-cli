package x402

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreSaveRefusesSecondPaymentForChallenge(t *testing.T) {
	s := NewProofStore(t.TempDir(), "https://h", "k")
	a := &SavedProof{ChallengeID: "c", Txid: "aa", ProofHeader: "h1", SavedAt: time.Now()}
	if err := s.Save(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(a); err != nil {
		t.Fatalf("re-saving the same payment must be allowed: %v", err)
	}
	if err := s.Save(&SavedProof{ChallengeID: "c", Txid: "bb", ProofHeader: "h2"}); err == nil {
		t.Fatal("a different payment for a pending challenge must be refused")
	}
	if err := s.Archive("c", "aa", "settled"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(&SavedProof{ChallengeID: "c", Txid: "bb", ProofHeader: "h2"}); err == nil {
		t.Fatal("a different payment for an archived challenge must be refused")
	}
}

func TestStoreArchiveOnlyMatchingTxid(t *testing.T) {
	s := NewProofStore(t.TempDir(), "https://h", "k")
	if err := s.Save(&SavedProof{ChallengeID: "c", Txid: "aa", ProofHeader: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Archive("c", "zz", "settled"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(); len(l) != 1 {
		t.Fatal("archiving another txid must leave the pending proof alone")
	}
	if err := s.Archive("c", "aa", "settled"); err != nil {
		t.Fatal(err)
	}
	if l, _ := s.List(); len(l) != 0 {
		t.Fatal("matching archive must clear the pending proof")
	}
}

func TestStoreLockExclusive(t *testing.T) {
	s := NewProofStore(t.TempDir(), "https://h", "k")
	unlock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(); err != ErrLocked {
		t.Fatalf("second lock: %v", err)
	}
	unlock()
	u2, err := s.Lock()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	u2()
}

func TestStoreKeySpellingsShareScope(t *testing.T) {
	dir := t.TempDir()
	a := NewProofStore(dir, "https://H.example/", " bb_live_x\n")
	for _, h := range []string{"https://h.example", "https://h.example:443", "HTTPS://h.example.:443/api"} {
		b := NewProofStore(dir, h, "bb_live_x")
		if a.Dir != b.Dir {
			t.Fatalf("%q must share the store of https://H.example/", h)
		}
	}
	for _, pair := range [][2]string{
		{"http://127.0.0.1:8080", "http://127.0.0.1:08080"},
		{"http://[2001:db8::1]:80", "http://[2001:0db8:0000:0000:0000:0000:0000:0001]"},
	} {
		if NewProofStore(dir, pair[0], "k").Dir != NewProofStore(dir, pair[1], "k").Dir {
			t.Fatalf("%q and %q must share a store", pair[0], pair[1])
		}
	}
	if NewProofStore(dir, "https://h.example:8443", "bb_live_x").Dir == a.Dir {
		t.Fatal("a non-default port is a different server")
	}
}

func TestStorePaidWithGlobMetacharactersInPath(t *testing.T) {
	s := NewProofStore(filepath.Join(t.TempDir(), "bb[prod]*?"), "https://h", "k")
	if err := s.Save(&SavedProof{ChallengeID: "c", Txid: "aa", ProofHeader: "h"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Archive("c", "aa", "abandoned"); err != nil {
		t.Fatal(err)
	}
	if p, err := s.Paid("c"); err != nil || p == nil {
		t.Fatalf("archived challenge not found under a metacharacter path: %v, %v", p, err)
	}
}

func TestStorePaidFailsClosedOnUnreadableArchive(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	s := NewProofStore(t.TempDir(), "https://h", "k")
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.Dir, "archive"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(s.Dir, "archive"), 0o700) })
	if _, err := s.Paid("c"); err == nil {
		t.Fatal("an unreadable archive must fail closed")
	}
}
