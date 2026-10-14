package client

import "context"

// getRange runs one RYW range read synchronously: issue, then fetch and merge.
func (c *rywCache) getRange(
	ctx context.Context,
	begin, end []byte,
	limit int,
	byteTarget int,
	reverse bool,
	serverGetRange func(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, error),
) ([]KeyValue, bool, error) {
	r := &rywRangeRead{}
	c.issueRangeInto(r, begin, end, limit, byteTarget, reverse, false)
	return r.run(ctx, withoutReadThrough(serverGetRange))
}

// withoutReadThrough adapts a storage fake that never reports readThrough.
func withoutReadThrough(serverGetRange func(ctx context.Context, begin, end []byte, limit int, byteTarget int, reverse bool) ([]KeyValue, bool, error)) rangeServerFunc {
	return func(ctx context.Context, begin, end []byte, limit, byteTarget int, reverse bool) ([]KeyValue, bool, []byte, error) {
		kvs, more, err := serverGetRange(ctx, begin, end, limit, byteTarget, reverse)
		return kvs, more, nil, err
	}
}
