package client

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"
	"time"
)

// RFC-098 matrix: reads of pending versionstamped keys per the C++ dispatch
// (ReadYourWrites.actor.cpp:397-406) — regular reads and snapshot reads with
// snapshot-RYW enabled (the default) throw accessed_unreadable (1036);
// RYW-disabled transactions and snapshot-RYW-disabled snapshot reads keep
// storage semantics; BYPASS_UNREADABLE returns the operand as written.

// svvOperand returns a valid SetVersionstampedValue operand: 10 placeholder
// bytes + 4-byte LE offset 0.
func svvOperand() []byte { return make([]byte, 14) }

// svkKey returns a valid SetVersionstampedKey key for prefix: prefix +
// 10-byte placeholder + 4-byte LE offset pointing at the placeholder.
func svkKey(prefix []byte) []byte {
	key := append(append([]byte(nil), prefix...), make([]byte, 14)...)
	binary.LittleEndian.PutUint32(key[len(key)-4:], uint32(len(prefix)))
	return key
}

// TestRYW_UnreadableCapScanNotQuadratic pins the unreadable-cap scan cost on
// the getRange fast path. unreadableScanCapLocked runs on EVERY getRange; its
// first version called ensureSortedLocked, which rebuilds sortedKeys O(N log N)
// after every write invalidation — interleaved set/getRange transactions went
// quadratic and the recordlayer suite timed out at its 900s budget. The scan
// now uses the dedicated unreadableKeys index (and short-circuits when there is
// no unreadable state), so this interleaved loop is O(N log N) total. The 30s
// bound has ~100x headroom on the fixed code (milliseconds) and is far exceeded
// by the quadratic version (minutes).
func TestRYW_UnreadableCapScanNotQuadratic(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	begin, end := []byte("zz-window/"), []byte("zz-window0")
	start := time.Now()
	for i := 0; i < 50000; i++ {
		key := []byte(fmt.Sprintf("bulk/%08d", i))
		c.set(key, []byte("v"))
		c.mu.Lock()
		if cap_ := c.unreadableScanCapLocked(begin, end, false); cap_ != nil {
			c.mu.Unlock()
			t.Fatalf("cap over a window with no unreadable state = %q, want nil", cap_)
		}
		if cap_ := c.unreadableScanCapLocked(begin, end, true); cap_ != nil {
			c.mu.Unlock()
			t.Fatalf("reverse cap over a window with no unreadable state = %q, want nil", cap_)
		}
		c.mu.Unlock()
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("50k interleaved set+capScan took %v — the per-read sortedKeys rebuild is back", elapsed)
	}
}

// TestRYW_UnreadableKeysIndex pins the incremental unreadableKeys index against
// the flag transitions: atomic-with-stamp inserts, plain Set preserves, clear
// and clearRange remove, and the cap scan sees exactly the live entries.
func TestRYW_UnreadableKeysIndex(t *testing.T) {
	t.Parallel()
	c := &rywCache{}
	op := make([]byte, 14) // 10-byte placeholder + LE32(0)
	all := func() ([]byte, []byte) { return []byte("a"), []byte("z") }

	c.atomic(MutSetVersionstampedValue, []byte("k2"), op)
	c.atomic(MutSetVersionstampedValue, []byte("k1"), op)
	c.set([]byte("k1"), []byte("plain")) // sticky: stays in the index
	c.set([]byte("k0"), []byte("plain")) // never unreadable: not in the index

	b, e := all()
	c.mu.Lock()
	if got := c.unreadableScanCapLocked(b, e, false); string(got) != "k1" {
		c.mu.Unlock()
		t.Fatalf("forward cap = %q, want k1", got)
	}
	if got := c.unreadableScanCapLocked(b, e, true); string(got) != "k2\x00" {
		c.mu.Unlock()
		t.Fatalf("reverse cap = %q, want k2\\x00", got)
	}
	c.mu.Unlock()

	c.clear([]byte("k1"))
	c.mu.Lock()
	if got := c.unreadableScanCapLocked(b, e, false); string(got) != "k2" {
		c.mu.Unlock()
		t.Fatalf("forward cap after clear(k1) = %q, want k2", got)
	}
	c.mu.Unlock()

	c.clearRange([]byte("k"), []byte("k\xff"))
	c.mu.Lock()
	if got := c.unreadableScanCapLocked(b, e, false); got != nil {
		c.mu.Unlock()
		t.Fatalf("cap after clearRange = %q, want nil", got)
	}
	if len(c.unreadableKeys) != 0 {
		c.mu.Unlock()
		t.Fatalf("unreadableKeys after clearRange = %q, want empty", c.unreadableKeys)
	}
	c.mu.Unlock()
}

