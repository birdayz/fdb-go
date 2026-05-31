package predicates

import (
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer/query/plan/cascades/values"
)

// TransformRowNumberDistanceRankMaybe ports Java
// RowNumberValue.transformComparisonMaybe.
//
// A comparison of the shape
//
//	ROW_NUMBER() OVER (PARTITION BY ... ORDER BY <distance>(field, queryVec)) {<,<=,=} K
//
// is rewritten into a ComparisonPredicate (Java's ValuePredicate) whose LHS
// is the distance-specialized row-number value (Euclidean / EuclideanSquare /
// Cosine / DotProduct) and whose RHS is a DistanceRank comparison capturing
// the query vector + K (+ the HNSW ef_search / return-vectors knobs). This is
// the shape the vector index match candidate recognizes and lowers to an HNSW
// K-NN scan.
//
// Returns (nil, false) when the pattern doesn't match — the ROW_NUMBER
// argument is not a single distance function, or the comparison is not one of
// =, <, <= — in which case the caller keeps the original comparison (Java
// returns Optional.empty()).
//
// Layering note: Java places this method on RowNumberValue itself; in Go the
// `values` package cannot import `predicates` (predicates depends on values),
// so the transform lives here as a function over a *values.RowNumberValue.
func TransformRowNumberDistanceRankMaybe(rn *values.RowNumberValue, cmpType ComparisonType, comparand values.Value) (*ComparisonPredicate, bool) {
	if rn == nil {
		return nil, false
	}
	// Window definition with more than one argument is too complicated to
	// adjust — bail out (Java: getArgumentValues().size() > 1).
	if len(rn.ArgumentValues) != 1 {
		return nil, false
	}
	dv, ok := rn.ArgumentValues[0].(*values.DistanceValue)
	if !ok {
		return nil, false
	}

	var drType ComparisonType
	switch cmpType {
	case ComparisonEquals:
		drType = ComparisonDistanceRankEquals
	case ComparisonLessThan:
		drType = ComparisonDistanceRankLessThan
	case ComparisonLessThanOrEq:
		drType = ComparisonDistanceRankLessThanOrEq
	default:
		return nil, false
	}

	// Distance(indexVector, queryVector): the indexed vector is a FieldValue
	// (the column), the query vector is a constant. Java accepts either
	// argument order (Distance(field, q) or Distance(q, field)); normalize so
	// the FieldValue is the index vector.
	indexVector := dv.LeftChild
	queryVector := dv.RightChild
	if !isFieldValue(indexVector) && isFieldValue(queryVector) {
		indexVector, queryVector = queryVector, indexVector
	}
	cmp := NewDistanceRankComparison(drType, queryVector, comparand, rn.EfSearch, rn.IsReturningVectors)

	var lhs values.Value
	switch dv.Operator {
	case values.DistanceEuclidean:
		lhs = values.NewEuclideanDistanceRowNumberValue(rn.PartitioningValues, []values.Value{indexVector})
	case values.DistanceEuclideanSquare:
		lhs = values.NewEuclideanSquareDistanceRowNumberValue(rn.PartitioningValues, []values.Value{indexVector})
	case values.DistanceCosine:
		lhs = values.NewCosineDistanceRowNumberValue(rn.PartitioningValues, []values.Value{indexVector})
	case values.DistanceDotProduct:
		lhs = values.NewDotProductDistanceRowNumberValue(rn.PartitioningValues, []values.Value{indexVector})
	default:
		return nil, false
	}

	return NewComparisonPredicate(lhs, cmp), true
}

// isFieldValue reports whether v is a column reference (the indexed vector
// side of a distance function, as opposed to the constant query vector).
func isFieldValue(v values.Value) bool {
	_, ok := v.(*values.FieldValue)
	return ok
}
