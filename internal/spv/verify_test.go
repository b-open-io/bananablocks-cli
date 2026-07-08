package spv

import (
	"context"
	"errors"
	"testing"
)

type stubVerifier struct {
	out    []ConfirmedRoot
	err    error
	called *int
}

func (s stubVerifier) ConfirmRoots(_ context.Context, _ []RootAtHeight) ([]ConfirmedRoot, error) {
	if s.called != nil {
		*s.called++
	}
	return s.out, s.err
}

// TestFallbackVerifier checks that the composite falls back only on a
// *HeadersServiceError, propagates other errors without downgrading, and skips
// the secondary entirely when the primary succeeds.
func TestFallbackVerifier(t *testing.T) {
	roots := []RootAtHeight{{Root: "aa", Height: 1}}
	confirmed := []ConfirmedRoot{{RootAtHeight: roots[0], State: ConfConfirmed}}

	// Service error → fall back to secondary, OnFallback fires.
	var secondaryCalls int
	fellBack := false
	fv := &FallbackVerifier{
		Primary:    stubVerifier{err: &HeadersServiceError{Err: errors.New("down")}},
		Secondary:  stubVerifier{out: confirmed, called: &secondaryCalls},
		OnFallback: func(error) { fellBack = true },
	}
	got, err := fv.ConfirmRoots(context.Background(), roots)
	if err != nil {
		t.Fatal(err)
	}
	if !fellBack || secondaryCalls != 1 || len(got) != 1 || got[0].State != ConfConfirmed {
		t.Fatalf("service error should fall back to secondary: fellBack=%v calls=%d got=%v", fellBack, secondaryCalls, got)
	}

	// Non-service error → propagate, never downgrade.
	secondaryCalls = 0
	fv = &FallbackVerifier{
		Primary:   stubVerifier{err: errors.New("structural")},
		Secondary: stubVerifier{out: confirmed, called: &secondaryCalls},
	}
	if _, err := fv.ConfirmRoots(context.Background(), roots); err == nil {
		t.Fatal("a non-service error must propagate, not fall back")
	}
	if secondaryCalls != 0 {
		t.Fatalf("a non-service error must not reach the secondary, called %d times", secondaryCalls)
	}

	// Primary success → secondary untouched.
	secondaryCalls = 0
	fv = &FallbackVerifier{
		Primary:   stubVerifier{out: confirmed},
		Secondary: stubVerifier{called: &secondaryCalls},
	}
	got, err = fv.ConfirmRoots(context.Background(), roots)
	if err != nil || len(got) != 1 || secondaryCalls != 0 {
		t.Fatalf("primary success must not call secondary: err=%v got=%v calls=%d", err, got, secondaryCalls)
	}
}
