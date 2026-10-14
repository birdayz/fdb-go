// Portions derived from FoundationDB Record Layer (CursorLimitManager.java,
// KeyValueCursorBase.java, SplitHelper.java, TimeScanLimiter.java,
// and others),
// Copyright 2015-2018 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"bytes"
	"context"
	"fmt"

	"fdb.dev/pkg/recordlayer/internal/tuplefast"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// continuationMagicNumber identifies Java's protobuf-wrapped key-value tokens.
const continuationMagicNumber int64 = 6_773_487_359_078_157_740

// wrapContinuation matches Java's TO_NEW format: a suffix relative to the range's
// common prefix. An empty-but-present suffix is a real position, distinct from nil.
func wrapContinuation(innerBytes []byte) ([]byte, error) {
	// A nil suffix is an end position; Java returns the end marker, not a wrapped
	// token. The cursor already represents end via EndContinuation, so this is
	// defensive — never fabricate a proto for nil. (An empty-but-non-nil suffix
	// IS a real position and is wrapped below.)
	if innerBytes == nil {
		return nil, nil
	}
	magic := continuationMagicNumber
	msg := &gen.KeyValueCursorContinuation{
		InnerContinuation: innerBytes,
		MagicNumber:       &magic,
	}
	out, err := msg.MarshalVT()
	if err != nil {
		return nil, fmt.Errorf("marshal key-value cursor continuation: %w", err)
	}
	return out, nil
}

// keyValueContinuation defers the proto marshal to ToBytes: most rows' continuations
// are never serialized (Java's KeyValueCursorBase.Continuation.toBytes is lazy too).
type keyValueContinuation struct {
	suffix []byte
}

func (c *keyValueContinuation) ToBytes() ([]byte, error) {
	return wrapContinuation(c.suffix)
}

// IsEnd mirrors wrapContinuation, which maps only a nil suffix to the end token.
func (c *keyValueContinuation) IsEnd() bool {
	return c.suffix == nil
}

// unwrapContinuation matches Java's getInnerContinuation: only a successfully
// decoded proto carrying the magic number is unwrapped; other bytes stay intact.
func unwrapContinuation(rawBytes []byte) []byte {
	if rawBytes == nil {
		return nil
	}
	msg := &gen.KeyValueCursorContinuation{}
	if err := UnmarshalVTAsJava(msg, rawBytes); err != nil {
		return rawBytes
	}
	if msg.MagicNumber == nil || *msg.MagicNumber != continuationMagicNumber {
		return rawBytes
	}
	return msg.InnerContinuation
}

// rangeIterator is the minimal FDB range-scan iterator the cursor depends on.
// *fdb.RangeIterator satisfies it; the interface exists so tests can inject a fake
// that returns an error at a chosen position (the row-limit boundary), which a
// concrete *fdb.RangeIterator can't be made to do deterministically.
type rangeIterator interface {
	Advance() bool
	Get() (fdb.KeyValue, error)
}

// packedRecordVersionSuffix is the tuple encoding of SplitHelper.RECORD_VERSION.
var packedRecordVersionSuffix = tuple.Tuple{recordVersionSuffix}.Pack()

