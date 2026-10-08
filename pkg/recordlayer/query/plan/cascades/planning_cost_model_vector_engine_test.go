package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// Ports of Java's PlanningCostModelVectorEngineTest (4.14.2.0), asserted on the
// criterion (compareVectorIndexEnginePreference) as the Java test's own
// criterion-level cases are. Java's three compare()-level cases
// (noPreferenceFallsThroughToPlanHash and the two assertSameAsWithoutPreference
// ones) reduce to the criterion abstaining, which is what is asserted here.

func vectorEnginePlan(t *testing.T, index, engine string) *plans.RecordQueryVectorIndexPlan {
	t.Helper()
	row := values.NewRecordType("VectorRow", false, []values.Field{
		{Name: "embedding", FieldType: values.NewArrayType(false, values.NotNullDouble)},
	})
	p, err := plans.NewRecordQueryVectorIndexPlan(index, nil,
		&values.ConstantValue{Value: []float64{1}, Typ: values.NewArrayType(false, values.NotNullDouble)},
		&values.ConstantValue{Value: int64(5), Typ: values.NotNullLong},
		predicates.ComparisonDistanceRankLessThanOrEq, nil, nil, []string{"T"}, row)
	if err != nil {
		t.Fatal(err)
	}
	return p.WithIndexEngine(engine)
}

// twoAccesses is a member making two vector index accesses.
func twoAccesses(t *testing.T, a, b plans.RecordQueryPlan) expressions.RelationalExpression {
	t.Helper()
	u, err := plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{a, b})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func nonVectorPlan(t *testing.T) expressions.RelationalExpression {
	t.Helper()
	// A plan with no vector index access at all.
	row := values.NewRecordType("VectorRow", false, []values.Field{
		{Name: "embedding", FieldType: values.NewArrayType(false, values.NotNullDouble)},
	})
	scan, err := plans.NewRecordQueryScanPlan([]string{"T"}, row, false)
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

func TestVectorEnginePreference_JavaCases(t *testing.T) {
	t.Parallel()
	hnsw := vectorEnginePlan(t, "hnswIndex", "HNSW")
	guardiann := vectorEnginePlan(t, "guardiannIndex", "GUARDIANN")
	prefs := []string{"", "HNSW", "GUARDIANN"}

	// preferHnswPicksTheHnswPlan / preferGuardiannPicksTheGuardiannPlan.
	if got := compareVectorIndexEnginePreference(hnsw, guardiann, "HNSW"); got >= 0 {
		t.Errorf("PREFER_HNSW: hnsw vs guardiann = %d, want < 0", got)
	}
	if got := compareVectorIndexEnginePreference(guardiann, hnsw, "HNSW"); got <= 0 {
		t.Errorf("PREFER_HNSW: antisymmetric, got %d", got)
	}
	if got := compareVectorIndexEnginePreference(guardiann, hnsw, "GUARDIANN"); got >= 0 {
		t.Errorf("PREFER_GUARDIANN: guardiann vs hnsw = %d, want < 0", got)
	}
	if got := compareVectorIndexEnginePreference(hnsw, guardiann, "GUARDIANN"); got <= 0 {
		t.Errorf("PREFER_GUARDIANN: antisymmetric, got %d", got)
	}
	// noPreferenceFallsThroughToPlanHash: the criterion abstains.
	if got := compareVectorIndexEnginePreference(hnsw, guardiann, ""); got != 0 {
		t.Errorf("NO_PREFERENCE: %d, want abstain", got)
	}

	abstains := func(name string, a, b expressions.RelationalExpression, preferences ...string) {
		t.Helper()
		for _, p := range preferences {
			if got := compareVectorIndexEnginePreference(a, b, p); got != 0 {
				t.Errorf("%s under %q: %d, want abstain", name, p, got)
			}
			if got := compareVectorIndexEnginePreference(b, a, p); got != 0 {
				t.Errorf("%s (reversed) under %q: %d, want abstain", name, p, got)
			}
		}
	}
	// preferenceDoesNotSeparateTwoPlansOfThePreferredEngine.
	abstains("two GUARDIANN plans", vectorEnginePlan(t, "guardiannIndexOne", "GUARDIANN"),
		vectorEnginePlan(t, "guardiannIndexTwo", "GUARDIANN"), "GUARDIANN")
	// preferenceForAbsentEngineChangesNothing.
	abstains("two HNSW plans under PREFER_GUARDIANN", vectorEnginePlan(t, "hnswIndexOne", "HNSW"),
		vectorEnginePlan(t, "hnswIndexTwo", "HNSW"), "GUARDIANN")
	// preferenceDoesNotSeparateAVectorPlanFromANonVectorPlan /
	// preferenceAbstainsWhenOneSideMakesNoVectorAccess.
	nonVector := nonVectorPlan(t)
	abstains("hnsw vs no vector access", hnsw, nonVector, prefs...)
	abstains("guardiann vs no vector access", guardiann, nonVector, prefs...)
	// preferenceAbstainsWhenOneSideMakesSeveralVectorAccesses.
	twoG := twoAccesses(t, vectorEnginePlan(t, "guardiannIndexOne", "GUARDIANN"), vectorEnginePlan(t, "guardiannIndexTwo", "GUARDIANN"))
	abstains("two GUARDIANN accesses vs one HNSW", twoG, hnsw, "GUARDIANN")
	// preferenceDoesNotRewardMoreAccessesOfThePreferredEngine.
	abstains("two GUARDIANN accesses vs one GUARDIANN", twoG, vectorEnginePlan(t, "guardiannIndexThree", "GUARDIANN"), "GUARDIANN")
}
