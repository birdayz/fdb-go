package cascades

import (
	"testing"

	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

func TestEmptyGroupByMappings(t *testing.T) {
	t.Parallel()

	gm := EmptyGroupByMappings()
	if gm == nil {
		t.Fatal("EmptyGroupByMappings returned nil")
	}
	if gm.MatchedGroupingsMap().Len() != 0 {
		t.Fatalf("expected empty matchedGroupingsMap, got %d entries", gm.MatchedGroupingsMap().Len())
	}
	if gm.MatchedAggregatesMap().Len() != 0 {
		t.Fatalf("expected empty matchedAggregatesMap, got %d entries", gm.MatchedAggregatesMap().Len())
	}
	if gm.UnmatchedAggregatesMap().Len() != 0 {
		t.Fatalf("expected empty unmatchedAggregatesMap, got %d entries", gm.UnmatchedAggregatesMap().Len())
	}
}

func TestGroupByMappingsWithPopulatedMaps(t *testing.T) {
	t.Parallel()

	// Build grouping map: query value -> candidate value.
	groupingsMap := NewValueBiMap()
	qGroupVal := &values.ConstantValue{Value: "q_group_col1", Typ: values.TypeString}
	cGroupVal := &values.ConstantValue{Value: "c_group_col1", Typ: values.TypeString}
	groupingsMap.Put(qGroupVal, cGroupVal)

	// Build aggregates map.
	aggMap := NewValueBiMap()
	qAggVal := &values.ConstantValue{Value: "q_sum", Typ: values.TypeInt}
	cAggVal := &values.ConstantValue{Value: "c_sum", Typ: values.TypeInt}
	aggMap.Put(qAggVal, cAggVal)

	// Build unmatched aggregates map.
	unmatchedMap := NewCorrValueBiMap()
	alias := values.NamedCorrelationIdentifier("unmatched_agg_alias")
	unmatchedVal := &values.ConstantValue{Value: "unmatched_count", Typ: values.TypeInt}
	unmatchedMap.Put(alias, unmatchedVal)

	gm := NewGroupByMappings(groupingsMap, aggMap, unmatchedMap)

	// Verify groupings map.
	if gm.MatchedGroupingsMap().Len() != 1 {
		t.Fatalf("expected 1 grouping entry, got %d", gm.MatchedGroupingsMap().Len())
	}
	gotCand, ok := gm.MatchedGroupingsMap().Get(qGroupVal)
	if !ok {
		t.Fatal("query grouping value not found in map")
	}
	if gotCand != cGroupVal {
		t.Fatalf("expected candidate grouping value %v, got %v", cGroupVal, gotCand)
	}

	// Verify aggregates map.
	if gm.MatchedAggregatesMap().Len() != 1 {
		t.Fatalf("expected 1 aggregate entry, got %d", gm.MatchedAggregatesMap().Len())
	}
	gotAgg, ok := gm.MatchedAggregatesMap().Get(qAggVal)
	if !ok {
		t.Fatal("query aggregate value not found in map")
	}
	if gotAgg != cAggVal {
		t.Fatalf("expected candidate aggregate value %v, got %v", cAggVal, gotAgg)
	}

	// Verify unmatched aggregates map.
	if gm.UnmatchedAggregatesMap().Len() != 1 {
		t.Fatalf("expected 1 unmatched entry, got %d", gm.UnmatchedAggregatesMap().Len())
	}
	gotUnmatched, ok := gm.UnmatchedAggregatesMap().Get(alias)
	if !ok {
		t.Fatal("alias not found in unmatched aggregates map")
	}
	if gotUnmatched != unmatchedVal {
		t.Fatalf("expected unmatched value %v, got %v", unmatchedVal, gotUnmatched)
	}
}

func TestGroupByMappingsDefensiveCopy(t *testing.T) {
	t.Parallel()

	// Verify that mutations to the source maps don't affect GroupByMappings.
	groupingsMap := NewValueBiMap()
	qVal := &values.ConstantValue{Value: "q", Typ: values.TypeString}
	cVal := &values.ConstantValue{Value: "c", Typ: values.TypeString}
	groupingsMap.Put(qVal, cVal)

	aggMap := NewValueBiMap()
	unmatchedMap := NewCorrValueBiMap()

	gm := NewGroupByMappings(groupingsMap, aggMap, unmatchedMap)

	// Mutate the original map after construction.
	qVal2 := &values.ConstantValue{Value: "q2", Typ: values.TypeString}
	cVal2 := &values.ConstantValue{Value: "c2", Typ: values.TypeString}
	groupingsMap.Put(qVal2, cVal2)

	// GroupByMappings should still have only the original entry.
	if gm.MatchedGroupingsMap().Len() != 1 {
		t.Fatalf("defensive copy failed: expected 1 entry, got %d", gm.MatchedGroupingsMap().Len())
	}
}

func TestGroupByMappingsMultipleEntries(t *testing.T) {
	t.Parallel()

	groupingsMap := NewValueBiMap()
	for i := 0; i < 5; i++ {
		q := &values.ConstantValue{Value: int64(i), Typ: values.TypeInt}
		c := &values.ConstantValue{Value: int64(i + 100), Typ: values.TypeInt}
		groupingsMap.Put(q, c)
	}

	aggMap := NewValueBiMap()
	for i := 0; i < 3; i++ {
		q := &values.ConstantValue{Value: int64(i + 200), Typ: values.TypeInt}
		c := &values.ConstantValue{Value: int64(i + 300), Typ: values.TypeInt}
		aggMap.Put(q, c)
	}

	unmatchedMap := NewCorrValueBiMap()
	for i := 0; i < 2; i++ {
		alias := values.NamedCorrelationIdentifier("alias_" + string(rune('a'+i)))
		val := &values.ConstantValue{Value: int64(i + 400), Typ: values.TypeInt}
		unmatchedMap.Put(alias, val)
	}

	gm := NewGroupByMappings(groupingsMap, aggMap, unmatchedMap)

	if gm.MatchedGroupingsMap().Len() != 5 {
		t.Fatalf("expected 5 grouping entries, got %d", gm.MatchedGroupingsMap().Len())
	}
	if gm.MatchedAggregatesMap().Len() != 3 {
		t.Fatalf("expected 3 aggregate entries, got %d", gm.MatchedAggregatesMap().Len())
	}
	if gm.UnmatchedAggregatesMap().Len() != 2 {
		t.Fatalf("expected 2 unmatched entries, got %d", gm.UnmatchedAggregatesMap().Len())
	}
}

// --- BiMap tests (data structure underpinning GroupByMappings) ---

func TestBiMapEmpty(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	if bm.Len() != 0 {
		t.Fatalf("expected empty bimap, got len %d", bm.Len())
	}
	_, ok := bm.Get("x")
	if ok {
		t.Fatal("expected Get on empty bimap to return false")
	}
	_, ok = bm.GetInverse(42)
	if ok {
		t.Fatal("expected GetInverse on empty bimap to return false")
	}
}

func TestBiMapPutAndGet(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("b", 2)
	bm.Put("c", 3)

	if bm.Len() != 3 {
		t.Fatalf("expected len 3, got %d", bm.Len())
	}

	v, ok := bm.Get("a")
	if !ok || v != 1 {
		t.Fatalf("Get(a) = %d, %v; want 1, true", v, ok)
	}
	v, ok = bm.Get("b")
	if !ok || v != 2 {
		t.Fatalf("Get(b) = %d, %v; want 2, true", v, ok)
	}

	k, ok := bm.GetInverse(3)
	if !ok || k != "c" {
		t.Fatalf("GetInverse(3) = %s, %v; want c, true", k, ok)
	}
}

func TestBiMapDuplicateValuePanics(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate value, got none")
		}
	}()
	bm.Put("b", 1) // same value, different key -> should panic
}

