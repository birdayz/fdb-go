package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// RFC-173 W4b shape 1 — the CLUSTERED-OUTER correlated-scalar ordinal seed.
//
// A correlated scalar subquery in the projection over a MULTI-TABLE outer FROM
// (`SELECT c.name, (SELECT o.amount FROM orders o WHERE o.id = c.id) FROM
// customers c, extras e ...`) was BROKEN on the name model: the level-2 outer
// quantifier carries only the RIGHTMOST leaf's alias, so a correlation to any
// other leg either failed to plan (comma clusters — the inner's correlation
// never matched the outer quantifier, no NLJ implemented) or returned a SILENT
// NULL (JOIN..ON / LEFT-join outers — the leg alias was unbound at eval).
//
// The ordinal path fixes the GATED-cluster class end-to-end:
//   - the outer cluster translates FRESH (enclosure lift) so it gates and
//     flows a positional row;
//   - the level-2 seed's outer leg is ONE full ordinal run over a
//     fresh-unique concat QOV, fields named DOTTED `LEG.COL` so the level-2
//     output row stays name-addressable for the projections above;
//   - the inner plan's correlated leg refs are PULLED UP onto the concat QOV
//     (Java `Value.pullUp` / `Quantifier.pullUpResultColumns`:
//     `FieldValue.ofOrdinalNumber` over the immediate quantifier), baked as
//     `ofOrdinal(QOV(outer, concat), legStart+idx)` — ALL legs, including the
//     rightmost: a lazy name read against a positional binding is a
//     mixed-model read.
// Ungated outers (LEFT-box, unnest, existential-carrying, dup-alias) keep the
// name-model fallback for rightmost-only correlation (works today) and DECLINE
// (clean plan error) for known non-rightmost correlation — CORRECT or LOUD,
// never silently NULL. The fresh-unique outer correlation id (the W2
// unique-ids principle) structurally rules out the DIVERGENT-baked-types
// collision between the level-2 concat type and the cluster's own rightmost
// leg type.

// clusterPullUp is the resolved spine of the gated clustered-outer ordinal
// path: the fresh level-2 outer correlation, the cluster's flat concat type,
// and the FROM-order per-leg column spans that map `LEG.COL` to a global
// ordinal of the concat.
type clusterPullUp struct {
	outerCorr  values.CorrelationIdentifier
	concatType *values.RecordType
	legs       []clusterLegSpan
	legByAlias map[string]clusterLegSpan
	// missed flips when the bake meets a leg reference it cannot map (a column
	// absent from the leg's type) — the caller must then decline the ordinal
	// path (design ruling (iv): never a silent partial bake).
	missed bool
}

// clusterLegSpan is one plain (non-box) leg's span within the flat concat.
type clusterLegSpan struct {
	alias string             // UPPER leg alias
	start int                // global ordinal of the leg's first column
	typ   *values.RecordType // the leg's own flowed type
}

// peelToClusterJoin walks row-shape-preserving unaries (WHERE filters without
// subquery riders, LIMIT, ORDER BY, DISTINCT) from the csq project's input
// down to the outer cluster join. nil when the outer is single-source or the
// chain contains anything else — the caller then keeps the existing paths.
func peelToClusterJoin(op logical.LogicalOperator) *logical.LogicalJoin {
	for {
		switch o := op.(type) {
		case *logical.LogicalJoin:
			return o
		case *logical.LogicalFilter:
			if len(o.ExistsSubqueries) > 0 || len(o.ScalarSubqueries) > 0 {
				return nil
			}
			op = o.Input
		case *logical.LogicalLimit:
			op = o.Input
		case *logical.LogicalSort:
			op = o.Input
		case *logical.LogicalDistinct:
			op = o.Input
		default:
			return nil
		}
	}
}

// buildClusterPullUp assembles the pull-up spine for a GATED cluster join.
// nil (decline) when a gathered leg is itself a join box (dotting its buried
// columns with the box alias would mis-name them), unaliased, or untypeable.
// Must only be called when the wedge gate admitted j — ordinalLegType on a
// name-model join leg panics by design (gate mis-scope).
func (t *cascadesTranslator) buildClusterPullUp(j *logical.LogicalJoin) *clusterPullUp {
	concat := t.ordinalLegType(j)
	if concat == nil || len(concat.Fields) == 0 {
		return nil
	}
	pu := &clusterPullUp{concatType: concat, legByAlias: map[string]clusterLegSpan{}}
	offset := 0
	for _, leg := range t.legsOfGatedJoin(j) {
		if _, isJoin := leg.op.(*logical.LogicalJoin); isJoin {
			return nil
		}
		if leg.alias == "" {
			return nil
		}
		typ := t.ordinalLegType(leg.op)
		if typ == nil || len(typ.Fields) == 0 {
			return nil
		}
		span := clusterLegSpan{alias: strings.ToUpper(leg.alias), start: offset, typ: typ}
		if _, dup := pu.legByAlias[span.alias]; dup {
			return nil // defensive: the gate already declines dup leg aliases
		}
		pu.legs = append(pu.legs, span)
		pu.legByAlias[span.alias] = span
		offset += len(typ.Fields)
	}
	if offset != len(concat.Fields) {
		return nil // leg-span/concat drift — decline, never a mis-ordinal bake
	}
	pu.outerCorr = values.UniqueCorrelationIdentifier()
	return pu
}

