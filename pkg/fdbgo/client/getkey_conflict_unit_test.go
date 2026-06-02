package client

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/wire"
)

// TestCommit_RYWPoisonBeatsTimeout pins codex's RFC-059 precedence point: a transaction
// poisoned by SetReadYourWritesDisable-after-an-op AND past its timeout must report
// client_invalid_operation (2000), NOT transaction_timed_out (1031). Reads check the poison
// before the timeout, and libfdb_c's checkDeferredError runs before any commit logic, so the
// Commit gate must out-rank checkTimeout. White-box: the poison check returns before any DB
// interaction, so no container is needed and it is deterministic (deadline set in the past, no
// sleep).
func TestCommit_RYWPoisonBeatsTimeout(t *testing.T) {
	t.Parallel()
	tx := &Transaction{}
	tx.state.Store(int32(txStateActive))
	tx.timeout = time.Millisecond
	tx.deadline = time.Now().Add(-time.Hour) // already elapsed → checkTimeout would return 1031
	tx.rywPoisonErr = &wire.FDBError{Code: 2000}

	err := tx.Commit(context.Background())
	var fe *wire.FDBError
	if !errors.As(err, &fe) || fe.Code != 2000 {
		t.Fatalf("poisoned + timed-out Commit: want client_invalid_operation (2000), got %v", err)
	}
}

// TestAddGetKeyConflictRange_RYWDisabledFullSpan pins codex's RFC-058 P2-2: when RYW is
// disabled, GetKey resolves against STORAGE only (Transaction.GetKey uses tx.getKey, not
// getKeyRYW), so the local write-map did NOT satisfy the read — the read-conflict must be the
// FULL base↔resolved span, NOT filtered through the bypassed write-map. Filtering would
// subtract a local Set/Clear segment and MISS a conflict from a concurrent insert into that
// gap (an unsafe under-conflict).
//
// White-box because this is not differential-testable: libfdb_c rejects reading a range that
// overlaps a locally-written key under RYW-disabled with client_invalid_operation (2000), so
// the under-conflict is unreachable via the legal API. (That go does not itself raise 2000 is
// a separate option-semantics divergence tracked under TODO item 3.)
func TestAddGetKeyConflictRange_RYWDisabledFullSpan(t *testing.T) {
	t.Parallel()

	// getKey(firstGreaterThan("a")) resolved to "z" → span [keyAfter("a"), keyAfter("z")) =
	// ["a\x00", "z\x00"). A local INDEPENDENT write sits at "c", inside that span.
	wantBegin := keyAfterBytes([]byte("a"))
	wantEnd := keyAfterBytes([]byte("z"))

	// RYW DISABLED → full span, "c" NOT subtracted.
	txDisabled := &Transaction{rywDisabled: true}
	txDisabled.ryw.set([]byte("c"), []byte("v"))
	txDisabled.addGetKeyConflictRange([]byte("a"), true, 1, []byte("z"))
	if len(txDisabled.readConflicts) != 1 {
		t.Fatalf("rywDisabled: want 1 full-span conflict, got %d: %v", len(txDisabled.readConflicts), txDisabled.readConflicts)
	}
	if !bytes.Equal(txDisabled.readConflicts[0].Begin, wantBegin) || !bytes.Equal(txDisabled.readConflicts[0].End, wantEnd) {
		t.Fatalf("rywDisabled span: got [%q,%q) want [%q,%q)",
			txDisabled.readConflicts[0].Begin, txDisabled.readConflicts[0].End, wantBegin, wantEnd)
	}

	// CONTRAST — RYW ENABLED → the same independent write at "c" IS filtered out
	// (updateConflictMap subtracts INDEPENDENT writes), so no conflict range covers "c".
	txEnabled := &Transaction{}
	txEnabled.ryw.set([]byte("c"), []byte("v"))
	txEnabled.addGetKeyConflictRange([]byte("a"), true, 1, []byte("z"))
	if len(txEnabled.readConflicts) == 0 {
		t.Fatalf("RYW-enabled: expected unmodified-gap conflicts, got none")
	}
	for _, r := range txEnabled.readConflicts {
		if bytes.Compare(r.Begin, []byte("c")) <= 0 && bytes.Compare([]byte("c"), r.End) < 0 {
			t.Fatalf("RYW-enabled: independent write \"c\" must be filtered out of the conflict, "+
				"but it is covered by [%q,%q)", r.Begin, r.End)
		}
	}
}
