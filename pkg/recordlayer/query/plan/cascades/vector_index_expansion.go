package cascades

import (
	"fdb.dev/pkg/recordlayer/query/plan/cascades/expressions"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/predicates"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// ExpandVectorIndex builds the match-candidate Traversal for a VECTOR (HNSW)
// index. Unlike ExpandValueIndex (one FieldValue placeholder per column), the
// vector expansion splits the index columns at the partition/vector boundary:
//
//   - each partition (key) column → an ordinary Placeholder(FieldValue(col)),
//     which binds the equality prefix that selects the HNSW partition;
//   - the single vector (value) column → one *distance placeholder* whose value
//     is the metric-specific DistanceRowNumberValue(partitionFields, [vecField]).
//     The query's QUALIFY ROW_NUMBER() OVER(... ORDER BY <distance>) predicate
//     lowers (transformComparisonMaybe) to exactly that value, so it binds by
//     structural match.
//
// Ports Java's VectorIndexExpansionVisitor.expand / createDistanceValuePlaceholder.
func ExpandVectorIndex(c *VectorIndexScanMatchCandidate) *Traversal {
	columnNames := c.columnNames
	recordTypes := c.recordTypes

	scan := expressions.NewFullUnorderedScanExpression(recordTypes, values.UnknownType)
	baseQuantifier := expressions.ForEachQuantifier(expressions.InitialOf(scan))
	baseAlias := baseQuantifier.GetAlias()

	builder := NewGraphExpansionBuilder()
	builder.AddQuantifier(baseQuantifier)

	// Partition (key) columns → FieldValue placeholders.
	partFields := make([]values.Value, 0, c.partitionCount)
	for i := 0; i < c.partitionCount && i < len(columnNames) && i < len(c.parameters); i++ {
		fv := values.NewFieldValue(
			values.NewQuantifiedObjectValue(baseAlias),
			columnNames[i], values.UnknownType,
		)
		partFields = append(partFields, fv)
		ph := predicates.NewPlaceholder(c.parameters[i], fv)
		builder.AddPredicate(ph)
		builder.AddPlaceholder(ph)
	}

	// Vector (value) column → the distance placeholder.
	if c.partitionCount < len(columnNames) {
		vecField := values.NewFieldValue(
			values.NewQuantifiedObjectValue(baseAlias),
			columnNames[c.partitionCount], values.UnknownType,
		)
		distValue := newDistanceRowNumberValueForMetric(c.metric, partFields, []values.Value{vecField})
		distPh := predicates.NewPlaceholder(c.distanceAlias, distValue)
		builder.AddPredicate(distPh)
		builder.AddPlaceholder(distPh)
	}

	expansion := builder.Build()
	sealed := expansion.Seal()
	selectExpr := sealed.BuildSelectWithResultValue(baseQuantifier.GetFlowedObjectValue())

	// The ordering the index provides is over the partition aliases (the
	// distance placeholder is not an ordering alias).
	if len(c.orderingAliases) == 0 {
		return NewTraversal(expressions.InitialOf(selectExpr))
	}
	matchableSort := expressions.NewMatchableSortExpressionFromExpr(c.orderingAliases, false, selectExpr)
	return NewTraversal(expressions.InitialOf(matchableSort))
}

// newDistanceRowNumberValueForMetric builds the metric-specific
// DistanceRowNumberValue used as the distance placeholder's value. Mirrors
// Java's createDistanceValuePlaceholder switch on HNSW_METRIC.
func newDistanceRowNumberValueForMetric(metric values.DistanceOperator, partitioningValues, argumentValues []values.Value) values.Value {
	switch metric {
	case values.DistanceCosine:
		return values.NewCosineDistanceRowNumberValue(partitioningValues, argumentValues)
	case values.DistanceDotProduct:
		return values.NewDotProductDistanceRowNumberValue(partitioningValues, argumentValues)
	case values.DistanceEuclideanSquare:
		return values.NewEuclideanSquareDistanceRowNumberValue(partitioningValues, argumentValues)
	default: // DistanceEuclidean
		return values.NewEuclideanDistanceRowNumberValue(partitioningValues, argumentValues)
	}
}
