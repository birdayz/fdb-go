package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The group-key output NAME that RecordQueryStreamingAggregationPlan.HintOrdering
// stamps on its advertised keys remains part of the complete accessor path.
//
// RFC-197 item 5 lists `expressions.AggregateKeyColumnName` as a naming authority
// whose consumers must become ordinal before the authority can return a
// display-only carrier type. Two of its consumers are ordinal at runtime and were
// measured so: every name the aggregate cursor writes onto its emitted
// PositionalRow, and every name the plan reported for the UNION position-remap
// (a consumer RFC-242 has since deleted outright), can
// be replaced wholesale by a positional probe string with the entire relational
// corpus — FDB driver suite, yamsql, rowdiff, plandiff, explaindiff, memoinvariant
// — staying green. Only tests that assert the emitted spelling itself notice.
//
// RFC-232 gives the streaming aggregate's PROVIDED key a tagged-current owner,
// while the ORDER BY's independently constructed REQUESTED key remains rooted
// at its named logical quantifier. Their ExplainValue renderings therefore
// differ by design. RichOrdering first tries that display-key fast path, then
// crosses this exact phase boundary through CanBridgeOrderingValueRoots, which
// requires one tagged-current root plus the same complete accessor path,
// ordinal, and result type. It never equates two arbitrary named roots.
//
// Change the spelling on one side and the match misses, the aggregate's group-key
// order stops satisfying the ORDER BY, and the planner stacks a second
// InMemorySort above the aggregate — measured, not predicted: probing the name at
// the HintOrdering site reds the FDB driver suite
// (TestFDB_GroupByHavingOverOrdinalJoin: "want exactly 1 InMemorySort (group-key
// sort, reused by ORDER BY), got 2") and moves the corpus plan-shape golden.
//
// The name/path is still load-bearing here, but correlation rendering is not.
// This test pins both the typed phase bridge and its end-to-end consequence.
func TestStreamingAggregation_ProvidedOrderingMatchesTheSortRebaseSpelling(t *testing.T) {
	t.Parallel()

	// Two group keys sharing a LEAF name across different sources — the shape
	// where a spelling-based match has the most to get wrong, and the shape the
	// `#ordinal` discriminator in the rendering exists to keep apart.
	inputType := values.NewRecordType("", false, []values.Field{
		{Name: "K", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "K2", FieldType: values.NullableLong, Ordinal: 1},
		{Name: "V", FieldType: values.NullableLong, Ordinal: 2},
	})
	qov := mustTestQOV(t, "T", inputType)
	keyA, err := values.ResolveFieldOrdinals(qov, []int{0})
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(0): %v", err)
	}
	keyB, err := values.ResolveFieldOrdinals(qov, []int{1})
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(1): %v", err)
	}
	groupKeys := []values.Value{keyA, keyB}
	aggs := []expressions.AggregateSpec{{Function: expressions.AggCount}}

	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, inputType, false)
	})
	plan := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlan(inner, groupKeys, aggs)
	})
	provided := plan.HintOrdering()
	if !provided.IsKnown || len(provided.Keys) != len(groupKeys) {
		t.Fatalf("HintOrdering = %#v, want %d known keys", provided, len(groupKeys))
	}

	// The REQUESTED side, built exactly as the translator's ORDER BY rebase
	// builds it (cascades_translator.translateSort): the output slot, spelled by
	// GroupByOutputColumnNames — the same authority, reached by a different call.
	outNames := expressions.GroupByOutputColumnNames(groupKeys, aggs)
	if len(outNames) < len(groupKeys) {
		t.Fatalf("GroupByOutputColumnNames = %v, want at least %d entries", outNames, len(groupKeys))
	}

	// (1) The mechanism. Provider and request roots name different phases, so
	// their renderings differ; the narrow typed root bridge must relate them.
	for i := range groupKeys {
		requested := testFieldAt(t, outNames[i], i, values.NullableLong)
		gotProvided := values.ExplainValue(provided.Keys[i])
		gotRequested := values.ExplainValue(requested)
		if gotProvided == gotRequested {
			t.Errorf("ordering key %d unexpectedly erased its phase-root distinction: %q", i, gotProvided)
		}
		if !values.CanBridgeOrderingValueRoots(requested, provided.Keys[i]) {
			t.Errorf("ordering key %d did not bridge named request %q to current provider %q",
				i, gotRequested, gotProvided)
		}
	}

	// (2) The end-to-end consequence, through the real matcher rather than
	// through the rendering it happens to use. Bindings are built the way
	// computeWrapperRichOrdering builds them for a plain (non-counterflow)
	// provided ordering.
	bindings := make(map[values.Value][]properties.OrderingBinding, len(provided.Keys))
	for _, k := range provided.Keys {
		bindings[k] = []properties.OrderingBinding{
			properties.SortedBinding(properties.ProvidedSortOrderAscending),
		}
	}
	rich := properties.NewRichOrdering(bindings, provided.Keys, properties.NotDistinct())

	parts := make([]properties.RequestedOrderingPart, len(groupKeys))
	for i := range groupKeys {
		parts[i] = properties.RequestedOrderingPart{
			Value:     testFieldAt(t, outNames[i], i, values.NullableLong),
			SortOrder: properties.RequestedSortOrderAscending,
		}
	}
	requested := properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)

	if !rich.Satisfies(requested) {
		t.Errorf("the streaming aggregate's provided group-key ordering does NOT satisfy "+
			"an ORDER BY over the same group keys.\nprovided=%v\nrequested=%v\n"+
			"This is the spurious-second-InMemorySort regression: the typed current-to-request "+
			"phase bridge must preserve the aggregate-key accessor path",
			renderKeys(provided.Keys), renderParts(parts))
	}
}

