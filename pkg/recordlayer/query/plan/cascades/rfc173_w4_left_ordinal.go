package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-173 Slice 3 W4 — post-rewrite LEFT ordinalization. RewriteOuterJoinRule
// dissolves a correlated LEFT box into an INNER select + null-on-empty
// quantifier; W2/W3 left that dissolved shape NAME-MODEL (it yielded the
// anchored RC). W4 ordinalizes it — but ONLY for a TOP-LEVEL-correlated
// dissolved LEFT (the null-supplying ON-preds correlate to the preserved
// quantifier's own alias, not a buried source). The design ruling (RFC-173 W4)
// (rfcs/173-ordinal-column-resolution.md § W4): ordinalizing a flattened
// preserved cluster M=(A JOIN B) to a bare concat ERASES the buried name A,
// and the innerSelect's ON-pred stays name-model with no span into the ordinal
// leg — the RFC-153 correlated-FlatMap-probe path breaks (PR-#201-class 0-row).
// So a BURIED-correlated dissolved LEFT stays name-model (the existing path);
// the executor's null-leg birth (contract ruling #3) supplies the NULL
// extension for the ordinalized case with no executor change.

// leftOrdinalizable reports whether a dissolved LEFT may ordinalize (RFC-173 W4 Q3):
// the preserved leg must be a SINGLE SOURCE — its quantifier provides ONLY its
// own top alias, no buried sources. A MULTI-source preserved cluster
// (`(A JOIN B) LEFT JOIN C`) is never eligible: ordinalizing it to a bare
// concat erases the buried names, and the null-supplying ON-pred stays
// name-model with no span into the ordinal leg (the RFC-153 hazard —
// PR-#201-class 0-row/unresolvable).
//
// Keying on the SET SIZE, not on which alias the ON-pred touches, is the
// alias-collision-safe reading of the ruling: a cluster's synthetic quantifier
// alias (sourceAlias) COINCIDES with its rightmost leaf's alias, so an ON-pred
// touching "B" over `(A JOIN B)` is touching the BURIED leaf B (whose column
// lives in the erased concat), not the cluster as a whole — indistinguishable
// by alias name. Single-source ⟺ preservedProvided == {topAlias} exactly.
// preservedProvided is preservedProvidedAliases(preserved) — {top} ∪ {buried}.
func leftOrdinalizable(_ []predicates.QueryPredicate, topAlias values.CorrelationIdentifier, preservedProvided map[values.CorrelationIdentifier]struct{}) bool {
	if len(preservedProvided) != 1 {
		return false // a multi-source preserved cluster — erasing the buried names is the hazard
	}
	_, ok := preservedProvided[topAlias]
	return ok // defensive: the sole provided alias must be the top
}

// ordinalSeedFromAnchoredLeft builds the dissolved LEFT's ordinal seed from the
// box's anchored RC (RFC-173 W4 Q1: the anchored RC is the only typed source at
// rewrite time — Go quantifier flowed types are untyped). It concatenates the
// preserved leg's columns then the null-supplying leg's, each a baked
// ofOrdinalNumber over the leg's typed QOV; the null-supplying leg's columns
// are NULLABLE-wrapped (RFC-173 W4 Q2 / Java OuterJoinExpression). Returns nil when
// the anchored RC does not cleanly decompose into the two legs (defensive —
// the caller then keeps the name-model shape).
func ordinalSeedFromAnchoredLeft(anchored *values.RecordConstructorValue, preservedAlias, nullSupplyingAlias values.CorrelationIdentifier) values.Value {
	preservedCols := anchoredLegColumns(anchored, preservedAlias)
	nullCols := anchoredLegColumns(anchored, nullSupplyingAlias)
	if len(preservedCols) == 0 || len(nullCols) == 0 {
		return nil
	}
	// The null-supplying leg's columns become nullable — it null-extends.
	for i := range nullCols {
		nullCols[i].FieldType = values.WithNullability(nullCols[i].FieldType, true)
	}
	var fields []values.RecordConstructorField
	appendLeg := func(alias values.CorrelationIdentifier, cols []values.Field) bool {
		typ := &values.RecordType{Fields: cols}
		qov := values.NewQuantifiedObjectValueOfType(alias, typ)
		for i := range cols {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				return false
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
		return true
	}
	if !appendLeg(preservedAlias, preservedCols) || !appendLeg(nullSupplyingAlias, nullCols) {
		return nil
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	values.AssertOrdinalJoinSeed(rc)
	return rc
}

// anchoredLegColumns extracts one Field per column of the given leg from an
// anchored RC, in output order. The anchored RC carries a dotted "ALIAS.COL"
// field per column (one-per-column, unambiguous) plus bare last-leg-wins forms;
// keying on the dotted field whose child QOV correlates to legAlias yields the
// leg's columns exactly once, in order, with their types.
func anchoredLegColumns(anchored *values.RecordConstructorValue, legAlias values.CorrelationIdentifier) []values.Field {
	prefix := strings.ToUpper(legAlias.Name()) + "."
	var cols []values.Field
	for _, f := range anchored.Fields {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		fv, ok := f.Value.(*values.FieldValue)
		if !ok {
			continue
		}
		qov, ok := fv.Child.(*values.QuantifiedObjectValue)
		if !ok || qov.Correlation != legAlias {
			continue
		}
		cols = append(cols, values.Field{Name: strings.ToUpper(fv.Field), FieldType: fv.Typ, Ordinal: len(cols)})
	}
	return cols
}
