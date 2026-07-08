package cmd

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/cobra"
)

var watchNoReconnect bool

var watchCmd = &cobra.Command{
	Use:   "watch [channel...]",
	Short: "Stream live events over WebSocket (blocks, mempool, address:<addr>, ...)",
	Long: `Subscribe to the server's WebSocket hub and print each event as one JSON
line. Default channel is "blocks". Channels include:

  blocks              new blocks and block-stats updates
  mempool             mempool activity
  address:<addr>      transactions touching an address
  scripthash:<hash>   transactions touching a script hash

The connection is re-established automatically unless --no-reconnect is
set. Interrupt (Ctrl-C) to stop.`,
	Example: `  bb watch
  bb watch blocks mempool
  bb watch address:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa`,
	RunE: func(cmd *cobra.Command, args []string) error {
		channels := args
		if len(channels) == 0 {
			channels = []string{"blocks"}
		}

		wsURL, err := websocketURL(host())
		if err != nil {
			return err
		}

		ctx := cmd.Context()
		backoff := time.Second
		for {
			connected, err := streamOnce(cmd, wsURL, channels)
			if ctx.Err() != nil {
				return nil // interrupted
			}
			if watchNoReconnect {
				return err
			}
			// A connection that actually established resets the backoff, so a
			// long-lived stream that later drops reconnects promptly rather than
			// inheriting the backoff grown during an earlier outage.
			if connected {
				backoff = time.Second
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "connection lost (%v); reconnecting in %s\n", err, backoff)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
		}
	},
}

// websocketURL converts the HTTP base URL into the /ws endpoint URL.
func websocketURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	default:
		return "", fmt.Errorf("unsupported scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/ws"
	return u.String(), nil
}

// streamOnce connects, subscribes, and pumps messages until the connection
// drops or the context is cancelled. The returned bool reports whether the
// connection was actually established, so the caller can reset its backoff.
func streamOnce(cmd *cobra.Command, wsURL string, channels []string) (bool, error) {
	ctx := cmd.Context()
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	// The server's CheckOrigin rejects origin-less upgrades (non-browser
	// clients must declare themselves); the host's own origin is allowlisted.
	hdr := http.Header{"Origin": {host()}}
	conn, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	for _, ch := range channels {
		if err := conn.WriteMessage(websocket.TextMessage, []byte("subscribe:"+ch)); err != nil {
			return true, err
		}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "connected to %s, subscribed: %s\n", wsURL, strings.Join(channels, ", "))

	// Close the connection when the context is cancelled so ReadMessage
	// unblocks promptly on Ctrl-C.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-done:
		}
	}()

	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return true, err
		}
		fmt.Fprintln(cmd.OutOrStdout(), string(msg))
	}
}

func init() {
	watchCmd.Flags().BoolVar(&watchNoReconnect, "no-reconnect", false, "exit when the connection drops")
	rootCmd.AddCommand(watchCmd)
}
