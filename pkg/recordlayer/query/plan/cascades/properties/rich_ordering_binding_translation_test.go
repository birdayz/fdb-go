package properties

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRichOrderingPullUpTranslatesFixedComparisonRangeOperand(t *testing.T) {
	t.Parallel()

	key := bindingTranslationField("key")
	comparand := bindingTranslationField("cutoff")
	ordering := bindingTranslationOrdering(t, key, comparand, true)
	upperAlias := values.NamedCorrelationIdentifier("binding_pullup")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: key},
		values.RecordConstructorField{Name: "renamed_cutoff", Value: comparand},
	)

	pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
	pulledKey := values.NewFieldValue(
		values.NewQuantifiedObjectValue(upperAlias),
		"renamed_key",
		values.NullableLong,
	)
	pulledComparand := values.NewFieldValue(
		values.NewQuantifiedObjectValue(upperAlias),
		"renamed_cutoff",
		values.NullableLong,
	)
	bindingTranslationAssertSingleFixedRange(
		t, pulled, pulledKey, pulledComparand, true)
}

func TestRichOrderingPushDownTranslatesFixedComparisonRangeOperand(t *testing.T) {
	t.Parallel()

	lowerKey := bindingTranslationField("key")
	lowerComparand := bindingTranslationField("cutoff")
	upperAlias := values.NamedCorrelationIdentifier("binding_pushdown")
	// The upper references address the constructor's OUTPUT SLOTS: a resolved
	// reference to a projection output carries the ordinal, and push-down selects
	// the member by it (RFC-197 item 3). A lazy carrier here would push down to
	// nothing, which is what the pull-up direction above deliberately still
	// produces and this direction must not depend on.
	upperKey := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(upperAlias), "renamed_key", 0, values.NullableLong,
	)
	upperComparand := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(upperAlias), "renamed_cutoff", 1, values.NullableLong,
	)
	ordering := bindingTranslationOrdering(t, upperKey, upperComparand, false)
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: lowerKey},
		values.RecordConstructorField{Name: "renamed_cutoff", Value: lowerComparand},
	)

	pushed := ordering.PushDownThroughValue(resultValue, upperAlias)
	bindingTranslationAssertSingleFixedRange(
		t, pushed, lowerKey, lowerComparand, false)
}

func TestRichOrderingPullUpTranslatesFixedDirectComparisonOperand(t *testing.T) {
	t.Parallel()

	key := bindingTranslationField("key")
	comparand := bindingTranslationField("cutoff")
	comparison := &predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: comparand,
	}
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {FixedBinding(comparison)},
		},
		[]values.Value{key},
		NotDistinct())
	upperAlias := values.NamedCorrelationIdentifier("binding_direct")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: key},
		values.RecordConstructorField{Name: "renamed_cutoff", Value: comparand},
	)

	pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
	if pulled == nil || len(pulled.GetKeys()) != 1 {
		t.Fatal("expected one translated ordering key")
	}
	bindings := pulled.GetBindingMap()[pulled.GetKeys()[0]]
	if len(bindings) != 1 || !bindings[0].IsFixed() {
		t.Fatalf("translated bindings = %#v, want one fixed binding", bindings)
	}
	translated, ok := bindings[0].GetComparison().(*predicates.Comparison)
	if !ok || translated == nil {
		t.Fatalf("translated comparison = %T, want *predicates.Comparison",
			bindings[0].GetComparison())
	}
	wantComparand := values.NewFieldValue(
		values.NewQuantifiedObjectValue(upperAlias),
		"renamed_cutoff",
		values.NullableLong,
	)
	if !values.ValuesStructurallyEqual(translated.Operand, wantComparand) {
		t.Fatalf("translated fixed comparand = %s, want %s",
			values.ExplainValue(translated.Operand),
			values.ExplainValue(wantComparand))
	}
}

func TestRichOrderingPullUpDropsUntranslatableDynamicFixedBinding(t *testing.T) {
	t.Parallel()

	key := bindingTranslationField("key")
	unprojectedComparand := bindingTranslationField("not_projected")
	ordering := bindingTranslationOrdering(t, key, unprojectedComparand, true)
	upperAlias := values.NamedCorrelationIdentifier("binding_drop")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: key},
	)

	pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
	if pulled == nil {
		t.Fatal("PullUpThroughValue returned nil")
	}
	if len(pulled.GetKeys()) != 0 {
		t.Fatalf("untranslatable dynamic fixed binding retained ordering keys %v",
			bindingTranslationExplainValues(pulled.GetKeys()))
	}
	// An ordering that lost every coordinate cannot still claim its rows are
	// distinct. The claim was proved over `key`; `key` is gone. Java carries
	// its boolean through this branch unexamined (Ordering.java:185-192
	// documents the field as not correct to propagate and leaves the
	// interpretation to the reader), and an earlier revision of this test
	// imported that behaviour and pinned it as REQUIRED. It is not required,
	// it is the bug: the claim is bound to its coordinate set, so a total
	// reduction drops it.
	if pulled.IsDistinct() {
		t.Fatal("pull-up that dropped every ordering key still claims distinct")
	}
}

