package client

// Adversarial RYW (read-your-writes) tests.
//
// These tests exercise the client-side RYW cache resolution path by:
// 1. Setting up a base value (committed to FDB)
// 2. Applying an atomic mutation within a new transaction (uncommitted)
// 3. Reading the key back via RYW within the same transaction
// 4. Committing the transaction
// 5. Reading the committed value in a fresh transaction
// 6. Comparing: RYW result MUST equal committed result
//
// Any divergence means the RYW resolution doesn't match what FDB actually does.
// This is the #1 gap in our test suite: existing atomic tests only verify the
// committed path, not the client-side merge path.

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

// le64v encodes a uint64 as little-endian bytes.
func le64v(v uint64) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint64(b, v)
	return b
}

// rywAtomicTest is a table-driven test case for RYW atomic verification.
type rywAtomicTest struct {
	name      string
	op        MutationType
	baseValue []byte // nil = absent key
	param     []byte
}

// runRYWAtomicTest runs a single RYW atomic verification test.
func runRYWAtomicTest(t *testing.T, db *Database, tt rywAtomicTest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	key := []byte(fmt.Sprintf("ryw_atomic_%s", tt.name))

	// Step 1: Set up base value (or ensure absent).
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		if tt.baseValue == nil {
			tx.Clear(key)
		} else {
			tx.Set(key, tt.baseValue)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Step 2: Apply atomic + read via RYW in one transaction.
	rywResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Atomic(tt.op, key, tt.param)
		val, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil
		}
		cp := make([]byte, len(val))
		copy(cp, val)
		return cp, nil
	})
	if err != nil {
		t.Fatalf("atomic+get: %v", err)
	}

	// Step 3: Read committed value in fresh transaction.
	committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		val, err := tx.Get(ctx, key)
		if err != nil {
			return nil, err
		}
		if val == nil {
			return nil, nil
		}
		cp := make([]byte, len(val))
		copy(cp, val)
		return cp, nil
	})
	if err != nil {
		t.Fatalf("read committed: %v", err)
	}

	// Step 4: Compare.
	var ryw, committed []byte
	if rywResult != nil {
		ryw = rywResult.([]byte)
	}
	if committedResult != nil {
		committed = committedResult.([]byte)
	}
	if !bytes.Equal(ryw, committed) {
		t.Fatalf("RYW/committed DIVERGENCE!\n  base=%v\n  op=%d param=%v\n  ryw=%v\n  committed=%v",
			tt.baseValue, tt.op, tt.param, ryw, committed)
	}
}

