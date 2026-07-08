package spv

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/api"
)

// Confirmation is the outcome of checking a computed merkle root against a
// source of truth for the block at a given height.
type Confirmation int

const (
	// ConfUnknown means the source could not decide (e.g. the header is not
	// synced yet or is too recent). This is inconclusive, not fraud. It is the
	// zero value so an absent or missing verdict defaults to inconclusive rather
	// than to a false fraud report.
	ConfUnknown Confirmation = iota
	// ConfConfirmed means the root is the merkle root of the block at that
	// height on the most-work chain.
	ConfConfirmed
	// ConfInvalid means the root is definitively not the merkle root of the
	// block at that height on the honest chain.
	ConfInvalid
)

func (c Confirmation) String() string {
	switch c {
	case ConfConfirmed:
		return "confirmed"
	case ConfInvalid:
		return "invalid"
	default:
		return "unverified"
	}
}

// MarshalJSON renders the verdict as its lowercase string, so a Result's typed
// State field serialises to the same JSON as a plain string would
// ("confirmed" / "invalid" / "unverified").
func (c Confirmation) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(c.String())), nil
}

// RootAtHeight is a computed merkle root paired with the block height its proof
// claims.
type RootAtHeight struct {
	Root   string
	Height uint32
}

// ConfirmedRoot is a verifier's verdict for one RootAtHeight. BlockHash and
// HeaderRoot are display metadata populated when the source knows them; they are
// not part of the security decision.
type ConfirmedRoot struct {
	RootAtHeight
	State      Confirmation
	BlockHash  string
	HeaderRoot string
}

// RootVerifier confirms computed merkle roots against a source of truth for the
// block headers. The headers-service implementation (HeadersVerifier) checks an
// independent, proof-of-work-validated chain; the IndexerVerifier is a weaker
// fallback that trusts the serving API. ConfirmRoots returns one result per
// input, in the same order; an error signals a transport/protocol failure, not
// a negative verdict (a root that simply does not match is reported in-band via
// its ConfirmedRoot.State).
type RootVerifier interface {
	ConfirmRoots(ctx context.Context, roots []RootAtHeight) ([]ConfirmedRoot, error)
}

// IndexerVerifier confirms roots by fetching each block from the BananaBlocks
// API and comparing merkle roots locally. It trusts that API, so it only
// catches proof corruption and endpoint bugs — not a server that fabricates a
// proof together with a matching header. See the package trust-boundary note.
type IndexerVerifier struct {
	Client *api.Client
}

// NewIndexerVerifier returns the fallback verifier backed by the given client.
func NewIndexerVerifier(c *api.Client) *IndexerVerifier {
	return &IndexerVerifier{Client: c}
}

// ConfirmRoots fetches one block per distinct height (every root at a height
// compares to the same header) and reports each root confirmed or invalid.
func (v *IndexerVerifier) ConfirmRoots(ctx context.Context, roots []RootAtHeight) ([]ConfirmedRoot, error) {
	type header struct {
		root string
		hash string
	}
	byHeight := make(map[uint32]header)
	out := make([]ConfirmedRoot, len(roots))
	for i, r := range roots {
		h, ok := byHeight[r.Height]
		if !ok {
			var blk blockInfo
			if err := v.Client.GetJSON(ctx, "/api/v1/block/"+strconv.FormatUint(uint64(r.Height), 10), nil, &blk); err != nil {
				return nil, fmt.Errorf("fetching header at height %d: %w", r.Height, err)
			}
			h = header{root: blk.MerkleRoot, hash: blk.Hash}
			byHeight[r.Height] = h
		}
		// Compare case-insensitively: the computed root is lowercase hex but a
		// server may report merkle_root in a different case.
		state := ConfInvalid
		switch {
		case h.root == "":
			// The block response carried no merkle root (block too recent, pruned,
			// or a field this fallback indexer doesn't populate). That is
			// inconclusive, not fraud — report ConfUnknown so a genuine proof is
			// not accused of being invalid just because the header was unavailable.
			state = ConfUnknown
		case strings.EqualFold(h.root, r.Root):
			state = ConfConfirmed
		}
		out[i] = ConfirmedRoot{RootAtHeight: r, State: state, BlockHash: h.hash, HeaderRoot: h.root}
	}
	return out, nil
}

// FallbackVerifier tries Primary and, only when it fails with a
// *HeadersServiceError (a transport/protocol outage, not a negative verdict),
// falls back to Secondary. OnFallback, when set, is called with the error just
// before the fallback. Falling back at the ConfirmRoots seam means the caller's
// proof parsing and root computation are not repeated.
type FallbackVerifier struct {
	Primary    RootVerifier
	Secondary  RootVerifier
	OnFallback func(err error)
}

func (f *FallbackVerifier) ConfirmRoots(ctx context.Context, roots []RootAtHeight) ([]ConfirmedRoot, error) {
	out, err := f.Primary.ConfirmRoots(ctx, roots)
	if err == nil {
		return out, nil
	}
	var se *HeadersServiceError
	if !errors.As(err, &se) {
		return nil, err // a genuine verifier error must not silently downgrade trust
	}
	if f.OnFallback != nil {
		f.OnFallback(err)
	}
	return f.Secondary.ConfirmRoots(ctx, roots)
}
