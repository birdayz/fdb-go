package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// makeLegOverColumns builds a leg whose index key is `columns` in order: the
// first numEquality columns are equality-bound by the scan, the rest continue
// the key as sorted parts. Unlike makeDataAccessTestPartialMatchWithPK, the
// columns are named explicitly so a leg can bind a PRIMARY-KEY column by
// equality — the shape of an index that repeats a primary-key component (a
// value index ON (pk2), or ON (b, pk1), whose entry is trimmed of the components
// it already carries).
func makeLegOverColumns(
	t testing.TB,
	name string,
	plan plans.RecordQueryPlan,
	columns []string,
	numEquality int,
) *testPartialMatch {
	t.Helper()
	eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, int64(1))
	eqRange := predicates.EmptyComparisonRange().Merge(&eqCmp).Range

	sargAliases := make([]values.CorrelationIdentifier, 0, numEquality)
	parts := make([]*MatchedOrderingPart, 0, len(columns))
	paramBindings := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, numEquality)
	for i, col := range columns {
		pid := values.UniqueCorrelationIdentifier()
		if i < numEquality {
			sargAliases = append(sargAliases, pid)
			paramBindings[pid] = eqRange
			parts = append(parts, NewMatchedOrderingPart(pid, dataAccessTestKey(col), eqRange, MatchedSortOrderAscending))
			continue
		}
		parts = append(parts, NewMatchedOrderingPart(pid, dataAccessTestKey(col), nil, MatchedSortOrderAscending))
	}
	return &testPartialMatch{
		candidate: &dataAccessTestCandidate{
			name:              name,
			sargableAliases:   sargAliases,
			columnNames:       columns[:numEquality],
			keyComponentTypes: syntheticIndexKeyTypes(numEquality),
			recordTypes:       []string{"TestRecord"},
			fixedPlan:         plan,
		},
		matchInfo: &testMatchInfo{
			orderingParts: parts,
			paramBindings: paramBindings,
		},
	}
}

func intersectionPlansOf(t testing.TB, result *IntersectionResult) []*plans.RecordQueryIntersectionPlan {
	t.Helper()
	var out []*plans.RecordQueryIntersectionPlan
	for _, e := range result.GetExpressions() {
		ip, ok := e.(*plans.RecordQueryIntersectionPlan)
		if !ok {
			t.Fatalf("expected intersection plan, got %T", e)
		}
		out = append(out, ip)
	}
	return out
}

// TestIntersector_DeclinesPrimaryKeyComponentFixedInOneLegOnly pins the
// merge-soundness proof leg by leg. PRIMARY KEY (ID, VERSION); leg A is an index
// ON (A, ID) — equality on A, key continues (ID, VERSION); leg B is an index ON
// (VERSION) — equality on VERSION, key continues (ID). The only comparison key the
// merged ordering can enumerate is (ID): VERSION is fixed in leg B, so the
// intersection ordering carries it as a constant. But leg A's stream holds
// several records per ID that differ only in VERSION, so aligning the legs on
// (ID) emits leg-A records whose VERSION is not the one leg B is bound to.
//
// Java's isCompatibleComparisonKey accepts this key because it subtracts the
// UNION of the legs' equality-bound values from the primary key; that is the
// upstream defect measured in
// conformance/pk_intersection_leg_bound_key_java_probe_test.go. The Go proof is
// per leg, so this partition yields no intersection at all.
func TestIntersector_DeclinesPrimaryKeyComponentFixedInOneLegOnly(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"),
		[]string{"A", "ID", "VERSION"}, 1)
	legB := makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"),
		[]string{"VERSION", "ID"}, 1)
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeVectoredAccess(legB, 1),
	}, nil)
	if got := intersectionPlansOf(t, result); len(got) != 0 {
		var keys []string
		for _, kv := range got[0].GetComparisonKeyValues() {
			keys = append(keys, values.ExplainValue(kv))
		}
		t.Fatalf("intersection built over a leg that fixes VERSION and a leg that sorts it; "+
			"comparison key %v cannot identify a record in the (A, ID, VERSION) leg, so the "+
			"merge returns records whose VERSION the other leg never matched", keys)
	}
}

// TestIntersector_AcceptsPrimaryKeyComponentFixedInEveryLeg is the control: when
// EVERY leg fixes VERSION, the key (ID) does identify a record in each leg's
// stream and the intersection is sound. A proof that declined this too would be
// declining every intersection whose legs share an equality, not the unsound one.
func TestIntersector_AcceptsPrimaryKeyComponentFixedInEveryLeg(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"),
		[]string{"A", "VERSION", "ID"}, 2)
	legB := makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"),
		[]string{"B", "VERSION", "ID"}, 2)
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeVectoredAccess(legB, 1),
	}, nil)
	got := intersectionPlansOf(t, result)
	if len(got) == 0 {
		t.Fatal("no intersection built although both legs fix VERSION and continue with ID: " +
			"the per-leg proof is declining a sound merge")
	}
	for _, ip := range got {
		keys := ip.GetComparisonKeyValues()
		if len(keys) != 1 {
			t.Fatalf("comparison key should be (ID) alone — VERSION is a constant of every leg — got %d keys", len(keys))
		}
		fv, ok := values.AsFieldValue(keys[0])
		if !ok || fv.Path() == nil || fv.Path().Len() != 1 || fv.Path().Ordinals()[0] != dataAccessTestOrdinal("ID") {
			t.Fatalf("comparison key is not the ID column: %s", values.ExplainValue(keys[0]))
		}
	}
}

// TestIntersector_ThreeWayKeepsSoundPairDropsLegFixingPkComponent: with a third
// leg present the sound pair (A ∧ B over (ID, VERSION)) must still be built while
// every partition that includes the VERSION-fixing leg is declined.
func TestIntersector_ThreeWayKeepsSoundPairDropsLegFixingPkComponent(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"), []string{"A", "ID", "VERSION"}, 1)
	legB := makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"), []string{"B", "ID", "VERSION"}, 1)
	legV := makeLegOverColumns(t, "idxC", mustDataAccessTestPlan(t, "scanC"), []string{"VERSION", "ID"}, 1)
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeVectoredAccess(legB, 1),
		makeVectoredAccess(legV, 2),
	}, nil)
	got := intersectionPlansOf(t, result)
	if len(got) == 0 {
		t.Fatal("the sound A ∧ B pair was not built")
	}
	for _, ip := range got {
		if n := len(ip.GetChildren()); n != 2 {
			t.Fatalf("an intersection with %d legs was built; only the A ∧ B pair is sound", n)
		}
		for _, child := range ip.GetChildren() {
			if tp, ok := child.(*testPlan); ok && tp.name == "scanC" {
				t.Fatal("the VERSION-fixing leg (scanC) is inside an intersection")
			}
		}
		if len(ip.GetComparisonKeyValues()) != 2 {
			t.Fatalf("A ∧ B must compare on (ID, VERSION); got %d keys", len(ip.GetComparisonKeyValues()))
		}
	}
}
