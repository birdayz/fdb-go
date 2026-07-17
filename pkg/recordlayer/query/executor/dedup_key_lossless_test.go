package executor

import "testing"

// TestPackedDedupKey_CompositeLossless pins RFC-180 C1/C3: composite slots
// encode via the continuation codec, not a rendered string — the %T:%v form
// collided on []any{"a b"} vs []any{"a","b"} (both rendered "[a b]"),
// merging distinct rows under DISTINCT/GROUP BY. RED pre-fix.
func TestPackedDedupKey_CompositeLossless(t *testing.T) {
	t.Parallel()
	a, errA := packedDedupKey([]any{[]any{"a b"}})
	b, errB := packedDedupKey([]any{[]any{"a", "b"}})
	if errA != nil || errB != nil {
		t.Fatalf("composite slots must encode: %v %v", errA, errB)
	}
	if a == b {
		t.Fatal("distinct composite rows must produce distinct dedup keys")
	}
	// Equal composites still collide (same key) — dedup correctness.
	c, _ := packedDedupKey([]any{[]any{"a", "b"}})
	if b != c {
		t.Fatal("equal composite rows must produce equal dedup keys")
	}
}
