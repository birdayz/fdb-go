package client

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/wire"
)

type mappedRangeResult struct {
	rows []MappedKeyValue
	more bool
}

type mappedRangeLimits struct {
	rows, bytes, minRows int
}

func newMappedRangeLimits(rows, byteTarget int) mappedRangeLimits {
	if rows == 0 {
		rows = -1
	}
	return mappedRangeLimits{rows: rows, bytes: byteTarget, minRows: 1}
}

func (l mappedRangeLimits) reached() bool {
	return l.rows == 0 || (l.bytes == 0 && l.minRows == 0)
}

func (l mappedRangeLimits) satisfiedMinRows() bool {
	return l.bytes != ByteLimitUnlimited && l.minRows == 0
}

func (l *mappedRangeLimits) decrement(rows []MappedKeyValue) {
	if l.rows != -1 {
		l.rows -= len(rows)
	}
	l.minRows = max(0, l.minRows-len(rows))
	if l.bytes != ByteLimitUnlimited {
		// NativeAPI:2831-2842 charges only the primary payload plus eight bytes.
		// Secondary bytes belong to the opt-in materialization ceiling instead.
		for i := range rows {
			l.bytes = max(0, l.bytes-8-len(rows[i].Key)-len(rows[i].Value))
		}
	}
}

// requestLimits feeds sendRangeRPC's existing transformRangeLimits machinery.
func (l mappedRangeLimits) requestLimits() (int, int) {
	if l.bytes == 0 {
		return l.minRows, ByteLimitUnlimited
	}
	rows := l.rows
	if rows == -1 {
		rows = replyByteLimit
	}
	return rows, l.bytes
}

func mappedSelectorFGE(key []byte) KeySelector {
	return KeySelector{Key: key, Offset: 1}
}

func mappedSelectorIsFGE(s KeySelector) bool { return !s.OrEqual && s.Offset == 1 }

func mappedRangeShardSelectors(begin, end KeySelector, shard LocationResult, reverse bool) (KeySelector, KeySelector, bool) {
	reqBegin, reqEnd, modified := begin, end, false
	if reverse && bytes.Compare(begin.Key, shard.ShardBegin) < 0 {
		reqBegin = mappedSelectorFGE(shard.ShardBegin)
		modified = true
	}
	if !reverse && bytes.Compare(end.Key, shard.ShardEnd) > 0 {
		reqEnd = mappedSelectorFGE(shard.ShardEnd)
		modified = true
	}
	return reqBegin, reqEnd, modified
}

func (tx *Transaction) sendGetMappedSelectors(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool, servers []ServerInfo) ([]MappedKeyValue, bool, error) {
	// FDB 7.3.77 constructs a KeyRange before rejecting resolved inversion.
	if !mappedSelectorIsFGE(begin) || !mappedSelectorIsFGE(end) || bytes.Compare(begin.Key, end.Key) >= 0 {
		return nil, false, &wire.FDBError{Code: ErrInternalError}
	}
	rows, byteTarget := limits.requestLimits()
	return sendRangeRPC(tx, ctx, begin.Key, end.Key, rows, byteTarget, reverse, servers, rangeRPCOps[MappedKeyValue]{
		endpoint: EndpointGetMappedKeyValues,
		build:    buildGetMappedKeyValuesRequest(mapper),
		putBuf:   func(bufp *[]byte) { getMappedKeyValuesBufPool.Put(bufp) },
		parse:    parseGetMappedKeyValuesReply,
	})
}

func (tx *Transaction) appendMappedSelectorRows(result *mappedRangeResult, rows []MappedKeyValue, materializedBytes *int64) error {
	if len(result.rows) == 0 && len(rows) > 0 {
		result.rows = rows[:len(rows):len(rows)]
	} else {
		result.rows = append(result.rows, rows...)
	}
	if ceiling := tx.db.rangeByteCeiling; ceiling > 0 {
		for i := range rows {
			*materializedBytes += mappedRangeScanOps.sizeBytes(&rows[i])
		}
		if *materializedBytes > ceiling {
			return &RangeMaterializationLimitError{LimitBytes: ceiling, ReachedBytes: *materializedBytes}
		}
	}
	return nil
}

