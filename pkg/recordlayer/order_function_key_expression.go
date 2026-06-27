package recordlayer

import (
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// Order function names matching Java's OrderFunctionKeyExpressionFactory.
// Each name maps to a TupleOrdering.Direction encoding strategy.
const (
	OrderFuncAscNullsFirst  = "order_asc_nulls_first"
	OrderFuncAscNullsLast   = "order_asc_nulls_last"
	OrderFuncDescNullsFirst = "order_desc_nulls_first"
	OrderFuncDescNullsLast  = "order_desc_nulls_last"
)

func init() {
	RegisterFunction(OrderFuncAscNullsFirst, makeOrderEvaluator(OrderAscNullsFirst))
	RegisterFunction(OrderFuncAscNullsLast, makeOrderEvaluator(OrderAscNullsLast))
	RegisterFunction(OrderFuncDescNullsFirst, makeOrderEvaluator(OrderDescNullsFirst))
	RegisterFunction(OrderFuncDescNullsLast, makeOrderEvaluator(OrderDescNullsLast))
}

// OrderFuncExpr creates a FunctionKeyExpression for ordering with the given
// direction name and arguments. Convenience wrapper matching Java's
// OrderFunctionKeyExpression constructor.
func OrderFuncExpr(direction string, arguments KeyExpression) *FunctionKeyExpression {
	return FunctionExpr(direction, arguments)
}

// makeOrderEvaluator creates a FunctionEvaluator that encodes tuple values
// according to the given order direction. Each argument tuple is packed into
// bytes using TupleOrdering encoding, which is then stored as the index key
// value (a byte string element in the FDB tuple).
//
// Matches Java's OrderFunctionKeyExpression.evaluateFunction():
//
//	return Collections.singletonList(Key.Evaluated.scalar(
//	    ZeroCopyByteString.wrap(TupleOrdering.pack(arguments.toTuple(), direction))));
func makeOrderEvaluator(dir OrderDirection) FunctionEvaluator {
	return func(_ *FDBStoredRecord[proto.Message], _ proto.Message, arguments [][]any) ([][]any, error) {
		results := make([][]any, 0, len(arguments))
		for _, args := range arguments {
			t := make(tuple.Tuple, len(args))
			for i, a := range args {
				t[i] = a
			}
			packed := tupleOrderingPack(t, dir)
			results = append(results, []any{packed})
		}
		return results, nil
	}
}
