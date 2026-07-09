package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 Slice 2 W3b — the ordinal join SEED. When the W2 cluster-arity gate
// admits a 2-way join into the wedge (wedgeGateDecision.Gated), translateJoin
// builds its result value as the ORDINAL concatenation of the two legs
// (Java's `FieldValue.ofOrdinalNumber(QOV(leg), i)` per column — contract
// ruling #2's eager baking) instead of the name-model anchored RC, and bakes
// the join predicates' direct leg references to (leg QOV, field ordinal).
// This is the LIVE flip: every gated join plan from here on carries baked
// values, the executor births positional merged rows (W3a), and the name
// model coexists via dual emission until Slice 4.

// ordinalLegType derives a GATED join leg's flowed RecordType: one field per
// leg output column, in output order, RAW construction (duplicate names
// survive — a gated OUTER-BOX leg's ordinal output is the bare concatenation
// of ITS legs, which may collide; positional access is by ordinal, so
// duplicates are unambiguous). Returns nil when the leg's columns are not
// derivable (the catalog-free nil-md path — same untranslatable rule as the
// anchored seed).
func (t *cascadesTranslator) ordinalLegType(op logical.LogicalOperator) *values.RecordType {
	cols := t.ordinalLegColumns(op)
	if cols == nil {
		return nil
	}
	fields := make([]values.Field, len(cols))
	for i, c := range cols {
		fields[i] = values.Field{Name: strings.ToUpper(c.Name), FieldType: c.FieldType, Ordinal: i}
	}
	// RecordName = the underlying SCAN TABLE (when the leg is a table scan
	// through transparent wrappers): the ordinal counterpart of the name
	// model's qualifyTypeFallback namespace — a reference qualified by the
	// TABLE name while the leg is aliased differently ("PA.ID" over `FROM PA
	// AS s`) resolves through the span's type name exactly as it resolved
	// through the TYPE.COL Datum keys.
	rt := &values.RecordType{RecordName: t.legScanTableName(op), Fields: fields}
	// RFC-173 item 3: a CLUSTERED box leg's concat records its buried-leg
	// boundaries so the layout authority (OrdinalSeedLegWindows) can emit
	// per-buried-leg sub-windows — the serve-side twin of the predicate
	// bake's buried windows (amendment C).
	if bj, isJoin := op.(*logical.LogicalJoin); isJoin {
		rt.Legs = t.buriedLegBounds(bj, 0)
	}
	return rt
}

// buriedLegBounds walks a gated box leg's subtree recording each buried
// LEAF's binding + starting slot within the flat concat (nested gated boxes
// recurse with the cumulative offset). nil on an underivable sub-leg — the
// caller's type stays boundary-free and buried reads keep their loud decline.
func (t *cascadesTranslator) buriedLegBounds(j *logical.LogicalJoin, base int) []values.RecordTypeLeg {
	var out []values.RecordTypeLeg
	off := base
	for _, sub := range t.legsOfGatedJoin(j) {
		subTyp := t.ordinalLegType(sub.op)
		if subTyp == nil {
			return nil
		}
		if sj, isJ := sub.op.(*logical.LogicalJoin); isJ {
			out = append(out, t.buriedLegBounds(sj, off)...)
		} else if sub.binding != "" {
			out = append(out, values.RecordTypeLeg{Name: strings.ToUpper(sub.binding), Start: off, Width: len(subTyp.Fields)})
		}
		off += len(subTyp.Fields)
	}
	return out
}

// legScanTableName walks transparent wrappers to the leg's BASE-TABLE scan
// and returns its UPPER table name; "" for non-scan legs and CTE/derived
// references (the name model's type fallback keys on the stored record's
// TYPE name, which only real tables have — a CTE-scoped scan's Datum rows
// carry no record, so the name model never wrote a fallback key for them).
func (t *cascadesTranslator) legScanTableName(op logical.LogicalOperator) string {
	for {
		switch o := op.(type) {
		case *logical.LogicalScan:
			key := strings.ToUpper(o.Table)
			if _, ok := t.cteExprScope[key]; ok {
				return ""
			}
			if _, ok := t.cteScope[key]; ok {
				return ""
			}
			return key
		case *logical.LogicalFilter:
			op = o.Input
		case *logical.LogicalLimit:
			op = o.Input
		// LogicalProject is DELIBERATELY not transparent here, unlike in
		// ordinalEligible/clusterArity (@claude PR-447 flagged the asymmetry;
		// documented rather than changed): a projected leg's columns are the
		// PROJECTION's outputs, and in the name model a projected row carries
		// no stored record — recordTypeName(qr) is empty, so
		// qualifyTypeFallback never wrote TYPE.COL keys for it. Adding the
		// base table's name would INVENT a namespace the name model never
		// had (a dualwindow divergence), not restore one.
		default:
			return ""
		}
	}
}

