package cascades

// This file is a STANDING cross-check between the two independent row-count
// derivations this planner carries:
//
//   - computeCardinalities / CardinalitiesProperty (plan_properties.go,
//     properties/cardinality.go) — a faithful port of Java's
//     CardinalitiesProperty. STRUCTURAL min/max bounds, or an explicit
//     unknown. Never statistics. Trustworthy by construction: e.g. a unique
//     index with every column equality-bound PROVES max=1, not "probably 1".
//   - The cost model (properties/cost_formulas.go, plans/cost.go's HintCost
//     methods, planning_cost_model.go's concretePlanCost) — a Go-only
//     extension with no Java counterpart, producing a POINT ESTIMATE from
//     flat selectivity constants (DistinctSelectivity, FilterSelectivity, …).
//
// A point estimate that exceeds a PROVEN max, or falls below a PROVEN min, is
// wrong by construction — no statistics needed to know it. Two independent
// derivations of "how many rows" can disagree with each other silently
// because nothing checks them against each other; this file is that check.
//
// Design: build real physical plan trees with the PRODUCTION constructors
// (plans.NewXxxPlan), which wire real Reference/Quantifier edges via
// plans.QuantifierOverPlan internally. For each shape, compute BOTH:
//   - the property's bounds, via the SAME priming sequence
//     computeRefPlanProperties runs in production (primeCardinalitiesProperty
//     below primes every child Reference bottom-up, then computeCardinalities
//     reads them exactly as computeWrapperProperties does);
//   - the cost model's point-estimate Cardinality, via the SAME recursion the
//     memo's winner selection uses (properties.EstimateCost).
//
// Assert the estimate falls in [provenMin, provenMax] whenever the property
// proves a bound. Where the property is unknown, nothing is asserted, but the
// gap is real: see the per-shape comments below for where the cost model
// flies blind.
//
// There are ZERO exclusions, and there is no mechanism to add one. This file
// once carried six documented-and-not-fixed violations under a self-cleaning
// exclusion list; RFC-195 fixed all six at the root (the cost walks clamp every
// estimate into the interval the property proves, at their combine step) and
// deleted the list along with them. The absence of an exclusion mechanism is
// deliberate: six violations accumulated precisely because there was somewhere
// to write them down, and each formula that got "this operator guarantees a
// row" wrong got it wrong independently. An estimate that cannot satisfy the
// invariant is now a bug in the clamp or in the proof, and belongs in one of
// those two places — not in a list here.
import (
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

const (
	cardinalityFieldID = iota
	cardinalityFieldK
	cardinalityFieldV
	cardinalityFieldN
	cardinalityFieldTags
	cardinalityFieldA
	cardinalityFieldB
)

type capturedBuild[T any] struct {
	value T
	err   error
}

// captureBuild lets a fallible constructor's two return values remain one
// expression at call sites; mustBuild delegates the assertion to the shared
// RFC-232 helper.
func captureBuild[T any](value T, err error) capturedBuild[T] {
	return capturedBuild[T]{value: value, err: err}
}

func mustBuild[T any](
	t testing.TB,
	construction capturedBuild[T],
) T {
	t.Helper()
	return mustConstruct(t, construction.value, construction.err)
}

// cardinalityRowType is the exact authority shared by these shape-only tests.
// The tests intentionally exercise cardinality rather than schema variation,
// so one complete row type keeps every scan and every field-bearing operator
// executable without smuggling UnknownType or an ownerless FieldValue through
// the plan constructors.
func cardinalityRowType() values.Type {
	return values.NewRecordType("CARDINALITY_ROW", false, []values.Field{
		{Name: "ID", FieldType: values.NullableLong},
		{Name: "K", FieldType: values.NullableLong},
		{Name: "V", FieldType: values.NullableDouble},
		{Name: "N", FieldType: values.NullableLong},
		{Name: "TAGS", FieldType: values.NewArrayType(false, values.NullableLong)},
		{Name: "A", FieldType: values.NullableLong},
		{Name: "B", FieldType: values.NullableLong},
	})
}

func cardinalityLong(value int64) values.Value {
	return &values.ConstantValue{Value: value, Typ: values.NotNullLong}
}

func cardinalityLiteral(t testing.TB, value any) values.Value {
	t.Helper()
	switch typed := value.(type) {
	case int64:
		return cardinalityLong(typed)
	case float64:
		return &values.ConstantValue{Value: typed, Typ: values.NotNullDouble}
	default:
		t.Fatalf("cardinality test has no exact literal type for %T", value)
		return nil
	}
}

func cardinalityFieldOrdinal(name string) (int, bool) {
	switch name {
	case "ID":
		return cardinalityFieldID, true
	case "K":
		return cardinalityFieldK, true
	case "V":
		return cardinalityFieldV, true
	case "N":
		return cardinalityFieldN, true
	case "TAGS":
		return cardinalityFieldTags, true
	case "A":
		return cardinalityFieldA, true
	case "B":
		return cardinalityFieldB, true
	default:
		return 0, false
	}
}

func mustCardinalityScan(t testing.TB, name string) *plans.RecordQueryScanPlan {
	t.Helper()
	return mustBuild(t, captureBuild(plans.NewRecordQueryScanPlan([]string{name}, cardinalityRowType(), false)))
}

func mustCardinalityIndex(
	t testing.TB,
	name string,
	comparisons []*predicates.ComparisonRange,
) *plans.RecordQueryIndexPlan {
	t.Helper()
	return mustBuild(t, captureBuild(plans.NewRecordQueryIndexPlan(
		name, comparisons, []string{"T"}, cardinalityRowType(), false)))
}

func mustCardinalityField(t testing.TB, owner plans.RecordQueryPlan, name string) values.Value {
	t.Helper()
	return mustCardinalityFieldFromRoot(t, owner.GetResultValue(), name)
}

func mustCardinalityFieldFromRoot(t testing.TB, root values.Value, name string) values.Value {
	t.Helper()
	ordinal, ok := cardinalityFieldOrdinal(name)
	if !ok {
		t.Fatalf("cardinality test requested undeclared field %q", name)
	}
	field, err := values.ResolveFieldOrdinals(root, []int{ordinal})
	return mustConstruct(t, field, err)
}

func mustCardinalityOrdinalField(t testing.TB, owner plans.RecordQueryPlan, ordinal int) values.Value {
	t.Helper()
	field, err := values.ResolveFieldOrdinals(owner.GetResultValue(), []int{ordinal})
	return mustConstruct(t, field, err)
}

func mustCardinalityScalarField(t testing.TB, owner plans.RecordQueryPlan) values.Value {
	t.Helper()
	// A physical consumer evaluates against the exact row carrier the owner
	// publishes. ResultValue can be a retained producer program whose root row
	// type differs from that carrier (notably a nested Projection), so using it
	// as the field root creates a cross-domain current QOV that checked
	// admission rightly rejects. This shape generator needs only one scalar
	// slot; derive it from the admitted output layout instead.
	layout, err := owner.ProvidedOutputLayout()
	if err != nil {
		t.Fatalf("cardinality fixture output layout: %v", err)
	}
	root := values.Value(layout.Carrier())
	typ := root.Type()
	var ordinals []int
	for {
		record, ok := typ.(*values.RecordType)
		if !ok {
			break
		}
		if len(record.Fields) == 0 {
			t.Fatal("cardinality fixture has no scalar field")
		}
		ordinals = append(ordinals, 0)
		typ = record.Fields[0].FieldType
	}
	if len(ordinals) == 0 {
		t.Fatalf("cardinality fixture result is scalar %v; expected a row", typ)
	}
	field, err := values.ResolveFieldOrdinals(root, ordinals)
	return mustConstruct(t, field, err)
}

func cardinalityScalarPredicate(
	t testing.TB,
	owner plans.RecordQueryPlan,
) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		mustCardinalityScalarField(t, owner),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: cardinalityLong(1)},
	)
}

