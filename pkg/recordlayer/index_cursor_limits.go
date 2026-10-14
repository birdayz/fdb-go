package recordlayer

import (
	"bytes"
	"math"

	"fdb.dev/pkg/fdbgo/fdb"
)

func (c *indexCursor) withContext(rc *FDBRecordContext) *indexCursor {
	if rc != nil && c.scanProps.ExecuteProperties.TimeLimit > 0 {
		c.clock = NewScanLimiterStateIn(rc.Env())
		c.clock.AnchorAt(rc.createTime)
	}
	return c
}

func indexRangeOptions(props ScanProperties) fdb.RangeOptions {
	limit := props.ExecuteProperties.ReturnedRowLimit
	if limit <= 0 {
		limit = 0
	}
	options := fdb.RangeOptions{Mode: props.CursorStreamingMode.ToFDB(), Reverse: props.IsReverse(), Limit: limit}
	// Java switches a finite WANT_ALL scan to EXACT; other explicit modes stay.
	if props.CursorStreamingMode == StreamingModeWantAll && options.Limit > 0 {
		options.Mode = fdb.StreamingModeExact
	}
	return options
}

// indexSkipRange applies a positive Skip as Java's selector offsets after the
// continuation (KeyValueCursorBase.java:345-352): begin FGE(low)+skip forward,
// end FGE(high)-skip reverse. The skipped rows are never read by the cursor.
func indexSkipRange(rt fdb.ReadTransaction, rng fdb.KeyRange, skip int, reverse bool) (fdb.Range, error) {
	if skip <= 0 {
		return rng, nil
	}
	begin, end := rng.Begin.FDBKey(), rng.End.FDBKey()
	if reverse {
		sel, err := shiftSelector(rt, end, -int64(skip), func(k fdb.Key) bool { return bytes.Compare(k, begin) <= 0 })
		return fdb.SelectorRange{Begin: fdb.FirstGreaterOrEqual(begin), End: sel}, err
	}
	sel, err := shiftSelector(rt, begin, int64(skip), func(k fdb.Key) bool { return bytes.Compare(k, end) >= 0 })
	return fdb.SelectorRange{Begin: sel, End: fdb.FirstGreaterOrEqual(end)}, err
}

// shiftSelector is FGE(key) moved delta keys. Java's int KeySelector.add wraps a
// forward MaxInt32 skip negative; Go resolves offsets beyond the native int32
// in steps instead, stopping once a step leaves the range (empty).
func shiftSelector(rt fdb.ReadTransaction, key fdb.Key, delta int64, outside func(fdb.Key) bool) (fdb.KeySelector, error) {
	sel := fdb.FirstGreaterOrEqual(key)
	for {
		offset := int64(sel.Offset)
		if delta >= math.MinInt32-offset && delta <= math.MaxInt32-offset {
			sel.Offset = int(offset + delta)
			return sel, nil
		}
		step := math.MaxInt32 - offset
		if delta < 0 {
			step = math.MinInt32 - offset
		}
		sel.Offset = int(offset + step)
		delta -= step
		resolved, err := rt.GetKey(sel).Get()
		if err != nil {
			return fdb.KeySelector{}, err
		}
		if outside(resolved) {
			return fdb.FirstGreaterOrEqual(resolved), nil
		}
		sel = fdb.LastLessOrEqual(resolved)
	}
}

// CursorLimitManager charges attempts, including exhaustion and rejected scans.
func tryIndexRecordScan(state, clock *ScanLimiterState, props ExecuteProperties, initialPass *bool) (bool, NoNextReason) {
	before := state.RecordsScanned()
	state.AddRecordScanned()
	if props.ScannedRecordsLimit > 0 && before >= props.ScannedRecordsLimit && (*initialPass || props.FailOnScanLimitReached) {
		return false, ScanLimitReached
	}
	if props.ScannedBytesLimit > 0 && state.BytesScanned() >= props.ScannedBytesLimit && *initialPass {
		return false, ByteLimitReached
	}
	if props.TimeLimit > 0 && clock.Elapsed() >= props.TimeLimit && *initialPass {
		return false, TimeLimitReached
	}
	*initialPass = true
	return true, SourceExhausted
}