func TestBiMapSameKeyReplace(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("a", 2) // same key, new value -> replace

	if bm.Len() != 1 {
		t.Fatalf("expected len 1 after replace, got %d", bm.Len())
	}
	v, ok := bm.Get("a")
	if !ok || v != 2 {
		t.Fatalf("Get(a) after replace = %d, %v; want 2, true", v, ok)
	}
	// Old value should be removed from inverse.
	_, ok = bm.GetInverse(1)
	if ok {
		t.Fatal("old value 1 should not be in inverse after replace")
	}
	k, ok := bm.GetInverse(2)
	if !ok || k != "a" {
		t.Fatalf("GetInverse(2) = %s, %v; want a, true", k, ok)
	}
}

func TestBiMapSameKeyAndValueNoPanic(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("a", 1) // same key and same value -> no-op, no panic

	if bm.Len() != 1 {
		t.Fatalf("expected len 1, got %d", bm.Len())
	}
}

func TestBiMapFromMap(t *testing.T) {
	t.Parallel()

	m := map[string]int{"x": 10, "y": 20, "z": 30}
	bm := NewBiMapFromMap(m)

	if bm.Len() != 3 {
		t.Fatalf("expected len 3, got %d", bm.Len())
	}
	v, ok := bm.Get("x")
	if !ok || v != 10 {
		t.Fatalf("Get(x) = %d, %v; want 10, true", v, ok)
	}
	k, ok := bm.GetInverse(20)
	if !ok || k != "y" {
		t.Fatalf("GetInverse(20) = %s, %v; want y, true", k, ok)
	}
}