// TestRYWAtomic_AllTypes runs all atomic mutation types through the RYW path
// and verifies against committed values. This is the main adversarial test.
func TestRYWAtomic_AllTypes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	tests := []rywAtomicTest{
		// --- Add ---
		{"Add_absent", MutAddValue, nil, le64v(42)},
		{"Add_existing", MutAddValue, le64v(100), le64v(42)},
		{"Add_overflow", MutAddValue, le64v(^uint64(0)), le64v(1)},

		// --- And ---
		{"And_absent", MutAnd, nil, []byte{0xFF, 0xFF}},
		{"And_existing", MutAnd, []byte{0xF0, 0x0F}, []byte{0xFF, 0x00}},

		// --- AndV2 ---
		{"AndV2_absent", MutAndV2, nil, []byte{0xFF, 0xFF}},
		{"AndV2_existing", MutAndV2, []byte{0xF0, 0x0F}, []byte{0xFF, 0x00}},

		// --- Or ---
		{"Or_absent", MutOr, nil, []byte{0x0F, 0xF0}},
		{"Or_existing", MutOr, []byte{0xF0, 0x0F}, []byte{0x0F, 0xF0}},

		// --- Xor ---
		{"Xor_absent", MutXor, nil, []byte{0xFF, 0xFF}},
		{"Xor_existing", MutXor, []byte{0xAA, 0x55}, []byte{0xFF, 0xFF}},

		// --- Max (little-endian unsigned) ---
		{"Max_absent", MutMax, nil, le64v(42)},
		{"Max_existing_param_bigger", MutMax, le64v(10), le64v(42)},
		{"Max_existing_base_bigger", MutMax, le64v(42), le64v(10)},
		{"Max_equal", MutMax, le64v(42), le64v(42)},

		// --- Min (little-endian unsigned) ---
		{"Min_absent", MutMin, nil, le64v(42)},
		{"Min_existing_param_smaller", MutMin, le64v(42), le64v(10)},
		{"Min_existing_base_smaller", MutMin, le64v(10), le64v(42)},
		{"Min_equal", MutMin, le64v(42), le64v(42)},

		// --- MinV2 ---
		{"MinV2_absent", MutMinV2, nil, le64v(42)},
		{"MinV2_existing", MutMinV2, le64v(42), le64v(10)},

		// --- ByteMax (lexicographic) ---
		{"ByteMax_absent", MutByteMax, nil, []byte{0x01, 0x02}},
		{"ByteMax_param_bigger", MutByteMax, []byte{0x01}, []byte{0x02}},
		{"ByteMax_base_bigger", MutByteMax, []byte{0x02}, []byte{0x01}},
		{"ByteMax_mixed_len", MutByteMax, []byte{0x01}, []byte{0x01, 0x00}},

		// --- ByteMin (lexicographic) ---
		{"ByteMin_absent", MutByteMin, nil, []byte{0x01, 0x02}},
		{"ByteMin_param_smaller", MutByteMin, []byte{0x02}, []byte{0x01}},
		{"ByteMin_base_smaller", MutByteMin, []byte{0x01}, []byte{0x02}},
		{"ByteMin_mixed_len", MutByteMin, []byte{0x01, 0x00}, []byte{0x01}},

		// --- AppendIfFits ---
		{"AppendIfFits_absent", MutAppendIfFits, nil, []byte("hello")},
		{"AppendIfFits_existing", MutAppendIfFits, []byte("hello"), []byte(" world")},
		{"AppendIfFits_empty_param", MutAppendIfFits, []byte("hello"), []byte{}},
		{"AppendIfFits_empty_base", MutAppendIfFits, []byte{}, []byte("hello")},

		// --- CompareAndClear ---
		{"CompareAndClear_match", MutCompareAndClear, []byte("hello"), []byte("hello")},
		{"CompareAndClear_mismatch", MutCompareAndClear, []byte("hello"), []byte("world")},
		{"CompareAndClear_absent", MutCompareAndClear, nil, []byte("hello")},

		// --- Edge cases: empty values ---
		{"Add_empty_param", MutAddValue, le64v(42), []byte{}},
		{"Or_empty_base", MutOr, []byte{}, []byte{0xFF}},
		{"And_empty_base", MutAnd, []byte{}, []byte{0xFF}},
		{"ByteMax_empty_base", MutByteMax, []byte{}, []byte{0x01}},
		{"ByteMin_empty_base", MutByteMin, []byte{}, []byte{0x01}},

		// --- Mismatched lengths ---
		{"Add_short_base", MutAddValue, []byte{1, 2}, le64v(42)},
		{"And_long_base", MutAnd, le64v(0xFFFFFFFFFFFFFFFF), []byte{0x0F}},
		{"Max_different_lengths", MutMax, []byte{0xFF}, le64v(42)},
		{"Min_different_lengths", MutMin, le64v(42), []byte{0xFF}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			runRYWAtomicTest(t, db, tt)
		})
	}
}

