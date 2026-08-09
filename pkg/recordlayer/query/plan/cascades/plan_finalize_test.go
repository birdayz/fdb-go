package cascades

import (
	"reflect"
	"sort"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/properties"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// This file guards the ONE invariant FinalizePlan rests on: every value tree
// hanging off a plan is reached by the walk. Two ways a tree escapes:
//
//	(A) a plan type grows a value-bearing FIELD with no arm in
//	    stampNodeLocalValues; or
//	(B) a plan's SUBTREE is unreachable because GetChildren() does not return
//	    it — the shape that let RecordQueryAggregateIndexPlan's embedded index
//	    plan go unstamped (GetChildren returns nil by design, the index plan is
//	    a structural field, and there was no arm).
//
// The guard is two halves that need each other:
//
//	CENSUS (reflection, type-level) — for every plan type, walk its struct
//	fields and flag any whose type transitively carries a values.Value, a
//	predicates.QueryPredicate, a *predicates.ComparisonRange, a
//	RecordQueryPlan, or an expressions.Quantifier (a child edge). This catches
//	(A): a new field is a field name the specimen table does not know.
//
//	BEHAVIOUR (constructive) — for every flagged field, build the plan with a
//	distinct sentinel RecordConstructorValue planted in that field, run
//	FinalizePlan, and assert the sentinel came back stamped. This catches (B),
//	which no structural check can see: whether GetChildren actually returns the
//	edge is a runtime fact, not a type fact.
//
// The census DRIVES the behaviour half — a flagged field must have either a
// planted sentinel or an allowlist entry with a written reason, so a new field
// cannot be silently ignored by forgetting to test it.
//
// The roster of plan TYPES the census runs over is DERIVED rather than
// hand-listed — see planTypes below for why a hand-written roster is not a
// guard at all.

// ---------------------------------------------------------------------------
// Census: which field types carry something FinalizePlan must reach
// ---------------------------------------------------------------------------

var (
	valueIface      = reflect.TypeOf((*values.Value)(nil)).Elem()
	predicateIface  = reflect.TypeOf((*predicates.QueryPredicate)(nil)).Elem()
	planIface       = reflect.TypeOf((*plans.RecordQueryPlan)(nil)).Elem()
	comparisonRange = reflect.TypeOf((*predicates.ComparisonRange)(nil))
	quantifier      = reflect.TypeOf(expressions.Quantifier{})
)

// carriesStampable reports whether t transitively reaches something the bake
// must stamp: a Value, a QueryPredicate, a ComparisonRange, a plan, or a
// quantifier (a plan edge).
//
// Two deliberate stopping points, both because a TYPE cannot tell you what an
// interface holds:
//
//   - a non-target interface (KeysSource, PlanSelector, values.Type) is a leaf.
//     A concrete implementation could in principle hold a Value; none does, and
//     the behavioural half would catch it if one started to.
//   - `any` (as in RecordQueryInJoinPlan.inValues) is such an interface. Those
//     slots hold Go literals bound at execution, never Values.
//
// A func type is a leaf too: TranslateValueFunction takes and returns Values but
// owns none, and nothing can walk a closure.
func carriesStampable(t reflect.Type, seen map[reflect.Type]bool) bool {
	if t == nil || seen[t] {
		return false
	}
	seen[t] = true

	if t == quantifier || t == comparisonRange {
		return true
	}
	if t.Implements(valueIface) || t.Implements(predicateIface) || t.Implements(planIface) {
		return true
	}
	if t.Kind() != reflect.Interface && reflect.PointerTo(t).Implements(planIface) {
		return true
	}

	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array:
		return carriesStampable(t.Elem(), seen)
	case reflect.Map:
		return carriesStampable(t.Key(), seen) || carriesStampable(t.Elem(), seen)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if carriesStampable(t.Field(i).Type, seen) {
				return true
			}
		}
	}
	return false
}

