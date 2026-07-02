package query

import (
	"strings"

	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 Slice 2 — the translation-time cluster-arity scoping gate
// (implementation contract ruling #1, rfcs/173-ordinal-column-resolution.md §4).
//
// The ordinal wedge covers exactly the 2-way joins that are NOT consumed as a
// leg of a name-model merge select. A naive check on the enclosing operator is
// evadable because SelectMergeRule flattens inner-join-equivalent boxes during
// exploration (`FROM (a JOIN b) t1, (c JOIN d) t2` is 2-way at translation and
// 4-way post-flattening), so the gate computes the POST-FLATTENING ForEach
// arity of the transitive inner-join-equivalent cluster, at translation time,
// by walking the logical tree with the same transparency/opacity the rule
// actually implements (rule_select_merge.go). Drift between this walk and the
// rule is caught by the loud assert in the rule's target loop — a decline is
// forbidden (it would change plan shapes, contract ruling #1).
//
// W2 status: DARK — decisions are computed and recorded at the translateJoin
// seed but nothing consumes them until W3 builds the ordinal result value.

// arityPoison marks a subtree that makes its cluster unclassifiable. The
// contract direction: anything unclassifiable counts as >2, failing toward
// the name model.
const arityPoison = -1

// wedgeGateDecision records the RFC-173 Slice 2 gate outcome for one
// translateJoin seed, for the W3 seed to consume and for tests to pin.
type wedgeGateDecision struct {
	Gated  bool
	Arity  int // post-flattening ForEach arity of the maximal cluster (arityPoison when unclassifiable; 0 when not walked)
	Reason string
}

// ordinalWedgeGate decides whether the join being seeded at translateJoin is
// in the Slice 2 ordinal wedge, and records the decision. Must be called
// BEFORE the seed sets leg enclosure (t.inInnerCluster reflects the join's own
// position at call time).
func (t *cascadesTranslator) ordinalWedgeGate(j *logical.LogicalJoin) wedgeGateDecision {
	d := t.ordinalWedgeGateDecide(j)
	if t.wedgeGate == nil {
		t.wedgeGate = make(map[*logical.LogicalJoin]wedgeGateDecision)
	}
	t.wedgeGate[j] = d
	return d
}

func (t *cascadesTranslator) ordinalWedgeGateDecide(j *logical.LogicalJoin) wedgeGateDecision {
	if _, isUnnest := j.Right.(*logical.LogicalUnnest); isUnnest {
		// Lateral unnest lowers to FlatMap-over-Explode with dotted-prefix
		// bipartition machinery (RFC-142) — name model until the W4 port.
		return wedgeGateDecision{Arity: arityPoison, Reason: "lateral unnest join (RFC-142 machinery, W4)"}
	}
	if len(j.OnExistsSubqueries) > 0 {
		// The seed select carries existential quantifiers; if it merges, they
		// ride along and land the merged select in the ≥3-quantifier
		// partition machinery. Name model (W4 owns the existential seeds).
		return wedgeGateDecision{Arity: arityPoison, Reason: "existential quantifiers on the join select"}
	}
	if j.Kind != logical.JoinInner {
		// Outer-join boxes are structurally binary and opaque on BOTH sides
		// (ChildrenAsSet opacity: SelectMergeRule neither merges them into a
		// parent nor merges children into them), so enclosure and cluster
		// arity are irrelevant — contract ruling #3 (appendNullLeg) flips
		// them in W3 unconditionally.
		return wedgeGateDecision{Gated: true, Arity: 2, Reason: "binary outer-join box (opaque both ways)"}
	}
	if t.inInnerCluster {
		// This inner join is a leg subtree of an enclosing inner-join cluster
		// (or of an existential/unnest flatten): post-flattening it merges
		// into a select of arity ≥ 3. Name model.
		return wedgeGateDecision{Reason: "enclosed in an inner-join cluster (leg of a name-model merge)"}
	}
	a := t.clusterArity(j)
	if a == 2 {
		return wedgeGateDecision{Gated: true, Arity: a, Reason: "maximal inner-join cluster of arity 2"}
	}
	return wedgeGateDecision{Arity: a, Reason: "maximal cluster arity != 2"}
}

// clusterArity computes the post-flattening ForEach arity of the transitive
// inner-join-equivalent cluster rooted at op: how many ForEach quantifiers
// this subtree contributes to the select that ultimately absorbs it under
// maximal SelectMergeRule flattening.
//
// The classification mirrors the rule's ACTUAL mergeability, not the
// contract's shorthand (two deliberate errata, both failing toward the name
// model — flagged in the RFC contract block):
//
//   - inner/cross join: arity(L) + arity(R) — the join select merges into
//     inner-equivalent parents and absorbs mergeable children;
//   - filter/project WITHOUT subqueries: transparent — they lower to selects
//     the rule merges through (rule_select_merge.go TranslationMap path);
//   - filter/project WITH exists/scalar subqueries: POISON, not an opaque
//     leaf. Their selects still merge (ChildrenAsSet is true), and the rule
//     splices ALL child quantifiers — the existential/NullOnEmpty legs ride
//     along, landing the merged select in the ≥3-quantifier partition
//     machinery whose dotted-name classifiers the wedge must never feed;
//   - outer join: opaque leaf of 1 (ChildrenAsSet opacity, both directions);
//   - aggregate / DISTINCT / sort / limit / union: opaque leaf of 1 — they
//     lower to non-SelectExpression boxes (not
//     RelationalExpressionWithPredicates), which the rule cannot merge or
//     merge through;
//   - scan of a cteScope-registered body: arity(body) — translateScan inlines
//     the body per scan site and its selects merge up through the projection
//     wrapper (the flattening-evasion shape flows through here). The body is
//     temporarily removed from scope while walking, exactly like
//     translateScan/legColumns, so a same-named scan inside the body resolves
//     to the real table instead of recursing forever;
//   - scan of a cteExprScope name (recursive-CTE self-reference / temp
//     table): leaf of 1 — a pre-translated opaque reference;
//   - base-table scan: leaf of 1;
//   - non-recursive LogicalCTE (derived table): arity(Main) with the body in
//     scope; recursive: POISON (rare in a leg; conservative);
//   - anything else: POISON.
func (t *cascadesTranslator) clusterArity(op logical.LogicalOperator) int {
	switch o := op.(type) {
	case *logical.LogicalJoin:
		if _, isUnnest := o.Right.(*logical.LogicalUnnest); isUnnest {
			return arityPoison
		}
		if len(o.OnExistsSubqueries) > 0 {
			return arityPoison
		}
		if o.Kind != logical.JoinInner {
			return 1
		}
		l := t.clusterArity(o.Left)
		if l == arityPoison {
			return arityPoison
		}
		r := t.clusterArity(o.Right)
		if r == arityPoison {
			return arityPoison
		}
		return l + r
	case *logical.LogicalFilter:
		if len(o.ExistsSubqueries) > 0 || len(o.ScalarSubqueries) > 0 {
			return arityPoison
		}
		return t.clusterArity(o.Input)
	case *logical.LogicalProject:
		if len(o.ScalarSubqueries) > 0 || len(o.CorrelatedScalarSubqueries) > 0 {
			return arityPoison
		}
		return t.clusterArity(o.Input)
	case *logical.LogicalScan:
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return 1
		}
		if body, ok := t.cteScope[key]; ok {
			delete(t.cteScope, key)
			a := t.clusterArity(body)
			t.cteScope[key] = body
			return a
		}
		return 1
	case *logical.LogicalCTE:
		if o.Recursive {
			return arityPoison
		}
		key := strings.ToUpper(o.Name)
		prev, had := t.cteScope[key]
		// The ColumnAliases projection wrapper translateCTE adds is a plain
		// Project — arity-transparent — so registering the raw body walks the
		// same cluster the translation produces.
		t.cteScope[key] = o.Body
		a := t.clusterArity(o.Main)
		if had {
			t.cteScope[key] = prev
		} else {
			delete(t.cteScope, key)
		}
		return a
	case *logical.LogicalAggregate, *logical.LogicalDistinct, *logical.LogicalSort,
		*logical.LogicalLimit, *logical.LogicalUnion:
		return 1
	default:
		return arityPoison
	}
}
