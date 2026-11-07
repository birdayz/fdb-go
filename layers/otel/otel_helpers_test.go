package otel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
)

func freshRun(t *testing.T) string {
	t.Helper()
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	return "test_" + hex.EncodeToString(nonce[:])
}

func requireOtelKind(t *testing.T, err error, kind string) {
	t.Helper()
	var classified *OtelError
	if !errors.As(err, &classified) || classified.Kind != kind {
		t.Fatalf("want %s error, got %T: %v", kind, err, err)
	}
}

func requireOtelBudget(t *testing.T, err error) {
	t.Helper()
	var budget *QueryBudgetError
	if !errors.As(err, &budget) {
		t.Fatalf("want QueryBudgetError, got %T: %v", err, err)
	}
}

func testConfig() Config {
	c := DefaultConfig()
	c.Run = "unit_fixture_283"
	c.Count = 48
	c.Tenants = 2
	c.Attributes = 8
	c.MatchEvery = 3
	c.K = 100
	c.MaxCandidates = 1000
	return c
}

func collision(target string) string {
	wanted := sha256.Sum256([]byte(target))
	for i := 0; ; i++ {
		s := fmt.Sprintf("collision_%d", i)
		h := sha256.Sum256([]byte(s))
		if s != target && h[0] == wanted[0] {
			return s
		}
	}
}
