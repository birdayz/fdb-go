package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

func matchCandidateQOV(
	t testing.TB,
	alias values.CorrelationIdentifier,
	rowType values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(alias, rowType)
	return mustConstruct(t, qov, err)
}

func matchCandidateField(
	t testing.TB,
	alias values.CorrelationIdentifier,
	rowType values.Type,
	ordinals []int,
	frontierPinned bool,
) values.Value {
	t.Helper()
	root := matchCandidateQOV(t, alias, rowType)
	var field values.Value
	var err error
	if frontierPinned {
		if len(ordinals) != 1 {
			t.Fatalf("frontier-pinned match-candidate field needs one ordinal, got %v", ordinals)
		}
		field, err = values.ResolveOrdinalSeedField(root, ordinals[0])
	} else {
		field, err = values.ResolveFieldOrdinals(root, ordinals)
	}
	return mustConstruct(t, field, err)
}

func TestValueIndexScanMatchCandidate_PrefixMap_AllEquality(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	c := newKnownDistinctValueIndexCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		testRecordRowType("Order", "STATUS", "DATE"),
		false,
		nil,
	)
	eq1 := predicates.EmptyComparisonRange()
	eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
	eq2 := predicates.EmptyComparisonRange()
	eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))}).Range,
		a2: eq2.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(2))}).Range,
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 2 {
		t.Fatalf("expected 2 prefix entries, got %d", len(prefix))
	}
}

func TestValueIndexScanMatchCandidate_PrefixMap_StopsAtEmpty(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	c := newKnownDistinctValueIndexCandidate(
		"idx",
		[]string{"T"},
		[]string{"A", "B", "C"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		testRecordRowType("T", "A", "B", "C"),
		false,
		nil,
	)
	eq1 := predicates.EmptyComparisonRange()
	res := eq1.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: res.Range,
		// a2 is unbound — prefix should stop here
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 1 {
		t.Fatalf("expected 1 prefix entry (stop at unbound a2), got %d", len(prefix))
	}
}

func TestValueIndexScanMatchCandidate_PrefixMap_StopsAfterInequality(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	a3 := values.UniqueCorrelationIdentifier()
	c := newKnownDistinctValueIndexCandidate(
		"idx",
		[]string{"T"},
		[]string{"A", "B", "C"},
		[]values.CorrelationIdentifier{a1, a2, a3},
		testRecordRowType("T", "A", "B", "C"),
		false,
		nil,
	)
	eq := predicates.EmptyComparisonRange()
	eqRes := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})
	ineq := predicates.EmptyComparisonRange()
	ineqRes := ineq.Merge(&predicates.Comparison{Type: predicates.ComparisonGreaterThan, Operand: values.LiteralValue(int64(5))})
	eq3 := predicates.EmptyComparisonRange()
	eq3Res := eq3.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(9))})

	bindings := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eqRes.Range,
		a2: ineqRes.Range,
		a3: eq3Res.Range, // should NOT be in prefix (after inequality)
	}
	prefix := c.ComputeBoundParameterPrefixMap(bindings)
	if len(prefix) != 2 {
		t.Fatalf("expected 2 prefix entries (eq + ineq, stop before a3), got %d", len(prefix))
	}
	if _, ok := prefix[a3]; ok {
		t.Fatal("a3 should NOT be in prefix — it's after the inequality")
	}
}

func TestValueIndexScanMatchCandidate_ToScanPlan(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	c := newKnownDistinctValueIndexCandidate(
		"Order$status",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		testRecordRowType("Order", "STATUS", "DATE"),
		false,
		nil,
	)
	eq := predicates.EmptyComparisonRange()
	eqRes := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue(int64(1))})

	prefix := map[values.CorrelationIdentifier]*predicates.ComparisonRange{
		a1: eqRes.Range,
	}
	plan := c.ToScanPlan(prefix, false)
	fetchPlan, ok := plan.(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("expected *RecordQueryFetchFromPartialRecordPlan, got %T", plan)
	}
	idxPlan, ok := fetchPlan.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("expected inner *RecordQueryIndexPlan, got %T", fetchPlan.GetInner())
	}
	if idxPlan.GetIndexName() != "Order$status" {
		t.Fatalf("index name=%q, want Order$status", idxPlan.GetIndexName())
	}
	comps := idxPlan.GetScanComparisons()
	if len(comps) != 2 {
		t.Fatalf("expected 2 scan comparisons (one per column), got %d", len(comps))
	}
	if !comps[0].IsEquality() {
		t.Fatal("first comparison should be equality")
	}
	if !comps[1].IsEmpty() {
		t.Fatal("second comparison should be empty (unbound)")
	}
}

