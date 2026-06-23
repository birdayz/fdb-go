package cascades

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/expressions"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/predicates"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// columnValueProvider is implemented by match candidates whose key columns are
// not all bare fields — they supply the per-column match Value (e.g. a
// CARDINALITY()-keyed column yields CardinalityValue(FieldValue(col))). The
// base argument is the QuantifiedObjectValue of the index's record source.
// Candidates that don't implement it default to FieldValue(base, col).
type columnValueProvider interface {
	ColumnValue(i int, base values.Value) values.Value
}

// ExpandValueIndex builds a Traversal from an index definition,
// producing a candidate expression tree with Placeholder predicates
// for each index column. The resulting Traversal is used by matching
// rules to match query predicates against index columns.
//
// The output structure matches Java's ValueIndexExpansionVisitor:
//
//	MatchableSortExpression(sortParamIDs, isReverse=false,
//	  SelectExpression(resultValue,
//	    [ForEach(FullUnorderedScanExpression(recordTypes))],
//	    [Placeholder(param0, FieldValue("col0")),
//	     Placeholder(param1, FieldValue("col1")),
//	     ...]))
//
// Go's simpler index column model (flat list of column names) replaces
// Java's KeyExpression visitor walk, but the output Traversal structure
// is identical.
func ExpandValueIndex(candidate MatchCandidate) *Traversal {
	columnNames := candidate.GetColumnNames()
	sargableAliases := candidate.GetSargableAliases()
	recordTypes := candidate.GetRecordTypes()

	// Base scan: FullUnorderedScanExpression over the candidate's record types.
	scan := expressions.NewFullUnorderedScanExpression(recordTypes, values.UnknownType)
	baseQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(scan))

	// Build the graph expansion: one Placeholder per index column.
	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(baseQuantifier)

	// columnNames and sargableAliases are parallel slices; iterate over
	// sargableAliases as the authoritative length (callers that pass nil
	// sargableAliases get zero placeholders).
	//
	// The per-column placeholder Value is normally FieldValue(base, col). A
	// candidate that carries a function-keyed column (e.g. a CARDINALITY()
	// index) overrides this via columnValueProvider so the placeholder Value
	// is CardinalityValue(FieldValue(base, col)) — the SAME Value the query
	// side builds, so the predicate (and, via the same provider, the sort)
	// binds by Value-tree equality. Mirrors Java's match candidate carrying
	// the column's Value (CardinalityFunctionKeyExpression.toValue()).
	baseAlias := baseQuantifier.GetAlias()
	provider, _ := candidate.(columnValueProvider)
	for i, alias := range sargableAliases {
		var colValue values.Value
		if provider != nil {
			colValue = provider.ColumnValue(i, values.NewQuantifiedObjectValue(baseAlias))
		} else {
			colValue = values.NewFieldValue(
				values.NewQuantifiedObjectValue(baseAlias),
				columnNames[i], values.UnknownType,
			)
		}
		ph := predicates.NewPlaceholder(alias, colValue)
		builder.AddPredicate(ph)
		builder.AddPlaceholder(ph)
	}

	expansion := builder.Build()
	sealed := expansion.Seal()

	// Build SelectExpression with the base quantifier's flowed object value
	// as the result value. The sealed expansion must have no result columns
	// (we only added predicates/placeholders, no columns), so
	// BuildSelectWithResultValue is the right call.
	selectExpr := sealed.BuildSelectWithResultValue(baseQuantifier.GetFlowedObjectValue())

	// Wrap in MatchableSortExpression — the sort is defined by the
	// sargable aliases (one per index key column), not reversed.
	matchableSort := expressions.NewMatchableSortExpressionFromExpr(
		sargableAliases,
		false,
		selectExpr,
	)

	return NewTraversal(expressions.InitialOf(matchableSort))
}
