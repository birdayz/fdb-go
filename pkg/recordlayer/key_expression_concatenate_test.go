package recordlayer

import (
	"bytes"
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// TestFieldKeyExpression_ConcatenatePacksAsNestedTuple pins that a FanType.Concatenate
// index over a repeated field produces a PACKABLE index-key element — a nested
// tuple.Tuple, matching Java's Tuple.addObject(List) — not a bare []any.
//
// A bare []any has no case in the FDB tuple packer and hits its `default: panic
// ("unencodable element")`. Since the index-maintainer pack path copies the key
// element verbatim (index_maintainer.go) and then calls Pack, the pre-fix code
// panicked on EVERY save of a record carrying a Concatenate index over a repeated
// field (and on the empty/unset case via getNullResult). don't-leak-panics: a
// user-saved record must never crash the write path.
func TestFieldKeyExpression_ConcatenatePacksAsNestedTuple(t *testing.T) {
	t.Parallel()

	packRow := func(t *testing.T, row []any) []byte {
		t.Helper()
		k := make(tuple.Tuple, len(row))
		for i, v := range row { // element-wise (the maintainer's pack path does the same)
			k[i] = v
		}
		var out []byte
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("index-key Pack panicked (unencodable element): %v", r)
			}
		}()
		out = k.Pack()
		return out
	}

	t.Run("non-empty", func(t *testing.T) {
		t.Parallel()
		expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
		order := newOrder(1, 100, "a", "b", "c")
		rows, err := expr.Evaluate(asStored(order), order)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("Concatenate: want 1 row of 1 element, got %v", rows)
		}
		got := packRow(t, rows[0])
		want := tuple.Tuple{tuple.Tuple{"a", "b", "c"}}.Pack()
		if !bytes.Equal(got, want) {
			t.Errorf("Concatenate key = %x, want nested-tuple encoding %x", got, want)
		}
	})

	t.Run("empty-repeated", func(t *testing.T) {
		t.Parallel()
		expr := &FieldKeyExpression{fieldName: "tags", fanType: FanTypeConcatenate}
		order := newOrder(2, 100) // no tags
		rows, err := expr.Evaluate(asStored(order), order)
		if err != nil {
			t.Fatalf("Evaluate: %v", err)
		}
		if len(rows) != 1 || len(rows[0]) != 1 {
			t.Fatalf("Concatenate(empty): want 1 row of 1 element, got %v", rows)
		}
		got := packRow(t, rows[0])
		want := tuple.Tuple{tuple.Tuple{}}.Pack()
		if !bytes.Equal(got, want) {
			t.Errorf("Concatenate(empty) key = %x, want empty-nested-tuple %x", got, want)
		}
	})
}
