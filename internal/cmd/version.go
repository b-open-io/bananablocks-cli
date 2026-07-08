package cmd

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the bb version",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprintln(cmd.OutOrStdout(), "bb", Version)
	},
}

func init() {
	// `go install .../cmd/bb@vX.Y.Z` doesn't run the Makefile's ldflags; fall
	// back to the module version recorded in the binary's build info.
	if Version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
			Version = bi.Main.Version
		}
	}
	rootCmd.AddCommand(versionCmd)
}
