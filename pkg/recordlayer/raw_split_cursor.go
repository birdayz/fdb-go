package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// rawSplitRecord is the storage carrier before record or queue deserialization.
type rawSplitRecord struct {
	key     tuple.Tuple
	value   []byte
	version *FDBRecordVersion
}

// rawSplitCursor follows Java KeyValueUnsplitter's physical-KV accounting.
// It intentionally does not use the record cursor's logical-record limit policy.
type rawSplitCursor struct {
	context      *FDBRecordContext
	subspace     subspace.Subspace
	props        ScanProperties
	state        *ScanLimiterState
	clock        *ScanLimiterState
	iterator     rangeIterator
	pending      *fdb.KeyValue
	continuation RecordCursorContinuation
	stopped      NoNextReason
	hasStopped   bool
	initialPass  bool
	exhausted    bool
	returned     int
	closed       bool
	terminal     *RecordCursorResult[*rawSplitRecord]
}

func newRawSplitCursor(rc *FDBRecordContext, ss subspace.Subspace, props ScanProperties, continuation []byte) *rawSplitCursor {
	state := props.ExecuteProperties.ScanState
	if state == nil {
		state = NewScanLimiterStateIn(rc.env)
	}
	cont := RecordCursorContinuation(&StartContinuation{})
	if continuation != nil {
		cont = NewBytesContinuation(continuation)
	}
	return &rawSplitCursor{context: rc, subspace: ss, props: props, state: state, clock: NewScanLimiterStateIn(rc.env), continuation: cont}
}

func (c *rawSplitCursor) init() error {
	begin, end := c.subspace.FDBRangeKeys()
	low, high := begin.FDBKey(), end.FDBKey()
	continuation, err := c.continuation.ToBytes()
	if err != nil {
		return err
	}
	if continuation != nil {
		key := append(append([]byte(nil), c.subspace.Bytes()...), unwrapContinuation(continuation)...)
		if c.props.IsReverse() {
			high = key
		} else {
			low = append(key, 0)
		}
	}
	c.iterator = c.context.ReadTransaction(true).GetRange(fdb.KeyRange{Begin: low, End: high}, fdb.RangeOptions{Reverse: c.props.IsReverse(), Mode: c.props.CursorStreamingMode.ToFDB()}).Iterator()
	return nil
}

func (c *rawSplitCursor) take() (*fdb.KeyValue, error) {
	if c.pending != nil {
		kv := c.pending
		c.pending = nil
		return kv, nil
	}
	if c.exhausted {
		return nil, nil
	}
	if !c.iterator.Advance() {
		if _, err := c.iterator.Get(); err != nil {
			return nil, err
		}
		c.exhausted = true
		return nil, nil
	}
	kv, err := c.iterator.Get()
	if err != nil {
		return nil, err
	}
	p := c.props.ExecuteProperties
	before := c.state.RecordsScanned()
	c.state.AddRecordScanned()
	c.hasStopped = false
	switch {
	case p.ScannedRecordsLimit > 0 && before >= p.ScannedRecordsLimit && (c.initialPass || p.FailOnScanLimitReached):
		c.hasStopped = true
		c.stopped = ScanLimitReached
	case p.ScannedBytesLimit > 0 && c.state.BytesScanned() >= p.ScannedBytesLimit && c.initialPass:
		c.hasStopped = true
		c.stopped = ByteLimitReached
	case p.TimeLimit > 0 && c.clock.Elapsed() >= p.TimeLimit && c.initialPass:
		c.hasStopped = true
		c.stopped = TimeLimitReached
	}
	if c.hasStopped && p.FailOnScanLimitReached {
		return nil, &ScanLimitReachedError{Reason: c.stopped}
	}
	if !c.hasStopped {
		c.initialPass = true
	}
	return &kv, nil
}

func (c *rawSplitCursor) stop(reason NoNextReason) RecordCursorResult[*rawSplitRecord] {
	cont := c.continuation
	if reason == SourceExhausted {
		cont = &EndContinuation{}
	}
	result := NewResultNoNext[*rawSplitRecord](reason, cont)
	c.terminal = &result
	return result
}

