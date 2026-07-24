package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func TestRFC190OrderingPushdownScopesJoinKeysToOwningChild(t *testing.T) {
	t.Parallel()
	cAlias := values.NamedCorrelationIdentifier("C")
	iAlias := values.NamedCorrelationIdentifier("I")
	cName := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(cAlias), "NAME", 1, values.UnknownType)
	iTitle := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(iAlias), "TITLE", 2, values.UnknownType)
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "NAME", Value: cName},
		values.RecordConstructorField{Name: "TITLE", Value: iTitle},
	)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{
			{Value: cName, SortOrder: properties.RequestedSortOrderAscending},
			{Value: iTitle, SortOrder: properties.RequestedSortOrderDescending},
		},
		properties.DistinctnessNotDistinct,
		false,
	)
	localAliases := map[values.CorrelationIdentifier]struct{}{
		cAlias: {},
		iAlias: {},
	}

	cOrdering := pushRequestedOrderingToSelectChild(
		requested, result, cAlias, localAliases)
	iOrdering := pushRequestedOrderingToSelectChild(
		requested, result, iAlias, localAliases)

	if got := cOrdering.GetParts(); len(got) != 1 ||
		!values.ValuesStructurallyEqual(got[0].Value, cName) {
		t.Fatalf("C ordering parts = %#v, want only C.NAME", got)
	}
	if got := iOrdering.GetParts(); len(got) != 1 ||
		!values.ValuesStructurallyEqual(got[0].Value, iTitle) {
		t.Fatalf("I ordering parts = %#v, want only I.TITLE", got)
	}
}

func TestRFC190OrderingPushdownUsesLegLocalOrdinalBeforeConstructorOrdinal(t *testing.T) {
	t.Parallel()
	cAlias := values.NamedCorrelationIdentifier("C")
	iAlias := values.NamedCorrelationIdentifier("I")
	cPriority := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(cAlias), "PRIORITY", 2, values.UnknownType)
	iTitle := values.NewCorrelatedFieldValueWithResolvedOrdinal(
		values.NewQuantifiedObjectValue(iAlias), "TITLE", 2, values.UnknownType)
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: values.NewFlatFieldValue("ID", values.UnknownType)},
		values.RecordConstructorField{Name: "NAME", Value: values.NewFlatFieldValue("NAME", values.UnknownType)},
		values.RecordConstructorField{Name: "PRIORITY", Value: cPriority},
		values.RecordConstructorField{Name: "TITLE", Value: iTitle},
	)
	requested := properties.NewRequestedOrdering(
		[]properties.RequestedOrderingPart{{
			Value:     iTitle,
			SortOrder: properties.RequestedSortOrderAscending,
		}},
		properties.DistinctnessNotDistinct,
		false,
	)

	pushed := requested.PushDownThroughValue(result, iAlias)
	if got := pushed.GetParts(); len(got) != 1 ||
		!values.ValuesStructurallyEqual(got[0].Value, iTitle) ||
		values.ValuesStructurallyEqual(got[0].Value, cPriority) {
		t.Fatalf("pushed parts = %#v, want I.TITLE rather than constructor slot 2", got)
	}
}
