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

// TestAtomic_InvalidOp_DefersToEarlierIllegalMutation pins C++ "the FIRST illegal op throws"
// (ReadYourWrites.actor.cpp:2226-2234, eager). Go defers Set/Atomic validation to Commit, so the
// bad-Atomic poison must defer to an EARLIER illegal buffered mutation (codex delta-review): a
// system-key Set BEFORE an invalid-op Atomic surfaces the Set's key_outside_legal_range (2004), not
// the Atomic's invalid_mutation_type (2018); the reverse order surfaces 2018. Revert-proof: a poison
// that out-ranks the buffered-mutation loop returns 2018 for the first (Set-before-Atomic) case.
func TestAtomic_InvalidOp_DefersToEarlierIllegalMutation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	// Set(systemKey) [defers 2004] THEN Atomic(badOp) → the Set is the first illegal op → 2004.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte("\xff\x05"), []byte("v"))            // out-of-legal-range system key (non-system txn)
		tx.Atomic(MutClearRange, []byte("k"), []byte("v")) // invalid atomic op-code → 2018
		return nil, nil
	})
	if c := fdbCodeOf(err); c != 2004 {
		t.Fatalf("Set(systemKey) BEFORE Atomic(badOp): first illegal op (the Set) wins → 2004, got %d (%v)", c, err)
	}

	// Reverse: Atomic(badOp) FIRST (no preceding mutation) → the Atomic is the first illegal op → 2018.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(MutClearRange, []byte("k"), []byte("v"))
		tx.Set([]byte("\xff\x05"), []byte("v"))
		return nil, nil
	})
	if c := fdbCodeOf(err); c != ErrInvalidMutationType {
		t.Fatalf("Atomic(badOp) BEFORE Set(systemKey): the Atomic is first → 2018, got %d (%v)", c, err)
	}
}

// TestCommit_InvalidAtomicMarksErrored pins that the invalid-atomic poison marks the transaction
// errored on the COMMON path (poison set before Commit entry: Atomic(badOp); Commit()), matching the
// snapshot re-check that also errors it — so the post-failure txn state does not depend on whether
// the bad Atomic landed before commit entry or raced into the re-check (codex). Revert-proof: drop
// the state.Store on the entry check and the txn stays active after the failed commit.
func TestCommit_InvalidAtomicMarksErrored(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)
	defer db.Close()

	tx := db.CreateTransaction()
	tx.Atomic(MutClearRange, []byte("k"), []byte("v")) // invalid op-code → poison, set before Commit entry
	if c := fdbCodeOf(tx.Commit(ctx)); c != ErrInvalidMutationType {
		t.Fatalf("invalid-atomic commit must be invalid_mutation_type (2018), got %d", c)
	}
	if txState(tx.state.Load()) != txStateErrored {
		t.Fatalf("after a failed invalid-atomic commit the txn must be errored (not active), got state %d", tx.state.Load())
	}
}
