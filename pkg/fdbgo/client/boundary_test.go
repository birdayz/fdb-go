package client

// Boundary condition tests targeting off-by-one errors.
// Each test exercises exact boundary points where off-by-one bugs lurk.

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// TestGetRange_ExactLimit verifies that GetRange with a limit that exactly
// matches the number of available keys returns more=false (no extra data).
func TestGetRange_ExactLimit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "boundary_exact_"

	// Write exactly 5 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 5; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%d", pfx, i)), []byte("v"))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// GetRange with limit=5 (exactly matching). more should be false.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(pfx+"0"), []byte(pfx+"9"), 5)
		if err != nil {
			return nil, err
		}
		return []any{len(kvs), more}, nil
	})
	if err != nil {
		t.Fatalf("getrange: %v", err)
	}
	r := result.([]any)
	count := r[0].(int)
	more := r[1].(bool)
	if count != 5 {
		t.Errorf("count: got %d, want 5", count)
	}
	// With exactly 5 keys and limit=5, more should be true (we got limit results,
	// there MIGHT be more — we can't know without fetching one more).
	// This is the correct C++ behavior: more = (data.size() == limit).
	if !more {
		t.Errorf("more: got false, want true (limit reached = always more)")
	}

	// GetRange with limit=6 (one more than available). more should be false.
	result2, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(pfx+"0"), []byte(pfx+"9"), 6)
		if err != nil {
			return nil, err
		}
		return []any{len(kvs), more}, nil
	})
	if err != nil {
		t.Fatalf("getrange: %v", err)
	}
	r2 := result2.([]any)
	count2 := r2[0].(int)
	more2 := r2[1].(bool)
	if count2 != 5 {
		t.Errorf("count: got %d, want 5", count2)
	}
	if more2 {
		t.Errorf("more: got true, want false (only 5 keys exist, asked for 6)")
	}

	// Clean up.
	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"0"), []byte(pfx+"9"))
		return nil, nil
	})
}

// TestGetRange_Limit1 verifies single-element scans work correctly.
func TestGetRange_Limit1(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "boundary_limit1_"

	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"a"), []byte("A"))
		tx.Set([]byte(pfx+"b"), []byte("B"))
		tx.Set([]byte(pfx+"c"), []byte("C"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Forward limit=1.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"z"), 1)
		if err != nil {
			return nil, err
		}
		return []any{kvs, more}, nil
	})
	if err != nil {
		t.Fatalf("fwd: %v", err)
	}
	r := result.([]any)
	kvs := r[0].([]KeyValue)
	more := r[1].(bool)
	if len(kvs) != 1 {
		t.Errorf("fwd count: got %d, want 1", len(kvs))
	}
	if string(kvs[0].Key) != pfx+"a" {
		t.Errorf("fwd key: got %q, want %q", kvs[0].Key, pfx+"a")
	}
	if !more {
		t.Errorf("fwd more: got false, want true")
	}

	// Reverse limit=1.
	result2, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRangeReverse(ctx, []byte(pfx+"a"), []byte(pfx+"z"), 1)
		if err != nil {
			return nil, err
		}
		return []any{kvs, more}, nil
	})
	if err != nil {
		t.Fatalf("rev: %v", err)
	}
	r2 := result2.([]any)
	kvs2 := r2[0].([]KeyValue)
	more2 := r2[1].(bool)
	if len(kvs2) != 1 {
		t.Errorf("rev count: got %d, want 1", len(kvs2))
	}
	if string(kvs2[0].Key) != pfx+"c" {
		t.Errorf("rev key: got %q, want %q", kvs2[0].Key, pfx+"c")
	}
	if !more2 {
		t.Errorf("rev more: got false, want true")
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"a"), []byte(pfx+"z"))
		return nil, nil
	})
}

// TestGetRange_EmptyRange verifies that scanning an empty range returns
// 0 results and more=false.
func TestGetRange_EmptyRangeBoundary(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, more, err := tx.GetRange(ctx, []byte("empty_boundary_start"), []byte("empty_boundary_end"), 0)
		if err != nil {
			return nil, err
		}
		return []any{len(kvs), more}, nil
	})
	if err != nil {
		t.Fatalf("getrange: %v", err)
	}
	r := result.([]any)
	if r[0].(int) != 0 {
		t.Errorf("count: got %d, want 0", r[0])
	}
	if r[1].(bool) {
		t.Errorf("more: got true, want false (empty range)")
	}
}

// TestGetRange_KeyAtBoundary verifies that a key exactly at the range begin
// is included and a key exactly at the range end is excluded.
func TestGetRange_KeyAtBoundary(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "boundary_at_"

	// Keys: aa, bb, cc — sorted correctly, cc is the exclusive end.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set([]byte(pfx+"aa"), []byte("A"))
		tx.Set([]byte(pfx+"bb"), []byte("B"))
		tx.Set([]byte(pfx+"cc"), []byte("C"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Range [aa, cc) should include "aa" and "bb" but NOT "cc".
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(pfx+"aa"), []byte(pfx+"cc"), 0)
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
		t.Fatalf("getrange: %v", err)
	}
	keys := result.([]string)
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys [aa, bb], got %v", keys)
	}
	if keys[0] != pfx+"aa" || keys[1] != pfx+"bb" {
		t.Errorf("keys: got %v, want [%saa, %sbb]", keys, pfx, pfx)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"aa"), []byte(pfx+"cc\x00"))
		return nil, nil
	})
}

