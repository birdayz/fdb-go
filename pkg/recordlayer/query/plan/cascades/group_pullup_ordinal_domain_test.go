package cascades

// The group-by pull-up walk rebuilds a decomposed reference from an exact
// typed base. Each rebuilt path must retain the root layout its ordinal indexes;
// an unresolved or non-record base must fail at the construction boundary.

import (
	"slices"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

func groupPullUpQOV(
	t testing.TB,
	typ values.Type,
) values.QuantifiedObjectValue {
	t.Helper()
	qov, err := values.NewQuantifiedObjectValue(
		values.NamedCorrelationIdentifier("upper"), typ)
	return mustConstruct(t, qov, err)
}

func TestGroupPullUpFieldPath_DerivesDomainFromTypedBase(t *testing.T) {
	t.Parallel()

	layout := testRecordRowType("T", "ID", "ADDR", "CITY")
	base := groupPullUpQOV(t, layout)

	rebuilt := applyGroupFieldPath(base, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  2,
		resolved: true,
	}}, values.NullableLong)

	field, ok := values.AsFieldValue(rebuilt)
	if !ok || field.Path() == nil {
		t.Fatalf("rebuilt reference is %T, want exact FieldValue", rebuilt)
	}
	wantDomain := values.OrdinalDomainOfType(layout)
	if field.Path().RootDomain() != wantDomain {
		t.Fatalf("rebuilt domain = %v, want typed base domain %v",
			field.Path().RootDomain(), wantDomain)
	}
	identity, answered := values.CorrelatedFieldIdentityIn(rebuilt, wantDomain)
	if !answered || identity.Ordinal != 2 {
		t.Fatalf("rebuilt identity = (%+v, %t), want CITY ordinal 2", identity, answered)
	}
}

func TestGroupPullUpFieldPath_UnresolvedOrNonRecordBaseFailsClosed(t *testing.T) {
	t.Parallel()

	alias := values.NamedCorrelationIdentifier("upper")
	if unresolved, err := values.NewQuantifiedObjectValue(alias, values.UnknownType); err == nil || unresolved != nil {
		t.Fatalf("unresolved QOV admission = (%v, %v), want (nil, error)", unresolved, err)
	}

	scalarBase := groupPullUpQOV(t, values.NotNullLong)
	rebuilt := applyGroupFieldPath(scalarBase, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  2,
		resolved: true,
	}}, values.NullableLong)
	if rebuilt != nil {
		t.Fatalf("field path over exact scalar base = %T (%v), want fail-closed nil", rebuilt, rebuilt)
	}
}

func TestGroupPullUpFieldPath_FusedStepKeepsTheReceiversDomain(t *testing.T) {
	t.Parallel()

	// Fusing an outer step onto an inner exact path keeps the root QOV's layout;
	// the nested suffix has its own local ordinal but cannot turn the whole fused
	// path into a top-level column identity.
	nested := values.NewRecordType("ADDR", false, []values.Field{
		{Name: "STREET", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "CITY", FieldType: values.NullableLong, Ordinal: 1},
	})
	layout := values.NewRecordType("T", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong, Ordinal: 0},
		{Name: "ADDR", FieldType: nested, Ordinal: 1},
	})
	base := groupPullUpQOV(t, layout)
	inner, err := values.ResolveFieldOrdinals(base, []int{1})
	inner = mustConstruct(t, inner, err)

	rebuilt := applyGroupFieldPath(inner, []groupFieldPathStep{{
		field:    "CITY",
		ordinal:  1,
		resolved: true,
	}}, values.NullableLong)

	field, ok := values.AsFieldValue(rebuilt)
	if !ok || field.Path() == nil {
		t.Fatalf("rebuilt reference is %T, want exact fused FieldValue", rebuilt)
	}
	if got := field.Path().Ordinals(); !slices.Equal(got, []int{1, 1}) {
		t.Fatalf("fused path = %v, want [1 1]", got)
	}
	wantDomain := values.OrdinalDomainOfType(layout)
	if field.Path().RootDomain() != wantDomain {
		t.Fatalf("fused path domain = %v, want receiver's root layout %v",
			field.Path().RootDomain(), wantDomain)
	}
	if identity, answered := values.CorrelatedFieldIdentityIn(rebuilt, wantDomain); answered {
		t.Fatalf("multi-accessor path answered as top-level identity %+v", identity)
	}
}
