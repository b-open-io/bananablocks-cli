package cmd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/render"
	"github.com/spf13/cobra"
)

var hexRe = regexp.MustCompile(`^[0-9a-fA-F]+$`)

var broadcastCmd = &cobra.Command{
	Use:   "broadcast <hex|file|-> [more files...]",
	Short: "Broadcast raw transaction(s) to the network",
	Long: `Broadcast one or more raw transactions. Each argument is a hex string, a
path to a file containing hex, or "-" for stdin. With multiple arguments
the transactions are submitted in order to the multi-broadcast endpoint,
split into batches the server admits (anonymous callers get small batches;
bb waits out the broadcast rate limit between them).

The command exits non-zero unless every transaction is accepted: a pending
status such as QUEUED, or a per-transaction refusal, is still printed but
counts as not accepted.`,
	Example: `  bb broadcast 01000000...
  bb broadcast tx.hex
  cat tx.hex | bb broadcast -
  bb broadcast tx1.hex tx2.hex tx3.hex`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var hexes []string
		for _, a := range args {
			h, err := readTxHexArg(a)
			if err != nil {
				return err
			}
			hexes = append(hexes, h)
		}
		c := client()
		ctx := cmd.Context()

		if len(hexes) == 1 {
			var out json.RawMessage
			err := c.JSON(ctx, http.MethodPost, wocPath("/tx/broadcast"), nil,
				"text/plain", strings.NewReader(hexes[0]), &out)
			if err != nil {
				return broadcastHTTPError(err, 0, 1)
			}
			if err := render.JSON(cmd.OutOrStdout(), out); err != nil {
				return err
			}
			return checkAccepted([]json.RawMessage{out})
		}

		results, err := broadcastBatches(ctx, c, cmd.ErrOrStderr(), hexes)
		// Print whatever was answered, even on a mid-run failure, so the
		// caller sees which transactions were already submitted.
		if len(results) > 0 {
			if rerr := render.JSON(cmd.OutOrStdout(), results); rerr != nil {
				return rerr
			}
		}
		if err != nil {
			return err
		}
		return checkAccepted(results)
	},
}

const (
	// maxMultiBatch is the server's hard per-request cap on
	// /tx/broadcast/multi.
	maxMultiBatch = 100
	// maxMultiBodyBytes keeps each JSON batch body under the server's 32 MiB
	// encoded cap, with room for framing.
	maxMultiBodyBytes = 30 << 20
	// maxRateLimitWaits bounds how often one batch waits out a 429 before bb
	// gives up, so a misbehaving server cannot hold the command forever.
	maxRateLimitWaits = 10
)

// broadcastBatches submits hexes to /tx/broadcast/multi in order, in batches
// no larger than the server admits. A 413 naming max_batch_txs shrinks the
// batch size; a 429 waits for the window and resends the same batch. It
// stops at the first other failure, returning the results gathered so far.
func broadcastBatches(ctx context.Context, c *api.Client, errw io.Writer, hexes []string) ([]json.RawMessage, error) {
	var results []json.RawMessage
	limit := maxMultiBatch
	waits := 0
	for i := 0; i < len(hexes); {
		end := batchEnd(hexes, i, limit)
		batch := make([]map[string]string, 0, end-i)
		for _, h := range hexes[i:end] {
			batch = append(batch, map[string]string{"rawtx": h})
		}
		var out []json.RawMessage
		err := c.PostJSONBody(ctx, wocPath("/tx/broadcast/multi"), nil, batch, &out)
		if err == nil {
			if len(out) != end-i {
				return results, fmt.Errorf("server answered %d results for a batch of %d transactions (transactions %d-%d); check each txid's status before resubmitting", len(out), end-i, i+1, end)
			}
			results = append(results, out...)
			i, waits = end, 0
			continue
		}
		var apiErr *api.Error
		if !errors.As(err, &apiErr) {
			return results, broadcastHTTPError(err, i, end)
		}
		switch apiErr.Status {
		case http.StatusRequestEntityTooLarge:
			var body struct {
				MaxBatchTxs int `json:"max_batch_txs"`
			}
			if json.Unmarshal(apiErr.Body, &body) == nil && body.MaxBatchTxs > 0 && body.MaxBatchTxs < end-i {
				limit = body.MaxBatchTxs
				fmt.Fprintf(errw, "server admits batches of at most %d transactions for this caller; splitting\n", limit)
				continue
			}
			return results, broadcastHTTPError(err, i, end)
		case http.StatusTooManyRequests:
			if waits >= maxRateLimitWaits {
				return results, broadcastHTTPError(err, i, end)
			}
			waits++
			// A 429 with Retry-After is a batch bigger than what is left of
			// this minute; one without spent nothing and means wait a minute.
			d := time.Minute
			if v := apiErr.Header.Get("Retry-After"); v != "" {
				d = parseRetryAfter(v)
			}
			fmt.Fprintf(errw, "broadcast rate limit reached; waiting %s before sending transactions %d-%d...\n", d, i+1, end)
			if err := sleepCtx(ctx, d); err != nil {
				return results, fmt.Errorf("interrupted before transactions %d-%d were sent: %w", i+1, len(hexes), err)
			}
		default:
			return results, broadcastHTTPError(err, i, end)
		}
	}
	return results, nil
}

