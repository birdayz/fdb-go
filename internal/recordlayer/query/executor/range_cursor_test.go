package executor

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer/query/plan/cascades/values"
)

// Java's RangeValue.Cursor: each row's continuation is a
// RangeCursorContinuation naming the next position, and resuming from it
// yields the remainder.
func TestRangeCursorContinuation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rv := values.NewRangeValue(values.LiteralValue(int64(0)), values.LiteralValue(int64(7)), values.LiteralValue(int64(3)))
	drain := func(cont []byte) (ids []int64, conts [][]byte) {
		c, err := newRangeCursor(rv, nil, cont)
		require.NoError(t, err)
		for {
			r, err := c.OnNext(ctx)
			require.NoError(t, err)
			if !r.HasNext() {
				return ids, conts
			}
			ids = append(ids, r.GetValue().Positional.Slots[0].(int64))
			b, err := r.GetContinuation().ToBytes()
			require.NoError(t, err)
			conts = append(conts, b)
		}
	}
	ids, conts := drain(nil)
	require.Equal(t, []int64{0, 3, 6}, ids)
	var c gen.RangeCursorContinuation
	require.NoError(t, proto.Unmarshal(conts[0], &c))
	require.Equal(t, int64(3), c.GetNextPosition())
	rest, _ := drain(conts[0])
	require.Equal(t, []int64{3, 6}, rest)
	// After the last row the continuation is not yet the end (9 < 7+3).
	require.NotNil(t, conts[2])
	last, _ := drain(conts[2])
	require.Empty(t, last)
}
