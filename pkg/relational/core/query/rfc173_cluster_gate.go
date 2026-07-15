package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
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
//
// A box LEG that EXPOSES A BURIED OUTER-BOX join is EXCLUDED (stays name-model):
// its nested outer box is an unverified shape whose buried-leaf resolution under
// a FULL box is not yet exercised. A buried INNER-CLUSTER leg (`(A JOIN B) FULL
// OUTER C`, the c5b target) is ADMITTED: the seed concats the clustered leg's
// buried columns and the executor hoist resolves a buried EXISTS ref by its
// [Start,Width) window — verified e2e (a buried `FOD.K` inside EXISTS resolves).
// A regular WHERE conjunct on a buried leg routes through the
// unnestBoxLegConjunct verdict: Bakeable rides the gathered path (baked over
// the seed's recorded legTypes), Unbakeable declines to name-model; the
// BINARY seed declines for any non-None verdict. Only a box whose legs are SIMPLE
// (scan / opaque box) or INNER-cluster joins may birth positional here; a nested
// LEFT/RIGHT/FULL box leg (`A FULL B FULL C` — clusterArity(FULL)==1 makes an
// arity proxy blind to it) stays name-model until its own slice verifies it.
func (t *cascadesTranslator) boxGatesFresh(input logical.LogicalOperator) bool {
	j, ok := input.(*logical.LogicalJoin)
	if !ok {
		return false
	}
	if j.Kind == logical.JoinInner {
		return false
	}
	if legExposesBuriedOuterBox(j.Left) || legExposesBuriedOuterBox(j.Right) {
		return false
	}
	return t.gatesAsFreshCluster(j)
}

// legExposesBuriedOuterBox reports whether op is a box leg the c5b positional
// seed must NOT birth. It peels the row-shape-preserving wrappers the gate
// treats as TRANSPARENT (LogicalFilter, and a LogicalProject without
// correlated-scalar subqueries, mirroring ordinalEligible/clusterArity), then:
//   - a DIRECT INNER-cluster join → FALSE (admitted): the positional seed concats
//     its buried leaves (ordinalLegType records the [Start,Width) bounds for a
//     direct LogicalJoin) and the executor resolves a buried ref by window
//     (verified — both buried leaves disambiguate).
//   - a DIRECT OUTER box (LEFT/RIGHT/FULL) → TRUE (excluded): its own slice.
//   - a WRAPPED join (reached by peeling) → TRUE (excluded): ordinalLegType
//     records buried bounds ONLY for a DIRECT LogicalJoin leg, so a wrapped inner
//     cluster would birth positional WITHOUT its buried windows (a buried ref
//     unrebased → malformed). Also SQL-unreachable (a JOIN-bodied derived table
//     is a LogicalCTE / loud-rejected, not a transparent wrapper) — defensive.
//   - a scan / OPAQUE box (aggregate/union/sort/CTE), wrapped or not → FALSE:
//     its output is its own flat single-namespace row, safely windowed.
//
// The peel is INTENTIONALLY SHALLOW: a nested OUTER box buried INSIDE an admitted
// INNER cluster (`((A LEFT B) JOIN C) FULL OUTER D`) ordinalizes CORRECTLY (the
// machinery recurses — legsOfGatedJoin marks the null-supplying leg, the executor
// null-supplies through the positional birth; verified), so this must NOT be
// tightened into a recursive exclusion. Only a WRAPPED join is excluded, because
// the wrapper (not the join's depth) is what strips the buried-leg metadata.
func legExposesBuriedOuterBox(op logical.LogicalOperator) bool {
	peeled := false
	for {
		switch o := op.(type) {
		case *logical.LogicalJoin:
			if peeled {
				return true // a WRAPPED join at the top — no buried windows recorded
			}
			if o.Kind != logical.JoinInner {
				return true // a direct OUTER box — its own slice
			}
			// A direct INNER cluster is admitted, BUT a WRAPPED join buried
			// ANYWHERE inside it is excluded: buriedLegBounds records windows only
			// for a DIRECT LogicalJoin leg, so `(Filter(A JOIN B) JOIN C)` would
			// birth positional without windows for A/B → a buried ref malforms. A
			// BARE nested join (any kind, any depth) IS windowed (buriedLegBounds
			// recurses through direct joins) and stays admitted — a nested OUTER
			// box inside an INNER cluster ordinalizes correctly (verified).
			return hasWrappedBuriedJoin(o.Left) || hasWrappedBuriedJoin(o.Right)
		case *logical.LogicalFilter:
			op = o.Input
			peeled = true
		case *logical.LogicalProject:
			if len(o.CorrelatedScalarSubqueries) > 0 {
				return false
			}
			op = o.Input
			peeled = true
		default:
			return false
		}
	}
}

