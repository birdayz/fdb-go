package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
)

// countKVCursor scans an aggregate index and returns IndexEntry values.
// By default, decodes values as little-endian int64 (for COUNT/SUM/MIN_EVER_LONG/MAX_EVER_LONG).
// Set tupleValues=true to decode values as tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
type countKVCursor struct {
	index         *Index
	indexSubspace subspace.Subspace
	tx            fdb.Transaction
	tupleRange    TupleRange
	continuation  []byte
	scanProps     ScanProperties

	iterator     rangeIterator
	closed       bool
	returned     int
	prefixLength int
	lastCont     []byte
	tupleValues  bool // if true, decode values as tuple-packed bytes

	// Scan accounting for RFC-106a resource limits. Each aggregate-index KV is a
	// pre-aggregated group, so one KV read == one entry returned; `returned`
	// doubles as the scanned-records count (like index_scan's recordsRead).
	bytesScanned int64
	startTime    time.Time
}

// newCountIndexCursor creates a cursor that scans a COUNT index.
// Each entry's Value is the count decoded from the little-endian int64 FDB value.
func newCountIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		prefixLength:  len(indexSubspace.FDBKey()),
	}
}

// newTupleValueIndexCursor creates a cursor that scans an aggregate index with tuple-packed values.
// Each entry's Value is decoded from tuple-packed bytes (for MIN_EVER_TUPLE/MAX_EVER_TUPLE).
func newTupleValueIndexCursor(index *Index, indexSubspace subspace.Subspace, tx fdb.Transaction,
	scanRange TupleRange, continuation []byte, scanProperties ScanProperties,
) RecordCursor[*IndexEntry] {
	return &countKVCursor{
		index:         index,
		indexSubspace: indexSubspace,
		tx:            tx,
		tupleRange:    scanRange,
		continuation:  continuation,
		scanProps:     scanProperties,
		prefixLength:  len(indexSubspace.FDBKey()),
		tupleValues:   true,
	}
}

func (c *countKVCursor) initIterator() error {
	c.startTime = time.Now() // per-page time-limit reference (RFC-106a)
	// Compute begin from TupleRange low endpoint
	var begin fdb.Key
	switch c.tupleRange.LowEndpoint {
	case EndpointTypeTreeStart:
		begin = c.indexSubspace.FDBKey()
	case EndpointTypeRangeInclusive:
		begin = c.indexSubspace.Pack(c.tupleRange.Low)
	case EndpointTypeRangeExclusive:
		packed := c.indexSubspace.Pack(c.tupleRange.Low)
		var err error
		begin, err = fdb.Strinc(packed)
		if err != nil {
			return fmt.Errorf("strinc for exclusive low endpoint: %w", err)
		}
	default:
		begin = c.indexSubspace.FDBKey()
	}

	// Compute end from TupleRange high endpoint
	var end fdb.Key
	switch c.tupleRange.HighEndpoint {
	case EndpointTypeTreeEnd:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.FDBKey()
	case EndpointTypeRangeInclusive:
		packed := c.indexSubspace.Pack(c.tupleRange.High)
		end = append(packed, 0xFF)
	case EndpointTypeRangeExclusive:
		end = c.indexSubspace.Pack(c.tupleRange.High)
	default:
		_, endKey := c.indexSubspace.FDBRangeKeys()
		end = endKey.FDBKey()
	}

	// Apply continuation — overrides one endpoint
	if c.continuation != nil {
		innerCont := unwrapContinuation(c.continuation)
		fullKey := append(append(fdb.Key(nil), c.indexSubspace.FDBKey()...), innerCont...)

		if c.scanProps.IsReverse() {
			end = fullKey
		} else {
			begin = append(fullKey, 0x00)
		}
	}

	rng := fdb.KeyRange{Begin: begin, End: end}
	options := fdb.RangeOptions{
		Reverse: c.scanProps.IsReverse(),
	}

	if c.scanProps.ExecuteProperties.ReturnedRowLimit > 0 {
		limit := c.scanProps.ExecuteProperties.ReturnedRowLimit - c.returned
		if limit <= 0 {
			limit = 1
		}
		options.Limit = saturatingAdd(limit, 1)
	}

	c.iterator = c.tx.GetRange(rng, options).Iterator()
	return nil
}