// stampableFields returns the names of planType's fields that carry something
// the bake must reach, in declaration order.
func stampableFields(planType reflect.Type) []string {
	var out []string
	for i := 0; i < planType.NumField(); i++ {
		f := planType.Field(i)
		if carriesStampable(f.Type, map[reflect.Type]bool{}) {
			out = append(out, f.Name)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Specimens: one buildable instance per plan type, sentinels planted per field
// ---------------------------------------------------------------------------

// specimen is one plan type's proof obligation.
type specimen struct {
	// build returns a plan wired with a distinct sentinel per field name.
	build func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue)
	// allow names fields that carry no plantable sentinel, each with the
	// reason the walk reaches them anyway.
	allow map[string]string
	// writeFed marks a DML root: feedsAWrite stops the walk there ON PURPOSE
	// (the target's declared descriptor governs a stored record, not the
	// constructor's inferred one), so its sentinels must come back UNSTAMPED.
	writeFed bool
}

// resultValueIsMinted is the allowlist reason shared by every plan whose
// resultValue field is minted inside its constructor (a QuantifiedObjectValue
// standing for the rows the leaf emits, RFC-184 W2) and is therefore not
// settable from a test. stampNodeLocalValues stamps plan.GetResultValue()
// unconditionally as its first act, and the four plans that DO accept a
// caller-supplied resultValue (Map, FlatMap, NestedLoopJoin,
// MultiIntersectionOnValues) plant a real sentinel there — so the mechanism is
// proven behaviourally where it can be, and asserted only where the field
// cannot be reached from outside the package.
const resultValueIsMinted = "constructor-minted QuantifiedObjectValue, returned by GetResultValue() which stampNodeLocalValues stamps first"

func sentinel() *values.RecordConstructorValue {
	return values.NewRecordConstructorValue(values.RecordConstructorField{
		Name:  "S",
		Value: &values.ConstantValue{Value: int64(1), Typ: values.NullableLong},
	})
}

// sentinelRange wraps s as the comparand of an equality range.
func sentinelRange(t *testing.T, s values.Value) *predicates.ComparisonRange {
	t.Helper()
	cmp := predicates.Comparison{Type: predicates.ComparisonEquals, Operand: s}
	res := predicates.EmptyComparisonRange().Merge(&cmp)
	if !res.Ok {
		t.Fatalf("could not build an equality ComparisonRange around the sentinel")
	}
	return res.Range
}

// sentinelChild returns a leaf plan carrying a sentinel in a field the walk
// stamps (RecordQueryValuesPlan.columns), so "was the sentinel stamped?"
// answers "did GetChildren() return this edge?".
func sentinelChild() (plans.RecordQueryPlan, *values.RecordConstructorValue) {
	s := sentinel()
	return plans.NewRecordQueryValuesPlan([]values.Value{s}), s
}

// uncostedPlanTypes names the plan types that answer NO cost/proof contract and
// are therefore absent from plans.CostedPlanPrototypes. They are listed here so
// the census still covers them, each with the reason it is not costed.
//
// The list is self-cleaning in both directions, enforced by
// TestFinalizePlanRosterIsDerived: an entry that starts satisfying
// plans.CostedPlan is stale (it belongs in the prototypes now and would be
// counted twice conceptually), and a prototype missing from the census fails the
// main test by name.
var uncostedPlanTypes = map[reflect.Type]string{
	reflect.TypeOf(plans.RecordQueryComparatorPlan{}):   "a plan-comparison harness, never costed or extracted",
	reflect.TypeOf(plans.RecordQueryLoadByKeysPlan{}):   "legacy record-layer plan, not constructed by Cascades",
	reflect.TypeOf(plans.RecordQueryScoreForRankPlan{}): "legacy record-layer plan, not constructed by Cascades",
	reflect.TypeOf(plans.RecordQuerySelectorPlan{}):     "a plan-selection harness, never costed or extracted",
	reflect.TypeOf(plans.RecordQueryTextIndexPlan{}):    "legacy record-layer plan, not constructed by Cascades",
}

// planTypes is the roster of concrete plan types the census runs over. It is
// DERIVED, not hand-listed, and that distinction is the whole guard.
//
// A hand-written roster is checked only against itself, so it stops guarding the
// moment a plan type is ADDED: the new type is simply absent, the loop never
// visits it, and the suite stays green while its value trees go unstamped. That
// is not hypothetical — RecordQueryCoveringIndexPlan was added, wraps an index
// scan in a structural field exactly as RecordQueryAggregateIndexPlan does,
// never appeared in the roster, and its inner scan's comparands were never
// baked. Nothing went red.
//
// So the roster is built from plans.CostedPlanPrototypes — the plans package's
// own enumeration of every operator the memo costs, which a new physical plan
// must join to be costed at all — plus uncostedPlanTypes for the handful that
// answer no cost contract. This is the same self-cleaning shape
// TestRFC195_LogicalPhysicalArmsArePaired uses, and for the same reason.
//
// What remains undetected is a plan type in NEITHER list. Closing that needs the
// plans package's SOURCE, which the AST route in plans' own
// TestEveryPlanTypeIsAMemoParticipant has via //go:embed *.go — a directive that
// cannot reach a sibling package, and staging the sources as a Bazel `data` dep
// of this target is a BUILD change this test cannot make for itself.
var planTypes = derivePlanTypes()

func derivePlanTypes() []reflect.Type {
	seen := map[reflect.Type]bool{}
	var out []reflect.Type
	add := func(t reflect.Type) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, proto := range plans.CostedPlanPrototypes {
		add(reflect.TypeOf(proto).Elem())
	}
	for t := range uncostedPlanTypes {
		add(t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// specimens maps plan type NAME to its proof obligation. A plan type whose
// census is empty (RecordQueryLoadByKeysPlan, RecordQueryTextIndexPlan) needs
// no entry.
var specimens = map[string]specimen{
	"RecordQueryAggregateIndexPlan": {
		build: func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			// The embedded index plan is NOT a child (GetChildren returns nil
			// by design, mirroring Java's RecordQueryPlanWithNoChildren), so
			// only a dedicated arm reaches its comparands.
			s := sentinel()
			idx := plans.NewRecordQueryIndexPlan(
				"IDX", []*predicates.ComparisonRange{sentinelRange(t, s)},
				[]string{"T"}, values.UnknownType, false,
			)
			p := plans.NewRecordQueryAggregateIndexPlan(idx, "T", values.UnknownType, "COUNT")
			return p, map[string]*values.RecordConstructorValue{"indexPlan": s}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryCoveringIndexPlan": {
		build: func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			// Same shape as the aggregate-index specimen above and for the same
			// reason: the wrapped index plan is a FIELD, GetChildren returns nil,
			// so only a dedicated arm reaches the inner scan's comparands.
			s := sentinel()
			idx := plans.NewRecordQueryIndexPlan(
				"IDX", []*predicates.ComparisonRange{sentinelRange(t, s)},
				[]string{"T"}, values.UnknownType, false,
			)
			return plans.NewRecordQueryCoveringIndexPlan(idx),
				map[string]*values.RecordConstructorValue{"indexPlan": s}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryComparatorPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			key := sentinel()
			p := plans.NewRecordQueryComparatorPlan(
				[]plans.RecordQueryPlan{child}, []values.Value{key}, 0, false, false)
			return p, map[string]*values.RecordConstructorValue{
				"childQs":             cs,
				"comparisonKeyValues": key,
			}
		},
	},

	"RecordQueryDefaultOnEmptyPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			def := sentinel()
			return plans.NewRecordQueryDefaultOnEmptyPlan(child, def),
				map[string]*values.RecordConstructorValue{"innerQ": cs, "defaultValue": def}
		},
	},

	"RecordQueryDeletePlan": {
		writeFed: true,
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryDeletePlan(child, "T"),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryDistinctPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryDistinctPlan(child),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryExplodePlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			coll := sentinel()
			return plans.NewRecordQueryExplodePlan(coll),
				map[string]*values.RecordConstructorValue{"collectionValue": coll}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryFetchFromPartialRecordPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			p := plans.NewRecordQueryFetchFromPartialRecordPlan(
				child, plans.UnableToTranslate, values.UnknownType,
				plans.FetchIndexRecordsPrimaryKey)
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryFilterPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			pv := sentinel()
			p := plans.NewRecordQueryFilterPlan(
				[]predicates.QueryPredicate{predicates.NewValuePredicate(pv)}, child)
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs, "predicates": pv}
		},
	},

	"RecordQueryFirstOrDefaultPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			def := sentinel()
			return plans.NewRecordQueryFirstOrDefaultPlan(child, def),
				map[string]*values.RecordConstructorValue{"innerQ": cs, "defaultValue": def}
		},
	},

	"RecordQueryFlatMapPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			outer, os := sentinelChild()
			inner, is := sentinelChild()
			res := sentinel()
			p := plans.NewRecordQueryFlatMapPlan(outer, inner,
				values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier(),
				res, false)
			return p, map[string]*values.RecordConstructorValue{
				"outerQ": os, "innerQ": is, "resultValue": res,
			}
		},
	},

	"RecordQueryInJoinPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryInJoinPlan(child, "b", false, false),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryInMemorySortPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			key := sentinel()
			p := plans.NewRecordQueryInMemorySortPlan(child,
				[]plans.SortKey{{Field: "S", ValueExpr: key}})
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs, "sortKeys": key}
		},
	},

	"RecordQueryInUnionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			key := sentinel()
			p := plans.NewRecordQueryInUnionPlan(child, []string{"b"}, []values.Value{key}, false)
			return p, map[string]*values.RecordConstructorValue{
				"innerQ": cs, "comparisonKeys": key,
			}
		},
	},

	"RecordQueryIndexPlan": {
		build: func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			comp := sentinel()
			pk := sentinel()
			p := plans.NewRecordQueryIndexPlan(
				"IDX", []*predicates.ComparisonRange{sentinelRange(t, comp)},
				[]string{"T"}, values.UnknownType, false,
			).WithCommonPrimaryKey([]values.Value{pk})
			return p, map[string]*values.RecordConstructorValue{
				"scanComparisons": comp, "commonPrimaryKeyValues": pk,
			}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryInsertPlan": {
		writeFed: true,
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryInsertPlan(child, "T", values.UnknownType),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryIntersectionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			// Built through the ORDERING constructor so the ordering-part
			// Value and the executable comparison Value are the same object —
			// the aliasing that lets the comparisonKeyValues arm cover
			// comparisonKeyOrderingParts. If NaturalComparisonKeyValues ever
			// stops returning the parts' raw Values, this goes red.
			key := sentinel()
			p := plans.NewRecordQueryIntersectionPlanWithOrdering(
				[]plans.RecordQueryPlan{child},
				[]properties.ProvidedOrderingPart{{
					Value: key, SortOrder: properties.ProvidedSortOrderAscending,
				}},
				false,
			)
			if p == nil {
				panic("intersection-with-ordering constructor failed closed")
			}
			return p, map[string]*values.RecordConstructorValue{
				"childQs":                    cs,
				"comparisonKeyValues":        key,
				"comparisonKeyOrderingParts": key,
			}
		},
	},

	"RecordQueryLimitPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			lim := sentinel()
			return plans.NewRecordQueryLimitPlanWithValue(child, lim, 0),
				map[string]*values.RecordConstructorValue{"innerQ": cs, "limitValue": lim}
		},
	},

	"RecordQueryMapPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			res := sentinel()
			return plans.NewRecordQueryMapPlan(child, res),
				map[string]*values.RecordConstructorValue{"innerQ": cs, "resultValue": res}
		},
	},

	"RecordQueryMergeSortUnionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			key := sentinel()
			p := plans.NewRecordQueryMergeSortUnionPlan(
				[]plans.RecordQueryPlan{child}, []values.Value{key}, false, false)
			return p, map[string]*values.RecordConstructorValue{
				"childQs": cs, "comparisonKeys": key,
			}
		},
	},

	"RecordQueryMultiIntersectionOnValuesPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			key := sentinel()
			res := sentinel()
			p := plans.NewRecordQueryMultiIntersectionOnValuesPlan(
				[]plans.RecordQueryPlan{child}, []values.Value{key}, res)
			return p, map[string]*values.RecordConstructorValue{
				"childQs": cs, "comparisonKey": key, "resultValue": res,
			}
		},
	},

	"RecordQueryNestedLoopJoinPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			outer, os := sentinelChild()
			inner, is := sentinelChild()
			pv := sentinel()
			res := sentinel()
			p := plans.NewRecordQueryNestedLoopJoinPlan(outer, inner,
				[]predicates.QueryPredicate{predicates.NewValuePredicate(pv)},
				plans.JoinInner,
				values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier(),
				res)
			return p, map[string]*values.RecordConstructorValue{
				"outerQ": os, "innerQ": is, "predicates": pv, "resultValue": res,
			}
		},
	},

	"RecordQueryPredicatesFilterPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			pv := sentinel()
			p := plans.NewRecordQueryPredicatesFilterPlan(child,
				[]predicates.QueryPredicate{predicates.NewValuePredicate(pv)})
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs, "predicates": pv}
		},
	},

	"RecordQueryProjectionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			proj := sentinel()
			return plans.NewRecordQueryProjectionPlan([]values.Value{proj}, child),
				map[string]*values.RecordConstructorValue{"innerQ": cs, "projections": proj}
		},
	},

	"RecordQueryRecursiveDfsJoinPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			root, rs := sentinelChild()
			child, cs := sentinelChild()
			p := plans.NewRecordQueryRecursiveDfsJoinPlan(root, child,
				values.UniqueCorrelationIdentifier(), plans.DfsPreorder)
			return p, map[string]*values.RecordConstructorValue{"rootQ": rs, "childQ": cs}
		},
	},

	"RecordQueryRecursiveLevelUnionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			initial, is := sentinelChild()
			rec, rs := sentinelChild()
			p := plans.NewRecordQueryRecursiveLevelUnionPlan(initial, rec,
				values.UniqueCorrelationIdentifier(), values.UniqueCorrelationIdentifier())
			return p, map[string]*values.RecordConstructorValue{"initialQ": is, "recursiveQ": rs}
		},
	},

	"RecordQueryScanPlan": {
		build: func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			comp := sentinel()
			pk := sentinel()
			p := plans.NewRecordQueryScanPlan([]string{"T"}, values.UnknownType, false).
				WithScanComparisons([]*predicates.ComparisonRange{sentinelRange(t, comp)}).
				WithPrimaryKey([]values.Value{pk})
			return p, map[string]*values.RecordConstructorValue{
				"scanComparisons": comp, "primaryKeyVals": pk,
			}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryScoreForRankPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryScoreForRankPlan(child, nil),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQuerySelectorPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			p := plans.NewRecordQuerySelectorPlanWithProbabilities(
				[]plans.RecordQueryPlan{child}, []int{100}, false)
			return p, map[string]*values.RecordConstructorValue{"childQs": cs}
		},
	},

	"RecordQueryStreamingAggregationPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			grp := sentinel()
			agg := sentinel()
			p := plans.NewRecordQueryStreamingAggregationPlan(child,
				[]values.Value{grp},
				[]expressions.AggregateSpec{{Function: expressions.AggSum, Operand: agg}})
			return p, map[string]*values.RecordConstructorValue{
				"innerQ": cs, "groupingKeys": grp, "aggregates": agg,
			}
		},
	},

	"RecordQueryTableFunctionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			sv := sentinel()
			return plans.NewRecordQueryTableFunctionPlan(sv),
				map[string]*values.RecordConstructorValue{"streamValue": sv}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryTempTableInsertPlan": {
		writeFed: true,
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			p := plans.NewRecordQueryTempTableInsertPlan(child,
				values.UniqueCorrelationIdentifier(), true)
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryTempTableScanPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			return plans.NewRecordQueryTempTableScanPlan(values.UniqueCorrelationIdentifier()), nil
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryTypeFilterPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryTypeFilterPlan([]string{"T"}, child),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryUnionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryUnionPlan([]plans.RecordQueryPlan{child}),
				map[string]*values.RecordConstructorValue{"childQs": cs}
		},
	},

	"RecordQueryUnorderedPrimaryKeyDistinctPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryUnorderedPrimaryKeyDistinctPlan(child),
				map[string]*values.RecordConstructorValue{"innerQ": cs}
		},
	},

	"RecordQueryUnorderedUnionPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			return plans.NewRecordQueryUnorderedUnionPlan([]plans.RecordQueryPlan{child}),
				map[string]*values.RecordConstructorValue{"childQs": cs}
		},
	},

	"RecordQueryUpdatePlan": {
		writeFed: true,
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			child, cs := sentinelChild()
			nv := sentinel()
			p := plans.NewRecordQueryUpdatePlan(child, "T",
				[]expressions.UpdateTransform{{FieldPath: "A", NewValue: nv}})
			return p, map[string]*values.RecordConstructorValue{"innerQ": cs, "transforms": nv}
		},
	},

	"RecordQueryValuesPlan": {
		build: func(_ *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			col := sentinel()
			return plans.NewRecordQueryValuesPlan([]values.Value{col}),
				map[string]*values.RecordConstructorValue{"columns": col}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},

	"RecordQueryVectorIndexPlan": {
		build: func(t *testing.T) (plans.RecordQueryPlan, map[string]*values.RecordConstructorValue) {
			pre := sentinel()
			qv := sentinel()
			k := sentinel()
			p := plans.NewRecordQueryVectorIndexPlan(
				"VIDX", []*predicates.ComparisonRange{sentinelRange(t, pre)},
				qv, k, predicates.ComparisonDistanceRankLessThanOrEq,
				nil, nil, []string{"T"}, values.UnknownType)
			return p, map[string]*values.RecordConstructorValue{
				"prefixComparisons": pre, "queryVector": qv, "k": k,
			}
		},
		allow: map[string]string{"resultValue": resultValueIsMinted},
	},
}

