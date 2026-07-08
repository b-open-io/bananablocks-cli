// Package spv verifies merkle proofs (TSC and BEEF/BUMP) locally, checking the
// computed roots against block headers fetched from the API.
//
// Trust boundary: this is NOT full SPV. The block headers the computed roots
// are checked against come from the same API that supplies the proofs, and
// their proof-of-work is not validated, nor are they cross-checked against an
// independent header source. A single malicious or buggy server could therefore
// return a fabricated proof together with a matching fabricated header and pass
// verification. What this package does guarantee is internal consistency —
// that the proof's own math is correct and agrees with the header the server
// reports for the block — which catches proof corruption and endpoint bugs, but
// does not remove trust in the server. Callers wanting a stronger guarantee
// must validate header work and/or compare headers against an independent
// source.
package spv

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strconv"
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
func VerifyTSC(ctx context.Context, c *api.Client, txid string, p *TSCProof) (*Result, error) {
	root, err := ComputeTSCRoot(txid, p.Index, p.Nodes)
	if err != nil {
		return nil, err
	}
	res := &Result{Txid: txid, ComputedRoot: root}
	if p.Target != "" && p.Target != root {
		res.HeaderRoot = p.Target
		return res, nil
	}
	// Independently fetch the mined block's header and compare its merkle
	// root, so the proof is checked against the chain rather than only its
	// own target field.
	var tx txInfo
	if err := c.GetJSON(ctx, "/api/v1/tx/"+url.PathEscape(txid), nil, &tx); err != nil {
		return nil, fmt.Errorf("fetching tx for header cross-check: %w", err)
	}
	if tx.BlockHash == "" {
		return nil, fmt.Errorf("transaction is not mined; nothing to verify against")
	}
	var blk blockInfo
	if err := c.GetJSON(ctx, "/api/v1/block/"+url.PathEscape(tx.BlockHash), nil, &blk); err != nil {
		return nil, fmt.Errorf("fetching block header: %w", err)
	}
	res.BlockHash = blk.Hash
	res.BlockHeight = blk.Height
	res.HeaderRoot = blk.MerkleRoot
	res.Valid = blk.MerkleRoot == root
	return res, nil
}

// VerifyBEEF parses BEEF bytes and verifies every BUMP's computed merkle root
// against the block header at its height. The subject txid must be present in
// the BEEF and provably linked — directly by a BUMP, or transitively through
// its ancestors' inputs — to a merkle-proven transaction. Without that link a
// server could return unrelated-but-valid proofs and pass verification.
func VerifyBEEF(ctx context.Context, c *api.Client, txid string, beefBytes []byte) ([]*Result, error) {
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

	var results []*Result
	subjectCovered := false
	allValid := true
	for i, bump := range beef.BUMPs {
		// Verify every level-0 leaf the SDK treats as a proven transaction, not
		// just one representative. A compound BUMP proves several txids at once;
		// checking a single leaf's root could let a valid leaf mask another
		// leaf whose proof does not match the header, and the subject's
		// transitive validity may rest on exactly that other leaf.
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

		// One header fetch per BUMP; every leaf in it shares the same height.
		var blk blockInfo
		if err := c.GetJSON(ctx, "/api/v1/block/"+strconv.FormatUint(uint64(bump.BlockHeight), 10), nil, &blk); err != nil {
			return nil, fmt.Errorf("fetching header at height %d: %w", bump.BlockHeight, err)
		}

		for _, leaf := range leaves {
			if leaf.IsEqual(subject) {
				subjectCovered = true
			}
			root, err := bump.ComputeRoot(leaf)
			if err != nil {
				return nil, fmt.Errorf("computing root for BUMP %d leaf %s: %w", i, leaf.String(), err)
			}
			valid := blk.MerkleRoot == root.String()
			allValid = allValid && valid
			results = append(results, &Result{
				Txid:         leaf.String(),
				ComputedRoot: root.String(),
				BlockHeight:  blk.Height,
				BlockHash:    blk.Hash,
				HeaderRoot:   blk.MerkleRoot,
				Valid:        valid,
			})
		}
	}

	if !subjectCovered {
		// No BUMP covers the subject directly; it is proven transitively via
		// its ancestors' BUMPs (standard for unconfirmed-chain BEEF). The graph
		// linkage was already checked above, so this is Valid only if every
		// BUMP root also matched its header.
		results = append(results, &Result{
			Txid:         txid,
			ComputedRoot: "(no direct merkle path; proven via linked ancestors)",
			Valid:        allValid,
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

// DecodeHexOrRaw accepts either raw bytes or a hex string of them.
func DecodeHexOrRaw(b []byte) []byte {
	if raw, err := hex.DecodeString(string(b)); err == nil {
		return raw
	}
	return b
}
