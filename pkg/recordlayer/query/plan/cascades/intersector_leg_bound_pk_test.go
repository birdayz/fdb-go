package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// makeLegOverColumns builds a leg whose index key is `columns` in order: the
// first numEquality columns are equality-bound by the scan to the literal 1,
// the rest continue the key as sorted parts. Unlike
// makeDataAccessTestPartialMatchWithPK, the columns are named explicitly so a
// leg can bind a PRIMARY-KEY column by equality — the shape of an index that
// repeats a primary-key component (a value index ON (pk2), or ON (b, pk1),
// whose entry is trimmed of the components it already carries).
func makeLegOverColumns(
	t testing.TB,
	name string,
	plan plans.RecordQueryPlan,
	columns []string,
	numEquality int,
) *testPartialMatch {
	t.Helper()
	return makeLegOverColumnsBoundTo(t, name, plan, columns, numEquality, int64(1))
}

// makeLegOverColumnsBoundTo is makeLegOverColumns with the equality literal
// chosen, so two legs can fix the same primary-key component to DIFFERENT
// constants.
func makeLegOverColumnsBoundTo(
	t testing.TB,
	name string,
	plan plans.RecordQueryPlan,
	columns []string,
	numEquality int,
	literal any,
) *testPartialMatch {
	t.Helper()
	eqCmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	eqRange := predicates.EmptyComparisonRange().Merge(&eqCmp).Range
	return makeLegOverColumnsWithRange(t, name, plan, columns, numEquality, eqRange)
}

