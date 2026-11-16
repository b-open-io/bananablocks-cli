package cmd

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// recordingServer answers every request from route (path → handler) and
// records each request's path and raw query.
type recordingServer struct {
	mu   sync.Mutex
	reqs []string
	srv  *httptest.Server
}

func newRecordingServer(t *testing.T, route func(w http.ResponseWriter, r *http.Request)) *recordingServer {
	t.Helper()
	rs := &recordingServer{}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rs.mu.Lock()
		u := r.URL.EscapedPath()
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		rs.reqs = append(rs.reqs, u)
		rs.mu.Unlock()
		if route != nil {
			route(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(rs.srv.Close)
	return rs
}

func (rs *recordingServer) requests() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.reqs...)
}

// resetFlags restores every flag in the tree to its default, so one test's
// flags never leak into the next run of the shared command tree.
func resetFlags(c *cobra.Command) {
	reset := func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	}
	c.Flags().VisitAll(reset)
	c.PersistentFlags().VisitAll(reset)
	for _, sub := range c.Commands() {
		resetFlags(sub)
	}
}

// runBB runs `bb <args>` against host and returns stdout, stderr and the
// error.
func runBB(t *testing.T, host string, args ...string) (string, string, error) {
	t.Helper()
	resetFlags(rootCmd)
	t.Cleanup(func() { resetFlags(rootCmd) })
	var out, errb bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errb)
	rootCmd.SetArgs(append([]string{"--host", host}, args...))
	err := rootCmd.ExecuteContext(context.Background())
	return out.String(), errb.String(), err
}

func TestFeatureQueries(t *testing.T) {
	const addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"blocks", "--from", "800000", "--limit", "5"}, "/api/v1/blocks?from=800000&limit=5"},
		{[]string{"blocks", "--from", "0"}, "/api/v1/blocks?from=0"},
		{[]string{"address", addr, "--txs", "--from-height", "800000", "--limit", "50"}, "/api/v1/address/" + addr + "/txs?from_height=800000&limit=50"},
		{[]string{"address", addr, "--txs", "--cursor", "abc"}, "/api/v1/address/" + addr + "/txs?cursor=abc"},
		{[]string{"address", addr, "--utxos", "--page", "0", "--limit", "100"}, "/api/v1/address/" + addr + "/utxos?limit=100&page=0"},
		{[]string{"tokens", "--page", "2", "--limit", "25"}, "/api/v1/tokens?limit=25&page=2"},
		{[]string{"token", "abc_0", "--holders", "--page", "1"}, "/api/v1/token/abc_0/holders?page=1"},
		{[]string{"token", "abc_0", "--history", "--limit", "100"}, "/api/v1/token/abc_0/history?limit=100"},
		{[]string{"token", "abc_0", "--funding"}, "/api/v1/token/abc_0/funding"},
		{[]string{"stats", "network", "--window", "24h"}, "/api/v1/stats/network?window=24h"},
		{[]string{"stats", "network", "--days", "90"}, "/api/v1/stats/network?days=90"},
		{[]string{"stats", "protocols", "--days", "30"}, "/api/v1/stats/protocols?days=30"},
		{[]string{"stats", "price", "--days", "365"}, "/api/v1/stats/price-history?days=365"},
		{[]string{"stats", "richlist", "--page", "1", "--limit", "100"}, "/api/v1/stats/richlist?limit=100&page=1"},
		{[]string{"status"}, "/api/v1/status/indexing"},
		{[]string{"tx", txid, "--proof", "--target-type", "hash"}, "/api/v1/tx/" + txid + "/proof?targetType=hash"},
		{[]string{"tx", txid, "--proof"}, "/api/v1/tx/" + txid + "/proof"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			rs := newRecordingServer(t, nil)
			if _, errOut, err := runBB(t, rs.srv.URL, tc.args...); err != nil {
				t.Fatalf("%v (stderr %q)", err, errOut)
			}
			if got := rs.requests(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("requests %v, want [%s]", got, tc.want)
			}
		})
	}
}