// getMappedRangeSelectors retains NativeAPI's direct-read pagination after
// client-side resolution. Nonpositive Skip uses the existing exact scanner.
func (tx *Transaction) getMappedRangeSelectors(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	originalBegin, originalEnd, originalLimits := begin, end, limits
	var output mappedRangeResult
	var materializedBytes int64
	tx.hadRead.Store(true)
	for {
		if bytes.Compare(begin.Key, end.Key) >= 0 {
			return output, nil
		}
		// NativeAPI:4632 precedes both location lookup and fallback resolution.
		if len(end.Key) == 0 && (end.Offset < 1 || mappedSelectorIsFGE(end)) {
			return output, nil
		}
		locationKey, backward := begin.Key, keySelectorIsBackward(begin.OrEqual, begin.Offset)
		if reverse {
			locationKey = end.Key
			backward = keySelectorIsBackward(end.OrEqual, end.Offset-1)
		}
		loc, err := tx.db.locCache.locate(tx.db, ctx, locationKey, tx.tenantId, tx.currentSpan(), backward)
		if err != nil {
			return mappedRangeResult{}, fmt.Errorf("locate mapped range: %w", err)
		}
		if loc.ShardEnd == nil {
			loc.ShardEnd = allKeysEnd
		}
		reqBegin, reqEnd, modified := mappedRangeShardSelectors(begin, end, loc, reverse)
		rows, more, err := tx.sendGetMappedSelectors(ctx, reqBegin, reqEnd, mapper, limits, reverse, loc.Servers)
		if err != nil {
			if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
				tx.db.locCache.invalidate(locationKey, tx.tenantId, backward)
				if isWrongShardServer(err) {
					return tx.getMappedRangeSelectorFallback(ctx, originalBegin, originalEnd, mapper, originalLimits, reverse)
				}
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return mappedRangeResult{}, err
				}
				continue
			}
			if isReplyTimeout(err) {
				if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
					return mappedRangeResult{}, err
				}
				continue
			}
			return mappedRangeResult{}, err
		}
		if (more && len(rows) == 0) || (limits.rows != -1 && len(rows) > limits.rows) {
			return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
		}
		limits.decrement(rows)
		finished := limits.reached() || (!modified && !more) || limits.satisfiedMinRows()
		if err := tx.appendMappedSelectorRows(&output, rows, &materializedBytes); err != nil {
			return mappedRangeResult{}, err
		}
		if finished {
			output.more = modified || limits.reached() || more
			return output, nil
		}
		if !more {
			if len(rows) == 0 {
				return tx.getMappedRangeSelectorFallback(ctx, originalBegin, originalEnd, mapper, originalLimits, reverse)
			}
			if reverse {
				end = mappedSelectorFGE(loc.ShardBegin)
			} else {
				begin = mappedSelectorFGE(loc.ShardEnd)
			}
		} else if reverse {
			end = mappedSelectorFGE(output.rows[len(output.rows)-1].Key)
		} else {
			begin = mappedSelectorFGE(keyAfterBytes(output.rows[len(output.rows)-1].Key))
		}
	}
}

func (tx *Transaction) getMappedRangeSelectorFallback(ctx context.Context, begin, end KeySelector, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	if bytes.Compare(begin.Key, end.Key) >= 0 {
		return mappedRangeResult{}, nil
	}
	return tx.getMappedRangeSelectorExact(ctx, begin.Key, end.Key, mapper, limits, reverse)
}

