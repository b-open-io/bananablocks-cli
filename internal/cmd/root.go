// Package cmd implements the bb command tree.
package cmd

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/spf13/cobra"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

const defaultHost = "https://bananablocks.com"

var (
	flagHost    string
	flagAPIKey  string
	flagChain   string
	flagTimeout time.Duration
)

var rootCmd = &cobra.Command{
	Use:   "bb",
	Short: "Command-line client for the BananaBlocks explorer API",
	Long: `bb is a command-line client for the BananaBlocks BSV explorer API.

It covers block, transaction, address, and token queries, SPV proof
verification (BEEF / TSC), transaction broadcast, live event streaming over
WebSocket, and x402 pay-to-upgrade for API-key rate-limit tiers.

Configuration:
  --host      or BB_HOST      target server (default ` + defaultHost + `)
  --api-key   or BB_API_KEY   API key (bb_live_... / bb_test_...)
`,
	SilenceUsage:  true,
	SilenceErrors: true,
}

// Execute runs the root command, printing any error to stderr. It installs a
// signal-aware context so Ctrl-C cancels the command's context (which long-
// running commands like `watch` observe) rather than only killing the process.
func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := rootCmd.ExecuteContext(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		offerUpgradeOn402(os.Stderr, err)
	}
	return err
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVar(&flagHost, "host", "", "server base URL (env BB_HOST, default "+defaultHost+")")
	pf.StringVar(&flagAPIKey, "api-key", "", "API key (env BB_API_KEY)")
	pf.StringVar(&flagChain, "chain", "main", "chain segment for WhatsonChain-compatible endpoints")
	pf.DurationVar(&flagTimeout, "timeout", 30*time.Second, "HTTP request timeout")
	rootCmd.PersistentPreRunE = func(*cobra.Command, []string) error { return validateHost() }
}

// validateHost fails fast with a clear message when the resolved host carries
// an unusable scheme, rather than surfacing an opaque error deep in request or
// WebSocket construction.
func validateHost() error {
	h := host()
	u, err := url.Parse(h)
	if err != nil {
		return fmt.Errorf("invalid host %q: %w", h, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported host scheme %q in %q (want http or https)", u.Scheme, h)
	}
	return nil
}

// host resolves the target server from flag, env, or default.
func host() string {
	if flagHost != "" {
		return normalizeHost(flagHost)
	}
	if h := os.Getenv("BB_HOST"); h != "" {
		return normalizeHost(h)
	}
	return defaultHost
}

func normalizeHost(h string) string {
	h = strings.TrimRight(strings.TrimSpace(h), "/")
	if !strings.Contains(h, "://") {
		h = "https://" + h
	}
	return h
}

func apiKey() string {
	if flagAPIKey != "" {
		return flagAPIKey
	}
	return os.Getenv("BB_API_KEY")
}

// client builds the API client from global flags.
func client() *api.Client {
	return &api.Client{
		BaseURL:   host(),
		APIKey:    apiKey(),
		UserAgent: "bananablocks-cli/" + Version,
		HTTP:      &http.Client{Timeout: flagTimeout},
	}
}

// streamClient builds a client for streaming potentially large responses. Its
// http.Client sets no overall Timeout — that would also bound the body read and
// abort a long download mid-copy; instead the per-request timeout bounds only
// how long the server may take to send response headers.
func streamClient() *api.Client {
	c := client()
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = flagTimeout
	c.HTTP = &http.Client{Transport: tr}
	return c
}

// wocPath prefixes p with the WhatsonChain-compatible base for --chain.
func wocPath(p string) string {
	return "/api/v1/bsv/" + flagChain + p
}