func TestStreamingAggregation_DuplicateGroupKeyNamesMatchLogicalNativeRow(t *testing.T) {
	t.Parallel()
	inputType := values.NewRecordType("", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong, Ordinal: 0},
	})
	inner := mustChecked(t, func() (*RecordQueryScanPlan, error) {
		return NewRecordQueryScanPlan([]string{"T"}, inputType, false)
	})
	innerQ := QuantifierOverPlan(inner)
	inputQOV, err := innerQ.RequireFlowedObjectValue()
	if err != nil {
		t.Fatalf("input flowed object: %v", err)
	}
	key, err := values.ResolveFieldOrdinals(inputQOV, []int{0})
	if err != nil {
		t.Fatalf("grouping key: %v", err)
	}
	keys := []values.Value{key, key}
	aggs := []expressions.AggregateSpec{{Function: expressions.AggCount}}

	logical, err := expressions.NewGroupByExpression(keys, aggs, innerQ)
	if err != nil {
		t.Fatalf("logical group by: %v", err)
	}
	physical := mustChecked(t, func() (*RecordQueryStreamingAggregationPlan, error) {
		return NewRecordQueryStreamingAggregationPlanFromQuantifier(innerQ, keys, aggs)
	})
	if !logical.GetResultValue().Type().Equals(physical.GetResultValue().Type()) {
		t.Fatalf("logical/physical native aggregate rows disagree: %s vs %s",
			logical.GetResultValue().Type(), physical.GetResultValue().Type())
	}
	logicalType := logical.GetResultValue().Type().(*values.RecordType)
	if got := []string{logicalType.Fields[0].Name, logicalType.Fields[1].Name}; got[0] != "ID" || got[1] != "ID" {
		t.Fatalf("private aggregate row de-duplicated positional group keys: %v", got)
	}
}

func renderKeys(ks []values.Value) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = values.ExplainValue(k)
	}
	return out
}

func renderParts(ps []properties.RequestedOrderingPart) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = values.ExplainValue(p.Value)
	}
	return out
}