// ---------------------------------------------------------------------------
// The test
// ---------------------------------------------------------------------------

// TestFinalizePlanCoversStructuralKey asserts that FinalizePlan reaches every
// value-bearing field of every plan type: the census flags the fields, the
// specimens prove reachability by planting a sentinel in each and checking it
// came back stamped.
//
// It went red on RecordQueryAggregateIndexPlan.indexPlan — a plan-typed field
// that GetChildren() deliberately does not return and that had no arm in
// stampNodeLocalValues, so the embedded index scan's comparands were never
// baked.
func TestFinalizePlanCoversStructuralKey(t *testing.T) {
	t.Parallel()

	for _, planType := range planTypes {
		name := planType.Name()
		fields := stampableFields(planType)
		spec, hasSpec := specimens[name]

		if len(fields) == 0 {
			if hasSpec {
				t.Errorf("%s: has a specimen but no value-bearing fields; drop the specimen", name)
			}
			continue
		}
		if !hasSpec {
			t.Errorf("%s: carries value-bearing fields %v but has no specimen. "+
				"FinalizePlan's walk is unproven for it: add a specimen that plants "+
				"a sentinel RecordConstructorValue in each field.", name, fields)
			continue
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()
			plan, planted := spec.build(t)

			// Census half: every flagged field must be either planted or
			// allowlisted with a reason.
			for _, f := range fields {
				_, isPlanted := planted[f]
				reason := spec.allow[f]
				if !isPlanted && reason == "" {
					t.Errorf("%s.%s carries values FinalizePlan must stamp, and nothing "+
						"proves the walk reaches it. Either add an arm to "+
						"stampNodeLocalValues (or make GetChildren return the edge) and "+
						"plant a sentinel for %[2]q in this specimen, or allowlist %[2]q "+
						"with the reason the walk already reaches it.", name, f)
				}
			}
			// And nothing may be planted/allowlisted for a field the census
			// does not know — that means the table has gone stale.
			known := map[string]bool{}
			for _, f := range fields {
				known[f] = true
			}
			for _, f := range sortedSpecimenFields(planted, spec.allow) {
				if !known[f] {
					t.Errorf("%s: specimen names field %q, which no longer carries values "+
						"(or no longer exists). Update the specimen.", name, f)
				}
			}

			// Behaviour half.
			FinalizePlan(plan)
			for field, s := range planted {
				stamped := s.MessageDescriptor() != nil
				switch {
				case spec.writeFed && stamped:
					t.Errorf("%s.%s: sentinel WAS stamped under a write-fed root. "+
						"feedsAWrite must stop the walk there — the target's declared "+
						"descriptor governs a stored record, not the constructor's "+
						"inferred one.", name, field)
				case !spec.writeFed && !stamped:
					t.Errorf("%s.%s: sentinel RecordConstructorValue was NOT stamped — "+
						"FinalizePlan never reached it. Add an arm for %[1]s in "+
						"stampNodeLocalValues that walks this field (or return the edge "+
						"from GetChildren).", name, field)
				}
			}
		})
	}
}