func TestFDB_Unreadable_Matrix(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	// No defer db.Close(): the parallel subtests outlive this function body;
	// openTestDB's t.Cleanup closes the handle after they all finish.
	db := openTestDB(t, ctx)

	pfx := []byte(t.Name() + "/")
	storageKey := append(append([]byte(nil), pfx...), []byte("seeded")...)
	if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
		tx.Set(storageKey, []byte("storage-v"))
		return nil, nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	t.Run("svv_regular_get_1036", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("svv1")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		_, err := tx.Get(ctx, k)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("svv_snapshot_get_1036", func(t *testing.T) {
		// Snapshot reads with snapshot-RYW enabled (the default) traverse the
		// write map and throw too (C++ :400-405 dispatch).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("svv2")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		_, err := tx.Snapshot().Get(ctx, k)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("svv_snapshot_rywoff_reads_storage", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		tx.SetSnapshotRYWDisable()
		tx.Atomic(MutSetVersionstampedValue, storageKey, svvOperand())
		v, err := tx.Snapshot().Get(ctx, storageKey)
		if err != nil {
			t.Fatalf("snapshot+rywOff Get: %v", err)
		}
		if string(v) != "storage-v" {
			t.Fatalf("snapshot+rywOff Get = %q, want storage value", v)
		}
	})

	t.Run("svv_rywdisabled_reads_storage", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		tx.SetReadYourWritesDisable()
		tx.Atomic(MutSetVersionstampedValue, storageKey, svvOperand())
		v, err := tx.Get(ctx, storageKey)
		if err != nil {
			t.Fatalf("rywDisabled Get: %v", err)
		}
		if string(v) != "storage-v" {
			t.Fatalf("rywDisabled Get = %q, want storage value", v)
		}
	})

	t.Run("svv_bypass_returns_operand", func(t *testing.T) {
		// C++ bypass returns the write-map value with placeholder bytes as
		// written, INCLUDING the trailing offset suffix (RYWIterator.cpp:433-449
		// pins kv->value == metadataVersionRequiredValue, all 14 bytes).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		tx.SetBypassUnreadable(true)
		k := append(append([]byte(nil), pfx...), []byte("svv3")...)
		op := svvOperand()
		tx.Atomic(MutSetVersionstampedValue, k, op)
		v, err := tx.Get(ctx, k)
		if err != nil {
			t.Fatalf("bypass Get: %v", err)
		}
		if !bytes.Equal(v, op) {
			t.Fatalf("bypass Get = %x, want the operand as written %x", v, op)
		}
	})

	t.Run("bypass_not_persistent_across_reset", func(t *testing.T) {
		// bypass_unreadable carries no persistent="true" in fdb.options: C++
		// resetRyow() → options.reset() drops it and applyPersistentOptions
		// does NOT re-apply it. After Reset the same read throws again.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		tx.SetBypassUnreadable(true)
		k := append(append([]byte(nil), pfx...), []byte("svv6")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		if _, err := tx.Get(ctx, k); err != nil {
			t.Fatalf("bypass Get before reset: %v", err)
		}
		tx.Reset()
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		_, err := tx.Get(ctx, k)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("svv_sticky_plain_set_still_1036", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("svv4")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		tx.Set(k, []byte("later"))
		_, err := tx.Get(ctx, k)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("svk_different_key_in_range_1036", func(t *testing.T) {
		// SVK marks the ENTIRE candidate stamp range unreadable
		// (ReadYourWrites.actor.cpp:2271): reading a DIFFERENT key inside
		// it throws.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		svkPfx := append(append([]byte(nil), pfx...), []byte("svk1/")...)
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		_, err := tx.Get(ctx, other)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("svk_bypass_reads_through", func(t *testing.T) {
		// The SVK range is UNMODIFIED+unreadable: under bypass a key in the
		// range with no local entry reads through to storage (absent here).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		tx.SetBypassUnreadable(true)
		svkPfx := append(append([]byte(nil), pfx...), []byte("svk2/")...)
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		v, err := tx.Get(ctx, other)
		if err != nil {
			t.Fatalf("bypass Get in SVK range: %v", err)
		}
		if v != nil {
			t.Fatalf("bypass Get in SVK range = %x, want absent (reads through to storage)", v)
		}
	})

	t.Run("svk_clear_erases_range_unreadability", func(t *testing.T) {
		// A Clear over the candidate range makes the cleared span readable
		// again (C++ gets this free from the shared PTree; review caution).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		svkPfx := append(append([]byte(nil), pfx...), []byte("svk3/")...)
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		clearEnd := append(append([]byte(nil), svkPfx...), 0xff, 0xff)
		tx.ClearRange(svkPfx, clearEnd)
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		v, err := tx.Get(ctx, other)
		if err != nil {
			t.Fatalf("Get after clearing the SVK range: %v", err)
		}
		if v != nil {
			t.Fatalf("Get after clear = %x, want absent (cleared)", v)
		}
	})

	t.Run("getkey_reaching_pending_stamp_1036", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("zz-getkey")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		// firstGreaterOrEqual(k) = {k, orEqual:false, +1} lands directly on the
		// pending-stamp segment → 1036. (firstGreaterThan would removeOrEqual to
		// FGE(keyAfter(k)) and legitimately skip past it without throwing.)
		_, err := tx.GetKey(ctx, k, false, 1)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)

		if _, err := tx.GetKey(ctx, k, true, 1); err != nil {
			t.Fatalf("firstGreaterThan(stamp key) resolves past the segment without touching it: %v", err)
		}
	})

	t.Run("pipelined_get_svk_range_1036", func(t *testing.T) {
		// GetPipelined's inline cache consult must hit the unreadable gate too:
		// before RFC-098 it read straight through to storage for a key inside a
		// pending SVK candidate range (the facade Get path — a silent wrong
		// answer vs libfdb_c, caught by the differential).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		svkPfx := append(append([]byte(nil), pfx...), []byte("svk4/")...)
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		other := append(append([]byte(nil), svkPfx...), bytes.Repeat([]byte{0x7f}, 10)...)
		_, pending, err := tx.GetPipelined(ctx, other)
		if pending != nil {
			t.Fatalf("GetPipelined sent a server read for a key in a pending SVK range")
		}
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("pipelined_get_sticky_entry_1036", func(t *testing.T) {
		// A plain Set AFTER a versionstamped op folds the entry to a resolved
		// value but keeps it unreadable (sticky). GetPipelined must not return
		// that folded value as a cache hit.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("svv5")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		tx.Set(k, []byte("later"))
		v, pending, err := tx.GetPipelined(ctx, k)
		if pending != nil || v != nil {
			t.Fatalf("GetPipelined returned (%x, pending=%v) for a sticky-unreadable entry", v, pending)
		}
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("commit_poisoned_by_swallowed_read_error", func(t *testing.T) {
		// C++ commit() waits on ryw->reading (the AndFuture of every read this
		// transaction issued) before any commit work: a read that failed 1036 —
		// even though the caller swallowed the error — fails the commit with the
		// same 1036. Reset clears the poison (resetRyow: reading = AndFuture()).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("poison1")...)
		other := append(append([]byte(nil), pfx...), []byte("poison1-other")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		if _, err := tx.Get(ctx, k); err == nil {
			t.Fatal("Get of pending SVV did not fail")
		}
		tx.Set(other, []byte("v"))
		assertFDBErrorCode(t, tx.Commit(ctx), ErrAccessedUnreadable)

		tx.Reset()
		tx.Set(other, []byte("v2"))
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit after Reset cleared the read poison: %v", err)
		}
	})

	t.Run("commit_poison_precedes_validation", func(t *testing.T) {
		// C++ commit() waits on ryw->reading before ANY commit work (:1358 —
		// before writeRangeToNativeTransaction and tr.commit's checks). Go
		// defers the oversized-key rejection to commit (set() is void; C++
		// throws 2102 eagerly at set() — documented divergence), so the poison
		// check must precede that validation: a poisoned commit carrying an
		// oversized key reports the read's 1036, not key_too_large (2102).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		tx := db.CreateTransaction()
		k := append(append([]byte(nil), pfx...), []byte("poison2")...)
		tx.Atomic(MutSetVersionstampedValue, k, svvOperand())
		if _, err := tx.Get(ctx, k); err == nil {
			t.Fatal("Get of pending SVV did not fail")
		}
		tx.Set(make([]byte, 10001), []byte("v"))
		assertFDBErrorCode(t, tx.Commit(ctx), ErrAccessedUnreadable)
	})

	t.Run("getkey_crosses_svk_range_1036", func(t *testing.T) {
		// Documenting row for the FDB-C++ boundary catch: this shape passes
		// even WITHOUT the boundCandidatesLocked unreadableRanges fix (the
		// pending entry's own write-key boundary stops the walk) — the actual
		// red→green pin is getkey_from_inside_svk_range_head_1036. Kept
		// because it covers 1036 through the forward stop-at-entry path.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		gPfx := append(append([]byte(nil), pfx...), []byte("gkx/")...)
		a := append(append([]byte(nil), gPfx...), 'a')
		c := append(append([]byte(nil), gPfx...), 'c')
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(a, []byte("va"))
			tx.Set(c, []byte("vc"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		svkPfx := append(append([]byte(nil), gPfx...), 'b')
		tx := db.CreateTransaction()
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		// Forward: fGT(a) must stop on the candidate range, not resolve to c.
		_, err := tx.GetKey(ctx, a, true, 1)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
		// Reverse: lastLessThan(c) walks down across the range.
		_, err = tx.GetKey(ctx, c, false, 0)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("getkey_crosses_emptied_svk_range_1036", func(t *testing.T) {
		// Same as above but the range's only write ENTRY is cleared away
		// (clear subtracts only its own span from the unreadable ranges), so
		// the remaining unreadable span contains NO write-map key at all —
		// the pure missing-boundary shape.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		gPfx := append(append([]byte(nil), pfx...), []byte("gke/")...)
		a := append(append([]byte(nil), gPfx...), 'a')
		c := append(append([]byte(nil), gPfx...), 'c')
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(a, []byte("va"))
			tx.Set(c, []byte("vc"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		svkPfx := append(append([]byte(nil), gPfx...), 'b')
		tx := db.CreateTransaction()
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		// Clear ONLY the entry's vicinity at the start of the candidate range:
		// the entry vanishes, [svkPfx+\x01, range end) stays unreadable.
		clearEnd := append(append([]byte(nil), svkPfx...), 0x01)
		tx.ClearRange(svkPfx, clearEnd)
		_, err := tx.GetKey(ctx, a, true, 1)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
		_, err = tx.GetKey(ctx, c, false, 0)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("getkey_from_inside_svk_range_head_1036", func(t *testing.T) {
		// FDB-C++ review catch (boundCandidatesLocked omits unreadableRanges
		// edges): the candidate range BEGIN B = key@stamp(minVersion) precedes
		// the pending entry T = B + 4 suffix bytes, so the head sub-span [B, T)
		// contains no write-map key and — without a boundary at B — is swallowed
		// into the unknown segment that starts BELOW the range. A reverse
		// selector anchored inside [B, T) then escapes downward and resolves to
		// a storage key, where libfdb_c classifies the unreadable range node and
		// throws (WriteMap addUnmodifiedAndUnreadableRange boundary nodes;
		// RYWIterator.cpp:45-46).
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		gPfx := append(append([]byte(nil), pfx...), []byte("gkh/")...)
		a := append(append([]byte(nil), gPfx...), 'a')
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(a, []byte("va"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}

		svkPfx := append(append([]byte(nil), gPfx...), 'b')
		tx := db.CreateTransaction()
		tx.Atomic(MutSetVersionstampedKey, svkKey(svkPfx), []byte("v"))
		// Fresh tx, no read version → minVersion 0 → B = svkPfx + 10 zero
		// bytes; the entry sits at T = B + LE32(len(svkPfx)). Anchor strictly
		// inside (B, T).
		inside := append(append([]byte(nil), svkPfx...), make([]byte, 10)...)
		inside = append(inside, 0x00, 0x00, 0x01)
		_, err := tx.GetKey(ctx, inside, false, 0) // lastLessThan(inside)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})

	t.Run("getrange_reach_semantics", func(t *testing.T) {
		// A limited scan that stops BEFORE the pending key does not throw
		// (C++ :685 limit-break precedes the :692 throw); reaching it does.
		// Reverse symmetric.
		t.Parallel()
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		rPfx := append(append([]byte(nil), pfx...), []byte("reach/")...)
		a := append(append([]byte(nil), rPfx...), 'a')
		b := append(append([]byte(nil), rPfx...), 'b')
		z := append(append([]byte(nil), rPfx...), 'z')
		if _, err := db.Transact(ctx, func(tx *Transaction) (any, error) {
			tx.Set(a, []byte("va"))
			tx.Set(b, []byte("vb"))
			return nil, nil
		}); err != nil {
			t.Fatalf("seed: %v", err)
		}
		end := append(append([]byte(nil), rPfx...), 0xff)

		tx := db.CreateTransaction()
		tx.Atomic(MutSetVersionstampedValue, z, svvOperand())

		kvs, _, err := tx.GetRange(ctx, rPfx, end, 2)
		if err != nil {
			t.Fatalf("limited forward scan stopping before the stamp: %v", err)
		}
		if len(kvs) != 2 || !bytes.Equal(kvs[0].Key, a) || !bytes.Equal(kvs[1].Key, b) {
			t.Fatalf("limited scan = %v, want [a b]", kvs)
		}

		_, _, err = tx.GetRange(ctx, rPfx, end, 0) // unlimited reaches z
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)

		// Reverse: the pending key is FIRST in iteration order — even limit 1
		// reaches it.
		_, _, err = tx.GetRangeReverse(ctx, rPfx, end, 1)
		assertFDBErrorCode(t, err, ErrAccessedUnreadable)
	})
}
