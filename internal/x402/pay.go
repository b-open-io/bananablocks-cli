package x402

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	feemodel "github.com/bsv-blockchain/go-sdk/transaction/fee_model"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
)

// VerifyPayee checks that the challenge's human-readable PayeeAddress actually
// corresponds to PayeeLockingScriptHex — the script the payment transaction
// will pay. Without this a challenge could display one address while the signed
// output pays a different script, so the confirmation prompt would attest to a
// destination the money never reaches. An empty PayeeAddress leaves nothing to
// attest and is allowed (the script hex is then the only stated destination).
func (c *Challenge) VerifyPayee() error {
	if c.PayeeAddress == "" {
		return nil
	}
	addr, err := script.NewAddressFromString(c.PayeeAddress)
	if err != nil {
		return fmt.Errorf("challenge payee address %q is invalid: %w", c.PayeeAddress, err)
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return err
	}
	if !strings.EqualFold(lock.String(), c.PayeeLockingScriptHex) {
		return fmt.Errorf("challenge payee address %s does not match the payment script the transaction would pay — refusing to sign", c.PayeeAddress)
	}
	return nil
}

// utxo mirrors the explorer's /api/v1/address/{addr}/utxos items.
type utxo struct {
	TxID       string `json:"txid"`
	Vout       int    `json:"vout"`
	Value      int64  `json:"value"`
	ScriptType string `json:"script_type"`
}

// Wallet is a single-key P2PKH signer funded by UTXOs fetched from the API.
type Wallet struct {
	priv    *ec.PrivateKey
	Address string
	lockHex string
}

// NewWallet derives the P2PKH wallet for a WIF private key.
func NewWallet(wif string, mainnet bool) (*Wallet, error) {
	priv, err := ec.PrivateKeyFromWif(wif)
	if err != nil {
		return nil, fmt.Errorf("invalid WIF: %w", err)
	}
	addr, err := script.NewAddressFromPublicKey(priv.PubKey(), mainnet)
	if err != nil {
		return nil, err
	}
	lock, err := p2pkh.Lock(addr)
	if err != nil {
		return nil, err
	}
	return &Wallet{priv: priv, Address: addr.AddressString, lockHex: lock.String()}, nil
}

// fundingTarget estimates the sats needed to cover amount plus fees for a tx
// with nIn inputs, two outputs, at feeRate sat/kB. The estimate is generous
// (the exact fee is recomputed by the SDK fee model before signing).
func fundingTarget(amount int64, nIn int, feeRate uint64) int64 {
	size := 11 + 148*int64(nIn+1) + 34*2
	fee := (size*int64(feeRate) + 999) / 1000
	if fee < 1 {
		fee = 1
	}
	return amount + fee
}

// BuildPayment fetches UTXOs for the wallet's address, selects enough to
// cover the challenge amount plus fees, and returns a signed transaction
// paying the challenge's locking script with change back to the wallet.
//
// The returned transaction is NOT broadcast: the server broadcasts it via ARC
// when the proof is submitted.
func (w *Wallet) BuildPayment(ctx context.Context, c *api.Client, ch *Challenge, feeRate uint64) (*transaction.Transaction, error) {
	if ch.AmountSats <= 0 {
		return nil, fmt.Errorf("challenge amount %d is not payable", ch.AmountSats)
	}
	if feeRate == 0 {
		// A zero rate builds a zero-fee transaction the broadcaster will reject;
		// fail here with a clear message instead of after signing.
		return nil, fmt.Errorf("fee rate must be at least 1 sat/kB")
	}
	payeeScript, err := script.NewFromHex(ch.PayeeLockingScriptHex)
	if err != nil {
		return nil, fmt.Errorf("challenge locking script: %w", err)
	}
	ownScript, err := script.NewFromHex(w.lockHex)
	if err != nil {
		return nil, err
	}
	unlocker, err := p2pkh.Unlock(w.priv, nil)
	if err != nil {
		return nil, err
	}

	tx := transaction.NewTransaction()
	var total int64
	selected := 0
	// Track added outpoints so a UTXO returned on more than one page (the set can
	// shift between fetches) is not added twice, which would make an invalid tx
	// with duplicate inputs.
	seen := make(map[string]bool)

	// Page through UTXOs (largest pages the API allows) until the target is
	// covered. The legacy page path caps out at offset 1000, which is far more
	// coin selection than a tier purchase should ever need.
	for page := 0; page < 10 && total < fundingTarget(ch.AmountSats, selected, feeRate); page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "limit": {"100"}}
		var utxos []utxo
		if err := c.GetJSON(ctx, "/api/v1/address/"+url.PathEscape(w.Address)+"/utxos", q, &utxos); err != nil {
			return nil, fmt.Errorf("fetching UTXOs for %s: %w", w.Address, err)
		}
		if len(utxos) == 0 {
			break
		}
		for _, u := range utxos {
			if u.ScriptType != "" && u.ScriptType != "p2pkh" {
				continue // only spendable with our single-key signer
			}
			if u.Value <= 0 {
				continue
			}
			op := u.TxID + ":" + strconv.Itoa(u.Vout)
			if seen[op] {
				continue // already selected on an earlier page
			}
			seen[op] = true
			if err := tx.AddInputFrom(u.TxID, uint32(u.Vout), w.lockHex, uint64(u.Value), unlocker); err != nil {
				return nil, fmt.Errorf("adding input %s:%d: %w", u.TxID, u.Vout, err)
			}
			total += u.Value
			selected++
			if total >= fundingTarget(ch.AmountSats, selected, feeRate) {
				break
			}
		}
	}
	if total < fundingTarget(ch.AmountSats, selected, feeRate) {
		return nil, fmt.Errorf("insufficient funds: address %s has %d sats spendable, need ~%d (amount %d + fee)",
			w.Address, total, fundingTarget(ch.AmountSats, selected, feeRate), ch.AmountSats)
	}

	tx.AddOutput(&transaction.TransactionOutput{LockingScript: payeeScript, Satoshis: uint64(ch.AmountSats)})
	tx.AddOutput(&transaction.TransactionOutput{LockingScript: ownScript, Change: true})

	if err := tx.Fee(&feemodel.SatoshisPerKilobyte{Satoshis: feeRate}, transaction.ChangeDistributionEqual); err != nil {
		return nil, fmt.Errorf("computing fee/change: %w", err)
	}
	if err := tx.Sign(); err != nil {
		return nil, fmt.Errorf("signing payment: %w", err)
	}
	return tx, nil
}