// makeLegOverColumnsWithRange binds the first numEquality columns to the given
// equality range — an IS NULL range, for the arm that needs one.
func makeLegOverColumnsWithRange(
	t testing.TB,
	name string,
	plan plans.RecordQueryPlan,
	columns []string,
	numEquality int,
	eqRange *predicates.ComparisonRange,
) *testPartialMatch {
	t.Helper()
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

// makeReverseVectoredAccess is makeVectoredAccess for a leg scanned in reverse:
// its sorted parts flow DESCENDING.
func makeReverseVectoredAccess(pm *testPartialMatch, position int) Vectored[*SingleMatchedAccess] {
	access := NewSingleMatchedAccess(pm, NoCompensation, values.UniqueCorrelationIdentifier(),
		true, EmptyTranslationMap(), nil)
	return NewVectored(access, position)
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

// comparisonKeyNames renders an intersection's comparison key as column names,
// in key order.
func comparisonKeyNames(t testing.TB, ip *plans.RecordQueryIntersectionPlan) []string {
	t.Helper()
	var names []string
	for _, kv := range ip.GetComparisonKeyValues() {
		fv, ok := values.AsFieldValue(kv)
		if !ok {
			t.Fatalf("comparison key part %s is not a column", values.ExplainValue(kv))
		}
		names = append(names, fv.DisplayName())
	}
	return names
}

func legNames(ip *plans.RecordQueryIntersectionPlan) []string {
	var names []string
	for _, child := range ip.GetChildren() {
		if tp, ok := child.(*testPlan); ok {
			names = append(names, tp.name)
		}
	}
	return names
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestIntersector_ComparesOnTheComponentOneLegFixes pins the merge-soundness
// proof leg by leg AND the key it produces. PRIMARY KEY (ID, VERSION); leg A is
// an index ON (A, ID) — equality on A, key continues (ID, VERSION); leg B is an
// index ON (VERSION) — equality on VERSION, key continues (ID). VERSION is
// constant in leg B and varies in leg A, so a merge on (ID) alone would emit
// leg-A records whose VERSION leg B never matched (Java's isCompatibleComparisonKey
// accepts that key; the upstream defect is measured in
// conformance/pk_intersection_leg_bound_key_java_probe_test.go). The sound
// merge compares on (ID, VERSION): both legs deliver that order — leg A by its
// key, leg B trivially — and it is the ONLY key built, in that order.
//
// "Exactly one, in that order" is the pin of the directed gate
// (everyLegDeliversComparisonKey). The merged ordering binds VERSION FIXED and
// carries no edge for it, so the enumeration also offers (VERSION, ID); leg A
// does not deliver that order, and without the gate a second intersection is
// built on it.
func TestIntersector_ComparesOnTheComponentOneLegFixes(t *testing.T) {
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
	got := intersectionPlansOf(t, result)
	if len(got) != 1 {
		var keys [][]string
		for _, ip := range got {
			keys = append(keys, comparisonKeyNames(t, ip))
		}
		t.Fatalf("%d intersections built with keys %v, want exactly one on (ID, VERSION): "+
			"a key that omits VERSION cannot identify a record in the (A, ID, VERSION) leg, and "+
			"(VERSION, ID) is an order that leg does not deliver", len(got), keys)
	}
	if keys := comparisonKeyNames(t, got[0]); !sameStrings(keys, []string{"ID", "VERSION"}) {
		t.Fatalf("comparison key = %v, want (ID, VERSION) in that order", keys)
	}
	if got[0].IsReverse() {
		t.Fatal("two forward legs merged in reverse")
	}
}

// TestIntersector_AcceptsPrimaryKeyComponentFixedInEveryLeg is the control: when
// EVERY leg fixes VERSION to the SAME constant, the key (ID) does identify a
// record in each leg's stream and the intersection is sound. A proof that
// declined this too would be declining every intersection whose legs share an
// equality, not the unsound one — and one that widened it would be comparing a
// value it has already proved constant across the legs.
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
		t.Fatal("no intersection built although both legs fix VERSION to the same constant and continue with ID: " +
			"the per-leg proof is declining a sound merge")
	}
	for _, ip := range got {
		if keys := comparisonKeyNames(t, ip); !sameStrings(keys, []string{"ID"}) {
			t.Fatalf("comparison key should be (ID) alone — VERSION is the same constant in every leg — got %v", keys)
		}
	}
}

// TestIntersector_ComparesOnTheComponentLegsFixToDifferentConstants: the other
// half of "fixed in every leg". Leg A fixes VERSION = 1 and leg B fixes
// VERSION = 2; each leg's stream is constant in VERSION, but not the SAME
// constant, so records (id = 1, version = 1) from A and (id = 1, version = 2)
// from B are different records that a key of (ID) would align — and the merge
// would emit one for a conjunction whose result is empty. Red at 64a737edd,
// which built exactly that (ID) merge. VERSION must be compared; being
// constant within each leg it may sit at either position, and every key built
// carries both components.
func TestIntersector_ComparesOnTheComponentLegsFixToDifferentConstants(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumnsBoundTo(t, "idxA", mustDataAccessTestPlan(t, "scanA"),
		[]string{"A", "VERSION", "ID"}, 2, int64(1))
	legB := makeLegOverColumnsBoundTo(t, "idxB", mustDataAccessTestPlan(t, "scanB"),
		[]string{"B", "VERSION", "ID"}, 2, int64(2))
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeVectoredAccess(legB, 1),
	}, nil)
	got := intersectionPlansOf(t, result)
	if len(got) == 0 {
		t.Fatal("no intersection built: comparing on (ID, VERSION) is sound here (the merge finds nothing, correctly)")
	}
	for _, ip := range got {
		keys := comparisonKeyNames(t, ip)
		if len(keys) != 2 || !((keys[0] == "ID" && keys[1] == "VERSION") || (keys[0] == "VERSION" && keys[1] == "ID")) {
			t.Fatalf("comparison key = %v, want both components: the legs fix VERSION to different constants, "+
				"so equal (ID) does not mean the same record", keys)
		}
	}
}

// TestIntersector_ThreeWayBuildsTheWidestPartitionOnBothComponents: with the
// VERSION-fixing leg admitted, every partition is sound and the redundancy
// pruning keeps the widest — measured: exactly one intersection, over all
// three legs, comparing on (ID, VERSION). Before RFC-247 the only sound
// partition was the A ∧ B pair (two children, scanC in no intersection); those
// assertions flip here to three children including scanC.
func TestIntersector_ThreeWayBuildsTheWidestPartitionOnBothComponents(t *testing.T) {
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
	if len(got) != 1 {
		t.Fatalf("%d intersections built, want exactly one (the redundancy pruning keeps the widest sound partition)", len(got))
	}
	if legs := legNames(got[0]); !sameStrings(legs, []string{"scanA", "scanB", "scanC"}) {
		t.Fatalf("intersection legs = %v, want all three: the VERSION-fixing leg is sound to merge on (ID, VERSION)", legs)
	}
	if keys := comparisonKeyNames(t, got[0]); !sameStrings(keys, []string{"ID", "VERSION"}) {
		t.Fatalf("comparison key = %v, want (ID, VERSION)", keys)
	}
}

