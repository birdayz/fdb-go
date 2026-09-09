package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// aggregateOrderingBase is the record the grouping columns resolve against.
func aggregateOrderingBase() *values.RecordType {
	return values.NewRecordType("agg_ordering_base", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "A", FieldType: values.NullableLong, Ordinal: 2},
		{Name: "D", FieldType: values.NullableDouble, Ordinal: 3},
	})
}

// aggregateOrderingRow is the row an aggregate index scan flows:
// [groupCols..., FUNC(col)].
func aggregateOrderingRow(groupCols []string, groupTypes []values.Type) *values.RecordType {
	fields := make([]values.Field, 0, len(groupCols)+1)
	for i, col := range groupCols {
		fields = append(fields, values.Field{Name: col, FieldType: groupTypes[i], Ordinal: i})
	}
	fields = append(fields, values.Field{Name: "COUNT(*)", FieldType: values.NotNullLong, Ordinal: len(groupCols)})
	return values.NewRecordType("agg_ordering_row", false, fields)
}

// aggregateOrderingPlan builds a grouped COUNT(*) aggregate scan over the
// grouping columns, bound by comps on the leading coordinates.
func aggregateOrderingPlan(t *testing.T, groupCols []string, groupTypes []values.Type, comps []*predicates.ComparisonRange, reverse bool) *RecordQueryAggregateIndexPlan {
	t.Helper()
	index := mustChecked(t, func() (*RecordQueryIndexPlan, error) {
		return NewRecordQueryIndexPlan("AGG_IDX", comps, []string{"T"}, aggregateOrderingRow(groupCols, groupTypes), reverse)
	}).WithKeyComponentTypes(groupTypes)
	agg, err := NewRecordQueryAggregateIndexPlan(index, "T", aggregateOrderingRow(groupCols, groupTypes), "COUNT")
	if err != nil {
		t.Fatalf("aggregate index plan: %v", err)
	}
	return agg.WithGroupColumns(groupCols, "").WithGroupColumnLayout(aggregateOrderingBase())
}

func orderingKeyNames(t *testing.T, keys []values.Value) []string {
	t.Helper()
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		fv, ok := values.AsFieldValue(k)
		if !ok {
			t.Fatalf("ordering key %v is not a field", k)
		}
		out = append(out, fv.DisplayName())
	}
	return out
}

func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var twoLongs = []values.Type{values.NullableLong, values.NullableLong}

// TestAggregateIndexPlan_HintOrdering_Unbound: with no bound prefix every
// grouping column is a sorted key, in scan direction.
func TestAggregateIndexPlan_HintOrdering_Unbound(t *testing.T) {
	t.Parallel()
	plan := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs, nil, false)
	got := plan.HintOrdering()
	if !got.IsKnown || !sameNames(orderingKeyNames(t, got.Keys), []string{"B", "A"}) {
		t.Fatalf("HintOrdering(unbound) = %#v, want [B, A]", got)
	}
	rev := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs, nil, true).HintOrdering()
	if !rev.DescendingAt(0) || !rev.DescendingAt(1) {
		t.Fatalf("HintOrdering(unbound, reverse) = %#v, want descending", rev)
	}
}

// TestAggregateIndexPlan_HintOrdering_EqualityPrefixDropped is the aggregate
// twin of TestRecordQueryIndexPlan_HintOrdering_EqualityPrefixDropped: a
// grouping column the scan binds by equality contributes no sort position, so
// `WHERE b = 1 GROUP BY b, a ORDER BY a` is satisfied by the scan. Before this
// pin the plan claimed [B, A] and the planner sorted the groups in memory.
func TestAggregateIndexPlan_HintOrdering_EqualityPrefixDropped(t *testing.T) {
	t.Parallel()
	plan := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs,
		[]*predicates.ComparisonRange{pkOrderingEq(t, int64(1))}, false)
	got := plan.HintOrdering()
	if !got.IsKnown || !sameNames(orderingKeyNames(t, got.Keys), []string{"A"}) {
		t.Fatalf("HintOrdering(b = 1) = %#v, want [A] — the bound grouping column must not occupy a sort position", got)
	}
	fv, _ := values.AsFieldValue(got.Keys[0])
	if ord := fv.Path().Ordinals()[0]; ord != 1 {
		t.Fatalf("A resolves to ordinal %d of the flowed row, want 1: the key must keep its slot after the prefix is dropped", ord)
	}

	rich := plan.HintRichOrdering()
	if !sameNames(orderingKeyNames(t, rich.GetKeys()), []string{"B", "A"}) {
		t.Fatalf("HintRichOrdering(b = 1) keys = %v, want [B, A] with B fixed", orderingKeyNames(t, rich.GetKeys()))
	}
	bm := rich.GetBindingMap()
	if b := bm[rich.GetKeys()[0]]; len(b) != 1 || !b[0].IsFixed() {
		t.Fatalf("B binding = %v, want FIXED to the equality", b)
	}
	if a := bm[rich.GetKeys()[1]]; len(a) != 1 || a[0].IsFixed() {
		t.Fatalf("A binding = %v, want SORTED", a)
	}
	if eq := rich.GetEqualityBoundValues(); len(eq) != 1 {
		t.Fatalf("equality-bound values = %d, want exactly B", len(eq))
	}
}