// keyValueCursor is FDBRecordStore.scanTypedRecords. A suffixed store runs
// KeyValueUnsplitter over an unlimited KeyValueCursor, then skip and limitRowsTo;
// a legacy suffix-less unsplit store maps a KeyValueCursor with the scan's
// properties one key-value per record.
type keyValueCursor struct {
	store          *FDBRecordStore
	continuation   []byte // the caller's resume point, read once by initIterator
	scanProperties ScanProperties
	reverse        bool

	iterator        rangeIterator
	closed          bool
	keyPrefixLength int // records subspace length: where a key's primary-key tuple starts
	bounds          kvScanBounds
	prefixLength    int // continuation prefix length (Java's calculatePrefixLength)

	// scanState holds the (possibly shared) scanned-records/bytes counters; clock
	// anchors the time limit at the transaction's creation, as TimeScanLimiter does.
	scanState *ScanLimiterState
	clock     *ScanLimiterState

	omitUnsplitRecordSuffix bool
	useOldVersionFormat     bool

	// CursorLimitManager: halted is the outcome of the latest tryRecordScan.
	initialPass bool
	halted      bool
	haltReason  NoNextReason

	// SkipCursor and RowLimitedCursor over the unsplitter.
	skipped  int
	returned int
	lastCont RecordCursorContinuation

	// The first terminal result, replayed by later calls.
	terminal    RecordCursorResult[*FDBStoredRecord[proto.Message]]
	hasTerminal bool

	// Inner KeyValueCursor: innerLimit is the row limit passed down to the range
	// read, selectorSkip the skip it applies by offsetting the range's selector.
	innerLimit   int
	selectorSkip int
	valuesSeen   int
	lastKey      fdb.Key
	innerDone    bool

	// KeyValueUnsplitter. contKey is the key of the last appended key-value, or
	// with contEnd the inner cursor's end continuation.
	pending        fdb.KeyValue
	hasPending     bool
	contKey        fdb.Key
	contEnd        bool
	innerReason    NoNextReason
	hasInnerReason bool
	nextPrefix     []byte
	nextIndex      int64
	nextVersion    *FDBRecordVersion
	haveData       bool
	parts          [][]byte
	sizes          sizeInfo
	versionKey     []byte
}

// OnNext returns the next record or indicates why iteration stopped.
func (c *keyValueCursor) OnNext(ctx context.Context) (RecordCursorResult[*FDBStoredRecord[proto.Message]], error) {
	if c.closed {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, fmt.Errorf("cursor is closed")
	}
	if c.hasTerminal {
		return c.terminal, nil
	}
	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
	}
	var result RecordCursorResult[*FDBStoredRecord[proto.Message]]
	var err error
	if c.omitUnsplitRecordSuffix {
		result, err = c.nextBareRecord(ctx)
	} else {
		result, err = c.nextRecord(ctx)
	}
	if err == nil && !result.HasNext() {
		c.terminal, c.hasTerminal = result, true
	}
	return result, err
}

// nextRecord is unsplitter.skip(skip).limitRowsTo(limit). The row limit stops
// without reading further, so an exactly exhausted page still ends with
// RETURN_LIMIT_REACHED and its last record's continuation.
func (c *keyValueCursor) nextRecord(ctx context.Context) (RecordCursorResult[*FDBStoredRecord[proto.Message]], error) {
	props := c.scanProperties.ExecuteProperties
	if props.ReturnedRowLimit > 0 && c.returned >= props.ReturnedRowLimit {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](ReturnLimitReached, c.lastCont), nil
	}
	for {
		ok, reason, cont, err := c.unsplit(ctx)
		if err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
		if !ok {
			return NewResultNoNext[*FDBStoredRecord[proto.Message]](reason, cont), nil
		}
		if c.skipped < props.Skip {
			c.skipped++
			continue
		}
		record, err := c.storedRecord(c.nextPrefix, c.assemble(), c.nextVersion, c.sizes)
		if err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
		cont, err = c.makeContinuation(c.contKey)
		if err != nil {
			return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
		}
		c.returned++
		c.lastCont = cont
		return NewResultWithValue(record, cont), nil
	}
}

