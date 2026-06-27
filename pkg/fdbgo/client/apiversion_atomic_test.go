package client

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestAtomic_MinAndV2UpgradeGate pins the Min→MinV2 / And→AndV2 op-code upgrade
// in client.Transaction.Atomic (RFC-149, C++ RYW::atomicOp). At API version
// >=510 the legacy codes upgrade to V2 (which fold correctly on an absent key);
// below 510 they are left as-is. This is the closure proof for the binding
// tester: cmd/fdb-stacktester maps MIN/AND to MutMin/MutAnd and calls this exact
// Atomic, so with its API version (730) wired in it now emits MinV2(18)/AndV2(19)
// — matching libfdb_c — rather than the legacy Min(13)/And(6).
func TestAtomic_MinAndV2UpgradeGate(t *testing.T) {
	t.Parallel()
	lastOp := func(tx *Transaction) MutationType { return tx.mutations[len(tx.mutations)-1].Type }

	// The gate is apiVersionAtLeast(510) — STRICTLY >= 510, exactly as C++. Pin
	// the BOUNDARY (509 no-upgrade, 510 upgrade), not just far-apart values: a
	// wrong gate of e.g. 600 would silently ship legacy Min(13)/And(6) to apps at
	// API 510-599 — the exact wire divergence this RFC closes.
	for _, tc := range []struct {
		apiVersion  int
		wantUpgrade bool
	}{
		{500, false}, {509, false}, {510, true}, {730, true},
	} {
		tc := tc
		t.Run(fmt.Sprintf("api%d", tc.apiVersion), func(t *testing.T) {
			t.Parallel()
			tx := &Transaction{db: &database{apiVersion: tc.apiVersion}, rywDisabled: true}

			wantMin, wantAnd := MutMin, MutAnd
			if tc.wantUpgrade {
				wantMin, wantAnd = MutMinV2, MutAndV2
			}
			tx.Atomic(MutMin, []byte("k"), []byte{0x0a})
			if got := lastOp(tx); got != wantMin {
				t.Fatalf("Min @%d: got op %d, want %d", tc.apiVersion, got, wantMin)
			}
			tx.Atomic(MutAnd, []byte("k"), []byte{0xff})
			if got := lastOp(tx); got != wantAnd {
				t.Fatalf("And @%d: got op %d, want %d", tc.apiVersion, got, wantAnd)
			}
			// Only Min/And are upgraded — Add never is, at any version.
			tx.Atomic(MutAddValue, []byte("k"), []byte{0x01})
			if got := lastOp(tx); got != MutAddValue {
				t.Fatalf("Add @%d must NOT upgrade: got op %d, want MutAddValue(%d)", tc.apiVersion, got, MutAddValue)
			}
		})
	}
}

// TestOpenDatabase_RequiresAPIVersion pins the mandatory-set semantics (RFC-149
// 3b, mirroring fdb_select_api_version): opening a client database WITHOUT an
// API version is rejected, so the apiVersionAtLeast(510) gate can never silently
// no-op. Returns before any bootstrap, so no FDB is needed.
func TestOpenDatabase_RequiresAPIVersion(t *testing.T) {
	t.Parallel()
	cf := &ClusterFile{}
	_, err := OpenDatabaseFromConfig(context.Background(), cf)
	if err == nil {
		t.Fatal("OpenDatabaseFromConfig without WithAPIVersion must error (api_version_unset)")
	}
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != 2200 {
		t.Fatalf("err = %v (%T), want *wire.FDBError{Code: 2200}", err, err)
	}
}