// hasWrappedBuriedJoin reports whether op contains a join reached by peeling a
// transparent wrapper (Filter / non-scalar Project) — at op itself or buried
// inside a bare nested join. buriedLegBounds records positional windows only for
// a DIRECT LogicalJoin leg, so a WRAPPED join anywhere in a box leg's subtree
// would birth positional without its buried leaves' windows (malformed). A bare
// join is recursed THROUGH (its own leaves ARE windowed) looking for a wrapped
// join deeper. A CORRELATED-scalar Project stops the walk (ineligible upstream).
func hasWrappedBuriedJoin(op logical.LogicalOperator) bool {
	var walk func(op logical.LogicalOperator, wrapped bool) bool
	walk = func(op logical.LogicalOperator, wrapped bool) bool {
		switch o := op.(type) {
		case *logical.LogicalJoin:
			if wrapped {
				return true
			}
			return walk(o.Left, false) || walk(o.Right, false)
		case *logical.LogicalFilter:
			return walk(o.Input, true)
		case *logical.LogicalProject:
			if len(o.CorrelatedScalarSubqueries) > 0 {
				return false
			}
			return walk(o.Input, true)
		default:
			return false
		}
	}
	return walk(op, false)
}

// scanFamilyLegCteAware reports whether op is a single base-table SCAN
// (through filters), resolving a CTE-name scan THROUGH cteScope to its body.
// This is the cteScope-AWARE root-cause counterpart of the plain (syntactic,
// cteScope-blind) isScanFamilyLeg: a CTE whose body is a JOIN or an OPAQUE BOX
// (aggregate/union/sort/distinct/limit) is NOT scan-family — it flows a merged
// or multi-source row the 2-way EXISTS-in-ON ordinal seed is unverified over.
// A cteExprScope name (recursive-CTE self-reference / temp-table scan) is a
// pre-translated opaque reference → not scan-family (conservative). The body
// is walked under inCTEDefiningScope — the SAME cteShadowStack mechanism
// translateScan/legColumns use — so a SAME-NAMED scan inside the body resolves
// to the OUTER shadowed binding (not the base table), keeping classification
// in lockstep with translation (a plain delete-during-walk would diverge:
// `WITH c AS (SELECT * FROM c)` shadowing an outer join/box would misresolve
// the inner c to the base table and over-gate).
func (t *cascadesTranslator) scanFamilyLegCteAware(op logical.LogicalOperator) bool {
	for {
		switch n := op.(type) {
		case *logical.LogicalScan:
			key := strings.ToUpper(n.Table)
			if _, ok := t.cteExprScope[key]; ok {
				return false
			}
			if body, ok := t.cteScope[key]; ok {
				var r bool
				t.inCTEDefiningScope(key, body, func() {
					r = t.scanFamilyLegCteAware(body)
				})
				return r
			}
			return true
		case *logical.LogicalFilter:
			op = n.Input
		default:
			return false
		}
	}
}

