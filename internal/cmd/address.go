package cmd

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

var (
	addrUTXOs      bool
	addrUTXOsBEEF  bool
	addrTxs        bool
	addrBalance    bool
	addrTokens     bool
	addrCSV        bool
	addrLimit      int
	addrPage       int
	addrCursor     string
	addrFromHeight uint64
	addrOutFile    string
)

var addressCmd = &cobra.Command{
	Use:   "address <addr>",
	Short: "Show an address (summary, UTXOs, transactions, balance, tokens)",
	Long: `Show an address. Native address reads are confirmed-only; mempool-aware
reads live on the WhatsonChain-compatible API (bb get bsv/main/address/<addr>/unconfirmed/balance).

  --txs         confirmed history, oldest first. Continue with --cursor set to
                the response's next_cursor (or start at --from-height).
  --utxos       confirmed UTXOs. --cursor pages any depth; --page (legacy
                offset paging) reaches only the first 1000 UTXOs.
  --utxos-beef  one BEEF (BRC-62) with the source transaction and merkle proof
                of every UTXO on a page, for importing coins with SPV proofs.
                Binary with -o, hex on stdout; the next page's cursor is
                printed to stderr.
  --csv         the 100 most recent transactions as CSV (-o file or stdout).`,
	Example: `  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --utxos --limit 50
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --txs --from-height 800000
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --utxos-beef -o coins.beef
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --csv -o history.csv`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateAddressFlags(cmd); err != nil {
			return err
		}
		seg, err := pathSegment("address", args[0])
		if err != nil {
			return err
		}
		base := "/api/v1/address/" + seg
		q := url.Values{}
		if addrLimit > 0 {
			q.Set("limit", strconv.Itoa(addrLimit))
		}
		if cmd.Flags().Changed("page") {
			q.Set("page", strconv.Itoa(addrPage))
		}
		if addrCursor != "" {
			q.Set("cursor", addrCursor)
		}
		if cmd.Flags().Changed("from-height") {
			q.Set("from_height", strconv.FormatUint(addrFromHeight, 10))
		}
		switch {
		case addrUTXOsBEEF:
			return addressUTXOsBEEF(cmd, base+"/utxos/beef", q)
		case addrCSV:
			return addressCSV(cmd, seg)
		case addrUTXOs:
			return getRender(cmd, base+"/utxos", q)
		case addrTxs:
			return getRender(cmd, base+"/txs", q)
		case addrBalance:
			return getRender(cmd, base+"/balance", q)
		case addrTokens:
			return getRender(cmd, base+"/tokens", q)
		}
		return getRender(cmd, base, q)
	},
}

// validateAddressFlags rejects flag combinations the API would ignore or
// refuse, so a paging flag is never silently dropped.
func validateAddressFlags(cmd *cobra.Command) error {
	set := 0
	for _, b := range []bool{addrUTXOs, addrUTXOsBEEF, addrTxs, addrBalance, addrTokens, addrCSV} {
		if b {
			set++
		}
	}
	if set > 1 {
		return errors.New("pick at most one of --utxos, --utxos-beef, --txs, --balance, --tokens, --csv")
	}
	changed := cmd.Flags().Changed
	paged := addrUTXOs || addrUTXOsBEEF || addrTxs
	download := addrUTXOsBEEF || addrCSV
	if addrLimit < 0 {
		return errors.New("--limit must be positive")
	}
	if changed("limit") && !paged {
		return errors.New("--limit applies to --utxos, --utxos-beef and --txs")
	}
	if changed("page") {
		if !addrUTXOs {
			return errors.New("--page applies only to --utxos (use --cursor for --txs and --utxos-beef)")
		}
		if addrPage < 0 {
			return errors.New("--page is zero-indexed and cannot be negative")
		}
		if addrCursor != "" {
			return errors.New("use either --page or --cursor, not both")
		}
	}
	if addrCursor != "" && !paged {
		return errors.New("--cursor applies to --utxos, --utxos-beef and --txs")
	}
	if changed("from-height") {
		if !addrTxs {
			return errors.New("--from-height applies only to --txs")
		}
		if addrCursor != "" {
			return errors.New("use either --cursor or --from-height, not both")
		}
	}
	if changed("output") && !download {
		return errors.New("-o applies to --utxos-beef and --csv")
	}
	return nil
}

