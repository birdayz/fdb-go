package client

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/pkg/fdbgo/wire"
)

// TestWatchSetup_RejectsSystemAndOversizedKeys pins the eager legal-range + key-size validation
// C++ RYW watch performs before registering (ReadYourWrites.actor.cpp:2450-2456). A normal
// (non-system) transaction must not be able to register a watch on a \xff system key
// (key_outside_legal_range 2004) or an oversized key (key_too_large 2102) — libfdb_c rejects both.
// The checks run BEFORE the read version, so no FDB container is needed. Revert-proof: removing the
// checks lets WatchSetup proceed past them.
func TestWatchSetup_RejectsSystemAndOversizedKeys(t *testing.T) {
	t.Parallel()

	t.Run("system_key_2004", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		_, _, _, _, err := tx.WatchSetup(context.Background(), []byte("\xff\x05"))
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != 2004 {
			t.Fatalf("Watch on a \\xff system key must be key_outside_legal_range (2004), got %v", err)
		}
	})

	t.Run("oversized_key_2102", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.tenantId = NoTenantID
		big := make([]byte, 20000) // > KEY_SIZE_LIMIT (10000); all-zero so it passes the legal-range gate
		_, _, _, _, err := tx.WatchSetup(context.Background(), big)
		var fe *wire.FDBError
		if !errors.As(err, &fe) || fe.Code != 2102 {
			t.Fatalf("Watch on an oversized key must be key_too_large (2102), got %v", err)
		}
	})
}
