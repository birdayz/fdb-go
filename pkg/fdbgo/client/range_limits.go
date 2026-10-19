package client

import "math"

// GetRangeLimits::ROW_LIMIT_UNLIMITED and BYTE_LIMIT_UNLIMITED (FDBTypes.h:728).
const (
	rowLimitUnlimited  = -1
	byteLimitUnlimited = -1
)

// rangeLimits is C++ GetRangeLimits (FDBTypes.h:727-757): a row limit, a byte
// limit, and the rows a read must return before a byte limit may stop it. The
// fields are C ints; -1 means unlimited.
type rangeLimits struct{ rows, minRows, bytes int32 }

// newRangeLimits is GetRangeLimits(rows, bytes) as the C binding builds it,
// with 0 mapped to unlimited (fdb_c.cpp:991-994). Callers have already rejected
// a row limit below -1 (range_limits_invalid).
func newRangeLimits(rows, bytes int) rangeLimits {
	if rows == 0 {
		rows = rowLimitUnlimited
	}
	if bytes == 0 {
		bytes = byteLimitUnlimited
	}
	return rangeLimits{rows: clampInt32(int64(rows)), minRows: 1, bytes: clampInt32(int64(bytes))}
}

func clampInt32(v int64) int32 { return int32(max(math.MinInt32, min(v, math.MaxInt32))) }

func (l rangeLimits) hasRowLimit() bool  { return l.rows != rowLimitUnlimited }
func (l rangeLimits) hasByteLimit() bool { return l.bytes != byteLimitUnlimited }

// isReached is NativeAPI.actor.cpp:2856-2858.
func (l rangeLimits) isReached() bool { return l.rows == 0 || (l.bytes == 0 && l.minRows == 0) }

// hasSatisfiedMinRows is NativeAPI.actor.cpp:2875-2877.
func (l rangeLimits) hasSatisfiedMinRows() bool { return l.hasByteLimit() && l.minRows == 0 }

// isValid is FDBTypes.h:753-756.
func (l rangeLimits) isValid() bool {
	return (l.rows >= 0 || l.rows == rowLimitUnlimited) && (l.bytes >= 0 || l.bytes == byteLimitUnlimited) &&
		l.minRows >= 0 && (l.minRows <= l.rows || l.rows == rowLimitUnlimited)
}

// decrement is GetRangeLimits::decrement for one reply of n rows charging
// size bytes (NativeAPI.actor.cpp:2811-2821; size counts 8 per row plus key and
// value, :2819). C++ asserts a reply never exceeds the row limit; the row count
// stops at zero instead of going negative so a misbehaving server still ends
// the read.
func (l *rangeLimits) decrement(n, size int) {
	if l.hasRowLimit() {
		l.rows = int32(max(0, int64(l.rows)-int64(n)))
	}
	l.minRows = int32(max(0, int64(l.minRows)-int64(n)))
	if l.hasByteLimit() {
		l.bytes = int32(max(0, int64(l.bytes)-int64(size)))
	}
}

// transformRangeLimits is NativeAPI.actor.cpp:4222-4240: the request's row and
// byte limits. A byte limit already spent to zero asks for the rows minRows
// still needs, under the reply ceiling.
func transformRangeLimits(l rangeLimits, reverse bool) (limit, limitBytes int32) {
	limitBytes = replyByteLimit
	if l.bytes != 0 {
		limit = replyByteLimit
		if l.hasRowLimit() {
			limit = min(replyByteLimit, l.rows)
		}
		if l.hasByteLimit() {
			limitBytes = min(replyByteLimit, l.bytes)
		}
	} else {
		limit = l.minRows
	}
	if reverse {
		limit = -limit
	}
	return limit, limitBytes
}

// setRequestLimits is ReadYourWrites.actor.cpp:569-590: a RYW storage
// request's limits from the caller's remaining limits, the extra rows the
// request must return (cleared keys it crosses, rows past a selector), the
// selector offset, and how many requests this read has sent.
func setRequestLimits(l *rangeLimits, additionalRows int64, offset int32, requestCount int) {
	rowsFor := func(rows int32) int32 {
		return int32(min(max(int64(max(1, rows))+additionalRows, int64(offset)), math.MaxInt32))
	}
	l.minRows = int32(min(max(1+additionalRows, int64(offset)), math.MaxInt32))
	if l.hasRowLimit() {
		l.rows = rowsFor(l.rows)
	}
	if l.bytes == 0 {
		l.bytes = byteLimitUnlimited
		if !l.hasRowLimit() {
			l.rows = rowsFor(l.rows)
		}
	} else if l.hasByteLimit() {
		l.bytes = int32(min(int64(l.bytes)<<min(requestCount, 20), replyByteLimit))
	}
}