// TestValueIndexScanMatchCandidate_PushValueThroughFetch_ChainedFieldDeclines pins
// that PushValueThroughFetch translates ONLY a top-level bare field over the source
// quantifier by covered-column name. A chained accessor (ADDR.CITY) whose LEAF name
// collides with a covered top-level column (CITY) must NOT translate to a flat read
// of the index entry's CITY column — that is a silent wrong-rows/wrong-value bug.
//
// Java ref: ScanWithFetchMatchCandidate.pushValueThroughFetch matches the WHOLE value
// tree (accessor chain included) against a provided index Value via semanticEquals and
// accepts only when no source correlation remains; a chained ADDR.CITY does not
// semantically equal the flat top-level CITY, so it is rejected.
//
// Revert-proof: before the fix the FieldValue arm matched by leaf name only and
// rebuilt a flat FieldValue{CITY, QOV(TGT)}, silently dropping the ADDR accessor —
// ok==true with the wrong value. After the fix the chained shape returns ok==false,
// while the legitimate bare shape still translates.
func TestValueIndexScanMatchCandidate_PushValueThroughFetch_ChainedFieldDeclines(t *testing.T) {
	t.Parallel()

	src := values.NamedCorrelationIdentifier("SRC")
	tgt := values.NamedCorrelationIdentifier("TGT")
	sarg := values.UniqueCorrelationIdentifier()
	addressType := values.NewRecordType("Address", false, []values.Field{
		{Name: "CITY", FieldType: values.NullableLong, Ordinal: 0},
	})
	rowType := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ADDR", FieldType: addressType, Ordinal: 1},
		{Name: "CITY", FieldType: values.NullableLong, Ordinal: 2},
	})
	c := newKnownDistinctValueIndexCandidate(
		"idx_city",
		[]string{"T"},
		[]string{"CITY"},
		[]values.CorrelationIdentifier{sarg},
		rowType,
		false,
		nil,
	)

	// Chained ADDR.CITY is one exact, fused two-step access rooted at QOV(SRC).
	// Its leaf name collides with the covered top-level column but its full path
	// denotes a different value, so it must decline.
	chained := matchCandidateField(t, src, rowType, []int{1, 0}, false)
	got, ok := c.PushValueThroughFetch(chained, src, tgt)
	if ok {
		t.Fatalf("chained ADDR.CITY must NOT push through fetch (leaf-name collision), got ok=true value=%v", got)
	}

	// Bare top-level CITY over the source: FieldValue{CITY, Child: QOV(SRC)} — the
	// legitimate case that must STILL translate (guard against over-restriction).
	bare := matchCandidateField(t, src, rowType, []int{2}, false)
	gotBare, okBare := c.PushValueThroughFetch(bare, src, tgt)
	if !okBare {
		t.Fatal("bare top-level CITY over the source must push through fetch")
	}
	fv, isFV := values.AsFieldValue(gotBare)
	if !isFV || fv.DisplayName() != "CITY" {
		t.Fatalf("translated value=%v, want FieldValue{CITY, ...}", gotBare)
	}
	childQOV, isQOV := values.AsQuantifiedObjectValue(fv.ChildValue())
	if !isQOV || childQOV.Correlation() != tgt {
		t.Fatalf("translated child=%v, want QOV(TGT)", fv.ChildValue())
	}

	// The machinery-owned, frontier-pinned form is still QOV-rooted under
	// RFC-232. It must translate without losing the exact ordinal address.
	baked := matchCandidateField(t, src, rowType, []int{2}, true)
	gotBaked, okBaked := c.PushValueThroughFetch(baked, src, tgt)
	if !okBaked {
		t.Fatal("frontier-pinned bare CITY must push through fetch")
	}
	if bfv, isFV := values.AsFieldValue(gotBaked); !isFV || bfv.DisplayName() != "CITY" {
		t.Fatalf("translated baked value=%v, want FieldValue{CITY, ...}", gotBaked)
	}

	// RFC-232 closes the old non-QOV composite-child escape hatch at construction:
	// such a node cannot enter PushValueThroughFetch and therefore cannot have its
	// child silently discarded by this rewrite.
	if invalid, err := values.ResolveFieldOrdinals(
		&values.ConstantValue{Value: int64(1), Typ: values.NotNullLong}, []int{0},
	); err == nil || invalid != nil {
		t.Fatalf("non-QOV scalar unexpectedly published a FieldValue: (%v, %v)", invalid, err)
	}
}