// ordinalLegColumns is the ORDINAL-model counterpart of legColumns for a
// gated join's legs: scans/boxes resolve exactly as legColumns does, but a
// nested JOIN leg — necessarily a GATED box (an inner join as a leg would
// make the cluster ≥3-way and the gate would not have admitted the parent;
// outer boxes gate unconditionally) — contributes the BARE ordinal
// concatenation of its own legs (its ordinal output type), NOT the anchored
// bare+qualified key set the name model exposes. Duplicates survive. The
// name-model legColumns stays untouched: a name-model PARENT over a gated box
// still reads the box's dual-emitted Datum by dotted keys.
func (t *cascadesTranslator) ordinalLegColumns(op logical.LogicalOperator) []values.Field {
	switch o := op.(type) {
	case *logical.LogicalJoin:
		if un, isUnnest := o.Right.(*logical.LogicalUnnest); isUnnest {
			// A lateral-unnest join consumed as an ORDINAL OUTER LEG — the
			// CHAINED unnest's FIRST link (`(t ⋈ t.arr AS x)` as the outer of
			// `x.sub AS y`, RFC-173 S4 unnest-residual class 4). The gate
			// (ordinalWedgeGateDecide) POISONS every unnest-right join, so the
			// panic arm below would fire; but a chained first link that
			// ITSELF ordinalized (translateUnnestJoin's :1565 binary seed) flows
			// a genuine POSITIONAL row whose type is exactly the outer's ordinal
			// columns concatenated with the unnest's element/ordinal columns.
			// Mirror that layout: ordinalLegColumns(o.Left) ++ legColumns(unnest)
			// — the SAME order unnestOrdinalSeed(o.Left) emits (outer run then
			// unnestSeedInnerFields), so the second link's ofOrdinal reads land on
			// the slots the first link births. legColumns(unnest) gives the AS/AT
			// column names (element type is best-effort UnknownType — a struct
			// element the second link descends by name, never a positional read).
			//
			// Reachable ONLY through the chained ordinal seed: ordinalEligible and
			// clusterArity both poison an unnest-right join, so no gated CLUSTER
			// admits one as a leg — the panic tripwire for a genuine NAME-MODEL
			// (non-unnest) join leg below is fully preserved. A nil from the outer
			// (underivable) declines cleanly (the caller fails open to name-model).
			//
			// ARBITRARY DEPTH AND TOPOLOGY: the chained ordinal seed gate
			// (translateChainedUnnestOrdinal → chainedSpineWalk) admits any
			// unnest-right spine whose BOTTOM is a single lateral source
			// (clusterArity==1) and whose links each own exactly one deeper
			// link's element — linear chains and forks alike. This arm's
			// recursion on o.Left therefore descends one level per admitted
			// link until it bottoms out at that single source; the merged type
			// it accumulates is the same for a fork as for a linear chain
			// (columns append in spine order — ownership only changes WHERE
			// the collection roots, via chainedOwnerElementSlot, never the row
			// layout). A MULTI-source BOX bottom still declines at the gate
			// (c5b), so this recursion only ever walks the unnest-right spine,
			// never a box.
			//
			// LOAD-BEARING INVARIANT: legColumns(un) here must stay layout-identical
			// (count / order / names) to unnestSeedInnerFields(un) — the actual inner
			// fields unnestOrdinalSeed(o.Left) births — because the second link's
			// ofOrdinal reads index into THIS leg type. If either drifts (a refactor of
			// legColumns or unnestSeedInnerFields), the reads silently land on the wrong
			// slot. Pinned by the certificate's actual-value + AT-both-links assertions
			// (rfc173_s4_chained_unnest_ordinal_fdb_test.go).
			outerCols := t.ordinalLegColumns(o.Left)
			if outerCols == nil {
				return nil
			}
			merged := make([]values.Field, 0, len(outerCols)+2)
			merged = append(merged, outerCols...)
			merged = append(merged, t.legColumns(un)...)
			return merged
		}
		// S3 fulcrum: a GATED join leg (a FULL box's gated inner cluster, or
		// a gated box under a FULL boundary) contributes the flat ordinal
		// concatenation of ITS legs, in rv (FROM) order — the box's own
		// output type; duplicates survive (positional access). A NAME-MODEL
		// join leg reaching here is still a gate mis-scope: loud, never a
		// silently mis-typed leg.
		prev := t.inInnerCluster
		t.inInnerCluster = false
		d := t.ordinalWedgeGateDecide(o)
		t.inInnerCluster = prev
		if !d.Gated {
			panic("RFC-173 ordinal seed: a NAME-MODEL join leg reached the ordinal seed (cluster-arity gate mis-scope, planner bug)")
		}
		var fields []values.Field
		for _, leg := range t.legsOfGatedJoin(o) {
			cols := t.ordinalLegColumns(leg.op)
			if cols == nil {
				return nil
			}
			if leg.nullSupplying {
				// Amendment B (ruling I3's second half): the NULL-SUPPLYING
				// window's COLUMN types go nullable through the box's own
				// output type — Java's pullUpResultColumnsWithNullability
				// (true). The padded rows serve NULL in these slots, so the
				// flowed metadata (driver column nullability) must say so.
				// Copy-on-wrap: legColumns may hand back shared slices.
				wrapped := make([]values.Field, len(cols))
				for i, c := range cols {
					c.FieldType = values.WithNullability(c.FieldType, true)
					wrapped[i] = c
				}
				cols = wrapped
			}
			fields = append(fields, cols...)
		}
		return fields
	default:
		return t.legColumns(op)
	}
}

