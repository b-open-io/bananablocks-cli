package cmd

import (
	"encoding/json"
	"fmt"

	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/spf13/cobra"
)

// statsPaths maps the stats subcommand argument to an API path.
var statsPaths = map[string]string{
	"summary":   "/api/v1/stats/summary",
	"network":   "/api/v1/stats/network",
	"richlist":  "/api/v1/stats/richlist",
	"mempool":   "/api/v1/mempool/stats",
	"protocols": "/api/v1/stats/protocols",
	"price":     "/api/v1/stats/price-history",
	"rates":     "/api/v1/exchangerates",
}

var statsCmd = &cobra.Command{
	Use:   "stats [summary|network|richlist|mempool|protocols|price|rates]",
	Short: "Chain, network, mempool, and protocol statistics",
	Example: `  bb stats
  bb stats network
  bb stats mempool`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		which := "summary"
		if len(args) == 1 {
			which = args[0]
		}
		path, ok := statsPaths[which]
		if !ok {
			return fmt.Errorf("unknown stats view %q (want one of summary, network, richlist, mempool, protocols, price, rates)", which)
		}
		var out json.RawMessage
		if err := client().GetJSON(cmd.Context(), path, nil, &out); err != nil {
			return err
		}
		return render.JSON(cmd.OutOrStdout(), out)
	},
}

func init() {
	rootCmd.AddCommand(statsCmd)
}
