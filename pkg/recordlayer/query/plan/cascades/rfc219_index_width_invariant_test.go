package cascades

import (
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

func mustRFC219WidthConstruct[T any](value T, err error) T {
	if err != nil {
		panic("construct RFC-219 width fixture: " + err.Error())
	}
	return value
}

// The width invariant measured here is the load-bearing premise behind RFC-219's
// scope-out of Java's CardinalitiesProperty criterion 1
// (CardinalitiesProperty.java:329-336, PK-bound -> atMostOne). Criterion 1 needs
// equalities BINDING primary-key positions; Go's index candidates never fold the
// primary key into the sargable surface (see the GetPKColumnNames contract in
// match_candidate_index.go), so a production index plan can only ever carry
// comparisons on index KEY columns. Stated as a measurable predicate:
//
//	len(plan.GetScanComparisons()) <= len(candidate.GetColumnNames())
//
// THREE of the four construction sites enforce this structurally:
//   - the ValueIndex site sizes comps from sargableAliases, one per key column;
//   - the aggregate site clamps physicalGroupingPrefixCount to len(groupCols);
//   - the nested-loop-join correlated probe hardcodes a single comparison, and
//     its candCols come from plainFieldColumnsForShortcut, whose sole success
//     return is c.columnNames verbatim, so the loop's len(candCols) == 0 guard
//     makes 1 <= len(GetColumnNames()) structural rather than contractual.
//
// The FOURTH — the windowed/rank candidate, whose columnNames and grouping/rank
// aliases are INDEPENDENT constructor parameters with no clamp relating them —
// is the one arm resting on a CALLER CONTRACT. It is also the one arm with no
// production constructor call: NewWindowedIndexScanMatchCandidate has no
// non-test caller, so its unclamped parameter space is not reachable today.
// That makes the invariant stronger than a four-way contract, not weaker, but
// it also means this arm is the first thing to re-check if a production caller
// ever appears.
//
// All four are measured the same way so a future divergence in any of them is
// loud.

// rfc219WidthFailure is the diagnosis a violation carries. It is spelled out
// once because the consequence, not the arithmetic, is the point: a plan wider
// than its index key means criterion 1 has a constructible input in Go.
const rfc219WidthFailure = "an index plan carrying comparisons beyond its index key width makes " +
	"Java's CardinalitiesProperty criterion 1 (PK-bound -> atMostOne) REACHABLE in Go, which makes " +
	"RFC-219 §3's scope-out stale; and folding the primary key into the sargable surface to get " +
	"there re-opens the unmatchedFieldsForIndex / RFC-069 mis-ranking, because the cost model would " +
	"then see PK coordinates as unmatched index columns"

// rfc219IndexPlanIn extracts the single RecordQueryIndexPlan from a plan tree.
// ToScanPlan wraps the index scan in a FetchFromPartialRecordPlan for the value
// and windowed candidates, so the measurement has to look through the wrapper
// rather than assume a bare index plan.
func rfc219IndexPlanIn(t *testing.T, plan plans.RecordQueryPlan) *plans.RecordQueryIndexPlan {
	t.Helper()
	if plan == nil {
		t.Fatal("the site produced NO plan; the width invariant is then measured over nothing, " +
			"which reports green for the wrong reason")
	}
	var found []*plans.RecordQueryIndexPlan
	var walk func(plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		if idx, ok := p.(*plans.RecordQueryIndexPlan); ok {
			found = append(found, idx)
		}
		for _, child := range p.GetChildren() {
			walk(child)
		}
	}
	walk(plan)
	if len(found) != 1 {
		t.Fatalf("expected exactly one RecordQueryIndexPlan in the produced tree, got %d; the "+
			"measurement cannot name which plan it is describing", len(found))
	}
	return found[0]
}

// rfc219EqualityRange is a bound equality range, the widest thing a prefix map
// entry can be: an inequality would truncate the bound prefix and understate the
// comparison count, which is the opposite of what this measurement wants.
func rfc219EqualityRange(t *testing.T, literal int64) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.NewLiteralComparison(predicates.ComparisonEquals, literal)
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatal("Empty + EQUALS must merge; an unbound range here would silently shrink the " +
			"comparison count the invariant is measuring")
	}
	return res.Range
}

