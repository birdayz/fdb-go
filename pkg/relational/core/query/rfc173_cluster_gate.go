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
// actually implements (rule_select_merge.go). The rule's original target-loop
// drift assert was retired at the S3 fulcrum (positional merges are legal
// now); drift between this walk and the rule is held by the contract pins
// (ClusterArity_Shapes / WalkArmParity) and the seed-side loud asserts — a
// decline is forbidden (it would change plan shapes, contract ruling #1).
//
// LIVE since W3b: the gate's per-seed decisions drive the ordinal seed —
// Gated joins get the baked ofOrdinalNumber result value + cross-leg
// predicate baking; the S3 fulcrum widened the wedge to N-way inner
// clusters (arity >= 2); W4-left gates single-source LEFT/RIGHT boxes and
// routes EXISTS-over-gated-joins through the ordinal existential rebase. A
// join over a RECURSIVE-CTE REFERENCE (a cteExprScope temp-table scan) has
// gated since the fulcrum — the reference is an ordinal-eligible arity-1
// leaf typed by cteColumnsScope (W4-left commit-3 pin); only the recursive
// DEFINITION node in leg position stays poison, and that shape is
// production-unreachable (derived-table WITH is 42F01). The name model
// survives only for the PINNED residuals (joined-preserved LEFT clusters,
// unnest declines, ungated correlated-scalar outers, dup-alias clusters).

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

// existsOuterGatesFresh is the RFC-173 item-2 commit-4 enclosure-lift
// predicate: it reports whether a WHERE-EXISTS filter's OUTER input would gate
// ordinal AS A FRESH CLUSTER (design ruling condition 4 — the decision routes
// through the ONE gate authority, no parallel re-derivation). The generic
// filter arm used to force enclosure under EXISTS unconditionally, so a gated
// LEFT/RIGHT box stayed name-model and the W4-left ordinal existential rebase
// was dead; when this returns true the caller LEAVES enclosure off so the box
// gates ordinal and implementExistentialSelect's below-FOD ordinal rebase
// fires.
//
// Only a DIRECT LogicalJoin input qualifies: a buried join (a derived table /
// aggregate over a join) keeps the name-model enclosure — its OUTPUT row is
// the box's own opaque row that the outer ForEach merges as ONE leg, never a
// leg concat. LEFT/RIGHT only: the single-source LEFT/RIGHT box is the
// W4-left gated class the ordinal below-FOD rebase handles (it dissolves to
// INNER + null-on-empty, the shape the machinery implements). FULL-outer
// under EXISTS stays name-model — its drain births compose with the
// existential semi-join in ways the LEFT/RIGHT class does not exercise; a
// dedicated FULL slice widens it. The INNER case never reaches here (it routes
// to translateJoinWithExists upstream). The probe runs Decide with enclosure
// forced FALSE (the fresh-cluster position a box roots) — side-effect-free,
// mirroring ordinalEligible.
func (t *cascadesTranslator) existsOuterGatesFresh(input logical.LogicalOperator) bool {
	j, ok := input.(*logical.LogicalJoin)
	if !ok {
		return false
	}
	if j.Kind != logical.JoinLeft && j.Kind != logical.JoinRight {
		return false
	}
	return t.gatesAsFreshCluster(j)
}

// gatesAsFreshCluster runs the ordinal wedge gate on j with enclosure forced
// FALSE — the fresh-cluster position a box or join leg roots (it sits at an
// outer-box or leg boundary) — restoring the ambient bit. Side-effect-free,
// mirroring ordinalEligible. This is the shared probe behind
// existsOuterGatesFresh, boxGatesFresh, and ordinalEligible's LogicalJoin arm;
// each caller keeps its OWN kind-guard and calls this for the gate decision.
// ONE gate authority (ordinalWedgeGateDecide), ONE probe mechanism.
func (t *cascadesTranslator) gatesAsFreshCluster(j *logical.LogicalJoin) bool {
	prev := t.inInnerCluster
	t.inInnerCluster = false
	d := t.ordinalWedgeGateDecide(j)
	t.inInnerCluster = prev
	return d.Gated
}