func cardinalityPredicate(
	t testing.TB,
	owner plans.RecordQueryPlan,
	name string,
) predicates.QueryPredicate {
	t.Helper()
	return predicates.NewComparisonPredicate(
		mustCardinalityField(t, owner, name),
		predicates.Comparison{Type: predicates.ComparisonEquals, Operand: cardinalityLong(1)},
	)
}

func cardinalityNull(owner plans.RecordQueryPlan) values.Value {
	return values.NewNullValue(owner.GetResultType())
}

func cardinalityJoinResult(outer, inner plans.RecordQueryPlan) values.Value {
	return values.NewRecordConstructorValue(
		values.RecordConstructorField{Name: "outer", Value: outer.GetResultValue()},
		values.RecordConstructorField{Name: "inner", Value: inner.GetResultValue()},
	)
}

func cardinalityEqualityRange(t testing.TB, literal any) *predicates.ComparisonRange {
	t.Helper()
	comparison := predicates.Comparison{
		Type:    predicates.ComparisonEquals,
		Operand: cardinalityLiteral(t, literal),
	}
	merged := predicates.EmptyComparisonRange().Merge(&comparison)
	if !merged.Ok {
		t.Fatal("failed to build cardinality equality range")
	}
	return merged.Range
}

// ============================================================================
// Harness: prime the property bottom-up, then read both sides.
// ============================================================================

// primeCardinalitiesProperty computes and stores the Cardinalities property
// (via computeRefPlanProperties — the exact call the PLANNING phase makes
// after ImplementationRules fire on a group) for every Reference in plan's
// subtree, bottom-up. Each plan constructor mints a FRESH, finals-only
// Reference per child edge (plans.QuantifierOverPlan / QuantifiersOverPlans),
// so every edge in a freshly-built tree starts unprimed; recursing into
// children before priming the parent's edge is what lets
// cardinalitiesFromChildRefOrInner read a populated PlanPropertiesMap instead
// of an empty one. A no-op on an already-primed Reference (shared sub-trees are
// primed once).
func primeCardinalitiesProperty(plan plans.RecordQueryPlan) {
	if plan == nil {
		return
	}
	for _, q := range plan.GetQuantifiers() {
		ref := q.GetRangesOver()
		if ref == nil || GetRefPlanPropertiesMap(ref) != nil {
			continue
		}
		members := ref.FinalMembers()
		if len(members) == 0 {
			members = ref.AllMembers()
		}
		for _, m := range members {
			if childPlan, ok := m.(plans.RecordQueryPlan); ok {
				primeCardinalitiesProperty(childPlan)
			}
		}
		computeRefPlanProperties(ref)
	}
}

// provenCardinalities returns the STRUCTURAL Cardinalities bound
// computeCardinalities proves for plan, after priming every child
// Reference's PlanPropertiesMap bottom-up (see primeCardinalitiesProperty).
// This is the oracle side of the check: a bound derived from plan SHAPE
// alone, never statistics.
func provenCardinalities(t *testing.T, plan plans.RecordQueryPlan) properties.Cardinalities {
	t.Helper()
	primeCardinalitiesProperty(plan)
	w, ok := plan.(physicalPlanExpression)
	if !ok {
		t.Fatalf("%T does not implement physicalPlanExpression -- every RecordQueryPlan should since RFC-184 W2", plan)
	}
	return computeCardinalities(w, plan)
}

// costCardinality returns the cost model's point-estimate Cardinality for
// plan, via properties.EstimateCost — the SAME recursion the memo's winner
// selection performs. No priming needed on this side: EstimateCost walks
// GetQuantifiers()/ref.Get() directly, and Reference.Get()'s no-exploratory-
// members fallback resolves a finals-only Reference (what QuantifierOverPlan
// mints) straight to its pinned final member.
func costCardinality(plan plans.RecordQueryPlan) float64 {
	return properties.EstimateCost(plan).Cardinality
}

