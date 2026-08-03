package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// FinalizePlan performs the plan-time bake: it builds ONE type repository for
// the whole plan and stamps every reachable *values.RecordConstructorValue with
// the message descriptor for its result type.
//
// This is Java's QueryPlan.java:639-643, which builds one TypeRepository per
// plan from usedTypes().evaluate(relationalExpression) and hands it to the
// evaluation context for RecordConstructorValue.eval to read back
// (RecordConstructorValue.java:113-114). Go stamps the descriptor onto the
// value instead of carrying it on a context, because Go has no uniform
// evaluation context to carry it on — RFC-204 §4.5.1 records the decision and
// the reasoning, and TestFrontierContextIsNotAUniformCarrier pins the premise
// it rests on.
//
// One repository per plan is what makes the COMMON case free; it is not a
// correctness requirement. Descriptor identity is per-repository, so two
// constructors of the same record type stamped from different repositories
// produce messages that are wire-compatible but not identity-equal. Within one
// plan that never happens, which is why a nested constructor's message can be
// set straight into its parent's field — Java's deepCopyIfNeeded
// (RecordConstructorValue.java:165-216) has nothing to reconcile there.
//
// ACROSS plans it does happen, by design: a scalar subquery is planned and
// baked as its own plan (embedded/scalar_subquery_planning.go calls FinalizePlan
// on it separately, because it hangs off the cascadesPlan rather than off any
// child edge of the outer plan), so it carries its own repository. A message
// built from that repository and set into an outer constructor's field takes
// the by-number copy in values.rowMessageToProtoValue — the port of
// deepCopyIfNeeded — and lands correctly. So the guarantee this walk provides
// is "one repository per plan", and the copy is what covers the rest.
//
// Call it exactly once per plan, on the plan-cache MISS path before
// PlanCache.Put. The cache returns the SAME plan pointer to every subsequent
// execution and each page rebuilds its cursor hierarchy from it concurrently,
// so stamping at execution time would be a data race on values.
//
// A type with no message form (a bare scalar record field, an erased array)
// yields a *values.ProtoTypeError from MessageDescriptorFor. That is NOT a
// query failure: the constructor simply stays unstamped and evaluates to its
// name-keyed map, exactly as it did before the bake existed.
func FinalizePlan(plan plans.RecordQueryPlan) {
	if plan == nil {
		return
	}
	repo := values.NewTypeProtoRepository()
	seenPlans := map[plans.RecordQueryPlan]struct{}{}
	stampPlanNode(plan, repo, seenPlans)
}

// feedsAWrite reports whether this plan node and everything beneath it
// produces values destined for a STORED record rather than for the driver.
//
// Such values must NOT be stamped, and the reason is semantic rather than
// defensive. The baked descriptor is synthesised from the constructor's OWN
// inferred type. That is the right descriptor exactly where the constructor's
// type IS the final result type — the read path. On the write path the
// TARGET's declared descriptor governs, and the two differ: `(9, 8.5, 'z',
// false)` infers `_0 INT` where the target column is BIGINT, and it carries the
// anonymous ordinal names `_0…_3` where the target declares `A…D`.
//
// Java never faces this because it binds the target EARLIER: parseRecordFields
// applies the target type's field names and types to the constructor while
// visiting it, so by the time eval runs, the constructor's own type already IS
// the target's and the descriptor it bakes is the target's descriptor. Go
// builds the constructor in expression position with no target — a COALESCE
// operand has none until the assignment coerces it — so the binding happens at
// the coercion instead (values.BuildStructMessage, reached from the executor's
// goToProtoValue map arm).
//
// Stamping here would route the value past that coercion: the executor's
// MessageKind arm tries copyMessageIntoDescriptor BEFORE its map arm, and the
// by-number copy performs no width promotion, no anonymous-positional binding,
// no NOT NULL rejection and no arity check. The measured symptom is
// `proto: S4.A: assigning invalid type int32` — the INT literal copied
// unpromoted into the BIGINT field.
//
// This costs nothing today: a DML statement returns a row COUNT, not rows. The
// grammar carries a RETURNING token (it is generated from Java's) but the Go
// translator implements no RETURNING clause, so no computed record under a DML
// root can reach the driver. TestFDB_RecordConstructorInExpressionPosition's
// multi_row_insert_values_is_not_baked subtest pins that, and names what gets
// re-armed if RETURNING ever lands: the returned projection would need
// stamping while the write source still must not be.
func feedsAWrite(plan plans.RecordQueryPlan) bool {
	switch plan.(type) {
	case *plans.RecordQueryInsertPlan,
		*plans.RecordQueryUpdatePlan,
		*plans.RecordQueryDeletePlan,
		*plans.RecordQueryTempTableInsertPlan:
		return true
	}
	return false
}

