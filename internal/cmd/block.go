package cmd

import (
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

var blockProtocols bool

var blockCmd = &cobra.Command{
	Use:   "block <hash|height|tip>",
	Short: "Show a block by hash or height",
	Example: `  bb block tip
  bb block 800000
  bb block 000000000000000004a288072ebb35e37233f419918f9783d499979cb6ac33eb --protocols`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := "/api/v1/block/" + url.PathEscape(args[0])
		if args[0] == "tip" {
			path = "/api/v1/block/tip"
		}
		if blockProtocols {
			path += "/protocols"
		}
		return getRender(cmd, path, nil)
	},
}

var blocksLimit int

var blocksCmd = &cobra.Command{
	Use:     "blocks",
	Short:   "List recent blocks",
	Example: `  bb blocks --limit 10`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		q := url.Values{}
		if blocksLimit > 0 {
			q.Set("limit", strconv.Itoa(blocksLimit))
		}
		return getRender(cmd, "/api/v1/blocks", q)
	},
}

func init() {
	blockCmd.Flags().BoolVar(&blockProtocols, "protocols", false, "show protocol counts for the block")
	blocksCmd.Flags().IntVar(&blocksLimit, "limit", 0, "number of blocks to return")
	rootCmd.AddCommand(blockCmd, blocksCmd)
}