// ============================================================================
// Shape table.
// ============================================================================

type cardinalityCostShape struct {
	name  string
	build func(t *testing.T) plans.RecordQueryPlan
}

func cardinalityCostShapes() []cardinalityCostShape {
	scan := func(t testing.TB, name string) *plans.RecordQueryScanPlan {
		return mustCardinalityScan(t, name)
	}
	// pointLookupScan is a full-PK-equality primary scan: computeCardinalities
	// proves AtMostOne (0,1), and HintCost's isProvablePointProbe branch
	// independently agrees (cardinality=1) -- the two derivations happen to
	// share this one recognizer, so this shape should never disagree.
	pointLookupScan := func(t *testing.T, table, field string) *plans.RecordQueryScanPlan {
		base := scan(t, table)
		return base.WithPrimaryKey([]values.Value{mustCardinalityField(t, base, field)}).WithScanComparisons(
			[]*predicates.ComparisonRange{cardinalityEqualityRange(t, int64(1))})
	}
	uniquePointLookupIndex := func(t *testing.T, name string) *plans.RecordQueryIndexPlan {
		return mustCardinalityIndex(t, name,
			[]*predicates.ComparisonRange{cardinalityEqualityRange(t, int64(1))}).
			WithIndexMetadata([]string{"K"}, []string{"ID"}, true)
	}

	var shapes []cardinalityCostShape
	add := func(name string, build func(t *testing.T) plans.RecordQueryPlan) {
		shapes = append(shapes, cardinalityCostShape{name: name, build: build})
	}

	// --- scans -----------------------------------------------------------
	add("scan/unbounded", func(t *testing.T) plans.RecordQueryPlan {
		return scan(t, "SCAN_UNB")
	})
	add("scan/pointLookup", func(t *testing.T) plans.RecordQueryPlan {
		return pointLookupScan(t, "SCAN_PL", "ID")
	})
	// A stamped-PK scan whose sole equality binds a ZERO FLOAT. The executor
	// widens a zero bound across -0.0 and +0.0 (IEEE-equal, distinct adjacent
	// keys), so this is NOT a point probe and nothing may prove at-most-one for
	// it.
	//
	// Carried in the shape table rather than only in the point-probe agreement
	// test because every table-driven check inherits the dimension:
	// TestRFC195_Criterion2AgreesWithTheProvenBound claims to hold criterion 2
	// against the proven bound for every data-access shape, and without a
	// widening shape it never exercised the axis on which those two derivations
	// actually diverged. A gate that cannot see the one historical divergence
	// is not a gate.
	add("scan/zeroFloatEqualityWidens", func(t *testing.T) plans.RecordQueryPlan {
		base := scan(t, "SCAN_ZF")
		return base.WithPrimaryKey([]values.Value{mustCardinalityField(t, base, "V")}).
			WithScanComparisons([]*predicates.ComparisonRange{cardinalityEqualityRange(t, float64(0))})
	})

	// --- index scans -------------------------------------------------------
	add("indexScan/nonUnique", func(t *testing.T) plans.RecordQueryPlan {
		return mustCardinalityIndex(t, "IDX_NU", nil)
	})
	add("indexScan/uniquePointLookup", func(t *testing.T) plans.RecordQueryPlan {
		return uniquePointLookupIndex(t, "IDX_U_PL")
	})

	// --- fetch (1:1, transparent) -------------------------------------------
	add("fetch/overNonCoveringUnboundedIndex", func(t *testing.T) plans.RecordQueryPlan {
		idx := mustCardinalityIndex(t, "IDX_FETCH_UNB", nil)
		return mustBuild(t, captureBuild(plans.NewRecordQueryFetchFromPartialRecordPlan(
			idx, nil, cardinalityRowType(), plans.FetchIndexRecordsPrimaryKey)))
	})
	add("fetch/overUniquePointLookupIndex", func(t *testing.T) plans.RecordQueryPlan {
		idx := uniquePointLookupIndex(t, "IDX_FETCH_PL")
		return mustBuild(t, captureBuild(plans.NewRecordQueryFetchFromPartialRecordPlan(
			idx, nil, cardinalityRowType(), plans.FetchIndexRecordsPrimaryKey)))
	})

	// --- filter / predicates filter (varying predicate counts) -------------
	add("filter/onePred/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "FILT1", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{cardinalityPredicate(t, child, "A")}, child)))
	})
	add("predicatesFilter/twoPreds/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "FILT2", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryPredicatesFilterPlan(
			child, []predicates.QueryPredicate{
				cardinalityPredicate(t, child, "A"), cardinalityPredicate(t, child, "B"),
			})))
	})

	// --- type filter (transparent) ------------------------------------------
	add("typeFilter/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryTypeFilterPlan(
			[]string{"T1"}, pointLookupScan(t, "TF", "ID"))))
	})

	// --- projection / map (cardinality-preserving) --------------------------
	add("projection/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "PROJ", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryProjectionPlan(
			[]values.Value{mustCardinalityField(t, child, "K")}, child)))
	})
	add("map/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "MAP", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryMapPlan(child, child.GetResultValue())))
	})

	// --- FlatMap (correlated join): Times(outer, inner) --------------------
	add("flatMap/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		outer := pointLookupScan(t, "FM_O", "ID")
		inner := pointLookupScan(t, "FM_I", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryFlatMapPlan(
			outer, inner,
			values.NamedCorrelationIdentifier("fm_o"), values.NamedCorrelationIdentifier("fm_i"),
			cardinalityJoinResult(outer, inner), false)))
	})
	add("flatMap/boundedOuterUnboundedInner_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		// The inner is unbounded, so the property's Times() propagates unknown
		// max -- nothing is asserted here, but the cost model still produces a
		// confident point estimate (outer*inner = 1e6). This is a documented
		// BLIND SPOT, not a violation: no proven bound exists to check against.
		outer := pointLookupScan(t, "FM_O2", "ID")
		inner := scan(t, "FM_I2")
		return mustBuild(t, captureBuild(plans.NewRecordQueryFlatMapPlan(
			outer, inner,
			values.NamedCorrelationIdentifier("fm_o2"), values.NamedCorrelationIdentifier("fm_i2"),
			cardinalityJoinResult(outer, inner), false)))
	})

	// --- NestedLoopJoin (materialized): Times(outer, inner) ----------------
	add("nestedLoopJoin/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		outer := pointLookupScan(t, "NLJ_O", "ID")
		inner := pointLookupScan(t, "NLJ_I", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryNestedLoopJoinPlan(
			outer, inner,
			[]predicates.QueryPredicate{cardinalityPredicate(t, outer, "A")},
			plans.JoinInner, values.NamedCorrelationIdentifier("O"),
			values.NamedCorrelationIdentifier("I"), cardinalityJoinResult(outer, inner))))
	})

	// --- InJoin: child cardinalities scaled by in-list size ----------------
	add("inJoin/pointProbeInner/threeValues", func(t *testing.T) plans.RecordQueryPlan {
		p := mustBuild(t, captureBuild(plans.NewRecordQueryInJoinPlan(
			pointLookupScan(t, "INJ_PP", "ID"), "inj_binding", false, false)))

		p = p.WithInValues([]any{int64(1), int64(2), int64(3)})
		return p
	})
	add("inJoin/unboundedInList_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		// No WithInValues call -> the in-list size is unknown at plan time, so
		// the property abstains (UnknownMaxCardinality) -- documented BLIND
		// SPOT: the cost model still charges a conservative default (10 rows).
		return mustBuild(t, captureBuild(plans.NewRecordQueryInJoinPlan(
			pointLookupScan(t, "INJ_UNK", "ID"), "inj_binding2", false, false)))
	})

	// --- InUnion: child cardinalities scaled by literal-source fanout -------
	add("inUnion/knownFanoutThree", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "INU_PP", "ID")
		p := mustBuild(t, captureBuild(plans.NewRecordQueryInUnionPlan(
			child, []string{"inu_b"},
			[]values.Value{mustCardinalityField(t, child, "K")}, false)))

		p = p.WithInSources([][]any{{int64(1), int64(2), int64(3)}})
		return p
	})
	add("inUnion/emptySource", func(t *testing.T) plans.RecordQueryPlan {
		child := pointLookupScan(t, "INU_EMPTY", "ID")
		p := mustBuild(t, captureBuild(plans.NewRecordQueryInUnionPlan(
			child, []string{"inu_b2"},
			[]values.Value{mustCardinalityField(t, child, "K")}, false)))

		p = p.WithInSources([][]any{{}})
		return p
	})

	// --- union variants: sum of children ------------------------------------
	add("union/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "UN_A", "ID"), pointLookupScan(t, "UN_B", "ID"),
		})))
	})
	add("mergeSortUnion/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryMergeSortUnionPlan(
			[]plans.RecordQueryPlan{pointLookupScan(t, "MSU_A", "ID"), pointLookupScan(t, "MSU_B", "ID")},
			nil, false, true)))
	})
	add("unorderedUnion/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "UU_A", "ID"), pointLookupScan(t, "UU_B", "ID"),
		})))
	})

	// --- intersection: min-of-mins/maxes -------------------------------------
	add("intersection/bothBounded", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "INT_A", "ID"), pointLookupScan(t, "INT_B", "ID"),
		}, nil)))
	})
	add("intersection/oneUnbounded_blindSpotOnMax", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryIntersectionPlan([]plans.RecordQueryPlan{
			pointLookupScan(t, "INT_C", "ID"), scan(t, "INT_D"),
		}, nil)))
	})

	// --- distinct (both variants) -------------------------------------------
	// Both use the SAME child: FirstOrDefault produces EXACTLY one row
	// (property ExactlyOne(), cost also exactly 1 -- FirstOrDefaultCost floors
	// unconditionally), so both distinct operators' PROPERTY says min=1 (an
	// exactly-one-row input can't become fewer rows). DistinctCost's flat 0.7
	// selectivity multiplier still computes 0.7; RFC-195's clamp raises it to
	// the proven floor at the combine step.
	distinctChild := func(t *testing.T) *plans.RecordQueryFirstOrDefaultPlan {
		child := scan(t, "DISTINCT_SRC")
		return mustBuild(t, captureBuild(plans.NewRecordQueryFirstOrDefaultPlan(child, cardinalityNull(child))))
	}
	add("distinct/overExactlyOneChild", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryDistinctPlan(distinctChild(t))))
	})
	add("unorderedPrimaryKeyDistinct/overExactlyOneChild", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(distinctChild(t))))
	})
	add("distinct/overUnboundedChild_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryDistinctPlan(scan(t, "DISTINCT_UNB"))))
	})

	// --- sort (transparent) --------------------------------------------------
	add("inMemorySort/overBoundedChild", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryInMemorySortPlan(
			pointLookupScan(t, "SORT", "ID"), nil)))
	})

	// --- limit, including LIMIT 0 --------------------------------------------
	add("limit/cappedBelowChild", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "LIMIT5"), 5, 0)))
	})
	add("limit/zero", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "LIMIT0"), 0, 0)))
	})
	add("limit/runtimeValue_transparentBothSides", func(t *testing.T) plans.RecordQueryPlan {
		child := scan(t, "LIMIT_RT")
		return mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlanWithValue(
			child, mustCardinalityField(t, child, "N"), 0)))
	})

	// --- aggregate index (leaf, always unknown-max) --------------------------
	add("aggregateIndex/leaf_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		idx := mustCardinalityIndex(t, "AGG_IDX", nil)
		resultType := values.NewRecordType("AGG_RESULT", false, []values.Field{
			{Name: "COUNT", FieldType: values.NullableLong},
		})
		return mustBuild(t, captureBuild(plans.NewRecordQueryAggregateIndexPlan(idx, "T", resultType, "COUNT")))
	})

	// --- streaming aggregation: grouped (unknown) vs ungrouped (KNOWN VIOLATION) ---
	add("streamingAggregation/grouped", func(t *testing.T) plans.RecordQueryPlan {
		child := scan(t, "SAGG_GROUPED")
		return mustBuild(t, captureBuild(plans.NewRecordQueryStreamingAggregationPlan(
			child, []values.Value{mustCardinalityField(t, child, "K")}, nil)))
	})
	// An ungrouped RecordQueryStreamingAggregationPlan structurally emits at
	// most ONE output row. Its HintCost formula charges in*DistinctSelectivity
	// with no cap of its own -- ~700,000 for a 1e6-row child -- and RFC-195's
	// clamp caps it at the proven max. This is the five-orders-of-magnitude
	// case: every join ordering above an ungrouped aggregate used to be
	// computed against a row count wrong by 700,000x.
	add("streamingAggregation/ungrouped", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryStreamingAggregationPlan(
			scan(t, "SAGG_UNGROUPED"), nil, nil)))
	})

	// --- recursive DFS join ---------------------------------------------------
	add("recursiveDfsJoin/unknownMaxOverUnboundedRoot", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryRecursiveDfsJoinPlan(
			scan(t, "DFS_ROOT"), scan(t, "DFS_CHILD"),
			values.NamedCorrelationIdentifier("dfs_prior"), plans.DfsPreorder)))
	})
	// The DFS join is the level union's twin: same logical recursion, same
	// proven bound (root.Min, unknown). It proved NOTHING until RFC-195's
	// review, which is why the identical zero-collapse survived here after
	// being fixed on the level union — and survived on the alternative the
	// cost model PREFERS, since the level union carries a strictly larger
	// buffer term by construction. Measured before the arm existed:
	// FlatMap(scan, dfsJoin) costed 0 rows while FlatMap(scan, levelUnion)
	// costed 1e6 over the SAME children.
	add("recursiveDfsJoin/recursiveLegCollapsesTowardZero", func(t *testing.T) plans.RecordQueryPlan {
		seedInput := scan(t, "DFS_SEED")
		seed := mustBuild(t, captureBuild(plans.NewRecordQueryFirstOrDefaultPlan(seedInput, cardinalityNull(seedInput))))
		rec := mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "DFS_REC_ZERO"), 0, 0)))
		return mustBuild(t, captureBuild(plans.NewRecordQueryRecursiveDfsJoinPlan(
			seed, rec, values.NamedCorrelationIdentifier("dfs_prior2"), plans.DfsPreorder)))
	})

	// --- recursive level union: KNOWN VIOLATION when the recursive leg's cost
	// estimate is below 1 ------------------------------------------------------
	add("recursiveLevelUnion/normalRecursion", func(t *testing.T) plans.RecordQueryPlan {
		// Seed and recursive legs both cost exactly 1 (Values-plan / point-
		// lookup style leaves) -- recursiveCost = 1*1 = 1, which meets the
		// property's proven min (seed.Min = 1). No violation: the defect below
		// needs the recursive leg's OWN cost estimate to drop under 1, which a
		// plain point-lookup leg does not do.
		seed := mustBuild(t, captureBuild(plans.NewRecordQueryValuesPlan(
			[]values.Value{cardinalityLong(1)})))

		recBase := mustBuild(t, captureBuild(plans.NewRecordQueryScanPlan(
			[]string{"LU_REC"}, seed.GetResultType(), false)))

		rec := recBase.WithPrimaryKey([]values.Value{mustCardinalityOrdinalField(t, recBase, 0)}).
			WithScanComparisons([]*predicates.ComparisonRange{cardinalityEqualityRange(t, int64(1))})
		return mustBuild(t, captureBuild(plans.NewRecordQueryRecursiveLevelUnionPlan(
			seed, rec, values.NamedCorrelationIdentifier("lu_scan"), values.NamedCorrelationIdentifier("lu_insert"))))
	})
	// The property proves min=seed.Min (UNION ALL always emits at least the
	// seed: a RecordQueryFirstOrDefaultPlan seed structurally guarantees exactly
	// one row), but recursiveCost computes seedCard*recCard with NO additive
	// seed term -- when the recursive leg's own cost estimate is exactly zero (a
	// LIMIT 0 leg), the product collapses to zero even though the seed alone
	// guarantees a row, and that zero then propagates multiplicatively through
	// FlatMapCost/NestedLoopJoinCost, costing an entire join subtree at zero.
	// RFC-195's clamp floors it -- and, because this operator's buffer CPU
	// derives from its OWN OUTPUT cardinality, it clamps BEFORE charging that
	// term (HintCostWithin), so the emitted Cost stays internally consistent.
	add("recursiveLevelUnion/recursiveLegCollapsesTowardZero", func(t *testing.T) plans.RecordQueryPlan {
		seedInput := scan(t, "LU_SEED")
		seed := mustBuild(t, captureBuild(plans.NewRecordQueryFirstOrDefaultPlan(seedInput, cardinalityNull(seedInput))))
		rec := mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "LU_REC_ZERO"), 0, 0)))
		return mustBuild(t, captureBuild(plans.NewRecordQueryRecursiveLevelUnionPlan(
			seed, rec, values.NamedCorrelationIdentifier("lu_scan2"), values.NamedCorrelationIdentifier("lu_insert2"))))
	})

	// --- explode (leaf) --------------------------------------------------------
	add("explode/literalCollection", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryExplodePlan(&values.ConstantValue{
			Value: []any{int64(1), int64(2), int64(3), int64(4)},
			Typ:   values.NewArrayType(false, values.NotNullLong),
		})))
	})
	add("explode/nonLiteralCollection_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		root := mustBuild(t, captureBuild(values.NewQuantifiedObjectValue(
			values.NamedCorrelationIdentifier("explode_owner"), cardinalityRowType())))

		return mustBuild(t, captureBuild(plans.NewRecordQueryExplodePlan(
			mustCardinalityFieldFromRoot(t, root, "TAGS"))))
	})

	// --- values (leaf, exactly one row) -----------------------------------------
	add("values/leaf", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryValuesPlan(
			[]values.Value{cardinalityLong(1)})))
	})

	// --- temp-table scan (leaf, always unknown-max) -----------------------------
	add("tempTableScan/leaf_blindSpot", func(t *testing.T) plans.RecordQueryPlan {
		return mustBuild(t, captureBuild(plans.NewRecordQueryTempTableScanPlan(
			values.NamedCorrelationIdentifier("tt_alias"), cardinalityRowType())))
	})

	// --- DefaultOnEmpty: KNOWN VIOLATION when the child's cost is genuinely zero --
	add("defaultOnEmpty/overNonZeroChild", func(t *testing.T) plans.RecordQueryPlan {
		// Child already costs >=1 (a point lookup), so the un-floored HintCost
		// happens to already satisfy the property's Floor(1) -- NOT a
		// violation. Contrast with the excluded shape below, where the child's
		// cost estimate is a genuine zero.
		child := pointLookupScan(t, "DOE_OK", "ID")
		return mustBuild(t, captureBuild(plans.NewRecordQueryDefaultOnEmptyPlan(child, cardinalityNull(child))))
	})
	// The property applies child.Floor(1) (DefaultOnEmpty guarantees at least
	// one row, real-or-default), but HintCost returns the child's cost UNCHANGED
	// with no floor of its own -- when the child's own cost estimate is a
	// genuine zero (a LIMIT 0 child), the formula's DefaultOnEmpty cost is zero.
	// RFC-195's clamp supplies the floor generically, rather than by adding a
	// per-formula max(1, ...) that the next operator would get wrong again.
	add("defaultOnEmpty/overZeroCostChild", func(t *testing.T) plans.RecordQueryPlan {
		child := mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "DOE_ZERO"), 0, 0)))
		return mustBuild(t, captureBuild(plans.NewRecordQueryDefaultOnEmptyPlan(child, cardinalityNull(child))))
	})

	// --- typeFilter over an EXACTLY-ONE child --------------------------------
	// Found by the random-combo generator below (TypeFilter/Values(1) composes
	// two building blocks neither the fixed shapes above ever pairs directly):
	// RecordQueryValuesPlan proves ExactlyOne (min=1, max=1) -- it is a
	// literal row, deterministic, no "might not exist" the way a point-lookup
	// scan has -- while TypeFilterCost applies its flat TypeFilterSelectivity=0.5
	// with no floor of its own. The fixed typeFilter/overBoundedChild shape
	// above cannot see this: its child (pointLookupScan) proves AtMostOne
	// (min=0), and 0.5 >= 0 never violates. That dimensional gap is why this
	// shape is kept explicitly rather than left to the random generator.
	add("typeFilter/overExactlyOneChild", func(t *testing.T) plans.RecordQueryPlan {
		child := mustBuild(t, captureBuild(plans.NewRecordQueryValuesPlan(
			[]values.Value{cardinalityLong(1)})))

		return mustBuild(t, captureBuild(plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, child)))
	})

	// --- FirstOrDefault: always exactly one, and the cost model agrees --------
	add("firstOrDefault/overZeroCostChild_costFloorsCorrectly", func(t *testing.T) plans.RecordQueryPlan {
		// Direct contrast with defaultOnEmpty/overZeroCostChild above:
		// FirstOrDefaultCost floors unconditionally (Cardinality:1 regardless
		// of the child), so this NEVER violates even over a zero-cost child --
		// proof the floor is a simple, already-known-correct fix, not a hard
		// problem.
		child := mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(scan(t, "FOD_ZERO"), 0, 0)))
		return mustBuild(t, captureBuild(plans.NewRecordQueryFirstOrDefaultPlan(child, cardinalityNull(child))))
	})

	return shapes
}