func (c *countKVCursor) OnNext(ctx context.Context) (RecordCursorResult[*IndexEntry], error) {
	if c.closed {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("cursor is closed")
	}

	// Honor a statement deadline / cancellation (RFC-106a).
	if err := ctx.Err(); err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}

	if c.iterator == nil {
		if err := c.initIterator(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, err
		}
	}

	executeProps := c.scanProps.GetExecuteProperties()

	// Scanned-records limit (RFC-106a parity): one aggregate-index KV per entry,
	// so `returned` is the scanned-records count. noNextOrFail → 54F01 in fail mode.
	if executeProps.ScannedRecordsLimit > 0 && c.returned >= executeProps.ScannedRecordsLimit {
		return noNextOrFail[*IndexEntry](executeProps, ScanLimitReached, &BytesContinuation{bytes: c.lastCont})
	}

	// Time limit (free initial pass like the other leaf cursors).
	if executeProps.TimeLimit > 0 && c.returned > 0 && time.Since(c.startTime) >= executeProps.TimeLimit {
		return NewResultNoNext[*IndexEntry](TimeLimitReached, &BytesContinuation{bytes: c.lastCont}), nil
	}

	// Scanned-bytes limit (free initial pass).
	if executeProps.ScannedBytesLimit > 0 && c.returned > 0 && c.bytesScanned >= executeProps.ScannedBytesLimit {
		return noNextOrFail[*IndexEntry](executeProps, ByteLimitReached, &BytesContinuation{bytes: c.lastCont})
	}

	// Check row limit
	if executeProps.ReturnedRowLimit > 0 && c.returned >= executeProps.ReturnedRowLimit {
		if c.iterator.Advance() {
			return NewResultNoNext[*IndexEntry](
				ReturnLimitReached,
				&BytesContinuation{bytes: c.lastCont},
			), nil
		}
		// Advance() returns false on exhaustion OR error — check Get() for the stored
		// error so a transient transaction_too_old (1007) / timeout at the row-limit
		// boundary surfaces instead of being read as end-of-data (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan at row-limit boundary: %w", err)
		}
		return NewResultNoNext[*IndexEntry](
			SourceExhausted,
			&EndContinuation{},
		), nil
	}

	if !c.iterator.Advance() {
		// Advance() returns false on exhaustion OR error — surface the stored Get()
		// error rather than reporting SourceExhausted (silent row loss).
		if _, err := c.iterator.Get(); err != nil {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan: %w", err)
		}
		return NewResultNoNext[*IndexEntry](SourceExhausted, &EndContinuation{}), nil
	}

	kv, err := c.iterator.Get()
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index scan: %w", err)
	}
	c.bytesScanned += int64(len(kv.Key) + len(kv.Value)) // RFC-106a byte accounting

	// Unpack key using fastUnpack for zero-alloc integer decode.
	prefixLen := len(c.indexSubspace.Bytes())
	if len(kv.Key) < prefixLen {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index key shorter than subspace prefix")
	}
	keyTuple, err := fastUnpack(kv.Key[prefixLen:])
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack count index key: %w", err)
	}

	// Decode value based on index type
	var valueTuple tuple.Tuple
	if c.tupleValues {
		// TUPLE variants: decode value as tuple-packed bytes
		if len(kv.Value) > 0 {
			var err2 error
			valueTuple, err2 = fastUnpack(kv.Value)
			if err2 != nil {
				return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("unpack tuple value: %w", err2)
			}
		}
	} else {
		// COUNT/SUM/LONG variants: decode value as little-endian int64
		count := int64(0)
		if len(kv.Value) > 0 && len(kv.Value) < 8 {
			return RecordCursorResult[*IndexEntry]{}, fmt.Errorf("count index %q: corrupted value: expected 8 bytes, got %d", c.index.Name, len(kv.Value))
		} else if len(kv.Value) >= 8 {
			count = int64(binary.LittleEndian.Uint64(kv.Value))
		}
		valueTuple = tuple.Tuple{count}
	}

	entry := &IndexEntry{
		Index: c.index,
		Key:   keyTuple,
		Value: valueTuple,
	}

	c.returned++

	cont, err := c.makeContinuation(kv.Key)
	if err != nil {
		return RecordCursorResult[*IndexEntry]{}, err
	}
	c.lastCont = cont

	return NewResultWithValue(entry, &BytesContinuation{bytes: cont}), nil
}

func (c *countKVCursor) makeContinuation(key fdb.Key) ([]byte, error) {
	var keySuffix []byte
	if len(key) > c.prefixLength {
		keySuffix = key[c.prefixLength:]
	} else {
		keySuffix = key
	}
	return wrapContinuation(keySuffix)
}

func (c *countKVCursor) Close() error {
	c.closed = true
	return nil
}

func (c *countKVCursor) IsClosed() bool { return c.closed }