// TestRichOrderingPrefixDoesNotInheritDistinctness is the shape §5.5 names as
// the one most likely to be got wrong, and the one neither Java nor Go pinned:
// an ordering by (a, b, c) is also an ordering by (a, b), the first may be
// strict, and the second must not inherit that.
//
// The trailing coordinate is what makes the key unique here, so the prefix is
// not merely unproven — it is genuinely non-distinct, and a carried claim would
// be a wrong answer rather than a conservative one.
func TestRichOrderingPrefixDoesNotInheritDistinctness(t *testing.T) {
	t.Parallel()

	a := bindingTranslationField("a")
	b := bindingTranslationField("b")
	c := bindingTranslationField("c")
	full := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			a: {SortedBinding(ProvidedSortOrderAscending)},
			b: {SortedBinding(ProvidedSortOrderAscending)},
			c: {SortedBinding(ProvidedSortOrderAscending)},
		},
		[]values.Value{a, b, c},
		DistinctOverAllKeys())
	if !full.IsDistinct() {
		t.Fatal("the full (a, b, c) ordering must keep the claim it was minted with")
	}

	upperAlias := values.NamedCorrelationIdentifier("prefix_pullup")
	// A projection that carries only (a, b) forward. The uniqueness-making
	// coordinate c has no output slot, so it cannot survive the pull-up.
	prefix := full.PullUpThroughValue(
		values.NewRecordConstructorValue(
			values.RecordConstructorField{Name: "a", Value: a},
			values.RecordConstructorField{Name: "b", Value: b},
		),
		upperAlias)
	if prefix == nil {
		t.Fatal("PullUpThroughValue returned nil")
	}
	if got := len(prefix.GetKeys()); got != 2 {
		t.Fatalf("prefix ordering has %d keys, want 2 (%v)",
			got, bindingTranslationExplainValues(prefix.GetKeys()))
	}
	if prefix.IsDistinct() {
		t.Fatal("(a, b) prefix inherited the (a, b, c) distinctness claim")
	}
}

// TestRichOrderingCarriedClaimFailsUnderKeyRemoval pins the CONSTRUCTOR-level
// rule the derivation sites rely on. Several rules rebuild an ordering from a
// filtered key list and hand the source ordering's claim along —
// ImplementDistinctUnionRule strips the entries common to every leg, and
// ImplementInUnionRule rebuilds with the full key set. Both spell the carry
// identically, and the difference between them is whether keys were dropped, so
// that difference is what has to decide the answer rather than the call site.
func TestRichOrderingCarriedClaimFailsUnderKeyRemoval(t *testing.T) {
	t.Parallel()

	a := bindingTranslationField("a")
	b := bindingTranslationField("b")
	bindings := map[values.Value][]OrderingBinding{
		a: {SortedBinding(ProvidedSortOrderAscending)},
		b: {SortedBinding(ProvidedSortOrderAscending)},
	}
	source := NewRichOrdering(bindings, []values.Value{a, b}, DistinctOverAllKeys())

	rebuiltWhole := NewRichOrdering(
		bindings, []values.Value{a, b}, source.DistinctnessClaim())
	if !rebuiltWhole.IsDistinct() {
		t.Fatal("rebuilding over the SAME key set must keep the carried claim")
	}

	reduced := NewRichOrdering(
		map[values.Value][]OrderingBinding{a: bindings[a]},
		[]values.Value{a},
		source.DistinctnessClaim())
	if reduced.IsDistinct() {
		t.Fatal("rebuilding over a REDUCED key set kept the carried claim")
	}
}

func TestRichOrderingPullUpPreservesNonValueFixedComparands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		operand  values.Value
		expected values.Value
	}{
		{
			name:     "literal",
			operand:  values.LiteralValue(int64(42)),
			expected: values.LiteralValue(int64(42)),
		},
		{
			name:     "parameter",
			operand:  values.NewNamedParameterValue("limit"),
			expected: values.NewNamedParameterValue("limit"),
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			key := bindingTranslationField("key_" + test.name)
			ordering := bindingTranslationOrdering(t, key, test.operand, false)
			upperAlias := values.NamedCorrelationIdentifier("binding_static_" + test.name)
			resultValue := values.NewRecordConstructorValue(
				values.RecordConstructorField{Name: "renamed_key", Value: key},
			)
			pulledKey := values.NewFieldValue(
				values.NewQuantifiedObjectValue(upperAlias),
				"renamed_key",
				values.NullableLong,
			)

			pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
			bindingTranslationAssertSingleFixedRange(
				t, pulled, pulledKey, test.expected, false)
		})
	}
}