// ============================================================================
// The check itself, and the standing test.
// ============================================================================

// cardinalityCostViolations returns a human-readable violation message per
// broken expectation for sh's (prov, cost) pair, or nil when everything
// holds. Pure (no *testing.T) so the mutation-guard test below can exercise
// it directly without ever causing an actually-failing subtest to live
// permanently in the suite.
func cardinalityCostViolations(sh cardinalityCostShape, prov properties.Cardinalities, cost float64) []string {
	var out []string
	if !prov.Min.IsUnknown() && cost < float64(prov.Min.Value()) {
		out = append(out, fmt.Sprintf(
			"%s: cost estimate %.4f is BELOW the proven min %d -- impossible by construction",
			sh.name, cost, prov.Min.Value()))
	}
	if !prov.Max.IsUnknown() && cost > float64(prov.Max.Value()) {
		out = append(out, fmt.Sprintf(
			"%s: cost estimate %.4f EXCEEDS the proven max %d -- impossible by construction",
			sh.name, cost, prov.Max.Value()))
	}
	return out
}

// assertCardinalityCostBound is cardinalityCostViolations wired to *testing.T.
func assertCardinalityCostBound(t *testing.T, sh cardinalityCostShape, prov properties.Cardinalities, cost float64) {
	t.Helper()
	for _, v := range cardinalityCostViolations(sh, prov, cost) {
		t.Error(v)
	}
	if prov.Min.IsUnknown() && prov.Max.IsUnknown() {
		t.Logf("%s: property proves NO bound at all (blind spot) -- cost estimate %.4f is unchecked", sh.name, cost)
	} else if prov.Max.IsUnknown() {
		t.Logf("%s: property proves min=%d but max is UNKNOWN (blind spot on the upper bound) -- "+
			"cost estimate %.4f is unchecked above", sh.name, prov.Min.Value(), cost)
	} else if prov.Min.IsUnknown() {
		t.Logf("%s: property proves max=%d but min is UNKNOWN (blind spot on the lower bound) -- "+
			"cost estimate %.4f is unchecked below", sh.name, prov.Max.Value(), cost)
	}
}

