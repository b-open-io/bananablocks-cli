package cmd

import (
	"fmt"
	"strings"
)

// normalizeTxID trims, lowercases, and validates a transaction id as 64 hex
// characters, so malformed ids fail locally instead of being escaped into an
// API path. It reuses hexRe and truncateArg from broadcast.go.
func normalizeTxID(s string) (string, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	if len(t) != 64 || !hexRe.MatchString(t) {
		return "", fmt.Errorf("invalid txid %q", truncateArg(t))
	}
	return t, nil
}