// batchEnd returns the end index of the batch starting at i: at most limit
// transactions and maxMultiBodyBytes of hex, and always at least one.
func batchEnd(hexes []string, i, limit int) int {
	end, size := i, 2
	for end < len(hexes) && end-i < limit {
		n := len(hexes[end]) + len(`{"rawtx":""},`)
		if end > i && size+n > maxMultiBodyBytes {
			break
		}
		size += n
		end++
	}
	return end
}

// broadcastHTTPError explains a failed broadcast request for transactions
// [from, to). An upstream 502/503 does not mean nothing was sent.
func broadcastHTTPError(err error, from, to int) error {
	var apiErr *api.Error
	where := ""
	if to-from > 1 || from > 0 {
		where = fmt.Sprintf(" (transactions %d-%d)", from+1, to)
	}
	if errors.As(err, &apiErr) && (apiErr.Status == http.StatusBadGateway || apiErr.Status == http.StatusServiceUnavailable || apiErr.Status == http.StatusGatewayTimeout) {
		return fmt.Errorf("broadcast failed upstream%s: %w — some or all of these transactions may already be on the network; resubmit the same hex or check each txid's status, and do not rebuild them spending the same inputs", where, err)
	}
	if apiErr == nil {
		// A transport error after the request was sent leaves the outcome
		// unknown, exactly like an upstream failure.
		return fmt.Errorf("broadcast request failed%s: %w — the outcome is unknown; resubmit the same hex or check each txid's status before rebuilding", where, err)
	}
	if where != "" {
		return fmt.Errorf("broadcast refused%s: %w", where, err)
	}
	return err
}

// checkAccepted fails when any result does not prove acceptance. The server
// answers 200 for pending statuses (e.g. QUEUED) and per-transaction refusals
// alike; only accepted:true means the network took the transaction.
func checkAccepted(results []json.RawMessage) error {
	var notAccepted []string
	for n, raw := range results {
		var r struct {
			Accepted *bool  `json:"accepted"`
			TxID     string `json:"txid"`
			TxStatus string `json:"txStatus"`
			Error    string `json:"error"`
		}
		_ = json.Unmarshal(raw, &r)
		if r.Accepted != nil && *r.Accepted {
			continue
		}
		label := r.TxID
		if label == "" {
			label = fmt.Sprintf("#%d", n+1)
		}
		detail := r.TxStatus
		if r.Error != "" {
			if detail != "" {
				detail += ": "
			}
			detail += r.Error
		}
		if detail == "" {
			detail = "no acceptance reported"
		}
		notAccepted = append(notAccepted, label+" ("+detail+")")
	}
	if len(notAccepted) == 0 {
		return nil
	}
	return fmt.Errorf("%d of %d transaction(s) not accepted by the network: %s", len(notAccepted), len(results), strings.Join(notAccepted, "; "))
}

// readTxHexArg resolves an argument into validated transaction hex.
func readTxHexArg(arg string) (string, error) {
	var raw []byte
	switch {
	case arg == "-":
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return "", err
		}
		raw = b
	case hexRe.MatchString(arg) && len(arg)%2 == 0:
		// An argument that is itself valid, even-length hex is transaction hex —
		// take it literally even if a file of the same name exists, so a hex
		// string is never silently read from a colliding filename. (Real tx hex
		// is always even-length; odd-length all-hex args fall through so a file
		// named e.g. "abc" is still readable.)
		raw = []byte(arg)
	case fileExists(arg):
		b, err := os.ReadFile(arg)
		if err != nil {
			return "", err
		}
		raw = b
	default:
		raw = []byte(arg)
	}
	// Collapse all ASCII whitespace so line-wrapped hex (e.g. from `xxd -p`,
	// which wraps every 60 chars) is recognized as hex rather than mistaken
	// for binary and double-encoded.
	s := strings.Join(strings.Fields(string(raw)), "")
	if s == "" {
		return "", fmt.Errorf("%s: empty transaction payload", arg)
	}
	// All-hex content that is odd-length is malformed (truncated/corrupt) hex,
	// not binary — flag it precisely rather than double-encoding it as bytes.
	// This check applies to files and stdin too, where an odd-length hex payload
	// would otherwise be silently re-encoded and broadcast as a wrong tx.
	if hexRe.MatchString(s) {
		if len(s)%2 == 0 {
			return s, nil
		}
		return "", fmt.Errorf("%q looks like hex but has an odd number of digits (%d)", truncateArg(arg), len(s))
	}
	// Not hex at all: a real file (or stdin) may hold raw binary — encode it. The
	// original bytes are encoded, not the whitespace-stripped view.
	if arg == "-" || fileExists(arg) {
		return hex.EncodeToString(raw), nil
	}
	return "", fmt.Errorf("%q is neither valid hex nor an existing file", truncateArg(arg))
}

func truncateArg(a string) string {
	if len(a) > 40 {
		return a[:40] + "..."
	}
	return a
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func init() {
	rootCmd.AddCommand(broadcastCmd)
}