func TestValueIndexScanMatchCandidate_FanOutElementIsNotCoveredAsArray(t *testing.T) {
	t.Parallel()

	tagAlias := values.UniqueCorrelationIdentifier()
	scoreAlias := values.UniqueCorrelationIdentifier()
	reportedNoDuplicates := false
	itemRowType := values.NewRecordType("Item", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 1},
		{Name: "SCORE", FieldType: values.NullableLong, Ordinal: 2},
	})
	fanOutCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_tags_score",
		[]string{"Item"},
		[]string{"TAGS", "SCORE"},
		nil,
		[]values.CorrelationIdentifier{tagAlias, scoreAlias},
		itemRowType,
		false,
		[]string{"ID"},
		&reportedNoDuplicates,
	).WithRootKeyExpression(&gen.KeyExpression{
		Then: &gen.Then{Child: []*gen.KeyExpression{
			candidateTestKeyField("TAGS", gen.Field_FAN_OUT),
			candidateTestKeyField("SCORE", gen.Field_SCALAR),
		}},
	}).WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableLong})
	if !fanOutCandidate.CreatesDuplicates() {
		t.Fatal("structured FAN_OUT must override a stale false duplicate signal")
	}
	if signal := fanOutCandidate.DistinctRecordsSignal(); signal == nil || !*signal {
		t.Fatalf("structured FAN_OUT distinct signal = %v, want true", signal)
	}

	missingSignalCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_tags",
		[]string{"Item"},
		[]string{"TAGS"},
		nil,
		[]values.CorrelationIdentifier{values.UniqueCorrelationIdentifier()},
		itemRowType,
		false,
		nil,
		nil,
	).WithRootKeyExpression(candidateTestKeyField("TAGS", gen.Field_FAN_OUT))
	if !missingSignalCandidate.CreatesDuplicates() {
		t.Fatal("structured FAN_OUT must establish duplicates when the optional signal is missing")
	}
	if signal := missingSignalCandidate.DistinctRecordsSignal(); signal == nil || !*signal {
		t.Fatalf("missing external distinct signal with FAN_OUT root = %v, want true", signal)
	}

	sourceAlias := values.NamedCorrelationIdentifier("source")
	targetAlias := values.NamedCorrelationIdentifier("target")
	// The covering question is asked in ordinals now, so a probe carries the
	// ordinal AND the layout it indexes — the shape the resolver mints.
	field := func(name string) values.Value {
		for ordinal, declared := range itemRowType.Fields {
			if declared.Name == name {
				return matchCandidateField(t, sourceAlias, itemRowType, []int{ordinal}, false)
			}
		}
		t.Fatalf("test row has no field %q", name)
		return nil
	}

	if translated, ok := fanOutCandidate.PushValueThroughFetch(
		field("TAGS"),
		sourceAlias,
		targetAlias,
	); ok {
		t.Fatalf(
			"fan-out element must not cover the original TAGS array, got %v",
			translated,
		)
	}
	if translated, ok := fanOutCandidate.PushValueThroughFetch(
		matchCandidateField(t, sourceAlias, itemRowType, []int{1}, true),
		sourceAlias,
		targetAlias,
	); ok {
		t.Fatalf(
			"baked TAGS array must not be translated from one exploded element, got %v",
			translated,
		)
	}

	for _, coveredColumn := range []string{"SCORE", "ID"} {
		translated, ok := fanOutCandidate.PushValueThroughFetch(
			field(coveredColumn),
			sourceAlias,
			targetAlias,
		)
		if !ok {
			t.Fatalf(
				"ordinary covered column %s was rejected on a fan-out index",
				coveredColumn,
			)
		}
		translatedField, ok := values.AsFieldValue(translated)
		if !ok || translatedField.DisplayName() != coveredColumn {
			t.Fatalf(
				"translated %s = %#v, want the same field over target",
				coveredColumn,
				translated,
			)
		}
	}

	// Predicate binding is intentionally independent of covering. An exact
	// exploded-element comparison must still form the scan prefix.
	equality := predicates.NewLiteralComparison(
		predicates.ComparisonEquals,
		"blue",
	)
	equalityRange := predicates.EmptyComparisonRange().Merge(&equality).Range
	prefix := fanOutCandidate.ComputeBoundParameterPrefixMap(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			tagAlias: equalityRange,
		},
	)
	if got := prefix[tagAlias]; got != equalityRange {
		t.Fatalf("exact fan-out element binding was not retained in scan prefix: %v", prefix)
	}

	fetch, ok := fanOutCandidate.ToScanPlan(prefix, false).(*plans.RecordQueryFetchFromPartialRecordPlan)
	if !ok {
		t.Fatalf("fan-out scan = %T, want Fetch(IndexScan)", fetch)
	}
	indexPlan, ok := fetch.GetInner().(*plans.RecordQueryIndexPlan)
	if !ok {
		t.Fatalf("fan-out fetch child = %T, want IndexPlan", fetch.GetInner())
	}
	pkColumns := indexPlan.GetPKColumnNames()
	if len(pkColumns) != 1 || pkColumns[0] != "ID" {
		t.Fatalf(
			"fan-out scan primary-key coverage = %v, want [ID]",
			pkColumns,
		)
	}
	if ordering := indexPlan.HintOrdering(); ordering.IsKnown ||
		len(ordering.Keys) != 0 {
		t.Fatalf(
			"candidate fan-out signal did not suppress physical ordering: %#v",
			ordering,
		)
	}
	if rich := indexPlan.HintRichOrdering(); len(rich.GetKeys()) != 0 {
		t.Fatalf(
			"candidate fan-out signal did not suppress rich ordering: %#v",
			rich.GetKeys(),
		)
	}

	// A non-fan-out scalar index with the same translation machinery retains
	// its historical covering behavior.
	scalarAlias := values.UniqueCorrelationIdentifier()
	noDuplicates := false
	scalarCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_score",
		[]string{"Item"},
		[]string{"SCORE"},
		nil,
		[]values.CorrelationIdentifier{scalarAlias},
		itemRowType,
		false,
		nil,
		&noDuplicates,
	).WithRootKeyExpression(candidateTestKeyField("SCORE", gen.Field_SCALAR)).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	if _, ok := scalarCandidate.PushValueThroughFetch(
		field("SCORE"),
		sourceAlias,
		targetAlias,
	); !ok {
		t.Fatal("scalar index column must remain pushable through fetch")
	}
}

