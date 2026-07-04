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
	if out == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// GetJSON is JSON with method GET and no body.
func (c *Client) GetJSON(ctx context.Context, path string, query url.Values, out any) error {
	return c.JSON(ctx, http.MethodGet, path, query, "", nil, out)
}

// Bytes performs a GET and returns the raw body plus response headers.
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return raw, resp.Header, nil
}

// PostJSONBody marshals v and POSTs it as application/json.
func (c *Client) PostJSONBody(ctx context.Context, path string, query url.Values, v any, out any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.JSON(ctx, http.MethodPost, path, query, "application/json", bytes.NewReader(raw), out)
}
