package cascades

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// RFC-173 Slice 3 W4 — post-rewrite LEFT ordinalization. RewriteOuterJoinRule
// dissolves a correlated LEFT box into an INNER select + null-on-empty
// quantifier; W2/W3 left that dissolved shape NAME-MODEL (it yielded the
// anchored RC). W4 ordinalizes it — but ONLY when BOTH legs are SINGLE SOURCES
// (design ruling RFC-173 W4, Q3 + the symmetric-null extension): ordinalizing
// a flattened preserved OR null-supplying cluster to a bare concat ERASES the
// buried names, and the name-model ON-pred can't span into the ordinal leg
// (the RFC-153 correlated-FlatMap-probe hazard, PR-#201-class 0-row). A
// clustered leg on either side stays name-model. The executor's null-leg birth
// (contract ruling #3) supplies the NULL extension for the ordinalized case
// with no executor change.

// singleSourceLeg reports whether a leg quantifier is a SINGLE SOURCE — it
// provides ONLY its own alias, no buried sources. Keying on the provided-set
// SIZE is alias-collision-safe: a cluster's synthetic quantifier alias
// (sourceAlias) COINCIDES with its rightmost leaf's, so an alias-name test is
// ambiguous, but a multi-source cluster always provides ≥2 aliases. Uses the
// same physicalProvidedAliases machinery preservedProvidedAliases delegates to.
func singleSourceLeg(q expressions.Quantifier) bool {
	provided := preservedProvidedAliases(q)
	if len(provided) != 1 {
		return false
	}
	_, ok := provided[q.GetAlias()]
	return ok
}

// ordinalSeedFromAnchoredLeft builds the dissolved LEFT's ordinal seed from the
// box's anchored RC. It walks the anchored RC in FIELD (SQL DECLARATION) order
// — a RIGHT JOIN normalized to LEFT keeps declaration order in the anchored RC
// even though preserved/null are swapped, so following the anchored order keeps
// SELECT */positional output stable (the aliases identify only WHICH leg is
// nullable, not the emission order). Each column becomes a baked
// ofOrdinalNumber over its leg's typed QOV, leg-relative ordinal; the
// null-supplying leg's columns are NULLABLE-wrapped (Java OuterJoinExpression).
// Leg types derive from the anchored RC (the only typed source at rewrite time
// — Go quantifier flowed types are untyped). Returns nil (DECLINE, never panic
// — a rule reconstructing from a name-model RC is decline territory) unless the
// RC decomposes cleanly into EXACTLY the two expected legs.
func ordinalSeedFromAnchoredLeft(anchored *values.RecordConstructorValue, preservedAlias, nullSupplyingAlias values.CorrelationIdentifier) values.Value {
	// Collect each leg's columns in first-appearance (declaration) order,
	// keyed on the dotted ALIAS.COL field whose child QOV correlates to the
	// leg (one per column, unambiguous; the bare last-leg-wins forms are
	// skipped by the prefix test).
	var legOrder []values.CorrelationIdentifier
	legCols := map[values.CorrelationIdentifier][]values.Field{}
	for _, f := range anchored.Fields {
		fv, ok := f.Value.(*values.FieldValue)
		if !ok {
			continue
		}
		qov, ok := fv.Child.(*values.QuantifiedObjectValue)
		if !ok {
			continue
		}
		alias := qov.Correlation
		if !strings.HasPrefix(f.Name, strings.ToUpper(alias.Name())+".") {
			continue
		}
		if _, seenLeg := legCols[alias]; !seenLeg {
			legOrder = append(legOrder, alias)
		}
		legCols[alias] = append(legCols[alias], values.Field{
			Name: strings.ToUpper(fv.Field), FieldType: fv.Typ, Ordinal: len(legCols[alias]),
		})
	}
	// The RC must decompose into EXACTLY the two expected legs — anything else
	// (a clustered leg exposing sub-aliases, a missing leg) declines.
	if len(legOrder) != 2 {
		return nil
	}
	legSet := map[values.CorrelationIdentifier]struct{}{legOrder[0]: {}, legOrder[1]: {}}
	if _, ok := legSet[preservedAlias]; !ok {
		return nil
	}
	if _, ok := legSet[nullSupplyingAlias]; !ok {
		return nil
	}

	var fields []values.RecordConstructorField
	for _, alias := range legOrder {
		cols := legCols[alias]
		if len(cols) == 0 {
			return nil
		}
		if alias == nullSupplyingAlias {
			for i := range cols {
				cols[i].FieldType = values.WithNullability(cols[i].FieldType, true)
			}
		}
		typ := &values.RecordType{Fields: cols}
		qov := values.NewQuantifiedObjectValueOfType(alias, typ)
		for i := range cols {
			fv, err := values.NewFieldValueOfOrdinal(qov, i)
			if err != nil {
				return nil // decline, never panic
			}
			fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
		}
	}
	rc := values.NewRawRecordConstructorValue(fields...)
	// Executor-contract tripwire on the CONSTRUCTED output (distinct from the
	// input-decompose declines above): past the shape checks this can only
	// trip on a code bug, never on declinable input — a cheap regression net
	// on the seed the executor consumes.
	values.AssertOrdinalJoinSeed(rc)
	return rc
}