// TestRYWAtomic_ChainedOps tests multiple atomic operations on the same key
// within one transaction, verifying the RYW chain resolution matches committed.
func TestRYWAtomic_ChainedOps(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	type chainOp struct {
		op    MutationType
		param []byte
	}

	tests := []struct {
		name      string
		baseValue []byte
		ops       []chainOp
	}{
		{
			name:      "Add_then_Add",
			baseValue: le64v(100),
			ops:       []chainOp{{MutAddValue, le64v(10)}, {MutAddValue, le64v(20)}},
		},
		{
			name:      "Set_then_Add",
			baseValue: nil,
			ops:       []chainOp{{MutSetValue, le64v(50)}, {MutAddValue, le64v(10)}},
		},
		{
			name:      "Add_then_CompareAndClear_match",
			baseValue: le64v(100),
			ops:       []chainOp{{MutAddValue, le64v(0)}, {MutCompareAndClear, le64v(100)}},
		},
		{
			name:      "Or_then_And",
			baseValue: []byte{0xF0, 0x0F},
			ops:       []chainOp{{MutOr, []byte{0x0F, 0x00}}, {MutAnd, []byte{0xFF, 0x00}}},
		},
		{
			name:      "AppendIfFits_chain",
			baseValue: []byte("a"),
			ops:       []chainOp{{MutAppendIfFits, []byte("b")}, {MutAppendIfFits, []byte("c")}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			key := []byte(fmt.Sprintf("ryw_chain_%s", tt.name))

			// Setup base.
			_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				if tt.baseValue == nil {
					tx.Clear(key)
				} else {
					tx.Set(key, tt.baseValue)
				}
				return nil, nil
			})
			if err != nil {
				t.Fatalf("setup: %v", err)
			}

			// Apply chain + read via RYW.
			rywResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				for _, op := range tt.ops {
					if op.op == MutSetValue {
						// Atomic(MutSetValue) is rejected as invalid_mutation_type (C++ atomicOp,
						// ReadYourWrites.actor.cpp:2234 — SetValue is not in ATOMIC_MASK). A real
						// client models a chained "set" via Set(); the RYW fold is identical
						// (rywCache.set stores a resolved entry, the next atomic folds over it at
						// "Site B"), so Set(50)+Add(10) still resolves to 60.
						tx.Set(key, op.param)
						continue
					}
					tx.Atomic(op.op, key, op.param)
				}
				val, err := tx.Get(ctx, key)
				if err != nil {
					return nil, err
				}
				if val == nil {
					return nil, nil
				}
				cp := make([]byte, len(val))
				copy(cp, val)
				return cp, nil
			})
			if err != nil {
				t.Fatalf("chain+get: %v", err)
			}

			// Read committed.
			committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
				val, err := tx.Get(ctx, key)
				if err != nil {
					return nil, err
				}
				if val == nil {
					return nil, nil
				}
				cp := make([]byte, len(val))
				copy(cp, val)
				return cp, nil
			})
			if err != nil {
				t.Fatalf("read committed: %v", err)
			}

			var ryw, committed []byte
			if rywResult != nil {
				ryw = rywResult.([]byte)
			}
			if committedResult != nil {
				committed = committedResult.([]byte)
			}
			if !bytes.Equal(ryw, committed) {
				t.Fatalf("RYW/committed DIVERGENCE!\n  ryw=%v\n  committed=%v", ryw, committed)
			}
		})
	}
}

// TestRYWClearRange_ThenGet verifies reading a key that was just ClearRange'd.
func TestRYWClearRange_ThenGet(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	t.Run("cleared_keys_absent", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pfx := "ryw_clr_get_"
		keys := []string{pfx + "a", pfx + "b", pfx + "c", pfx + "d"}

		// Set up base values.
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			for _, k := range keys {
				tx.Set([]byte(k), []byte("val_"+k))
			}
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}

		// ClearRange [b, d), then read all keys via RYW.
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.ClearRange([]byte(pfx+"b"), []byte(pfx+"d"))
			vals := make(map[string][]byte)
			for _, k := range keys {
				v, err := tx.Get(ctx, []byte(k))
				if err != nil {
					return nil, err
				}
				if v != nil {
					vals[k] = append([]byte(nil), v...)
				}
			}
			return vals, nil
		})
		if err != nil {
			t.Fatalf("transaction: %v", err)
		}

		// Read committed.
		committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			vals := make(map[string][]byte)
			for _, k := range keys {
				v, err := tx.Get(ctx, []byte(k))
				if err != nil {
					return nil, err
				}
				if v != nil {
					vals[k] = append([]byte(nil), v...)
				}
			}
			return vals, nil
		})
		if err != nil {
			t.Fatalf("read committed: %v", err)
		}

		ryw := result.(map[string][]byte)
		committed := committedResult.(map[string][]byte)

		for _, k := range keys {
			rywV, rywOk := ryw[k]
			commitV, commitOk := committed[k]
			if rywOk != commitOk || !bytes.Equal(rywV, commitV) {
				t.Errorf("key %s: ryw(%v, exists=%v) != committed(%v, exists=%v)",
					k, rywV, rywOk, commitV, commitOk)
			}
		}
	})
}