// unsplit is KeyValueUnsplitter.onNext. On ok, the record is left in
// nextPrefix/parts/nextVersion/sizes until the next call.
func (c *keyValueCursor) unsplit(ctx context.Context) (bool, NoNextReason, RecordCursorContinuation, error) {
	c.nextPrefix, c.nextVersion, c.haveData = nil, nil, false
	if c.halted {
		if c.hasInnerReason && c.innerReason == SourceExhausted {
			return false, SourceExhausted, &EndContinuation{}, nil
		}
		return false, c.haltReason, c.unsplitContinuation(), nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, 0, nil, err
		}
		if c.hasPending {
			kv := c.pending
			c.pending, c.hasPending = fdb.KeyValue{}, false
			complete, err := c.append(kv)
			if err != nil {
				return false, 0, nil, err
			}
			if complete {
				break
			}
		}
		kv, ok, err := c.nextKV()
		if err != nil {
			return false, 0, nil, err
		}
		if !ok {
			if c.reverse && c.haveData && c.nextIndex != startSplitRecord && c.nextIndex != unsplitRecord && c.nextIndex != recordVersionSuffix {
				return false, 0, nil, &FoundSplitWithoutStartError{NextIndex: c.nextIndex, Reverse: true, KeyTuple: c.nextPrimaryKey()}
			}
			c.innerReason, c.hasInnerReason = SourceExhausted, true
			if c.innerLimit > 0 && c.valuesSeen >= c.innerLimit {
				c.innerReason = ReturnLimitReached
			}
			if !c.haveData {
				c.contKey, c.contEnd = c.lastKey, c.innerReason == SourceExhausted
			}
			break
		}
		c.hasInnerReason = false
		if err := c.tryRecordScan(); err != nil {
			return false, 0, nil, err
		}
		complete, err := c.append(kv)
		if err != nil {
			return false, 0, nil, err
		}
		if complete {
			break
		}
	}
	if c.nextVersion != nil && !c.haveData {
		return false, 0, nil, &FoundSplitWithoutStartError{NextIndex: recordVersionSuffix, Reverse: c.reverse, KeyTuple: c.nextPrimaryKey()}
	}
	if !c.haveData {
		reason := c.innerReason
		if c.halted && reason != SourceExhausted {
			reason = c.haltReason
		}
		return false, reason, c.unsplitContinuation(), nil
	}
	if !c.useOldVersionFormat {
		// A version saved in this transaction is still a pending versionstamp.
		c.versionKey = append(append(c.versionKey[:0], c.nextPrefix...), packedRecordVersionSuffix...)
		if local, ok := c.store.context.GetLocalVersion(c.versionKey); ok {
			version, err := IncompleteVersion(local)
			if err != nil {
				return false, 0, nil, err
			}
			c.nextVersion = version
			c.sizes.KeyCount++
			c.sizes.KeySize += len(c.versionKey)
			c.sizes.ValueSize += VersionBytes + 1
		}
	}
	c.sizes.VersionedInline = c.nextVersion != nil
	return true, 0, nil, nil
}

// tryRecordScan charges one scanned key-value; Java throws on a halt in fail mode.
func (c *keyValueCursor) tryRecordScan() error {
	props := c.scanProperties.ExecuteProperties
	allowed, reason := tryIndexRecordScan(c.scanState, c.clock, props, &c.initialPass)
	c.halted, c.haltReason = !allowed, reason
	if !allowed && props.FailOnScanLimitReached {
		return &ScanLimitReachedError{Reason: reason}
	}
	return nil
}

// append is KeyValueUnsplitter.append: it charges the key-value's bytes, a
// lookahead again when it is consumed, and reports whether the record is complete.
func (c *keyValueCursor) append(kv fdb.KeyValue) (bool, error) {
	c.scanState.AddBytesScanned(int64(len(kv.Key) + len(kv.Value)))
	if c.nextPrefix == nil {
		c.contKey, c.contEnd = kv.Key, false
		return c.appendFirst(kv)
	}
	if bytes.HasPrefix(kv.Key, c.nextPrefix) {
		c.contKey, c.contEnd = kv.Key, false
		return c.appendNext(kv)
	}
	if c.reverse && c.nextIndex != unsplitRecord && c.nextIndex != startSplitRecord && c.nextIndex != recordVersionSuffix {
		return false, &FoundSplitWithoutStartError{NextIndex: c.nextIndex, Reverse: true, KeyTuple: c.nextPrimaryKey()}
	}
	c.pending, c.hasPending = kv, true
	return true, nil
}

