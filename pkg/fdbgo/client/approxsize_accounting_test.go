package client

import "testing"

// TestGetApproximateSize_CppAccounting pins the exact C++ ReadYourWrites approximateSize accounting
// (no FDB, deterministic): sizeof(MutationRef)=44, sizeof(KeyRangeRef)=24 — StringRef is packed to
// 12 bytes by flow/Arena.h:370 `#pragma pack(push, 4)` — and the single-key-clear distinction (its
// mutation part is charged sizeof(KeyRangeRef), NOT sizeof(MutationRef); ReadYourWrites.actor.cpp:2431).
// Revert-proof: flipping either constant back to 48/32, or dropping the singleKeyClearCount
// adjustment, fails an assertion here. The cross-client proof is bench TestDifferential_ApproximateSize.
func TestGetApproximateSize_CppAccounting(t *testing.T) {
	t.Parallel()
	const (
		m = 44 // sizeof(MutationRef)
		r = 24 // sizeof(KeyRangeRef)
	)

	t.Run("set", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		k, v := []byte("key1"), []byte("value12")
		tx.Set(k, v)
		// mutation: len(k)+len(v)+M ; implicit write conflict [k, k+\x00]: len(k)+(len(k)+1)+R
		want := int64(len(k)+len(v)+m) + int64(len(k)+(len(k)+1)+r)
		if got := tx.GetApproximateSize(); got != want {
			t.Fatalf("Set: got %d want %d", got, want)
		}
	})

	t.Run("clear_single_key", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		k := []byte("singlekey")
		tx.Clear(k)
		// mutation (single-key clear, charged R not M): len(k)+(len(k)+1)+R
		// implicit write conflict [k, k+\x00]:           len(k)+(len(k)+1)+R
		want := int64(len(k)+(len(k)+1)+r) * 2
		if got := tx.GetApproximateSize(); got != want {
			t.Fatalf("Clear(single key): got %d want %d", got, want)
		}
	})

	t.Run("clear_range", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		b, e := []byte("r0"), []byte("r9")
		if err := tx.ClearRange(b, e); err != nil {
			t.Fatal(err)
		}
		// mutation (range clear, charged M): len(b)+len(e)+M ; write conflict [b,e]: len(b)+len(e)+R
		want := int64(len(b)+len(e)+m) + int64(len(b)+len(e)+r)
		if got := tx.GetApproximateSize(); got != want {
			t.Fatalf("ClearRange: got %d want %d", got, want)
		}
	})

	t.Run("conflict_ranges", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		rb, re := []byte("rc0"), []byte("rc9")
		wb, we := []byte("wc0"), []byte("wc9")
		if err := tx.AddReadConflictRange(rb, re); err != nil {
			t.Fatal(err)
		}
		if err := tx.AddWriteConflictRange(wb, we); err != nil {
			t.Fatal(err)
		}
		want := int64(len(rb)+len(re)+r) + int64(len(wb)+len(we)+r)
		if got := tx.GetApproximateSize(); got != want {
			t.Fatalf("conflict ranges: got %d want %d", got, want)
		}
	})

	t.Run("reset_clears_single_key_count", func(t *testing.T) {
		t.Parallel()
		tx := newTestTx()
		tx.rywDisabled = true
		tx.Clear([]byte("k"))
		tx.reset()
		if got := tx.GetApproximateSize(); got != 0 {
			t.Fatalf("after reset: got %d want 0", got)
		}
		// A range clear after reset must NOT be mis-charged as a single-key clear (count was reset),
		// i.e. its mutation is charged M, not R.
		b, e := []byte("a"), []byte("b")
		if err := tx.ClearRange(b, e); err != nil {
			t.Fatal(err)
		}
		want := int64(len(b)+len(e)+m) + int64(len(b)+len(e)+r)
		if got := tx.GetApproximateSize(); got != want {
			t.Fatalf("ClearRange after reset: got %d want %d (single-key count leaked?)", got, want)
		}
	})
}
