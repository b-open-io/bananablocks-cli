package cmd

import (
	"errors"
	"fmt"
	"strconv"

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

// statsFlagViews lists the views each option applies to.
var statsFlagViews = map[string][]string{
	"days":   {"network", "protocols", "price"},
	"window": {"network", "protocols"},
	"page":   {"richlist"},
	"limit":  {"richlist"},
}

var (
	statsDays   int
	statsWindow string
	statsPage   int
	statsLimit  int
)

var statsCmd = &cobra.Command{
	Use:   "stats [summary|network|richlist|mempool|protocols|price|rates]",
	Short: "Chain, network, mempool, and protocol statistics",
	Long: `Chain, network, mempool, and protocol statistics.

  --days N       network (daily rows, default 30), protocols (today plus N
                 preceding days, default 7), price (default 7)
  --window 24h   network and protocols over the trailing 24 hours; takes
                 precedence over --days
  --page/--limit richlist (zero-indexed page; limit 10, 20, 50 or 100)`,
	Example: `  bb stats
  bb stats network --window 24h
  bb stats protocols --days 30
  bb stats price --days 365
  bb stats richlist --page 1 --limit 100`,
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
		for flag, views := range statsFlagViews {
			if !cmd.Flags().Changed(flag) {
				continue
			}
			allowed := false
			for _, v := range views {
				allowed = allowed || v == which
			}
			if !allowed {
				return fmt.Errorf("--%s does not apply to %q (it applies to %v)", flag, which, views)
			}
		}
		q, err := offsetQuery(cmd, richlistPageLimits, statsPage, statsLimit)
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("days") {
			if statsDays <= 0 {
				return errors.New("--days must be positive")
			}
			q.Set("days", strconv.Itoa(statsDays))
		}
		if cmd.Flags().Changed("window") {
			if statsWindow != "24h" {
				return fmt.Errorf("--window supports only 24h (got %q)", statsWindow)
			}
			q.Set("window", statsWindow)
		}
		return getRender(cmd, path, q)
	},
}

func init() {
	f := statsCmd.Flags()
	f.IntVar(&statsDays, "days", 0, "days of history (network, protocols, price)")
	f.StringVar(&statsWindow, "window", "", "trailing window: 24h (network, protocols)")
	f.IntVar(&statsPage, "page", 0, "zero-indexed page (richlist)")
	f.IntVar(&statsLimit, "limit", 20, "page size: 10, 20, 50 or 100 (richlist)")
	rootCmd.AddCommand(statsCmd)
}