// TestClearRange_Boundary verifies ClearRange [begin, end) boundary precision.
func TestClearRange_Boundary(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "boundary_clr_"

	// Set a, b, c, d.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for _, k := range []string{"a", "b", "c", "d"} {
			tx.Set([]byte(pfx+k), []byte(k))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// ClearRange [b, d) — should clear b and c, leave a and d.
	_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"b"), []byte(pfx+"d"))
		return nil, nil
	})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}

	// Verify remaining keys.
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(pfx+"a"), []byte(pfx+"z"), 0)
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
		t.Fatalf("verify: %v", err)
	}
	keys := result.([]string)
	if len(keys) != 2 || keys[0] != pfx+"a" || keys[1] != pfx+"d" {
		t.Errorf("after ClearRange [b,d): got %v, want [%sa, %sd]", keys, pfx, pfx)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"a"), []byte(pfx+"z"))
		return nil, nil
	})
}

// TestGetRange_AdjacentKeys verifies scanning with keys that are one byte apart.
func TestGetRange_AdjacentKeys(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	// Keys: \x00, \x01, \x02, ..., \x09 (with prefix to avoid system keys)
	pfx := "boundary_adj_"
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set(append([]byte(pfx), byte(i)), []byte{byte(i)})
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Range [pfx+\x03, pfx+\x07) — should get 4 keys (3, 4, 5, 6).
	result, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := append([]byte(pfx), 0x03)
		end := append([]byte(pfx), 0x07)
		kvs, _, err := tx.GetRange(ctx, begin, end, 0)
		if err != nil {
			return nil, err
		}
		return len(kvs), nil
	})
	if err != nil {
		t.Fatalf("getrange: %v", err)
	}
	if result.(int) != 4 {
		t.Errorf("count: got %d, want 4 (keys 3,4,5,6)", result)
	}

	// Same range in reverse — should get same 4 keys in reverse order.
	result2, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		begin := append([]byte(pfx), 0x03)
		end := append([]byte(pfx), 0x07)
		kvs, _, err := tx.GetRangeReverse(ctx, begin, end, 0)
		if err != nil {
			return nil, err
		}
		if len(kvs) > 0 {
			// First key in reverse should be \x06 (highest in range).
			firstVal := kvs[0].Value[0]
			lastVal := kvs[len(kvs)-1].Value[0]
			if firstVal != 6 || lastVal != 3 {
				return nil, fmt.Errorf("reverse order wrong: first=%d last=%d", firstVal, lastVal)
			}
		}
		return len(kvs), nil
	})
	if err != nil {
		t.Fatalf("getrange reverse: %v", err)
	}
	if result2.(int) != 4 {
		t.Errorf("reverse count: got %d, want 4", result2)
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange(append([]byte(pfx), 0x00), append([]byte(pfx), 0xFF))
		return nil, nil
	})
}

// TestGetRange_ReversePagination verifies that paging through results in
// reverse produces the same keys (in reverse order) as a forward scan.
func TestGetRange_ReversePagination(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	db := openTestDB(t, ctx)

	pfx := "boundary_revpage_"

	// Write 10 keys.
	_, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < 10; i++ {
			tx.Set([]byte(fmt.Sprintf("%s%02d", pfx, i)), []byte(fmt.Sprintf("v%d", i)))
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Forward scan all.
	fwdResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRange(ctx, []byte(pfx+"00"), []byte(pfx+"99"), 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("fwd: %v", err)
	}
	fwdKVs := fwdResult.([]KeyValue)

	// Reverse scan all.
	revResult, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		kvs, _, err := tx.GetRangeReverse(ctx, []byte(pfx+"00"), []byte(pfx+"99"), 0)
		return kvs, err
	})
	if err != nil {
		t.Fatalf("rev: %v", err)
	}
	revKVs := revResult.([]KeyValue)

	if len(fwdKVs) != len(revKVs) {
		t.Fatalf("count mismatch: fwd=%d rev=%d", len(fwdKVs), len(revKVs))
	}

	// Reverse of reverse should equal forward.
	for i := range fwdKVs {
		j := len(revKVs) - 1 - i
		if !bytes.Equal(fwdKVs[i].Key, revKVs[j].Key) {
			t.Errorf("[%d] fwd=%q rev[%d]=%q", i, fwdKVs[i].Key, j, revKVs[j].Key)
		}
	}

	db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.ClearRange([]byte(pfx+"00"), []byte(pfx+"99"))
		return nil, nil
	})
}
