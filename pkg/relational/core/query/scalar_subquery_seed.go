package query

import (
	"strings"

	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
	"fdb.dev/pkg/relational/core/query/logical"
)

// scalarSubqueryOrdinalSeed builds the ORDINAL result-value seed for a
// correlated-scalar-subquery-in-projection join. It is the
// ordinal counterpart to the name-model correlated-scalar anchored record,
// which has been deleted — the single-source ordinal seed and the
// clustered-outer ordinal dispatch (translateClusteredOuterScalar) between them
// own every reachable correlated-scalar shape. Emitted ONLY when the caller has
// verified the OUTER leg is a SINGLE SOURCE (clusterArity(p.Input)==1): a
// multi-table outer cluster is owned by the clustered-outer ordinal dispatch.
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
// did).
//
// Returns nil (DECLINE — the caller loud-declines; the name-model fallback has
// been retired) when the outer leg is untranslatable. The
// AssertOrdinalJoinSeed on the constructed output is
// the executor-contract tripwire (distinct from the input declines): past the
// shape it can only trip on a code bug, never on declinable input.
//
// innerCorr is the FRESH unique correlation id the inner quantifier carries
// (shape 2 decouples it from the SQL alias — Java mints a globally-unique
// CorrelationIdentifier per quantifier, Quantifier.java:175/842): the seed's
// typed 1-field inner leg is keyed by it, NEVER by the SQL alias. A JOIN-inner
// names its first table under the same SQL alias csq.InnerAlias carries, and
// its own ordinal join types QOV(InnerAlias, N-field) during planning — keying
// the seed leg on the SQL alias collided the two at the executor's
// widenLegTypesFromPlan (DIVERGENT baked types). With the unique id the two
// correlations are distinct and JOIN-inners ordinalize. The RC field NAME
// stays <innerAlias>.<scalarCol> — the name-compat plumbing the projection
// reads (replaceScalarSubqueryRef) — only the correlation key is decoupled.
func (t *cascadesTranslator) scalarSubqueryOrdinalSeed(outerAlias string, outerOp logical.LogicalOperator, innerCorr values.CorrelationIdentifier, innerAlias, scalarCol string) values.Value {
	outerType := t.ordinalLegType(outerOp)
	if outerType == nil || len(outerType.Fields) == 0 {
		return nil // decline → caller loud-declines
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
	// types are untyped here), so the leg field is UnknownType;
	// nullability is the only type property that matters (LEFT-OUTER null-fill).
	// The reported result-set column TYPE is recovered from the inner plan later
	// by deriveColumnsFromFlatMap (Go quantifier flowed types are untyped here).
	innerType := &values.RecordType{Fields: []values.Field{
		{Name: scalarCol, FieldType: values.WithNullability(values.UnknownType, true), Ordinal: 0},
	}}
	innerQOV := values.NewQuantifiedObjectValueOfType(innerCorr, innerType)
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

// (Shape 2) The former innerContainsJoin gate is GONE with its
// premise: it declined JOIN-inners because the seed's inner leg was keyed by
// the SQL alias csq.InnerAlias — the same alias the join-inner's FIRST table
// carries — and the two typed QOVs (1-field seed leg vs N-field internal join
// leg) collided at the executor's widenLegTypesFromPlan ("DIVERGENT baked
// types"). The inner leg is now keyed by a FRESH unique correlation id (Java
// mints a unique CorrelationIdentifier per
// quantifier), so the collision is structurally impossible and JOIN-inners
// ordinalize like any other inner shape.

// (Shape 3) The former innerScalarIsRowColumn / innerHasAggregate
// gate is GONE: a COMPUTED scalar is now MATERIALIZED as the inner subquery's
// projected output (buildCorrelatedScalar, positional `_0`), so the scalar is
// ALWAYS present in the inner row — plain column, aggregate output, or projected
// computation alike — and the single inner leg reads ofOrdinal(inner, 0)
// unconditionally. The old guard declined computed scalars to the name model,
// where they resolved to nothing (a silent NULL); the materialization fixes that
// AND lets them ordinalize.