// TestAggregateIndexPlan_HintOrdering_EveryColumnBound: a scan that pins every
// grouping column flows at most one group — known and ordered by anything.
func TestAggregateIndexPlan_HintOrdering_EveryColumnBound(t *testing.T) {
	t.Parallel()
	plan := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs,
		[]*predicates.ComparisonRange{pkOrderingEq(t, int64(1)), pkOrderingEq(t, int64(2))}, false)
	got := plan.HintOrdering()
	if !got.IsKnown || len(got.Keys) != 0 {
		t.Fatalf("HintOrdering(b = 1, a = 2) = %#v, want known with no keys", got)
	}
	rich := plan.HintRichOrdering()
	for _, k := range rich.GetKeys() {
		if b := rich.GetBindingMap()[k]; len(b) != 1 || !b[0].IsFixed() {
			t.Fatalf("binding of %v = %v, want FIXED", k, b)
		}
	}
}

// TestAggregateIndexPlan_HintOrdering_RangeBoundPrefixStaysSorted: an
// inequality on the leading grouping column pins nothing, so every column
// stays a sorted key exactly as for an unbound scan.
func TestAggregateIndexPlan_HintOrdering_RangeBoundPrefixStaysSorted(t *testing.T) {
	t.Parallel()
	plan := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs,
		[]*predicates.ComparisonRange{pkOrderingGT(t, int64(1))}, false)
	got := plan.HintOrdering()
	if !got.IsKnown || !sameNames(orderingKeyNames(t, got.Keys), []string{"B", "A"}) {
		t.Fatalf("HintOrdering(b > 1) = %#v, want [B, A]", got)
	}
}

// TestAggregateIndexPlan_HintOrdering_FloatPrefix carries the value index's
// float rules onto the grouping key: a DOUBLE grouping column pinned to one
// physical key (d = 1.0) is FIXED and the columns after it stay claimable; a
// signed-zero equality (d = 0.0) spans two physical keys, keeps its own order
// and drops the tail; a DOUBLE in the sorted tail terminates the claim.
func TestAggregateIndexPlan_HintOrdering_FloatPrefix(t *testing.T) {
	t.Parallel()
	doubleThenLong := []values.Type{values.NullableDouble, values.NullableLong}

	pinned := aggregateOrderingPlan(t, []string{"D", "A"}, doubleThenLong,
		[]*predicates.ComparisonRange{pkOrderingEq(t, float64(1))}, false).HintOrdering()
	if !pinned.IsKnown || !sameNames(orderingKeyNames(t, pinned.Keys), []string{"A"}) {
		t.Fatalf("HintOrdering(d = 1.0) = %#v, want [A]: a float pinned to one key is harmless as a fixed prefix", pinned)
	}

	zero := aggregateOrderingPlan(t, []string{"D", "A"}, doubleThenLong,
		[]*predicates.ComparisonRange{pkOrderingEq(t, float64(0))}, false)
	if got := zero.HintOrdering(); got.IsKnown {
		t.Fatalf("HintOrdering(d = 0.0) = %#v, want unknown: the equality spans both signed zeros so nothing after it is globally ordered", got)
	}
	rich := zero.HintRichOrdering()
	if !sameNames(orderingKeyNames(t, rich.GetKeys()), []string{"D"}) {
		t.Fatalf("HintRichOrdering(d = 0.0) keys = %v, want [D] alone — the tail is dropped, the prefix keeps its own order", orderingKeyNames(t, rich.GetKeys()))
	}
	if b := rich.GetBindingMap()[rich.GetKeys()[0]]; len(b) != 1 || b[0].IsFixed() {
		t.Fatalf("D binding under a signed-zero equality = %v, want SORTED (two distinct sort values), not FIXED", b)
	}

	floatTail := aggregateOrderingPlan(t, []string{"B", "D"}, []values.Type{values.NullableLong, values.NullableDouble},
		[]*predicates.ComparisonRange{pkOrderingEq(t, int64(1))}, false).HintOrdering()
	if floatTail.IsKnown {
		t.Fatalf("HintOrdering(b = 1, tail D) = %#v, want unknown: a DOUBLE in the sorted tail terminates the claim", floatTail)
	}
}

