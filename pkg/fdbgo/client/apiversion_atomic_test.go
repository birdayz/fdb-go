package client

import (
	"context"
	"errors"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
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
	mk := func(apiVersion int) *Transaction {
		return &Transaction{db: &database{apiVersion: apiVersion}, rywDisabled: true}
	}
	lastOp := func(tx *Transaction) MutationType { return tx.mutations[len(tx.mutations)-1].Type }

	// >= 510: Min/And upgrade to V2; other ops untouched.
	tx := mk(730)
	tx.Atomic(MutMin, []byte("k"), []byte{0x0a})
	if got := lastOp(tx); got != MutMinV2 {
		t.Fatalf("Min @730: got op %d, want MutMinV2(%d)", got, MutMinV2)
	}
	tx.Atomic(MutAnd, []byte("k"), []byte{0xff})
	if got := lastOp(tx); got != MutAndV2 {
		t.Fatalf("And @730: got op %d, want MutAndV2(%d)", got, MutAndV2)
	}
	tx.Atomic(MutAddValue, []byte("k"), []byte{0x01})
	if got := lastOp(tx); got != MutAddValue {
		t.Fatalf("Add @730 must NOT upgrade: got op %d, want MutAddValue(%d)", got, MutAddValue)
	}

	// < 510: no upgrade (the C++ gate is apiVersionAtLeast(510) exactly).
	old := mk(500)
	old.Atomic(MutMin, []byte("k"), []byte{0x0a})
	if got := lastOp(old); got != MutMin {
		t.Fatalf("Min @500 must NOT upgrade: got op %d, want MutMin(%d)", got, MutMin)
	}
	old.Atomic(MutAnd, []byte("k"), []byte{0xff})
	if got := lastOp(old); got != MutAnd {
		t.Fatalf("And @500 must NOT upgrade: got op %d, want MutAnd(%d)", got, MutAnd)
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