func TestRichOrderingPullUpCollapsesDirectionalBindings(t *testing.T) {
	t.Parallel()

	key := bindingTranslationField("key")
	comparand := bindingTranslationField("cutoff")
	comparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: comparand,
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {
				FixedBinding(merged.Range),
				SortedBinding(ProvidedSortOrderDescending),
			},
		},
		[]values.Value{key},
		NotDistinct())
	upperAlias := values.NamedCorrelationIdentifier("binding_directional")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: key},
		values.RecordConstructorField{Name: "renamed_cutoff", Value: comparand},
	)

	pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
	if pulled == nil {
		t.Fatal("PullUpThroughValue returned nil")
	}
	if len(pulled.GetKeys()) != 1 {
		t.Fatalf("translated keys = %v, want one key",
			bindingTranslationExplainValues(pulled.GetKeys()))
	}
	bindings := pulled.GetBindingMap()[pulled.GetKeys()[0]]
	if len(bindings) != 1 ||
		!bindings[0].IsSorted() ||
		bindings[0].GetSortOrder() != ProvidedSortOrderDescending {
		t.Fatalf("translated bindings = %#v, want one descending binding", bindings)
	}
}

func TestRichOrderingPullUpPreservesCounterflowDirectionalBinding(t *testing.T) {
	t.Parallel()

	key := bindingTranslationField("key")
	upperAlias := values.NamedCorrelationIdentifier("binding_counterflow")
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {SortedBinding(ProvidedSortOrderAscendingNullsLast)},
		},
		[]values.Value{key},
		NotDistinct())
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "renamed_key", Value: key},
	)

	pulled := ordering.PullUpThroughValue(resultValue, upperAlias)
	if pulled == nil || len(pulled.GetKeys()) != 1 {
		t.Fatal("expected one translated ordering key")
	}
	bindings := pulled.GetBindingMap()[pulled.GetKeys()[0]]
	if len(bindings) != 1 ||
		bindings[0].GetSortOrder() != ProvidedSortOrderAscendingNullsLast {
		t.Fatalf("translated bindings = %#v, want ASC NULLS LAST", bindings)
	}
}

func TestRichOrderingPullUpDeclinesKeyCollision(t *testing.T) {
	t.Parallel()

	first := &values.FieldValue{Field: "first", Typ: values.UnknownType}
	second := &values.FieldValue{Field: "second", Typ: values.UnknownType}
	ordering := NewRichOrdering(
		map[values.Value][]OrderingBinding{
			first:  {SortedBinding(ProvidedSortOrderAscending)},
			second: {SortedBinding(ProvidedSortOrderDescending)},
		},
		[]values.Value{first, second},
		NotDistinct())
	collapsed := &values.FieldValue{Field: "collapsed", Typ: values.UnknownType}

	pulled := ordering.PullUp(map[string]values.Value{
		values.ExplainValue(first):  collapsed,
		values.ExplainValue(second): collapsed,
	})
	if len(pulled.GetKeys()) != 0 || !pulled.OrderingSet().IsEmpty() {
		t.Fatalf("colliding pull-up retained an ordering: %v", pulled.GetKeys())
	}
}

func bindingTranslationField(name string) values.Value {
	return &values.FieldValue{Field: name, Typ: values.NullableLong}
}

func bindingTranslationOrdering(
	t *testing.T,
	key values.Value,
	comparand values.Value,
	distinct bool,
) *RichOrdering {
	t.Helper()
	comparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: comparand,
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build equality comparison range")
	}
	return NewRichOrdering(
		map[values.Value][]OrderingBinding{
			key: {FixedBinding(merged.Range)},
		},
		[]values.Value{key},
		DistinctOverAllKeysIf(distinct))
}

func bindingTranslationAssertSingleFixedRange(
	t *testing.T,
	ordering *RichOrdering,
	wantKey values.Value,
	wantComparand values.Value,
	wantDistinct bool,
) {
	t.Helper()
	if ordering == nil {
		t.Fatal("translated ordering is nil")
	}
	if ordering.IsDistinct() != wantDistinct {
		t.Fatalf("distinctness = %v, want %v",
			ordering.IsDistinct(), wantDistinct)
	}
	keys := ordering.GetKeys()
	if len(keys) != 1 {
		t.Fatalf("translated keys = %v, want one key",
			bindingTranslationExplainValues(keys))
	}
	if !values.ValuesStructurallyEqual(keys[0], wantKey) {
		t.Fatalf("translated key = %s, want %s",
			values.ExplainValue(keys[0]), values.ExplainValue(wantKey))
	}
	bindings := ordering.GetBindingMap()[keys[0]]
	if len(bindings) != 1 || !bindings[0].IsFixed() {
		t.Fatalf("translated bindings = %#v, want one fixed binding", bindings)
	}
	comparisonRange, ok := bindings[0].GetComparison().(*predicates.ComparisonRange)
	if !ok || comparisonRange == nil || !comparisonRange.IsEquality() {
		t.Fatalf("translated comparison = %T, want equality ComparisonRange",
			bindings[0].GetComparison())
	}
	comparison := comparisonRange.GetEqualityComparison()
	if comparison == nil ||
		!values.ValuesStructurallyEqual(comparison.Operand, wantComparand) {
		var got string
		if comparison != nil {
			got = values.ExplainValue(comparison.Operand)
		}
		t.Fatalf("translated fixed comparand = %s, want %s",
			got, values.ExplainValue(wantComparand))
	}
}

func bindingTranslationExplainValues(items []values.Value) []string {
	explained := make([]string, len(items))
	for i, item := range items {
		explained[i] = values.ExplainValue(item)
	}
	return explained
}
