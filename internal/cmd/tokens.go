package cmd

import (
	"errors"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

var tokensLimit int

var tokensCmd = &cobra.Command{
	Use:     "tokens",
	Short:   "List BSV-21 tokens",
	Example: `  bb tokens --limit 25`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		q := url.Values{}
		if tokensLimit > 0 {
			q.Set("limit", strconv.Itoa(tokensLimit))
		}
		return getRender(cmd, "/api/v1/tokens", q)
	},
}

var (
	tokenHolders bool
	tokenHistory bool
	tokenLimit   int
)

var tokenCmd = &cobra.Command{
	Use:   "token <tokenId>",
	Short: "Show a BSV-21 token (detail, holders, history)",
	Example: `  bb token 90a5cb...ab_0
  bb token 90a5cb...ab_0 --holders
  bb token 90a5cb...ab_0 --history --limit 50`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if tokenHolders && tokenHistory {
			return errors.New("pick at most one of --holders, --history")
		}
		path := "/api/v1/token/" + url.PathEscape(args[0])
		switch {
		case tokenHolders:
			path += "/holders"
		case tokenHistory:
			path += "/history"
		}
		q := url.Values{}
		if tokenLimit > 0 {
			q.Set("limit", strconv.Itoa(tokenLimit))
		}
		return getRender(cmd, path, q)
	},
}

func init() {
	tokensCmd.Flags().IntVar(&tokensLimit, "limit", 0, "page size")
	tokenCmd.Flags().BoolVar(&tokenHolders, "holders", false, "list token holders")
	tokenCmd.Flags().BoolVar(&tokenHistory, "history", false, "list token transfer history")
	tokenCmd.Flags().IntVar(&tokenLimit, "limit", 0, "page size")
	rootCmd.AddCommand(tokensCmd, tokenCmd)
}