func TestValueIndexScanMatchCandidate_WholeRecordIsNotCovered(t *testing.T) {
	t.Parallel()

	columnAlias := values.UniqueCorrelationIdentifier()
	itemRowType := testRecordRowType("Item", "ID", "SCORE")
	candidate := newKnownDistinctValueIndexCandidate(
		"item_score",
		[]string{"Item"},
		[]string{"SCORE"},
		[]values.CorrelationIdentifier{columnAlias},
		itemRowType,
		false,
		nil,
	)
	sourceAlias := values.NamedCorrelationIdentifier("source")
	targetAlias := values.NamedCorrelationIdentifier("target")

	if translated, ok := candidate.PushValueThroughFetch(
		matchCandidateQOV(t, sourceAlias, itemRowType),
		sourceAlias,
		targetAlias,
	); ok {
		t.Fatalf(
			"whole source record must retain Fetch, got translated value %v",
			translated,
		)
	}

	uncorrelatedAlias := values.NamedCorrelationIdentifier("uncorrelated")
	uncorrelated := matchCandidateQOV(t, uncorrelatedAlias, itemRowType)
	translated, ok := candidate.PushValueThroughFetch(
		uncorrelated,
		sourceAlias,
		targetAlias,
	)
	if !ok || translated != uncorrelated {
		t.Fatalf(
			"uncorrelated value = (%v, %v), want unchanged success",
			translated,
			ok,
		)
	}
}

