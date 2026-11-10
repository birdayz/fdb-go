package recordlayer

import (
	"bytes"
	"context"
	"errors"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
)

// PartitionConsistencyKind identifies bounded, successfully checked work. A page
// containing no corruption is not a completeness claim until SourceExhausted.
type PartitionConsistencyKind string

const (
	PartitionConsistencyDirectory PartitionConsistencyKind = "directory"
	PartitionConsistencyProtocol  PartitionConsistencyKind = "protocol"
	PartitionConsistencyFence     PartitionConsistencyKind = "fence"
	PartitionConsistencyBody      PartitionConsistencyKind = "body"
	PartitionConsistencyVersion   PartitionConsistencyKind = "version"
)

type PartitionConsistencyResult struct {
	Kind          PartitionConsistencyKind
	Coordinate    int64
	HasCoordinate bool
	PhysicalKey   tuple.Tuple
}

type partitionChecker struct {
	read              *partitionReadCursor
	returned, skipped int
	terminal          *RecordCursorResult[PartitionConsistencyResult]
	terminalErr       error
}

// CheckPartitionConsistency is a read-only, paged checked-work cursor. It checks
// the table's entire protocol namespace (including unknown keys and tombstones),
// and every native/RYW body and legacy version in the selected coordinate range.
// It never repairs state. Unordered context-only orphan version mutations are
// not a native ordered stream and are not censused; normal pending versions on
// readable bodies are supported. A failed partition operation cannot pass.
// Open/Create/CreateOrOpen load the required StoreInfo; an unchecked Build
// handle without loaded StoreInfo is refused without storage I/O.
func (store *FDBRecordStore) CheckPartitionConsistency(recordTypeName string, r PartitionCoordinateRange, continuation []byte, props ScanProperties) RecordCursor[PartitionConsistencyResult] {
	read, err := newPartitionReadCursor(store, recordTypeName, r, continuation, props, partitionCheckerKind)
	if err != nil {
		return &errorCursor[PartitionConsistencyResult]{err: err}
	}
	return &partitionChecker{read: read}
}

func (c *partitionChecker) OnNext(ctx context.Context) (RecordCursorResult[PartitionConsistencyResult], error) {
	r := c.read
	if err := r.checkLocal(ctx); err != nil {
		return RecordCursorResult[PartitionConsistencyResult]{}, err
	}
	if c.terminalErr != nil {
		return RecordCursorResult[PartitionConsistencyResult]{}, c.terminalErr
	}
	if c.terminal != nil {
		return *c.terminal, nil
	}
	stop := func(reason NoNextReason) (RecordCursorResult[PartitionConsistencyResult], error) {
		if err := r.checkLocal(ctx); err != nil {
			return RecordCursorResult[PartitionConsistencyResult]{}, err
		}
		var continuation RecordCursorContinuation = r.continuation()
		if reason == SourceExhausted {
			continuation = &EndContinuation{}
		}
		result := NewResultNoNext[PartitionConsistencyResult](reason, continuation)
		c.terminal = &result
		return result, nil
	}
	if limit := r.budget.props.ReturnedRowLimit; limit > 0 && c.returned >= limit {
		return stop(ReturnLimitReached)
	}
	for {
		r.budget.beginAttempt()
		err := r.initialize(ctx)
		var item PartitionConsistencyResult
		var found bool
		if err == nil {
			item, found, err = c.nextWork(ctx)
		}
		var halted *partitionReadStop
		if errors.As(err, &halted) {
			return stop(halted.reason)
		}
		if err != nil {
			c.terminalErr = err
			return RecordCursorResult[PartitionConsistencyResult]{}, err
		}
		if !found {
			if r.checkpoint.phase == 4 || r.emptyStore {
				return stop(SourceExhausted)
			}
			// Even an empty phase consumes this root attempt. The next phase
			// cannot renew its allowance merely because no work was emitted.
			continue
		}
		if c.skipped < r.budget.props.Skip {
			c.skipped++
			continue
		}
		if err := r.checkLocal(ctx); err != nil {
			return RecordCursorResult[PartitionConsistencyResult]{}, err
		}
		c.returned++
		return NewResultWithValue(item, r.continuation()), nil
	}
}

