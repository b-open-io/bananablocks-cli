package x402

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
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

	// Set when the proof leaves the pending set.
	ArchivedAt    time.Time `json:"archived_at,omitempty"`
	ArchiveReason string    `json:"archive_reason,omitempty"`
}

// ProofStore keeps SavedProofs on disk in a directory scoped to a
// (host, API key) pair; the key itself is never written, only a hash of it.
// Pending proofs sit at the top level, one per challenge. A proof that
// settles, can never settle, or is abandoned moves to archive/, which also
// remembers every challenge that has ever been paid so none is paid twice.
type ProofStore struct {
	Dir string
}

// DefaultStateDir is where proofs live: $BB_STATE_DIR when set, otherwise
// the user config dir (durable, unlike a cache dir that the OS may clear
// while a payment is still pending).
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
	// The server trims the key before authenticating it, so equivalent
	// spellings must share one store (and one lock).
	sum := sha256.Sum256([]byte(canonicalOrigin(host) + "\x00" + strings.TrimSpace(apiKey)))
	return &ProofStore{Dir: filepath.Join(stateDir, "x402-proofs", hex.EncodeToString(sum[:12]))}
}

// canonicalOrigin reduces a host URL to scheme://host[:port] in lower case,
// dropping a default port and any path, so every spelling of one server
// shares one store and one lock.
func canonicalOrigin(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return scheme + "://" + host
}

// ErrLocked is returned by Lock when another process holds the store.
var ErrLocked = errors.New("another `bb key upgrade` (or inline upgrade) is running for this API key; wait for it to finish")

// Lock takes an exclusive, process-wide lock on the store. Every read of the
// pending set that can lead to a payment must hold it until the payment's
// proof is saved and settled, so two runs cannot pay the same challenge.
func (s *ProofStore) Lock() (unlock func(), err error) {
	if err := mkdirAllDurable(s.Dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		return nil, ErrLocked
	}
	return func() { _ = f.Close() }, nil
}

func challengeKey(challengeID string) string {
	sum := sha256.Sum256([]byte(challengeID))
	return hex.EncodeToString(sum[:16])
}

func (s *ProofStore) archiveDir() string { return filepath.Join(s.Dir, "archive") }

// Path reports where the pending proof for challengeID is (or would be).
func (s *ProofStore) Path(challengeID string) string {
	return filepath.Join(s.Dir, challengeKey(challengeID)+".json")
}

// Save writes p as the pending proof for its challenge, durably (temp file +
// fsync + rename) with owner-only permissions. It refuses a challenge that
// already has a pending or archived payment with a different transaction, so
// a proof is never overwritten and no challenge is funded twice.
func (s *ProofStore) Save(p *SavedProof) error {
	if p.ChallengeID == "" {
		return errors.New("saved proof has no challenge_id")
	}
	if prev, err := s.Paid(p.ChallengeID); err != nil {
		return err
	} else if prev != nil && prev.Txid != p.Txid {
		return fmt.Errorf("challenge %s already has a payment (%s); refusing to record a second one", p.ChallengeID, prev.Txid)
	}
	return writeJSONAtomic(s.Dir, s.Path(p.ChallengeID), p)
}

// Archive moves the pending proof for challengeID out of the pending set,
// but only when it is still the proof for txid: a pending proof that another
// payment replaced is left alone.
func (s *ProofStore) Archive(challengeID, txid, reason string) error {
	cur, err := readProof(s.Path(challengeID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Txid != txid {
		return nil
	}
	cur.ArchivedAt = time.Now().UTC()
	cur.ArchiveReason = reason
	dst := filepath.Join(s.archiveDir(), challengeKey(challengeID)+"-"+safeName(txid)+".json")
	// The archive entry is durable before the pending copy goes, so a crash
	// in between leaves the payment recorded in at least one place.
	if err := writeJSONAtomic(s.archiveDir(), dst, cur); err != nil {
		return err
	}
	if err := os.Remove(s.Path(challengeID)); err != nil {
		return err
	}
	return syncDir(s.Dir)
}

// Paid returns a pending or archived proof for challengeID, or nil when that
// challenge has never been paid from this store.
func (s *ProofStore) Paid(challengeID string) (*SavedProof, error) {
	p, err := readProof(s.Path(challengeID))
	if err == nil {
		return p, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(s.archiveDir(), challengeKey(challengeID)+"-*.json"))
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		p, err := readProof(m)
		if err != nil {
			return nil, err
		}
		if p.ChallengeID == challengeID {
			return p, nil
		}
	}
	return nil, nil
}

// List returns every pending proof, newest first. A missing directory is an
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
		p, err := readProof(filepath.Join(s.Dir, e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SavedAt.After(out[j].SavedAt) })
	return out, nil
}

func readProof(path string) (*SavedProof, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p SavedProof
	if err := json.Unmarshal(raw, &p); err != nil || p.ChallengeID == "" || p.ProofHeader == "" {
		return nil, fmt.Errorf("saved payment proof %s is unreadable; inspect it before paying again", path)
	}
	return &p, nil
}

// mkdirAllDurable creates dir and then flushes every directory from dir up
// to the filesystem root, so the whole path (including entries left by an
// earlier, interrupted run) survives a power loss.
func mkdirAllDurable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	for d := abs; ; d = filepath.Dir(d) {
		if err := syncDir(d); err != nil {
			return err
		}
		if filepath.Dir(d) == d {
			return nil
		}
	}
}

// writeJSONAtomic writes v to dst via a synced temp file and rename, then
// syncs the directory so the new name survives a power loss: a proof is only
// reported saved once a crash can no longer lose it.
func writeJSONAtomic(dir, dst string, v any) error {
	if err := mkdirAllDurable(dir); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".proof-*")
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
	if err := renameDurable(tmp.Name(), dst); err != nil {
		return err
	}
	return syncDir(dir)
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			b.WriteRune(r)
		}
		if b.Len() >= 64 {
			break
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}
