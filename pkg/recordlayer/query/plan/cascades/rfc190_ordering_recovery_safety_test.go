package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestRFC190FlatMapOrderingLensQualifiesOnlyInheritedOuterFields(t *testing.T) {
	t.Parallel()

	outerAlias := values.NamedCorrelationIdentifier("outer")
	innerAlias := values.NamedCorrelationIdentifier("inner")
	localID := values.NewFieldValueWithResolvedOrdinal(
		"ID", 0, values.UnknownType)
	alreadyOuter := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(outerAlias),
		"OUTER_NAME", 1, values.UnknownType)
	innerField := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(innerAlias),
		"INNER_VALUE", 2, values.UnknownType)
	literal := values.LiteralValue("constant")
	resultValue := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: localID},
		values.RecordConstructorField{Name: "OUTER_NAME", Value: alreadyOuter},
		values.RecordConstructorField{Name: "INNER_VALUE", Value: innerField},
		values.RecordConstructorField{Name: "LITERAL", Value: literal},
	)

	inheriting := plans.NewRecordQueryFlatMapPlan(
		nil, nil, outerAlias, innerAlias, resultValue, true)
	lens := flatMapOrderingResultForChild(inheriting, outerAlias, true)
	lensRecord, ok := lens.(*values.RecordConstructorValue)
	if !ok {
		t.Fatalf("inherited ordering lens = %T, want RecordConstructorValue", lens)
	}

	qualifiedID, ok := lensRecord.Fields[0].Value.(*values.FieldValue)
	if !ok {
		t.Fatalf("qualified ID = %T, want FieldValue", lensRecord.Fields[0].Value)
	}
	idRoot, ok := qualifiedID.Child.(*values.QuantifiedObjectValue)
	if !ok || idRoot.Correlation != outerAlias {
		t.Fatalf("qualified ID root = %#v, want QOV(%s)", qualifiedID.Child, outerAlias.Name())
	}
	if qualifiedID.Field != localID.Field ||
		qualifiedID.Resolved == nil ||
		!qualifiedID.Resolved.Equals(localID.Resolved) {
		t.Fatal("qualifying the source-local ID changed its accessor identity")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[1].Value, alreadyOuter) {
		t.Fatal("the lens rewrote an already-qualified outer field")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[2].Value, innerField) {
		t.Fatal("the lens claimed an inner-correlated field for the outer")
	}
	if !values.ValuesStructurallyEqual(lensRecord.Fields[3].Value, literal) {
		t.Fatal("the lens rewrote a non-field result value")
	}
	if got := flatMapOrderingResultForChild(
		inheriting, innerAlias, false,
	); got != resultValue {
		t.Fatal("the inherited-record lens must apply only to the outer child")
	}

	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     localID,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)
	localAliases := map[values.CorrelationIdentifier]struct{}{
		outerAlias: {},
		innerAlias: {},
	}
	pushedInherited := pushRequestedOrderingToSelectChild(
		requested, lens, outerAlias, localAliases)
	if pushedInherited.IsPreserve() || len(pushedInherited.GetParts()) != 1 {
		t.Fatalf("inherited outer request = %#v, want one concrete part",
			pushedInherited.GetParts())
	}
	pushedRoot := values.GetCorrelatedToOfValue(
		pushedInherited.GetParts()[0].Value)
	if _, ok := pushedRoot[outerAlias]; !ok || len(pushedRoot) != 1 {
		t.Fatalf("inherited outer request correlations = %v, want only %s",
			pushedRoot, outerAlias.Name())
	}

	ordinary := plans.NewRecordQueryFlatMapPlan(
		nil, nil, outerAlias, innerAlias, resultValue, false)
	ordinaryLens := flatMapOrderingResultForChild(
		ordinary, outerAlias, true)
	if ordinaryLens != resultValue {
		t.Fatal("an ordinary FlatMap unexpectedly acquired inherited outer ownership")
	}
	pushedOrdinary := pushRequestedOrderingToSelectChild(
		requested, ordinaryLens, outerAlias, localAliases)
	if !pushedOrdinary.IsPreserve() {
		t.Fatalf("ambiguous ordinary FlatMap request = %#v, want Preserve",
			pushedOrdinary.GetParts())
	}
}

type rfc190PrimaryKeyPlanContext struct {
	indexTestPlanContext
	primaryKeyColumns []string
}

func (c *rfc190PrimaryKeyPlanContext) GetPrimaryKeyColumns(string) []string {
	return c.primaryKeyColumns
}

func TestRFC190OrderedFullScanAlternativesFinalPrimaryScanSafety(t *testing.T) {
	t.Parallel()

	ctx := &rfc190PrimaryKeyPlanContext{
		primaryKeyColumns: []string{"ID"},
	}
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     values.NewFieldValueWithResolvedOrdinal("ID", 0, values.UnknownType),
			SortOrder: properties.RequestedSortOrderDescending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	t.Run("unbounded_forward_recovers_reverse", func(t *testing.T) {
		t.Parallel()

		forward := plans.NewRecordQueryScanPlan(
			[]string{"T"}, values.UnknownType, false,
		).WithPrimaryKey([]values.Value{
			values.NewFlatFieldValue("ID", values.UnknownType),
		})
		ref := expressions.FinalOf(forward)

		alternatives := orderedFullScanAlternatives(ref, requested, ctx)
		if len(alternatives) != 1 {
			t.Fatalf("ordered alternatives = %d, want one reverse primary scan",
				len(alternatives))
		}
		reverse, ok := alternatives[0].(*plans.RecordQueryScanPlan)
		if !ok {
			t.Fatalf("ordered alternative = %T, want RecordQueryScanPlan",
				alternatives[0])
		}
		if !reverse.IsReverse() {
			t.Fatal("recovered primary scan is not reverse")
		}
		if len(reverse.GetScanComparisons()) != 0 {
			t.Fatal("recovered full scan unexpectedly acquired bounds")
		}
		if len(ref.Members()) != 0 ||
			len(ref.FinalMembers()) != 1 ||
			ref.FinalMembers()[0] != forward {
			t.Fatal("ordered full-scan recovery mutated the finals-only source group")
		}
	})

	t.Run("bounded_scan_declines", func(t *testing.T) {
		t.Parallel()

		comparison := predicates.NewLiteralComparison(
			predicates.ComparisonEquals, int64(7))
		merged := predicates.EmptyComparisonRange().Merge(&comparison)
		if !merged.Ok {
			t.Fatal("failed to construct bounded-scan comparison")
		}
		bounded := plans.NewRecordQueryScanPlan(
			[]string{"T"}, values.UnknownType, false,
		).WithPrimaryKey([]values.Value{
			values.NewFlatFieldValue("ID", values.UnknownType),
		}).WithScanComparisons([]*predicates.ComparisonRange{merged.Range})
		ref := expressions.FinalOf(bounded)

		if alternatives := orderedFullScanAlternatives(
			ref, requested, ctx,
		); len(alternatives) != 0 {
			t.Fatalf("bounded scan produced %d ordered full-scan alternatives",
				len(alternatives))
		}
		if len(ref.Members()) != 0 ||
			len(ref.FinalMembers()) != 1 ||
			ref.FinalMembers()[0] != bounded {
			t.Fatal("bounded-scan decline mutated the finals-only source group")
		}
	})
}
