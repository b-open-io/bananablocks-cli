package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the node's indexing and data-freshness status",
	Long: `Show the serving node's own view of how current its data is
(/api/v1/status/indexing): history_state (ready, catching_up, reorganizing,
uninitialized or unknown), indexed and committed heights, and token-balance
maintenance state. It is relative to this node's committed data, not the
network tip, and is cached for up to five seconds.`,
	Example: `  bb status`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return getRender(cmd, "/api/v1/status/indexing", nil)
	},
}

var healthCmd = &cobra.Command{
	Use:     "health",
	Short:   "Check the server's health endpoint",
	Long:    "Print the server's /health answer (plain text). Exits non-zero unless it answers 2xx.",
	Example: `  bb health`,
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		raw, _, err := client().Bytes(cmd.Context(), "/health", nil)
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), strings.TrimSpace(string(raw)))
		return nil
	},
}

func init() {
	rootCmd.AddCommand(statusCmd, healthCmd)
}