// bake is the pull-up rewrite: a lazy reference to a cluster leg — either
// resolver-anchored `FieldValue(QOV(leg), COL)` or flat dotted `LEG.COL` —
// becomes `ofOrdinal(QOV(outerCorr, concat), legStart+idx)`. References to
// anything else (the inner's own alias, enclosing scopes, already-baked
// nodes) pass through untouched. A leg reference whose column is absent from
// the leg's type flips missed (the caller declines).
func (pu *clusterPullUp) bake(v values.Value) values.Value {
	fv, isFV := v.(*values.FieldValue)
	if !isFV || fv.Resolved != nil {
		return v
	}
	var alias, col string
	if fv.Child != nil {
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return v
		}
		alias = strings.ToUpper(qov.Correlation.Name())
		col = strings.ToUpper(fv.Field)
	} else if i := strings.IndexByte(fv.Field, '.'); i > 0 {
		alias = strings.ToUpper(fv.Field[:i])
		col = strings.ToUpper(fv.Field[i+1:])
	} else {
		return v
	}
	leg, isLeg := pu.legByAlias[alias]
	if !isLeg {
		return v
	}
	idx, found := leg.typ.FieldIndex(col)
	if !found {
		pu.missed = true
		return v
	}
	outerQOV := values.NewQuantifiedObjectValueOfType(pu.outerCorr, pu.concatType)
	baked, err := values.NewFieldValueOfOrdinal(outerQOV, leg.start+idx)
	if err != nil {
		pu.missed = true
		return v
	}
	return baked
}

// rebuildInnerWithValues rebuilds the csq's single-source inner chain with fn
// applied to every carried value — COPIES, never in-place mutation (the
// logical tree must survive a decline-and-fallback re-translation unpoisoned).
// The carrier enumeration is EXHAUSTIVE by construction: an inner chain with a
// node kind or subquery rider outside it returns ok=false, and the caller
// treats the chain as unanalyzable (ordinal path declines; the ref collector
// reports non-exhaustive). The white-box carrier pin enumerates these arms so
// a NEW logical node kind fails the pin instead of silently skipping values.
//
// Carriers: Filter.Predicate; Project.ProjectedValues; Aggregate
// .GroupKeyValues/.AggregateOperands/.HavingPredicate; Sort.Keys[].Value;
// Limit.LimitValue. Scan and Distinct carry no values. Joins are excluded by
// the shape-2 innerContainsJoin gate before this runs.
func rebuildInnerWithValues(op logical.LogicalOperator, fn func(values.Value) values.Value) (logical.LogicalOperator, bool) {
	switch o := op.(type) {
	case *logical.LogicalScan:
		return o, true
	case *logical.LogicalFilter:
		if len(o.ExistsSubqueries) > 0 || len(o.ScalarSubqueries) > 0 {
			return nil, false
		}
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		if o.Predicate != nil {
			cp.Predicate = predicates.ReplaceValues(o.Predicate, fn)
		}
		return &cp, true
	case *logical.LogicalProject:
		if len(o.ScalarSubqueries) > 0 || len(o.CorrelatedScalarSubqueries) > 0 {
			return nil, false
		}
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		if len(o.ProjectedValues) > 0 {
			pv := make([]values.Value, len(o.ProjectedValues))
			for i, v := range o.ProjectedValues {
				if v != nil {
					pv[i] = values.Replace(v, fn)
				}
			}
			cp.ProjectedValues = pv
		}
		return &cp, true
	case *logical.LogicalAggregate:
		if len(o.HavingExistsSubqueries) > 0 || len(o.HavingScalarSubqueries) > 0 {
			return nil, false
		}
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		if len(o.GroupKeyValues) > 0 {
			gk := make([]values.Value, len(o.GroupKeyValues))
			for i, v := range o.GroupKeyValues {
				if v != nil {
					gk[i] = values.Replace(v, fn)
				}
			}
			cp.GroupKeyValues = gk
		}
		if len(o.AggregateOperands) > 0 {
			ao := make([]values.Value, len(o.AggregateOperands))
			for i, v := range o.AggregateOperands {
				if v != nil {
					ao[i] = values.Replace(v, fn)
				}
			}
			cp.AggregateOperands = ao
		}
		if o.HavingPredicate != nil {
			cp.HavingPredicate = predicates.ReplaceValues(o.HavingPredicate, fn)
		}
		return &cp, true
	case *logical.LogicalSort:
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		keys := make([]logical.SortKey, len(o.Keys))
		copy(keys, o.Keys)
		for i := range keys {
			if keys[i].Value != nil {
				keys[i].Value = values.Replace(keys[i].Value, fn)
			}
		}
		cp.Keys = keys
		return &cp, true
	case *logical.LogicalLimit:
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		if o.LimitValue != nil {
			cp.LimitValue = values.Replace(o.LimitValue, fn)
		}
		return &cp, true
	case *logical.LogicalDistinct:
		in, ok := rebuildInnerWithValues(o.Input, fn)
		if !ok {
			return nil, false
		}
		cp := *o
		cp.Input = in
		return &cp, true
	default:
		return nil, false
	}
}

