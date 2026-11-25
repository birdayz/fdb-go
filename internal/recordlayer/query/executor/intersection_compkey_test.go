package executor

import (
	"bytes"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
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

	keyVals := []values.Value{&values.ConstantValue{Value: int32(7), Typ: values.NullableLong}}
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

// An admitted child row carries a current-only OrdinalLayout, which disables
// ambient positional fallback for named QOVs. The comparison-key program owns
// a synthetic exact row QOV shared by every intersection leg; both comparison
// key closures must explicitly declare it as the phase's input edge.
func TestIntersectionCompKeyFunc_DeclaresSyntheticInputOverLayoutRow(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("aggregate_index_row", false, []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "SUM(V)", FieldType: values.NullableLong, Ordinal: 1},
	})
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), rowType)
	key := mustTestFieldOrdinal(t, root, 0)
	layout, err := values.NewOrdinalLayoutForCarrierType(rowType, []values.OrdinalTileSpec{{
		Start: 0, Width: 2, Kind: values.OrdinalTileFlat,
	}}, nil)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatalf("layout row: %v", err)
	}
	row.Slots[0], row.Slots[1] = int64(7), int64(42)
	qr := QueryResult{Positional: row}

	for name, fn := range map[string]recordlayer.ComparisonKeyFunc[QueryResult]{
		"intersection":       intersectionCompKeyFunc([]values.Value{key}),
		"multi-intersection": multiIntersectionCompKeyFunc([]values.Value{key}),
	} {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := fn(qr)
			if err != nil {
				t.Fatalf("comparison key: %v", err)
			}
			if len(got) != 1 || got[0] != int64(7) {
				t.Fatalf("comparison key = %v, want [7]", got)
			}
		})
	}
}

// The current root is supplied by the admitted layout's carrier, not by a
// physical quantifier edge. Treating it as an edge is both redundant and
// invalid: the edge binder rejects the reserved current correlation so it
// cannot be forged into another source namespace.
func TestIntersectionCompKeyFunc_UsesLayoutCarrierForCurrentRoot(t *testing.T) {
	t.Parallel()
	rowType := values.NewRecordType("current_key_row", false, []values.Field{
		{Name: "K", FieldType: values.NotNullLong, Ordinal: 0},
	})
	layout, err := values.NewOrdinalLayoutForCarrierType(rowType, []values.OrdinalTileSpec{{
		Start: 0, Width: 1, Kind: values.OrdinalTileFlat,
	}}, nil)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	key := mustTestFieldOrdinal(t, layout.Carrier(), 0)
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatalf("layout row: %v", err)
	}
	row.Slots[0] = int64(9)
	qr := QueryResult{Positional: row}

	for name, fn := range map[string]recordlayer.ComparisonKeyFunc[QueryResult]{
		"intersection":       intersectionCompKeyFunc([]values.Value{key}),
		"multi-intersection": multiIntersectionCompKeyFunc([]values.Value{key}),
	} {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, keyErr := fn(qr)
			if keyErr != nil {
				t.Fatalf("comparison key: %v", keyErr)
			}
			if len(got) != 1 || got[0] != int64(9) {
				t.Fatalf("comparison key = %v, want [9]", got)
			}
		})
	}
}

func TestMultiIntersectionCompKeyFunc_AcceptsSiblingAggregatePayloadType(t *testing.T) {
	t.Parallel()
	keyType := values.NewRecordType("first_aggregate_index_row", false, []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "SUM(V)", FieldType: values.NullableLong, Ordinal: 1},
	})
	rowType := values.NewRecordType("sibling_aggregate_index_row", false, []values.Field{
		{Name: "G", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "COUNT(*)", FieldType: values.NullableLong, Ordinal: 1},
	})
	root := mustTestQOV(t, values.UniqueCorrelationIdentifier(), keyType)
	key := mustTestFieldOrdinal(t, root, 0)
	layout, err := values.NewOrdinalLayoutForCarrierType(rowType, []values.OrdinalTileSpec{{
		Start: 0, Width: 2, Kind: values.OrdinalTileFlat,
	}}, nil)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	row, err := NewLayoutPositionalRow(rowType, layout)
	if err != nil {
		t.Fatalf("layout row: %v", err)
	}
	row.Slots[0], row.Slots[1] = int64(7), int64(1)

	got, err := multiIntersectionCompKeyFunc([]values.Value{key})(QueryResult{Positional: row})
	if err != nil {
		t.Fatalf("comparison key: %v", err)
	}
	if len(got) != 1 || got[0] != int64(7) {
		t.Fatalf("comparison key = %v, want [7]", got)
	}
}
