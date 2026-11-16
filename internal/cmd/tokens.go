package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/spf13/cobra"
)

// pageLimits says which page sizes an offset-paged endpoint accepts. The
// token endpoints take any limit from 1 to 100; the rich list takes only 10,
// 20, 50 or 100 and silently falls back to its default for anything else, so
// bb rejects such a value instead.
type pageLimits struct {
	allowed []int // nil = any value in 1..100
}

var (
	tokenPageLimits    = pageLimits{}
	richlistPageLimits = pageLimits{allowed: []int{10, 20, 50, 100}}
)

func (p pageLimits) check(limit int) error {
	if p.allowed == nil {
		if limit < 1 || limit > 100 {
			return fmt.Errorf("--limit must be between 1 and 100 (got %d)", limit)
		}
		return nil
	}
	for _, l := range p.allowed {
		if l == limit {
			return nil
		}
	}
	return fmt.Errorf("--limit must be one of 10, 20, 50 or 100 (got %d)", limit)
}

// offsetQuery validates --page/--limit for an offset-paged endpoint and
// returns the query to send.
func offsetQuery(cmd *cobra.Command, limits pageLimits, page, limit int) (url.Values, error) {
	q := url.Values{}
	if cmd.Flags().Changed("limit") {
		if err := limits.check(limit); err != nil {
			return nil, err
		}
		q.Set("limit", strconv.Itoa(limit))
	}
	if cmd.Flags().Changed("page") {
		if page < 0 {
			return nil, errors.New("--page is zero-indexed and cannot be negative")
		}
		q.Set("page", strconv.Itoa(page))
	}
	return q, nil
}

var (
	tokensLimit int
	tokensPage  int
)

var tokensCmd = &cobra.Command{
	Use:   "tokens",
	Short: "List BSV-21 tokens",
	Example: `  bb tokens --limit 25
  bb tokens --page 2`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		q, err := offsetQuery(cmd, tokenPageLimits, tokensPage, tokensLimit)
		if err != nil {
			return err
		}
		return getRender(cmd, "/api/v1/tokens", q)
	},
}

var (
	tokenHolders bool
	tokenHistory bool
	tokenFunding bool
	tokenIcon    bool
	tokenLimit   int
	tokenPage    int
	tokenOutFile string
)

var tokenCmd = &cobra.Command{
	Use:   "token <tokenId>",
	Short: "Show a BSV-21 token (detail, holders, history, funding, icon)",
	Long: `Show a BSV-21 token. Amounts are decimal strings in the token's smallest
unit (divide by 10^decimals); circulating_supply and remaining_to_mint are
omitted, not zero, when the token's balances have never been computed.

  --holders / --history  offset-paged lists (--page, --limit 1-100)
  --funding              the token's 1sat fee address and funding status;
                         offer to pay only when "solicitable" is true
  --icon                 download the token's icon (-o file, default
                         <tokenId>.<ext>)`,
	Example: `  bb token 90a5cb...ab_0
  bb token 90a5cb...ab_0 --holders --page 1 --limit 100
  bb token 90a5cb...ab_0 --history --limit 50
  bb token 90a5cb...ab_0 --funding
  bb token 90a5cb...ab_0 --icon -o icon.png`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		set := 0
		for _, b := range []bool{tokenHolders, tokenHistory, tokenFunding, tokenIcon} {
			if b {
				set++
			}
		}
		if set > 1 {
			return errors.New("pick at most one of --holders, --history, --funding, --icon")
		}
		changed := cmd.Flags().Changed
		listing := tokenHolders || tokenHistory
		if (changed("page") || changed("limit")) && !listing {
			return errors.New("--page and --limit apply to --holders and --history")
		}
		if changed("output") && !tokenIcon {
			return errors.New("-o applies to --icon")
		}
		seg, err := pathSegment("token id", args[0])
		if err != nil {
			return err
		}
		path := "/api/v1/token/" + seg
		switch {
		case tokenIcon:
			return tokenIconDownload(cmd, path+"/icon", args[0])
		case tokenFunding:
			return getRender(cmd, path+"/funding", nil)
		case tokenHolders:
			path += "/holders"
		case tokenHistory:
			path += "/history"
		}
		q, err := offsetQuery(cmd, tokenPageLimits, tokenPage, tokenLimit)
		if err != nil {
			return err
		}
		return getRender(cmd, path, q)
	},
}

func tokenIconDownload(cmd *cobra.Command, path, tokenID string) error {
	body, hdr, err := streamClient().Stream(cmd.Context(), path, nil)
	if err != nil {
		return err
	}
	defer body.Close()
	ct := hdr.Get("Content-Type")
	if tokenOutFile == "-" {
		_, err := copyOut(cmd, body)
		return err
	}
	out := tokenOutFile
	if out == "" {
		out = safeFileName(tokenID) + extForContentType(ct)
	}
	return saveStream(cmd, body, out, ct)
}

// safeFileName keeps letters, digits, '_' and '-' (anything else becomes '_'), so a token id can
// never name a path outside the current directory.
func safeFileName(s string) string {
	b := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b = append(b, r)
		default:
			b = append(b, '_')
		}
	}
	if len(b) == 0 {
		return "token"
	}
	return string(b)
}

func init() {
	tokensCmd.Flags().IntVar(&tokensLimit, "limit", 20, "page size (1-100)")
	tokensCmd.Flags().IntVar(&tokensPage, "page", 0, "zero-indexed page")
	f := tokenCmd.Flags()
	f.BoolVar(&tokenHolders, "holders", false, "list token holders")
	f.BoolVar(&tokenHistory, "history", false, "list token transfer history")
	f.BoolVar(&tokenFunding, "funding", false, "show the token's fee address and funding status")
	f.BoolVar(&tokenIcon, "icon", false, "download the token's icon")
	f.IntVar(&tokenLimit, "limit", 20, "page size for --holders/--history (1-100)")
	f.IntVar(&tokenPage, "page", 0, "zero-indexed page for --holders/--history")
	f.StringVarP(&tokenOutFile, "output", "o", "", "output file for --icon (- for stdout)")
	rootCmd.AddCommand(tokensCmd, tokenCmd)
}
