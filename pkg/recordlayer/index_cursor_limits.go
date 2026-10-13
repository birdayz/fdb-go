package recordlayer

import "fdb.dev/pkg/fdbgo/fdb"

func (c *indexCursor) withContext(rc *FDBRecordContext) *indexCursor {
	if rc != nil && c.scanProps.ExecuteProperties.TimeLimit > 0 {
		c.clock = NewScanLimiterStateIn(rc.Env())
		c.clock.AnchorAt(rc.createTime)
	}
	return c
}

func indexRangeOptions(props ScanProperties) fdb.RangeOptions {
	options := fdb.RangeOptions{Mode: props.CursorStreamingMode.ToFDB(), Reverse: props.IsReverse(), Limit: props.ExecuteProperties.ReturnedRowLimit}
	// Java switches a finite WANT_ALL scan to EXACT; other explicit modes stay.
	if props.CursorStreamingMode == StreamingModeWantAll && options.Limit > 0 {
		options.Mode = fdb.StreamingModeExact
	}
	return options
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
