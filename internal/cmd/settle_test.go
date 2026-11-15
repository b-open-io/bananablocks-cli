package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/b-open-io/bananablocks-cli/internal/api"
	"github.com/b-open-io/bananablocks-cli/internal/x402"
)

// scriptedProofServer answers proof submits from a fixed script of
// (status, body, Retry-After) and records every X402-Proof header it saw.
type scriptedProofServer struct {
	mu      sync.Mutex
	answers []proofAnswer
	proofs  []string
	srv     *httptest.Server
}

type proofAnswer struct {
	status     int
	body       string
	retryAfter string
}

func newScriptedProofServer(t *testing.T, answers ...proofAnswer) *scriptedProofServer {
	t.Helper()
	s := &scriptedProofServer{answers: answers}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.proofs = append(s.proofs, r.Header.Get("X402-Proof"))
		if len(s.answers) == 0 {
			t.Errorf("unexpected extra proof submit")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		a := s.answers[0]
		s.answers = s.answers[1:]
		if a.retryAfter != "" {
			w.Header().Set("Retry-After", a.retryAfter)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var slept []time.Duration
	old := sleepCtx
	sleepCtx = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return ctx.Err() }
	t.Cleanup(func() { sleepCtx = old })
	return &slept
}

func savedFixture(t *testing.T, srvURL string) (*api.Client, *x402.ProofStore, *x402.SavedProof) {
	t.Helper()
	c := &api.Client{BaseURL: srvURL, APIKey: "bb_live_test", HTTP: http.DefaultClient}
	store := x402.NewProofStore(t.TempDir(), srvURL, "bb_live_test")
	ch := &x402.Challenge{ChallengeID: "ch-1", Tier: "pro", AmountSats: 5000, ExpiresAt: time.Now().Add(10 * time.Minute), PayURL: x402.UpgradePath}
	sp, err := newSavedProof(c, ch, []byte{1, 2, 3}, strings.Repeat("ab", 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}
	return c, store, sp
}

const settledBody = `{"tier":"pro","tier_expires_at":"2030-01-01T00:00:00Z","txid":"abab","amount_sats":5000}`

// TestSettleProofResubmitsSameProofWhilePending is the core guard: a 202
// pending answer is not a failure; the SAME proof is resubmitted after
// Retry-After until it settles, and the saved proof is then removed.
func TestSettleProofResubmitsSameProofWhilePending(t *testing.T) {
	slept := noSleep(t)
	s := newScriptedProofServer(t,
		proofAnswer{http.StatusAccepted, `{"status":"pending","error":"payment broadcast but not yet accepted by the network; resubmit the same proof"}`, "7"},
		proofAnswer{http.StatusServiceUnavailable, `{"error":"upgrade failed"}`, ""},
		proofAnswer{http.StatusOK, settledBody, ""},
	)
	c, store, sp := savedFixture(t, s.srv.URL)

	res, err := settleProof(context.Background(), c, io.Discard, store, sp, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if res.Tier != "pro" {
		t.Fatalf("result %+v", res)
	}
	if len(s.proofs) != 3 {
		t.Fatalf("submits = %d, want 3", len(s.proofs))
	}
	for i, p := range s.proofs {
		if p != sp.ProofHeader {
			t.Fatalf("submit %d sent a different proof", i)
		}
	}
	if (*slept)[0] != 7*time.Second {
		t.Fatalf("first wait %s, want the server's Retry-After of 7s", (*slept)[0])
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Fatal("settled proof must be removed")
	}
}

// TestSettleProofPendingPastWaitKeepsProof: when the wait budget runs out the
// proof stays saved and the error says not to pay again.
func TestSettleProofPendingPastWaitKeepsProof(t *testing.T) {
	noSleep(t)
	s := newScriptedProofServer(t, proofAnswer{http.StatusAccepted, `{"status":"pending"}`, "10"})
	c, store, sp := savedFixture(t, s.srv.URL)

	_, err := settleProof(context.Background(), c, io.Discard, store, sp, time.Second)
	if err == nil || !strings.Contains(err.Error(), "Do NOT pay again") {
		t.Fatalf("want a keep-the-proof error, got %v", err)
	}
	if left, _ := store.List(); len(left) != 1 {
		t.Fatal("a pending proof must stay saved")
	}
}

// TestSettleProof422RetriesThenStops: a 422 is resubmitted a bounded number of
// times, then reported with the proof kept.
func TestSettleProof422RetriesThenStops(t *testing.T) {
	noSleep(t)
	var answers []proofAnswer
	for i := 0; i <= max422Retries; i++ {
		answers = append(answers, proofAnswer{http.StatusUnprocessableEntity, `{"error":"transaction rejected by broadcast"}`, ""})
	}
	s := newScriptedProofServer(t, answers...)
	c, store, sp := savedFixture(t, s.srv.URL)

	_, err := settleProof(context.Background(), c, io.Discard, store, sp, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("got %v", err)
	}
	if len(s.proofs) != max422Retries+1 {
		t.Fatalf("submits = %d, want %d", len(s.proofs), max422Retries+1)
	}
	if left, _ := store.List(); len(left) != 1 {
		t.Fatal("proof must stay saved after 422s")
	}
}

// TestSettleProofTerminalAnswers covers the answers that must not be retried,
// which ones drop the saved proof, and that none of them advise paying again.
func TestSettleProofTerminalAnswers(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		keep    bool
		mustSay string
	}{
		{"consumed", 409, `{"error":"challenge already consumed"}`, false, "bb key usage"},
		{"replay", 409, `{"error":"payment txid already used"}`, false, "may already have settled"},
		{"closed", 409, `{"error":"payment request closed: a payment to it was refused and is recorded for a refund or a manual credit; request a new one"}`, false, "can never settle"},
		{"refused", 409, `{"error":"payment already refused and recorded for a refund or a manual credit"}`, false, "can never settle"},
		{"not-upgrade", 409, `{"error":"key is already on a tier at or above this challenge"}`, true, "no tier was granted"},
		{"expired-in-grace", 410, `{"error":"challenge expired; request a new one"}`, true, "can still settle"},
		{"underpaid", 402, `{"error":"payment insufficient","challenge":{"challenge_id":"ch-1"}}`, true, "Do NOT pay the challenge again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			noSleep(t)
			s := newScriptedProofServer(t, proofAnswer{tc.status, tc.body, ""})
			c, store, sp := savedFixture(t, s.srv.URL)
			var errw bytes.Buffer
			_, err := settleProof(context.Background(), c, &errw, store, sp, time.Hour)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.mustSay) {
				t.Fatalf("error %q should mention %q", err, tc.mustSay)
			}
			if strings.Contains(err.Error(), "rerun to retry") {
				t.Fatalf("error must not advise paying again: %q", err)
			}
			if len(s.proofs) != 1 {
				t.Fatalf("terminal answer was retried (%d submits)", len(s.proofs))
			}
			left, _ := store.List()
			if tc.keep != (len(left) == 1) {
				t.Fatalf("keep=%v but %d proofs left", tc.keep, len(left))
			}
		})
	}
}

