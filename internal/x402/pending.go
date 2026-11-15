package x402

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// PendingUpgrade is a payment proof that was saved before it was submitted and
// has not settled yet. The server broadcasts the payment on the first submit,
// so from then on the payer's coins may be spent: the only way to get the tier
// is to resubmit this same proof, never to pay the challenge again.
type PendingUpgrade struct {
	// KeyFingerprint identifies the (host, API key) pair the proof belongs to
	// without storing the key itself (see KeyFingerprint).
	KeyFingerprint string    `json:"key_fingerprint"`
	Host           string    `json:"host"`
	ChallengeID    string    `json:"challenge_id"`
	Tier           string    `json:"tier"`
	AmountSats     int64     `json:"amount_sats"`
	Txid           string    `json:"txid"`
	Proof          string    `json:"proof"` // the X402-Proof header value, resubmitted verbatim
	PayURL         string    `json:"pay_url"`
	CreatedAt      time.Time `json:"created_at"`
	// ExpiresAt is the challenge's expires_at, when known. It bounds how long
	// a closed entry must be kept (see closedRetention).
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// FirstGoneAt is when the server first answered 410 for this proof. The
	// challenge had expired by then, so for an entry saved without ExpiresAt
	// its settlement window ends at most a day after this.
	FirstGoneAt time.Time `json:"first_gone_at,omitempty"`
	// LastSubmitAt is when the proof was last sent to the server. A settle can
	// only happen on a submit, so this bounds how recently it could have
	// settled.
	LastSubmitAt time.Time `json:"last_submit_at,omitempty"`

	// ClosedAt marks a proof the server refused for good. It is no longer
	// resubmitted and no longer blocks new upgrades, but its challenge stays
	// recorded as paid: only one payment settles a challenge, and the server
	// re-serves an open one, so paying it again would never be credited.
	ClosedAt     time.Time `json:"closed_at,omitempty"`
	ClosedReason string    `json:"closed_reason,omitempty"`
}

// Closed reports whether the server refused this proof for good.
func (e PendingUpgrade) Closed() bool { return !e.ClosedAt.IsZero() }

// LastActivity is the latest time the proof could have settled: its last
// submit, or when it was saved if no submit is recorded.
func (e PendingUpgrade) LastActivity() time.Time {
	if e.LastSubmitAt.After(e.CreatedAt) {
		return e.LastSubmitAt
	}
	return e.CreatedAt
}

type pendingFile struct {
	Version int              `json:"version"`
	Pending []PendingUpgrade `json:"pending"`
}

// PendingStore keeps PendingUpgrade entries in one JSON file, readable and
// writable only by the owner.
type PendingStore struct {
	Path string
}

// DefaultPendingPath is <user config dir>/bb/pending-upgrades.json.
func DefaultPendingPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("locating the user config directory for saved x402 payments: %w", err)
	}
	return filepath.Join(dir, "bb", "pending-upgrades.json"), nil
}

// KeyFingerprint derives a stable identifier for an API key on a host, so
// saved proofs are matched to the key that paid them without writing the key
// to disk. Spellings of the same host and key that the server treats alike
// (scheme and host case, a default port, a trailing slash, whitespace around
// the key, which the server trims) give the same fingerprint, so a rerun with
// a differently spelled --host or BB_API_KEY still finds the saved proof.
func KeyFingerprint(host, apiKey string) string {
	sum := sha256.Sum256([]byte("bb-x402-pending\x00" + canonicalHost(host) + "\x00" + strings.TrimSpace(apiKey)))
	return hex.EncodeToString(sum[:16])
}

// canonicalHost lowercases the scheme and host, drops the scheme's default
// port and any trailing slash. The path keeps its case.
func canonicalHost(h string) string {
	h = strings.TrimRight(strings.TrimSpace(h), "/")
	u, err := url.Parse(h)
	if err != nil || u.Host == "" {
		return strings.ToLower(h)
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		port = ""
	}
	if strings.Contains(host, ":") { // IPv6 literal
		host = "[" + host + "]"
	}
	if port != "" {
		host += ":" + port
	}
	return scheme + "://" + host + strings.TrimRight(u.EscapedPath(), "/")
}

// load returns every saved entry. A missing or empty file holds none. A file
// that does not parse is an error rather than "no entries": it may record a
// payment that is still settling, and treating it as empty would let the
// caller pay again.
func (s *PendingStore) load() ([]PendingUpgrade, error) {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading saved x402 payments: %w", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	var f pendingFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("saved x402 payments file %s is not valid JSON (%v); it may record a payment that is still settling, so fix or move it aside before paying again", s.Path, err)
	}
	return f.Pending, nil
}