// clusterLeg is one leg of a gathered inner-join cluster: the leg operator
// and its FROM-order alias. nullSupplying marks a LEFT-outer box's null side
// (RFC-173 W4-left): the seed types that leg's QOV RECORD-LEVEL nullable —
// Java's `pullUpResultColumnsWithNullability(true)` re-pulls through
// `QOV(alias, type.withNullability(true))`, and OuterJoinExpression VERIFIES
// nullability on the QOV's result type, never per column (design ruling I3).
type clusterLeg struct {
	op    logical.LogicalOperator
	alias string
	// binding is the leg's BINDING correlation name (sourceBinding): equal
	// to alias for every non-duplicate leg; the parser-minted `Q$DUPN` for
	// a later duplicate-alias leg (RFC-173 QP-REF-BIND item 1). Seed
	// quantifiers, bake maps and windows key on THIS; alias stays display.
	// While the gate's dup poison arm stands (pre-item-1-c2) binding ==
	// alias on every leg that reaches a seed.
	binding       string
	nullSupplying bool
}

// clusterLegOf is the ONLY way to build a clusterLeg from a leg operator: it
// fills alias AND binding together so no construction site can forget the
// binding (an alias-only leg nils the seed via the binding=="" decline — the
// exact re-derivation hazard the design ruling's carried-not-rederived condition
// names).
func clusterLegOf(op logical.LogicalOperator, nullSupplying bool) clusterLeg {
	return clusterLeg{op: op, alias: sourceAlias(op), binding: legBinding(op), nullSupplying: nullSupplying}
}

// legBinding is clusterLegOf's binding derivation: sourceBinding, except a
// JOIN consumed AS A LEG (a clustered box) gets a DETERMINISTIC minted
// binding — its rightmost leaf's binding + "$BOX" (RFC-173 item 3). The bare
// leaf name cannot serve both levels: at the box's OWN select it names the
// leaf quantifier (typed by the leaf), while at the parent it would name the
// box (typed by the whole concat), and one plan carries both — the widen
// invariant's divergent-baked-types panic (aggregates over the mixed class).
// Fold-stable and unique (leaf bindings are dup-minted unique) — the Q$DUP
// discipline: internal plumbing users can never reference; display surfaces
// keep sourceAlias. Applied ONLY in leg contexts (here and the gated
// quantifier naming) — sourceBinding's non-leg consumers keep the leaf name.
func legBinding(op logical.LogicalOperator) string {
	if _, isJoin := op.(*logical.LogicalJoin); isJoin {
		return sourceBinding(op) + "$BOX"
	}
	return sourceBinding(op)
}

// gatherInnerClusterLegs flattens DIRECT inner-join nesting into FROM-order
// legs — Java flattens inner joins at translation (QueryVisitor.java:429-434
// accumulates into a single flat SelectExpression); nested binaries are never
// seeded (fulcrum ruling). Derived-table/filter boundaries stay LEGS and
// compose later via SelectMergeRule. Nested non-inner joins are unreachable
// under a gated root (clusterArity poisons LEFT; FULL contributes as an
// opaque leg) — treated defensively as legs. Uses the ORIGINAL operand order
// (j.Left, j.Right): the RIGHT-join execution swap never fires for inner
// kinds, so tree order IS FROM order.
func (t *cascadesTranslator) gatherInnerClusterLegs(j *logical.LogicalJoin) []clusterLeg {
	var legs []clusterLeg
	var walk func(op logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if nj, isJoin := op.(*logical.LogicalJoin); isJoin && nj.Kind == logical.JoinInner &&
			len(nj.OnExistsSubqueries) == 0 {
			if _, isUnnest := nj.Right.(*logical.LogicalUnnest); !isUnnest {
				walk(nj.Left)
				walk(nj.Right)
				return
			}
		}
		legs = append(legs, clusterLegOf(op, false))
	}
	walk(j.Left)
	walk(j.Right)
	return legs
}