// stampPlanNode walks the plan DAG. Plans are DAGs, not trees — a shared
// sub-plan is reachable by more than one edge — so the seen-set is required for
// termination, exactly as validatePlanNode needs it.
func stampPlanNode(plan plans.RecordQueryPlan, repo *values.TypeProtoRepository, seen map[plans.RecordQueryPlan]struct{}) {
	if plan == nil {
		return
	}
	if _, ok := seen[plan]; ok {
		return
	}
	seen[plan] = struct{}{}

	if feedsAWrite(plan) {
		return
	}

	stampNodeLocalValues(plan, repo)

	for _, c := range plan.GetChildren() {
		stampPlanNode(c, repo, seen)
	}
}

// stampNodeLocalValues reaches every value tree hanging off THIS plan node.
//
// GetResultValue() alone is not enough. Many plans flow their inner's value
// through GetResultValue and keep the computed tree in a node-local field
// instead (a projection's projections, a filter's predicates, an aggregation's
// grouping keys), so a result-value-only walk would miss precisely the
// constructors that produce computed records.
//
// GetChildren() alone is not enough either, and that gap is the sharper one: a
// plan may hold a whole sub-PLAN in a structural field that GetChildren
// deliberately does not return. RecordQueryAggregateIndexPlan is exactly that
// shape — a leaf by Java's RecordQueryPlanWithNoChildren contract, wrapping a
// RecordQueryIndexPlan whose scan comparands are only reachable from its arm
// here.
//
// TestFinalizePlanCoversStructuralKey guards both gaps. It reflects over every
// plan type's struct fields, flags the ones whose type transitively carries a
// Value, a QueryPredicate, a ComparisonRange or a plan edge, and then requires
// each flagged field to be proven BEHAVIOURALLY: a sentinel
// RecordConstructorValue planted in the field must come back stamped after
// FinalizePlan. A plan that grows a new value-bearing field, or one whose
// subtree stops being reachable, fails that test rather than silently going
// unstamped.
func stampNodeLocalValues(plan plans.RecordQueryPlan, repo *values.TypeProtoRepository) {
	stampValue(plan.GetResultValue(), repo)

	switch p := plan.(type) {
	case *plans.RecordQueryProjectionPlan:
		stampValues(p.GetProjections(), repo)
	case *plans.RecordQueryPredicatesFilterPlan:
		stampPredicates(p.GetPredicates(), repo)
	case *plans.RecordQueryFilterPlan:
		stampPredicates(p.GetPredicates(), repo)
	case *plans.RecordQueryNestedLoopJoinPlan:
		stampPredicates(p.GetPredicates(), repo)
	case *plans.RecordQueryStreamingAggregationPlan:
		stampValues(p.GetGroupingKeys(), repo)
		for _, a := range p.GetAggregates() {
			stampValue(a.Operand, repo)
		}
	case *plans.RecordQueryScanPlan:
		stampValues(p.GetPrimaryKeyValues(), repo)
		stampScanComparisons(p.GetScanComparisons(), repo)
	case *plans.RecordQueryIndexPlan:
		stampValues(p.GetCommonPrimaryKeyValues(), repo)
		stampScanComparisons(p.GetScanComparisons(), repo)
	case *plans.RecordQueryAggregateIndexPlan:
		// The wrapped index scan is a STRUCTURAL field, not a child: this plan
		// is Java's RecordQueryPlanWithNoChildren and GetChildren returns nil.
		// So the plan walk never descends into it and only this arm reaches its
		// comparands and common primary key. Recursing through
		// stampNodeLocalValues rather than repeating the index-plan field list
		// keeps the two in step by construction. The nil guard is for the
		// struct-literal test plans that bypass the constructor — a typed-nil
		// pointer in an interface is not == nil, so it must be checked here.
		if idx := p.GetIndexPlan(); idx != nil {
			stampNodeLocalValues(idx, repo)
		}
	case *plans.RecordQueryVectorIndexPlan:
		stampScanComparisons(p.GetPrefixComparisons(), repo)
		stampValue(p.GetQueryVector(), repo)
		stampValue(p.GetK(), repo)
	case *plans.RecordQueryInMemorySortPlan:
		for _, sk := range p.GetSortKeys() {
			stampValue(sk.ValueExpr, repo)
		}
	case *plans.RecordQueryMergeSortUnionPlan:
		stampValues(p.GetComparisonKeys(), repo)
	case *plans.RecordQueryInUnionPlan:
		stampValues(p.GetComparisonKeys(), repo)
	case *plans.RecordQueryIntersectionPlan:
		stampValues(p.GetComparisonKeyValues(), repo)
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		stampValues(p.GetComparisonKey(), repo)
	case *plans.RecordQueryComparatorPlan:
		stampValues(p.GetComparisonKeyValues(), repo)
	case *plans.RecordQueryExplodePlan:
		stampValue(p.GetCollectionValue(), repo)
	case *plans.RecordQueryValuesPlan:
		stampValues(p.GetColumns(), repo)
	case *plans.RecordQueryTableFunctionPlan:
		stampValue(p.GetStreamValue(), repo)
	case *plans.RecordQueryFirstOrDefaultPlan:
		stampValue(p.GetDefaultValue(), repo)
	case *plans.RecordQueryDefaultOnEmptyPlan:
		stampValue(p.GetDefaultValue(), repo)
	case *plans.RecordQueryLimitPlan:
		stampValue(p.GetLimitValue(), repo)
	}
}

