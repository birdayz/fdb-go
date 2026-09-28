package cascades

import (
	"errors"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/recordlayer/query/plan/plans"
)

// FinalizePlan collects computed types, validates their closures, seals one
// repository, and only then binds descriptors. Java's QueryPlan.optimize builds
// its TypeRepository after collecting used types (QueryPlan.java:648–678).
// Binding while still registering types creates multiple descriptor identities
// inside one plan. Across separately finalized subquery plans the strict
// by-number copy in rowMessageToProtoValue still reconciles foreign messages.
//
// Call only on a plan-cache miss before publication: binding is a plan-time
// mutation, never a lazy evaluation-time write. Write-fed constructors retain
// their raw representation so assignment can apply the stored target's type.
// Failed roots (duplicate field names, unescapable names, erased arrays and
// other non-protobuf types) retain the supported raw fallback without poisoning
// unrelated roots. Conflicting valid declarations of a name remain errors.
func FinalizePlan(plan plans.RecordQueryPlan) error {
	if plan == nil {
		return nil
	}
	st := &planStamper{repo: values.NewTypeProtoRepository()}
	var constructors []*values.RecordConstructorValue
	var promotions []*values.PromoteValue
	forEachPlanValue(plan, func(node values.Value) {
		switch v := node.(type) {
		case *values.RecordConstructorValue:
			if err := st.repo.RegisterType(v.Type()); err != nil {
				var clash *values.DeclaredNameClashError
				if errors.As(err, &clash) && st.nameClash == nil {
					st.nameClash = err
				}
				return
			}
			constructors = append(constructors, v)
		case *values.PromoteValue:
			if err := v.RegisterPromotionTypes(st.repo); err != nil && st.nameClash == nil {
				st.nameClash = err
			}
			promotions = append(promotions, v)
		}
	})
	if st.nameClash != nil {
		return st.nameClash
	}
	if err := st.repo.Seal(); err != nil {
		return err
	}
	for _, rc := range constructors {
		stampRecordConstructor(rc, st)
	}
	for _, promotion := range promotions {
		if err := promotion.BindPromotionTypes(st.repo); err != nil {
			return err
		}
	}
	return st.nameClash
}