// save replaces the file atomically (temp file + rename) with mode 0600, or
// removes it when no entries remain.
func (s *PendingStore) save(entries []PendingUpgrade) error {
	if len(entries) == 0 {
		if err := os.Remove(s.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	raw, err := json.MarshalIndent(pendingFile{Version: 1, Pending: entries}, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".pending-upgrades-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op once renamed
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, s.Path)
}

// ForKey returns the open (not closed) entries saved for a key fingerprint,
// oldest first.
func (s *PendingStore) ForKey(fingerprint string) ([]PendingUpgrade, error) {
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []PendingUpgrade
	for _, e := range all {
		if e.KeyFingerprint == fingerprint && !e.Closed() {
			out = append(out, e)
		}
	}
	return out, nil
}

// ForChallenge returns the entries saved for a challenge id under any key
// fingerprint, closed ones included, oldest first. The server scopes a challenge to the key it was
// issued to, so a challenge id it serves again belongs to the same key even
// when the fingerprint differs (e.g. the host was spelled another way).
func (s *PendingStore) ForChallenge(challengeID string) ([]PendingUpgrade, error) {
	if challengeID == "" {
		return nil, nil
	}
	all, err := s.load()
	if err != nil {
		return nil, err
	}
	var out []PendingUpgrade
	for _, e := range all {
		if e.ChallengeID == challengeID {
			out = append(out, e)
		}
	}
	return out, nil
}

// Put saves e, replacing any entry for the same key and challenge.
func (s *PendingStore) Put(e PendingUpgrade) error {
	if e.KeyFingerprint == "" || e.ChallengeID == "" || e.Proof == "" {
		return errors.New("a saved x402 payment needs a key fingerprint, challenge id and proof")
	}
	return s.mutate(func(all []PendingUpgrade) ([]PendingUpgrade, bool) {
		replaced := false
		for i := range all {
			if all[i].KeyFingerprint == e.KeyFingerprint && all[i].ChallengeID == e.ChallengeID {
				all[i] = e
				replaced = true
			}
		}
		if !replaced {
			all = append(all, e)
		}
		return all, true
	})
}

// Delete removes the entry for a key and challenge. Deleting an entry that
// is not there is not an error.
func (s *PendingStore) Delete(fingerprint, challengeID string) error {
	return s.mutate(func(all []PendingUpgrade) ([]PendingUpgrade, bool) {
		kept := all[:0]
		for _, e := range all {
			if e.KeyFingerprint == fingerprint && e.ChallengeID == challengeID {
				continue
			}
			kept = append(kept, e)
		}
		return kept, len(kept) != len(all)
	})
}

// mutate runs one read-modify-write of the file under a process-shared lock,
// so concurrent runs cannot lose each other's updates (a stale snapshot
// saved over a newer file would erase a proof that was just submitted).
func (s *PendingStore) mutate(fn func([]PendingUpgrade) ([]PendingUpgrade, bool)) error {
	unlock, err := s.lock(s.Path+".lock", false)
	if err != nil {
		return err
	}
	defer unlock()
	all, err := s.load()
	if err != nil {
		return err
	}
	next, changed := fn(all)
	if !changed {
		return nil
	}
	return s.save(next)
}

// ErrUpgradeRunning is returned by LockRun when another process holds it.
var ErrUpgradeRunning = errors.New("another `bb key upgrade` (or inline upgrade) is running; wait for it to finish — two runs at once could pay the same challenge twice")

// LockRun takes the lock a paying run holds from its first look at the saved
// payments until its own payment is saved and settled, so two runs cannot
// both see nothing saved and pay the same challenge. It fails at once if
// another run holds it.
func (s *PendingStore) LockRun() (func(), error) {
	unlock, err := s.lock(s.Path+".run.lock", true)
	if err != nil {
		if errors.Is(err, errLockBusy) {
			return nil, ErrUpgradeRunning
		}
		return nil, err
	}
	return unlock, nil
}

var errLockBusy = errors.New("lock busy")

func (s *PendingStore) lock(path string, nonBlocking bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f, nonBlocking); err != nil {
		_ = f.Close()
		if nonBlocking {
			return nil, errLockBusy
		}
		return nil, fmt.Errorf("locking %s: %w", path, err)
	}
	return func() { _ = f.Close() }, nil
}

// closedRetention is how long past its challenge's expires_at a closed entry
// is kept as the record that its challenge was paid. The server never
// re-serves an expired challenge, and settles a payment at most a day after
// expiry, so a week past expiry is ample. An entry whose expiry is unknown is
// kept: dropping it could let a still-open challenge be paid again.
const closedRetention = 7 * 24 * time.Hour

func (e PendingUpgrade) closedExpired(now time.Time) bool {
	return e.Closed() && !e.ExpiresAt.IsZero() && now.Sub(e.ExpiresAt) > closedRetention
}

// Close marks the entry for a key and challenge as refused for good (see
// PendingUpgrade.ClosedAt) and drops closed entries past closedRetention.
func (s *PendingStore) Close(fingerprint, challengeID, reason string, now time.Time) error {
	return s.mutate(func(all []PendingUpgrade) ([]PendingUpgrade, bool) {
		kept := all[:0]
		for _, e := range all {
			if e.KeyFingerprint == fingerprint && e.ChallengeID == challengeID && !e.Closed() {
				e.ClosedAt = now.UTC()
				e.ClosedReason = reason
			}
			if e.closedExpired(now) {
				continue
			}
			kept = append(kept, e)
		}
		return kept, true
	})
}