func (c *keyValueCursor) appendFirst(kv fdb.KeyValue) (bool, error) {
	// A key at or under the records prefix is stray or malformed; never slice past it.
	if len(kv.Key) <= c.keyPrefixLength {
		return false, fmt.Errorf("record cursor: key length %d <= subspace prefix length %d (malformed or out-of-range key under the records subspace)", len(kv.Key), c.keyPrefixLength)
	}
	suffix, pkEnd, err := tuplefast.SplitKeySuffix(kv.Key[c.keyPrefixLength:])
	if err != nil {
		return false, fmt.Errorf("failed to parse key suffix: %w", err)
	}
	end := c.keyPrefixLength + pkEnd
	c.nextPrefix = kv.Key[:end:end]
	c.nextIndex = suffix
	c.sizes.KeyCount, c.sizes.KeySize, c.sizes.ValueSize = 1, len(kv.Key), len(kv.Value)
	c.parts = append(c.parts[:0], kv.Value)
	c.haveData = true
	switch {
	case suffix == unsplitRecord:
		c.sizes.IsSplit = false
		return !c.reverse, nil
	case !c.reverse && suffix == recordVersionSuffix:
		version, err := c.inlineVersion(kv.Value)
		if err != nil {
			return false, err
		}
		c.sizes.VersionedInline = true
		c.nextVersion = version
		c.parts = c.parts[:0]
		c.haveData = false
		return false, nil
	case (c.reverse && suffix != recordVersionSuffix) || suffix == startSplitRecord:
		c.sizes.IsSplit = true
		return false, nil
	default:
		return false, &FoundSplitWithoutStartError{NextIndex: suffix, Reverse: c.reverse, KeyTuple: c.nextPrimaryKey()}
	}
}

func (c *keyValueCursor) appendNext(kv fdb.KeyValue) (bool, error) {
	// Java reads the first element after the record's prefix.
	rest := kv.Key[len(c.nextPrefix):]
	n := tuplefast.Skip(rest)
	if n <= 0 || n > len(rest) {
		return false, fmt.Errorf("failed to parse key suffix of %x", kv.Key)
	}
	index, _, err := tuplefast.SplitKeySuffix(rest[:n])
	if err != nil {
		return false, fmt.Errorf("failed to parse key suffix: %w", err)
	}
	c.sizes.KeyCount++
	c.sizes.KeySize += len(kv.Key)
	c.sizes.ValueSize += len(kv.Value)
	switch {
	case !c.reverse && c.nextIndex == recordVersionSuffix && (index == unsplitRecord || index == startSplitRecord):
		c.parts = append(c.parts[:0], kv.Value)
		c.haveData = true
		c.nextIndex = index
		c.sizes.IsSplit = index == startSplitRecord
		return index == unsplitRecord, nil
	case !c.reverse && index == c.nextIndex+1:
		c.parts = append(c.parts, kv.Value)
		c.nextIndex = index
		return false, nil
	case c.reverse && index == recordVersionSuffix && (c.nextIndex == startSplitRecord || c.nextIndex == unsplitRecord):
		version, err := c.inlineVersion(kv.Value)
		if err != nil {
			return false, err
		}
		c.nextVersion = version
		c.nextIndex = index
		return true, nil
	case c.reverse && index == c.nextIndex-1 && index != recordVersionSuffix:
		c.parts = append(c.parts, kv.Value)
		c.nextIndex = index
		return false, nil
	}
	expected := c.nextIndex + 1
	if c.reverse {
		expected = c.nextIndex - 1
	}
	if (c.reverse && expected == startSplitRecord) || (!c.reverse && c.nextIndex == recordVersionSuffix) {
		return false, &FoundSplitWithoutStartError{NextIndex: index, Reverse: c.reverse, KeyTuple: c.nextPrimaryKey()}
	}
	return false, &FoundSplitOutOfOrderError{Expected: expected, Found: index, KeyTuple: c.nextPrimaryKey()}
}