// TestIntersector_DeclinesWhenAComponentIsOrderedOnlyWithinAnUncomparableValue:
// leg V' is an index ON (VERSION, SORT_KEY) — equality on VERSION, key
// continues (SORT_KEY, ID) — so its ID is ordered only within SORT_KEY, and
// SORT_KEY is no primary-key component leg A can compare. The offer (VERSION) fails the identification
// proof (ID must be compared), and ID is not in the merged ordering at all (its
// order in leg A is not its order in leg V'), so nothing sound can be offered:
// no intersection.
func TestIntersector_DeclinesWhenAComponentIsOrderedOnlyWithinAnUncomparableValue(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"), []string{"A", "ID", "VERSION"}, 1)
	legV := makeLegOverColumns(t, "idxV", mustDataAccessTestPlan(t, "scanV"), []string{"VERSION", "SORT_KEY", "ID"}, 1)
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeVectoredAccess(legV, 1),
	}, nil)
	if got := intersectionPlansOf(t, result); len(got) != 0 {
		t.Fatalf("intersection built on %v over a leg whose ID is ordered only within SORT_KEY", comparisonKeyNames(t, got[0]))
	}
}

// TestIntersector_DeclinesWhenTheLegsSortTheFreeComponentOppositely: the
// VERSION-fixing leg is scanned in reverse, so its ID descends while leg A's
// ascends. ID never enters the merged ordering, the only offer is (VERSION),
// and the identification proof declines it: no intersection.
func TestIntersector_DeclinesWhenTheLegsSortTheFreeComponentOppositely(t *testing.T) {
	t.Parallel()

	legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"), []string{"A", "ID", "VERSION"}, 1)
	legB := makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"), []string{"VERSION", "ID"}, 1)
	ctx := newTestPKContext("TestRecord", []string{"id", "version"})

	result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
		makeVectoredAccess(legA, 0),
		makeReverseVectoredAccess(legB, 1),
	}, nil)
	if got := intersectionPlansOf(t, result); len(got) != 0 {
		t.Fatalf("intersection built on %v (reverse=%v) over legs whose ID orders disagree", comparisonKeyNames(t, got[0]), got[0].IsReverse())
	}
}

// TestIntersector_WidenedPartOrderingClaimIsVacuousByConstancy: the intersection
// plan's HintOrdering claims its comparison key with a direction and NULLS
// placement per part (plans/ordering.go). For the widened VERSION that claim
// is vacuous: the merged ordering binds VERSION FIXED because leg B fixes it,
// so every emitted row carries leg B's one value and any direction is
// trivially satisfied — including when that one value is NULL, the IS NULL
// arm. The pin is the pair of facts together: the hint claims [ID ASC,
// VERSION ASC], and the merged ordering the intersector reasons over binds
// VERSION FIXED.
func TestIntersector_WidenedPartOrderingClaimIsVacuousByConstancy(t *testing.T) {
	t.Parallel()

	isNull := predicates.EmptyComparisonRange().Merge(&predicates.Comparison{Type: predicates.ComparisonIsNull}).Range
	for _, tc := range []struct {
		name  string
		legB  *testPartialMatch
		fixed string
	}{
		{"literal", makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"), []string{"VERSION", "ID"}, 1), "= 1"},
		{"is_null", makeLegOverColumnsWithRange(t, "idxB", mustDataAccessTestPlan(t, "scanB"), []string{"VERSION", "ID"}, 1, isNull), "IS NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"), []string{"A", "ID", "VERSION"}, 1)
			ctx := newTestPKContext("TestRecord", []string{"id", "version"})
			result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
				makeVectoredAccess(legA, 0),
				makeVectoredAccess(tc.legB, 1),
			}, nil)
			got := intersectionPlansOf(t, result)
			if len(got) != 1 {
				t.Fatalf("%d intersections built, want one on (ID, VERSION) with VERSION %s in leg B", len(got), tc.fixed)
			}
			hint := got[0].HintOrdering()
			if !hint.IsKnown || len(hint.Keys) != 2 || hint.DescendingAt(0) || hint.DescendingAt(1) {
				t.Fatalf("HintOrdering = %#v, want [ID ASC, VERSION ASC]: the widened part carries the comparison direction", hint)
			}
			merged := result.GetCommonOrdering()
			if merged == nil {
				t.Fatal("no common ordering recorded")
			}
			if _, fixed := merged.GetEqualityBoundValues()[mergedKeyNamed(t, merged, "VERSION")]; !fixed {
				t.Fatalf("VERSION is not equality-bound in the merged ordering; the direction claim on it would not be vacuous")
			}
		})
	}
}

