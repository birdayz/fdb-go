package cascades

import (
	"sort"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// sortInJoinValues ports Java's InSource.sortValues (InSource.java:142-149).
//
// Java backs a "sorted" IN-source claim by construction: SortedInValuesSource
// physically sorts its literal list in its constructor — i.e. at planning
// time — and SortedInParameterSource sorts its bound list in getValues at
// execution time, since a parameter's values aren't known until then. Both
// call the same InSource.sortValues(values, isReversed): size<2 returns the
// input unchanged (no copy), otherwise a copy is sorted with
// Comparator.comparing(Comparable.class::cast), reversed when isReversed.
//
// Go's ImplementInJoinRule (rule_implement_in_join.go) reaches only the
// literal-list shape today — every SQL path that reaches this rule's match
// (InComparisonToExplodeRule, `col IN (v1,...,vn)`) wraps the list as a
// ConstantValue, so extractInValues always has a concrete slice to sort
// before RecordQueryInJoinPlan.SetInValues. This is called once, right
// there, mirroring SortedInValuesSource's planning-time sort. The
// InSourceParameter/InSourceComparand classification exists
// (classifyInSourceKind) for a QuantifiedObjectValue/constant-expression
// collection value, but no SQL-facing rule builds an ExplodeExpression with
// such a collection value that also satisfies ImplementInJoinRule's other
// match preconditions (resultValue == QOV(the single non-explode inner
// quantifier) — SQL UNNEST goes through ImplementExplodeRule +
// ImplementNestedLoopJoinRule instead, whose result exposes the exploded
// value itself). So the execution-time sort SortedInParameterSource performs
// has no reachable Go counterpart yet; wiring one in is scoped to whatever
// workstream first makes InSourceParameter reachable from SQL, alongside the
// executor's InJoin loop itself (which today only iterates a plan-time
// literal list — see executeInJoin, executor_new_plans.go).
//
// Java's comparator throws (ClassCastException / NullPointerException) on a
// null or cross-type pair rather than defining an order for it. Go mirrors
// the "throws rather than silently misorders" character via
// values.CompareOrdered's error return (this codebase never panics in
// library code) instead of an unchecked exception — using the SAME total
// order the executor's in-memory sort and merge cursors already use to
// agree with an ordered index scan, since the values sorted here must agree
// with the ordering the plan carrying them goes on to claim.
//
// ok reports whether every pair compared cleanly; false means the values
// could NOT be totally ordered (an incomparable pair — the planner's type
// checking is expected to exclude this on real queries) and the ORIGINAL
// slice is returned unchanged. The caller must not claim sorted=true over an
// unordered result.
func sortInJoinValues(vals []any, reverse bool) (sorted []any, ok bool) {
	if len(vals) < 2 {
		return vals, true
	}
	out := make([]any, len(vals))
	copy(out, vals)
	var sortErr error
	sort.SliceStable(out, func(i, j int) bool {
		if sortErr != nil {
			return false
		}
		c, err := values.CompareOrdered(out[i], out[j])
		if err != nil {
			sortErr = err
			return false
		}
		if reverse {
			return c > 0
		}
		return c < 0
	})
	if sortErr != nil {
		return vals, false
	}
	return out, true
}