func TestValueIndexScanMatchCandidate_FunctionKeyCoversOnlyPKAndAbstainsOrdering(
	t *testing.T,
) {
	t.Parallel()

	createsDuplicates := false
	alias := values.UniqueCorrelationIdentifier()
	itemRowType := values.NewRecordType("Item", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 1},
		{Name: "B", FieldType: values.NullableLong, Ordinal: 2},
	})
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_cardinality_tags",
		[]string{"Item"},
		[]string{"TAGS", "B"},
		[]string{FunctionKindCardinality, ""},
		[]values.CorrelationIdentifier{
			alias,
			values.UniqueCorrelationIdentifier(),
		},
		itemRowType,
		false,
		[]string{"ID", "TAGS"},
		&createsDuplicates,
	).WithRootKeyExpression(&gen.KeyExpression{Then: &gen.Then{
		Child: []*gen.KeyExpression{
			{Function: &gen.Function{
				Name: proto.String(FunctionKindCardinality),
				Arguments: candidateTestKeyField(
					"TAGS",
					gen.Field_CONCATENATE,
				),
			}},
			candidateTestKeyField("B", gen.Field_SCALAR),
		},
	}})

	source := values.UniqueCorrelationIdentifier()
	target := values.UniqueCorrelationIdentifier()
	translated, ok := candidate.PushValueThroughFetch(
		matchCandidateField(t, source, itemRowType, []int{1}, false),
		source,
		target,
	)
	if ok || translated != nil {
		t.Fatalf(
			"function-key candidate translated its bare argument through Fetch: %v, %t",
			translated,
			ok,
		)
	}
	translated, ok = candidate.PushValueThroughFetch(
		matchCandidateField(t, source, itemRowType, []int{2}, false),
		source,
		target,
	)
	if ok || translated != nil {
		t.Fatalf(
			"mixed function-key candidate translated sibling index field through Fetch: %v, %t",
			translated,
			ok,
		)
	}
	translated, ok = candidate.PushValueThroughFetch(
		matchCandidateField(t, source, itemRowType, []int{0}, false),
		source,
		target,
	)
	if !ok || translated == nil {
		t.Fatalf(
			"function-key candidate did not translate index-resident PK through Fetch: %v, %t",
			translated,
			ok,
		)
	}

	scan := candidate.ToScanPlan(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{},
		false,
	)
	indexPlan := extractIndexPlan(scan)
	if indexPlan == nil {
		t.Fatal("function-key candidate did not produce its non-covering scan")
	}
	if ordering := indexPlan.HintOrdering(); ordering.IsKnown ||
		len(ordering.Keys) != 0 {
		t.Fatalf(
			"function-key physical ordering = %#v, want abstain",
			ordering,
		)
	}
	if rich := indexPlan.HintRichOrdering(); len(rich.GetKeys()) != 0 {
		t.Fatalf(
			"function-key rich ordering = %#v, want abstain",
			rich.GetKeys(),
		)
	}
}