// collectClusterOuterRefs reports which OUTER-subtree source aliases the inner
// chain references (skipping the inner's own alias — SQL scoping shadows it).
// exhaustive=false when the chain contains carriers outside the
// rebuildInnerWithValues enumeration; the refs found up to that point are
// still definite (sound for the decline guard, which fails toward today's
// behavior when the walk is incomplete).
func collectClusterOuterRefs(op logical.LogicalOperator, outerAliases map[string]struct{}, skip string) (map[string]struct{}, bool) {
	refs := map[string]struct{}{}
	record := func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Resolved != nil {
			return v
		}
		var alias string
		if fv.Child != nil {
			qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
			if !isQOV {
				return v
			}
			alias = strings.ToUpper(qov.Correlation.Name())
		} else if i := strings.IndexByte(fv.Field, '.'); i > 0 {
			alias = strings.ToUpper(fv.Field[:i])
		} else {
			return v
		}
		if alias == skip {
			return v
		}
		if _, isOuter := outerAliases[alias]; isOuter {
			refs[alias] = struct{}{}
		}
		return v
	}
	_, exhaustive := rebuildInnerWithValues(op, record)
	return refs, exhaustive
}

// outerSubtreeAliases collects every source alias the outer subtree binds
// (scans by alias-or-table, unnest AS/AT aliases, CTE names) — the universe
// the decline guard tests inner references against. Best-effort by design:
// the guard's conservative direction on a missed alias is "cannot prove a
// non-rightmost correlation → keep today's behavior".
func outerSubtreeAliases(op logical.LogicalOperator) map[string]struct{} {
	out := map[string]struct{}{}
	var walk func(logical.LogicalOperator)
	walk = func(op logical.LogicalOperator) {
		if op == nil {
			return
		}
		switch o := op.(type) {
		case *logical.LogicalScan:
			a := o.Alias
			if a == "" {
				a = o.Table
			}
			out[strings.ToUpper(a)] = struct{}{}
		case *logical.LogicalUnnest:
			if o.Alias != "" {
				out[strings.ToUpper(o.Alias)] = struct{}{}
			}
			if o.AtAlias != "" {
				out[strings.ToUpper(o.AtAlias)] = struct{}{}
			}
		case *logical.LogicalCTE:
			out[strings.ToUpper(o.Name)] = struct{}{}
		}
		for _, c := range op.Children() {
			walk(c)
		}
	}
	walk(op)
	return out
}

