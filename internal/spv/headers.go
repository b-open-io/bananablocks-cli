package spv

import (
	"context"
	"net/http"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/api"
)

// HeadersVerifier confirms merkle roots against an independent block-headers
// service (github.com/bsv-blockchain/block-headers-service), which tracks the
// most-work header chain and validates its proof of work. This is a stronger
// trust anchor than the serving indexer: confirming a root here means it
// belongs to the honest chain at the claimed height, so a malicious indexer can
// no longer pass a fabricated proof by also serving a matching fabricated
// header.
type HeadersVerifier struct {
	Client *api.Client
}

// NewHeadersVerifier builds a verifier for the given service base URL. token is
// optional (sent as a Bearer credential when non-empty). httpc may be nil, in
// which case http.DefaultClient is used.
func NewHeadersVerifier(baseURL, token string, httpc *http.Client) *HeadersVerifier {
	if httpc == nil {
		httpc = http.DefaultClient
	}
	return &HeadersVerifier{Client: &api.Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    token, // api.Client sends this as Authorization: Bearer when set
		UserAgent: "bananablocks-cli",
		HTTP:      httpc,
	}}
}

// HeadersServiceError wraps a transport or protocol failure talking to the
// header service, as opposed to a negative verdict (which is returned in-band in
// the results). Callers may treat it as a signal to fall back to a weaker
// verifier rather than reporting the proof as fraudulent.
type HeadersServiceError struct{ Err error }

func (e *HeadersServiceError) Error() string { return e.Err.Error() }
func (e *HeadersServiceError) Unwrap() error { return e.Err }

// merkleRootRequest / confirmationResponse mirror the service's
// POST /api/v1/chain/merkleroot/verify contract.
type merkleRootRequest struct {
	MerkleRoot  string `json:"merkleRoot"`
	BlockHeight uint32 `json:"blockHeight"`
}

type confirmationResponse struct {
	ConfirmationState string `json:"confirmationState"`
	Confirmations     []struct {
		BlockHash    string `json:"blockHash"`
		BlockHeight  uint32 `json:"blockHeight"`
		MerkleRoot   string `json:"merkleRoot"`
		Confirmation string `json:"confirmation"`
	} `json:"confirmations"`
}

func parseConfirmation(s string) Confirmation {
	switch strings.ToUpper(s) {
	case "CONFIRMED":
		return ConfConfirmed
	case "INVALID":
		return ConfInvalid
	default: // UNABLE_TO_VERIFY, or anything unexpected → inconclusive
		return ConfUnknown
	}
}

// ConfirmRoots posts every (root, height) pair to the service in one request
// and maps the per-entry verdicts back to the inputs by (root, height). Any
// transport/protocol failure is wrapped as a *HeadersServiceError; a pair the
// service omits from its response is reported ConfUnknown rather than silently
// confirmed.
func (v *HeadersVerifier) ConfirmRoots(ctx context.Context, roots []RootAtHeight) ([]ConfirmedRoot, error) {
	if len(roots) == 0 {
		return nil, nil
	}
	reqBody := make([]merkleRootRequest, len(roots))
	for i, r := range roots {
		reqBody[i] = merkleRootRequest{MerkleRoot: r.Root, BlockHeight: r.Height}
	}
	var cr confirmationResponse
	if err := v.Client.PostJSONBody(ctx, "/api/v1/chain/merkleroot/verify", nil, reqBody, &cr); err != nil {
		// Every PostJSONBody error is a transport/protocol failure (non-2xx,
		// unreadable, or undecodable), never an in-band verdict.
		return nil, &HeadersServiceError{Err: err}
	}

	// Index the echoed confirmations by (root, height); the service is
	// case-insensitive on hex, so normalise for the lookup.
	type key struct {
		root   string
		height uint32
	}
	got := make(map[key]ConfirmedRoot, len(cr.Confirmations))
	for _, c := range cr.Confirmations {
		state := parseConfirmation(c.Confirmation)
		headerRoot := ""
		if state == ConfConfirmed {
			// The service only reveals the block's real root when it matches; on
			// INVALID it echoes the queried root but that is not authoritative.
			headerRoot = c.MerkleRoot
		}
		got[key{strings.ToLower(c.MerkleRoot), c.BlockHeight}] = ConfirmedRoot{
			RootAtHeight: RootAtHeight{Root: c.MerkleRoot, Height: c.BlockHeight},
			State:        state,
			BlockHash:    c.BlockHash,
			HeaderRoot:   headerRoot,
		}
	}

	out := make([]ConfirmedRoot, len(roots))
	for i, r := range roots {
		if c, ok := got[key{strings.ToLower(r.Root), r.Height}]; ok {
			c.Root = r.Root // preserve the caller's casing
			out[i] = c
			continue
		}
		out[i] = ConfirmedRoot{RootAtHeight: r, State: ConfUnknown}
	}
	return out, nil
}