// rfc219BindAll binds EVERY sargable alias so the produced comps are maximally
// wide. A partial binding would make the invariant hold trivially.
func rfc219BindAll(
	t *testing.T,
	aliases []values.CorrelationIdentifier,
) map[values.CorrelationIdentifier]*predicates.ComparisonRange {
	t.Helper()
	if len(aliases) == 0 {
		t.Fatal("a candidate with no sargable aliases cannot exercise the width invariant")
	}
	m := make(map[values.CorrelationIdentifier]*predicates.ComparisonRange, len(aliases))
	for i, alias := range aliases {
		m[alias] = rfc219EqualityRange(t, int64(i+1))
	}
	return m
}

func rfc219Aliases(n int) []values.CorrelationIdentifier {
	out := make([]values.CorrelationIdentifier, n)
	for i := range out {
		out[i] = values.UniqueCorrelationIdentifier()
	}
	return out
}

func TestRFC219_IndexPlanWidthInvariant(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// build returns the candidate's advertised index key columns and the
		// index plan the site actually constructs for them.
		build func(t *testing.T) (columnNames []string, plan *plans.RecordQueryIndexPlan)
	}{
		{
			// Site 1: comps are sized from sargableAliases, documented as one
			// per index key column. EQUALITY is expected here, not just <=.
			name: "matchCandidateIndex",
			build: func(t *testing.T) ([]string, *plans.RecordQueryIndexPlan) {
				t.Helper()
				cols := []string{"CUSTOMER_ID", "STATUS", "AMOUNT"}
				aliases := rfc219Aliases(len(cols))
				rowType := testRecordRowType("ORDERS", "CUSTOMER_ID", "STATUS", "AMOUNT", "ID")
				cand := newKnownDistinctValueIndexCandidate(
					"ORDERS$CUST_STATUS_AMOUNT",
					[]string{"ORDERS"},
					cols,
					aliases,
					rowType,
					false,
					[]string{"ID"},
				)
				prefixMap := rfc219BindAll(t, cand.GetSargableAliases())
				return cand.GetColumnNames(), rfc219IndexPlanIn(t, cand.ToScanPlan(prefixMap, false))
			},
		},
		{
			// Site 2: columnNames and (groupingAliases, rankAlias) are
			// INDEPENDENT constructor parameters. Nothing clamps one to the
			// other, so this arm measures a caller contract rather than a clamp.
			//
			// There is no production shape to compare against: this candidate's
			// constructor has no non-test caller, so ToScanPlan is production
			// code on a currently unreachable path. The shape below is therefore
			// the canonical rank-index shape — a rank index over (GROUP, RANK),
			// two sargable positions and two key columns — not an observed
			// production one. If a production caller ever appears, this arm is
			// the first to re-check, because its parameter space is unclamped.
			name: "windowedIndexCandidate",
			build: func(t *testing.T) ([]string, *plans.RecordQueryIndexPlan) {
				t.Helper()
				rowType := testRecordRowType("LEADER", "TEAM", "SCORE", "ID")
				cols := []string{"TEAM", "SCORE"}
				cand := NewWindowedIndexScanMatchCandidate(
					"LEADER$SCORE_BY_TEAM",
					[]string{"LEADER"},
					cols,
					rfc219Aliases(1),                     // grouping: TEAM
					values.UniqueCorrelationIdentifier(), // score
					values.UniqueCorrelationIdentifier(), // rank
					rfc219Aliases(1),                     // primary key
					nil,
					rowType,
					false,
					[]string{"ID"},
				)
				prefixMap := rfc219BindAll(t, cand.GetSargableAliases())
				return cand.GetColumnNames(), rfc219IndexPlanIn(t, cand.ToScanPlan(prefixMap, false))
			},
		},
		{
			// Site 3: comps are appended at most physicalGroupingPrefixCount
			// times, and that count is clamped to len(groupCols) at construction.
			// The clamp is exercised deliberately: an over-large count is passed
			// so the arm measures the clamp rather than a caller who happened to
			// pass a well-formed number.
			name: "aggregateIndexCandidate",
			build: func(t *testing.T) ([]string, *plans.RecordQueryIndexPlan) {
				t.Helper()
				groupCols := []string{"CUSTOMER_ID", "STATUS"}
				rowType := testRecordRowType(
					"ORDERS", "CUSTOMER_ID", "STATUS", "AMOUNT", "ID")
				cand := NewAggregateIndexMatchCandidate(
					"SUM_AMOUNT_BY_CUSTOMER_STATUS",
					[]string{"ORDERS"},
					groupCols,
					expressions.AggSum,
					"AMOUNT",
					rowType,
					[]values.Type{values.NullableLong, values.NullableString},
					len(groupCols)+3,
				)
				prefixMap := rfc219BindAll(t, cand.GetSargableAliases())
				return cand.GetColumnNames(), rfc219IndexPlanIn(t, cand.ToScanPlan(prefixMap, false))
			},
		},
		{
			// Site 4: the correlated EXISTS probe in the nested-loop-join rule
			// hardcodes exactly one comparison against the candidate's FIRST
			// column, guarded by len(candCols) != 0. Because candCols is
			// plainFieldColumnsForShortcut's verbatim c.columnNames, that guard
			// makes 1 <= len(GetColumnNames()) STRUCTURAL, not contractual.
			// This arm builds the plan the same way the rule does and measures it.
			name: "nestedLoopJoinCorrelatedProbe",
			build: func(t *testing.T) ([]string, *plans.RecordQueryIndexPlan) {
				t.Helper()
				cols := []string{"CUSTOMER_ID", "STATUS"}
				rowType := testRecordRowType("ORDERS", "CUSTOMER_ID", "STATUS", "ID")
				cand := newKnownDistinctValueIndexCandidate(
					"ORDERS$CUST_STATUS",
					[]string{"ORDERS"},
					cols,
					rfc219Aliases(len(cols)),
					rowType,
					false,
					[]string{"ID"},
				)
				candCols, plainFields := candidatePlainFieldColumnsForShortcut(cand)
				if !plainFields || len(candCols) == 0 {
					t.Fatal("the shortcut arm must accept this plain scalar candidate; if it " +
						"declines, the site is never reached and this arm measures nothing")
				}
				// The comparison the rule builds from the outer correlation.
				comparisonRange := rfc219EqualityRange(t, 7)
				plan := stampIndexMetadata(cand, mustRFC219WidthConstruct(plans.NewRecordQueryIndexPlan(
					cand.CandidateName(),
					[]*predicates.ComparisonRange{comparisonRange},
					cand.GetRecordTypes(),
					rowType,
					false,
				)))
				return cand.GetColumnNames(), plan
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			columnNames, plan := tc.build(t)
			comps := plan.GetScanComparisons()
			t.Logf("site %s: len(comps)=%d len(columnNames)=%d", tc.name, len(comps), len(columnNames))
			if len(columnNames) == 0 {
				t.Fatal("a candidate advertising zero key columns makes the invariant vacuous; " +
					"the arm must build a real multi-column index")
			}
			if len(comps) > len(columnNames) {
				t.Fatalf("width invariant VIOLATED at %s: len(scanComparisons)=%d > len(columnNames)=%d — %s",
					tc.name, len(comps), len(columnNames), rfc219WidthFailure)
			}
		})
	}
}

