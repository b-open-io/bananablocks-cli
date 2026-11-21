package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// TestJSONDecodesUnderCap confirms a normal JSON body decodes fine.
func TestJSONDecodesUnderCap(t *testing.T) {
	c := newTestClient(t, func(rw http.ResponseWriter, r *http.Request) {
		json.NewEncoder(rw).Encode(map[string]int{"n": 42})
	})
	var out struct {
		N int `json:"n"`
	}
	if err := c.GetJSON(context.Background(), "/x", nil, &out); err != nil {
		t.Fatal(err)
	}
	if out.N != 42 {
		t.Fatalf("got %d", out.N)
	}
}

// TestJSONRejectsOversized is the P1 guard: a body beyond the cap errors
// instead of decoding an unbounded stream into memory.
func TestJSONRejectsOversized(t *testing.T) {
	orig := maxBufferedResponse
	maxBufferedResponse = 64
	t.Cleanup(func() { maxBufferedResponse = orig })

	c := newTestClient(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte("[" + strings.Repeat("1,", 1000) + "1]"))
	})
	var out json.RawMessage
	err := c.GetJSON(context.Background(), "/big", nil, &out)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an over-cap error, got %v", err)
	}
}

// TestJSONNilOutAlsoCapped confirms the drain path (out == nil) is bounded too.
func TestJSONNilOutAlsoCapped(t *testing.T) {
	orig := maxBufferedResponse
	maxBufferedResponse = 64
	t.Cleanup(func() { maxBufferedResponse = orig })

	c := newTestClient(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(strings.Repeat("x", 1000)))
	})
	err := c.JSON(context.Background(), http.MethodPost, "/drain", nil, "", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an over-cap error, got %v", err)
	}
}

// TestBytesRejectsOversized covers the same cap on the raw-bytes path.
func TestBytesRejectsOversized(t *testing.T) {
	orig := maxBufferedResponse
	maxBufferedResponse = 64
	t.Cleanup(func() { maxBufferedResponse = orig })

	c := newTestClient(t, func(rw http.ResponseWriter, r *http.Request) {
		rw.Write([]byte(strings.Repeat("y", 1000)))
	})
	if _, _, err := c.Bytes(context.Background(), "/big", nil); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("expected an over-cap error, got %v", err)
	}
}

// TestPaymentAcceptOnlyWithAPIKey: an anonymous caller's rate-limit 402 has no
// challenge to pay, so only keyed requests advertise x402 and anonymous ones
// keep the plain 429.
func TestPaymentAcceptOnlyWithAPIKey(t *testing.T) {
	for _, tc := range []struct {
		key  string
		want string
	}{{"", ""}, {"bb_test_k", "x402"}} {
		var got string
		c := newTestClient(t, func(rw http.ResponseWriter, r *http.Request) {
			got = r.Header.Get("X-Payment-Accept")
			rw.Write([]byte(`{}`))
		})
		c.APIKey = tc.key
		if err := c.GetJSON(context.Background(), "/x", nil, nil); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Fatalf("key %q: X-Payment-Accept = %q, want %q", tc.key, got, tc.want)
		}
	}
}