// boxGatesFresh is the RFC-173 S4 Step-B enclosure-lift predicate — the SAME
// one gate authority as existsOuterGatesFresh, WIDENED to include FULL. It
// reports whether an OUTER-join box would gate ordinal AS A FRESH CLUSTER
// (enclosure forced FALSE), i.e. whether the box may BIRTH a positional row
// under a lateral unnest instead of a name-model Datum. Unlike
// existsOuterGatesFresh (LEFT/RIGHT only — its below-FOD scalar rebase is the
// 1+1 dissolve-to-INNER shape), this admits FULL: a box UNDER an unnest births
// its whole leg-concat positionally (ordinalJoinBirth NULL-fills the FULL
// drain; adaptLegPositional flows it through), and the per-leg-window rebase
// (channels 1+2) resolves each leg's dup-named columns by its [Start,Width)
// window rather than flat first-match — so a FULL box's two same-named legs
// disambiguate end-to-end.
//
// JoinInner is EXCLUDED: a multi-source INNER cluster (`FROM A, B, A.arr AS x`)
// gates as a flat N-way gather, and admitting it here would gate the cluster
// ordinal while its seed still declines the multi-source outer — wrong rows
// (R1). The probe runs Decide with enclosure forced FALSE (the fresh-cluster
// position a box roots), side-effect-free, mirroring existsOuterGatesFresh.
//
// The TWO axes this predicate gates — the box-outer enclosure at the unnest's
// translateRef and unnestExistsSeedSafe's multi-alias admission — MUST flip
// TOGETHER through this ONE predicate: a positional seed over a name-model box
// row hits the adaptLegPositional zero-match tripwire, and a name-model builder
// over a positional box row mis-types the leg. Either half alone is broken.
func (t *cascadesTranslator) boxGatesFresh(input logical.LogicalOperator) bool {
	j, ok := input.(*logical.LogicalJoin)
	if !ok {
		return false
	}
	if j.Kind == logical.JoinInner {
		return false
	}
	return t.gatesAsFreshCluster(j)
}

// forceOrdinalSpike is the RFC-173 S4 B1 CERTIFICATE oracle (test-only). When set,
// ordinalWedgeGateDecide skips the two PURELY-CIRCULAR enclosure declines — the outer
// box enclosed in a name-model parent, and the inner join enclosed in a name-model
// cluster (both `t.inInnerCluster` guards). Each is a FAITHFUL SYMPTOM of a name-model
// parent (a child gates iff its parent gates), so they lift together, atomically, when
// the name model is gone. The `ordinalEligible` (`:152`) decline is NOT spike-guarded:
// it is NOT purely circular — for a JOIN leg it self-corrects via recursion (once the
// enclosure declines lift, a nested join gates → its parent sees it as eligible), but
// for a GENUINE name-model leg (an UNNEST / aggregate / mixed-derived body) it must
// KEEP declining until that leg itself ordinalizes (W5). So the spike models the EXACT
// cap-gate state — flip the enclosure declines, keep `:152` — not B1's earlier
// over-aggressive all-three skip (which would gate genuine unnest legs → wrong rows for
// shapes the corpus need not cover). The corpus-level differential runs twice (spike
// OFF vs ON) and asserts identical rows: green PROVES the enclosure flip preserves rows.
// NOT a production narrowing — flipped only by the certificate harness at a phase
// barrier, exactly like the executor's DisablePositionalEmission name oracle.
var forceOrdinalSpike bool

// SetForceOrdinalSpike flips the B1 certificate oracle. Test-only; the caller must
// guarantee no translation is in flight (process-global, like SetNameModelOracle).
func SetForceOrdinalSpike(v bool) { forceOrdinalSpike = v }