func (c *partitionChecker) nextWork(ctx context.Context) (PartitionConsistencyResult, bool, error) {
	r := c.read
	if r.emptyStore {
		return PartitionConsistencyResult{}, false, nil
	}
	switch r.checkpoint.phase {
	case 0:
		entry, found, err := r.nextDirectory(ctx)
		if err != nil || !found {
			if err == nil {
				r.checkpoint.phase = 1
			}
			return PartitionConsistencyResult{}, false, err
		}
		return PartitionConsistencyResult{Kind: PartitionConsistencyDirectory, Coordinate: entry.Coordinate, HasCoordinate: true}, true, nil
	case 1:
		return r.checkProtocolWork(ctx)
	case 2:
		bounds := r.remaining(4, false)
		row, position, err := r.readBody(ctx, bounds, nil)
		if err != nil {
			return PartitionConsistencyResult{}, false, err
		}
		if row == nil {
			r.checkpoint.positions[4] = partitionPosition{state: partitionPositionEnd}
			r.checkpoint.phase = 3
			return PartitionConsistencyResult{}, false, nil
		}
		p, err := r.validateBody(ctx, row)
		if err != nil {
			return PartitionConsistencyResult{}, false, err
		}
		r.checkpoint.positions[4] = partitionPosition{state: partitionPositionKey, key: bytes.Clone(position)}
		return PartitionConsistencyResult{Kind: PartitionConsistencyBody, Coordinate: p, HasCoordinate: true, PhysicalKey: clonePartitionTuple(row.PrimaryKey)}, true, nil
	case 3:
		return r.checkLegacyWork(ctx)
	default:
		return PartitionConsistencyResult{}, false, nil
	}
}

func clonePartitionTuple(pk tuple.Tuple) tuple.Tuple {
	cloned, _ := tuple.Unpack(pk.Pack())
	return cloned
}

type partitionProtocolKey struct {
	kind string
	p    int64
}

func (r *partitionReadCursor) protocolKey(key []byte) (partitionProtocolKey, error) {
	ss := partitionSubspace(r.store.subspace, r.rt)
	parts, err := ss.Unpack(fdb.Key(key))
	if err != nil || len(parts) == 0 || !bytes.Equal(key, ss.Pack(parts)) {
		return partitionProtocolKey{}, partitionStateError(r.rt, 0, "malformed protocol key", err)
	}
	kind, ok := parts[0].(string)
	if !ok {
		return partitionProtocolKey{}, partitionStateError(r.rt, 0, "unknown protocol key", nil)
	}
	switch kind {
	case "header", "epoch":
		if len(parts) == 1 {
			return partitionProtocolKey{kind: kind}, nil
		}
	case "active", "fence":
		if len(parts) == 2 {
			p, err := r.coordinate(parts[1])
			return partitionProtocolKey{kind: kind, p: p}, err
		}
	}
	return partitionProtocolKey{}, partitionStateError(r.rt, 0, "unknown or malformed protocol key", nil)
}

func (r *partitionReadCursor) checkProtocolWork(ctx context.Context) (PartitionConsistencyResult, bool, error) {
	bounds := r.remaining(3, false)
	var kv fdb.KeyValue
	var found bool
	var err error
	if bytes.Compare(bounds.Begin.FDBKey(), bounds.End.FDBKey()) < 0 {
		kv, found, err = r.budget.first(ctx, bounds, r.reverse)
	}
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	if !found {
		r.checkpoint.positions[3] = partitionPosition{state: partitionPositionEnd}
		r.checkpoint.phase = 2
		return PartitionConsistencyResult{}, false, nil
	}
	key, err := r.protocolKey(kv.Key)
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	item := PartitionConsistencyResult{Kind: PartitionConsistencyProtocol}
	ss := partitionSubspace(r.store.subspace, r.rt)
	switch key.kind {
	case "header":
		if err := decodePartitionHeader(r.rt, r.layout.spec, kv.Value); err != nil {
			return item, false, err
		}
		if !bytes.Equal(kv.Value, r.checkpoint.header) {
			return item, false, &PartitionContinuationInvalidatedError{Reason: "header changed during checker"}
		}
	case "epoch":
		epoch, err := partitionCounter(r.rt, 0, kv.Value)
		if err != nil {
			return item, false, err
		}
		if epoch != r.checkpoint.epoch {
			return item, false, &PartitionContinuationInvalidatedError{Reason: "epoch changed during checker"}
		}
	case "active":
		generation, err := partitionCounter(r.rt, key.p, kv.Value)
		if err != nil {
			return item, false, err
		}
		raw, err := r.budget.point(ctx, ss.Pack(tuple.Tuple{"fence", key.p}))
		if err != nil {
			return item, false, err
		}
		fence, live, err := decodePartitionFence(r.rt, key.p, raw)
		if err != nil {
			return item, false, err
		}
		if !live || fence != generation {
			return item, false, partitionStateError(r.rt, key.p, "active entry and live fence disagree", nil)
		}
		item.HasCoordinate, item.Coordinate = true, key.p
	case "fence":
		generation, live, err := decodePartitionFence(r.rt, key.p, kv.Value)
		if err != nil {
			return item, false, err
		}
		raw, err := r.budget.point(ctx, ss.Pack(tuple.Tuple{"active", key.p}))
		if err != nil {
			return item, false, err
		}
		if !live {
			if raw != nil {
				return item, false, partitionStateError(r.rt, key.p, "inactive fence has an active entry", nil)
			}
		} else {
			active, err := partitionCounter(r.rt, key.p, raw)
			if err != nil {
				return item, false, err
			}
			if active != generation {
				return item, false, partitionStateError(r.rt, key.p, "live fence has no matching active entry", nil)
			}
		}
		item.Kind, item.HasCoordinate, item.Coordinate = PartitionConsistencyFence, true, key.p
	}
	if err := r.checkLocal(ctx); err != nil {
		return item, false, err
	}
	r.checkpoint.positions[3] = partitionPosition{state: partitionPositionKey, key: bytes.Clone(kv.Key)}
	return item, true, nil
}

