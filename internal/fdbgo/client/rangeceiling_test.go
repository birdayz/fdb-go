package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// TestRangeByteCeiling verifies the opt-in WithRangeByteCeiling OOM safety valve
// (RFC-115 §2): a GetRange that materializes more than the configured ceiling fails
// with *RangeMaterializationLimitError instead of accumulating unbounded, while the
// default (no ceiling) stays oracle-matching and returns the full range.
//
// Revert-proof: delete the ceiling check in getRangeImpl and the "exceeds cap" arm
// goes green-with-full-results (no error), reddening this test.
func TestRangeByteCeiling(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	if sharedClusterFile == nil {
		t.Fatal("shared FDB container not initialized — TestMain must run first")
	}

	// Seed ~200 KB under a test-unique prefix: 20 keys × 10 KB values.
	const (
		numKeys  = 20
		valBytes = 10 * 1024
		totalSz  = numKeys * valBytes // ~200 KB
	)
	prefix := []byte(t.Name() + "_")
	rangeEnd := append(append([]byte{}, prefix...), 0xff)

	seedDB := openTestDB(t, ctx)
	defer seedDB.Close()
	val := bytes.Repeat([]byte("x"), valBytes)
	if _, err := seedDB.Transact(ctx, func(tx *Transaction) (any, error) {
		for i := 0; i < numKeys; i++ {
			key := append(append([]byte{}, prefix...), []byte(fmt.Sprintf("%04d", i))...)
			tx.Set(key, val)
		}
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// ceiling well below the total so it trips, but above one wire reply (~80 KB)
	// is NOT required — the check fires after the first batch that pushes the
	// running total over the cap.
	const ceiling = 50 * 1024

	t.Run("exceeds_cap_errors", func(t *testing.T) {
		db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithRangeByteCeiling(ceiling), WithAPIVersion(730))
		if err != nil {
			t.Fatalf("open with ceiling: %v", err)
		}
		defer db.Close()

		_, err = db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, _, gerr := tx.GetRange(ctx, prefix, rangeEnd, 0) // 0 = unlimited
			return kvs, gerr
		})
		if err == nil {
			t.Fatal("expected RangeMaterializationLimitError, got nil (ceiling not enforced)")
		}
		var limitErr *RangeMaterializationLimitError
		if !errors.As(err, &limitErr) {
			t.Fatalf("expected *RangeMaterializationLimitError, got %T: %v", err, err)
		}
		if limitErr.LimitBytes != ceiling {
			t.Errorf("LimitBytes = %d, want %d", limitErr.LimitBytes, ceiling)
		}
		if limitErr.ReachedBytes <= ceiling {
			t.Errorf("ReachedBytes = %d, want > ceiling %d", limitErr.ReachedBytes, ceiling)
		}
		t.Logf("ceiling enforced: %v", err)
	})

	t.Run("unset_cap_is_unbounded", func(t *testing.T) {
		// No ceiling (default) — oracle-matching: the full range materializes.
		db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithAPIVersion(730))
		if err != nil {
			t.Fatalf("open without ceiling: %v", err)
		}
		defer db.Close()

		res, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, _, gerr := tx.GetRange(ctx, prefix, rangeEnd, 0)
			return kvs, gerr
		})
		if err != nil {
			t.Fatalf("unbounded GetRange should succeed without a ceiling, got: %v", err)
		}
		kvs := res.([]KeyValue)
		if len(kvs) != numKeys {
			t.Fatalf("got %d keys, want %d (full range)", len(kvs), numKeys)
		}
	})

	// A ceiling that exceeds the whole result must NOT trip (boundary: the cap is a
	// ceiling, not a per-batch limit).
	t.Run("cap_above_total_does_not_trip", func(t *testing.T) {
		db, err := OpenDatabaseFromConfig(ctx, sharedClusterFile, WithRangeByteCeiling(totalSz*4), WithAPIVersion(730))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer db.Close()
		res, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			kvs, _, gerr := tx.GetRange(ctx, prefix, rangeEnd, 0)
			return kvs, gerr
		})
		if err != nil {
			t.Fatalf("cap above total should not trip, got: %v", err)
		}
		if kvs := res.([]KeyValue); len(kvs) != numKeys {
			t.Fatalf("got %d keys, want %d", len(kvs), numKeys)
		}
	})
}