func TestBiMapFromMapDuplicateValuePanics(t *testing.T) {
	t.Parallel()

	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("expected panic on duplicate value in source map")
		}
	}()
	m := map[string]int{"a": 1, "b": 1}
	NewBiMapFromMap(m)
}

func TestBiMapCopy(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("b", 2)

	cp := bm.Copy()
	if cp.Len() != 2 {
		t.Fatalf("copy: expected len 2, got %d", cp.Len())
	}

	// Mutate original.
	bm.Put("c", 3)
	if cp.Len() != 2 {
		t.Fatal("copy was mutated by changes to original")
	}
}

func TestBiMapCopyNilPanics(t *testing.T) {
	t.Parallel()

	defer func() {
		if r := recover(); r == nil {
			t.Fatal("Copy on nil BiMap should panic")
		}
	}()
	var bm *BiMap[string, int]
	bm.Copy()
}

func TestBiMapRangeContents(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("b", 2)

	collected := make(map[string]int)
	bm.Range(func(k string, v int) bool {
		collected[k] = v
		return true
	})
	if len(collected) != 2 || collected["a"] != 1 || collected["b"] != 2 {
		t.Fatalf("Range collected = %v; want {a:1, b:2}", collected)
	}
}

func TestBiMapRange(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("b", 2)
	bm.Put("c", 3)

	seen := make(map[string]int)
	bm.Range(func(k string, v int) bool {
		seen[k] = v
		return true
	})
	if len(seen) != 3 {
		t.Fatalf("Range visited %d entries, want 3", len(seen))
	}
}

func TestBiMapRangeEarlyStop(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)
	bm.Put("b", 2)
	bm.Put("c", 3)

	count := 0
	bm.Range(func(k string, v int) bool {
		count++
		return false // stop after first
	})
	if count != 1 {
		t.Fatalf("Range with early stop visited %d entries, want 1", count)
	}
}

func TestBiMapPutAll(t *testing.T) {
	t.Parallel()

	bm1 := NewBiMap[string, int]()
	bm1.Put("a", 1)

	bm2 := NewBiMap[string, int]()
	bm2.Put("b", 2)
	bm2.Put("c", 3)

	bm1.PutAll(bm2)
	if bm1.Len() != 3 {
		t.Fatalf("expected len 3 after PutAll, got %d", bm1.Len())
	}
	v, ok := bm1.Get("b")
	if !ok || v != 2 {
		t.Fatalf("Get(b) = %d, %v; want 2, true", v, ok)
	}
}

func TestBiMapPutAllNil(t *testing.T) {
	t.Parallel()

	bm := NewBiMap[string, int]()
	bm.Put("a", 1)

	bm.PutAll(nil) // should not panic
	if bm.Len() != 1 {
		t.Fatalf("PutAll(nil) changed len to %d, want 1", bm.Len())
	}
}

