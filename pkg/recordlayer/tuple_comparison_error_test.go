package recordlayer

import (
	"errors"
	"math/big"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

func TestCompareTuplesCheckedArguments(t *testing.T) {
	t.Parallel()
	for _, bad := range []tuple.Tuple{{struct{}{}}, {tuple.Tuple{int64(1), struct{}{}}}, {(*big.Int)(nil)}} {
		for _, right := range []bool{false, true} {
			a, b := bad, tuple.Tuple{}
			side := "left"
			if right {
				a, b = b, a
				side = "right"
			}
			_, err := CompareTuples(a, b)
			var argument *TupleComparisonError
			if !errors.As(err, &argument) || argument.Side != side || argument.ElementPath == "" || argument.ElementType == "" {
				t.Fatalf("missing argument context: %#v/%v", argument, err)
			}
		}
	}
	if got, err := CompareTuples(tuple.Tuple{int64(1)}, tuple.Tuple{int64(2)}); err != nil || got != -1 {
		t.Fatalf("valid comparison=%d/%v", got, err)
	}
}
