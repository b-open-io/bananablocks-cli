package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestReadTxHexArgWrappedHex is the P3 guard: line-wrapped hex (as produced by
// `xxd -p`) must be read as the same transaction as its single-line form, not
// hex-encoded again as if it were binary.
func TestReadTxHexArgWrappedHex(t *testing.T) {
	const flat = "0100beef0000ff00ff00"
	wrapped := "0100beef00\n00ff00ff00\n"
	f := filepath.Join(t.TempDir(), "wrapped.hex")
	if err := os.WriteFile(f, []byte(wrapped), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTxHexArg(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != flat {
		t.Fatalf("wrapped hex = %q, want %q", got, flat)
	}
}

// TestReadTxHexArgSpacedInline covers a hex string argument with spaces.
func TestReadTxHexArgSpacedInline(t *testing.T) {
	got, err := readTxHexArg("0100 beef 00")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0100beef00" {
		t.Fatalf("got %q", got)
	}
}

// TestReadTxHexArgBinaryFile confirms a genuinely binary file (non-hex bytes)
// is still hex-encoded from its original bytes.
func TestReadTxHexArgBinaryFile(t *testing.T) {
	bin := []byte{0x01, 0x00, 0xbe, 0xef, 0x80, 0x0a} // 0x80 is not a hex digit
	f := filepath.Join(t.TempDir(), "tx.bin")
	if err := os.WriteFile(f, bin, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := readTxHexArg(f)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(bin) {
		t.Fatalf("binary file = %q, want %q", got, hex.EncodeToString(bin))
	}
}

// TestReadTxHexArgRejectsNonFile keeps the "not hex, not a file" error.
func TestReadTxHexArgRejectsNonFile(t *testing.T) {
	_, err := readTxHexArg("not-hex-and-not-a-file.zzz")
	if err == nil || !strings.Contains(err.Error(), "neither valid hex nor an existing file") {
		t.Fatalf("got %v", err)
	}
}

// TestReadTxHexArgOddLengthHex confirms a plain-string all-hex argument with an
// odd number of digits reports that precisely rather than the misleading
// "neither valid hex nor an existing file".
func TestReadTxHexArgOddLengthHex(t *testing.T) {
	_, err := readTxHexArg("0100beef0") // 9 hex digits
	if err == nil || !strings.Contains(err.Error(), "odd number of digits") {
		t.Fatalf("got %v, want an odd-length-hex error", err)
	}
}

// TestReadTxHexArgOddLengthHexFile confirms an odd-length all-hex FILE (a
// truncated/corrupt hex dump) is flagged as malformed hex rather than silently
// double-encoded and broadcast as a wrong transaction.
func TestReadTxHexArgOddLengthHexFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "truncated.hex")
	if err := os.WriteFile(f, []byte("0100beef0\n"), 0o644); err != nil { // 9 hex digits
		t.Fatal(err)
	}
	_, err := readTxHexArg(f)
	if err == nil || !strings.Contains(err.Error(), "odd number of digits") {
		t.Fatalf("got %v, want an odd-length-hex error", err)
	}
}

// multiServer mimics /tx/broadcast/multi with a per-caller batch cap: a
// batch over maxBatch gets the server's 413 with max_batch_txs, an optional
// number of leading 429s, and otherwise one accepted entry per tx.
func multiServer(t *testing.T, maxBatch, rateLimited int, entry func(rawtx string) map[string]any) (*httptest.Server, *[]int) {
	t.Helper()
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/bsv/main/tx/broadcast/multi" {
			http.NotFound(w, r)
			return
		}
		var batch []map[string]string
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Errorf("bad body: %v", err)
			w.WriteHeader(400)
			return
		}
		for _, e := range batch {
			if _, ok := e["rawtx"]; !ok || len(e) != 1 {
				t.Errorf("entry %v is not {\"rawtx\": ...}", e)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if len(batch) > maxBatch {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			fmt.Fprintf(w, `{"error":"split it","max_batch_txs":%d}`, maxBatch)
			return
		}
		if rateLimited > 0 {
			rateLimited--
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"broadcast rate limit exceeded"}`)
			return
		}
		sizes = append(sizes, len(batch))
		out := make([]map[string]any, 0, len(batch))
		for _, e := range batch {
			out = append(out, entry(e["rawtx"]))
		}
		json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(srv.Close)
	return srv, &sizes
}

func acceptedEntry(rawtx string) map[string]any {
	return map[string]any{"txid": rawtx, "txStatus": "SEEN_ON_NETWORK", "accepted": true}
}

func useHost(t *testing.T, u string) {
	t.Helper()
	old := flagHost
	flagHost = u
	t.Cleanup(func() { flagHost = old })
}

// TestBroadcastBatchesSplitOn413 is the anonymous-caller guard: a batch over
// the caller's per-server share is split using max_batch_txs, in order, and
// a 429 is waited out rather than failing the command.
func TestBroadcastBatchesSplitOn413(t *testing.T) {
	slept := noSleep(t)
	srv, sizes := multiServer(t, 3, 1, acceptedEntry)
	useHost(t, srv.URL)
	hexes := []string{"01", "02", "03", "04", "05", "06", "07"}

	var errw bytes.Buffer
	res, err := broadcastBatches(context.Background(), client(), &errw, hexes)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(hexes) {
		t.Fatalf("results = %d, want %d", len(res), len(hexes))
	}
	for i, raw := range res {
		var e struct{ TxID string }
		json.Unmarshal(raw, &e)
		if e.TxID != hexes[i] {
			t.Fatalf("result %d is %q, want %q (order must be kept)", i, e.TxID, hexes[i])
		}
	}
	if fmt.Sprint(*sizes) != "[3 3 1]" {
		t.Fatalf("batch sizes %v, want [3 3 1]", *sizes)
	}
	if len(*slept) != 1 || (*slept)[0] != 3*time.Second {
		t.Fatalf("waits %v, want one 3s Retry-After wait", *slept)
	}
	if err := checkAccepted(res); err != nil {
		t.Fatal(err)
	}
}

// TestCheckAccepted: a 200 whose entries are pending or refused must fail.
func TestCheckAccepted(t *testing.T) {
	res := []json.RawMessage{
		json.RawMessage(`{"txid":"aa","txStatus":"SEEN_ON_NETWORK","accepted":true}`),
		json.RawMessage(`{"txid":"bb","txStatus":"QUEUED","accepted":false}`),
		json.RawMessage(`{"txid":"cc","status":465,"error":"fee too low","accepted":false}`),
		json.RawMessage(`{"weird":1}`),
	}
	err := checkAccepted(res)
	if err == nil {
		t.Fatal("pending/refused entries must fail")
	}
	for _, want := range []string{"3 of 4", "bb (QUEUED)", "cc (fee too low)", "#4"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
	if checkAccepted(res[:1]) != nil {
		t.Fatal("all-accepted must pass")
	}
}

// TestBroadcastBatchesUpstreamFailure: a 503 mid-run keeps earlier results
// and warns that the failed batch may already be on the network.
func TestBroadcastBatchesUpstreamFailure(t *testing.T) {
	noSleep(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var batch []map[string]string
		json.NewDecoder(r.Body).Decode(&batch)
		if calls == 1 {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			io.WriteString(w, `{"error":"split","max_batch_txs":2}`)
			return
		}
		if calls == 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, `{"error":"broadcast service unavailable"}`)
			return
		}
		out := []map[string]any{}
		for _, e := range batch {
			out = append(out, acceptedEntry(e["rawtx"]))
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()
	useHost(t, srv.URL)

	res, err := broadcastBatches(context.Background(), client(), io.Discard, []string{"01", "02", "03", "04"})
	if len(res) != 2 {
		t.Fatalf("want the first batch's 2 results kept, got %d", len(res))
	}
	if err == nil || !strings.Contains(err.Error(), "transactions 3-4") || !strings.Contains(err.Error(), "may already be on the network") {
		t.Fatalf("got %v", err)
	}
}

func TestBatchEndRespectsBodyCap(t *testing.T) {
	big := strings.Repeat("ab", maxMultiBodyBytes/4) // ~half the cap each
	hexes := []string{big, big, big}
	if got := batchEnd(hexes, 0, 100); got != 1 {
		t.Fatalf("batchEnd = %d, want 1 (two halves plus framing exceed the cap)", got)
	}
	if got := batchEnd([]string{"01", "02"}, 0, 1); got != 1 {
		t.Fatalf("limit not applied: %d", got)
	}
}
