package x402

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreSaveRefusesSecondPaymentForChallenge(t *testing.T) {
	s := NewProofStore(t.TempDir(), "k")
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
	s := NewProofStore(t.TempDir(), "k")
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
	s := NewProofStore(t.TempDir(), "k")
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
	if NewProofStore(dir, " bb_live_x\n").Dir != NewProofStore(dir, "bb_live_x").Dir {
		t.Fatal("equivalent key spellings must share a store")
	}
	if NewProofStore(dir, "bb_live_y").Dir == NewProofStore(dir, "bb_live_x").Dir {
		t.Fatal("different keys must not share a store")
	}
}

func TestCanonicalOrigin(t *testing.T) {
	same := [][2]string{
		{"https://H.example/", "https://h.example:443"},
		{"https://h.example", "HTTPS://h.example.:443/api"},
		{"http://127.0.0.1:8080", "http://127.0.0.1:08080"},
		{"http://[2001:db8::1]:80", "http://[2001:0db8:0000:0000:0000:0000:0000:0001]"},
	}
	for _, p := range same {
		if CanonicalOrigin(p[0]) != CanonicalOrigin(p[1]) {
			t.Errorf("%q and %q should match", p[0], p[1])
		}
	}
	if CanonicalOrigin("https://h.example:8443") == CanonicalOrigin("https://h.example") {
		t.Error("a non-default port is a different server")
	}
}

func TestStorePaidWithGlobMetacharactersInPath(t *testing.T) {
	s := NewProofStore(filepath.Join(t.TempDir(), "bb[prod]*?"), "k")
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
	s := NewProofStore(t.TempDir(), "k")
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