// sortedKeys returns the union of the maps' keys, sorted, so failures are
// deterministic.
func sortedSpecimenFields(planted map[string]*values.RecordConstructorValue, allow map[string]string) []string {
	set := map[string]struct{}{}
	for k := range planted {
		set[k] = struct{}{}
	}
	for k := range allow {
		set[k] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestFinalizePlanStampsCoveringIndexInnerScan is the named pin for the defect
// the derived roster exposed: a covering index scan's comparands going unbaked.
//
// It states the failure in the terms that make it a CORRECTNESS bug rather than
// a coverage gap. An unstamped RecordConstructorValue evaluates to its
// NAME-keyed map instead of the field-NUMBER-keyed message, so a record-valued
// scan bound compares against a differently shaped operand.
//
// The plan is placed under a fetch, which is where a covering scan actually
// sits, so the pin also proves the walk arrives at the covering plan through a
// real child edge rather than only as a root.
func TestFinalizePlanStampsCoveringIndexInnerScan(t *testing.T) {
	t.Parallel()

	comparand := sentinel()
	pk := sentinel()
	idx := plans.NewRecordQueryIndexPlan(
		"IDX", []*predicates.ComparisonRange{sentinelRange(t, comparand)},
		[]string{"T"}, values.UnknownType, false,
	).WithCommonPrimaryKey([]values.Value{pk})

	covering := plans.NewRecordQueryCoveringIndexPlan(idx)
	root := plans.NewRecordQueryFetchFromPartialRecordPlan(
		covering, plans.UnableToTranslate, values.UnknownType,
		plans.FetchIndexRecordsPrimaryKey)

	FinalizePlan(root)

	for label, s := range map[string]*values.RecordConstructorValue{
		"scan comparand":     comparand,
		"common primary key": pk,
	} {
		if s.MessageDescriptor() == nil {
			t.Errorf("covering index scan's %s RecordConstructorValue was NOT stamped: "+
				"FinalizePlan never reached the wrapped index plan. It is a structural "+
				"FIELD, not a child, so GetChildren does not return it and only a "+
				"dedicated arm in stampNodeLocalValues can reach it. Unstamped, the "+
				"constructor evaluates to its name-keyed map instead of the "+
				"field-number-keyed message.", label)
		}
	}
}

// TestFinalizePlanRosterIsDerived pins the derivation itself — the half that
// decides WHICH types get censused, and therefore the half whose silent
// collapse would make the guard above vacuous rather than red.
//
// Three things are asserted, each guarding a different way the derivation dies:
// the population is non-empty (a green over zero types is the dominant false
// positive); every cost prototype is in the roster (the direction that missed
// RecordQueryCoveringIndexPlan); and no uncostedPlanTypes entry has since gained
// a cost contract (the direction in which the hand-written remainder rots).
func TestFinalizePlanRosterIsDerived(t *testing.T) {
	t.Parallel()

	if len(plans.CostedPlanPrototypes) == 0 {
		t.Fatal("plans.CostedPlanPrototypes is empty — the roster the census is derived " +
			"from is gone, so TestFinalizePlanCoversStructuralKey now guards nothing")
	}
	if len(planTypes) <= len(uncostedPlanTypes) {
		t.Fatalf("derived roster holds %d types, no more than the %d hand-listed uncosted "+
			"ones — the derivation from CostedPlanPrototypes has stopped contributing",
			len(planTypes), len(uncostedPlanTypes))
	}

	inRoster := map[reflect.Type]bool{}
	for _, pt := range planTypes {
		inRoster[pt] = true
	}
	for _, proto := range plans.CostedPlanPrototypes {
		pt := reflect.TypeOf(proto).Elem()
		if !inRoster[pt] {
			t.Errorf("%s answers the cost contract but is not in the census roster; "+
				"FinalizePlan's walk over it is unproven", pt.Name())
		}
	}

	costedPlan := reflect.TypeOf((*plans.CostedPlan)(nil)).Elem()
	for pt, reason := range uncostedPlanTypes {
		if reason == "" {
			t.Errorf("%s is listed as uncosted with no reason", pt.Name())
		}
		if reflect.PointerTo(pt).Implements(costedPlan) {
			t.Errorf("%s now satisfies plans.CostedPlan, so listing it in "+
				"uncostedPlanTypes is stale: register it in plans.CostedPlanPrototypes "+
				"and drop the entry, or the roster stops being derived from the "+
				"enumeration that self-cleans", pt.Name())
		}
	}
}

// TestFinalizePlanCensusSeesTheCarriers pins the census classifier itself. It
// is the half that decides which fields need proving, so a silent regression in
// carriesStampable would quietly shrink the guard above to nothing.
func TestFinalizePlanCensusSeesTheCarriers(t *testing.T) {
	t.Parallel()

	mustCarry := map[string]reflect.Type{
		"values.Value (iface)":              valueIface,
		"[]values.Value":                    reflect.TypeOf([]values.Value(nil)),
		"predicates.QueryPredicate (iface)": predicateIface,
		"[]predicates.QueryPredicate":       reflect.TypeOf([]predicates.QueryPredicate(nil)),
		"*predicates.ComparisonRange":       comparisonRange,
		"[]*predicates.ComparisonRange":     reflect.TypeOf([]*predicates.ComparisonRange(nil)),
		"plans.RecordQueryPlan (iface)":     planIface,
		"*plans.RecordQueryIndexPlan":       reflect.TypeOf((*plans.RecordQueryIndexPlan)(nil)),
		"expressions.Quantifier":            quantifier,
		"[]expressions.Quantifier":          reflect.TypeOf([]expressions.Quantifier(nil)),
		"[]plans.SortKey":                   reflect.TypeOf([]plans.SortKey(nil)),
		"[]expressions.AggregateSpec":       reflect.TypeOf([]expressions.AggregateSpec(nil)),
		"[]expressions.UpdateTransform":     reflect.TypeOf([]expressions.UpdateTransform(nil)),
		"[]properties.ProvidedOrderingPart": reflect.TypeOf([]properties.ProvidedOrderingPart(nil)),
	}
	for label, typ := range mustCarry {
		if !carriesStampable(typ, map[reflect.Type]bool{}) {
			t.Errorf("census no longer classifies %s as value-bearing; "+
				"TestFinalizePlanCoversStructuralKey has stopped guarding every field "+
				"of that type", label)
		}
	}

	mustNotCarry := map[string]reflect.Type{
		"string":         reflect.TypeOf(""),
		"[]string":       reflect.TypeOf([]string(nil)),
		"bool":           reflect.TypeOf(true),
		"[]any":          reflect.TypeOf([]any(nil)),
		"values.Type":    reflect.TypeOf((*values.Type)(nil)).Elem(),
		"plans.TextScan": reflect.TypeOf(plans.TextScan{}),
	}
	for label, typ := range mustNotCarry {
		if carriesStampable(typ, map[reflect.Type]bool{}) {
			t.Errorf("census classifies %s as value-bearing; that over-broadens the "+
				"guard and will demand sentinels nothing can plant", label)
		}
	}
}
