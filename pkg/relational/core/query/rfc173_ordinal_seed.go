package query

import (
	"strings"

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
	return &values.RecordType{RecordName: t.legScanTableName(op), Fields: fields}
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
	switch op.(type) {
	case *logical.LogicalJoin:
		// Unreachable when the gate is correct: join legs are categorically
		// INELIGIBLE in the Slice 2 wedge (ordinalEligible — a nested ordinal
		// box's bare concat erases buried aliases; nesting is S3's collapsed
		// FieldPath work). Loud, never a silently mis-typed leg.
		panic("RFC-173 ordinal seed: a JOIN leg reached the ordinal seed — join legs are ineligible in the S2 wedge (cluster-arity gate mis-scope, planner bug)")
	default:
		return t.legColumns(op)
	}
}

// buildOrdinalJoinResultValue builds the gated 2-way join's result value: the
// raw RC of ofOrdinalNumber references over the two legs' typed QOVs, in
// DECLARATION order (the caller passes rvLeft/rvRight — `SELECT *` column
// order follows the SQL FROM order, not the RIGHT-join execution swap; the
// executor binds legs by ALIAS, so RC leg order is independent of cursor
// outer/inner roles). Returns nil when a leg is untranslatable (same rule as
// the anchored seed). The seed shape is asserted loud
// (values.AssertOrdinalJoinSeed — Torvalds' standing condition on W3b).
// The returned legTypes map (UPPER alias → leg RecordType) feeds
// bakeGatedJoinPredicates at the seed and the WHERE-merge site.
func (t *cascadesTranslator) buildOrdinalJoinResultValue(left, right logical.LogicalOperator, leftAlias, rightAlias string) (values.Value, map[string]*values.RecordType) {
	if leftAlias == "" || rightAlias == "" {
		return nil, nil
	}
	leftType := t.ordinalLegType(left)
	rightType := t.ordinalLegType(right)
	if leftType == nil || rightType == nil {
		return nil, nil
	}
	var fields []values.RecordConstructorField
	legTypes := make(map[string]*values.RecordType, 2)
	for _, leg := range []struct {
		alias string
		typ   *values.RecordType
	}{{leftAlias, leftType}, {rightAlias, rightType}} {
		legTypes[strings.ToUpper(leg.alias)] = leg.typ
		qov := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(leg.alias), leg.typ)
		for i := range leg.typ.Fields {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				// Impossible by construction (the ordinal ranges over the
				// type's own fields) — loud, matching the seed assert below.
				panic("RFC-173 ordinal seed: " + err.Error())
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(rc)
	return rc, legTypes
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
func bakeGatedJoinPredicates(preds []predicates.QueryPredicate, legTypes map[string]*values.RecordType) []predicates.QueryPredicate {
	if len(preds) == 0 || len(legTypes) == 0 {
		return preds
	}
	bake := func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || fv.Resolved != nil || strings.Contains(fv.Field, ".") {
			return v
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return v
		}
		legType, isLeg := legTypes[strings.ToUpper(qov.Correlation.Name())]
		if !isLeg || legType == nil {
			return v
		}
		idx, found := legType.FieldIndex(fv.Field)
		if !found {
			return v
		}
		typedQOV := values.NewQuantifiedObjectValueOfType(qov.Correlation, legType)
		baked, err := values.NewFieldValueOfOrdinal(typedQOV, idx)
		if err != nil {
			panic("RFC-173 predicate bake: " + err.Error()) // FieldIndex guaranteed the range
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
		if predicateLegAliases(p, legTypes) < 2 {
			return p // single-leg or leg-free: stays lazy (pushdown-safe)
		}
		return predicates.ReplaceValues(p, bake)
	}
	out := make([]predicates.QueryPredicate, len(preds))
	for i, p := range preds {
		out[i] = bakeConjuncts(p)
	}
	return out
}

// predicateLegAliases counts how many DISTINCT gated-join leg aliases a
// predicate's value trees reference directly (bare-named lazy FieldValues
// over a leg QOV — the same references the bake rewrites).
func predicateLegAliases(p predicates.QueryPredicate, legTypes map[string]*values.RecordType) int {
	seen := make(map[string]struct{}, 2)
	predicates.ReplaceValues(p, func(v values.Value) values.Value {
		fv, isFV := v.(*values.FieldValue)
		if !isFV || strings.Contains(fv.Field, ".") {
			return v
		}
		qov, isQOV := fv.Child.(*values.QuantifiedObjectValue)
		if !isQOV {
			return v
		}
		key := strings.ToUpper(qov.Correlation.Name())
		if _, isLeg := legTypes[key]; isLeg {
			seen[key] = struct{}{}
		}
		return v
	})
	return len(seen)
}
