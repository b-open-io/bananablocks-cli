// Package api is a thin HTTP client for the BananaBlocks explorer API.
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Client talks to a BananaBlocks server (or any WhatsOnChain-compatible host
// for the /api/v1/bsv/{chain}/ endpoints).
type Client struct {
	BaseURL   string
	APIKey    string
	UserAgent string
	HTTP      *http.Client
}

// Error is a non-2xx API response. Message carries the server's {"error":...}
// body when present, otherwise the raw body.
type Error struct {
	Status  int
	Message string
	// Header keeps the response headers so callers can pull protocol data out
	// of error responses (e.g. the X402-Challenge header on a 402).
	Header http.Header
	// Body is the raw response body, useful when the payload carries more
	// than the error string (e.g. the challenge object on a 402).
	Body []byte
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// NewRequest builds a request with auth and UA headers applied.
func (c *Client) NewRequest(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader) (*http.Request, error) {
	u := strings.TrimRight(c.BaseURL, "/") + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	// Advertise x402 support: the server then answers rate-limited calls with
	// a payable 402 challenge instead of a bare 429 (only for clients that
	// opt in, so plain callers keep the historical 429).
	req.Header.Set("X-Payment-Accept", "x402")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

// Do executes req and converts non-2xx responses into *Error. On success the
// caller owns resp.Body.
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	apiErr := &Error{Status: resp.StatusCode, Header: resp.Header, Body: raw}
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		apiErr.Message = e.Error
	} else {
		apiErr.Message = strings.TrimSpace(string(raw))
	}
	return nil, apiErr
}

// ReadBody reads a response body the caller obtained via Do/NewRequest, bounded
// by maxBufferedResponse so a hostile or misconfigured host cannot stream
// unbounded data into a caller that decodes the body itself (the JSON and Bytes
// helpers apply the same cap internally).
func (c *Client) ReadBody(r io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(r, maxBufferedResponse+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxBufferedResponse {
		return nil, fmt.Errorf("response exceeds %d bytes", maxBufferedResponse)
	}
	return raw, nil
}

// JSON performs a request and decodes the JSON response into out. Pass a
// *json.RawMessage to keep the body verbatim.
func (c *Client) JSON(ctx context.Context, method, path string, query url.Values, contentType string, body io.Reader, out any) error {
	req, err := c.NewRequest(ctx, method, path, query, contentType, body)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Bound the body like Bytes does: a decoder reading straight from the
	// network lets a hostile or misconfigured host stream unbounded JSON.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBufferedResponse+1))
	if err != nil {
		return err
	}
	if int64(len(raw)) > maxBufferedResponse {
		return fmt.Errorf("response from %s exceeds %d bytes", path, maxBufferedResponse)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// GetJSON is JSON with method GET and no body.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.JSON(ctx, http.MethodGet, path, query, "", nil, out)
}

// maxBufferedResponse caps how much of a response Bytes and JSON will hold in
// memory, so a large or hostile host can't OOM the CLI. Content that can
// legitimately be larger (media downloads) should use Stream instead. It is a
// var only so tests can shrink it; production never reassigns it.
var maxBufferedResponse int64 = 256 << 20 // 256 MiB

// Bytes performs a GET and returns the raw body plus response headers. The body
// is bounded by maxBufferedResponse; use Stream for arbitrarily large content.
func (c *Client) Bytes(ctx context.Context, path string, query url.Values) ([]byte, http.Header, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, path, query, "", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBufferedResponse+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(raw)) > maxBufferedResponse {
		return nil, nil, fmt.Errorf("response from %s exceeds %d bytes", path, maxBufferedResponse)
	}
	return raw, resp.Header, nil
}

// Stream performs a GET and hands back the response body for the caller to copy
// without buffering it in memory. The caller owns and must Close the returned
// body. Use for potentially large content such as media downloads.
func (c *Client) Stream(ctx context.Context, path string, query url.Values) (io.ReadCloser, http.Header, error) {
	req, err := c.NewRequest(ctx, http.MethodGet, path, query, "", nil)
	if err != nil {
		return nil, nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, nil, err
	}
	return resp.Body, resp.Header, nil
}

// PostJSONBody marshals v and POSTs it as application/json.
func (c *Client) PostJSONBody(ctx context.Context, path string, query url.Values, v any, out any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.JSON(ctx, http.MethodPost, path, query, "application/json", bytes.NewReader(raw), out)
}