// mergedKeyNamed finds the merged ordering's own key object for a column.
func mergedKeyNamed(t testing.TB, o *properties.RichOrdering, name string) values.Value {
	t.Helper()
	for _, key := range o.GetKeys() {
		if fv, ok := values.AsFieldValue(key); ok && fv.DisplayName() == name {
			return key
		}
	}
	t.Fatalf("merged ordering has no key %s", name)
	return nil
}

// TestPrimaryKeyComponentsToCompare_OmitsOnlyWhatEveryLegFixesAlike drives the
// omission proof directly over hand-built leg orderings, one arm per branch of
// legFixedComparison. "Fixed alike" is the only way a primary-key component
// leaves the comparison key, and every way the proof cannot read a leg's
// binding must land on "compare it": a leg that SORTS the component; legs that
// fix it to different comparisons; a leg whose FIXED payload is not a
// *predicates.Comparison (a hand-built ordering stores a string; the plans
// package stores a *ComparisonRange) — reading that as the nil implicit
// payload would make two different constants compare "nil == nil" and drop the
// component; a leg carrying two bindings for it. Only the same comparison in
// every leg, or the nil payload the implicit record-type component carries in
// every leg, omits it.
func TestPrimaryKeyComponentsToCompare_OmitsOnlyWhatEveryLegFixesAlike(t *testing.T) {
	t.Parallel()

	id := dataAccessTestKey("ID")
	version := dataAccessTestKey("VERSION")
	eq := func(lit int64) *predicates.Comparison {
		cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, lit)
		return &cmp
	}
	leg := func(versionBindings ...properties.OrderingBinding) *properties.RichOrdering {
		return properties.NewRichOrdering(
			map[values.Value][]properties.OrderingBinding{
				version: versionBindings,
				id:      {properties.SortedBinding(properties.ProvidedSortOrderAscending)},
			},
			[]values.Value{version, id},
			properties.NotDistinct())
	}
	fixed := func(payload any) properties.OrderingBinding { return properties.FixedBinding(payload) }
	sorted := properties.SortedBinding(properties.ProvidedSortOrderAscending)

	for _, tc := range []struct {
		name        string
		legs        []*properties.RichOrdering
		wantVersion bool // VERSION must be compared
	}{
		{"same comparison in every leg: omitted", []*properties.RichOrdering{leg(fixed(eq(1))), leg(fixed(eq(1)))}, false},
		{"nil (implicit) payload in every leg: omitted", []*properties.RichOrdering{leg(fixed(nil)), leg(fixed(nil))}, false},
		{"different comparisons: compared", []*properties.RichOrdering{leg(fixed(eq(1))), leg(fixed(eq(2)))}, true},
		{"sorted in one leg: compared", []*properties.RichOrdering{leg(fixed(eq(1))), leg(sorted)}, true},
		{"unreadable payload in one leg: compared, not read as implicit", []*properties.RichOrdering{leg(fixed(nil)), leg(fixed("version = 2"))}, true},
		{"unreadable payload in every leg: compared", []*properties.RichOrdering{leg(fixed("version = 1")), leg(fixed("version = 2"))}, true},
		{"two bindings in one leg: compared", []*properties.RichOrdering{leg(fixed(eq(1))), leg(fixed(eq(1)), fixed(eq(1)))}, true},
		{"typed nil comparison: compared", []*properties.RichOrdering{leg(fixed((*predicates.Comparison)(nil))), leg(fixed((*predicates.Comparison)(nil)))}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			must := primaryKeyComponentsToCompare([]values.Value{id, version}, tc.legs)
			if !containsIntersectionValue(must, id) {
				t.Fatal("ID is sorted in every leg and must always be compared")
			}
			if got := containsIntersectionValue(must, version); got != tc.wantVersion {
				t.Fatalf("VERSION must be compared = %v, want %v: an omission the proof cannot justify is the "+
					"unsound single-component merge", got, tc.wantVersion)
			}
		})
	}
}

