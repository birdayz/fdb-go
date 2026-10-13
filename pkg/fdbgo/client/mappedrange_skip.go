package client

import (
	"bytes"
	"context"
)

// GetMappedRangeWithByteTargetAndSkip resolves the initial Skip with ordinary
// reads, without issuing secondary reads for the skipped primary rows.
// Nonpositive Skip retains GetMappedRangeWithByteTarget's existing exact path.
func (tx *Transaction) GetMappedRangeWithByteTargetAndSkip(ctx context.Context, begin, end, mapper []byte, limit, byteTarget int, skip int32, reverse bool) ([]MappedKeyValue, bool, error) {
	return tx.getMappedRangeWithByteTargetAndSkip(ctx, begin, end, mapper, limit, byteTarget, skip, reverse)
}

func (tx *Transaction) getMappedRangeWithResolvedSkip(ctx context.Context, begin, end, mapper []byte, limits mappedRangeLimits, skip int32, reverse bool) (mappedRangeResult, error) {
	if bytes.Compare(begin, end) >= 0 {
		return mappedRangeResult{}, nil
	}
	key, remaining := begin, int64(skip)
	if reverse {
		key, remaining = end, -remaining
	}
	for remaining != 0 {
		offset, rest := mappedSkipResolutionStep(remaining)
		// Native getKey adds neither a RYW overlay nor independent read conflicts.
		resolved, err := tx.getKey(ctx, key, false, offset)
		if err != nil {
			return mappedRangeResult{}, err
		}
		if (!reverse && bytes.Compare(resolved, end) >= 0) || (reverse && bytes.Compare(resolved, begin) <= 0) {
			return mappedRangeResult{}, nil
		}
		key, remaining = resolved, rest
	}
	if reverse {
		end = key
	} else {
		begin = key
	}
	// Mapped storage requests must not contain offsets that can invert the range.
	return tx.getMappedRangeSelectors(ctx, mappedSelectorFGE(begin), mappedSelectorFGE(end), mapper, limits, reverse)
}

func mappedSkipResolutionStep(remaining int64) (offset int32, rest int64) {
	// Leave room for SS.findKey's signed distance and equal-key arithmetic.
	const maxDistance int64 = 1 << 30
	distance := max(-maxDistance, min(remaining, maxDistance))
	return int32(distance + 1), remaining - distance
}