// TestUpgradeResubmitsSavedProofInsteadOfPaying: with a saved proof on disk,
// `bb key upgrade` resubmits it and never fetches a challenge or builds a new
// payment (the scripted server would fail on any request but the one proof).
func TestUpgradeResubmitsSavedProofInsteadOfPaying(t *testing.T) {
	noSleep(t)
	s := newScriptedProofServer(t, proofAnswer{http.StatusOK, settledBody, ""})
	state := t.TempDir()
	t.Setenv("BB_STATE_DIR", state)
	t.Setenv("BB_WIF", "")

	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = s.srv.URL, "bb_live_test"
	defer func() { flagHost, flagAPIKey = oldHost, oldKey }()

	store, err := proofStore()
	if err != nil {
		t.Fatal(err)
	}
	c := client()
	sp, err := newSavedProof(c, &x402.Challenge{ChallengeID: "old", Tier: "pro", PayURL: x402.UpgradePath}, []byte{9}, "cd")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	keyUpgradeCmd.SetOut(&out)
	keyUpgradeCmd.SetErr(&errBuf)
	keyUpgradeCmd.SetContext(context.Background())
	if err := keyUpgradeCmd.RunE(keyUpgradeCmd, nil); err != nil {
		t.Fatalf("%v (stderr %q)", err, errBuf.String())
	}
	if len(s.proofs) != 1 || s.proofs[0] != sp.ProofHeader {
		t.Fatalf("want exactly the saved proof resubmitted, got %d submits", len(s.proofs))
	}
	var res x402.UpgradeResult
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Tier != "pro" {
		t.Fatalf("output %q", out.String())
	}
}