// Flag combinations the API would ignore or refuse fail before any request.
func TestFeatureFlagValidation(t *testing.T) {
	const addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"address", addr, "--txs", "--page", "1"}, "--page applies only to --utxos"},
		{[]string{"address", addr, "--utxos", "--page", "1", "--cursor", "x"}, "either --page or --cursor"},
		{[]string{"address", addr, "--txs", "--from-height", "5", "--cursor", "x"}, "either --cursor or --from-height"},
		{[]string{"address", addr, "--utxos", "--from-height", "5"}, "--from-height applies only to --txs"},
		{[]string{"address", addr, "--balance", "--limit", "5"}, "--limit applies"},
		{[]string{"address", addr, "--cursor", "x"}, "--cursor applies"},
		{[]string{"address", addr, "--txs", "-o", "f"}, "-o applies"},
		{[]string{"address", addr, "--csv", "--txs"}, "pick at most one"},
		{[]string{"tokens", "--limit", "101"}, "between 1 and 100"},
		{[]string{"token", "abc_0", "--holders", "--limit", "0"}, "between 1 and 100"},
		{[]string{"tokens", "--page", "-1"}, "cannot be negative"},
		{[]string{"token", "abc_0", "--page", "1"}, "apply to --holders and --history"},
		{[]string{"token", "abc_0", "--funding", "--icon"}, "pick at most one"},
		{[]string{"token", "abc_0", "-o", "x"}, "-o applies to --icon"},
		{[]string{"stats", "summary", "--days", "3"}, "does not apply"},
		{[]string{"stats", "price", "--window", "24h"}, "does not apply"},
		{[]string{"stats", "network", "--window", "7d"}, "supports only 24h"},
		{[]string{"stats", "network", "--days", "0"}, "must be positive"},
		{[]string{"stats", "richlist", "--limit", "7"}, "one of 10, 20, 50 or 100"},
		{[]string{"blocks", "--from", "-2"}, "--from must be"},
		{[]string{"tx", txid, "--target-type", "hash"}, "applies only to --proof"},
		{[]string{"tx", txid, "--proof", "--target-type", "header"}, "merkleRoot or hash"},
		{[]string{"tx", txid, "--proof", "--target-type", "hash", "--verify"}, "--verify needs the merkle root"},
		{[]string{"tx", txid, "--bin", "--hex"}, "pick at most one"},
		{[]string{"tx", txid, "--bin", "--verify"}, "--verify cannot be combined with --bin"},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			rs := newRecordingServer(t, nil)
			_, _, err := runBB(t, rs.srv.URL, tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want an error mentioning %q", err, tc.want)
			}
			if got := rs.requests(); len(got) != 0 {
				t.Fatalf("validation error still sent %v", got)
			}
		})
	}
}

func TestAddressUTXOsBEEF(t *testing.T) {
	const addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Has-More", "true")
		w.Header().Set("X-Next-Cursor", "NEXT")
		w.Header().Set("X-BEEF-Proven-UTXOs", "2")
		w.Header().Set("X-BEEF-Omitted-UTXOs", "1")
		w.Header().Set("X-BEEF-Omitted-Txids", "aa,bb")
		w.Write([]byte{0x01, 0x00, 0xbe, 0xef})
	})
	out := filepath.Join(t.TempDir(), "u.beef")
	_, errOut, err := runBB(t, rs.srv.URL, "address", addr, "--utxos-beef", "--cursor", "C1", "--limit", "2", "-o", out)
	if err != nil {
		t.Fatal(err)
	}
	if got := rs.requests(); got[0] != "/api/v1/address/"+addr+"/utxos/beef?cursor=C1&limit=2" {
		t.Fatalf("request %v", got)
	}
	raw, _ := os.ReadFile(out)
	if !bytes.Equal(raw, []byte{0x01, 0x00, 0xbe, 0xef}) {
		t.Fatalf("file %x", raw)
	}
	for _, want := range []string{"proven UTXOs: 2", "1 UTXO(s)", "aa bb", "--cursor NEXT"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q missing %q", errOut, want)
		}
	}
	// Without -o the BEEF is hex on stdout.
	stdout, _, err := runBB(t, rs.srv.URL, "address", addr, "--utxos-beef")
	if err != nil || strings.TrimSpace(stdout) != "0100beef" {
		t.Fatalf("stdout %q, %v", stdout, err)
	}
}

func TestAddressCSV(t *testing.T) {
	const addr = "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	const csv = "txid,block_height\naa,1\n# truncated,\n"
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("X-Truncated", "true")
		w.Header().Set("X-Partial", "true")
		io.WriteString(w, csv)
	})
	stdout, errOut, err := runBB(t, rs.srv.URL, "address", addr, "--csv")
	if err != nil {
		t.Fatal(err)
	}
	if rs.requests()[0] != "/api/v1/download/address/"+addr+"/history.csv" {
		t.Fatalf("request %v", rs.requests())
	}
	if stdout != csv || !strings.Contains(errOut, "truncated") || !strings.Contains(errOut, "partial") {
		t.Fatalf("stdout %q stderr %q", stdout, errOut)
	}
	out := filepath.Join(t.TempDir(), "h.csv")
	if _, _, err := runBB(t, rs.srv.URL, "address", addr, "--csv", "-o", out); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); string(raw) != csv {
		t.Fatalf("file %q", raw)
	}
}

