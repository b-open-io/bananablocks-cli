package cmd

import (
	"errors"
	"fmt"
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
		if args[0] == "tip" {
			if !blockProtocols {
				return getRender(cmd, "/api/v1/block/tip", nil)
			}
			// The server has no tip/protocols route (it answers 400 "invalid
			// block identifier"), so resolve the tip to its hash first.
			if seg, err = tipHash(cmd); err != nil {
				return err
			}
		}
		path := "/api/v1/block/" + seg
		if blockProtocols {
			path += "/protocols"
		}
		return getRender(cmd, path, nil)
	},
}

// tipHash fetches the chain tip and returns its hash as a path segment.
func tipHash(cmd *cobra.Command) (string, error) {
	var tip struct {
		Hash string `json:"hash"`
	}
	if err := client().GetJSON(cmd.Context(), "/api/v1/block/tip", nil, &tip); err != nil {
		return "", fmt.Errorf("resolving the chain tip: %w", err)
	}
	if len(tip.Hash) != 64 || !hexRe.MatchString(tip.Hash) {
		return "", fmt.Errorf("resolving the chain tip: unexpected block hash %q", truncateArg(tip.Hash))
	}
	return tip.Hash, nil
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
	blockCmd.Flags().BoolVar(&blockProtocols, "protocols", false, "show protocol counts for the block (with tip, for the current tip block)")
	blocksCmd.Flags().IntVar(&blocksLimit, "limit", 0, "number of blocks to return (max 100)")
	blocksCmd.Flags().IntVar(&blocksFrom, "from", 0, "start at this block height and page backwards")
	rootCmd.AddCommand(blockCmd, blocksCmd)
}
