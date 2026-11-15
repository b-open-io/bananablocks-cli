package x402

import (
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
	if NewProofStore(dir, "https://h.example:8443", "bb_live_x").Dir == a.Dir {
		t.Fatal("a non-default port is a different server")
	}
}
