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
// LIVE since W3b: the gate's per-seed decisions drive the ordinal seed —
// Gated joins get the baked ofOrdinalNumber result value + cross-leg
// predicate baking; everything else stays name-model until Slice 3.

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
		// bipartition machinery (RFC-142) — name model until Slice 3 (review W4-deferral ruling: the ordinal port needs S3 FieldPaths).
		return wedgeGateDecision{Arity: arityPoison, Reason: "lateral unnest join (RFC-142 machinery, S3)"}
	}
	if len(j.OnExistsSubqueries) > 0 {
		// The seed select carries existential quantifiers; if it merges, they
		// ride along and land the merged select in the ≥3-quantifier
		// partition machinery. Name model (S3+ owns the existential seeds).
		return wedgeGateDecision{Arity: arityPoison, Reason: "existential quantifiers on the join select"}
	}
	if strings.EqualFold(sourceAlias(j.Left), sourceAlias(j.Right)) {
		// Both legs bind the SAME correlation (e.g. `FROM p JOIN p`, a CTE
		// referenced twice with no aliases): the ordinal seed's two leg QOVs
		// would be indistinguishable — two runs under one alias, an
		// unclassifiable shape. Fail toward the name model (whose
		// same-namespace merge semantics tolerate it) — the contract's
		// unclassifiable-counts-as->2 direction.
		return wedgeGateDecision{Arity: arityPoison, Reason: "duplicate leg aliases (indistinguishable leg correlations)"}
	}
	if !t.ordinalEligible(j.Left) || !t.ordinalEligible(j.Right) {
		// A leg CONTAINS a name-model join at its own boundary (a 3+-way
		// inner cluster, or an outer box over one): its output rows are the
		// name model's merged rows (dotted keys, no leg concat) — an ordinal
		// seed over it would type the leg wrongly. Mixed nesting stays
		// name-model until Slice 3 flips N-way (RFC §4 coexistence scoping).
		// Caught live by the W3b flip: `(A JOIN B JOIN C) LEFT JOIN D` — the
		// box gated while its left leg stayed name-model, and
		// ordinalLegColumns' mis-scope panic fired exactly as designed.
		return wedgeGateDecision{Arity: arityPoison, Reason: "a leg contains a name-model join (mixed nesting stays name-model until S3)"}
	}
	if j.Kind == logical.JoinFull {
		// FULL OUTER is the only genuinely opaque outer box: it is NEVER
		// rewritten (RewriteOuterJoinRule handles LeftOuter only; FULL stays
		// on the materialized NLJ) and never merged in either direction
		// (ChildrenAsSet false as parent and child). Gate it (contract ruling
		// #3's appendNullLeg — the FULL drain births are wired).
		return wedgeGateDecision{Gated: true, Arity: 2, Reason: "binary FULL-outer box (genuinely opaque both ways)"}
	}
	if j.Kind != logical.JoinInner {
		// LEFT OUTER (and RIGHT, normalized to LEFT) is NOT opaque after
		// REWRITING: RewriteOuterJoinRule dissolves a correlated
		// predicate-carrying LEFT box into an INNER select + null-on-empty
		// quantifier, which then MERGES like any inner select — the
		// dissolved box flattens into enclosing clusters and its own
		// preserved-leg child flattens into it (the RFC-153 joined-preserved
		// machinery). The W2 contract's "outer boxes are opaque both ways"
		// premise held only at translation time; caught live by the W3b flip
		// (SelectMergeRule drift assert + ordinalLegColumns mis-scope panic
		// on the RFC-153 shapes). LEFT OUTER therefore stays NAME-MODEL in
		// the wedge, pending a review re-ruling on the corrected premise
		// (recorded in the RFC).
		return wedgeGateDecision{Arity: arityPoison, Reason: "LEFT-outer box (dissolved by RewriteOuterJoinRule post-translation — not opaque; name model pending re-ruling)"}
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

// ordinalEligible reports whether a gated join could take op as a LEG: op's
// output boundary must not be a JOIN's merged row AT ALL in the S2 wedge.
// Join-free shapes are eligible (scans, aggregates, unions, sorts — their
// outputs are single-namespace rows the leg adapter synthesizes by name).
// ANY join — ordinal or name-model, inner or outer — is categorically
// INELIGIBLE: a nested ordinal box's bare concat ERASES buried aliases (an
// upper dotted reference into the buried leg has no span to resolve
// against), and a name-model join's merged row can't be ordinal-typed;
// nesting is S3's collapsed-FieldPath territory (contract ruling #2: S2 is
// single-accessor only).
// Transparent wrappers peel exactly as clusterArity does; a cteScope-scoped
// scan recurses into the body (the derived-table boundary is transparent to
// SelectMergeRule); opaque boxes over joins (aggregate/union/sort/distinct)
// are eligible — their OUTPUT is the box's own row, the buried join never
// reaches the leg boundary. Unclassifiable shapes: ineligible (name model).
func (t *cascadesTranslator) ordinalEligible(op logical.LogicalOperator) bool {
	switch o := op.(type) {
	case *logical.LogicalJoin:
		// ANY join leg is ineligible in the Slice 2 wedge — including a
		// would-be-gated one. A nested ordinal box's output type is the BARE
		// leg concatenation, which ERASES the buried aliases: an upper dotted
		// reference into the buried leg (`a.id` through `(a JOIN b) FULL JOIN
		// c`) has no span to resolve against — that is S3's collapsed
		// FieldPath territory (contract ruling #2: "S2 needs only
		// single-accessor ordinals"). The name model handles nested legs via
		// verbatim dotted-key propagation; dual emission keeps that path
		// alive (an ordinal box consumed as a NAME-model leg reads through
		// its bare+qualified Datum). Caught live by the W3b flip
		// (FULL-over-join: "A.ID not resolvable, row columns [ID FLAG ID
		// A_ID BX ID A_ID BX_REF]").
		return false
	case *logical.LogicalFilter:
		if len(o.ExistsSubqueries) > 0 || len(o.ScalarSubqueries) > 0 {
			return false
		}
		return t.ordinalEligible(o.Input)
	case *logical.LogicalProject:
		if len(o.ScalarSubqueries) > 0 || len(o.CorrelatedScalarSubqueries) > 0 {
			return false
		}
		return t.ordinalEligible(o.Input)
	case *logical.LogicalScan:
		key := strings.ToUpper(o.Table)
		if _, ok := t.cteExprScope[key]; ok {
			return true // pre-translated opaque reference (temp-table scan)
		}
		if body, ok := t.cteScope[key]; ok {
			delete(t.cteScope, key)
			eligible := t.ordinalEligible(body)
			t.cteScope[key] = body
			return eligible
		}
		return true
	case *logical.LogicalCTE:
		// A derived-table join SOURCE is built as a LogicalCTE node DIRECTLY
		// in the leg position (logical_builder: NewCTE(alias, body,
		// Scan(alias))) — without this arm it fell to the default and a
		// FULL box over `(SELECT ... c JOIN t ...) AS d` wrongly gated with a
		// buried join at its leg boundary (@claude PR-447 catch: the walk
		// must mirror clusterArity's CTE transparency exactly, per this
		// file's own header). Recurse through the registered body into Main,
		// identically to clusterArity.
		if o.Recursive {
			return false
		}
		key := strings.ToUpper(o.Name)
		prev, had := t.cteScope[key]
		t.cteScope[key] = o.Body
		eligible := t.ordinalEligible(o.Main)
		if had {
			t.cteScope[key] = prev
		} else {
			delete(t.cteScope, key)
		}
		return eligible
	default:
		// Non-join leaves and opaque boxes (aggregate, union, sort, limit,
		// distinct, values, …): the leg boundary sees the box's own output
		// row, never a buried join's merged row. (LogicalCTE has its OWN arm
		// above — derived-table sources sit directly in leg position.)
		return true
	}
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
		if o.Kind == logical.JoinFull {
			// FULL OUTER: genuinely opaque (never rewritten, never merged).
			return 1
		}
		if o.Kind != logical.JoinInner {
			// LEFT/RIGHT OUTER: RewriteOuterJoinRule dissolves the box into
			// an INNER + null-on-empty select during REWRITING, whose
			// preserved side flattens into the enclosing cluster with a
			// null-on-empty rider — post-flattening arity is not computable
			// at translation. POISON: any cluster containing a LEFT box
			// stays name-model (W3b premise correction; see
			// ordinalWedgeGateDecide).
			return arityPoison
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