func TestTokenIcon(t *testing.T) {
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("PNG"))
	})
	dir := t.TempDir()
	wd, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(wd)
	if _, _, err := runBB(t, rs.srv.URL, "token", "../evil_0", "--icon"); err != nil {
		t.Fatal(err)
	}
	if rs.requests()[0] != "/api/v1/token/..%2Fevil_0/icon" {
		t.Fatalf("request %v", rs.requests())
	}
	// The default name never escapes the current directory.
	if raw, err := os.ReadFile(filepath.Join(dir, "___evil_0.png")); err != nil || string(raw) != "PNG" {
		t.Fatalf("icon not saved under a safe name: %v", err)
	}
}

// "." and ".." are never sent as an id: they would climb out of the route.
func TestDotSegmentsRefused(t *testing.T) {
	rs := newRecordingServer(t, nil)
	for _, args := range [][]string{{"token", "..", "--icon"}, {"address", ".", "--csv"}, {"block", ".."}} {
		if _, _, err := runBB(t, rs.srv.URL, args...); err == nil || !strings.Contains(err.Error(), "invalid") {
			t.Errorf("%v: got %v", args, err)
		}
	}
	if got := rs.requests(); len(got) != 0 {
		t.Fatalf("sent %v", got)
	}
}

func TestMediaBareBFile(t *testing.T) {
	const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		io.WriteString(w, "hello")
	})
	stdout, _, err := runBB(t, rs.srv.URL, "media", txid, "--kind", "bfile", "--stdout")
	if err != nil || stdout != "hello" {
		t.Fatalf("stdout %q, %v", stdout, err)
	}
	if rs.requests()[0] != "/api/v1/tx/"+txid+"/bfile" {
		t.Fatalf("request %v", rs.requests())
	}
	// Other kinds still need an output index.
	if _, _, err := runBB(t, rs.srv.URL, "media", txid, "--kind", "inscription"); err == nil {
		t.Fatal("a bare txid needs --kind bfile")
	}
}

func TestTxBinAndTooLargeHint(t *testing.T) {
	const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/bin") {
			w.Write([]byte{0xde, 0xad})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		io.WriteString(w, `{"error":"transaction too large to decode","summary":{"txid":"`+txid+`","inputs":400000},"bin_url":"/tx/`+txid+`/bin"}`)
	})
	stdout, _, err := runBB(t, rs.srv.URL, "tx", txid, "--bin")
	if err != nil || strings.TrimSpace(stdout) != "dead" {
		t.Fatalf("--bin stdout %q, %v", stdout, err)
	}
	_, _, err = runBB(t, rs.srv.URL, "tx", txid)
	if err == nil || !strings.Contains(err.Error(), "--bin -o tx.bin") || !strings.Contains(err.Error(), `"inputs":400000`) {
		t.Fatalf("413 hint: %v", err)
	}
}

func TestHealth(t *testing.T) {
	rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok\n") })
	stdout, _, err := runBB(t, rs.srv.URL, "health")
	if err != nil || stdout != "ok\n" || rs.requests()[0] != "/health" {
		t.Fatalf("stdout %q, %v, %v", stdout, err, rs.requests())
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrShortWrite }

// A stdout write failure fails the command instead of reporting success.
func TestPayloadStdoutWriteFailure(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.SetOut(failingWriter{})
	if err := writePayloadTo(cmd, "", []byte{1, 2}, false); err == nil {
		t.Fatal("binary to a failing stdout must error")
	}
	if err := writePayloadTo(cmd, "-", []byte("ab"), true); err == nil {
		t.Fatal("text to a failing stdout must error")
	}
}

// Writing to a file goes through a temp file, so a failure leaves an existing
// file intact.
func TestPayloadFileReplacedAtomically(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "x.beef")
	if err := os.WriteFile(out, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	// A destination in a missing directory fails before touching anything.
	if err := writePayloadTo(cmd, filepath.Join(dir, "missing", "x"), []byte("new"), false); err == nil {
		t.Fatal("want an error")
	}
	if err := writePayloadTo(cmd, out, []byte("new"), false); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(out); string(raw) != "new" {
		t.Fatalf("file %q", raw)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

// The proof's targetType survives the round trip into bb's output.
func TestProofKeepsTargetType(t *testing.T) {
	const txid = "0e3e2357e806b6cdb1f70b54c3a3a17b6714ee1f0e68bebb44a74b1efd512098"
	for _, tt := range []string{"merkleRoot", "hash"} {
		rs := newRecordingServer(t, func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"index":0,"txOrId":"`+txid+`","targetType":"`+tt+`","target":"00","nodes":[]}`)
		})
		args := []string{"tx", txid, "--proof"}
		if tt == "hash" {
			args = append(args, "--target-type", "hash")
		}
		stdout, _, err := runBB(t, rs.srv.URL, args...)
		if err != nil || !strings.Contains(stdout, `"targetType": "`+tt+`"`) {
			t.Fatalf("%s: stdout %q, %v", tt, stdout, err)
		}
	}
}
