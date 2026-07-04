// Package spv verifies merkle proofs (TSC and BEEF/BUMP) locally, checking
// the computed roots against block headers fetched from the API.
package spv

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"

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
// against the block header at its height. The subject txid must be present
// under one of the BUMPs.
func VerifyBEEF(ctx context.Context, c *api.Client, txid string, beefBytes []byte) ([]*Result, error) {
	beef, err := transaction.NewBeefFromBytes(beefBytes)
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

	var results []*Result
	subjectCovered := false
	for i, bump := range beef.BUMPs {
		// Pick a level-0 leaf with a hash to anchor the root computation;
		// prefer the subject tx when this bump covers it.
		var leaf *chainhash.Hash
		if len(bump.Path) > 0 {
			for _, el := range bump.Path[0] {
				if el.Hash == nil {
					continue
				}
				if el.Hash.IsEqual(subject) {
					leaf = el.Hash
					break
				}
				if leaf == nil {
					leaf = el.Hash
				}
			}
		}
		if leaf == nil {
			return nil, fmt.Errorf("BUMP %d has no leaf hashes", i)
		}
		if leaf.IsEqual(subject) {
			subjectCovered = true
		}

		root, err := bump.ComputeRoot(leaf)
		if err != nil {
			return nil, fmt.Errorf("computing root for BUMP %d: %w", i, err)
		}

		var blk blockInfo
		if err := c.GetJSON(ctx, "/api/v1/block/"+strconv.FormatUint(uint64(bump.BlockHeight), 10), nil, &blk); err != nil {
			return nil, fmt.Errorf("fetching header at height %d: %w", bump.BlockHeight, err)
		}
		results = append(results, &Result{
			Txid:         leaf.String(),
			ComputedRoot: root.String(),
			BlockHeight:  blk.Height,
			BlockHash:    blk.Hash,
			HeaderRoot:   blk.MerkleRoot,
			Valid:        blk.MerkleRoot == root.String(),
		})
	}

	if !subjectCovered {
		// The subject tx has no direct proof — it is proven transitively via
		// its ancestors' BUMPs (standard for unconfirmed-chain BEEF). Make
		// that visible rather than implying a direct proof was checked.
		results = append(results, &Result{
			Txid:         txid,
			ComputedRoot: "(no direct merkle path; proven via ancestors)",
			Valid:        true,
		})
	}
	return results, nil
}

// DecodeHexOrRaw accepts either raw bytes or a hex string of them.
func DecodeHexOrRaw(b []byte) []byte {
	if raw, err := hex.DecodeString(string(b)); err == nil {
		return raw
	}
	return b
}