// TestAggregateIndexPlan_HintRichOrdering_UntypedOperandOnDoubleIsNotFixed:
// a DOUBLE grouping column bound by an UNKNOWN-typed non-constant operand (an
// IN binding, a parameter) may be zero at runtime and then widens across both
// signed-zero blocks, so it pins no single physical key. The plain form
// already declines to claim past it; the rich form must bind it SORTED (own
// order, scan direction), never FIXED — FIXED says "any requested direction is
// satisfied", which would let `ORDER BY d DESC` elide its sort against a
// forward scan that emits -0.0 before +0.0. The same coordinate on a LONG
// column IS fixed: no signed zero exists there, whatever the operand.
func TestAggregateIndexPlan_HintRichOrdering_UntypedOperandOnDoubleIsNotFixed(t *testing.T) {
	t.Parallel()
	untyped := plannerDynamicEquality(t, values.UnknownType)

	double := aggregateOrderingPlan(t, []string{"D", "A"}, []values.Type{values.NullableDouble, values.NullableLong},
		[]*predicates.ComparisonRange{untyped}, false)
	if got := double.HintOrdering(); got.IsKnown {
		t.Fatalf("HintOrdering(d = ?) = %#v, want unknown: the operand may be zero and the tail restarts at each block", got)
	}
	rich := double.HintRichOrdering()
	if !sameNames(orderingKeyNames(t, rich.GetKeys()), []string{"D"}) {
		t.Fatalf("HintRichOrdering(d = ?) keys = %v, want [D] alone", orderingKeyNames(t, rich.GetKeys()))
	}
	if b := rich.GetBindingMap()[rich.GetKeys()[0]]; len(b) != 1 || b[0].IsFixed() {
		t.Fatalf("D bound by an untyped operand = %v, want SORTED: the operand-only reading would call it FIXED", b)
	}

	long := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs,
		[]*predicates.ComparisonRange{untyped}, false)
	if got := long.HintOrdering(); !got.IsKnown || !sameNames(orderingKeyNames(t, got.Keys), []string{"A"}) {
		t.Fatalf("HintOrdering(b = ? on a LONG) = %#v, want [A]", got)
	}
	richLong := long.HintRichOrdering()
	if b := richLong.GetBindingMap()[richLong.GetKeys()[0]]; len(b) != 1 || !b[0].IsFixed() {
		t.Fatalf("B (LONG) bound by an untyped operand = %v, want FIXED", b)
	}
}

// TestAggregateIndexPlan_HintOrdering_ReverseWithPrefix: under a reverse scan
// the sorted tail descends while the fixed prefix stays direction-free.
func TestAggregateIndexPlan_HintOrdering_ReverseWithPrefix(t *testing.T) {
	t.Parallel()
	plan := aggregateOrderingPlan(t, []string{"B", "A"}, twoLongs,
		[]*predicates.ComparisonRange{pkOrderingEq(t, int64(1))}, true)
	got := plan.HintOrdering()
	if !got.IsKnown || !sameNames(orderingKeyNames(t, got.Keys), []string{"A"}) || !got.DescendingAt(0) {
		t.Fatalf("HintOrdering(b = 1, reverse) = %#v, want [A] descending", got)
	}
	rich := plan.HintRichOrdering()
	bm := rich.GetBindingMap()
	if b := bm[rich.GetKeys()[0]]; len(b) != 1 || !b[0].IsFixed() {
		t.Fatalf("B binding under a reverse scan = %v, want FIXED (direction-free)", b)
	}
	if a := bm[rich.GetKeys()[1]]; len(a) != 1 || a[0].IsFixed() || !a[0].GetSortOrder().IsAnyDescending() {
		t.Fatalf("A binding under a reverse scan = %v, want SORTED descending", a)
	}
}