// clusteredOuterOrdinalSeed builds the level-2 ordinal seed for a gated
// clustered outer: ONE full ordinal run over the fresh concat QOV — fields
// named DOTTED `LEG.COL` per gathered leg, so the level-2 output row stays
// name-addressable for the flat projection reads above (the shipped W4b
// mechanism) — then the single nullable inner scalar leg at ordinal 0, named
// exactly `INNER.SCALARCOL` (what replaceScalarSubqueryRef reads).
func clusteredOuterOrdinalSeed(pu *clusterPullUp, innerAlias, scalarCol string) values.Value {
	outerQOV := values.NewQuantifiedObjectValueOfType(pu.outerCorr, pu.concatType)
	var fields []values.RecordConstructorField
	for _, leg := range pu.legs {
		for i := range leg.typ.Fields {
			fv, err := values.NewFieldValueOfOrdinal(outerQOV, leg.start+i)
			if err != nil {
				return nil // decline
			}
			fields = append(fields, values.RecordConstructorField{
				Name:  leg.alias + "." + strings.ToUpper(leg.typ.Fields[i].Name),
				Value: fv,
			})
		}
	}
	// Same inner-leg construction as the single-source seed: the scalar's
	// concrete type is untyped at translation; nullability (LEFT-OUTER
	// null-fill) is the only type property that matters.
	innerType := &values.RecordType{Fields: []values.Field{
		{Name: scalarCol, FieldType: values.WithNullability(values.UnknownType, true), Ordinal: 0},
	}}
	innerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(innerAlias), innerType)
	innerFV, err := values.NewFieldValueOfOrdinal(innerQOV, 0)
	if err != nil {
		return nil // decline
	}
	fields = append(fields, values.RecordConstructorField{
		Name:  strings.ToUpper(innerAlias) + "." + scalarCol,
		Value: innerFV,
	})
	rc := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(rc) // output-contract tripwire (code-bug only)
	return rc
}

// clusterProjectionsResolvable reports whether EVERY top-level projection
// resolves against the dotted seed output: the csq reference itself (rewritten
// to `INNER.SCALARCOL` by replaceScalarSubqueryRef), or flat dotted `LEG.COL`
// reads. Bare names and resolver-anchored (QOV-child) references do not
// resolve over the level-2 output row — the ordinal path declines and the
// query keeps today's behavior (design ruling (v)).
func clusterProjectionsResolvable(p *logical.LogicalProject, csq logical.CorrelatedScalarSubquery, pu *clusterPullUp, innerKey string) bool {
	for i := range p.Projections {
		var v values.Value
		if i < len(p.ProjectedValues) {
			v = p.ProjectedValues[i]
		}
		if v != nil {
			ok := true
			values.WalkValue(v, func(node values.Value) bool {
				switch n := node.(type) {
				case *values.ScalarSubqueryValue:
					if n.Alias != csq.Alias {
						ok = false // a second (uncorrelated) subquery — fallback
					}
					return false
				case *values.FieldValue:
					if n.Resolved != nil || n.Child != nil || !clusterFieldResolvable(n.Field, pu, innerKey) {
						ok = false
					}
					return false
				}
				return true
			})
			if !ok {
				return false
			}
			continue
		}
		if i < len(p.IsComputed) && p.IsComputed[i] {
			return false // walker declined the expression — nothing to resolve
		}
		if !clusterFieldResolvable(p.Projections[i], pu, innerKey) {
			return false
		}
	}
	return true
}

// clusterFieldResolvable resolves one flat field name against the dotted seed
// output: the inner scalar key, or `LEG.COL` through the leg spans.
func clusterFieldResolvable(field string, pu *clusterPullUp, innerKey string) bool {
	f := strings.ToUpper(field)
	if f == innerKey {
		return true
	}
	i := strings.IndexByte(f, '.')
	if i <= 0 {
		return false
	}
	leg, isLeg := pu.legByAlias[f[:i]]
	if !isLeg {
		return false
	}
	_, found := leg.typ.FieldIndex(f[i+1:])
	return found
}

// translateClusteredOuterScalar is the W4b shape-1 dispatch, run BEFORE the
// single-source/name-model paths. Returns (expr, false) when the ordinal path
// translated the query; (nil, true) to DECLINE the whole query (a known
// non-rightmost correlation that did not ordinalize would silently NULL or
// mis-plan on the name model — CORRECT or LOUD, never silent); (nil, false)
// to fall through to the existing paths (single-source outers, and
// rightmost-only correlation which the name model handles correctly).
func (t *cascadesTranslator) translateClusteredOuterScalar(p *logical.LogicalProject, csq logical.CorrelatedScalarSubquery) (expressions.RelationalExpression, bool) {
	j := peelToClusterJoin(p.Input)
	if j == nil {
		return nil, false
	}
	// An ENCLOSED csq select (itself a leg of a name-model merge) stays on the
	// name-model path: an ordinal box under a name-model parent merge would mix
	// positional and dotted rows. Residual until the enclosing parents
	// ordinalize (W5 / W4-left / S4).
	if t.inInnerCluster {
		return nil, false
	}

	rightmost := strings.ToUpper(sourceAlias(p.Input))
	innerAliasU := strings.ToUpper(csq.InnerAlias)
	refs, exhaustive := collectClusterOuterRefs(csq.InnerPlan, outerSubtreeAliases(p.Input), innerAliasU)
	nonRightmost := false
	for a := range refs {
		if a != rightmost {
			nonRightmost = true
		}
	}

	// Enclosure-free gate probe — the ordinalEligible join-arm pattern
	// (side-effect-free Decide with enclosure forced false).
	prev := t.inInnerCluster
	t.inInnerCluster = false
	d := t.ordinalWedgeGateDecide(j)
	t.inInnerCluster = prev

	if d.Gated && exhaustive && !innerContainsJoin(csq.InnerPlan) {
		if sel := t.buildClusteredOuterOrdinalScalar(p, csq, j); sel != nil {
			return sel, false
		}
	}
	if nonRightmost {
		return nil, true
	}
	return nil, false
}

