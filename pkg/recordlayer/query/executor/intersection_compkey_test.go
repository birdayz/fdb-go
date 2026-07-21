package executor

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// TestIntersectionCompKeyFunc_Int32Widened pins RFC-092 (TODO-production P0.3-G).
//
// A comparison key that evaluates to int32 must be widened to int64 before it
// enters the tuple.Tuple: the FDB tuple layer has no int32 case and Pack() panics
// on it (tuple.go default arm), and the merge cursor packs the key for
// bytes.Compare (merge_cursor.go compareKeys). Before the fix the intersection
// comparison-key builders stored the raw int32, so an int32-keyed intersection
// errored out via compareKeys' panic-recover instead of returning rows; after, the
// key packs identically to int64 — Pack-safe AND order-preserving (the tuple
// integer encoding orders int64 the same way the child index streams are sorted).
func TestIntersectionCompKeyFunc_Int32Widened(t *testing.T) {
	t.Parallel()

	keyVals := []values.Value{&values.ConstantValue{Value: int32(7), Typ: values.TypeInt}}
	want := tuple.Tuple{int64(7)}.Pack()

	assertPacks := func(t *testing.T, tup tuple.Tuple) {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("comparison key with int32 element panicked in Pack: %v", r)
			}
		}()
		if got := tup.Pack(); !bytes.Equal(got, want) {
			t.Errorf("int32 comparison key packed as %x, want int64-equivalent %x", got, want)
		}
	}

	// Constant key args never error; a real eval failure surfaces as the
	// closure's error return (fail the query, not the process).
	t.Run("intersectionCompKeyFunc", func(t *testing.T) {
		tup, err := intersectionCompKeyFunc(keyVals)(dscalar(int64(0)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertPacks(t, tup)
	})

	t.Run("multiIntersectionCompKeyFunc", func(t *testing.T) {
		tup, err := multiIntersectionCompKeyFunc(keyVals)(dscalar(int64(0)))
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertPacks(t, tup)
	})
}
