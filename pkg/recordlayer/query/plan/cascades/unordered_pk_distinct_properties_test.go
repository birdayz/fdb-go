package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func TestUnorderedPrimaryKeyDistinctPlanProperties(t *testing.T) {
	t.Parallel()

	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.NotNullLong}}
	scan := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
		WithPrimaryKey(pk)
	distinct := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(scan)

	if !computeDistinctRecords(distinct, distinct) {
		t.Fatal("primary-key distinct must advertise distinct records")
	}
	if !computeStoredRecord(distinct) {
		t.Fatal("primary-key distinct must preserve its child's stored record")
	}
	gotPK, ok := computePrimaryKey(distinct).([]values.Value)
	if !ok || len(gotPK) != 1 || !values.ValuesStructurallyEqual(gotPK[0], pk[0]) {
		t.Fatalf("primary-key distinct PK = %v, want child PK %v", gotPK, pk)
	}

	nonStoredChild := plans.NewRecordQueryStreamingAggregationPlan(scan, nil, nil)
	nonStoredDistinct := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(nonStoredChild)
	if computeStoredRecord(nonStoredDistinct) {
		t.Fatal("primary-key distinct must not manufacture a stored record its child lacks")
	}
}

func TestFlatMapInheritedOuterPlanProperties(t *testing.T) {
	t.Parallel()

	pk := []values.Value{&values.FieldValue{Field: "ID", Typ: values.NotNullLong}}
	outer := plans.NewRecordQueryScanPlan([]string{"OUTER"}, values.UnknownType, false).
		WithPrimaryKey(pk)
	inner := plans.NewRecordQueryScanPlan([]string{"INNER"}, values.UnknownType, false)
	outerAlias := values.NamedCorrelationIdentifier("OUTER")
	innerAlias := values.NamedCorrelationIdentifier("INNER")
	resultValue := values.NewQuantifiedObjectValue(outerAlias)

	inheriting := plans.NewRecordQueryFlatMapPlan(
		outer, inner, outerAlias, innerAlias, resultValue, true)
	if !computeStoredRecord(inheriting) {
		t.Fatal("inheriting FlatMap must preserve the outer stored-record property")
	}
	gotPK, ok := computePrimaryKey(inheriting).([]values.Value)
	if !ok || len(gotPK) != 1 || !values.ValuesStructurallyEqual(gotPK[0], pk[0]) {
		t.Fatalf("inheriting FlatMap PK = %v, want outer PK %v", gotPK, pk)
	}

	nonInheriting := plans.NewRecordQueryFlatMapPlan(
		outer, inner, outerAlias, innerAlias, resultValue, false)
	if computeStoredRecord(nonInheriting) {
		t.Fatal("generic non-inheriting FlatMap must not claim an outer stored record")
	}
	if got := computePrimaryKey(nonInheriting); got != nil {
		t.Fatalf("generic non-inheriting FlatMap PK = %v, want nil", got)
	}
}

func TestUnorderedPrimaryKeyDistinctCardinalities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		child   properties.Cardinalities
		wantMin properties.Cardinality
		wantMax properties.Cardinality
	}{
		{
			name: "known_non_empty_min_collapses_to_one",
			child: properties.Cardinalities{
				Min: properties.OfCardinality(7),
				Max: properties.OfCardinality(11),
			},
			wantMin: properties.OfCardinality(1),
			wantMax: properties.OfCardinality(11),
		},
		{
			name: "possibly_empty_stays_possibly_empty",
			child: properties.Cardinalities{
				Min: properties.OfCardinality(0),
				Max: properties.OfCardinality(11),
			},
			wantMin: properties.OfCardinality(0),
			wantMax: properties.OfCardinality(11),
		},
		{
			name: "unknown_min_stays_unknown",
			child: properties.Cardinalities{
				Min: properties.UnknownCardinality(),
				Max: properties.OfCardinality(11),
			},
			wantMin: properties.UnknownCardinality(),
			wantMax: properties.OfCardinality(11),
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			child := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false)
			childRef := expressions.InitialOf(child)
			pm := NewPlanPropertiesMap()
			pm.props[child] = properties.PropertyMap{
				properties.PropCardinalities: tc.child,
			}
			childRef.SetPlanProperties(pm)

			distinct := plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlanFromQuantifier(
				expressions.NewPhysicalQuantifier(childRef))
			got := computeCardinalities(distinct, distinct)
			if !got.GetMinCardinality().Equal(tc.wantMin) ||
				!got.GetMaxCardinality().Equal(tc.wantMax) {
				t.Fatalf("cardinalities = %v, want min=%v max=%v", got, tc.wantMin, tc.wantMax)
			}
		})
	}
}