func (t *cascadesTranslator) ordinalWedgeGateDecide(j *logical.LogicalJoin) wedgeGateDecision {
	if _, isUnnest := j.Right.(*logical.LogicalUnnest); isUnnest {
		// Lateral unnest lowers to FlatMap-over-Explode with dotted-prefix
		// bipartition machinery (RFC-142): name model until the W5 unnest
		// rewrite (review W4-deferral ruling).
		return wedgeGateDecision{Arity: arityPoison, Reason: "lateral unnest join (RFC-142 machinery, S3)"}
	}
	if len(j.OnExistsSubqueries) > 0 {
		// The seed select carries existential quantifiers; if it merges, they
		// ride along and land the merged select in the ≥3-quantifier
		// partition machinery. Name model (S3+ owns the existential seeds).
		return wedgeGateDecision{Arity: arityPoison, Reason: "existential quantifiers on the join select"}
	}
	// PAIRWISE dup check over the kind-aware leg list, keyed by the BINDING
	// correlation (RFC-173 QP-REF-BIND item 1): duplicate SQL aliases with
	// DISTINCT parser-minted bindings are admissible — the seed's QOVs, bake
	// maps and windows key on the binding, so the legs stay distinguishable
	// end-to-end and per-attribute resolution owns any reference ambiguity
	// (Java's model: quantifier ids are never SQL names). Two legs binding
	// the SAME correlation remain unclassifiable — an unminted duplicate can
	// only reach here through a path the mint authority does not cover, and
	// failing toward the name model keeps that class correct-or-loud (the
	// contract's unclassifiable-counts-as->2 direction).
	seenBindings := make(map[string]struct{})
	for _, leg := range t.legsOfGatedJoin(j) {
		key := strings.ToUpper(leg.binding)
		if _, dup := seenBindings[key]; dup {
			return wedgeGateDecision{Arity: arityPoison, Reason: "duplicate leg bindings (indistinguishable leg correlations)"}
		}
		seenBindings[key] = struct{}{}
	}
	if !t.ordinalEligible(j.Left) || !t.ordinalEligible(j.Right) {
		// A leg CONTAINS a name-model join at its own boundary (an
		// aggregate/CTE/derived body the gate keeps anchored): its output
		// rows are the name model's merged rows (dotted keys, no leg concat)
		// — an ordinal seed over it would type the leg wrongly. The
		// remaining name-model residency retires at S4, the atomic
		// demolition (RFC §4 coexistence scoping; inner clusters flipped at
		// the S3 fulcrum, outer boxes at item 3). Caught live by the W3b
		// flip: `(A JOIN B JOIN C) LEFT JOIN D` — the box gated while its
		// left leg stayed name-model, and ordinalLegColumns' mis-scope panic
		// fired exactly as designed.
		return wedgeGateDecision{Arity: arityPoison, Reason: "a leg contains a name-model join (name-model residency retires at S4)"}
	}
	if j.Kind != logical.JoinInner && t.inInnerCluster && !forceOrdinalSpike {
		// An OUTER box that is a LEG of an enclosing name-model join (or of
		// an existential/unnest flatten) stays name-model: the parent's
		// merge binds leg rows by NAME, so a gated box's POSITIONAL row
		// under it reads the wrong source (`d LEFT JOIN e ON … JOIN c ON …`
		// returned d.id as e.id — the mixed-nesting runtime pins) or breaks
		// the partition rule's anchored re-enumeration (the RIGHT variant
		// panicked). The INNER arm has carried this exact enclosure guard
		// since Slice 2; the outer arms shipped without it — plans looked
		// clean while rows were wrong. The residual name-model parents here
		// (existential/unnest flattens, aggregate boxes) retire at S4, the
		// atomic demolition — this guard dies with them.
		return wedgeGateDecision{Arity: arityPoison, Reason: "outer box enclosed in a name-model parent (name-model residency retires at S4)"}
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
		// LEFT OUTER (and RIGHT, normalized to LEFT at execution) GATES at
		// the box root (RFC-173 W4-left granted single-source legs; item 3
		// commit 1 widened to CLUSTERED legs): RewriteOuterJoinRule
		// dissolves the box into an INNER select + null-on-empty quantifier
		// — the EXACT shape the ordinal machinery implements — and Java
		// builds the ordinal RV at translation (wrapOperandsForOuterJoin)
		// with the rule REUSING it unchanged. The seed marks the
		// null-supplying leg's QOV record-level nullable (legsOfGatedJoin,
		// design ruling I3). The former single-source condition
		// (clusterArity==1 per leg — the Q3 buried-names narrowing) retired
		// with item 3: a flattened preserved/null-supplying cluster's buried
		// sources are nameable per-leg since items 1+2 (binding-keyed
		// windows + positional binders), so the seed types them exactly as
		// Java does (a buried source is just another quantifier's window).
		// The leg-eligibility check above (ordinalEligible) remains the
		// admission for what a leg may CONTAIN.
		return wedgeGateDecision{Gated: true, Arity: 2, Reason: "binary LEFT/RIGHT-outer box (ordinal seed at translation; clustered legs per item 3)"}
	}
	if t.inInnerCluster && !forceOrdinalSpike {
		// This inner join is a leg subtree of an enclosing inner-join cluster
		// (or of an existential/unnest flatten): post-flattening it merges
		// into a select of arity ≥ 3. Name model.
		return wedgeGateDecision{Reason: "enclosed in an inner-join cluster (leg of a name-model merge)"}
	}
	a := t.clusterArity(j)
	if a >= 2 {
		// S3 fulcrum: the exactly-2 wedge lifted — every maximal inner-join
		// cluster seeds the flat N-leg ordinal RC (Java flattens inner joins
		// at translation, QueryVisitor.java:429-434; the partition rule's
		// positional merge case re-collapses subsets during exploration).
		return wedgeGateDecision{Gated: true, Arity: a, Reason: "maximal inner-join cluster (N-way flat seed, S3)"}
	}
	return wedgeGateDecision{Arity: a, Reason: "cluster arity below 2 (single quantifier or poison)"}
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
		// S3 fulcrum ("join legs eligible iff the leg itself gates"): a
		// GATED join leg's output is its flat ordinal concat — a typed
		// positional row that multi-accessor FieldPaths resolve through (the
		// S2 single-accessor restriction that made ANY join leg ineligible
		// died with W1). A NAME-MODEL join leg (dissolved-LEFT clusters,
		// unnest, mixed derived nesting) stays ineligible: its merged row
		// cannot be ordinal-typed. The probe runs Decide with enclosure
		// forced FALSE — a join leg roots a fresh cluster (it sits at an
		// outer-box or leg boundary) — and Decide is side-effect-free.
		//
		// Item-3 S3: a LEFT/RIGHT box is eligible AS A LEG exactly like any
		// gated join (the W4-left leg-ineligibility retired). The former
		// drift hazard — a parent typing the box as ONE leg against the
		// dissolved select's post-flattening splice — is closed by
		// amendment A: clusterArity counts the box as preserved + 1 (the
		// rules' actual mergeability), so the parent's arity accounting and
		// the seed layout agree; the W3b drift assert and the mixed-nesting
		// matrices stay the tripwires.
		return t.gatesAsFreshCluster(o)
	case *logical.LogicalFilter:
		// RFC-173 commit 5b: rider subqueries are transparent to eligibility,
		// mirroring clusterArity. A WHERE-EXISTS leg's output boundary is the
		// existential FlatMap's IDENTITY RV — the source row itself, a
		// single-namespace row the leg adapter types like any scan — and an
		// uncorrelated scalar rider is a root-context binding. Neither turns
		// the leg's output into a merged row.
		return t.ordinalEligible(o.Input)
	case *logical.LogicalProject:
		if len(o.CorrelatedScalarSubqueries) > 0 {
			return false // per-row scalar — the W4b clusterPullUp rework, booked
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
//   - filter/project WITH rider subqueries (RFC-173 commit 5b): TRANSPARENT
//     for EXISTS riders and UNCORRELATED scalar riders. Their selects merge
//     (ChildrenAsSet is true) and the rule splices ALL child quantifiers —
//     the existential legs ride the merged select, which the 2+1 flatten's
//     ordinal seed threads (commit 3); an uncorrelated scalar is a
//     pre-evaluated root-context binding (shape-agnostic, the 5c ruling).
//     Neither adds a ForEach quantifier. A CORRELATED projection scalar
//     still POISONS: per-outer-row evaluation needs the W4b clusterPullUp
//     rework (booked);
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
			// LEFT/RIGHT OUTER (item-3 amendment A): RewriteOuterJoinRule
			// dissolves the box into an INNER + null-on-empty select, and
			// the dissolved select MERGES into inner-equivalent parents —
			// the PRESERVED side's legs splice in and the null-on-empty
			// quantifier splices as EXACTLY ONE leg whose child never
			// merges (verified in both engines: Java's SelectMergeRule
			// matches via forEachQuantifierWithoutDefaultOnEmptyOverRef;
			// Go's rule skips IsNullOnEmpty quantifiers identically). So
			// post-flattening arity IS computable: preserved + 1. Poison
			// propagates from the preserved side. (The former blanket
			// poison — "not computable at translation" — was shorthand for
			// "not seedable pre-items-1+2", retired with S3.)
			preserved := o.Left
			if o.Kind == logical.JoinRight {
				preserved = o.Right
			}
			p := t.clusterArity(preserved)
			if p == arityPoison {
				return arityPoison
			}
			return p + 1
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
		// RFC-173 commit 5b: rider subqueries are TRANSPARENT to arity. An
		// EXISTS rider's existential quantifier rides the post-flattening
		// merge (the 2+1 flatten's ordinal seed threads existential
		// quantifiers on the seed select), and an uncorrelated scalar rider
		// (all a filter can carry — correlated ones never land on
		// LogicalFilter) is a pre-evaluated ROOT-context binding,
		// shape-agnostic. Neither adds a ForEach quantifier, so the filter
		// contributes its input's arity — the poison here made every cluster
		// with a subquery-bearing leg name-model for no structural reason.
		// TODO(rfc-173): two PRE-EXISTING reach limits are newly VISIBLE on
		// gated paths (loud 0AF00 on master too, proven at the 5b review): a
		// rider filter OVER A JOIN body consumed as a leg, and multiple
		// existential riders on one filter — both bail in the single-
		// existential 2+1 implementation, never wrong rows.
		return t.clusterArity(o.Input)
	case *logical.LogicalProject:
		if len(o.CorrelatedScalarSubqueries) > 0 {
			// A CORRELATED scalar needs per-outer-row evaluation the flat
			// seed cannot express (the W4b clusterPullUp rework, booked) —
			// still poison.
			return arityPoison
		}
		// Uncorrelated projection scalars: root-context bindings — transparent
		// (the same 5c ruling that flipped class-K).
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