// mappedExactLocations obtains the two clipped intervals used by getExactRange.
// Directional point lookups avoid requiring full-range cache coverage beforehand.
func (tx *Transaction) mappedExactLocations(ctx context.Context, begin, end []byte, reverse bool) ([]LocationResult, error) {
	locations := make([]LocationResult, 0, 2)
	for len(locations) < 2 && bytes.Compare(begin, end) < 0 {
		key := begin
		if reverse {
			key = end
		}
		loc, err := tx.db.locCache.locate(tx.db, ctx, key, tx.tenantId, tx.currentSpan(), reverse)
		if err != nil {
			return nil, fmt.Errorf("locate exact mapped range: %w", err)
		}
		if bytes.Compare(loc.ShardBegin, begin) < 0 {
			loc.ShardBegin = begin
		}
		if loc.ShardEnd == nil || bytes.Compare(loc.ShardEnd, end) > 0 {
			loc.ShardEnd = end
		}
		if bytes.Compare(loc.ShardBegin, loc.ShardEnd) >= 0 {
			return nil, &wire.FDBError{Code: ErrInternalError}
		}
		locations = append(locations, loc)
		if reverse {
			end = loc.ShardBegin
		} else {
			begin = loc.ShardEnd
		}
	}
	return locations, nil
}

// getMappedRangeSelectorExact is the positive-Skip fallback's NativeAPI:4255-4444
// result-state machine, including the exact-only reverse More correction.
func (tx *Transaction) getMappedRangeSelectorExact(ctx context.Context, begin, end, mapper []byte, limits mappedRangeLimits, reverse bool) (mappedRangeResult, error) {
	var output mappedRangeResult
	var materializedBytes int64
	for {
		locations, err := tx.mappedExactLocations(ctx, begin, end, reverse)
		if err != nil {
			return mappedRangeResult{}, err
		}
		if len(locations) == 0 {
			return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
		}
		for shard := 0; ; {
			loc := &locations[shard]
			rows, more, err := tx.sendGetMappedSelectors(ctx, mappedSelectorFGE(loc.ShardBegin), mappedSelectorFGE(loc.ShardEnd), mapper, limits, reverse, loc.Servers)
			if err != nil {
				if isWrongShardServer(err) || isAllAlternativesFailed(err) || isMaybeDelivered(err) {
					if reverse {
						end = loc.ShardEnd
					} else {
						begin = loc.ShardBegin
					}
					tx.db.locCache.invalidateRange(begin, end, tx.tenantId)
					if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
						return mappedRangeResult{}, err
					}
					break
				}
				if isReplyTimeout(err) {
					if err := sleepCtx(ctx, wrongShardRetryDelay); err != nil {
						return mappedRangeResult{}, err
					}
					continue
				}
				return mappedRangeResult{}, err
			}
			if (more && len(rows) == 0) || (limits.rows != -1 && len(rows) > limits.rows) {
				return mappedRangeResult{}, &wire.FDBError{Code: ErrInternalError}
			}
			if err := tx.appendMappedSelectorRows(&output, rows, &materializedBytes); err != nil {
				return mappedRangeResult{}, err
			}
			limits.decrement(rows)
			if limits.reached() {
				output.more = true
				return output, nil
			}
			if reverse && more && len(rows) > 0 && bytes.Equal(output.rows[len(output.rows)-1].Key, loc.ShardBegin) {
				more = false
			}
			if more {
				last := output.rows[len(output.rows)-1].Key
				if reverse {
					loc.ShardEnd = last
				} else {
					loc.ShardBegin = keyAfterBytes(last)
				}
			}
			if !more || bytes.Compare(loc.ShardBegin, loc.ShardEnd) >= 0 {
				if shard == len(locations)-1 {
					if reverse {
						end = loc.ShardBegin
					} else {
						begin = loc.ShardEnd
					}
					if bytes.Compare(begin, end) >= 0 {
						output.more = false
						return output, nil
					}
					break
				}
				shard++
			}
			if limits.satisfiedMinRows() && len(output.rows) > 0 {
				output.more = true
				return output, nil
			}
		}
	}
}