// TestCardinalityPropertyBoundsCostEstimate is the standing check: for every
// physical plan shape below, the cost model's Cardinality point estimate must
// fall inside the bound CardinalitiesProperty structurally proves, wherever a
// bound is proven at all. See the file doc comment for the full rationale and
// the exclusion-list self-cleaning mechanism.
func TestCardinalityPropertyBoundsCostEstimate(t *testing.T) {
	t.Parallel()
	for _, sh := range cardinalityCostShapes() {
		sh := sh
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			plan := sh.build(t)
			prov := provenCardinalities(t, plan)
			cost := costCardinality(plan)
			assertCardinalityCostBound(t, sh, prov, cost)
		})
	}
}

// TestCardinalityPropertyBoundsCostEstimate_MutationGuard proves the check
// above can actually FAIL, not just always report success. It takes a shape
// that legitimately passes (a plan-time LIMIT, whose proven max the cost
// model matches exactly), perturbs the proven max down by one in a LOCAL
// copy, and confirms cardinalityCostViolations flags it. A bound check that
// can never fail is worthless; this is the permanent, always-green
// demonstration that this one can -- without ever landing an intentionally
// failing subtest in the suite (see cardinalityCostViolations' doc comment).
func TestCardinalityPropertyBoundsCostEstimate_MutationGuard(t *testing.T) {
	t.Parallel()
	plan := mustBuild(t, captureBuild(plans.NewRecordQueryLimitPlan(
		mustCardinalityScan(t, "MUTGUARD"), 5, 0)))

	sh := cardinalityCostShape{name: "mutation-guard/limit5"}

	prov := provenCardinalities(t, plan)
	cost := costCardinality(plan)
	if v := cardinalityCostViolations(sh, prov, cost); len(v) != 0 {
		t.Fatalf("mutation guard shape must pass the REAL check first, got violations: %v", v)
	}
	if prov.Max.IsUnknown() {
		t.Fatal("mutation guard shape must have a known proven max to perturb")
	}

	perturbed := prov
	perturbed.Max = properties.OfCardinality(prov.Max.Value() - 1)
	v := cardinalityCostViolations(sh, perturbed, cost)
	if len(v) == 0 {
		t.Fatalf("MUTATION CHECK FAILED: perturbing provenMax down by 1 (from %d to %d) against cost=%v "+
			"produced NO violation -- the check does not actually fire",
			prov.Max.Value(), prov.Max.Value()-1, cost)
	}
	t.Logf("mutation check OK: cost=%v provenMax=%v (real, passes); perturbed provenMax=%v correctly flagged: %v",
		cost, prov.Max.Value(), prov.Max.Value()-1, v)
}

