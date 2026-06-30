package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"fdb.dev/pkg/fdbgo/wire"
)

func fdbCodeOf(err error) int {
	var fe *wire.FDBError
	if errors.As(err, &fe) {
		return fe.Code
	}
	return -1
}

// TestAtomic_InvalidOpCodePrecedence pins the C++ atomicOp check ORDER
// (ReadYourWrites.actor.cpp:2226-2234): metadataVersionKey (2000) / legal-range (2004) are checked
// BEFORE op-validity (2018). So an invalid op-code on an out-of-legal-range / metadataVersion key
// must report 2004 / 2000, not invalid_mutation_type. (The normal-key → 2018 case is in
// TestAtomic_RejectsNonAtomicOpCode.) Revert-proof: an unconditional 2018 poison fails these.
func TestAtomic_InvalidOpCodePrecedence(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Invalid op (ClearRange) on an out-of-legal-range \xff system key (non-system txn) → 2004.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutClearRange, []byte("\xff\x05"), []byte("\xff\x06"))
		return nil, nil
	})
	if c := fdbCodeOf(err); c != 2004 {
		t.Fatalf("Atomic(invalid op, system key) must be key_outside_legal_range (2004), got %d (%v)", c, err)
	}

	// Invalid op on metadataVersionKey → client_invalid_operation (2000).
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutClearRange, metadataVersionKeyBytes, []byte("x"))
		return nil, nil
	})
	if c := fdbCodeOf(err); c != 2000 {
		t.Fatalf("Atomic(invalid op, metadataVersionKey) must be client_invalid_operation (2000), got %d (%v)", c, err)
	}
}

// TestClear_OversizedSystemKey pins the C++ RYW clear() check order (ReadYourWrites.actor.cpp:
// 2419-2424): legal-range BEFORE the oversized-clear drop. An oversized \xff system key on a
// non-system txn must report key_outside_legal_range (2004), not be silently dropped by the
// size-clamp. An oversized LEGAL (user) key IS dropped (control). Revert-proof: a size-first drop
// silently swallows the system key.
func TestClear_OversizedSystemKey(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// \xff + 31000 bytes: oversized by the SYSTEM key limit (30000) AND out of legal range. Must be
	// > systemKeySizeLimit so the size-clamp WOULD drop it without the legal-first guard.
	bigSysKey := append([]byte("\xff"), make([]byte, 31000)...)
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(bigSysKey)
		return nil, nil
	})
	if c := fdbCodeOf(err); c != 2004 {
		t.Fatalf("Clear(oversized system key) must be key_outside_legal_range (2004), got %d (%v)", c, err)
	}

	// Control: an oversized LEGAL (user) key is silently dropped (C++ size-clamp), so the commit
	// succeeds with no mutation.
	bigUserKey := append([]byte("user"), make([]byte, 11000)...)
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Clear(bigUserKey)
		return nil, nil
	})
	if err != nil {
		t.Fatalf("Clear(oversized LEGAL key) must be silently dropped (no error), got %v", err)
	}
}