// buildClusteredOuterOrdinalScalar performs the full ordinal construction for
// a gated clustered outer. nil = decline (the dispatch then applies the
// CORRECT-or-LOUD policy).
func (t *cascadesTranslator) buildClusteredOuterOrdinalScalar(p *logical.LogicalProject, csq logical.CorrelatedScalarSubquery, j *logical.LogicalJoin) expressions.RelationalExpression {
	scalarCol := strings.ToUpper(csq.ScalarCol)
	if scalarCol == "" || csq.InnerAlias == "" {
		return nil
	}
	pu := t.buildClusterPullUp(j)
	if pu == nil {
		return nil
	}
	if _, collide := pu.legByAlias[strings.ToUpper(csq.InnerAlias)]; collide {
		// The inner's alias shadows a cluster leg — a value-level reference to
		// that alias is ambiguous between the scopes. Decline.
		return nil
	}
	innerKey := strings.ToUpper(csq.InnerAlias) + "." + scalarCol
	if !clusterProjectionsResolvable(p, csq, pu, innerKey) {
		return nil
	}

	// Java Value.pullUp: bake ALL leg refs (including the rightmost) onto the
	// level-2 concat QOV — rewritten copies, never mutation.
	bakedInner, ok := rebuildInnerWithValues(csq.InnerPlan, pu.bake)
	if !ok || pu.missed {
		return nil
	}

	// Enclosure lift: the gated outer translates FRESH so it actually seeds
	// ordinally and flows the positional concat the pull-up assumed.
	prevEnclosure := t.inInnerCluster
	t.inInnerCluster = false
	outerRef := t.translateRef(p.Input)
	t.inInnerCluster = prevEnclosure
	if outerRef == nil {
		return nil
	}
	outerQ := expressions.NamedForEachQuantifier(pu.outerCorr, outerRef)

	// Inner: identical limit-peel + strict-single treatment to the name-model
	// path (translateProjectWithCorrelatedScalar), over the BAKED copy.
	innerPlan := bakedInner
	var innerLimit *logical.LogicalLimit
	if lim, isLim := innerPlan.(*logical.LogicalLimit); isLim {
		innerLimit = lim
		innerPlan = lim.Input
	}
	innerRef := t.translateRef(innerPlan)
	if innerRef == nil {
		return nil
	}
	if innerLimit != nil {
		limitQ := t.namedQuantifier(sourceAlias(innerPlan), innerRef)
		limitExpr := newLimitExprFromLogical(innerLimit, limitQ)
		innerRef = expressions.InitialOf(limitExpr)
	}
	var innerQ expressions.Quantifier
	if csq.StrictSingle {
		innerQ = expressions.NamedForEachStrictSingleQuantifier(
			values.NamedCorrelationIdentifier(csq.InnerAlias), innerRef)
	} else {
		innerQ = t.namedQuantifier(csq.InnerAlias, innerRef)
	}

	seed := clusteredOuterOrdinalSeed(pu, csq.InnerAlias, scalarCol)
	if seed == nil {
		return nil
	}
	joinSelect := expressions.NewSelectExpressionWithJoinType(
		seed,
		[]expressions.Quantifier{outerQ, innerQ},
		nil,
		[]string{pu.outerCorr.Name(), csq.InnerAlias},
		expressions.JoinLeftOuter,
	)
	joinRef := expressions.InitialOf(joinSelect)

	projected := make([]values.Value, len(p.Projections))
	innerCorr := values.NamedCorrelationIdentifier(csq.InnerAlias)
	for i, col := range p.Projections {
		if i < len(p.ProjectedValues) && p.ProjectedValues[i] != nil {
			projected[i] = replaceScalarSubqueryRef(p.ProjectedValues[i], csq, innerCorr)
			continue
		}
		projected[i] = &values.FieldValue{Field: strings.ToUpper(col), Typ: values.UnknownType}
	}
	projQ := t.namedQuantifier("", joinRef)
	return expressions.NewLogicalProjectionExpressionWithAliases(
		projected,
		p.Aliases,
		projQ,
	)
}
