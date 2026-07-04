package cmd

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/spf13/cobra"
)

var (
	getPost bool
	getData string
)

var getCmd = &cobra.Command{
	Use:   "get <path>",
	Short: "Fetch any API path and pretty-print the JSON response",
	Long: `Fetch an arbitrary API path. Paths not starting with "/" are treated as
relative to /api/v1/, so "stats/summary" and "/api/v1/stats/summary" are
equivalent. Query strings are passed through.`,
	Example: `  bb get stats/summary
  bb get "blocks?limit=5"
  bb get /api/v1/bsv/main/policy
  bb get tools/decode --post --data '{"txhex":"01000000..."}'`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		path := args[0]
		if !strings.HasPrefix(path, "/") {
			path = "/api/v1/" + path
		}
		method := http.MethodGet
		var body *strings.Reader
		contentType := ""
		if getPost || getData != "" {
			method = http.MethodPost
			contentType = "application/json"
			body = strings.NewReader(getData)
		} else {
			body = strings.NewReader("")
		}
		var out json.RawMessage
		if err := client().JSON(cmd.Context(), method, path, nil, contentType, body, &out); err != nil {
			return err
		}
		return render.JSON(cmd.OutOrStdout(), out)
	},
}

func init() {
	getCmd.Flags().BoolVar(&getPost, "post", false, "use POST instead of GET")
	getCmd.Flags().StringVar(&getData, "data", "", "JSON request body (implies --post)")
	rootCmd.AddCommand(getCmd)
}