// The checker supplies a bounded native iterator to the existing unsplitter.
// Only this iterator accounts reads; the unsplitter's private counters are nil,
// so lookahead is charged once when actually fetched, not again from a buffer.
type partitionBodyIterator struct {
	read   *partitionReadCursor
	ctx    context.Context
	bounds fdb.KeyRange
	kv     fdb.KeyValue
	err    error
	done   bool
}

func (it *partitionBodyIterator) Advance() bool {
	if it.done || it.err != nil || bytes.Compare(it.bounds.Begin.FDBKey(), it.bounds.End.FDBKey()) >= 0 {
		return false
	}
	var found bool
	it.kv, found, it.err = it.read.budget.first(it.ctx, it.bounds, it.read.reverse)
	if it.err != nil || !found {
		it.done = true
		return false
	}
	if it.err = it.read.validateBodyKey(it.kv.Key); it.err != nil {
		return false
	}
	// Once a record starts, non-fail soft limits may finish its framing,
	// including lookahead. There is no invented constant split-record size.
	if !it.read.budget.props.FailOnScanLimitReached {
		it.read.budget.allow = true
	}
	if it.read.reverse {
		it.bounds.End = fdb.Key(bytes.Clone(it.kv.Key))
	} else {
		it.bounds.Begin = fdb.Key(append(bytes.Clone(it.kv.Key), 0))
	}
	return true
}

func (it *partitionBodyIterator) Get() (fdb.KeyValue, error) { return it.kv, it.err }

func (r *partitionReadCursor) validateBodyKey(key []byte) error {
	parts, err := r.store.recordsSubspace.Unpack(fdb.Key(key))
	want := r.rt.PrimaryKey.ColumnSize()
	withSuffix := !(r.store.omitUnsplitRecordSuffix() && !r.store.metaData.IsSplitLongRecords())
	if withSuffix {
		want++
	}
	if err != nil || len(parts) != want || !bytes.Equal(key, r.store.recordsSubspace.Pack(parts)) {
		return partitionStateError(r.rt, 0, "malformed complete record key", err)
	}
	if withSuffix {
		suffix, ok := parts[len(parts)-1].(int64)
		if !ok || suffix < recordVersionSuffix {
			return partitionStateError(r.rt, 0, "malformed record suffix", nil)
		}
	}
	return nil
}

func (r *partitionReadCursor) readLegacyVersion(ctx context.Context, pk tuple.Tuple) (*FDBRecordVersion, error) {
	if !r.store.metaData.IsStoreRecordVersions() {
		return nil, nil
	}
	key := r.store.subspace.Sub(RecordVersionKey).Pack(pk)
	if local, ok := r.store.context.GetLocalVersion(key); ok {
		return IncompleteVersion(local)
	}
	raw, err := r.budget.point(ctx, key)
	if err != nil {
		return nil, err
	}
	version, err := decodeLoadedVersionValue(raw, true)
	if err != nil {
		return nil, partitionStateError(r.rt, 0, "corrupt legacy record version", err)
	}
	return version, nil
}