// TestIntersector_WidenedPartIgnoresItsRequestedDirection: a request that
// names the widened component in the OTHER direction from the legs' sort —
// [VERSION DESC, ID ASC] over legs that deliver [ID ASC, VERSION ASC] and
// [ID ASC, VERSION fixed] — must still build the forward merge on
// (ID, VERSION). VERSION is constant in every emitted row, so its requested
// direction is satisfied by the output whatever the merge does with it; the
// merge takes its direction from ID. Stamping VERSION DESC from the request
// produced the mixed key [ID ASC, VERSION DESC], which no leg delivers, and
// forfeited the merge. The reverse request [ID DESC, VERSION ASC] over reverse
// legs likewise merges in reverse.
func TestIntersector_WidenedPartIgnoresItsRequestedDirection(t *testing.T) {
	t.Parallel()

	req := func(parts ...properties.RequestedOrderingPart) *properties.RequestedOrdering {
		return properties.NewRequestedOrdering(parts, properties.DistinctnessNotDistinct, false)
	}
	for _, tc := range []struct {
		name        string
		requested   *properties.RequestedOrdering
		reverseLegs bool
		wantReverse bool
	}{
		{
			"version desc then id asc, forward legs",
			req(
				properties.RequestedOrderingPart{Value: dataAccessTestKey("VERSION"), SortOrder: properties.RequestedSortOrderDescending},
				properties.RequestedOrderingPart{Value: dataAccessTestKey("ID"), SortOrder: properties.RequestedSortOrderAscending},
			),
			false, false,
		},
		{
			"id asc then version desc, forward legs",
			req(
				properties.RequestedOrderingPart{Value: dataAccessTestKey("ID"), SortOrder: properties.RequestedSortOrderAscending},
				properties.RequestedOrderingPart{Value: dataAccessTestKey("VERSION"), SortOrder: properties.RequestedSortOrderDescending},
			),
			false, false,
		},
		{
			"id desc then version asc, reverse legs",
			req(
				properties.RequestedOrderingPart{Value: dataAccessTestKey("ID"), SortOrder: properties.RequestedSortOrderDescending},
				properties.RequestedOrderingPart{Value: dataAccessTestKey("VERSION"), SortOrder: properties.RequestedSortOrderAscending},
			),
			true, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			legA := makeLegOverColumns(t, "idxA", mustDataAccessTestPlan(t, "scanA"), []string{"A", "ID", "VERSION"}, 1)
			legB := makeLegOverColumns(t, "idxB", mustDataAccessTestPlan(t, "scanB"), []string{"VERSION", "ID"}, 1)
			ctx := newTestPKContext("TestRecord", []string{"id", "version"})
			mk := makeVectoredAccess
			if tc.reverseLegs {
				mk = makeReverseVectoredAccess
			}
			result := WithPrimaryKeyIntersector(ctx)([]Vectored[*SingleMatchedAccess]{
				mk(legA, 0), mk(legB, 1),
			}, []*properties.RequestedOrdering{tc.requested})
			got := intersectionPlansOf(t, result)
			if len(got) != 1 {
				t.Fatalf("%d intersections built, want one: the widened VERSION's requested direction is vacuous "+
					"(constant in every emitted row) and must not forfeit the merge", len(got))
			}
			if keys := comparisonKeyNames(t, got[0]); !sameStrings(keys, []string{"ID", "VERSION"}) {
				t.Fatalf("comparison key = %v, want (ID, VERSION)", keys)
			}
			if got[0].IsReverse() != tc.wantReverse {
				t.Fatalf("reverse = %v, want %v: the merge direction comes from ID, the component the legs sort", got[0].IsReverse(), tc.wantReverse)
			}
			for _, part := range got[0].GetComparisonKeyOrderingParts() {
				if part.SortOrder.IsAnyDescending() != tc.wantReverse {
					t.Fatalf("part %s has direction %v; every part must carry the merge direction", values.ExplainValue(part.Value), part.SortOrder)
				}
			}
		})
	}
}