func TestValueIndexScanMatchCandidate_FanOutOrderingMatchesJava(t *testing.T) {
	t.Parallel()

	tagAlias := values.UniqueCorrelationIdentifier()
	scoreAlias := values.UniqueCorrelationIdentifier()
	duplicates := true
	itemRowType := values.NewRecordType("Item", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "TAGS", FieldType: values.NewArrayType(true, values.NotNullString), Ordinal: 1},
		{Name: "SCORE", FieldType: values.NullableLong, Ordinal: 2},
	})
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_tags_score",
		[]string{"Item"},
		[]string{"TAGS", "SCORE"},
		nil,
		[]values.CorrelationIdentifier{tagAlias, scoreAlias},
		itemRowType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(&gen.KeyExpression{
		Then: &gen.Then{Child: []*gen.KeyExpression{
			candidateTestKeyField("TAGS", gen.Field_FAN_OUT),
			candidateTestKeyField("SCORE", gen.Field_SCALAR),
		}},
	}).WithKeyComponentTypes([]values.Type{values.NullableString, values.NullableLong})

	equality := predicates.NewLiteralComparison(
		predicates.ComparisonEquals,
		"blue",
	)
	equalityRange := predicates.EmptyComparisonRange().Merge(&equality).Range
	equalityInfo := NewRegularMatchInfo(
		map[values.CorrelationIdentifier]*predicates.ComparisonRange{
			tagAlias: equalityRange,
		},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	parts := candidate.ComputeMatchedOrderingParts(
		equalityInfo,
		[]values.CorrelationIdentifier{tagAlias, scoreAlias},
		false,
	)
	if len(parts) != 1 {
		t.Fatalf(
			"equality-bound fan-out ordering parts = %d, want only trailing SCORE",
			len(parts),
		)
	}
	if parts[0].GetParameterId() != scoreAlias {
		t.Fatalf(
			"ordering parameter = %s, want trailing SCORE %s",
			parts[0].GetParameterId(),
			scoreAlias,
		)
	}
	scoreValue, ok := values.AsFieldValue(parts[0].GetValue())
	if !ok || scoreValue.DisplayName() != "SCORE" {
		t.Fatalf(
			"ordering value = %#v, want scalar SCORE (never TAGS array)",
			parts[0].GetValue(),
		)
	}

	reverseParts := candidate.ComputeMatchedOrderingParts(
		equalityInfo,
		[]values.CorrelationIdentifier{tagAlias, scoreAlias},
		true,
	)
	if len(reverseParts) != 1 ||
		reverseParts[0].GetMatchedSortOrder() != MatchedSortOrderDescending {
		t.Fatalf("reverse trailing ordering = %#v, want one descending SCORE", reverseParts)
	}

	inequality := predicates.NewLiteralComparison(
		predicates.ComparisonGreaterThan,
		"blue",
	)
	inequalityRange := predicates.EmptyComparisonRange().Merge(&inequality).Range
	for _, tc := range []struct {
		name     string
		bindings map[values.CorrelationIdentifier]*predicates.ComparisonRange
	}{
		{name: "unbound"},
		{
			name: "range-bound",
			bindings: map[values.CorrelationIdentifier]*predicates.ComparisonRange{
				tagAlias: inequalityRange,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			info := NewRegularMatchInfo(
				tc.bindings,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
				nil,
			)
			got := candidate.ComputeMatchedOrderingParts(
				info,
				[]values.CorrelationIdentifier{tagAlias, scoreAlias},
				false,
			)
			if len(got) != 0 {
				t.Fatalf(
					"fan-out %s ordering = %#v, want no claim past TAGS",
					tc.name,
					got,
				)
			}
		})
	}

	scalarAlias := values.UniqueCorrelationIdentifier()
	noDuplicates := false
	scalarCandidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_score",
		[]string{"Item"},
		[]string{"SCORE"},
		nil,
		[]values.CorrelationIdentifier{scalarAlias},
		itemRowType,
		false,
		nil,
		&noDuplicates,
	).WithRootKeyExpression(candidateTestKeyField("SCORE", gen.Field_SCALAR)).
		WithKeyComponentTypes([]values.Type{values.NullableLong})
	scalarParts := scalarCandidate.ComputeMatchedOrderingParts(
		NewRegularMatchInfo(nil, nil, nil, nil, nil, nil, nil, nil),
		[]values.CorrelationIdentifier{scalarAlias},
		false,
	)
	if len(scalarParts) != 1 ||
		scalarParts[0].GetParameterId() != scalarAlias {
		t.Fatalf("scalar ordering regression: got %#v, want one SCORE part", scalarParts)
	}
}

func candidateTestKeyField(
	name string,
	fanType gen.Field_FanType,
) *gen.KeyExpression {
	return &gen.KeyExpression{Field: &gen.Field{
		FieldName: proto.String(name),
		FanType:   &fanType,
	}}
}
