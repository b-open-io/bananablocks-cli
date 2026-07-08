// Package spv verifies merkle proofs (TSC and BEEF/BUMP) locally, then confirms
// the computed roots against a source of truth for the block headers via a
// RootVerifier.
//
// Trust boundary: the strength of verification depends on the RootVerifier used.
// HeadersVerifier checks each computed root against an independent block-headers
// service that tracks the most-work, proof-of-work-validated chain, so
// confirming a root means it genuinely belongs to the honest chain at the
// claimed height — a malicious indexer can no longer pass a fabricated proof by
// also serving a matching fabricated header. IndexerVerifier is a weaker
// fallback: it fetches the block from the same API that supplied the proof and
// compares roots locally, which catches proof corruption and endpoint bugs but
// does not remove trust in that server. Either way the proof's own math is
// checked locally first.
package spv

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// TSCProof mirrors the explorer's /api/v1/tx/{txid}/proof response. Target is
// the block's merkle root; a node of "*" duplicates the working hash.
type TSCProof struct {
	Index  uint64   `json:"index"`
	TxOrID string   `json:"txOrId"`
	Target string   `json:"target"`
	Nodes  []string `json:"nodes"`
}

// Result reports one verified proof.
type Result struct {
	Txid         string `json:"txid"`
	ComputedRoot string `json:"computed_root"`
	BlockHeight  int    `json:"block_height,omitempty"`
	BlockHash    string `json:"block_hash,omitempty"`
	HeaderRoot   string `json:"header_merkle_root"`
	Valid        bool   `json:"valid"`
	// State is the verifier's verdict (confirmed / invalid / unverified). It is
	// typed so callers switch on the constant rather than a string literal;
	// MarshalJSON renders it to the same lowercase string in output.
	State Confirmation `json:"state"`
}

// blockInfo is the slice of the block JSON we need.
type blockInfo struct {
	Hash       string `json:"hash"`
	Height     int    `json:"height"`
	MerkleRoot string `json:"merkle_root"`
}

// txInfo is the slice of the tx JSON we need.
type txInfo struct {
	BlockHash   string `json:"block_hash"`
	BlockHeight int    `json:"block_height"`
}

// ComputeTSCRoot folds the TSC node path over the txid and returns the merkle
// root in display (reversed-hex) order.
func ComputeTSCRoot(txid string, index uint64, nodes []string) (string, error) {
	cur, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return "", fmt.Errorf("invalid txid: %w", err)
	}
	idx := index
	for i, n := range nodes {
		var sib *chainhash.Hash
		if n == "*" {
			sib = cur
		} else {
			if sib, err = chainhash.NewHashFromHex(n); err != nil {
				return "", fmt.Errorf("invalid proof node %d: %w", i, err)
			}
		}
		if idx&1 == 1 {
			cur = transaction.MerkleTreeParent(sib, cur)
		} else {
			cur = transaction.MerkleTreeParent(cur, sib)
		}
		idx >>= 1
	}
	if idx != 0 {
		return "", fmt.Errorf("proof index %d does not fit in a tree of depth %d", index, len(nodes))
	}
	return cur.String(), nil
}

// VerifyTSC recomputes the proof's merkle root and checks it against both the
// proof's own target and the block header the transaction is mined in.
func VerifyTSC(ctx context.Context, c *api.Client, v RootVerifier, txid string, p *TSCProof) (*Result, error) {
	root, err := ComputeTSCRoot(txid, p.Index, p.Nodes)
	if err != nil {
		return nil, err
	}
	res := &Result{Txid: txid, ComputedRoot: root}
	// Compare case-insensitively: the computed root is lowercase hex but the
	// server may report target in a different case (every other root comparison
	// in this package uses EqualFold, so a valid proof must not be failed here
	// over letter case alone).
	if p.Target != "" && !strings.EqualFold(p.Target, root) {
		// The proof is internally inconsistent: its own target disagrees with the
		// root its node path folds to. No need to consult the chain.
		res.HeaderRoot = p.Target
		res.State = ConfInvalid
		return res, nil
	}
	// Resolve the block the tx was mined in so the root can be confirmed at the
	// right height. The height comes from the (untrusted) API, but confirming
	// (root, height) against the verifier still catches a lie: a wrong height or
	// a fabricated root both fail confirmation.
	var tx txInfo
	if err := c.GetJSON(ctx, "/api/v1/tx/"+url.PathEscape(txid), nil, &tx); err != nil {
		return nil, fmt.Errorf("fetching tx for header cross-check: %w", err)
	}
	if tx.BlockHash == "" {
		return nil, fmt.Errorf("transaction is not mined; nothing to verify against")
	}
	height := tx.BlockHeight
	if height == 0 {
		// Some responses carry only the block hash; resolve its height.
		var blk blockInfo
		if err := c.GetJSON(ctx, "/api/v1/block/"+url.PathEscape(tx.BlockHash), nil, &blk); err != nil {
			return nil, fmt.Errorf("resolving block height: %w", err)
		}
		height = blk.Height
	}
	confs, err := v.ConfirmRoots(ctx, []RootAtHeight{{Root: root, Height: uint32(height)}})
	if err != nil {
		return nil, err
	}
	if len(confs) == 0 {
		return nil, fmt.Errorf("verifier returned no confirmation for the proof")
	}
	cr := confs[0]
	res.BlockHeight = int(height)
	res.BlockHash = cr.BlockHash
	if res.BlockHash == "" {
		res.BlockHash = tx.BlockHash
	}
	res.HeaderRoot = cr.HeaderRoot
	res.Valid = cr.State == ConfConfirmed
	res.State = cr.State
	return res, nil
}