// ============================================================================
// Random combos: the fixed shape table above scaled arbitrarily wide.
// ============================================================================
//
// The standing invariant (cost estimate inside [provenMin, provenMax]
// whenever a bound is proven) needs NO reference oracle to check — both
// sides come from the plan itself, so a random COMPOSITION of the same
// well-behaved building blocks costs nothing extra to verify and can run at
// however many combos time allows. This is the self-checking-oracle
// equivalent of rowdiff/sargability's random generators, sized for a
// PR-gate-safe default and a nightly deep sweep via env, exactly like
// SARG_ORACLE_COMBOS/SARG_ORACLE_SEED:
//
//	CARDINALITY_COMBOS=200000 CARDINALITY_SEED=99 bazelisk test \
//	  //pkg/recordlayer/query/plan/cascades:cascades_test \
//	  --test_arg="--test.run=TestCardinalityPropertyBoundsCostEstimate_RandomCombos" \
//	  --test_arg="-test.v"
//
// The generator draws from the transparent and set-shaped operators
// (scan/indexScan/values/filter/typeFilter/projection/map/flatMap/
// nestedLoopJoin/union variants/intersection/sort). The invariant holds for ANY
// composition of them, so no per-shape precondition has to be re-derived per
// random tree — which is what keeps a scale exercise from becoming a source of
// false, un-investigatable failures.