func (c *rawSplitCursor) OnNext(ctx context.Context) (RecordCursorResult[*rawSplitRecord], error) {
	if c.terminal != nil {
		return *c.terminal, nil
	}
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*rawSplitRecord]{}, err
	}
	if c.closed {
		return RecordCursorResult[*rawSplitRecord]{}, fmt.Errorf("cursor is closed")
	}
	if limit := c.props.ExecuteProperties.ReturnedRowLimit; limit > 0 && c.returned >= limit {
		return c.stop(ReturnLimitReached), nil
	}
	if c.hasStopped {
		if c.exhausted {
			return c.stop(SourceExhausted), nil
		}
		return c.stop(c.stopped), nil
	}
	if c.iterator == nil {
		if err := c.init(); err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
	}
	var record *rawSplitRecord
	var prefix []byte
	var lastSuffix int64
	var haveData bool
	for {
		if err := ctx.Err(); err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
		kv, err := c.take()
		if err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
		if kv == nil {
			break
		}
		// Java charges append, including a lookahead again when it is consumed.
		c.state.AddBytesScanned(int64(len(kv.Key) + len(kv.Value)))
		key, err := c.subspace.Unpack(kv.Key)
		if err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
		if len(key) == 0 {
			return RecordCursorResult[*rawSplitRecord]{}, &RecordCoreStorageError{Message: "Missing split record suffix"}
		}
		suffix, ok := key[len(key)-1].(int64)
		if !ok {
			return RecordCursorResult[*rawSplitRecord]{}, &RecordCoreStorageError{Message: "Invalid split record suffix", PrimaryKey: key}
		}
		pk := key[:len(key)-1]
		packed := c.subspace.Pack(pk)
		if record != nil && !bytes.Equal(prefix, packed) {
			c.pending = kv
			break
		}
		first := record == nil
		if first {
			record = &rawSplitRecord{key: pk}
			prefix = packed
		}
		valid := false
		if first {
			valid = suffix == unsplitRecord || (!c.props.IsReverse() && suffix == recordVersionSuffix) || suffix == startSplitRecord || (c.props.IsReverse() && suffix != recordVersionSuffix)
		} else if c.props.IsReverse() {
			valid = (suffix == recordVersionSuffix && (lastSuffix == startSplitRecord || lastSuffix == unsplitRecord)) || (suffix == lastSuffix-1 && suffix != recordVersionSuffix)
		} else {
			valid = (lastSuffix == recordVersionSuffix && (suffix == unsplitRecord || suffix == startSplitRecord)) || (lastSuffix > 0 && suffix == lastSuffix+1)
		}
		if !valid {
			if first {
				return RecordCursorResult[*rawSplitRecord]{}, &FoundSplitWithoutStartError{NextIndex: suffix, Reverse: c.props.IsReverse(), KeyTuple: pk}
			}
			expected := lastSuffix + 1
			if c.props.IsReverse() {
				expected = lastSuffix - 1
			}
			if (c.props.IsReverse() && expected == startSplitRecord) || (!c.props.IsReverse() && lastSuffix == recordVersionSuffix) {
				return RecordCursorResult[*rawSplitRecord]{}, &FoundSplitWithoutStartError{NextIndex: suffix, Reverse: c.props.IsReverse(), KeyTuple: pk}
			}
			return RecordCursorResult[*rawSplitRecord]{}, &FoundSplitOutOfOrderError{Expected: expected, Found: suffix, KeyTuple: pk}
		}
		if suffix == recordVersionSuffix {
			record.version, err = unpackVersion(kv.Value)
			if err != nil {
				return RecordCursorResult[*rawSplitRecord]{}, err
			}
		} else {
			haveData = true
			if c.props.IsReverse() {
				record.value = append(append([]byte(nil), kv.Value...), record.value...)
			} else {
				record.value = append(record.value, kv.Value...)
			}
		}
		lastSuffix = suffix
		cont, err := wrapContinuation(kv.Key[len(c.subspace.Bytes()):])
		if err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
		c.continuation = NewBytesContinuation(cont)
		if (!c.props.IsReverse() && suffix == unsplitRecord) || (c.props.IsReverse() && suffix == recordVersionSuffix) {
			break
		}
	}
	if record == nil {
		return c.stop(SourceExhausted), nil
	}
	if !haveData || (c.props.IsReverse() && lastSuffix != startSplitRecord && lastSuffix != unsplitRecord && lastSuffix != recordVersionSuffix) {
		return RecordCursorResult[*rawSplitRecord]{}, &FoundSplitWithoutStartError{NextIndex: lastSuffix, Reverse: c.props.IsReverse(), KeyTuple: record.key}
	}
	versionKey := c.subspace.Pack(append(append(tuple.Tuple(nil), record.key...), recordVersionSuffix))
	if local, ok := c.context.GetLocalVersion(versionKey); ok {
		var err error
		record.version, err = IncompleteVersion(local)
		if err != nil {
			return RecordCursorResult[*rawSplitRecord]{}, err
		}
	}
	c.returned++
	return NewResultWithValue(record, c.continuation), nil
}

func (c *rawSplitCursor) Close() error   { c.closed = true; return nil }
func (c *rawSplitCursor) IsClosed() bool { return c.closed }
