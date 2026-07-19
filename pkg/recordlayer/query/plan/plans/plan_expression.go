package plans

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// This file makes a RecordQueryPlan BE a RelationalExpression, which is what
// Java has and Go did not:
//
//	QueryPlan<T> extends PlanHashable, RelationalExpression   (QueryPlan.java:51)
//	RecordQueryPlan extends QueryPlan<FDBQueriedRecord<Message>>
//	                                                     (RecordQueryPlan.java:73)
//
// plan.go's package doc claimed the opposite — "physical and logical plan
// trees live in different namespaces in Java" — and the whole
// physical_*_wrapper.go layer, plus the nil-inner shell bug class, descends
// from that misreading. Java does keep plans in a separate PACKAGE (which
// this package mirrors correctly); it does not keep them in a separate
// HIERARCHY.
//
// STEP 1 of RFC-183 P5. Plans still store their children as raw
// RecordQueryPlan pointers, so GetQuantifiers reports none — see the method
// comment. Step 2 replaces that storage with a Quantifier over a Reference,
// at which point the parent->child edge is stored ONCE and the shell state
// becomes unrepresentable rather than merely absent.

// PlanExprBase supplies the RelationalExpression methods that are identical
// across every plan type. Embed it; override anything a specific plan needs
// to answer differently (a set-op overrides ChildrenAsSet, a correlating
// operator overrides CanCorrelate and GetCorrelatedToWithoutChildren).
//
// Deliberately a zero-size struct: embedding it must not change any plan's
// memory layout, its construction, or its hash.
type PlanExprBase struct{}

// CanCorrelate reports whether this operator anchors a correlation between
// its children. False for the physical operators that simply consume their
// input; the join-shaped plans override.
func (PlanExprBase) CanCorrelate() bool { return false }

// ChildrenAsSet reports whether the children are commutative. False by
// default; the set operations override.
func (PlanExprBase) ChildrenAsSet() bool { return false }

// GetCorrelatedToWithoutChildren returns the correlations this node's own
// information depends on. Empty by default — a plan that carries predicates
// or a result value referencing an outer quantifier must override, or
// correlation-driven rules will misclassify it.
func (PlanExprBase) GetCorrelatedToWithoutChildren() map[values.CorrelationIdentifier]struct{} {
	return map[values.CorrelationIdentifier]struct{}{}
}

// GetQuantifiers returns no quantifiers.
//
// This is honest rather than lazy at this step: a plan's children really are
// raw RecordQueryPlan pointers right now, so there are no quantifiers to
// report, and synthesising throwaway ones per call would invent Reference
// identities that nothing else shares. Step 2 gives each plan real quantifier
// storage and this method goes away in favour of per-type accessors.
//
// Nothing traverses plans as expressions yet, so reporting none changes no
// behaviour today.
func (PlanExprBase) GetQuantifiers() []expressions.Quantifier { return nil }

// GetResultValue returns a value standing for the rows this plan emits.
//
// Matches what the physical wrappers return — a fresh QuantifiedObjectValue —
// so behaviour is unchanged as the wrapper layer is retired. The rich row
// TYPE remains available through GetResultType.
//
// FOUR PLANS SHADOW THIS with their own resultValue field: Map, FlatMap,
// NestedLoopJoin and MultiIntersectionOnValues. That is the richer answer and
// matches Java (RecordQueryFlatMapPlan.getResultValue returns the result
// value), but those fields are unguarded constructor parameters, and the
// surrounding code nil-checks them (map.go, multi_intersection.go), so nil
// looks reachable — a hazard once step 2 starts dereferencing this while
// traversing plans as expressions.
//
// Measured before relying on it: instrumenting all four to report a nil
// resultValue found ZERO occurrences across the 2407-query corpus and the
// entire Go test suite. The nil checks are defensive, not reachable, so no
// guard is added here — one that never fires would be noise, and papering
// over the accessor is the wrong layer anyway. If step 2 ever needs the
// invariant enforced, the place is construction (Java's resultValue is
// @Nonnull), not this method.
//
// One asymmetry to carry into step 2: physicalMultiIntersectionWrapper's
// nil-fallback returns the FIRST INNER's flowed object value, not a fresh
// stand-in — so a blanket non-nil guard here would silently change that
// wrapper's answer. The plan cannot reproduce that fallback yet because it
// has no quantifiers; that is precisely what step 2 fixes.
func (PlanExprBase) GetResultValue() values.Value {
	return values.NewQuantifiedObjectValue(values.UniqueCorrelationIdentifier())
}

// planEqualsAsExpression is the shared body of every plan's
// EqualsWithoutChildren(RelationalExpression, *AliasMap).
//
// The alias map is unused while plans hold no quantifiers: alias-aware
// comparison exists to equate two expressions whose children are bound to
// differently-named quantifiers, and a plan currently has none. Step 2 makes
// it meaningful.
//
// Not a method on PlanExprBase because it needs the concrete receiver to
// reach its EqualsPlanWithoutChildren; each plan type spells a one-line
// override that calls this.
func planEqualsAsExpression(self RecordQueryPlan, other expressions.RelationalExpression) bool {
	op, ok := other.(RecordQueryPlan)
	return ok && self.EqualsPlanWithoutChildren(op)
}

// scanComparisonCorrelations returns the correlations a scan-shaped plan
// reaches through its comparison operands. Ported from the cascades package's
// helper of the same name, which the scan and index-scan physical wrappers used
// for GetCorrelatedToWithoutChildren; the wrappers are retired in a later step.
func scanComparisonCorrelations(comps []*predicates.ComparisonRange) map[values.CorrelationIdentifier]struct{} {
	out := map[values.CorrelationIdentifier]struct{}{}
	collect := func(c *predicates.Comparison) {
		if c == nil || c.Operand == nil {
			return
		}
		for a := range values.GetCorrelatedToOfValue(c.Operand) {
			out[a] = struct{}{}
		}
		// A query-parameter (ConstantObjectValue) comparand is an execution constant
		// bound at run time, NOT a row correlation — its constant-pool alias appears
		// in GetCorrelatedToOfValue but must not make a `Scan(T,[k=?param])` look
		// join-correlated to planning (B1 leg detection) or to the
		// probe-fed-residual guard (compensationProbeCorrelations). Subtract any such
		// aliases — the value-level twin of deletePredicateConstantObjectAliases.
		values.WalkValue(c.Operand, func(node values.Value) bool {
			if cov, ok := node.(*values.ConstantObjectValue); ok {
				delete(out, cov.Alias)
			}
			return true
		})
	}
	for _, cr := range comps {
		if cr == nil || cr.IsEmpty() {
			continue
		}
		if cr.IsEquality() {
			collect(cr.GetEqualityComparison())
		} else if cr.IsInequality() {
			for _, c := range cr.GetInequalityComparisons() {
				collect(c)
			}
		}
	}
	return out
}