func (t *cascadesTranslator) ordinalWedgeGateDecide(j *logical.LogicalJoin) wedgeGateDecision {
	if _, isUnnest := j.Right.(*logical.LogicalUnnest); isUnnest {
		// Lateral unnest lowers to FlatMap-over-Explode with dotted-prefix
		// bipartition machinery (RFC-142): name model until the W5 unnest
		// rewrite (review W4-deferral ruling).
		return wedgeGateDecision{Arity: arityPoison, Reason: "lateral unnest join (RFC-142 machinery, S3)"}
	}
	if len(j.OnExistsSubqueries) > 0 {
		// RFC-173 S4: a BARE 2-way NON-ENCLOSED INNER EXISTS-in-ON gates
		// ordinal. Both legs must be a single SCAN SOURCE (through filters),
		// tested by scanFamilyLegCteAware — the SINGLE root-cause predicate that
		// resolves a CTE-name scan THROUGH cteScope and checks the BODY, so it
		// excludes at once: a FULL/aggregate/union/sort box (not a scan → a
		// buried join = the index-matching wall, a null-drain, or an opaque
		// merged row the 2-leg seed is unverified over), an N-way join leg (not
		// a scan), a CTE-backed join AND a CTE-backed opaque box (the scan
		// resolves to a non-scan body). A scan-scan 2-way seeds
		// [ForEach, ForEach, Existential], which implementJoinWithExistential's
		// 2-leg arm plans ordinal AND index-neutral (the existential already
		// drops even a 2-way join to a plain NLJ on the name model, so no index
		// is lost — EXPLAIN-verified); the translateJoin binary arm builds the
		// ordinal seed and attaches the existentials unchanged. The poison STAYS
		// for an ENCLOSED or N-WAY EXISTS-in-ON (the existential would ride into
		// the ≥3-quantifier partition machinery, or hit the buried-inner-box
		// index-matching wall — the booked ordinal-fold-over-index-matched-box
		// prerequisite) and for any non-scan leg.
		// DUPLICATE ALIAS: two legs sharing a SQL alias get a parser-minted
		// Q$DUPn binding on the later leg (mintedBindingLeg finds it). The gated
		// arm here RETURNS EARLY, before the pairwise dup-binding poison (:294),
		// so without this guard a dup-alias EXISTS-in-ON would gate and the
		// 2-leg fold would LOSE the minted binding → serve NULLs (silent wrong;
		// the name model loud-declines it). Keep dup-alias poisoned here so it
		// falls to that loud decline (base behaviour).
		if j.Kind == logical.JoinInner && !t.inInnerCluster &&
			t.scanFamilyLegCteAware(j.Left) && t.scanFamilyLegCteAware(j.Right) &&
			mintedBindingLeg(j.Left, j.Right) == "" {
			return wedgeGateDecision{Gated: true, Arity: 2, Reason: "bare 2-way non-enclosed EXISTS-in-ON (scan legs; 2-leg ordinal fold)"}
		}
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
	if j.Kind != logical.JoinInner && t.inInnerCluster {
		// An OUTER box (FULL/LEFT/RIGHT) that is a LEG of an enclosing name-model
		// parent (a name-model unnest residual / existential flatten / aggregate
		// box) stays name-model: the parent's merge and its chained/box-leg
		// conjunct rebase bind the box's columns BY NAME, so ordinalizing the box
		// here strands the name-model residual over a positional/ordinal row (it
		// reads the box's qualified LEG.COL keys at ordinal -1 → malformed plan).
		// The INNER-cluster enclosure decline retired at the S4 cap (the flat
		// N-leg seed the enclosed inner cluster now composes ordinalizes-or-loud);
		// the OUTER-box residency retires with item 3 (buildUnnestResultValue
		// deletion — the FULL/nested-box residual callers must first ordinalize or
		// go loud, verified case-by-case). Keeping this decline holds those shapes
		// on the correct name-model rows until then, rather than the strand.
		return wedgeGateDecision{Arity: arityPoison, Reason: "outer box enclosed in a name-model parent (outer-box name-model residency retires at item 3)"}
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
	if t.inInnerCluster {
		// This inner join is a leg subtree of an enclosing name-model composition
		// (a name-model unnest residual over a multi-source outer, or an existential
		// flatten): post-flattening it merges into a select of arity ≥ 3. It stays
		// name-model until the ENCLOSED inner-cluster-under-unnest residual is
		// ordinalized (item 3 / buildUnnestResultValue deletion). Collapsing this
		// decline today ordinalizes the enclosed seed → the anchored re-enumeration
		// mismatch (PartitionSelectRule) for the un-enclosed unbakeable case, and,
		// worse, 0 rows for the enclosed E-1b CTE-leg (the seedWindowed anchored-seed
		// arm — the original enclosed-CTE P1). Keep it name-model until that arm
		// ordinalizes with correct rows.
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
			// A bare-projected unnest-cluster derived boundary is an OPAQUE ordinal
			// leg (its projected row is a clean single namespace) even though its
			// internal unnest would poison the plain recursion below. So is the
			// single-link STAR body (its boundary is the unnest seed's own
			// positional row — derivedBodyStarOrdinalLeg).
			if t.derivedBodyOpaqueOrdinalLeg(body) {
				return true
			}
			if _, star := t.derivedBodyStarOrdinalLeg(body); star {
				return true
			}
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

// derivedBodyOpaqueOrdinalLeg reports whether a CTE/derived-table body presents a
// CLEAN single-namespace ordinal row at its derived boundary — so a parent may
// gate over the derived source as an OPAQUE arity-1 leg even though the body's
// internal cluster is an unnest (which poisons clusterArity/ordinalEligible as a
// direct leg). It admits ONLY the BARE-PROJECTED unnest-cluster body:
//
//	WITH D AS (SELECT x [, y …] FROM <sources>, <lateral unnest> [WHERE …]) …
//
// The projection to BARE (unqualified) names is load-bearing: the parent's
// `D.col` read resolves POSITIONALLY to the renamed bare column, whereas a
// `SELECT *` body keeps qualified multi-source names (`LA.K`) that the parent's
// `D.K` read cannot map (ordinal -1 strand). A correlated-scalar projection stays
// name-model (per-row scalar needs the clusterPullUp rework). This makes the
// enclosed-CTE E-1b leg (`WITH D AS (SELECT X FROM A,B,A.arr AS X WHERE …) SELECT
// D.X FROM D, EEV`) ordinalize coherently: the PARENT gates → the body translates
// FRESH (prevEnclosure false) → its unnest cluster gathers, and the parent reads
// D's projected row opaquely. No anchored re-enumeration (the name-model residual)
// is involved — the opposite of forcing the enclosed inner cluster to gate under a
// still-name-model parent, which strands the seed at 0 rows.
func (t *cascadesTranslator) derivedBodyOpaqueOrdinalLeg(body logical.LogicalOperator) bool {
	proj, ok := body.(*logical.LogicalProject)
	if !ok || len(proj.Projections) == 0 || len(proj.CorrelatedScalarSubqueries) > 0 {
		return false
	}
	for i := range proj.Projections {
		name := proj.Projections[i]
		if i < len(proj.Aliases) && proj.Aliases[i] != "" {
			name = proj.Aliases[i]
		}
		if name == "*" || strings.Contains(name, ".") {
			return false
		}
	}
	// The projection body must reduce (through row-preserving filters) to a
	// lateral-unnest cluster — the shape that poisons clusterArity as a direct leg
	// but ordinalizes fresh via the gather path. A non-unnest body is either
	// already computable (a plain flattening join — not poison) or a shape whose
	// opaque-leg ordinalization is unverified; keep it name-model.
	op := proj.Input
	for {
		switch o := op.(type) {
		case *logical.LogicalFilter:
			op = o.Input
		case *logical.LogicalJoin:
			_, isUnnest := o.Right.(*logical.LogicalUnnest)
			return isUnnest
		default:
			return false
		}
	}
}

// derivedBodyStarOrdinalLeg is derivedBodyOpaqueOrdinalLeg's STAR sibling: it
// reports whether a PROJECTION-LESS CTE/derived-table body (`WITH S AS (SELECT
// * FROM t, t.arr AS x [AT o] [WHERE …]) …`) is a SINGLE-LINK, SINGLE-SOURCE
// lateral unnest whose FRESH translation takes the ordinal seed — so the
// derived boundary flows the seed's POSITIONAL row under the seed's own labels
// (the outer scan's bare columns then the AS/AT aliases), and a parent may gate
// over the reference as an OPAQUE arity-1 leg. Returns the unnest join so the
// leg-schema authority (ordinalLegColumns) derives the boundary labels from the
// SAME node the seed builds over (its unnest-join arm — the layout-identity
// invariant).
//
// The admission mirrors the fresh body translation's own ordinal-seed gate
// EXACTLY (translateUnnestJoin: single-source arity, exists-safety via the
// single-alias bypass, segment classification) — an over-admission would type
// the leg positionally while the body falls back to the name-model anchored RC,
// whose interleaved bare+qualified field layout silently misaligns every
// positional read. Conditions:
//   - plain filters only above the join (a subquery-carrying WHERE routes the
//     body through the EXISTS/scalar dispatches — different seeds);
//   - a bare INNER comma unnest join (no ON, no ON-EXISTS) with an AS or AT
//     binding and a segment path (`t.arr`, `t.rec.arr`);
//   - a SINGLE-SOURCE, single-alias outer (clusterArity 1 — excludes the
//     merge-opaque FULL box, which is also arity 1 but multi-alias) bound to a
//     REAL table whose segment path names an ARRAY (the classification
//     gauntlet's own resolvers: a non-resolving segment 0 is a schema-qualified
//     cross join, not an unnest; a CTE/derived-bound source classifies through
//     its body — residual class 3);
//   - NOT under an existential (the under-EXISTS seed gate adds scope-collision
//     arms this admission does not replicate);
//   - SHADOW-DEDUPED, then UNIQUE, boundary labels: a link's AS/AT alias
//     SHADOWS a same-named column of the spine's BOTTOM source (`t.scarr AS
//     SUB` over a table with a scalar SUB) — the RFC-142 collision rule the
//     name model applied everywhere (buildUnnestResultValue drops the outer's
//     bare field; the star expansion of the direct query exposes ONE such
//     column, the element). The boundary labels drop the shadowed bottom
//     occurrence, so the normalization's bare projection resolves the
//     surviving label through the SAME shadow rule — element/ordinal wins,
//     identical to the WITH form's pinned rows. (Java resolves this collision
//     differently — a duplicate-label CTE column reference is AMBIGUOUS_COLUMN,
//     SemanticAnalyzer.resolveIdentifier — but Go's deployed RFC-142 shadow
//     semantics governs the whole unnest surface; aligning the collision model
//     with Java's is a conformance question for that surface as a whole, not a
//     per-boundary special case.) Any dup the shadow rule does NOT explain
//     (two links sharing an alias, bottom-internal dups) still declines.
//
// A CHAINED star body (`SELECT * FROM t, t.arr AS x, x.sub AS y`) is admitted
// through the SAME gate its fresh translation runs — chainedUnnestOrdinalGate,
// the side-effect-free factoring of translateChainedUnnestOrdinal's decision
// (spine walk + seed-safety + owner slot + collection/seed construction) — so
// admission ⇔ the top link ordinalizes; no admission/translation drift is
// possible for the top link, and the gate's whole-spine derivations
// (ordinalLegType over every prefix) cover the deeper links' schemas.
//
// Returns the boundary LABELS (shadow-deduped, unique) — the single authority
// every consumer shares: the translateScan normalization projects exactly
// these names, and the leg-schema arms (legColumns cteScope/LogicalCTE) type
// the boundary by them.
func (t *cascadesTranslator) derivedBodyStarOrdinalLeg(body logical.LogicalOperator) ([]values.Field, bool) {
	if t.md == nil || t.unnestUnderExistential {
		return nil, false
	}
	op := body
	for {
		f, isF := op.(*logical.LogicalFilter)
		if !isF {
			break
		}
		if len(f.ExistsSubqueries) > 0 || len(f.ScalarSubqueries) > 0 {
			return nil, false
		}
		op = f.Input
	}
	j, isJ := op.(*logical.LogicalJoin)
	if !isJ || j.Kind != logical.JoinInner || len(j.OnExistsSubqueries) > 0 || j.OnPredicate != nil {
		return nil, false
	}
	u, isU := j.Right.(*logical.LogicalUnnest)
	if !isU || len(u.Segments) < 2 || (u.Alias == "" && u.AtAlias == "") {
		return nil, false
	}
	if isChainedUnnest(j.Left, u) {
		// The CHAINED star body: the top link is owned by a deeper link's
		// element. Mirror translateChainedUnnestJoin's gauntlet side-effect-free:
		// the AS==AT overwrite reject (translation loud-errors it; the admission
		// just declines so the raw body surfaces that error itself), the
		// classification (must be a genuine ARRAY sub-path — the non-array
		// dispositions loud-error in translation), then the factored ordinal
		// gate. The correlations passed are the seed's own (sourceAlias /
		// unnestSourceCorrelation); the built values are discarded — the gate is
		// side-effect-free construction.
		if u.Alias != "" && u.AtAlias != "" && strings.EqualFold(u.Alias, u.AtAlias) {
			return nil, false
		}
		elementType, _, disp := t.classifyChainedUnnestArray(j.Left, u)
		if disp != derivedUnnestArray {
			return nil, false
		}
		outerCorr := values.NamedCorrelationIdentifier(sourceAlias(j.Left))
		if _, _, ok := t.chainedUnnestOrdinalGate(j, u, outerCorr, unnestSourceCorrelation(u), elementType); !ok {
			return nil, false
		}
	} else {
		if t.clusterArity(j.Left) != 1 || len(outerBoundAliases(j.Left)) != 1 {
			return nil, false
		}
		outerTable := findOuterScanTable(j.Left, u.Segments[0])
		if outerTable == "" || t.outerSourceIsCTE(outerTable) || outerSourceIsDerivedTable(j.Left, u.Segments[0]) {
			return nil, false
		}
		if _, _, isArray, _ := t.unnestArrayElementType(outerTable, u.Segments[1:]); !isArray {
			return nil, false
		}
	}
	cols := t.ordinalLegColumns(j)
	if cols == nil {
		return nil, false
	}
	// Collect the spine links' AS/AT aliases (the SHADOWING names) and their
	// column count. The seed layout appends the link columns AFTER the bottom
	// source's run, so the alias columns are exactly the trailing aliasCount
	// entries of cols — an earlier same-named entry in the BOTTOM region is the
	// shadowed outer occurrence (dropped); a dup wholly inside the alias region
	// (two links sharing a name) is not a shadow and declines below.
	shadowing, aliasCount := starSpineAliasBindings(j)
	bottomLen := len(cols) - aliasCount
	lastIdx := make(map[string]int, len(cols))
	for i, c := range cols {
		lastIdx[strings.ToUpper(c.Name)] = i
	}
	labels := make([]values.Field, 0, len(cols))
	for i, c := range cols {
		key := strings.ToUpper(c.Name)
		if _, shadowed := shadowing[key]; shadowed && i < bottomLen && lastIdx[key] != i {
			continue // a bottom-source column a link's AS/AT alias shadows
		}
		labels = append(labels, c)
	}
	seen := make(map[string]struct{}, len(labels))
	for _, c := range labels {
		key := strings.ToUpper(c.Name)
		if _, dup := seen[key]; dup {
			return nil, false
		}
		seen[key] = struct{}{}
	}
	return labels, true
}

// starSpineAliasBinding is one spine link label's binding: the link's VISIBLE
// correlation name plus the label's slot WITHIN the link's leg window (the
// element rides slot 0, the AT ordinal slot 1 — the runtime OrdinalityLegs
// layout, `_0`=element / `_1`=ordinal).
type starSpineAliasBinding struct {
	Corr string
	Ord  int
}

// starSpineAliasBindings walks a star body's unnest-right spine (the top join
// down through every lateral link) and returns each link's AS/AT LABEL mapped
// to the link's VISIBLE correlation name (unnestSourceCorrelation — the
// quantifier both bindings ride, the exact correlation the SQL resolver's
// ResolveColumnShadowingQualified emits for a reference to the binding) plus
// the label's leg-window ordinal, plus the total alias-column COUNT (kept
// separately: two links sharing a label collapse to one map key but still
// contribute two seed columns — the count anchors the bottom-region boundary
// in the shadow dedup, and the shared-label shape itself declines at the
// unique guard).
func starSpineAliasBindings(j *logical.LogicalJoin) (map[string]starSpineAliasBinding, int) {
	bindings := make(map[string]starSpineAliasBinding, 2)
	count := 0
	for cur := logical.LogicalOperator(j); ; {
		bj, isJoin := cur.(*logical.LogicalJoin)
		if !isJoin {
			break
		}
		un, isUn := bj.Right.(*logical.LogicalUnnest)
		if !isUn {
			break
		}
		corr := unnestSourceCorrelation(un).Name()
		ord := 0
		if un.Alias != "" {
			bindings[strings.ToUpper(un.Alias)] = starSpineAliasBinding{Corr: corr, Ord: ord}
			ord++
			count++
		}
		if un.AtAlias != "" {
			bindings[strings.ToUpper(un.AtAlias)] = starSpineAliasBinding{Corr: corr, Ord: ord}
			count++
		}
		cur = bj.Left
	}
	return bindings, count
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
			if t.derivedBodyOpaqueOrdinalLeg(body) {
				// A bare-projected unnest-cluster derived boundary is OPAQUE: its
				// internal unnest poison does not propagate past the boundary — the
				// parent sees ONE projected leg (arity 1). Its unnest cluster
				// ordinalizes fresh inside the boundary.
				return 1
			}
			if _, star := t.derivedBodyStarOrdinalLeg(body); star {
				// The single-link STAR body is equally opaque: its boundary is
				// the unnest seed's positional row (one leg, arity 1).
				return 1
			}
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
