package x402

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SavedProof is a signed payment proof persisted before it is first
// submitted. The server may broadcast the payment on any submit, so once a
// proof exists the only safe way to settle its challenge is to resubmit the
// SAME proof: a fresh payment to the same challenge is never credited. The
// store lets a later run resubmit it instead of paying again.
type SavedProof struct {
	Host        string    `json:"host"`
	ChallengeID string    `json:"challenge_id"`
	Tier        string    `json:"tier"`
	AmountSats  int64     `json:"amount_sats"`
	Txid        string    `json:"txid"`
	ProofHeader string    `json:"proof_header"`
	SubmitPath  string    `json:"submit_path"`
	ExpiresAt   time.Time `json:"expires_at"`
	SavedAt     time.Time `json:"saved_at"`
}

// ProofStore keeps SavedProofs on disk, one file per challenge, in a
// directory scoped to a (host, API key) pair. The key itself is never
// written: the directory name is a hash of it.
type ProofStore struct {
	Dir string
}

// DefaultStateDir is where pending proofs live: $BB_STATE_DIR when set,
// otherwise the user config dir (durable, unlike a cache dir that the OS may
// clear while a payment is still pending).
func DefaultStateDir() (string, error) {
	if d := os.Getenv("BB_STATE_DIR"); d != "" {
		return d, nil
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "bananablocks-cli"), nil
}

// NewProofStore returns the store for host + apiKey under stateDir.
func NewProofStore(stateDir, host, apiKey string) *ProofStore {
	sum := sha256.Sum256([]byte(strings.ToLower(host) + "\x00" + apiKey))
	return &ProofStore{Dir: filepath.Join(stateDir, "x402-proofs", hex.EncodeToString(sum[:12]))}
}

func (s *ProofStore) path(challengeID string) string {
	sum := sha256.Sum256([]byte(challengeID))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:16])+".json")
}

// Save writes p durably (temp file + fsync + rename) with owner-only
// permissions. It must succeed before the proof is first submitted.
func (s *ProofStore) Save(p *SavedProof) error {
	if p.ChallengeID == "" {
		return errors.New("saved proof has no challenge_id")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".proof-*")
	if err != nil {
		return err
	}
	// Removing the temp file after a successful rename fails harmlessly.
	defer func() { _ = os.Remove(tmp.Name()) }()
	werr := tmp.Chmod(0o600)
	if werr == nil {
		_, werr = tmp.Write(raw)
	}
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return werr
	}
	return os.Rename(tmp.Name(), s.path(p.ChallengeID))
}

// Path reports where the proof for challengeID is (or would be) stored.
func (s *ProofStore) Path(challengeID string) string { return s.path(challengeID) }

// Delete removes the proof for challengeID; a missing file is not an error.
func (s *ProofStore) Delete(challengeID string) error {
	err := os.Remove(s.path(challengeID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List returns every saved proof, newest first. A missing directory is an
// empty list; an unreadable or corrupt file is an error, so a damaged proof
// can never be silently skipped and paid over.
func (s *ProofStore) List() ([]*SavedProof, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*SavedProof
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(s.Dir, e.Name())
		raw, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		var p SavedProof
		if err := json.Unmarshal(raw, &p); err != nil || p.ChallengeID == "" || p.ProofHeader == "" {
			return nil, fmt.Errorf("saved payment proof %s is unreadable; inspect it before paying again", full)
		}
		out = append(out, &p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}