// stampValues stamps each value in a slice.
func stampValues(vs []values.Value, repo *values.TypeProtoRepository) {
	for _, v := range vs {
		stampValue(v, repo)
	}
}

// stampPredicates crosses from the predicate spine into the value spine. The
// two walkers are disjoint — WalkPredicate does not descend into Values and
// WalkValue does not descend into predicates — so a predicate's value operands
// are only reachable by walking both.
func stampPredicates(preds []predicates.QueryPredicate, repo *values.TypeProtoRepository) {
	for _, p := range preds {
		predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
			switch q := node.(type) {
			case *predicates.ComparisonPredicate:
				stampValue(q.Operand, repo)
				stampValue(q.Comparison.Operand, repo)
			case *predicates.ValuePredicate:
				stampValue(q.Value, repo)
			case *predicates.ExistentialValuePredicate:
				stampValue(q.Value, repo)
				stampValue(q.Comparison.Operand, repo)
			case *predicates.Placeholder:
				stampValue(q.Value, repo)
				stampComparisonRange(q.CompRange, repo)
			}
			return true
		})
	}
}

// stampScanComparisons reaches the comparands baked into a scan's ranges.
func stampScanComparisons(ranges []*predicates.ComparisonRange, repo *values.TypeProtoRepository) {
	for _, r := range ranges {
		stampComparisonRange(r, repo)
	}
}

// stampComparisonRange stamps every comparand of one range. GetComparisons()
// enumerates equality and inequality comparisons uniformly, so a range shape
// added later cannot slip past a hand-branched IsEquality/IsInequality test.
func stampComparisonRange(r *predicates.ComparisonRange, repo *values.TypeProtoRepository) {
	if r == nil {
		return
	}
	for _, c := range r.GetComparisons() {
		if c != nil {
			stampValue(c.Operand, repo)
		}
	}
}

// stampValue walks one value tree and stamps every record constructor in it.
//
// The walk continues THROUGH a stamped constructor rather than pruning at it:
// a constructor's children can hold further constructors (a nested record
// literal), and those need their own descriptors — which, coming from the same
// repository, are identical to the ones their parent's descriptor references.
func stampValue(v values.Value, repo *values.TypeProtoRepository) {
	if v == nil {
		return
	}
	values.WalkValue(v, func(node values.Value) bool {
		rc, ok := node.(*values.RecordConstructorValue)
		if !ok {
			return true
		}
		md, err := repo.MessageDescriptorFor(rc.Type())
		if err != nil {
			// A type with no message form. Not a query failure — the
			// constructor keeps its map representation.
			return true
		}
		rc.SetMessageDescriptor(md)
		return true
	})
}
