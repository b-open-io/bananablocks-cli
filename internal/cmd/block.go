package cmd

import (
	"errors"
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
		seg, err := pathSegment("block", args[0])
		if err != nil {
			return err
		}
		path := "/api/v1/block/" + seg
		if args[0] == "tip" {
			path = "/api/v1/block/tip"
		}
		if blockProtocols {
			path += "/protocols"
		}
		return getRender(cmd, path, nil)
	},
}

var (
	blocksLimit int
	blocksFrom  int
)

var blocksCmd = &cobra.Command{
	Use:   "blocks",
	Short: "List recent blocks",
	Long: `List blocks newest first. --limit is clamped to 100 by the server; page
backwards by passing --from the height below the last block you received.`,
	Example: `  bb blocks --limit 10
  bb blocks --from 800000 --limit 100`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		if blocksLimit < 0 {
			return errors.New("--limit must be positive")
		}
		q := url.Values{}
		if blocksLimit > 0 {
			q.Set("limit", strconv.Itoa(blocksLimit))
		}
		if cmd.Flags().Changed("from") {
			if blocksFrom < 0 {
				return errors.New("--from must be a block height (0 or more)")
			}
			q.Set("from", strconv.Itoa(blocksFrom))
		}
		return getRender(cmd, "/api/v1/blocks", q)
	},
}

func init() {
	blockCmd.Flags().BoolVar(&blockProtocols, "protocols", false, "show protocol counts for the block")
	blocksCmd.Flags().IntVar(&blocksLimit, "limit", 0, "number of blocks to return (max 100)")
	blocksCmd.Flags().IntVar(&blocksFrom, "from", 0, "start at this block height and page backwards")
	rootCmd.AddCommand(blockCmd, blocksCmd)
}
