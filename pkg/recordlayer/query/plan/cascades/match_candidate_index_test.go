package cascades

import (
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
	"google.golang.org/protobuf/proto"
)

func TestValueIndexScanMatchCandidate_PrefixMap_AllEquality(t *testing.T) {
	t.Parallel()
	a1 := values.UniqueCorrelationIdentifier()
	a2 := values.UniqueCorrelationIdentifier()
	c := newKnownDistinctValueIndexCandidate(
		"Order$status_date",
		[]string{"Order"},
		[]string{"STATUS", "DATE"},
		[]values.CorrelationIdentifier{a1, a2},
		values.UnknownType,
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
		values.UnknownType,
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
		values.UnknownType,
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
		values.UnknownType,
		false,
		nil,
	)
	eq := predicates.EmptyComparisonRange()
	eqRes := eq.Merge(&predicates.Comparison{Type: predicates.ComparisonEquals, Operand: values.LiteralValue("active")})

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
	rowType := testRecordRowType("T", "ID", "ADDR", "CITY")
	c := newKnownDistinctValueIndexCandidate(
		"idx_city",
		[]string{"T"},
		[]string{"CITY"},
		[]values.CorrelationIdentifier{sarg},
		rowType,
		false,
		nil,
	)

	// Chained ADDR.CITY: FieldValue{CITY, Child: FieldValue{ADDR, Child: QOV(SRC)}}.
	// Its leaf name (CITY) collides with the covered top-level column but the value
	// is a different indexed Value — must decline.
	inner := testColumnRef(values.NewQuantifiedObjectValue(src), rowType, "ADDR", values.UnknownType)
	chained := values.NewFieldValue(inner, "CITY", values.UnknownType)
	got, ok := c.PushValueThroughFetch(chained, src, tgt)
	if ok {
		t.Fatalf("chained ADDR.CITY must NOT push through fetch (leaf-name collision), got ok=true value=%v", got)
	}

	// Bare top-level CITY over the source: FieldValue{CITY, Child: QOV(SRC)} — the
	// legitimate case that must STILL translate (guard against over-restriction).
	bare := testColumnRef(values.NewQuantifiedObjectValue(src), rowType, "CITY", values.UnknownType)
	gotBare, okBare := c.PushValueThroughFetch(bare, src, tgt)
	if !okBare {
		t.Fatal("bare top-level CITY over the source must push through fetch")
	}
	fv, isFV := gotBare.(*values.FieldValue)
	if !isFV || fv.Field != "CITY" {
		t.Fatalf("translated value=%v, want FieldValue{CITY, ...}", gotBare)
	}
	childQOV, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV || childQOV.Correlation != tgt {
		t.Fatalf("translated child=%v, want QOV(TGT)", fv.Child)
	}

	// BAKED bare column: FieldValue{CITY, Child: nil, Resolved} — the
	// post-ordinalization (resolved-ordinal / leaf) shape the covering merge
	// actually produces. An over-restrictive "Child must be QOV(SRC)" check
	// WRONGLY declined this and regressed the covering-index merge; only this
	// childless case (not the bare-QOV case above) reveals that. It must STILL
	// translate.
	baked := values.NewFieldValueWithResolvedOrdinalInDomain(
		"CITY", 2, values.UnknownType, values.OrdinalDomainOfType(rowType),
	)
	gotBaked, okBaked := c.PushValueThroughFetch(baked, src, tgt)
	if !okBaked {
		t.Fatal("baked bare CITY (Child==nil, the post-ordinalization covering shape) must push through fetch")
	}
	if bfv, isFV := gotBaked.(*values.FieldValue); !isFV || bfv.Field != "CITY" {
		t.Fatalf("translated baked value=%v, want FieldValue{CITY, ...}", gotBaked)
	}

	// A FieldValue over a NON-QOV, non-nil composite child (here a constant) is not
	// a bare column and must DECLINE — pins the allowlist (Child==nil || Child==QOV),
	// not a chained-only blocklist that would let this slip through and drop the child.
	overConst := values.NewFieldValue(&values.ConstantValue{Value: "x", Typ: values.UnknownType}, "CITY", values.UnknownType)
	if got2, ok2 := c.PushValueThroughFetch(overConst, src, tgt); ok2 {
		t.Fatalf("FieldValue over a non-QOV composite must NOT push through fetch, got ok=true value=%v", got2)
	}
}

func TestValueIndexScanMatchCandidate_FanOutElementIsNotCoveredAsArray(t *testing.T) {
	t.Parallel()

	tagAlias := values.UniqueCorrelationIdentifier()
	scoreAlias := values.UniqueCorrelationIdentifier()
	reportedNoDuplicates := false
	itemRowType := testRecordRowType("Item", "ID", "TAGS", "SCORE")
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
	})
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
		values.UnknownType,
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
		return testColumnRef(
			values.NewQuantifiedObjectValue(sourceAlias),
			itemRowType, name, values.UnknownType,
		)
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
		testColumnRef(nil, itemRowType, "TAGS", values.UnknownType),
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
		translatedField, ok := translated.(*values.FieldValue)
		if !ok || translatedField.Field != coveredColumn {
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
	).WithRootKeyExpression(candidateTestKeyField("SCORE", gen.Field_SCALAR))
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
	candidate := newKnownDistinctValueIndexCandidate(
		"item_score",
		[]string{"Item"},
		[]string{"SCORE"},
		[]values.CorrelationIdentifier{columnAlias},
		values.UnknownType,
		false,
		nil,
	)
	sourceAlias := values.NamedCorrelationIdentifier("source")
	targetAlias := values.NamedCorrelationIdentifier("target")

	if translated, ok := candidate.PushValueThroughFetch(
		values.NewQuantifiedObjectValue(sourceAlias),
		sourceAlias,
		targetAlias,
	); ok {
		t.Fatalf(
			"whole source record must retain Fetch, got translated value %v",
			translated,
		)
	}

	uncorrelatedAlias := values.NamedCorrelationIdentifier("uncorrelated")
	uncorrelated := values.NewQuantifiedObjectValue(uncorrelatedAlias)
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
	itemRowType := testRecordRowType("Item", "ID", "TAGS", "B")
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
		testColumnRef(values.NewQuantifiedObjectValue(source), itemRowType, "TAGS", values.UnknownType),
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
		testColumnRef(values.NewQuantifiedObjectValue(source), itemRowType, "B", values.UnknownType),
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
		testColumnRef(values.NewQuantifiedObjectValue(source), itemRowType, "ID", values.UnknownType),
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
	candidate := NewValueIndexScanMatchCandidateWithFunctions(
		"item_tags_score",
		[]string{"Item"},
		[]string{"TAGS", "SCORE"},
		nil,
		[]values.CorrelationIdentifier{tagAlias, scoreAlias},
		values.UnknownType,
		false,
		nil,
		&duplicates,
	).WithRootKeyExpression(&gen.KeyExpression{
		Then: &gen.Then{Child: []*gen.KeyExpression{
			candidateTestKeyField("TAGS", gen.Field_FAN_OUT),
			candidateTestKeyField("SCORE", gen.Field_SCALAR),
		}},
	})

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
	scoreValue, ok := parts[0].GetValue().(*values.FieldValue)
	if !ok || scoreValue.Field != "SCORE" {
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
		values.UnknownType,
		false,
		nil,
		&noDuplicates,
	).WithRootKeyExpression(candidateTestKeyField("SCORE", gen.Field_SCALAR))
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