// TestPayChallengeRefusesWithPendingProof: the inline rate-limit offer never
// pays while a saved proof is unresolved.
func TestPayChallengeRefusesWithPendingProof(t *testing.T) {
	t.Setenv("BB_STATE_DIR", t.TempDir())
	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = "https://example.invalid", "bb_live_test"
	defer func() { flagHost, flagAPIKey = oldHost, oldKey }()

	store, err := proofStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&x402.SavedProof{ChallengeID: "p", ProofHeader: "h", Txid: "ee", SavedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	err = payChallenge(context.Background(), io.Discard, testWIF, &x402.Challenge{ChallengeID: "new", ExpiresAt: time.Now().Add(time.Minute)})
	if err == nil || !strings.Contains(err.Error(), "still pending") {
		t.Fatalf("got %v", err)
	}
}

func TestRetryAfter(t *testing.T) {
	h := http.Header{}
	if got := retryAfter(h, 3*time.Second); got != 3*time.Second {
		t.Fatalf("default: %s", got)
	}
	h.Set("Retry-After", "4")
	if got := retryAfter(h, time.Second); got != 4*time.Second {
		t.Fatalf("seconds: %s", got)
	}
	h.Set("Retry-After", "9999")
	if got := retryAfter(h, time.Second); got != maxProofRetry {
		t.Fatalf("clamp: %s", got)
	}
	h.Set("Retry-After", "junk")
	if got := retryAfter(h, 2*time.Second); got != 2*time.Second {
		t.Fatalf("junk: %s", got)
	}
}

// TestSettleProof410AfterGraceArchives: once the settlement window has closed
// a 410 is final and the proof leaves the pending set.
func TestSettleProof410AfterGraceArchives(t *testing.T) {
	noSleep(t)
	s := newScriptedProofServer(t, proofAnswer{http.StatusGone, `{"error":"challenge expired; request a new one"}`, ""})
	c, store, sp := savedFixture(t, s.srv.URL)
	sp.ExpiresAt = time.Now().Add(-25 * time.Hour)
	if err := store.Save(sp); err != nil {
		t.Fatal(err)
	}
	_, err := settleProof(context.Background(), c, io.Discard, store, sp, time.Hour)
	if err == nil || !strings.Contains(err.Error(), "window has closed") {
		t.Fatalf("got %v", err)
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Fatal("proof should be archived after the grace window")
	}
	if prev, _ := store.Paid(sp.ChallengeID); prev == nil {
		t.Fatal("an archived challenge must still count as paid")
	}
}

// TestEnsureUnpaidRefusesReusedChallenge is the --new-payment guard: after a
// proof is abandoned, the server re-serving the same open challenge must not
// be paid again.
func TestEnsureUnpaidRefusesReusedChallenge(t *testing.T) {
	_, store, sp := savedFixture(t, "https://example.invalid")
	if err := store.Archive(sp.ChallengeID, sp.Txid, "abandoned"); err != nil {
		t.Fatal(err)
	}
	if left, _ := store.List(); len(left) != 0 {
		t.Fatal("archived proof still pending")
	}
	err := ensureUnpaid(store, &x402.Challenge{ChallengeID: sp.ChallengeID, ExpiresAt: time.Now().Add(time.Minute)})
	if err == nil || !strings.Contains(err.Error(), "never credited") {
		t.Fatalf("got %v", err)
	}
	if ensureUnpaid(store, &x402.Challenge{ChallengeID: "fresh"}) != nil {
		t.Fatal("a fresh challenge must be payable")
	}
}

// TestUpgradeIsExclusive: a second run for the same key cannot start while
// the first holds the store.
func TestUpgradeIsExclusive(t *testing.T) {
	t.Setenv("BB_STATE_DIR", t.TempDir())
	oldHost, oldKey := flagHost, flagAPIKey
	flagHost, flagAPIKey = "https://example.invalid", "bb_live_test"
	defer func() { flagHost, flagAPIKey = oldHost, oldKey }()

	store, err := proofStore()
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.Lock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = payChallenge(context.Background(), io.Discard, testWIF, &x402.Challenge{ChallengeID: "x", ExpiresAt: time.Now().Add(time.Minute)})
	if !errors.Is(err, x402.ErrLocked) {
		t.Fatalf("want ErrLocked, got %v", err)
	}
}