// TestRYWClearRange_ThenSet_ThenGet verifies ClearRange, then Set within the
// cleared range, then Get. The Set should shadow the clear.
func TestRYWClearRange_ThenSet_ThenGet(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	t.Run("set_shadows_clear", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pfx := "ryw_clrset_"
		keyB := []byte(pfx + "b")

		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(keyB, []byte("original"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}

		rywResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.ClearRange([]byte(pfx+"a"), []byte(pfx+"z"))
			tx.Set(keyB, []byte("resurrected"))
			val, err := tx.Get(ctx, keyB)
			if err != nil {
				return nil, err
			}
			if val == nil {
				return nil, nil
			}
			return append([]byte(nil), val...), nil
		})
		if err != nil {
			t.Fatalf("transaction: %v", err)
		}

		committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			val, err := tx.Get(ctx, keyB)
			if err != nil {
				return nil, err
			}
			if val == nil {
				return nil, nil
			}
			return append([]byte(nil), val...), nil
		})
		if err != nil {
			t.Fatalf("read committed: %v", err)
		}

		var ryw, committed []byte
		if rywResult != nil {
			ryw = rywResult.([]byte)
		}
		if committedResult != nil {
			committed = committedResult.([]byte)
		}
		if !bytes.Equal(ryw, committed) {
			t.Fatalf("RYW/committed divergence: ryw=%q committed=%q", ryw, committed)
		}
		if string(committed) != "resurrected" {
			t.Fatalf("expected 'resurrected', got %q", committed)
		}
	})
}

// TestRYWGetRange_WithLocalWrites verifies getRange merges local writes correctly.
func TestRYWGetRange_WithLocalWrites(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	t.Run("merge_writes_forward_and_reverse", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pfx := "ryw_range_writes_"

		// Set up base: a=1, c=3, e=5
		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte(pfx+"a"), []byte("1"))
			tx.Set([]byte(pfx+"c"), []byte("3"))
			tx.Set([]byte(pfx+"e"), []byte("5"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}

		// Set b=2, Clear c, Set d=4, then getRange forward and reverse.
		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte(pfx+"b"), []byte("2"))
			tx.Clear([]byte(pfx + "c"))
			tx.Set([]byte(pfx+"d"), []byte("4"))

			fwdKVs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"f"), 0)
			if err != nil {
				return nil, fmt.Errorf("forward: %w", err)
			}
			revKVs, _, err := tx.GetRangeReverse(ctx, []byte(pfx+"a"), []byte(pfx+"f"), 0)
			if err != nil {
				return nil, fmt.Errorf("reverse: %w", err)
			}
			return [][]KeyValue{fwdKVs, revKVs}, nil
		})
		if err != nil {
			t.Fatalf("transaction: %v", err)
		}

		// Read committed.
		committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			fwdKVs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"f"), 0)
			if err != nil {
				return nil, fmt.Errorf("forward: %w", err)
			}
			revKVs, _, err := tx.GetRangeReverse(ctx, []byte(pfx+"a"), []byte(pfx+"f"), 0)
			if err != nil {
				return nil, fmt.Errorf("reverse: %w", err)
			}
			return [][]KeyValue{fwdKVs, revKVs}, nil
		})
		if err != nil {
			t.Fatalf("committed read: %v", err)
		}

		rywBoth := result.([][]KeyValue)
		commitBoth := committedResult.([][]KeyValue)

		// Compare forward.
		rywFwd := rywBoth[0]
		commitFwd := commitBoth[0]
		if len(rywFwd) != len(commitFwd) {
			t.Fatalf("forward length: ryw=%d committed=%d", len(rywFwd), len(commitFwd))
		}
		for i := range rywFwd {
			if !bytes.Equal(rywFwd[i].Key, commitFwd[i].Key) || !bytes.Equal(rywFwd[i].Value, commitFwd[i].Value) {
				t.Errorf("forward[%d]: ryw=(%q,%q) committed=(%q,%q)",
					i, rywFwd[i].Key, rywFwd[i].Value, commitFwd[i].Key, commitFwd[i].Value)
			}
		}

		// Compare reverse.
		rywRev := rywBoth[1]
		commitRev := commitBoth[1]
		if len(rywRev) != len(commitRev) {
			t.Fatalf("reverse length: ryw=%d committed=%d", len(rywRev), len(commitRev))
		}
		for i := range rywRev {
			if !bytes.Equal(rywRev[i].Key, commitRev[i].Key) || !bytes.Equal(rywRev[i].Value, commitRev[i].Value) {
				t.Errorf("reverse[%d]: ryw=(%q,%q) committed=(%q,%q)",
					i, rywRev[i].Key, rywRev[i].Value, commitRev[i].Key, commitRev[i].Value)
			}
		}

		// Verify forward has expected keys: a, b, d, e (c was cleared).
		expectedKeys := []string{pfx + "a", pfx + "b", pfx + "d", pfx + "e"}
		if len(rywFwd) != 4 {
			t.Fatalf("expected 4 keys, got %d", len(rywFwd))
		}
		for i, ek := range expectedKeys {
			if string(rywFwd[i].Key) != ek {
				t.Errorf("forward[%d]: got key %q, want %q", i, rywFwd[i].Key, ek)
			}
		}
	})
}