// legsOfGatedJoin is the kind-aware leg list of a GATED join: an INNER root
// gathers its whole direct-nesting cluster (flat N-way); an outer box keeps
// its two box legs verbatim (each leg may itself be a gated inner cluster —
// ONE leg, not gathered through), in DECLARATION order with only the
// null-supplying ROLE keyed by kind (design ruling I2: Java assembles the RV
// in source order regardless of join type; a RIGHT join's null side is its
// LEFT operand). W4-left: LEFT/RIGHT mark exactly one null-supplying leg;
// FULL marks NEITHER leg record-level nullable — the column TYPES read NOT
// NULL where Java would type all-FULL-columns nullable. This is ANSWER-INVARIANT
// (pinned): the row VALUES are correct because the NLJ binds nil for the
// unmatched leg regardless of the static type marker, so a correlation to a
// null-supplied FULL leg (`FOB.K` in `a FULL OUTER b`) evaluates against nil and
// yields the right rows — proven end-to-end by
// TestFDB_RFC173S4_C5a_FullOuterUnnestExists (the null-supplied-leg EXISTS/
// NOT-EXISTS discriminators). The type marker affects only schema-level
// nullability reporting, not the runtime value.
func (t *cascadesTranslator) legsOfGatedJoin(j *logical.LogicalJoin) []clusterLeg {
	if j.Kind == logical.JoinInner {
		return t.gatherInnerClusterLegs(j)
	}
	return []clusterLeg{
		clusterLegOf(j.Left, j.Kind == logical.JoinRight),
		clusterLegOf(j.Right, j.Kind == logical.JoinLeft),
	}
}

// bakeLegType is one gated-join leg's predicate-bake entry: the leg's flowed
// type (the baked QOV's type — the flat concat for a box leg) plus the BAKE
// WINDOW bare names resolve within. A box leg is NAMED after its rightmost
// LEAF (sourceAlias), so a bare reference `b.col` addressing the box alias
// means THAT LEAF's column: FieldIndex must run against the leaf's own type
// at its concat offset, never first-match across the whole concat — an
// earlier leg's duplicate name would silently bake the WRONG column. For a
// plain (non-join) leg the window IS the whole type at offset 0.
type bakeLegType struct {
	typ        *values.RecordType // the leg's flowed type (the baked QOV's type)
	leafOffset int                // the leaf's column offset within typ
	leafTyp    *values.RecordType // the leaf's own type (== typ for plain legs)
	// bakeCorr is the correlation the baked reference binds to when it
	// DIFFERS from the referenced alias: a BURIED leg of a clustered box leg
	// (RFC-173 item 3, amendment C) has no quantifier of its own — the box's
	// single quantifier is named by its RIGHTMOST leaf (the sourceBinding
	// convention), so every other buried leaf bakes onto that correlation at
	// its own offset within the box concat. Empty = the referenced alias
	// itself (every top-level leg and the rightmost leaf).
	bakeCorr string
}

// legBakeWindow resolves a leg's bake window: the rightmost LEAF's offset and
// own type, recursing through gated join boxes exactly as sourceAlias recurses
// to the name (legsOfGatedJoin's last leg IS sourceAlias's j.Right chain, for
// both gathered inner clusters and FULL box legs). nil leafTyp when a
// sub-leg's columns are underivable — the caller then omits the entry and the
// references stay lazy (sound by the load-bearing lazy invariant).
func (t *cascadesTranslator) legBakeWindow(op logical.LogicalOperator) (int, *values.RecordType) {
	j, isJoin := op.(*logical.LogicalJoin)
	if !isJoin {
		return 0, t.ordinalLegType(op)
	}
	legs := t.legsOfGatedJoin(j)
	offset := 0
	for _, leg := range legs[:len(legs)-1] {
		cols := t.ordinalLegColumns(leg.op)
		if cols == nil {
			return 0, nil
		}
		offset += len(cols)
	}
	subOffset, leafTyp := t.legBakeWindow(legs[len(legs)-1].op)
	if leafTyp == nil {
		return 0, nil
	}
	return offset + subOffset, leafTyp
}