// planStamper carries the one repository a plan's constructors are stamped
// from, and the first declared-name clash the walk met.
type planStamper struct {
	repo      *values.TypeProtoRepository
	nameClash error
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

// forEachNodeLocalValue reaches every value tree hanging off THIS plan node.
// It does not walk the plan — forEachPlanValue does that, and owns
// the DAG seen-set and the write-fed prune.
//
// GetResultValue() alone is not enough. Many plans flow their inner's value
// through GetResultValue and keep the computed tree in a node-local field
// instead (a projection's projections, a filter's predicates, an aggregation's
// grouping keys), so a result-value-only walk would miss precisely the
// constructors that produce computed records.
//
// GetChildren() alone is not enough either, and that gap is the sharper one: a
// plan may hold a whole sub-PLAN in a structural field that GetChildren
// deliberately does not return. RecordQueryAggregateIndexPlan and
// RecordQueryCoveringIndexPlan are both that shape — leaves by Java's
// RecordQueryPlanWithNoChildren contract, each wrapping a RecordQueryIndexPlan
// whose scan comparands are only reachable from its arm here.
//
// TestFinalizePlanCoversStructuralKey guards both gaps. It reflects over every
// plan type's struct fields, flags the ones whose type transitively carries a
// Value, a QueryPredicate, a ComparisonRange or a plan edge, and then requires
// each flagged field to be proven BEHAVIOURALLY: a sentinel
// RecordConstructorValue planted in the field must come back stamped after
// FinalizePlan. A plan that grows a new value-bearing field, or one whose
// subtree stops being reachable, fails that test rather than silently going
// unstamped.
func forEachNodeLocalValue(plan plans.RecordQueryPlan, emit func(values.Value)) {
	emit(plan.GetResultValue())

	switch p := plan.(type) {
	case *plans.RecordQueryProjectionPlan:
		forEachValue(p.GetProjections(), emit)
	case *plans.RecordQueryPredicatesFilterPlan:
		forEachPredicateValue(p.GetPredicates(), emit)
	case *plans.RecordQueryFilterPlan:
		forEachPredicateValue(p.GetPredicates(), emit)
	case *plans.RecordQueryNestedLoopJoinPlan:
		forEachPredicateValue(p.GetPredicates(), emit)
	case *plans.RecordQueryStreamingAggregationPlan:
		forEachValue(p.GetGroupingKeys(), emit)
		for _, a := range p.GetAggregates() {
			emit(a.Operand)
		}
	case *plans.RecordQueryScanPlan:
		forEachValue(p.GetPrimaryKeyValues(), emit)
		forEachScanComparisonValue(p.GetScanComparisons(), emit)
	case *plans.RecordQueryIndexPlan:
		forEachValue(p.GetCommonPrimaryKeyValues(), emit)
		forEachScanComparisonValue(p.GetScanComparisons(), emit)
	case *plans.RecordQueryAggregateIndexPlan:
		// The wrapped index scan is a STRUCTURAL field, not a child: this plan
		// is Java's RecordQueryPlanWithNoChildren and GetChildren returns nil.
		// So the plan walk never descends into it and only this arm reaches its
		// comparands and common primary key. Recursing through
		// forEachNodeLocalValue rather than repeating the index-plan field list
		// keeps the two in step by construction. The nil guard is for the
		// struct-literal test plans that bypass the constructor — a typed-nil
		// pointer in an interface is not == nil, so it must be checked here.
		if idx := p.GetIndexPlan(); idx != nil {
			forEachNodeLocalValue(idx, emit)
		}
	case *plans.RecordQueryCoveringIndexPlan:
		// The second plan of the same shape, and for the same reason: the
		// wrapped index scan is a STRUCTURAL field (Java's covering plan
		// likewise implements RecordQueryPlanWithNoChildren), GetChildren
		// returns nil, so the plan walk never descends into it.
		//
		// Recursing through forEachNodeLocalValue rather than reading this
		// plan's own delegating GetScanComparisons/GetCommonPrimaryKeyValues is
		// deliberate: the delegates would have to be re-enumerated here every
		// time the index-plan arm above grows a field, and the day they drift
		// the miss is silent — an unstamped RecordConstructorValue does not
		// fail, it quietly evaluates to its name-keyed map instead of the
		// field-number-keyed message. Recursion keeps the two in step by
		// construction.
		if idx := p.GetIndexPlan(); idx != nil {
			forEachNodeLocalValue(idx, emit)
		}
	case *plans.RecordQueryVectorIndexPlan:
		forEachScanComparisonValue(p.GetPrefixComparisons(), emit)
		emit(p.GetQueryVector())
		emit(p.GetK())
	case *plans.RecordQueryInMemorySortPlan:
		for _, sk := range p.GetSortKeys() {
			emit(sk.ValueExpr)
		}
	case *plans.RecordQueryMergeSortUnionPlan:
		forEachValue(p.GetComparisonKeys(), emit)
	case *plans.RecordQueryInUnionPlan:
		forEachValue(p.GetComparisonKeys(), emit)
	case *plans.RecordQueryIntersectionPlan:
		forEachValue(p.GetComparisonKeyValues(), emit)
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		forEachValue(p.GetComparisonKey(), emit)
	case *plans.RecordQueryComparatorPlan:
		forEachValue(p.GetComparisonKeyValues(), emit)
	case *plans.RecordQueryExplodePlan:
		emit(p.GetCollectionValue())
	case *plans.RecordQueryValuesPlan:
		forEachValue(p.GetColumns(), emit)
	case *plans.RecordQueryTableFunctionPlan:
		emit(p.GetStreamValue())
	case *plans.RecordQueryFirstOrDefaultPlan:
		emit(p.GetDefaultValue())
	case *plans.RecordQueryDefaultOnEmptyPlan:
		emit(p.GetDefaultValue())
	case *plans.RecordQueryLimitPlan:
		emit(p.GetLimitValue())
	}
}

// ForEachPlanRecordConstructor visits every RecordConstructorValue the plan-time
// bake considers, in the bake's own order. Both this visitor and FinalizePlan
// use forEachPlanValue, including its write-fed prune; the census counts exactly
// the constructor population considered for registration.
//
// That identity is the point. A census that walks only result values and child
// edges misses every constructor reached through a projection list, a predicate,
// a grouping key, a scan comparand, a default value or a structural plan field,
// and it misses them SILENTLY — a smaller population that still reads like a
// measurement. A census that walks MORE than the bake fails the other way, and
// this function shipped that bug once: it descended into write-fed subtrees the
// bake prunes, so it reported constructors FinalizePlan never stamps and would
// have inflated any DML census. Both directions are why the walk is shared
// rather than described.
//
// Write-fed subtrees are excluded, node and descendants, because the bake
// excludes them: a plan feeding an INSERT, UPDATE, DELETE or temp-table insert
// has its row shape fixed by the TARGET's declared descriptor, not by the
// constructor's own inferred type, so stamping there would bake the wrong
// descriptor. TestFinalizePlanCoversStructuralKey asserts that exclusion is
// deliberate, and TestTheCensusWalkPrunesWriteFedSubtreesAsTheBakeDoes pins that
// this walk and the bake agree about it.
//
// The value walk continues THROUGH a constructor rather than pruning at it: a
// constructor's children can hold further constructors (a nested record
// literal), and those need their own descriptors — which, coming from the same
// repository, are identical to the ones their parent's descriptor references.
// That containment is what makes a stamped parent imply a stamped child, and
// TestTheBakeStampsAParentAndItsChildTogetherOrNeither pins it.
//
// visit is called once per constructor occurrence, so a value reachable by two
// routes is visited twice; plan NODES are visited once each.
func ForEachPlanRecordConstructor(plan plans.RecordQueryPlan, visit func(*values.RecordConstructorValue)) {
	forEachPlanValue(plan, func(node values.Value) {
		if rc, ok := node.(*values.RecordConstructorValue); ok {
			visit(rc)
		}
	})
}

func forEachPlanValue(plan plans.RecordQueryPlan, visit func(values.Value)) {
	seen := map[plans.RecordQueryPlan]struct{}{}
	var walk func(plans.RecordQueryPlan)
	walk = func(p plans.RecordQueryPlan) {
		if p == nil {
			return
		}
		if _, done := seen[p]; done {
			return
		}
		seen[p] = struct{}{}
		if feedsAWrite(p) {
			return
		}
		forEachNodeLocalValue(p, func(v values.Value) {
			if v == nil {
				return
			}
			values.WalkValue(v, func(node values.Value) bool {
				visit(node)
				return true
			})
		})
		for _, c := range p.GetChildren() {
			walk(c)
		}
	}
	walk(plan)
}

// forEachValue emits each value in a slice.
func forEachValue(vs []values.Value, emit func(values.Value)) {
	for _, v := range vs {
		emit(v)
	}
}

// forEachPredicateValue crosses from the predicate spine into the value spine. The
// two walkers are disjoint — WalkPredicate does not descend into Values and
// WalkValue does not descend into predicates — so a predicate's value operands
// are only reachable by walking both.
func forEachPredicateValue(preds []predicates.QueryPredicate, emit func(values.Value)) {
	for _, p := range preds {
		predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
			switch q := node.(type) {
			case *predicates.ComparisonPredicate:
				emit(q.Operand)
				emit(q.Comparison.Operand)
			case *predicates.ValuePredicate:
				emit(q.Value)
			case *predicates.ExistentialValuePredicate:
				emit(q.Value)
				emit(q.Comparison.Operand)
			case *predicates.Placeholder:
				emit(q.Value)
				forEachComparisonRangeValue(q.CompRange, emit)
			}
			return true
		})
	}
}

