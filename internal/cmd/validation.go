package cmd

import (
	"fmt"
	"net/url"
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

// pathSegment escapes an argument for use as one URL path segment. "." and
// ".." (and the empty string) are refused: escaping leaves them unchanged, so
// they would climb out of the intended route (e.g. /token/../icon).
func pathSegment(name, s string) (string, error) {
	t := strings.TrimSpace(s)
	if t == "" || t == "." || t == ".." {
		return "", fmt.Errorf("invalid %s %q", name, s)
	}
	return url.PathEscape(t), nil
}