// gatedJoinLegTypes builds the predicate-bake legTypes map (UPPER alias →
// bakeLegType) for a GATED join from its GATHERED legs — the exact pairing the
// seed's quantifiers use. Deriving it from the binary root's two operands is
// WRONG for a nested-binary cluster: sourceAlias(join-operand) recurses to the
// buried RIGHTMOST table while ordinalLegType(join-operand) is the whole
// subtree's concat, so a bake would pin a single table's reference to a
// concat-relative ordinal (out of range at runtime — or silently the WRONG
// column when FieldIndex first-matches an earlier leg's duplicate name). A leg
// with an empty alias or an undeciphered type is simply absent: its references
// stay lazy, which is sound (the load-bearing lazy invariant).
func (t *cascadesTranslator) gatedJoinLegTypes(j *logical.LogicalJoin) map[string]bakeLegType {
	legs := t.legsOfGatedJoin(j)
	legTypes := make(map[string]bakeLegType, len(legs))
	for _, leg := range legs {
		if leg.binding == "" {
			continue
		}
		typ := t.ordinalLegType(leg.op)
		if typ == nil {
			continue
		}
		leafOffset, leafTyp := t.legBakeWindow(leg.op)
		if leafTyp == nil {
			continue
		}
		// Keyed by the BINDING correlation name (== UPPER alias for every
		// non-duplicate leg; the parser-minted id for a later duplicate —
		// RFC-173 QP-REF-BIND item 1, the map key an alias would collide).
		legTypes[leg.binding] = bakeLegType{typ: typ, leafOffset: leafOffset, leafTyp: leafTyp}
		// A CLUSTERED box leg buries sources whose references bake onto the
		// BOX quantifier at each buried leaf's offset within the concat
		// (amendment C) — the same registration the seed's own legTypes get
		// in ordinalJoinSeedFields. Without it a WHERE conjunct naming a
		// buried source stays lazy at the box select and grandchild-binds.
		if bj, isJoin := leg.op.(*logical.LogicalJoin); isJoin {
			t.addBuriedBakeWindows(bj, leg.binding, typ, 0, legTypes)
		}
	}
	return legTypes
}

// gatherInnerClusterPreds collects every nested inner join's ON predicate in
// the gathered cluster (the root's own ON is the caller's — translateJoin
// already extracts it), post-order so a deterministic pred order rides the
// seed select.
func gatherInnerClusterPreds(j *logical.LogicalJoin) []predicates.QueryPredicate {
	var preds []predicates.QueryPredicate
	var walk func(op logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		nj, isJoin := op.(*logical.LogicalJoin)
		if !isJoin || nj.Kind != logical.JoinInner || len(nj.OnExistsSubqueries) > 0 {
			return
		}
		if _, isUnnest := nj.Right.(*logical.LogicalUnnest); isUnnest {
			return
		}
		walk(nj.Left)
		walk(nj.Right)
		if nj.OnPredicate != nil {
			if qp, ok := nj.OnPredicate.(predicates.QueryPredicate); ok {
				preds = append(preds, qp)
			}
		}
	}
	walk(j.Left)
	walk(j.Right)
	return preds
}

// translateGatheredInnerCluster builds the FLAT N-quantifier select for a
// gated inner cluster with nested direct joins (the S3 fulcrum's Java-aligned
// translation shape): one ForEach quantifier per gathered leg in FROM order,
// the flat N-leg ordinal seed RC, and the union of every nested join's ON
// predicates with cross-leg conjuncts baked. Legs translate ENCLOSED (their
// own nested content — derived bodies etc. — is inside this cluster).
func (t *cascadesTranslator) translateGatheredInnerCluster(j *logical.LogicalJoin, legs []clusterLeg) expressions.RelationalExpression {
	// Legs of a GATED parent translate FRESH (S3 fulcrum): a leg's own inner
	// joins gate independently and SelectMergeRule composes the leg's ordinal
	// RV into this parent via translateValueCorrelations + the fuse arm —
	// Java's model. (Enclosure poisoning survives only for the name-model
	// parents still standing after W4-left/W5: the residual unnest
	// compositions and ungated correlated-scalar outers — recursive-CTE
	// REFERENCE joins gate ordinal, and gated existential selects take the
	// ordinal rebase.)
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	quantifiers := make([]expressions.Quantifier, 0, len(legs))
	sourceAliases := make([]string, 0, len(legs))
	for _, leg := range legs {
		ref := t.translateRef(leg.op)
		if ref == nil {
			t.inInnerCluster = prevEnclosure
			return nil
		}
		quantifiers = append(quantifiers, expressions.NamedForEachQuantifier(
			values.NamedCorrelationIdentifier(leg.binding), ref,
		))
		sourceAliases = append(sourceAliases, leg.binding)
	}
	t.inInnerCluster = prevEnclosure

	var preds []predicates.QueryPredicate
	if j.OnPredicate != nil {
		if qp, ok := j.OnPredicate.(predicates.QueryPredicate); ok {
			preds = append(preds, qp)
		}
	}
	preds = append(preds, gatherInnerClusterPreds(j)...)

	resultValue, legTypes := t.buildOrdinalJoinResultValue(legs)
	if resultValue == nil {
		// A leg's columns are not derivable (catalog-free nil-md path) —
		// untranslatable, same rule as the binary seed.
		return nil
	}
	preds = bakeGatedJoinPredicates(preds, legTypes)

	return expressions.NewSelectExpressionWithJoinType(
		resultValue,
		quantifiers,
		preds,
		sourceAliases,
		expressions.JoinInner,
	)
}