func TestBiMapWithValueInterface(t *testing.T) {
	t.Parallel()

	// Ensure BiMap works with values.Value as both K and V (the actual
	// use case in GroupByMappings).
	bm := NewValueBiMap()

	q1 := &values.ConstantValue{Value: "q1", Typ: values.TypeString}
	c1 := &values.ConstantValue{Value: "c1", Typ: values.TypeString}
	q2 := &values.ConstantValue{Value: "q2", Typ: values.TypeString}
	c2 := &values.ConstantValue{Value: "c2", Typ: values.TypeString}

	bm.Put(q1, c1)
	bm.Put(q2, c2)

	if bm.Len() != 2 {
		t.Fatalf("expected len 2, got %d", bm.Len())
	}

	got, ok := bm.Get(q1)
	if !ok {
		t.Fatal("Get(q1) returned false")
	}
	if values.ExplainValue(got) != values.ExplainValue(c1) {
		t.Fatalf("Get(q1) = %v; want c1", values.ExplainValue(got))
	}

	invK, ok := bm.GetInverse(c2)
	if !ok {
		t.Fatal("GetInverse(c2) returned false")
	}
	if values.ExplainValue(invK) != values.ExplainValue(q2) {
		t.Fatalf("GetInverse(c2) = %v; want q2", values.ExplainValue(invK))
	}
}

func TestBiMapWithCorrelationIdentifier(t *testing.T) {
	t.Parallel()

	bm := NewCorrValueBiMap()
	alias1 := values.NamedCorrelationIdentifier("alias1")
	alias2 := values.NamedCorrelationIdentifier("alias2")
	v1 := &values.ConstantValue{Value: int64(10), Typ: values.TypeInt}
	v2 := &values.ConstantValue{Value: int64(20), Typ: values.TypeInt}

	bm.Put(alias1, v1)
	bm.Put(alias2, v2)

	if bm.Len() != 2 {
		t.Fatalf("expected len 2, got %d", bm.Len())
	}

	got, ok := bm.Get(alias1)
	if !ok {
		t.Fatal("Get(alias1) returned false")
	}
	if values.ExplainValue(got) != values.ExplainValue(v1) {
		t.Fatalf("Get(alias1) = %v; want v1", values.ExplainValue(got))
	}

	k, ok := bm.GetInverse(v2)
	if !ok {
		t.Fatal("GetInverse(v2) returned false")
	}
	if k != alias2 {
		t.Fatalf("GetInverse(v2) = %v; want alias2", k)
	}
}

// TestBiMap_StructuralEquality verifies that BiMap uses structural equality
// (via ExplainValue) rather than pointer identity for interface values.
// This is the key behavioral difference from Go's built-in map[interface{}].
func TestBiMap_StructuralEquality(t *testing.T) {
	t.Parallel()

	bm := NewValueBiMap()

	// Create two structurally identical but pointer-different Values.
	v1 := &values.FieldValue{Field: "col1", Typ: values.TypeInt}
	v2 := &values.FieldValue{Field: "col1", Typ: values.TypeInt}

	target := &values.FieldValue{Field: "agg_sum", Typ: values.TypeInt}

	bm.Put(v1, target)

	// Lookup with a DIFFERENT pointer but same structure — must find it.
	got, ok := bm.Get(v2)
	if !ok {
		t.Fatal("structural equality lookup failed — BiMap using pointer identity")
	}
	if got != target {
		t.Fatal("wrong value returned")
	}

	// Inverse lookup with a different pointer to the same structure.
	target2 := &values.FieldValue{Field: "agg_sum", Typ: values.TypeInt}
	invKey, ok := bm.GetInverse(target2)
	if !ok {
		t.Fatal("structural equality inverse lookup failed — BiMap using pointer identity")
	}
	if values.ExplainValue(invKey) != values.ExplainValue(v1) {
		t.Fatalf("GetInverse returned %v, want %v", values.ExplainValue(invKey), values.ExplainValue(v1))
	}
}
