package cascades

// RFC-232 makes a FieldValue one exact, QOV-rooted full path. These tests pin
// the max-match behavior of fused multi-accessor paths and, just as
// importantly, the admission boundary that prevents the old chained and
// caller-renamed representations from re-entering the matcher.

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func maxMatchResolve(
	t testing.TB,
	child values.Value,
	ordinals ...int,
) values.Value {
	t.Helper()
	resolved, err := values.ResolveFieldOrdinals(child, ordinals)
	return mustConstruct(t, resolved, err)
}

func maxMatchField(
	t testing.TB,
	value values.Value,
	wantPathLen int,
) values.FieldValue {
	t.Helper()
	field, ok := values.AsFieldValue(value)
	if !ok || field.Path() == nil || field.Path().Len() != wantPathLen {
		t.Fatalf("value = %T/%v, want exact FieldValue with %d accessors", value, value, wantPathLen)
	}
	if _, ok := values.AsQuantifiedObjectValue(field.ChildValue()); !ok {
		t.Fatalf("field child = %T, want exact QOV root", field.ChildValue())
	}
	return field
}

// w3FusedRef builds the exact two-step m[slot][legOrd] reference the
// partition-rule rebase produces.
func w3FusedRef(
	t testing.TB,
	mergeQOV values.QuantifiedObjectValue,
	slot, legOrd int,
) values.FieldValue {
	t.Helper()
	return maxMatchField(t, maxMatchResolve(t, mergeQOV, slot, legOrd), 2)
}

func w3MergeQOV(t testing.TB, name string) values.QuantifiedObjectValue {
	t.Helper()
	leg := values.NewRecordType("Leg", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
		{Name: "Y", FieldType: values.NotNullLong, Ordinal: 1},
	})
	merged := values.NewRecordType("Merged", false, []values.Field{
		{Name: values.OrdinalFieldName(0), FieldType: leg, Ordinal: 0},
	})
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(name),
		merged,
	)
	return mustConstruct(t, qov, err)
}

func TestMaxMatchMap_FusedVsFused(t *testing.T) {
	t.Parallel()
	m := w3MergeQOV(t, "m")
	qv := w3FusedRef(t, m, 0, 1)
	cv := w3FusedRef(t, m, 0, 1)
	if mmm := ComputeMaxMatchMap(qv, cv, nil); mmm.Size() < 1 {
		t.Fatal("identical exact fused paths must max-match")
	}

	// Display names are derived from the exact root; a caller cannot publish a
	// same-ordinal clone carrying invented names and bypass that identity.
	request, err := values.FieldByNameAndOrdinal("RENAMED", 1)
	request = mustConstruct(t, request, err)
	if renamed, resolveErr := values.ResolveFieldAccess(
		maxMatchResolve(t, m, 0),
		[]values.FieldRequest{request},
	); resolveErr == nil || renamed != nil {
		t.Fatalf("invented display name unexpectedly published a fused field: (%v, %v)", renamed, resolveErr)
	}
}

// Resolving one step over an already-resolved exact field composes into the
// canonical full path immediately. There is no admitted chained FieldValue
// alternative for matching to expand.
func TestMaxMatchMap_FusedVsChained(t *testing.T) {
	t.Parallel()
	m := w3MergeQOV(t, "m")
	direct := w3FusedRef(t, m, 0, 1)
	step0 := maxMatchResolve(t, m, 0)
	sequential := maxMatchField(t, maxMatchResolve(t, step0, 1), 2)
	if mmm := ComputeMaxMatchMap(direct, sequential, nil); mmm.Size() < 1 {
		t.Fatal("direct and sequentially resolved canonical paths must match")
	}
}

func maxMatchNestedQOV(
	t testing.TB,
	name string,
	depth int,
) values.QuantifiedObjectValue {
	t.Helper()
	var typ values.Type = values.NotNullLong
	for level := depth - 1; level >= 0; level-- {
		fieldName := values.OrdinalFieldName(0)
		if level == depth-1 {
			fieldName = "C"
		}
		typ = values.NewRecordType("Nested", false, []values.Field{
			{Name: fieldName, FieldType: typ, Ordinal: 0},
		})
	}
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier(name),
		typ,
	)
	return mustConstruct(t, qov, err)
}

func maxMatchSequentialZeroPath(
	t testing.TB,
	root values.Value,
	depth int,
) values.FieldValue {
	t.Helper()
	current := root
	for i := 0; i < depth; i++ {
		current = maxMatchResolve(t, current, 0)
	}
	return maxMatchField(t, current, depth)
}

func TestMaxMatchMap_FusedVsChained_ThreeAccessor(t *testing.T) {
	t.Parallel()
	q := maxMatchNestedQOV(t, "m", 3)
	direct := maxMatchField(t, maxMatchResolve(t, q, 0, 0, 0), 3)
	sequential := maxMatchSequentialZeroPath(t, q, 3)
	if mmm := ComputeMaxMatchMap(direct, sequential, nil); mmm.Size() < 1 {
		t.Fatal("three-step direct and sequential canonical paths must match")
	}
}

func TestMaxMatchMap_FusedVsOneStepSplit(t *testing.T) {
	t.Parallel()
	q := maxMatchNestedQOV(t, "m", 3)
	direct := maxMatchField(t, maxMatchResolve(t, q, 0, 0, 0), 3)
	prefix := maxMatchField(t, maxMatchResolve(t, q, 0, 0), 2)
	resolvedOuter := maxMatchField(t, maxMatchResolve(t, prefix, 0), 3)
	if mmm := ComputeMaxMatchMap(direct, resolvedOuter, nil); mmm.Size() < 1 {
		t.Fatal("resolving a suffix over a fused prefix must remain matchable")
	}
}

func TestMaxMatchMap_FusedVsMidSplit_FourAccessor(t *testing.T) {
	t.Parallel()
	q := maxMatchNestedQOV(t, "m", 4)
	direct := maxMatchField(t, maxMatchResolve(t, q, 0, 0, 0, 0), 4)
	prefix := maxMatchField(t, maxMatchResolve(t, q, 0, 0), 2)
	mid := maxMatchField(t, maxMatchResolve(t, prefix, 0), 3)
	resolvedOuter := maxMatchField(t, maxMatchResolve(t, mid, 0), 4)
	if mmm := ComputeMaxMatchMap(direct, resolvedOuter, nil); mmm.Size() < 1 {
		t.Fatal("four-step direct and incrementally resolved canonical paths must match")
	}
}
