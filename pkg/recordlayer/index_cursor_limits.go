package recordlayer

import (
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
// end FGE(high)-skip reverse. Selector resolution reads the skipped keys; the
// cursor never returns them. Java's int KeySelector.add wraps a forward skip of
// MaxInt32 to MinInt32; Go saturates the begin offset at MaxInt32 instead.
func indexSkipRange(rng fdb.KeyRange, skip int, reverse bool) fdb.Range {
	if skip <= 0 {
		return rng
	}
	begin, end := fdb.FirstGreaterOrEqual(rng.Begin), fdb.FirstGreaterOrEqual(rng.End)
	if reverse {
		end.Offset = 1 - min(skip, math.MaxInt32)
	} else {
		begin.Offset = 1 + min(skip, math.MaxInt32-1)
	}
	return fdb.SelectorRange{Begin: begin, End: end}
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
