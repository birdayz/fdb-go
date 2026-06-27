package recordlayer

import (
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// TestTupleToVector pins the error contract for the VECTOR index write path
// (FDB C++ review of PR #272, finding C1): an absent/null vector is skipped, but a
// NON-null but UNDECODABLE vector must return an ERROR — Java's RealVector.fromBytes
// throws and fails the write. Pre-fix tupleToVector returned nil for an undecodable
// vector too, so the maintainer silently skipped it (`if vector == nil { continue }`),
// saving the record UNINDEXED — a vector search would miss the row. Surfacing the
// error makes the write fail, matching Java and avoiding silent index incompleteness.
func TestTupleToVector(t *testing.T) {
	t.Parallel()

	t.Run("valid-numeric", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{float64(1), float64(2), float64(3)})
		if err != nil {
			t.Fatalf("valid numeric vector: unexpected error %v", err)
		}
		if len(v) != 3 || v[0] != 1 || v[2] != 3 {
			t.Fatalf("valid numeric vector: got %v, want [1 2 3]", v)
		}
	})

	t.Run("absent-empty-skips", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{})
		if err != nil || v != nil {
			t.Fatalf("empty tuple: got (%v, %v), want (nil, nil) — an absent vector is skipped", v, err)
		}
	})

	t.Run("null-component-skips", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{nil})
		if err != nil || v != nil {
			t.Fatalf("null element: got (%v, %v), want (nil, nil) — a null vector is skipped (matches Java)", v, err)
		}
	})

	t.Run("undecodable-bytes-errors", func(t *testing.T) {
		t.Parallel()
		// A non-null but garbage serialized vector. Pre-fix this returned (nil, nil) and
		// the record was saved unindexed; now it must error (Java throws).
		v, err := tupleToVector(tuple.Tuple{[]byte{0xff, 0x01, 0x02}})
		if err == nil {
			t.Fatalf("undecodable serialized vector: want error, got (%v, nil) — would save unindexed", v)
		}
	})

	t.Run("non-numeric-errors", func(t *testing.T) {
		t.Parallel()
		v, err := tupleToVector(tuple.Tuple{"not a number"})
		if err == nil {
			t.Fatalf("non-numeric element: want error, got (%v, nil)", v)
		}
	})
}