// addressUTXOsBEEF fetches one page of the address's UTXO BEEF and reports
// the paging and coverage headers on stderr.
func addressUTXOsBEEF(cmd *cobra.Command, path string, q url.Values) error {
	raw, hdr, err := client().Bytes(cmd.Context(), path, q)
	if err != nil {
		return err
	}
	// Write the payload first, then the paging and coverage notes. The
	// headers are the only place the server reports how to continue and what
	// it left out, so a failure to print them fails the command.
	if err := writePayloadTo(cmd, addrOutFile, raw, false); err != nil {
		return err
	}
	var notes strings.Builder
	if p := hdr.Get("X-BEEF-Proven-UTXOs"); p != "" {
		fmt.Fprintf(&notes, "proven UTXOs: %s\n", p)
	}
	if o := hdr.Get("X-BEEF-Omitted-UTXOs"); o != "" && o != "0" {
		fmt.Fprintf(&notes, "warning: %s UTXO(s) on this page are not in the BEEF (no proof yet, or their transaction exceeds 4 MiB)", o)
		if ids := hdr.Get("X-BEEF-Omitted-Txids"); ids != "" {
			fmt.Fprintf(&notes, "; fetch them with bb tx <txid> --beef: %s", strings.ReplaceAll(ids, ",", " "))
		}
		notes.WriteString("\n")
	}
	if strings.EqualFold(hdr.Get("X-Has-More"), "true") {
		if next := hdr.Get("X-Next-Cursor"); next != "" {
			fmt.Fprintf(&notes, "more UTXOs: rerun with --cursor %s\n", next)
		} else {
			notes.WriteString("more UTXOs exist, but the server sent no cursor to continue from\n")
		}
	}
	return writeNotes(cmd, notes.String())
}

// writeNotes prints s on stderr, failing when it cannot be written.
func writeNotes(cmd *cobra.Command, s string) error {
	if s == "" {
		return nil
	}
	if _, err := io.WriteString(cmd.ErrOrStderr(), s); err != nil {
		return fmt.Errorf("writing paging and coverage notes to stderr: %w", err)
	}
	return nil
}

// addressCSV downloads the recent-history CSV, warning when the server says
// it is truncated or partial.
func addressCSV(cmd *cobra.Command, addrSeg string) error {
	body, hdr, err := streamClient().Stream(cmd.Context(), "/api/v1/download/address/"+addrSeg+"/history.csv", nil)
	if err != nil {
		return err
	}
	defer body.Close()
	var notes strings.Builder
	if strings.EqualFold(hdr.Get("X-Truncated"), "true") {
		notes.WriteString("note: only the most recent transactions are included (the file ends with a '# truncated' row)\n")
	}
	if strings.EqualFold(hdr.Get("X-Partial"), "true") {
		notes.WriteString("warning: some rows could not be fully read and have blank fields (see the '# partial' row)\n")
	}
	if err := writeNotes(cmd, notes.String()); err != nil {
		return err
	}
	if addrOutFile == "" || addrOutFile == "-" {
		_, err := copyOut(cmd, body)
		return err
	}
	return saveStream(cmd, body, addrOutFile, "")
}

func init() {
	f := addressCmd.Flags()
	f.BoolVar(&addrUTXOs, "utxos", false, "list confirmed unspent outputs")
	f.BoolVar(&addrUTXOsBEEF, "utxos-beef", false, "fetch a page of UTXOs as one BEEF with merkle proofs")
	f.BoolVar(&addrTxs, "txs", false, "list confirmed transaction history (oldest first)")
	f.BoolVar(&addrBalance, "balance", false, "show confirmed balance only")
	f.BoolVar(&addrTokens, "tokens", false, "list token balances")
	f.BoolVar(&addrCSV, "csv", false, "download recent history as CSV")
	f.IntVar(&addrLimit, "limit", 0, "page size")
	f.IntVar(&addrPage, "page", 0, "zero-indexed page for --utxos (legacy offset paging)")
	f.StringVar(&addrCursor, "cursor", "", "continuation cursor from a previous page")
	f.Uint64Var(&addrFromHeight, "from-height", 0, "start --txs at this block height (inclusive)")
	f.StringVarP(&addrOutFile, "output", "o", "", "write --utxos-beef or --csv to a file")
	rootCmd.AddCommand(addressCmd)
}