// TestRYWGetRange_AtomicInRange verifies getRange with an atomic mutation
// on a key within the range.
func TestRYWGetRange_AtomicInRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	t.Run("add_in_range", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pfx := "ryw_range_atomic_"

		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte(pfx+"a"), le64v(10))
			tx.Set([]byte(pfx+"b"), le64v(20))
			tx.Set([]byte(pfx+"c"), le64v(30))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}

		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Atomic(MutAddValue, []byte(pfx+"b"), le64v(5))
			kvs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"d"), 0)
			if err != nil {
				return nil, err
			}
			cp := make([]KeyValue, len(kvs))
			for i, kv := range kvs {
				cp[i] = KeyValue{Key: append([]byte(nil), kv.Key...), Value: append([]byte(nil), kv.Value...)}
			}
			return cp, nil
		})
		if err != nil {
			t.Fatalf("transaction: %v", err)
		}

		committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"d"), 0)
			if err != nil {
				return nil, err
			}
			cp := make([]KeyValue, len(kvs))
			for i, kv := range kvs {
				cp[i] = KeyValue{Key: append([]byte(nil), kv.Key...), Value: append([]byte(nil), kv.Value...)}
			}
			return cp, nil
		})
		if err != nil {
			t.Fatalf("committed: %v", err)
		}

		rywKVs := result.([]KeyValue)
		commitKVs := committedResult.([]KeyValue)

		if len(rywKVs) != len(commitKVs) {
			t.Fatalf("length: ryw=%d committed=%d", len(rywKVs), len(commitKVs))
		}
		for i := range rywKVs {
			if !bytes.Equal(rywKVs[i].Key, commitKVs[i].Key) || !bytes.Equal(rywKVs[i].Value, commitKVs[i].Value) {
				t.Errorf("[%d]: ryw=(%q,%v) committed=(%q,%v)",
					i, rywKVs[i].Key, rywKVs[i].Value, commitKVs[i].Key, commitKVs[i].Value)
			}
		}
	})
}

// TestRYWGetRange_NewKeyInRange verifies getRange includes a newly Set key
// interleaved with existing server keys.
func TestRYWGetRange_NewKeyInRange(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	t.Run("interleaved_new_key", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		pfx := "ryw_range_newkey_"

		_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte(pfx+"a"), []byte("A"))
			tx.Set([]byte(pfx+"c"), []byte("C"))
			return nil, nil
		})
		if err != nil {
			t.Fatalf("setup: %v", err)
		}

		result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set([]byte(pfx+"b"), []byte("B"))
			kvs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"d"), 0)
			if err != nil {
				return nil, err
			}
			var keys []string
			for _, kv := range kvs {
				keys = append(keys, string(kv.Key))
			}
			return keys, nil
		})
		if err != nil {
			t.Fatalf("transaction: %v", err)
		}

		committedResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"d"), 0)
			if err != nil {
				return nil, err
			}
			var keys []string
			for _, kv := range kvs {
				keys = append(keys, string(kv.Key))
			}
			return keys, nil
		})
		if err != nil {
			t.Fatalf("committed: %v", err)
		}

		rywKeys := result.([]string)
		commitKeys := committedResult.([]string)

		if len(rywKeys) != len(commitKeys) {
			t.Fatalf("key count: ryw=%v committed=%v", rywKeys, commitKeys)
		}
		for i := range rywKeys {
			if rywKeys[i] != commitKeys[i] {
				t.Errorf("[%d]: ryw=%q committed=%q", i, rywKeys[i], commitKeys[i])
			}
		}
	})
}