func (c *keyValueCursor) inlineVersion(value []byte) (*FDBRecordVersion, error) {
	if c.useOldVersionFormat {
		return nil, &RecordCoreError{Message: fmt.Sprintf("Found record version when old format specified (key_tuple=%v)", c.nextPrimaryKey())}
	}
	return unpackVersion(value)
}

// nextPrimaryKey decodes the current record's primary key for error context.
func (c *keyValueCursor) nextPrimaryKey() tuple.Tuple {
	if len(c.nextPrefix) < c.keyPrefixLength {
		return nil
	}
	pk, _ := tuplefast.Unpack(c.nextPrefix[c.keyPrefixLength:])
	return pk
}

// assemble joins the record's values in suffix order; reverse scans read them last first.
func (c *keyValueCursor) assemble() []byte {
	if len(c.parts) == 1 {
		return c.parts[0]
	}
	total := 0
	for _, part := range c.parts {
		total += len(part)
	}
	data := make([]byte, 0, total)
	if c.reverse {
		for i := len(c.parts) - 1; i >= 0; i-- {
			data = append(data, c.parts[i]...)
		}
		return data
	}
	for _, part := range c.parts {
		data = append(data, part...)
	}
	return data
}

// unsplitContinuation is the unsplitter's current continuation.
func (c *keyValueCursor) unsplitContinuation() RecordCursorContinuation {
	if c.contEnd {
		return &EndContinuation{}
	}
	if c.contKey == nil {
		return &StartContinuation{}
	}
	cont, err := c.makeContinuation(c.contKey)
	if err != nil {
		return &StartContinuation{}
	}
	return cont
}

// nextBareRecord is KeyValueCursorBase.onNext mapped to a record, for stores
// whose unsplit records sit at the bare primary key. Skip and the row limit are
// applied by the range read itself, so skipped keys are neither read nor charged.
func (c *keyValueCursor) nextBareRecord(ctx context.Context) (RecordCursorResult[*FDBStoredRecord[proto.Message]], error) {
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	if err := c.tryRecordScan(); err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	if c.halted {
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](c.haltReason, c.lastKeyContinuation()), nil
	}
	kv, ok, err := c.nextKV()
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	if !ok {
		if c.innerLimit > 0 && c.valuesSeen >= c.innerLimit {
			return NewResultNoNext[*FDBStoredRecord[proto.Message]](ReturnLimitReached, c.lastKeyContinuation()), nil
		}
		return NewResultNoNext[*FDBStoredRecord[proto.Message]](SourceExhausted, &EndContinuation{}), nil
	}
	c.scanState.AddBytesScanned(int64(len(kv.Key) + len(kv.Value)))
	if len(kv.Key) <= c.keyPrefixLength {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, fmt.Errorf("record cursor: key length %d <= subspace prefix length %d (malformed or out-of-range key under the records subspace)", len(kv.Key), c.keyPrefixLength)
	}
	record, err := c.storedRecord(kv.Key, kv.Value, nil, sizeInfo{KeyCount: 1, KeySize: len(kv.Key), ValueSize: len(kv.Value)})
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*FDBStoredRecord[proto.Message]]{}, err
	}
	return NewResultWithValue(record, cont), nil
}

// lastKeyContinuation is KeyValueCursorBase's continuation: the last key read, or end.
func (c *keyValueCursor) lastKeyContinuation() RecordCursorContinuation {
	if c.lastKey == nil {
		return &EndContinuation{}
	}
	cont, err := c.makeContinuation(c.lastKey)
	if err != nil {
		return &EndContinuation{}
	}
	return cont
}

