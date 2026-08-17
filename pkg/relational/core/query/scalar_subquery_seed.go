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
// the executor's null-leg build supplies it, no executor change). The inner RC
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
func (t *cascadesTranslator) scalarSubqueryOrdinalSeed(outerAlias string, outerOp logical.LogicalOperator, innerOp logical.LogicalOperator, innerCorr values.CorrelationIdentifier, innerAlias, scalarCol string) values.Value {
	outerType := t.ordinalLegType(outerOp)
	if outerType == nil || len(outerType.Fields) == 0 {
		return nil // decline → caller loud-declines
	}

	var fields []values.RecordConstructorField

	// OUTER leg: ofOrdinal(QOV(outer), i), leg-relative ordinal 0..n-1.
	outerQOV, err := values.NewQuantifiedObjectValue(values.NamedCorrelationIdentifier(outerAlias), outerType)
	if err != nil {
		return nil
	}
	for i := range outerType.Fields {
		resolved, err := values.ResolveOrdinalSeedField(outerQOV, i)
		if err != nil {
			return nil // decline
		}
		fv, ok := values.AsFieldValue(resolved)
		if !ok {
			return nil
		}
		fields = append(fields, values.RecordConstructorField{Name: fv.DisplayName(), Value: fv})
	}

	// INNER scalar leg: ONE nullable ordinal-0 field named <inner>.<scalarCol>.
	// The field's TYPE is the inner subquery's OWN flowed output type, read off
	// the inner logical operator — the same catalog-backed derivation every other
	// leg uses. Java never re-derives a column's type downstream: client metadata
	// is positional over the plan's flowed record type, so the type must be
	// present on the value here (QueryPlan.getResultType →
	// RelationalStructMetaData.of).
	//
	// Minting UnknownType here instead left the type to be re-guessed by NAME at
	// the metadata surface, where a bare-name descriptor search across the join's
	// leaf descriptors first-matches: with two legs declaring the same column
	// name at different types, the scalar's column was reported as the OTHER
	// leg's type (a STRING column served to the client as BIGINT).
	//
	// The inner exposes exactly ONE value (buildCorrelatedScalar materializes a
	// computed scalar as a single projected output), so a derivable inner type
	// has exactly one field. Anything else contradicts that premise — keep the
	// nullable-unknown leg rather than guess which field is the scalar.
	scalarType := scalarColumnType(t.legColumns(innerOp), scalarCol)
	if scalarType == nil || scalarType.Code() == values.TypeCodeUnknown {
		return nil
	}
	scalarType = values.WithNullability(scalarType, true)
	innerTitle := unqualifiedScalarTitle(scalarCol)
	// Nullability is independent of the concrete type and always true: the join
	// is LEFT-OUTER, so an outer row with no inner match yields NULL in this slot.
	// RFC-212 §11.3 — THE RETITLING. The flowed column's title must not be
	// parseable as a qualifier.
	//
	// `scalarCol` is the subquery's OUTPUT TITLE, and when a query titles its
	// scalar with something already qualified (`SELECT ... AS "C.CV"`, or a title
	// derived from a qualified reference) that dotted string becomes this leg
	// type's only column name. It then reaches executor.rowSlotForLegColumn's
	// dotted arm indistinguishable from a leg-qualified reference — the arm
	// splits it at the dot and resolves a LEG named `C` and a column named `CV`,
	// neither of which this leg has. Attribution measured both live witnesses
	// (`C.CV`, `I.QTY`) originating exactly here.
	//
	// The title is a LABEL on a single-field type; the leg's identity is
	// `innerCorr`, threaded in and unique, and nothing is minted. So stripping a
	// qualifier off the label removes the ambiguity without touching any identity.
	// The RC FIELD name below keeps the qualified spelling — that is the row-key
	// arm, which replaceScalarSubqueryRef reads, and it is deliberately not in
	// scope here.
	innerType := &values.RecordType{Nullable: true, Fields: []values.Field{
		{Name: innerTitle, FieldType: scalarType, Ordinal: 0},
	}}
	// RFC-212 §10.3 deliverable 1, SECOND producer: register the (correlation,
	// TITLE) pair so the executor's dotted-arm answers can be attributed BY
	// IDENTITY. This is the SINGLE-SOURCE outer's seed; its multi-table twin
	// (clusteredOuterOrdinalSeed) registers the same way, and the attribution
	// census reports which — if either — named the leg type each dotted answer
	// comes from.
	if values.LegIdentityCensusEnabled() {
		// The title the TYPE carries, not the pre-strip one: "attributed" must mean
		// "this producer named this leg type's column", so registering the old
		// spelling would attribute a title the leg no longer has.
		values.RecordInnerScalarLegTitleAt(values.InnerLegProducerSingleSource, innerCorr, innerTitle)
	}
	innerQOV, err := values.NewQuantifiedObjectValue(innerCorr, innerType)
	if err != nil {
		return nil
	}
	innerFV, err := values.ResolveOrdinalSeedField(innerQOV, 0)
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

// scalarColumnType resolves the correlated scalar's column type within the
// INNER subquery's OWN output columns. This is not a cross-leg name search: the
// inner is a single subquery and scalarCol is the column ITS select list
// exposes, so the resolution is the same one Java's SemanticAnalyzer performs
// against a single operator's output (SemanticAnalyzer.java:417-437, which
// likewise rejects rather than guesses when a name is ambiguous).
//
// Returns UnknownType — leaving the type to be recovered from the physical
// inner plan downstream — in exactly the cases where a type cannot be resolved
// without guessing:
//   - the column is absent (a COMPUTED scalar or aggregate, materialized as a
//     positional output that the inner leg's stored columns do not carry);
//   - the name is AMBIGUOUS within the leg. First-match here would reintroduce
//     the defect this function exists to remove, one level down.
func scalarColumnType(innerCols []values.Field, scalarCol string) values.Type {
	// A materialising projection gives the scalar seed a proven one-field row.
	// Its SQL/display title can contain dots that are not qualification at all
	// (`SUM(C.VAL)`), while the slot identity is already positional.  Taking the
	// only exact field is therefore both stronger and safer than parsing its
	// title.  UNKNOWN still declines at the caller.
	if len(innerCols) == 1 && innerCols[0].FieldType != nil {
		return innerCols[0].FieldType
	}

	// Prefer the complete exact output name.  Join inners expose qualified
	// fields such as C.VAL; stripping the qualifier before this comparison made
	// a perfectly exact field invisible.
	var found values.Type
	for _, c := range innerCols {
		if !strings.EqualFold(c.Name, scalarCol) {
			continue
		}
		if found != nil {
			return values.UnknownType
		}
		found = c.FieldType
	}
	if found != nil {
		return found
	}

	bare := scalarCol
	if i := strings.LastIndex(bare, "."); i >= 0 {
		bare = bare[i+1:]
	}
	found = nil
	for _, c := range innerCols {
		if !strings.EqualFold(c.Name, bare) {
			continue
		}
		if found != nil {
			return values.UnknownType // ambiguous within the leg — never first-match
		}
		found = c.FieldType
	}
	if found == nil {
		return values.UnknownType
	}
	return found
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

// unqualifiedScalarTitle strips a leading qualifier off a correlated scalar's
// output title, so the title cannot be read as `LEG.COL` by a downstream reader
// that splits at the dot.
//
// It takes the LAST segment, not the first, and it leaves a title with no dot
// untouched. A title that is ONLY a dot, or that would strip to empty, is left
// verbatim: an empty column name is worse than an ambiguous one, and this
// function's job is to remove an ambiguity rather than to guarantee a name.
//
// A rendered composite (`{_0: C.ID#0}`) is left alone for the reason
// executor.isDottedQualifiedName states: a rendered record type carries the dots
// of every qualified column inside it, so slicing one would mangle a name that
// was never a qualifier.
func unqualifiedScalarTitle(title string) string {
	if title == "" || strings.HasPrefix(title, "{") || strings.HasPrefix(title, "[") {
		return title
	}
	i := strings.LastIndexByte(title, '.')
	if i <= 0 || i == len(title)-1 {
		return title
	}
	return title[i+1:]
}
