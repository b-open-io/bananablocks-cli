// Package x402 implements the client side of BananaBlocks' x402
// (BRC-120-style, scheme bsv-tx-v1) pay-to-upgrade flow: request a 402
// challenge, build a signed BSV transaction paying the challenge's derived
// P2PKH script, and submit it as an X402-Proof header. The server verifies,
// broadcasts via ARC, and bumps the API key's tier — the client never
// broadcasts the payment itself.
package x402

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// UpgradePath is the single x402-conformant endpoint for buying a tier.
const UpgradePath = "/api/v1/key/upgrade"

// Version is the payment scheme identifier.
const Version = "bsv-tx-v1"

// Challenge mirrors the server's 402 challenge JSON (body "challenge" field,
// also base64url-encoded in the X402-Challenge header).
type Challenge struct {
	Version               string    `json:"version"`
	ChallengeID           string    `json:"challenge_id"`
	Tier                  string    `json:"tier"`
	DurationDays          int       `json:"duration_days"`
	AmountSats            int64     `json:"amount_sats"`
	PayeeLockingScriptHex string    `json:"payee_locking_script_hex"`
	PayeeAddress          string    `json:"payee_address"`
	ExpiresAt             time.Time `json:"expires_at"`
	PayURL                string    `json:"pay_url"`
}

// Proof is the payload of the X402-Proof request header. RawTxBase64 is
// standard (padded) base64 of the raw transaction bytes; the header itself is
// base64url of this JSON.
type Proof struct {
	Version     string `json:"version"`
	ChallengeID string `json:"challenge_id"`
	RawTxBase64 string `json:"rawtx_base64"`
	Txid        string `json:"txid,omitempty"`
}

// UpgradeResult is the 200 response after a settled payment.
type UpgradeResult struct {
	Tier          string    `json:"tier"`
	TierExpiresAt time.Time `json:"tier_expires_at"`
	Txid          string    `json:"txid"`
	AmountSats    int64     `json:"amount_sats"`
}

// ChallengeBody is the JSON body of a 402 response.
type ChallengeBody struct {
	Error     string     `json:"error"`
	Challenge *Challenge `json:"challenge"`
}

// EncodeProofHeader renders the X402-Proof header value.
func EncodeProofHeader(p *Proof) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeChallengeHeader parses an X402-Challenge header value.
func DecodeChallengeHeader(v string) (*Challenge, error) {
	raw, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		if raw, err = base64.URLEncoding.DecodeString(v); err != nil {
			return nil, fmt.Errorf("X402-Challenge header is not base64url: %w", err)
		}
	}
	var c Challenge
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("X402-Challenge header is not JSON: %w", err)
	}
	return &c, nil
}
