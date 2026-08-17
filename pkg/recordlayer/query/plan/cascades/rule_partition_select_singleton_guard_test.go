package cascades

import (
	"sort"
	"strings"
	"testing"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// nestedLowerAliasSets returns the direct quantifier-alias set of every
// SelectExpression NESTED beneath the root (depth >= 1) — the "lower" selects
// PartitionSelectRule builds under a fresh lower quantifier. The ROOT's own
// alias set is deliberately excluded: the yielded UPPER legitimately carries
// the untorn {A,X} component plus a (possibly alias-reused) lower quantifier,
// which is NOT a cross-product tear — only a nested LOWER that unions the whole
// component with an extra leg is the shape the singleton clause must reject. A
// per-Reference visited set guards against cycles.
func nestedLowerAliasSets(root expressions.RelationalExpression) []map[values.CorrelationIdentifier]struct{} {
	var out []map[values.CorrelationIdentifier]struct{}
	seen := make(map[*expressions.Reference]bool)
	var descend func(e expressions.RelationalExpression)
	descend = func(e expressions.RelationalExpression) {
		sel, ok := e.(*expressions.SelectExpression)
		if !ok {
			return
		}
		for _, q := range sel.GetQuantifiers() {
			ref := q.GetRangesOver()
			if ref == nil || seen[ref] {
				continue
			}
			seen[ref] = true
			for _, m := range ref.Members() {
				if nested, ok := m.(*expressions.SelectExpression); ok {
					set := make(map[values.CorrelationIdentifier]struct{})
					for _, nq := range nested.GetQuantifiers() {
						set[nq.GetAlias()] = struct{}{}
					}
					out = append(out, set)
				}
				descend(m)
			}
		}
	}
	descend(root)
	return out
}

func isSupersetOf(super, sub map[values.CorrelationIdentifier]struct{}) bool {
	for a := range sub {
		if _, ok := super[a]; !ok {
			return false
		}
	}
	return true
}

func sortedAliasNames(set map[values.CorrelationIdentifier]struct{}) string {
	names := make([]string, 0, len(set))
	for a := range set {
		names = append(names, a.Name())
	}
	sort.Strings(names)
	return "{" + strings.Join(names, ",") + "}"
}

// TestPartitionSelect_CrossProductLowerUsesExactSentinel pins the Case-1
// lower's scalar row. That row is planner-generated, so its literal must carry
// an exact type just like a translated SQL literal; an UnknownType here makes
// the checked Select constructor reject every disconnected bipartition.
func TestPartitionSelect_CrossProductLowerUsesExactSentinel(t *testing.T) {
	t.Parallel()

	a := partitionBinaryNamedScanQuantifier("A")
	b := partitionBinaryNamedScanQuantifier("B")
	c := partitionBinaryNamedScanQuantifier("C")
	selectExpr := mustPartitionBinaryConstruct(expressions.NewSelectExpression(
		mustPartitionBinaryConstruct(a.RequireFlowedObjectValue()),
		[]expressions.Quantifier{a, b, c},
		nil,
	))
	yields := mustFirePartitionExpressionRule(
		t, NewPartitionSelectRule(), expressions.InitialOf(selectExpr))
	if len(yields) == 0 {
		t.Fatal("PartitionSelectRule yielded nothing for an independent three-way cross product")
	}

	foundExactSentinel := false
	seen := make(map[*expressions.Reference]struct{})
	var visit func(expressions.RelationalExpression)
	visit = func(expr expressions.RelationalExpression) {
		if constructor, ok := expr.GetResultValue().(*values.RecordConstructorValue); ok {
			for _, field := range constructor.Fields {
				constant, isConstant := field.Value.(*values.ConstantValue)
				if !isConstant || constant.Value != int64(1) {
					continue
				}
				if !constant.Type().Equals(values.NotNullLong) {
					t.Fatalf("cross-product sentinel type = %v, want LONG NOT NULL", constant.Type())
				}
				foundExactSentinel = true
			}
		}
		for _, quantifier := range expr.GetQuantifiers() {
			ref := quantifier.GetRangesOver()
			if ref == nil {
				continue
			}
			if _, alreadySeen := seen[ref]; alreadySeen {
				continue
			}
			seen[ref] = struct{}{}
			for _, member := range ref.AllMembers() {
				visit(member)
			}
		}
	}
	for _, yield := range yields {
		visit(yield)
	}
	if !foundExactSentinel {
		t.Fatal("PartitionSelectRule yielded no exact Case-1 cross-product sentinel")
	}
}

// TestPartitionSelect_RejectsNonSingletonCrossProductLower pins the SINGLETON
// clause of the disconnected-lower guard in PartitionSelectRule (the
// lowerComponentsAreSingletons conjunct at the disconnectedLower `continue`).
//
// Shape (the logical analog of `FROM A, B, EE, A.ARR AS X`):
//   - component {A, X}: X is a lateral unnest correlated to A (a quantifier-
//     level correlation edge, NO predicate between them) — one multi-alias
//     independent join component;
//   - component {B}: an unrelated singleton leg;
//   - component {EE}: another unrelated singleton leg.
//
// The bipartition lower={A,X,B} | upper={EE} is a component-ALIGNED cross
// product (isCrossProduct is true — no component straddles the halves), so the
// isCrossProduct half of the guard admits it. Only the SINGLETON half rejects
// it: the lower unions the WHOLE {A,X} component (a real join component) with a
// singleton leg, which is NOT an unavoidable cross product. Java's
// PartitionSelectRule (isCrossProduct only) plans this bipartition; Go's
// positionalMergeCase cannot correctly wire it (it mis-wires the source-relative
// ordinal for a lower holding an unnest component plus an extra leg), so the
// mis-partition PLANS cleanly and fails at RUNTIME in the executor with the
// RFC-173 ordinal tripwire ("multi-leg row cannot serve a source-relative
// ordinal"). The guard keeps it out of the search entirely.
//
// Dropping the lowerComponentsAreSingletons conjunct from the guard makes the
// rule yield a lower containing {A,X}+leg — this test goes RED. (Revert-proof:
// only the FDB TestFDB_OrderByGather previously caught the regression.)
func TestPartitionSelect_RejectsNonSingletonCrossProductLower(t *testing.T) {
	t.Parallel()

	a := partitionBinaryNamedScanQuantifier("A")
	b := partitionBinaryNamedScanQuantifier("B")
	ee := partitionBinaryNamedScanQuantifier("EE")

	// X ranges over a filter referencing A, so X correlates to A with no
	// predicate between them — {A, X} is one independent component (the
	// lateral-unnest pairing). Mirrors TestTransitiveCorrelationOrder_RangesOverEdges.
	xInner := mustPartitionBinaryConstruct(expressions.NewLogicalFilterExpression(
		[]predicates.QueryPredicate{partitionBinaryJoinPredicate("A", "X")},
		pbForEachOf(partitionBinaryScan("X")),
	))
	x := expressions.NamedForEachQuantifier(
		values.NamedCorrelationIdentifier("X"),
		expressions.InitialOf(xInner),
	)

	// Pure cross product at the top (no join predicates); the result value
	// names only the EE leg, so the {A,X,B}|{EE} bipartition is a clean Case-1
	// cross product (no upper->lower correlation) — it WOULD be yielded if the
	// guard admitted it.
	sel := mustPartitionBinaryConstruct(expressions.NewSelectExpression(
		mustPartitionBinaryConstruct(ee.RequireFlowedObjectValue()),
		[]expressions.Quantifier{a, b, ee, x},
		nil,
	))

	yields := mustFirePartitionExpressionRule(t, NewPartitionSelectRule(), expressions.InitialOf(sel))

	// The rule must actually enumerate bipartitions here (the exempt cross
	// products {B,EE}|… and the {A,X}|… component peel), else the reject
	// assertion below would be vacuously true.
	if len(yields) == 0 {
		t.Fatal("PartitionSelectRule yielded nothing; the reject assertion would be vacuous")
	}

	component := partitionBinaryAliasSet("A", "X")
	for _, y := range yields {
		for _, lower := range nestedLowerAliasSets(y) {
			if len(lower) >= 3 && isSupersetOf(lower, component) {
				t.Errorf("PartitionSelectRule yielded a disconnected lower %s that unions the "+
					"multi-alias component {A,X} with an extra leg — the lowerComponentsAreSingletons "+
					"clause of the disconnected-lower guard must reject it (a real join component is "+
					"not an unavoidable cross product)", sortedAliasNames(lower))
			}
		}
	}
}