func cardinalityComboCount(defaultN int) int {
	if v := os.Getenv("CARDINALITY_COMBOS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultN
}

func cardinalityComboSeed(defaultSeed uint64) uint64 {
	if v := os.Getenv("CARDINALITY_SEED"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultSeed
}

// randCardinalityPlan builds a random tree of noKnownViolation building
// blocks, up to depth deep. nextCorr mints a fresh, never-reused correlation
// identifier per FlatMap edge (kept out of the recursion's own state so the
// caller controls it, and so this function stays free of any shared
// mutable package state a concurrent caller would race on).
func randCardinalityPlan(t *testing.T, rng *rand.Rand, depth int, nextCorr func(prefix string) values.CorrelationIdentifier) plans.RecordQueryPlan {
	scan := func(name string) *plans.RecordQueryScanPlan {
		return mustCardinalityScan(t, name)
	}
	// RecordQueryValuesPlan (ExactlyOne, min=1) is IN this leaf pool, and that
	// is load-bearing. It used to be excluded: composed under a TypeFilter
	// anywhere in the random tree it reproduced the then-unfixed
	// "typeFilter/overExactlyOneChild" violation on every run, so the generator
	// was restricted to leaves proving either UNKNOWN bounds or AtMostOne
	// (min=0), over which no composition of transparent operators can push an
	// estimate below a proven floor.
	//
	// That restriction also made the generator blind to the entire class of
	// below-a-proven-FLOOR defects — it could only ever find above-a-proven-CAP
	// ones. RFC-195 removed the violation it was avoiding, so the exactly-one
	// leaf comes back and the generator regains the dimension that found the
	// bug in the first place.
	leaf := func() plans.RecordQueryPlan {
		switch rng.IntN(4) {
		case 0:
			return scan("RC_SCAN")
		case 1:
			base := scan("RC_PL")
			return base.WithPrimaryKey([]values.Value{mustCardinalityField(t, base, "ID")}).
				WithScanComparisons([]*predicates.ComparisonRange{cardinalityEqualityRange(t, int64(1))})
		case 2:
			return mustBuild(t, captureBuild(plans.NewRecordQueryValuesPlan(
				[]values.Value{cardinalityLong(1)})))

		default:
			return mustBuild(t, captureBuild(plans.NewRecordQueryIndexPlan(
				"RC_IDX", nil, []string{"RC_IDX_T"}, cardinalityRowType(), false)))

		}
	}
	if depth <= 0 || rng.IntN(3) == 0 {
		return leaf()
	}
	child := func() plans.RecordQueryPlan { return randCardinalityPlan(t, rng, depth-1, nextCorr) }
	setLeg := func() plans.RecordQueryPlan {
		// Set operators still draw independent subtrees, but RFC-232 admission
		// correctly requires their output row programs to agree. A constant
		// one-column projection is cardinality-preserving and gives both random
		// legs the same exact type without narrowing the generated tree corpus.
		return mustBuild(t, captureBuild(plans.NewRecordQueryProjectionPlan(
			[]values.Value{cardinalityLong(1)}, child())))
	}
	switch rng.IntN(11) {
	case 0:
		inner := child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryFilterPlan(
			[]predicates.QueryPredicate{cardinalityScalarPredicate(t, inner)}, inner)))

	case 1:
		inner := child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryPredicatesFilterPlan(inner,
			[]predicates.QueryPredicate{
				cardinalityScalarPredicate(t, inner), cardinalityScalarPredicate(t, inner),
			})))

	case 2:
		return mustBuild(t, captureBuild(plans.NewRecordQueryTypeFilterPlan([]string{"T1"}, child())))
	case 3:
		inner := child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryProjectionPlan(
			[]values.Value{mustCardinalityScalarField(t, inner)}, inner)))

	case 4:
		inner := child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryMapPlan(inner, inner.GetResultValue())))
	case 5:
		outer, inner := child(), child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryFlatMapPlan(outer, inner,
			nextCorr("rc_o"), nextCorr("rc_i"), cardinalityJoinResult(outer, inner), false)))

	case 6:
		outer, inner := child(), child()
		return mustBuild(t, captureBuild(plans.NewRecordQueryNestedLoopJoinPlan(outer, inner,
			[]predicates.QueryPredicate{cardinalityScalarPredicate(t, outer)},
			plans.JoinInner, values.NamedCorrelationIdentifier("O"),
			values.NamedCorrelationIdentifier("I"), cardinalityJoinResult(outer, inner))))

	case 7:
		return mustBuild(t, captureBuild(plans.NewRecordQueryUnionPlan(
			[]plans.RecordQueryPlan{setLeg(), setLeg()})))

	case 8:
		return mustBuild(t, captureBuild(plans.NewRecordQueryUnorderedUnionPlan(
			[]plans.RecordQueryPlan{setLeg(), setLeg()})))

	case 9:
		return mustBuild(t, captureBuild(plans.NewRecordQueryIntersectionPlan(
			[]plans.RecordQueryPlan{setLeg(), setLeg()}, nil)))

	default:
		return mustBuild(t, captureBuild(plans.NewRecordQueryInMemorySortPlan(child(), nil)))
	}
}

