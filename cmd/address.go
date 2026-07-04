package cmd

import (
	"encoding/json"
	"errors"
	"net/url"
	"strconv"

	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/spf13/cobra"
)

var (
	addrUTXOs   bool
	addrTxs     bool
	addrBalance bool
	addrTokens  bool
	addrLimit   int
	addrPage    int
	addrCursor  string
)

var addressCmd = &cobra.Command{
	Use:   "address <addr>",
	Short: "Show an address (summary, UTXOs, transactions, balance, tokens)",
	Example: `  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --utxos --limit 50
  bb address 1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa --balance`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		set := 0
		for _, b := range []bool{addrUTXOs, addrTxs, addrBalance, addrTokens} {
			if b {
				set++
			}
		}
		if set > 1 {
			return errors.New("pick at most one of --utxos, --txs, --balance, --tokens")
		}
		base := "/api/v1/address/" + url.PathEscape(args[0])
		path := base
		switch {
		case addrUTXOs:
			path = base + "/utxos"
		case addrTxs:
			path = base + "/txs"
		case addrBalance:
			path = base + "/balance"
		case addrTokens:
			path = base + "/tokens"
		}
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
		var out json.RawMessage
		if err := client().GetJSON(cmd.Context(), path, q, &out); err != nil {
			return err
		}
		return render.JSON(cmd.OutOrStdout(), out)
	},
}

func init() {
	f := addressCmd.Flags()
	f.BoolVar(&addrUTXOs, "utxos", false, "list unspent outputs")
	f.BoolVar(&addrTxs, "txs", false, "list transaction history")
	f.BoolVar(&addrBalance, "balance", false, "show balance only")
	f.BoolVar(&addrTokens, "tokens", false, "list token balances")
	f.IntVar(&addrLimit, "limit", 0, "page size")
	f.IntVar(&addrPage, "page", 0, "page number (legacy offset pagination)")
	f.StringVar(&addrCursor, "cursor", "", "pagination cursor")
	rootCmd.AddCommand(addressCmd)
}
