package cascades

import (
	"errors"

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
// Every OTHER descriptor failure leaves the constructor unstamped, evaluating
// to its name-keyed map exactly as it did before the bake existed, and is not
// a query failure. The failures that reach it, enumerated rather than
// characterised — a COUNT is the same over-claim one layer up:
//
//   - a type with no message form (a bare scalar record field, an erased
//     array), a *values.ProtoTypeError, which
//     TestFinalizePlanReturnsTheNameClashAndKeepsTheMapForNoMessageForm drives;
//   - a record or field name protoname cannot ESCAPE (`$lead`, `a-b`, a name
//     starting `__0`), also a *values.ProtoTypeError but NOT a missing message
//     form — defineRecordLocked refuses it before a message is built;
//   - a synthesised file that does not VALIDATE for a reason other than a
//     declared-name clash, returned by compileLocked through the same call.
//     A FULL OUTER JOIN over legs that both carry `ID` reaches it: the
//     ordinal row is built by NewRawRecordConstructorValue, which keeps field
//     names VERBATIM by design (NewRecordConstructorValue suffixes instead —
//     `ID`, `ID_2`), so its descriptor cannot validate. Compilation is
//     per-REPOSITORY and the bad message stays in it, so the damage is NOT
//     that row alone: every type asked for after it fails the same way. On
//     the query below THREE of the four constructors end up with no descriptor though only
//     ONE repeats a name, and the fourth keeps the descriptor it was given
//     before the bad message was appended — so which rows survive is
//     walk-order dependent. It costs descriptor IDENTITY rather than data —
//     the emitting paths build dense positional rows the result set reads by
//     ordinal, so every field still arrives. TODO.md's "A join row that names
//     one field twice leaves its plan's rows unstamped" carries the closure;
//     TestFinalizePlanLeavesTheDuplicateNameJoinRowUnstamped pins the query and
//     TestDuplicateFieldNameRowPoisonsTheWholeRepository the mechanism.
//
// Turning the third loud refuses a query that answers today, so it stays
// swallowed and pinned instead.
//
// One declared name over two shapes — `STRUCT foo (1 AS p)` beside `STRUCT
// foo (2 AS p, 3 AS q)` — is a query failure, and the one error this returns
// (values.DeclaredNameClashError): each constructor has a message form, and
// the two DescriptorProtos of one name make a file that does not validate.
// Java throws there (TypeRepository.build); swallowing it here let the driver
// hand such a row back as raw maps with no error.
func FinalizePlan(plan plans.RecordQueryPlan) error {
	if plan == nil {
		return nil
	}
	st := &planStamper{repo: values.NewTypeProtoRepository()}
	seenPlans := map[plans.RecordQueryPlan]struct{}{}
	stampPlanNode(plan, st, seenPlans)
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

// stampPlanNode walks the plan DAG. Plans are DAGs, not trees — a shared
// sub-plan is reachable by more than one edge — so the seen-set is required for
// termination, exactly as validatePlanNode needs it.
func stampPlanNode(plan plans.RecordQueryPlan, st *planStamper, seen map[plans.RecordQueryPlan]struct{}) {
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

	stampNodeLocalValues(plan, st)

	for _, c := range plan.GetChildren() {
		stampPlanNode(c, st, seen)
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
func stampNodeLocalValues(plan plans.RecordQueryPlan, st *planStamper) {
	stampValue(plan.GetResultValue(), st)

	switch p := plan.(type) {
	case *plans.RecordQueryProjectionPlan:
		stampValues(p.GetProjections(), st)
	case *plans.RecordQueryPredicatesFilterPlan:
		stampPredicates(p.GetPredicates(), st)
	case *plans.RecordQueryFilterPlan:
		stampPredicates(p.GetPredicates(), st)
	case *plans.RecordQueryNestedLoopJoinPlan:
		stampPredicates(p.GetPredicates(), st)
	case *plans.RecordQueryStreamingAggregationPlan:
		stampValues(p.GetGroupingKeys(), st)
		for _, a := range p.GetAggregates() {
			stampValue(a.Operand, st)
		}
	case *plans.RecordQueryScanPlan:
		stampValues(p.GetPrimaryKeyValues(), st)
		stampScanComparisons(p.GetScanComparisons(), st)
	case *plans.RecordQueryIndexPlan:
		stampValues(p.GetCommonPrimaryKeyValues(), st)
		stampScanComparisons(p.GetScanComparisons(), st)
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
			stampNodeLocalValues(idx, st)
		}
	case *plans.RecordQueryCoveringIndexPlan:
		// The second plan of the same shape, and for the same reason: the
		// wrapped index scan is a STRUCTURAL field (Java's covering plan
		// likewise implements RecordQueryPlanWithNoChildren), GetChildren
		// returns nil, so the plan walk never descends into it.
		//
		// Recursing through stampNodeLocalValues rather than reading this
		// plan's own delegating GetScanComparisons/GetCommonPrimaryKeyValues is
		// deliberate: the delegates would have to be re-enumerated here every
		// time the index-plan arm above grows a field, and the day they drift
		// the miss is silent — an unstamped RecordConstructorValue does not
		// fail, it quietly evaluates to its name-keyed map instead of the
		// field-number-keyed message. Recursion keeps the two in step by
		// construction.
		if idx := p.GetIndexPlan(); idx != nil {
			stampNodeLocalValues(idx, st)
		}
	case *plans.RecordQueryVectorIndexPlan:
		stampScanComparisons(p.GetPrefixComparisons(), st)
		stampValue(p.GetQueryVector(), st)
		stampValue(p.GetK(), st)
	case *plans.RecordQueryInMemorySortPlan:
		for _, sk := range p.GetSortKeys() {
			stampValue(sk.ValueExpr, st)
		}
	case *plans.RecordQueryMergeSortUnionPlan:
		stampValues(p.GetComparisonKeys(), st)
	case *plans.RecordQueryInUnionPlan:
		stampValues(p.GetComparisonKeys(), st)
	case *plans.RecordQueryIntersectionPlan:
		stampValues(p.GetComparisonKeyValues(), st)
	case *plans.RecordQueryMultiIntersectionOnValuesPlan:
		stampValues(p.GetComparisonKey(), st)
	case *plans.RecordQueryComparatorPlan:
		stampValues(p.GetComparisonKeyValues(), st)
	case *plans.RecordQueryExplodePlan:
		stampValue(p.GetCollectionValue(), st)
	case *plans.RecordQueryValuesPlan:
		stampValues(p.GetColumns(), st)
	case *plans.RecordQueryTableFunctionPlan:
		stampValue(p.GetStreamValue(), st)
	case *plans.RecordQueryFirstOrDefaultPlan:
		stampValue(p.GetDefaultValue(), st)
	case *plans.RecordQueryDefaultOnEmptyPlan:
		stampValue(p.GetDefaultValue(), st)
	case *plans.RecordQueryLimitPlan:
		stampValue(p.GetLimitValue(), st)
	}
}

// stampValues stamps each value in a slice.
func stampValues(vs []values.Value, st *planStamper) {
	for _, v := range vs {
		stampValue(v, st)
	}
}

// stampPredicates crosses from the predicate spine into the value spine. The
// two walkers are disjoint — WalkPredicate does not descend into Values and
// WalkValue does not descend into predicates — so a predicate's value operands
// are only reachable by walking both.
func stampPredicates(preds []predicates.QueryPredicate, st *planStamper) {
	for _, p := range preds {
		predicates.WalkPredicate(p, func(node predicates.QueryPredicate) bool {
			switch q := node.(type) {
			case *predicates.ComparisonPredicate:
				stampValue(q.Operand, st)
				stampValue(q.Comparison.Operand, st)
			case *predicates.ValuePredicate:
				stampValue(q.Value, st)
			case *predicates.ExistentialValuePredicate:
				stampValue(q.Value, st)
				stampValue(q.Comparison.Operand, st)
			case *predicates.Placeholder:
				stampValue(q.Value, st)
				stampComparisonRange(q.CompRange, st)
			}
			return true
		})
	}
}

// stampScanComparisons reaches the comparands baked into a scan's ranges.
func stampScanComparisons(ranges []*predicates.ComparisonRange, st *planStamper) {
	for _, r := range ranges {
		stampComparisonRange(r, st)
	}
}

// stampComparisonRange stamps every comparand of one range. GetComparisons()
// enumerates equality and inequality comparisons uniformly, so a range shape
// added later cannot slip past a hand-branched IsEquality/IsInequality test.
func stampComparisonRange(r *predicates.ComparisonRange, st *planStamper) {
	if r == nil {
		return
	}
	for _, c := range r.GetComparisons() {
		if c != nil {
			stampValue(c.Operand, st)
		}
	}
}

// stampValue walks one value tree and stamps every record constructor in it.
//
// The walk continues THROUGH a stamped constructor rather than pruning at it:
// a constructor's children can hold further constructors (a nested record
// literal), and those need their own descriptors — which, coming from the same
// repository, are identical to the ones their parent's descriptor references.
func stampValue(v values.Value, st *planStamper) {
	if v == nil {
		return
	}
	values.WalkValue(v, func(node values.Value) bool {
		rc, ok := node.(*values.RecordConstructorValue)
		if !ok {
			return true
		}
		md, err := st.repo.MessageDescriptorFor(rc.Type())
		if err != nil {
			var clash *values.DeclaredNameClashError
			if errors.As(err, &clash) && st.nameClash == nil {
				// One declared name over two shapes: a query failure, carried
				// out of the walk (FinalizePlan).
				st.nameClash = err
			}
			// Otherwise a type with no message form, or a file that does not
			// validate for another reason (see FinalizePlan's doc). Not a query
			// failure — the constructor keeps its map representation.
			return true
		}
		rc.SetMessageDescriptor(md)
		return true
	})
}