// buildOrdinalJoinResultValue builds the gated 2-way join's result value: the
// raw RC of ofOrdinalNumber references over the two legs' typed QOVs, in
// DECLARATION order (the caller passes rvLeft/rvRight — `SELECT *` column
// order follows the SQL FROM order, not the RIGHT-join execution swap; the
// executor binds legs by ALIAS, so RC leg order is independent of cursor
// outer/inner roles). Returns nil when a leg is untranslatable (same rule as
// the anchored seed). The seed shape is asserted loud
// (values.AssertOrdinalJoinSeed — the standing review condition on W3b).
// The returned legTypes map (UPPER alias → bakeLegType) feeds
// bakeGatedJoinPredicates at the seed and the WHERE-merge site.
func (t *cascadesTranslator) buildOrdinalJoinResultValue(legs []clusterLeg) (values.Value, map[string]bakeLegType) {
	fields, legTypes := t.ordinalJoinSeedFields(legs)
	if fields == nil {
		return nil, nil
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(rc)
	return rc, legTypes
}

// ordinalJoinSeedFields builds the flat ordinal leg runs + the predicate-bake
// legTypes map WITHOUT constructing/asserting the RC — the W5 gathered-unnest
// seed composes these outer runs with the unnest inner-leg fields (whose
// mixed/partial shapes legitimately skip AssertOrdinalJoinSeed) before
// deciding on the assert. nil fields = a leg is untranslatable (same decline
// rule as the seed).
func (t *cascadesTranslator) ordinalJoinSeedFields(legs []clusterLeg) ([]values.RecordConstructorField, map[string]bakeLegType) {
	var fields []values.RecordConstructorField
	legTypes := make(map[string]bakeLegType, len(legs))
	for _, leg := range legs {
		if leg.binding == "" {
			return nil, nil
		}
		typ := t.ordinalLegType(leg.op)
		if typ == nil {
			return nil, nil
		}
		leafOffset, leafTyp := t.legBakeWindow(leg.op)
		if leafTyp == nil {
			return nil, nil
		}
		// BINDING-keyed (RFC-173 QP-REF-BIND item 1): == UPPER alias for
		// every non-duplicate leg; the parser-minted id for later duplicates
		// keeps the map and the QOV correlations collision-free.
		legTypes[leg.binding] = bakeLegType{typ: typ, leafOffset: leafOffset, leafTyp: leafTyp}
		// RFC-173 item 3 (amendment C): a CLUSTERED box leg buries sources
		// whose references must bake onto the BOX's quantifier at each
		// buried leg's offset within the box concat — Java's
		// collapseLeftSideOperators + rewireQov-by-ordinal analog. Without
		// these entries predicateLegAliases counts a cross-leg conjunct
		// spanning a buried source (`c.a_id = a.id` over `(A⋈B) LEFT C`) as
		// single-leg and leaves a grandchild-correlated lazy reference on
		// the box select.
		if bj, isJoin := leg.op.(*logical.LogicalJoin); isJoin {
			t.addBuriedBakeWindows(bj, leg.binding, typ, 0, legTypes)
		}
		if leg.nullSupplying {
			// The LEFT-outer null side: the QOV's RECORD TYPE goes nullable
			// (Java's type.withNullability(true) at the pull-up; the verify
			// keys on the QOV's result type — design ruling I3). Column
			// types stay their own; the record-level bit is what the
			// executor's null-leg birth and the metadata nullability read.
			// WithNullability, NOT NewRecordType: a CLUSTERED
			// null-supplying leg (item 3) concatenates its buried legs'
			// columns positionally and duplicate bare names legitimately
			// survive (the same rule as ordinalLegType's own construction);
			// the constructor's duplicate check is for name-addressed types.
			typ = values.WithNullability(typ, true).(*values.RecordType)
		}
		qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(leg.binding), typ)
		for i := range typ.Fields {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				// Impossible by construction (the ordinal ranges over the
				// type's own fields) — loud, matching the seed assert.
				panic("RFC-173 ordinal seed: " + err.Error())
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	return fields, legTypes
}

// bakeGatedJoinPredicates rewrites a gated join's CROSS-LEG predicates so
// direct leg references — lazy `FieldValue(QOV(leg), col)`, bare (non-dotted)
// names — become BAKED `ofOrdinalNumber(typedQOV(leg), idx)` per contract
// ruling #2: eager (quantifier, field ordinal) resolution for the predicates
// the join loop itself evaluates.
//
// Only predicates referencing BOTH legs are baked: a SINGLE-LEG predicate is
// pushdown fodder (PushFilterBelowJoinRule / SARG extraction move it into the
// leg's own select — including NAME-MODEL legs like aggregate boxes whose
// rows carry no positional; a baked node there is a loud
// BakedNameContextError, the W3b flip's live catch on CTE-aggregate joins).
// Lazy single-leg predicates are SOUND wherever they land by the
// load-bearing lazy invariant itself: they evaluate against ONE leg's row and
// nothing re-types a leg between plan-finalize and eval — pushdown moves the
// predicate, never the leg's type. Cross-leg predicates cannot be pushed
// below either leg (they need both), so they stay in the join select where
// the per-leg binder context resolves them.
//
// References that are not direct leg columns (dotted names, outer
// correlations, columns absent from the leg type, already-baked nodes) pass
// through untouched. Shares the exact predicate-walk spine the planner rules
// use (predicates.ReplaceValues).
// addBuriedBakeWindows registers a bake window for every leaf buried inside a
// clustered box leg (RFC-173 item 3, amendment C): binding → {the box's
// flowed concat type, the leaf's cumulative offset within it, the leaf's own
// type, the box quantifier's correlation (boxCorr — the rightmost-leaf
// binding the box's QOV is named by)}. Nested gated boxes recurse with the
// SAME boxCorr and cumulative offset — everything within the box leg binds
// to its single quantifier. The rightmost leaf's own-keyed entry (written by
// the caller) is preserved (first-writer wins). An underivable sub-leg stops
// the walk — its references stay lazy (the load-bearing lazy invariant).
func (t *cascadesTranslator) addBuriedBakeWindows(j *logical.LogicalJoin, boxCorr string, boxTyp *values.RecordType, base int, legTypes map[string]bakeLegType) {
	off := base
	for _, sub := range t.legsOfGatedJoin(j) {
		subTyp := t.ordinalLegType(sub.op)
		if subTyp == nil {
			return
		}
		if sj, isJ := sub.op.(*logical.LogicalJoin); isJ {
			t.addBuriedBakeWindows(sj, boxCorr, boxTyp, off, legTypes)
		} else if _, exists := legTypes[sub.binding]; !exists && sub.binding != "" {
			legTypes[sub.binding] = bakeLegType{typ: boxTyp, leafOffset: off, leafTyp: subTyp, bakeCorr: boxCorr}
		}
		off += len(subTyp.Fields)
	}
}

func bakeGatedJoinPredicates(preds []predicates.QueryPredicate, legTypes map[string]bakeLegType) []predicates.QueryPredicate {
	if len(preds) == 0 || len(legTypes) == 0 {
		return preds
	}
	bake := func(v values.Value) values.Value {
		key, isRef := legRef(v)
		if !isRef {
			return v
		}
		legType, isLeg := legTypes[key]
		if !isLeg || legType.typ == nil || legType.leafTyp == nil {
			return v
		}
		fv := v.(*values.FieldValue) // legRef confirmed the cast + guards
		// Resolve the bare name within the leg's BAKE WINDOW (the rightmost
		// leaf for a box leg — the alias names that leaf), then offset into
		// the leg's flowed concat. A whole-concat FieldIndex would first-match
		// an earlier leg's duplicate name — silently the wrong column.
		idx, found := legType.leafTyp.FieldIndex(fv.Field)
		if !found {
			return v
		}
		// The baked node's correlation takes the LEG-ALIAS CASE the gather
		// minted (UPPER via sourceAlias — the one case authority): legRef
		// already uppercased it, but emitting the reference's ORIGINAL case
		// would let a baked node's correlation diverge from the quantifier it
		// must bind to — downstream classification compares correlations
		// exactly, and a case-mismatched ON conjunct silently classified as
		// deeply-correlated during commit-2 bring-up (unreachable via
		// production SQL, which uppercases upstream; pinned white-box).
		bakeCorr := key
		if legType.bakeCorr != "" {
			// A BURIED leg (amendment C): the reference binds the box's
			// quantifier — named by its rightmost leaf — not the buried
			// alias, which has no quantifier at this select.
			bakeCorr = strings.ToUpper(legType.bakeCorr)
		}
		typedQOV := values.NewQuantifiedObjectValueOfType(
			values.NamedCorrelationIdentifier(bakeCorr), legType.typ,
		)
		baked, err := values.NewFieldValueOfOrdinal(typedQOV, legType.leafOffset+idx)
		if err != nil {
			panic("RFC-173 predicate bake: " + err.Error()) // the window is within the concat by construction
		}
		return baked
	}
	// Bake per CONJUNCT: a top-level AND is split by the partition/pushdown
	// rules later, so the cross-leg test must apply to each conjunct
	// independently (`c.id = sub.id AND sub.cnt > 1` — the first bakes, the
	// second is single-leg pushdown fodder and stays lazy).
	var bakeConjuncts func(p predicates.QueryPredicate) predicates.QueryPredicate
	bakeConjuncts = func(p predicates.QueryPredicate) predicates.QueryPredicate {
		if and, isAnd := p.(*predicates.AndPredicate); isAnd {
			changed := false
			newSubs := make([]predicates.QueryPredicate, len(and.SubPredicates))
			for i, s := range and.SubPredicates {
				newSubs[i] = bakeConjuncts(s)
				if newSubs[i] != s {
					changed = true
				}
			}
			if !changed {
				return p
			}
			return predicates.NewAnd(newSubs...)
		}
		if predicateLegAliases(p, legTypes) < 2 && !predicateRefsBuriedLeg(p, legTypes) {
			return p // single-leg TOP-LEVEL or leg-free: stays lazy (pushdown-safe)
		}
		return predicates.ReplaceValues(p, bake)
	}
	out := make([]predicates.QueryPredicate, len(preds))
	for i, p := range preds {
		out[i] = bakeConjuncts(p)
	}
	return out
}

// legRef extracts the UPPER leg-correlation name of a BARE, UNBAKED FieldValue over a
// QuantifiedObjectValue — the reference shape all three gated-predicate walks key on
// (the bake closure, predicateLegAliases, predicateRefsBuriedLeg). Returns "",false for
// a non-FieldValue, an already-baked ref (Resolved != nil — a re-walk must not re-count
// or re-bake it), a flat-dotted read (fv.Field carries a '.', the RFC-142 mergedQOV.
// "leg.col" channel, resolved elsewhere), or a non-QOV child. Each caller then consults
// legTypes[key] for its own decision — is-a-leg (count), is-buried (bakeCorr != ""), or
// build the baked node — so one prologue serves three keys.
func legRef(v values.Value) (string, bool) {
	fv, isFV := v.(*values.FieldValue)
	if !isFV || fv.Resolved != nil || strings.Contains(fv.Field, ".") {
		return "", false
	}
	qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
	if !isQOV {
		return "", false
	}
	return strings.ToUpper(qov.Correlation.Name()), true
}

// predicateLegAliases counts how many DISTINCT gated-join leg aliases a predicate's
// value trees reference directly (bare unbaked FieldValues over a leg QOV — the same
// references the bake rewrites; see legRef).
func predicateLegAliases(p predicates.QueryPredicate, legTypes map[string]bakeLegType) int {
	seen := make(map[string]struct{}, 2)
	predicates.ReplaceValues(p, func(v values.Value) values.Value {
		if key, ok := legRef(v); ok {
			if _, isLeg := legTypes[key]; isLeg {
				seen[key] = struct{}{}
			}
		}
		return v
	})
	return len(seen)
}

// predicateRefsBuriedLeg reports whether a predicate references a BURIED box leg
// (a legType with bakeCorr != "" — the buried leaf has NO quantifier of its own;
// the box's single quantifier, named by its rightmost leaf, holds its slot). Such a
// reference MUST bake even as the ONLY leg named: a single-leg predicate is normally
// left lazy for pushdown, but a lazy BURIED reference has no per-leg select or
// quantifier to push into, so it evaluates QOV(buriedAlias) → unbound → NULL and
// silently drops rows. This is the buried-vs-element WHERE class (`WHERE A.K = X`,
// A buried in `(A FULL C)`, X the unnest element): the element is not a leg, so the
// two-leg cross-leg test never fires; only the buried single-leg check catches it.
func predicateRefsBuriedLeg(p predicates.QueryPredicate, legTypes map[string]bakeLegType) bool {
	buried := false
	predicates.ReplaceValues(p, func(v values.Value) values.Value {
		if key, ok := legRef(v); ok {
			if lt, ok := legTypes[key]; ok && lt.bakeCorr != "" {
				buried = true
			}
		}
		return v
	})
	return buried
}