func (r *partitionReadCursor) readBody(ctx context.Context, bounds fdb.KeyRange, knownLegacy *FDBRecordVersion) (*FDBStoredRecord[proto.Message], []byte, error) {
	if bytes.Compare(bounds.Begin.FDBKey(), bounds.End.FDBKey()) >= 0 {
		return nil, nil, nil
	}
	it := &partitionBodyIterator{read: r, ctx: ctx, bounds: bounds}
	var legacyErr error
	legacy := func(pk tuple.Tuple) (*FDBRecordVersion, error) {
		if knownLegacy != nil {
			return knownLegacy, nil
		}
		version, err := r.readLegacyVersion(ctx, pk)
		legacyErr = err
		return version, err
	}
	c := &keyValueCursor{
		store: r.store, iterator: it, reverse: r.reverse, keyPrefixLength: len(r.store.recordsSubspace.Bytes()), prefixLength: len(r.store.recordsSubspace.Bytes()),
		omitUnsplitRecordSuffix: r.store.omitUnsplitRecordSuffix() && !r.store.metaData.IsSplitLongRecords(), useOldVersionFormat: r.store.useOldVersionFormat(), partitionLegacyVersion: legacy,
	}
	result, err := c.OnNext(ctx)
	if liveErr := r.checkLocal(ctx); liveErr != nil {
		return nil, nil, liveErr
	}
	if err != nil {
		if it.err != nil {
			return nil, nil, it.err
		}
		if legacyErr != nil {
			return nil, nil, legacyErr
		}
		var stopped *partitionReadStop
		var limit *ScanLimitReachedError
		if errors.As(err, &stopped) || errors.As(err, &limit) {
			return nil, nil, err
		}
		return nil, nil, partitionStateError(r.rt, 0, "record body, split framing or version is corrupt", err)
	}
	if !result.HasNext() {
		return nil, nil, nil
	}
	position := c.contKey
	if c.omitUnsplitRecordSuffix {
		position = c.lastKey
	}
	return result.GetValue(), bytes.Clone(position), nil
}

func (r *partitionReadCursor) validateBody(ctx context.Context, row *FDBStoredRecord[proto.Message]) (int64, error) {
	if err := r.checkLocal(ctx); err != nil {
		return 0, err
	}
	if row.RecordType != r.rt || len(row.PrimaryKey) != r.rt.PrimaryKey.ColumnSize() || !recordTypeKeyEquals(row.PrimaryKey[0], r.rt.GetRecordTypeKey()) {
		return 0, partitionStateError(r.rt, 0, "body type or full physical primary key disagrees with its prefix", nil)
	}
	p, err := r.coordinate(row.PrimaryKey[1])
	if err != nil {
		return 0, err
	}
	actual, err := evaluateKeyFlat(r.rt.PrimaryKey, row, row.Record)
	if liveErr := r.checkLocal(ctx); liveErr != nil {
		return 0, liveErr
	}
	if err != nil {
		return 0, partitionStateError(r.rt, p, "decoded body does not match its complete physical primary key", err)
	}
	actualKey := make(tuple.Tuple, len(actual))
	for i, value := range actual {
		actualKey[i] = value
	}
	if !bytes.Equal(actualKey.Pack(), row.PrimaryKey.Pack()) {
		return 0, partitionStateError(r.rt, p, "decoded body does not match its complete physical primary key", nil)
	}
	return p, nil
}

func (r *partitionReadCursor) checkLegacyWork(ctx context.Context) (PartitionConsistencyResult, bool, error) {
	bounds := r.remaining(5, false)
	var kv fdb.KeyValue
	var found bool
	var err error
	if bytes.Compare(bounds.Begin.FDBKey(), bounds.End.FDBKey()) < 0 {
		kv, found, err = r.budget.first(ctx, bounds, r.reverse)
	}
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	if !found {
		r.checkpoint.positions[5] = partitionPosition{state: partitionPositionEnd}
		r.checkpoint.phase = 4
		return PartitionConsistencyResult{}, false, nil
	}
	legacy := r.store.subspace.Sub(RecordVersionKey)
	pk, err := legacy.Unpack(kv.Key)
	if err != nil || len(pk) != r.rt.PrimaryKey.ColumnSize() || !bytes.Equal(kv.Key, legacy.Pack(pk)) {
		return PartitionConsistencyResult{}, false, partitionStateError(r.rt, 0, "malformed legacy-version primary key", err)
	}
	p, err := r.coordinate(pk[1])
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	version, err := decodeLoadedVersionValue(kv.Value, true)
	if err != nil || version == nil {
		return PartitionConsistencyResult{}, false, partitionStateError(r.rt, p, "corrupt legacy version", err)
	}
	bodyBounds, err := fdb.PrefixRange(r.store.recordsSubspace.Pack(pk))
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	row, _, err := r.readBody(ctx, bodyBounds, version)
	if err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	if row == nil || !bytes.Equal(row.PrimaryKey.Pack(), pk.Pack()) {
		return PartitionConsistencyResult{}, false, partitionStateError(r.rt, p, "legacy version has no matching record body", nil)
	}
	if _, err := r.validateBody(ctx, row); err != nil {
		return PartitionConsistencyResult{}, false, err
	}
	r.checkpoint.positions[5] = partitionPosition{state: partitionPositionKey, key: bytes.Clone(kv.Key)}
	return PartitionConsistencyResult{Kind: PartitionConsistencyVersion, Coordinate: p, HasCoordinate: true, PhysicalKey: clonePartitionTuple(pk)}, true, nil
}

func (c *partitionChecker) Close() error   { return c.read.Close() }
func (c *partitionChecker) IsClosed() bool { return c.read.IsClosed() }
