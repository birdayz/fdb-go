package plans

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// The group-key output NAME that RecordQueryStreamingAggregationPlan.HintOrdering
// stamps on its advertised keys is NOT a display label. It is half of the ordering
// MATCH KEY, and dropping it costs a plan.
//
// RFC-197 item 5 lists `expressions.AggregateKeyColumnName` as a naming authority
// whose consumers must become ordinal before the authority can return a
// display-only carrier type. Two of its consumers are ordinal at runtime and were
// measured so: every name the aggregate cursor writes onto its emitted
// PositionalRow, and every name the plan reports for the UNION position-remap, can
// be replaced wholesale by a positional probe string with the entire relational
// corpus — FDB driver suite, yamsql, rowdiff, plandiff, explaindiff, memoinvariant
// — staying green. Only tests that assert the emitted spelling itself notice.
//
// This consumer is different, and this file exists because it was flagged as
// "possibly display-only" and is not. `properties.RichOrdering` addresses its
// ordering set by `values.ExplainValue` — a RENDERED STRING (rich_ordering.go,
// orderingKeyFor). The streaming aggregate's PROVIDED key and the ORDER BY's
// REQUESTED key are two independently constructed FieldValues that meet only as
// that rendering, and they render alike solely because both sides derive the
// spelling from this one authority: HintOrdering through AggregateKeyColumnName,
// the translator's sort rebase through GroupByOutputColumnNames, which is the same
// function over the same keys.
//
// Change the spelling on one side and the match misses, the aggregate's group-key
// order stops satisfying the ORDER BY, and the planner stacks a second
// InMemorySort above the aggregate — measured, not predicted: probing the name at
// the HintOrdering site reds the FDB driver suite
// (TestFDB_GroupByHavingOverOrdinalJoin: "want exactly 1 InMemorySort (group-key
// sort, reused by ORDER BY), got 2") and moves the corpus plan-shape golden.
//
// So the name is load-bearing here, and this test is what re-arms if someone
// makes AggregateKeyColumnName return a display-only carrier without first
// converting the ordering property to a structural (ordinal + domain) match.
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
	qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier("T"), inputType)
	keyA, err := values.NewFieldValueOfOrdinal(qov, 0)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(0): %v", err)
	}
	keyB, err := values.NewFieldValueOfOrdinal(qov, 1)
	if err != nil {
		t.Fatalf("NewFieldValueOfOrdinal(1): %v", err)
	}
	groupKeys := []values.Value{keyA, keyB}
	aggs := []expressions.AggregateSpec{{Function: expressions.AggCount}}

	plan := NewRecordQueryStreamingAggregationPlan(nil, groupKeys, aggs)
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

	// (1) The mechanism. RichOrdering.orderingKeyFor's exact-match arm compares
	// values.ExplainValue renderings, so the two sides must render identically.
	for i := range groupKeys {
		requested := values.NewFieldValueWithResolvedOrdinal(outNames[i], i, values.UnknownType)
		gotProvided := values.ExplainValue(provided.Keys[i])
		gotRequested := values.ExplainValue(requested)
		if gotProvided != gotRequested {
			t.Errorf("ordering key %d renders as %q provided vs %q requested — "+
				"RichOrdering matches these two independently built values BY THIS STRING, "+
				"so a divergence here silently stops the aggregate's group-key order from "+
				"satisfying an ORDER BY over the same key and stacks a second sort. "+
				"Both spellings must keep coming from expressions.AggregateKeyColumnName",
				i, gotProvided, gotRequested)
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
	rich := properties.NewRichOrdering(bindings, provided.Keys, false)

	parts := make([]properties.RequestedOrderingPart, len(groupKeys))
	for i := range groupKeys {
		parts[i] = properties.RequestedOrderingPart{
			Value:     values.NewFieldValueWithResolvedOrdinal(outNames[i], i, values.UnknownType),
			SortOrder: properties.RequestedSortOrderAscending,
		}
	}
	requested := properties.NewRequestedOrdering(parts, properties.DistinctnessPreserveDistinctness, false)

	if !rich.Satisfies(requested) {
		t.Errorf("the streaming aggregate's provided group-key ordering does NOT satisfy "+
			"an ORDER BY over the same group keys.\nprovided=%v\nrequested=%v\n"+
			"This is the spurious-second-InMemorySort regression: the two sides are matched "+
			"by rendered string, and they only agree because both spell the key through "+
			"expressions.AggregateKeyColumnName",
			renderKeys(provided.Keys), renderParts(parts))
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