// forEachScanComparisonValue reaches the comparands baked into a scan's ranges.
func forEachScanComparisonValue(ranges []*predicates.ComparisonRange, emit func(values.Value)) {
	for _, r := range ranges {
		forEachComparisonRangeValue(r, emit)
	}
}

// forEachComparisonRangeValue emits every comparand of one range. GetComparisons()
// enumerates equality and inequality comparisons uniformly, so a range shape
// added later cannot slip past a hand-branched IsEquality/IsInequality test.
func forEachComparisonRangeValue(r *predicates.ComparisonRange, emit func(values.Value)) {
	if r == nil {
		return
	}
	for _, c := range r.GetComparisons() {
		if c != nil {
			emit(c.Operand)
		}
	}
}

// stampRecordConstructor binds one constructor after collection and sealing.
// It walks no children and must not extend the sealed repository.
func stampRecordConstructor(rc *values.RecordConstructorValue, st *planStamper) {
	md, err := st.repo.MessageDescriptorFor(rc.Type())
	if err != nil {
		var clash *values.DeclaredNameClashError
		if errors.As(err, &clash) && st.nameClash == nil {
			// One declared name over two shapes: a query failure, carried out of
			// the walk (FinalizePlan).
			st.nameClash = err
		}
		// Otherwise a type with no message form, or a file that does not
		// validate for another reason (see FinalizePlan's doc). Not a query
		// failure — the constructor keeps its map representation.
		return
	}
	rc.SetMessageDescriptor(md)
}
