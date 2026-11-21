package cmd

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const tipHashHex = "000000000000000004a288072ebb35e37233f419918f9783d499979cb6ac33eb"

// The server has no /block/tip/protocols route; the tip must be resolved to a
// hash first.
func TestBlockTipProtocolsResolvesTip(t *testing.T) {
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/block/tip" {
			fmt.Fprintf(w, `{"hash":%q,"height":800000}`, tipHashHex)
			return
		}
		io.WriteString(w, `{"ord":1}`)
	})
	if _, errOut, err := runBB(t, rs.srv.URL, "block", "tip", "--protocols"); err != nil {
		t.Fatalf("%v (stderr %q)", err, errOut)
	}
	want := []string{"/api/v1/block/tip", "/api/v1/block/" + tipHashHex + "/protocols"}
	if got := rs.requests(); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("requests %v, want %v", got, want)
	}
}

func TestBlockTipWithoutProtocols(t *testing.T) {
	rs := newRecordingServer(t, nil)
	if _, _, err := runBB(t, rs.srv.URL, "block", "tip"); err != nil {
		t.Fatal(err)
	}
	if got := rs.requests(); len(got) != 1 || got[0] != "/api/v1/block/tip" {
		t.Fatalf("requests %v", got)
	}
}

// tokenServer serves n native token balances and, when total >= 0, a WoC
// balance envelope with that total_count (otherwise the WoC route 500s).
func tokenServer(t *testing.T, n, total int) *recordingServer {
	return newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokens"):
			items := make([]string, n)
			for i := range items {
				items[i] = fmt.Sprintf(`{"token_id":"t%d_0","balance":"1"}`, i)
			}
			io.WriteString(w, "["+strings.Join(items, ",")+"]")
		case strings.HasSuffix(r.URL.Path, "/balance") && total >= 0:
			fmt.Fprintf(w, `{"tokens":[],"total_count":%d}`, total)
		default:
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		}
	})
}

func TestAddressTokensTruncationWarning(t *testing.T) {
	const addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	cases := []struct {
		name     string
		n, total int
		want     string // "" = no warning
	}{
		{"complete", 3, 3, ""},
		{"truncated", 5, 1200, "warning: showing 5 of 1200 tokens"},
		{"total unavailable, under cap", 3, -1, ""},
		{"total unavailable, at cap", nativeTokenCap, -1, "warning: 1000 tokens returned; the list may be truncated"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rs := tokenServer(t, tc.n, tc.total)
			out, errOut, err := runBB(t, rs.srv.URL, "address", addr, "--tokens")
			if err != nil {
				t.Fatalf("%v (stderr %q)", err, errOut)
			}
			if !strings.Contains(out, `"t0_0"`) {
				t.Fatalf("token list not printed: %q", out)
			}
			if tc.want == "" && errOut != "" || !strings.Contains(errOut, tc.want) {
				t.Fatalf("stderr %q, want %q", errOut, tc.want)
			}
			got := rs.requests()
			if len(got) != 2 || got[1] != "/api/v1/bsv/main/token/bsv21/"+addr+"/balance" {
				t.Fatalf("requests %v", got)
			}
		})
	}
}

func TestWatchChannelValidation(t *testing.T) {
	hex := strings.Repeat("ab", 32)
	for _, ch := range []string{"blocks", "mempool", "lock:address:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa",
		"spend:address:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "lock:scripthash:" + hex, "spend:scripthash:" + hex, "tx:" + hex} {
		if err := validateWatchChannel(ch); err != nil {
			t.Errorf("%s: %v", ch, err)
		}
	}
	for _, ch := range []string{"address:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "scripthash:" + hex, "lock:address:",
		"lock:" + hex, "tx:" + strings.ToUpper(hex), "lock:scripthash:" + hex[:62], "block"} {
		if err := validateWatchChannel(ch); err == nil {
			t.Errorf("%s: accepted", ch)
		}
	}
}

func TestRejectedChannelFrame(t *testing.T) {
	if rejectedChannel([]byte(`{"error":"invalid subscribe channel: lock:address:x"}`)) == nil {
		t.Fatal("rejection frame not recognised")
	}
	for _, f := range []string{`{"type":"block","height":1}`, `{"error":"subscription rejected for: blocks"}`, `not json`} {
		if rejectedChannel([]byte(f)) != nil {
			t.Fatalf("%s treated as a rejection", f)
		}
	}
}

// A server rejection of a channel name ends `bb watch` (even with reconnect
// on) instead of leaving it subscribed to nothing.
func TestWatchExitsOnRejectedChannel(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte(`{"error":"invalid subscribe channel: blocks"}`))
		time.Sleep(10 * time.Second)
	}))
	t.Cleanup(srv.Close)
	done := make(chan error, 1)
	go func() {
		_, _, err := runBB(t, srv.URL, "watch", "blocks")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid subscribe channel") {
			t.Fatalf("got %v, want the server's rejection", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watch kept running after the server rejected its channel")
	}
}

func TestHelpMatchesServerContract(t *testing.T) {
	if !strings.Contains(watchCmd.Long, "lock:address:<addr>") || strings.Contains(watchCmd.Long, " address:<addr>      transactions touching") ||
		!strings.Contains(watchCmd.Example, "bb watch lock:address:") {
		t.Fatal("watch help still names channels the server rejects")
	}
	if !strings.Contains(getCmd.Example, `"rawtx"`) || strings.Contains(getCmd.Example, "txhex") {
		t.Fatal("tools/decode example must send rawtx")
	}
}

func TestTestnetHostWarning(t *testing.T) {
	for _, tc := range []struct {
		host  string
		chain []string
		warn  bool
	}{
		{"https://test.bananablocks.com", nil, true},
		{"https://test.bananablocks.com", []string{"--chain", "test"}, false},
		{"https://bananablocks.com", nil, false},
	} {
		resetFlags(rootCmd)
		flagHost = tc.host
		if len(tc.chain) > 0 {
			flagChain = tc.chain[1]
		}
		var b strings.Builder
		rootCmd.SetErr(&b)
		warnTestnetHostOnMain(rootCmd)
		if got := strings.Contains(b.String(), "--chain test"); got != tc.warn {
			t.Errorf("%s %v: warned=%v (%q)", tc.host, tc.chain, got, b.String())
		}
	}
	resetFlags(rootCmd)
}