// TestCardinalityPropertyBoundsCostEstimate_RandomCombos scales the standing
// check (TestCardinalityPropertyBoundsCostEstimate) to however many random
// compositions the caller budgets, at a tiny PR-gate default and orders of
// magnitude more in the nightly deep sweep — see the file-section doc
// comment above for why this needs no reference oracle and stays free of
// false failures. t.Parallel() covers this test's scheduling relative to its
// SIBLINGS; the combo loop itself runs single-threaded in one goroutine (the
// correlation-identifier counter is not synchronized), which is what keeps
// it race-free without needing one.
func TestCardinalityPropertyBoundsCostEstimate_RandomCombos(t *testing.T) {
	t.Parallel()
	n := cardinalityComboCount(2000)
	seed := cardinalityComboSeed(20260726)
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))

	var corrCounter int
	nextCorr := func(prefix string) values.CorrelationIdentifier {
		corrCounter++
		return values.NamedCorrelationIdentifier(fmt.Sprintf("%s%d", prefix, corrCounter))
	}

	for i := 0; i < n; i++ {
		plan := randCardinalityPlan(t, rng, 4, nextCorr)
		prov := provenCardinalities(t, plan)
		cost := costCardinality(plan)
		sh := cardinalityCostShape{name: fmt.Sprintf("random/combo_%d", i)}
		for _, v := range cardinalityCostViolations(sh, prov, cost) {
			t.Errorf("combo %d (seed %d): %s\n  plan:\n%s", i, seed, v, plan.Explain())
		}
	}
	t.Logf("cardinality/cost random combos: %d checked (seed %d)", n, seed)
}