// TestRFC219_IndexPlanWidthInvariant_MutationGuard is the positive control for
// the four arms above. Without it, four greens are uninterpretable: an assertion
// that cannot fire is indistinguishable from an invariant that holds. The plan
// here is hand-built, deliberately bypassing every production construction path,
// so it proves the PREDICATE detects a violation — not that production can
// produce one.
func TestRFC219_IndexPlanWidthInvariant_MutationGuard(t *testing.T) {
	t.Parallel()

	cols := []string{"CUSTOMER_ID"}
	rowType := testRecordRowType("ORDERS", "CUSTOMER_ID", "ID")
	cand := newKnownDistinctValueIndexCandidate(
		"ORDERS$CUST",
		[]string{"ORDERS"},
		cols,
		rfc219Aliases(len(cols)),
		rowType,
		false,
		[]string{"ID"},
	)

	// Two comparisons over a one-column index: the exact shape a PK-folding
	// sargable surface would produce, which is what criterion 1 would need.
	overWide := mustRFC219WidthConstruct(plans.NewRecordQueryIndexPlan(
		cand.CandidateName(),
		[]*predicates.ComparisonRange{
			rfc219EqualityRange(t, 1),
			rfc219EqualityRange(t, 2),
		},
		cand.GetRecordTypes(),
		rowType,
		false,
	))

	found := rfc219IndexPlanIn(t, overWide)
	comps := found.GetScanComparisons()
	columnNames := cand.GetColumnNames()
	t.Logf("mutation guard: len(comps)=%d len(columnNames)=%d", len(comps), len(columnNames))

	if !(len(comps) > len(columnNames)) {
		t.Fatalf("the positive control did NOT exceed the index key width (comps=%d, columnNames=%d); "+
			"the four production arms are then unfalsifiable, because an assertion that never fires "+
			"reports green whether or not the invariant holds — %s",
			len(comps), len(columnNames), rfc219WidthFailure)
	}
}