// storedRecord is deserializeRecord. prefix is the records-subspace key up to the
// primary key; old-format versions take a separate read.
func (c *keyValueCursor) storedRecord(prefix, value []byte, version *FDBRecordVersion, sizes sizeInfo) (*FDBStoredRecord[proto.Message], error) {
	primaryKey, err := tuplefast.Unpack(prefix[c.keyPrefixLength:])
	if err != nil {
		return nil, fmt.Errorf("failed to unpack primary key: %w", err)
	}
	recordType, protoMessage, wire, err := c.store.deserializeAndDiscover(value)
	if err != nil {
		return nil, &RecordDeserializationError{PrimaryKey: primaryKey, Cause: err}
	}
	if c.useOldVersionFormat {
		version, err = c.store.LoadRecordVersion(primaryKey, c.scanProperties.ExecuteProperties.IsolationLevel.IsSnapshot())
		if err != nil {
			return nil, err
		}
	}
	return &FDBStoredRecord[proto.Message]{
		PrimaryKey:      primaryKey,
		RecordType:      recordType,
		Record:          protoMessage,
		wire:            wire,
		Version:         version,
		Store:           c.store,
		KeyCount:        sizes.KeyCount,
		KeySize:         sizes.KeySize,
		ValueSize:       sizes.ValueSize,
		Split:           sizes.IsSplit,
		VersionedInline: sizes.VersionedInline,
	}, nil
}

// nextKV is the inner KeyValueCursor's read; it charges nothing itself.
func (c *keyValueCursor) nextKV() (fdb.KeyValue, bool, error) {
	if c.innerDone {
		return fdb.KeyValue{}, false, nil
	}
	if !c.iterator.Advance() {
		// Advance returns false on exhaustion OR error; Get surfaces the error
		// (transaction_too_old 1007) instead of treating it as end-of-data.
		if _, err := c.iterator.Get(); err != nil {
			return fdb.KeyValue{}, false, fmt.Errorf("key-value cursor: iterator advance: %w", err)
		}
		c.innerDone = true
		return fdb.KeyValue{}, false, nil
	}
	kv, err := c.iterator.Get()
	if err != nil {
		return fdb.KeyValue{}, false, fmt.Errorf("key-value cursor: iterator get: %w", err)
	}
	c.valuesSeen++
	c.lastKey = kv.Key
	return kv, true, nil
}

// makeContinuation aliases key without copying: RangeIterator never reuses
// returned key bytes, so the lazily marshaled suffix stays valid.
func (c *keyValueCursor) makeContinuation(key fdb.Key) (*keyValueContinuation, error) {
	suffix, err := keySuffix(key, c.prefixLength)
	if err != nil {
		return nil, err
	}
	return &keyValueContinuation{suffix: suffix}, nil
}

// initIterator issues the inner KeyValueCursor's range read.
func (c *keyValueCursor) initIterator() error {
	rng, err := c.bounds.keyRange(c.continuation, c.reverse)
	if err != nil {
		return err
	}
	inner := c.scanProperties
	inner.ExecuteProperties.ReturnedRowLimit = c.innerLimit
	options := indexRangeOptions(inner)
	var r fdb.Range = rng
	if c.selectorSkip > 0 {
		// KeyValueCursorBase skips by moving the begin (or reverse end) selector.
		begin, end := fdb.FirstGreaterOrEqual(rng.Begin), fdb.FirstGreaterOrEqual(rng.End)
		if c.reverse {
			end.Offset -= c.selectorSkip
		} else {
			begin.Offset += c.selectorSkip
		}
		r = fdb.SelectorRange{Begin: begin, End: end}
	}
	tx := c.store.context.Transaction()
	var rangeResult fdb.RangeResult
	if c.scanProperties.ExecuteProperties.IsolationLevel == SnapshotIsolation {
		rangeResult = tx.Snapshot().GetRange(r, options)
	} else {
		rangeResult = tx.GetRange(r, options)
	}
	c.iterator = rangeResult.Iterator()
	return nil
}

// Close releases resources held by the cursor
func (c *keyValueCursor) Close() error {
	c.closed = true
	return nil
}

func (c *keyValueCursor) IsClosed() bool { return c.closed }
