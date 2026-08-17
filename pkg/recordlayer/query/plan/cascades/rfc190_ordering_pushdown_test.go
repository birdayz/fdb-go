package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func mustRFC190PushdownConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-190 pushdown fixture: " + err.Error())
	}
	return value
}

func rfc190PushdownCType() *values.RecordType {
	return values.NewRecordType("RFC190Customer", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "PRIORITY", FieldType: values.NullableLong},
	})
}

func rfc190PushdownIType() *values.RecordType {
	return values.NewRecordType("RFC190Item", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "NAME", FieldType: values.NullableString},
		{Name: "TITLE", FieldType: values.NullableString},
	})
}

func rfc190PushdownField(
	alias values.CorrelationIdentifier,
	rowType values.Type,
	ordinal int,
) values.Value {
	root := mustRFC190PushdownConstruct(values.NewQuantifiedObjectValue(alias, rowType))
	return mustRFC190PushdownConstruct(values.ResolveFieldOrdinals(root, []int{ordinal}))
}

func TestRFC190OrderingPushdownScopesJoinKeysToOwningChild(t *testing.T) {
	t.Parallel()
	cAlias := values.NamedCorrelationIdentifier("C")
	iAlias := values.NamedCorrelationIdentifier("I")
	cName := rfc190PushdownField(cAlias, rfc190PushdownCType(), 1)
	iTitle := rfc190PushdownField(iAlias, rfc190PushdownIType(), 2)
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
	cPriority := rfc190PushdownField(cAlias, rfc190PushdownCType(), 2)
	iTitle := rfc190PushdownField(iAlias, rfc190PushdownIType(), 2)
	localAlias := values.NamedCorrelationIdentifier("LOCAL")
	localType := values.NewRecordType("RFC190Local", false, []values.Field{
		{Name: "ID", FieldType: values.NotNullLong},
		{Name: "NAME", FieldType: values.NullableString},
	})
	localID := rfc190PushdownField(localAlias, localType, 0)
	localName := rfc190PushdownField(localAlias, localType, 1)
	result := values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "ID", Value: localID},
		values.RecordConstructorField{Name: "NAME", Value: localName},
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