// VerifyBEEF parses BEEF bytes and verifies every BUMP's computed merkle root
// against the block header at its height. The subject txid must be present in
// the BEEF and provably linked — directly by a BUMP, or transitively through
// its ancestors' inputs — to a merkle-proven transaction. Without that link a
// server could return unrelated-but-valid proofs and pass verification.
func VerifyBEEF(ctx context.Context, v RootVerifier, txid string, beefBytes []byte) ([]*Result, error) {
	beef, err := parseBEEF(beefBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing BEEF: %w", err)
	}
	if len(beef.BUMPs) == 0 {
		return nil, fmt.Errorf("BEEF contains no merkle paths to verify")
	}

	subject, err := chainhash.NewHashFromHex(txid)
	if err != nil {
		return nil, fmt.Errorf("invalid txid: %w", err)
	}

	// The subject transaction must actually be in the BEEF, and its input
	// graph must bottom out at merkle-proven transactions. ValidateTransactions
	// resolves that linkage (a tx is valid if it has a BUMP or all its inputs
	// trace to valid txs); we still check every BUMP's root against a header
	// below, so both the graph and the proofs are verified.
	if beef.FindTransaction(txid) == nil {
		return nil, fmt.Errorf("BEEF does not contain the requested transaction %s (the server returned an unrelated set of proofs)", txid)
	}
	vr := beef.ValidateTransactions()
	if !slices.Contains(vr.Valid, txid) {
		detail := "it has no merkle-proven ancestry"
		if len(vr.MissingInputs) > 0 {
			detail = "these input transactions are missing from the BEEF: " + strings.Join(vr.MissingInputs, ", ")
		}
		return nil, fmt.Errorf("BEEF does not prove %s: %s", txid, detail)
	}

	// Collect every proven leaf across all BUMPs with the root its path computes
	// and the height it claims. Verifying every level-0 leaf (not just one
	// representative) matters for compound BUMPs: a single valid leaf could
	// otherwise mask another whose proof does not match the header, and the
	// subject's transitive validity may rest on exactly that other leaf.
	type leafProof struct {
		txid   string
		root   string
		height uint32
	}
	var proofs []leafProof
	for i, bump := range beef.BUMPs {
		var leaves []*chainhash.Hash
		if len(bump.Path) > 0 {
			for _, el := range bump.Path[0] {
				if el.Hash == nil || el.Txid == nil || !*el.Txid {
					continue
				}
				leaves = append(leaves, el.Hash)
			}
		}
		if len(leaves) == 0 {
			return nil, fmt.Errorf("BUMP %d has no transaction leaves to verify", i)
		}
		for _, leaf := range leaves {
			root, err := bump.ComputeRoot(leaf)
			if err != nil {
				return nil, fmt.Errorf("computing root for BUMP %d leaf %s: %w", i, leaf.String(), err)
			}
			proofs = append(proofs, leafProof{txid: leaf.String(), root: root.String(), height: bump.BlockHeight})
		}
	}

	// Confirm the distinct (root, height) pairs against the verifier in one batch.
	type key struct {
		root   string
		height uint32
	}
	seenPair := make(map[key]bool)
	var pairs []RootAtHeight
	for _, p := range proofs {
		k := key{p.root, p.height}
		if !seenPair[k] {
			seenPair[k] = true
			pairs = append(pairs, RootAtHeight{Root: p.root, Height: p.height})
		}
	}
	confs, err := v.ConfirmRoots(ctx, pairs)
	if err != nil {
		return nil, err
	}
	byPair := make(map[key]ConfirmedRoot, len(confs))
	for _, c := range confs {
		byPair[key{c.Root, c.Height}] = c
	}

	subjStr := subject.String()
	var results []*Result
	subjectCovered := false
	anyInvalid, anyUnknown := false, false
	for _, p := range proofs {
		c := byPair[key{p.root, p.height}]
		switch c.State {
		case ConfConfirmed:
		case ConfUnknown:
			anyUnknown = true
		default:
			anyInvalid = true
		}
		if p.txid == subjStr {
			subjectCovered = true
		}
		results = append(results, &Result{
			Txid:         p.txid,
			ComputedRoot: p.root,
			BlockHeight:  int(p.height),
			BlockHash:    c.BlockHash,
			HeaderRoot:   c.HeaderRoot,
			Valid:        c.State == ConfConfirmed,
			State:        c.State,
		})
	}

	if !subjectCovered {
		// No BUMP covers the subject directly; it is proven transitively via its
		// ancestors' BUMPs (standard for unconfirmed-chain BEEF). The graph
		// linkage was already checked above, so the subject is confirmed only if
		// every BUMP root was too. Report the weakest outcome so an unverified
		// ancestor is neither passed off as fraud nor as OK.
		state := ConfConfirmed
		if anyUnknown {
			state = ConfUnknown
		}
		if anyInvalid {
			state = ConfInvalid
		}
		results = append(results, &Result{
			Txid:         txid,
			ComputedRoot: "(no direct merkle path; proven via linked ancestors)",
			Valid:        state == ConfConfirmed,
			State:        state,
		})
	}
	return results, nil
}

// parseBEEF wraps the SDK parser, which reads untrusted server bytes and can
// panic (not just error) on malformed input. Recover so a hostile or buggy
// server yields an error instead of crashing the CLI.
func parseBEEF(beefBytes []byte) (beef *transaction.Beef, err error) {
	defer func() {
		if r := recover(); r != nil {
			beef, err = nil, fmt.Errorf("malformed BEEF bytes: %v", r)
		}
	}()
	return transaction.NewBeefFromBytes(beefBytes)
}
