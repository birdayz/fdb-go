package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// scalarSubqueryOrdinalSeed builds the ORDINAL result-value seed for a
// correlated-scalar-subquery-in-projection join (RFC-173 W4b), replacing the
// name-model NewScalarSubqueryAnchoredRecord so the seed survives the S4
// name-model deletion. Emitted ONLY when the caller has verified the OUTER leg
// is a SINGLE SOURCE (clusterArity(p.Input)==1): ordinalizing a flattened
// multi-table outer cluster to a bare positional concat erases the buried
// source names, so a clustered outer stays name-model.
//
// Shape (Java LogicalOperator.convertToExpressions — ofOrdinalNumber per flowed
// column): the OUTER leg's columns become ofOrdinal(QOV(outer), 0..n-1), and the
// INNER scalar subquery — which exposes exactly ONE value — becomes a single
// ofOrdinal(QOV(inner), 0). The inner is the LEFT-OUTER null-supplying leg, so
// its ordinal is NULLABLE-wrapped (an outer row with no inner match yields NULL;
// the executor's null-leg birth supplies it, no executor change). The inner RC
// field is named EXACTLY <inner>.<scalarCol> — what replaceScalarSubqueryRef
// emits and composeFieldOverConstructor folds — and, unlike the name-model
// anchored record, the ordinal seed emits ONE field per column (no bare+dotted
// duplicates: AssertOrdinalJoinSeed forbids duplicate ordinals; the outer leg's
// RecordName carries the source table so a table-qualified reference still
// resolves through the span type, exactly as the name model's qualified form
// did). The unlike-W4-LEFT asymmetry: only the OUTER is single-source-gated —
// the inner contributes one positional scalar regardless of its own internal
// structure, so an internally-joining scalar subquery still ordinalizes (gating
// it would strand such subqueries name-model → dead at S4).
//
// Returns nil (DECLINE — the caller falls back to the name model) when the outer
// leg is untranslatable. The AssertOrdinalJoinSeed on the constructed output is
// the executor-contract tripwire (distinct from the input declines): past the
// shape it can only trip on a code bug, never on declinable input.
// innerContainsJoin reports whether a correlated scalar subquery's inner plan
// contains a JOIN at any depth. This is the INNER gate the seed needs, and it
// corrects the original W4b "inner needs no gate" ruling. A join-inner names its
// FIRST table under the SAME alias csq.InnerAlias carries (sq.tableAlias), and
// that inner join ordinalizes into a typed QOV(InnerAlias, N-field) leg. The
// ordinal seed would ALSO type the inner scalar leg as QOV(InnerAlias, 1-field)
// — two divergent typed QOVs for one alias, which the executor's
// widenLegTypesFromPlan type-consistency check rejects ("leg X carries DIVERGENT
// baked types"). The name model dodges it by referencing the inner through an
// UNTYPED QOV. clusterArity cannot see this: it returns 1 for LogicalAggregate
// without recursing into the join beneath a COUNT(*)/SUM(...). So a join-inner
// stays name-model; the seed field-structure argument was sound, but the
// type-consistency machinery keys on the shared alias, not the seed field.
func innerContainsJoin(op logical.LogicalOperator) bool {
	if op == nil {
		return false
	}
	if _, ok := op.(*logical.LogicalJoin); ok {
		return true
	}
	for _, c := range op.Children() {
		if innerContainsJoin(c) {
			return true
		}
	}
	return false
}

func (t *cascadesTranslator) scalarSubqueryOrdinalSeed(outerAlias string, outerOp logical.LogicalOperator, innerAlias, scalarCol string) values.Value {
	outerType := t.ordinalLegType(outerOp)
	if outerType == nil || len(outerType.Fields) == 0 {
		return nil // decline → name-model fallback
	}

	var fields []values.RecordConstructorField

	// OUTER leg: ofOrdinal(QOV(outer), i), leg-relative ordinal 0..n-1.
	outerQOV := values.NewQuantifiedObjectValueOfType(values.NamedCorrelationIdentifier(outerAlias), outerType)
	for i := range outerType.Fields {
		fv, err := values.NewFieldValueOfOrdinal(outerQOV, i)
		if err != nil {
			return nil // decline
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.Field, Value: fv})
	}

	// INNER scalar leg: ONE nullable ordinal-0 field named <inner>.<scalarCol>.
	// The scalar's concrete type is untyped at translation (Go quantifier flowed
	// types are untyped — the W4 Q1 ruling), so the leg field is UnknownType;
	// nullability is the only type property that matters (LEFT-OUTER null-fill).
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
