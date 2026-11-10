package recordlayer

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"math/big"

	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
)

// PartitionCoordinateBound is an inclusive or exclusive BIGINT coordinate.
type PartitionCoordinateBound struct {
	Value     int64
	Inclusive bool
}

// PartitionCoordinateRange uses nil for an unbounded endpoint. Empty explicitly
// selects no coordinates; otherwise Lower must not exceed Upper. Skip, as on a
// record scan, applies after the continuation on each cursor invocation.
type PartitionCoordinateRange struct {
	Lower, Upper *PartitionCoordinateBound
	Empty        bool
}

// PartitionSourceEndpoint represents a mathematical interval endpoint outside
// BIGINT as unbounded, never as a wrapped BIGINT. Upper endpoints are exclusive.
type PartitionSourceEndpoint struct {
	Value     int64
	Unbounded bool
}

// PartitionEntry describes a retained active partition, including empty ones.
// Its source interval is [Lower, UpperExclusive), clipped by the BIGINT domain.
type PartitionEntry struct {
	Coordinate     int64
	Generation     int64
	Lower          PartitionSourceEndpoint
	UpperExclusive PartitionSourceEndpoint
}

func partitionSourceInterval(coordinate, width int64) (PartitionSourceEndpoint, PartitionSourceEndpoint) {
	low := new(big.Int).Mul(big.NewInt(coordinate), big.NewInt(width))
	high := new(big.Int).Add(new(big.Int).Set(low), big.NewInt(width))
	endpoint := func(n *big.Int) PartitionSourceEndpoint {
		if !n.IsInt64() {
			return PartitionSourceEndpoint{Unbounded: true}
		}
		return PartitionSourceEndpoint{Value: n.Int64()}
	}
	return endpoint(low), endpoint(high)
}

func normalizePartitionRange(r PartitionCoordinateRange) (PartitionCoordinateRange, error) {
	if r.Empty && (r.Lower != nil || r.Upper != nil) {
		return r, &RecordCoreArgumentError{Message: "empty partition range cannot carry bounds"}
	}
	if r.Lower != nil {
		bound := *r.Lower
		r.Lower = &bound
	}
	if r.Upper != nil {
		bound := *r.Upper
		r.Upper = &bound
	}
	if r.Lower != nil && r.Upper != nil && r.Lower.Value > r.Upper.Value {
		return r, &RecordCoreArgumentError{Message: "reversed partition coordinate range"}
	}
	return r, nil
}

func partitionCoordinateBounds(prefix []byte, r PartitionCoordinateRange, spec *PartitionSpec) (fdb.KeyRange, error) {
	bounds, err := fdb.PrefixRange(prefix)
	if err != nil {
		return bounds, err
	}
	minimum, _ := spec.Coordinate(math.MinInt64)
	maximum, _ := spec.Coordinate(math.MaxInt64)
	empty := r.Empty || (r.Lower != nil && (r.Lower.Value > maximum || (r.Lower.Value == maximum && !r.Lower.Inclusive))) ||
		(r.Upper != nil && (r.Upper.Value < minimum || (r.Upper.Value == minimum && !r.Upper.Inclusive)))
	if r.Lower != nil {
		begin := append(bytes.Clone(prefix), (tuple.Tuple{r.Lower.Value}).Pack()...)
		if !r.Lower.Inclusive {
			begin, err = fdb.Strinc(begin)
			if err != nil {
				return bounds, err
			}
		}
		bounds.Begin = fdb.Key(begin)
	}
	if r.Upper != nil {
		end := append(bytes.Clone(prefix), (tuple.Tuple{r.Upper.Value}).Pack()...)
		if r.Upper.Inclusive {
			end, err = fdb.Strinc(end)
			if err != nil {
				return bounds, err
			}
		}
		bounds.End = fdb.Key(end)
	}
	if empty || bytes.Compare(bounds.Begin.FDBKey(), bounds.End.FDBKey()) >= 0 {
		bounds.End = fdb.Key(bytes.Clone(bounds.Begin.FDBKey()))
	}
	return bounds, nil
}

// partitionReadStop is internal: only the owning root turns it into a stopped
// result. Nested helpers cannot accidentally renew the root's initial attempt.
type partitionReadStop struct{ reason NoNextReason }

func (e *partitionReadStop) Error() string {
	return fmt.Sprintf("partition read stopped: %d", e.reason)
}

type partitionReadBudget struct {
	owner        *partitionReadCursor
	state, clock *ScanLimiterState
	props        ExecuteProperties
	initialUsed  bool
	allow        bool
}

func (b *partitionReadBudget) beginAttempt() {
	b.allow = !b.initialUsed
	b.initialUsed = true
}

func (b *partitionReadBudget) before(ctx context.Context) error {
	if err := b.owner.checkLocal(ctx); err != nil {
		return err
	}
	before := b.state.RecordsScanned()
	b.state.AddRecordScanned()
	reason := SourceExhausted
	switch {
	case b.props.ScannedRecordsLimit > 0 && before >= b.props.ScannedRecordsLimit && (!b.allow || b.props.FailOnScanLimitReached):
		reason = ScanLimitReached
	case !b.allow && b.props.ScannedBytesLimit > 0 && b.state.BytesScanned() >= b.props.ScannedBytesLimit:
		reason = ByteLimitReached
	case !b.allow && b.props.TimeLimit > 0 && b.clock.Elapsed() >= b.props.TimeLimit:
		reason = TimeLimitReached
	}
	if reason == SourceExhausted {
		return nil
	}
	if b.props.FailOnScanLimitReached {
		return &ScanLimitReachedError{Reason: reason}
	}
	return &partitionReadStop{reason: reason}
}

func (b *partitionReadBudget) point(ctx context.Context, key []byte) ([]byte, error) {
	if err := b.before(ctx); err != nil {
		return nil, err
	}
	tx := b.owner.store.context.ReadTransaction(b.props.IsolationLevel.IsSnapshot())
	value, err := tx.Get(fdb.Key(key)).Get()
	if value != nil {
		b.state.AddBytesScanned(int64(len(key) + len(value)))
	}
	if liveErr := b.owner.checkLocal(ctx); liveErr != nil {
		return nil, liveErr
	}
	return value, err
}

func (b *partitionReadBudget) first(ctx context.Context, bounds fdb.KeyRange, reverse bool) (fdb.KeyValue, bool, error) {
	if err := b.before(ctx); err != nil {
		return fdb.KeyValue{}, false, err
	}
	tx := b.owner.store.context.ReadTransaction(b.props.IsolationLevel.IsSnapshot())
	rows, err := tx.GetRange(bounds, fdb.RangeOptions{Limit: 1, Mode: fdb.StreamingModeExact, Reverse: reverse}).GetSliceWithError()
	for _, row := range rows {
		b.state.AddBytesScanned(int64(len(row.Key) + len(row.Value)))
	}
	if liveErr := b.owner.checkLocal(ctx); liveErr != nil {
		return fdb.KeyValue{}, false, liveErr
	}
	if err != nil {
		return fdb.KeyValue{}, false, err
	}
	if len(rows) == 0 {
		return fdb.KeyValue{}, false, nil
	}
	if len(rows) != 1 || bytes.Compare(rows[0].Key, bounds.Begin.FDBKey()) < 0 || bytes.Compare(rows[0].Key, bounds.End.FDBKey()) >= 0 {
		return fdb.KeyValue{}, false, partitionStateError(b.owner.rt, 0, "bounded partition observation returned an invalid range", nil)
	}
	return rows[0], true, nil
}

type partitionLandmark struct {
	coordinate int64
	kv         fdb.KeyValue
}

type partitionReadCursor struct {
	store             *FDBRecordStore
	rt                *RecordType
	layout            *PartitionKeyLayout
	layoutIdentity    []byte
	coordinateRange   PartitionCoordinateRange
	reverse           bool
	kind              int64
	state             *partitionContextState
	localRevision     int64
	localNonce        []byte
	checkpoint        partitionCheckpoint
	resume            bool
	initialized       bool
	emptyStore        bool
	closed            bool
	budget            partitionReadBudget
	prefixes          [6][]byte
	bounds            [6]fdb.KeyRange
	heads             [3]*partitionLandmark
	returned, skipped int
	terminal          *RecordCursorResult[PartitionEntry]
	terminalErr       error
}

func newPartitionReadCursor(store *FDBRecordStore, recordTypeName string, r PartitionCoordinateRange, continuation []byte, props ScanProperties, kind int64) (*partitionReadCursor, error) {
	rt := store.metaData.GetRecordType(recordTypeName)
	layout, err := DescribePartitionKey(rt)
	if err != nil {
		return nil, err
	}
	if layout == nil {
		return nil, &PartitionMetadataError{RecordType: recordTypeName, Message: "partition read requires a partitioned record type"}
	}
	r, err = normalizePartitionRange(r)
	if err != nil {
		return nil, err
	}
	identity, err := partitionLayoutIdentity(store, rt)
	if err != nil {
		return nil, err
	}
	ss := partitionSubspace(store.subspace, rt)
	c := &partitionReadCursor{store: store, rt: rt, layout: layout, layoutIdentity: identity, coordinateRange: r, reverse: props.IsReverse(), kind: kind}
	c.prefixes = [6][]byte{ss.Sub("active").Bytes(), store.recordsSubspace.Sub(rt.GetRecordTypeKey()).Bytes(), store.subspace.Sub(RecordVersionKey, rt.GetRecordTypeKey()).Bytes(), ss.Bytes(), store.recordsSubspace.Sub(rt.GetRecordTypeKey()).Bytes(), store.subspace.Sub(RecordVersionKey, rt.GetRecordTypeKey()).Bytes()}
	for i, prefix := range c.prefixes {
		if i == 3 {
			c.bounds[i], err = fdb.PrefixRange(prefix)
		} else {
			c.bounds[i], err = partitionCoordinateBounds(prefix, r, layout.spec)
		}
		if err != nil {
			return nil, err
		}
	}
	if continuation != nil {
		if err := c.parseContinuation(continuation); err != nil {
			return nil, err
		}
	}
	if err := c.contextLive(context.Background()); err != nil {
		return nil, err
	}
	c.state = store.partitionContextState()
	c.state.mu.Lock()
	if c.state.revisions == nil {
		c.state.revisions = make(map[string]int64)
	}
	table := string(ss.Bytes())
	c.localRevision = c.state.revisions[table]
	c.state.revisions[table] = c.localRevision
	if !c.state.nonceReady {
		_, c.state.nonceErr = io.ReadFull(store.context.Env(), c.state.nonce[:])
		c.state.nonceReady = true
	}
	c.localNonce = bytes.Clone(c.state.nonce[:])
	nonceErr := c.state.nonceErr
	c.state.mu.Unlock()
	if nonceErr != nil {
		return nil, partitionStateError(rt, 0, "cannot generate partition read context nonce", nonceErr)
	}
	if c.resume {
		same := bytes.Equal(c.checkpoint.nonce, c.localNonce)
		if (c.checkpoint.bound && !same) || (same && c.checkpoint.revision != c.localRevision) {
			return nil, &PartitionContinuationInvalidatedError{Reason: "context-bound token or local topology revision changed"}
		}
	}
	state := props.ExecuteProperties.ScanState
	if state == nil {
		state = NewScanLimiterStateIn(store.context.Env())
	}
	props.ExecuteProperties.ScanState = state
	c.budget = partitionReadBudget{owner: c, state: state, clock: transactionClock(store.context, props.ExecuteProperties, state), props: props.ExecuteProperties}
	return c, nil
}

func (c *partitionReadCursor) contextLive(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rc := c.store.context
	if rc.ctx != nil {
		if err := rc.ctx.Err(); err != nil {
			return err
		}
	}
	if rc.deactivated.Load() || rc.partitionReadEnded.Load() {
		return errTransactionNotActive()
	}
	rc.recordPhysical.mu.Lock()
	sealed := rc.recordPhysical.sealed
	rc.recordPhysical.mu.Unlock()
	if sealed {
		return errTransactionNotActive()
	}
	return nil
}

func (c *partitionReadCursor) checkLocal(ctx context.Context) error {
	if c.closed {
		return &RecordCoreError{Message: "partition cursor is closed"}
	}
	if err := c.contextLive(ctx); err != nil {
		return err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.failed != nil {
		return c.state.failed
	}
	if c.localRevision != c.state.revisions[string(c.prefixes[3])] {
		return &PartitionContinuationInvalidatedError{Reason: "local topology changed during partition read"}
	}
	return nil
}

func (c *partitionReadCursor) initialize(ctx context.Context) error {
	if c.initialized {
		return nil
	}
	ss := partitionSubspace(c.store.subspace, c.rt)
	header, err := c.budget.point(ctx, ss.Pack(tuple.Tuple{"header"}))
	if err != nil {
		return err
	}
	var epoch int64
	if header == nil {
		// Missing identity is empty only after bounded WHOLE-type probes. A
		// range outside the source domain must not hide an uninitialized store.
		for _, prefix := range [][]byte{ss.Bytes(), c.prefixes[1], c.prefixes[2]} {
			bounds, err := fdb.PrefixRange(prefix)
			if err != nil {
				return err
			}
			_, found, err := c.budget.first(ctx, bounds, false)
			if err != nil {
				return err
			}
			if found {
				return partitionStateError(c.rt, 0, "routing state or physical data exist without a header", nil)
			}
		}
		c.emptyStore = true
		for i := 0; i < 3; i++ {
			c.checkpoint.positions[i] = partitionPosition{state: partitionPositionEnd}
		}
	} else {
		if err := decodePartitionHeader(c.rt, c.layout.spec, header); err != nil {
			return err
		}
		raw, err := c.budget.point(ctx, ss.Pack(tuple.Tuple{"epoch"}))
		if err != nil {
			return err
		}
		epoch, err = partitionCounter(c.rt, 0, raw)
		if err != nil {
			return err
		}
	}
	if c.resume && (!bytes.Equal(header, c.checkpoint.header) || epoch != c.checkpoint.epoch) {
		return &PartitionContinuationInvalidatedError{Reason: "persisted incarnation or topology epoch changed"}
	}
	c.checkpoint.header, c.checkpoint.epoch = bytes.Clone(header), epoch
	c.checkpoint.nonce, c.checkpoint.revision = bytes.Clone(c.localNonce), c.localRevision
	c.checkpoint.bound = c.localRevision != 0
	c.initialized = true
	return c.checkLocal(ctx)
}

func (c *partitionReadCursor) coordinate(value any) (int64, error) {
	p, ok := value.(int64)
	if !ok {
		return 0, partitionStateError(c.rt, 0, "coordinate is not a BIGINT", nil)
	}
	minimum, _ := c.layout.spec.Coordinate(math.MinInt64)
	maximum, _ := c.layout.spec.Coordinate(math.MaxInt64)
	if p < minimum || p > maximum {
		return 0, partitionStateError(c.rt, p, "coordinate is outside the source domain", nil)
	}
	return p, nil
}

func (c *partitionReadCursor) positionCoordinate(stream int, key []byte) (int64, error) {
	prefix := c.prefixes[stream]
	if !bytes.HasPrefix(key, prefix) {
		return 0, partitionStateError(c.rt, 0, "position outside its stream", nil)
	}
	parts, err := tuple.Unpack(key[len(prefix):])
	if err != nil || len(parts) == 0 || !bytes.Equal(key[len(prefix):], parts.Pack()) {
		return 0, partitionStateError(c.rt, 0, "noncanonical partition key", err)
	}
	return c.coordinate(parts[0])
}

func (c *partitionReadCursor) validatePosition(stream int, key []byte) error {
	bounds := c.bounds[stream]
	if bytes.Compare(key, bounds.Begin.FDBKey()) < 0 || bytes.Compare(key, bounds.End.FDBKey()) >= 0 {
		return fmt.Errorf("partition stream %d position outside requested bounds", stream)
	}
	if stream == 3 {
		_, err := c.protocolKey(key)
		return err
	}
	p, err := c.positionCoordinate(stream, key)
	if err != nil {
		return err
	}
	if stream <= 2 {
		expected := append(bytes.Clone(c.prefixes[stream]), (tuple.Tuple{p}).Pack()...)
		if !bytes.Equal(key, expected) {
			return fmt.Errorf("landmark continuation is not a whole coordinate prefix")
		}
	} else {
		ss := c.store.recordsSubspace
		if stream == 5 {
			ss = c.store.subspace.Sub(RecordVersionKey)
		}
		parts, err := ss.Unpack(fdb.Key(key))
		want := c.rt.PrimaryKey.ColumnSize()
		if stream == 4 && !(c.store.omitUnsplitRecordSuffix() && !c.store.metaData.IsSplitLongRecords()) {
			want++
		}
		if err != nil || len(parts) != want || !bytes.Equal(key, ss.Pack(parts)) {
			return fmt.Errorf("invalid physical primary-key continuation")
		}
		if stream == 4 && want > c.rt.PrimaryKey.ColumnSize() {
			suffix, ok := parts[len(parts)-1].(int64)
			if !ok || suffix < recordVersionSuffix || (suffix == recordVersionSuffix && (!c.reverse || c.store.useOldVersionFormat())) {
				return fmt.Errorf("invalid consumed record suffix")
			}
		}
	}
	return nil
}

func (c *partitionReadCursor) remaining(stream int, wholePrefix bool) fdb.KeyRange {
	bounds := c.bounds[stream]
	p := c.checkpoint.positions[stream]
	if p.state == partitionPositionEnd {
		bounds.End = bounds.Begin
	} else if p.state == partitionPositionKey {
		if c.reverse {
			bounds.End = fdb.Key(p.key)
		} else if wholePrefix {
			begin, _ := fdb.Strinc(p.key)
			bounds.Begin = fdb.Key(begin)
		} else {
			bounds.Begin = fdb.Key(append(bytes.Clone(p.key), 0))
		}
	}
	return bounds
}

func (c *partitionReadCursor) fetchLandmark(ctx context.Context, stream int) error {
	if c.heads[stream] != nil || c.checkpoint.positions[stream].state == partitionPositionEnd {
		return nil
	}
	bounds := c.remaining(stream, stream != 0)
	if bytes.Compare(bounds.Begin.FDBKey(), bounds.End.FDBKey()) >= 0 {
		c.checkpoint.positions[stream] = partitionPosition{state: partitionPositionEnd}
		return nil
	}
	kv, found, err := c.budget.first(ctx, bounds, c.reverse)
	if err != nil {
		return err
	}
	if !found {
		c.checkpoint.positions[stream] = partitionPosition{state: partitionPositionEnd}
		return nil
	}
	p, err := c.positionCoordinate(stream, kv.Key)
	if err != nil {
		return err
	}
	parts, err := tuple.Unpack(kv.Key[len(c.prefixes[stream]):])
	want := c.rt.PrimaryKey.ColumnSize() - 1
	if stream == 0 {
		want = 1
	} else if stream == 1 && !(c.store.omitUnsplitRecordSuffix() && !c.store.metaData.IsSplitLongRecords()) {
		want++
	}
	if err != nil || len(parts) != want {
		return partitionStateError(c.rt, p, "malformed physical or directory landmark key", err)
	}
	c.heads[stream] = &partitionLandmark{coordinate: p, kv: kv}
	return nil
}

// nextDirectory uses safe consumed positions; fetching a head never consumes it.
// It does no speculative next-head I/O when an entry has been selected.
func (c *partitionReadCursor) nextDirectory(ctx context.Context) (PartitionEntry, bool, error) {
	for stream := 0; stream < 3; stream++ {
		if err := c.fetchLandmark(ctx, stream); err != nil {
			return PartitionEntry{}, false, err
		}
	}
	directory := c.heads[0]
	for stream := 1; stream < 3; stream++ {
		physical := c.heads[stream]
		if physical != nil && (directory == nil || (!c.reverse && physical.coordinate < directory.coordinate) || (c.reverse && physical.coordinate > directory.coordinate)) {
			return PartitionEntry{}, false, partitionStateError(c.rt, physical.coordinate, "occupied physical prefix has no active entry", nil)
		}
	}
	if directory == nil {
		return PartitionEntry{}, false, nil
	}
	p := directory.coordinate
	generation, err := partitionCounter(c.rt, p, directory.kv.Value)
	if err != nil {
		return PartitionEntry{}, false, err
	}
	raw, err := c.budget.point(ctx, partitionSubspace(c.store.subspace, c.rt).Pack(tuple.Tuple{"fence", p}))
	if err != nil {
		return PartitionEntry{}, false, err
	}
	fence, live, err := decodePartitionFence(c.rt, p, raw)
	if err != nil {
		return PartitionEntry{}, false, err
	}
	if !live || fence != generation {
		return PartitionEntry{}, false, partitionStateError(c.rt, p, "active entry and live fence disagree", nil)
	}
	if err := c.checkLocal(ctx); err != nil {
		return PartitionEntry{}, false, err
	}
	c.checkpoint.positions[0] = partitionPosition{state: partitionPositionKey, key: bytes.Clone(directory.kv.Key)}
	c.heads[0] = nil
	for stream := 1; stream < 3; stream++ {
		if head := c.heads[stream]; head != nil && head.coordinate == p {
			c.checkpoint.positions[stream] = partitionPosition{state: partitionPositionKey, key: append(bytes.Clone(c.prefixes[stream]), (tuple.Tuple{p}).Pack()...)}
			c.heads[stream] = nil
		}
	}
	low, high := partitionSourceInterval(p, c.layout.spec.IntervalWidth())
	return PartitionEntry{Coordinate: p, Generation: generation, Lower: low, UpperExclusive: high}, true, nil
}

// ScanPartitions enumerates sparse active coordinates and detects occupied record
// or legacy-version prefixes without active state. It does not audit every body
// or fence-only orphan; use CheckPartitionConsistency for that stronger check.
// Open/Create/CreateOrOpen load the required StoreInfo; an unchecked Build
// handle without loaded StoreInfo is refused without storage I/O.
func (store *FDBRecordStore) ScanPartitions(recordTypeName string, r PartitionCoordinateRange, continuation []byte, props ScanProperties) RecordCursor[PartitionEntry] {
	c, err := newPartitionReadCursor(store, recordTypeName, r, continuation, props, partitionDirectoryKind)
	if err != nil {
		return &errorCursor[PartitionEntry]{err: err}
	}
	return c
}

func (c *partitionReadCursor) OnNext(ctx context.Context) (RecordCursorResult[PartitionEntry], error) {
	if err := c.checkLocal(ctx); err != nil {
		return RecordCursorResult[PartitionEntry]{}, err
	}
	if c.terminalErr != nil {
		return RecordCursorResult[PartitionEntry]{}, c.terminalErr
	}
	if c.terminal != nil {
		return *c.terminal, nil
	}
	stop := func(reason NoNextReason) (RecordCursorResult[PartitionEntry], error) {
		if err := c.checkLocal(ctx); err != nil {
			return RecordCursorResult[PartitionEntry]{}, err
		}
		var continuation RecordCursorContinuation = c.continuation()
		if reason == SourceExhausted {
			continuation = &EndContinuation{}
		}
		result := NewResultNoNext[PartitionEntry](reason, continuation)
		c.terminal = &result
		return result, nil
	}
	if limit := c.budget.props.ReturnedRowLimit; limit > 0 && c.returned >= limit {
		return stop(ReturnLimitReached)
	}
	for {
		c.budget.beginAttempt()
		err := c.initialize(ctx)
		var entry PartitionEntry
		var found bool
		if err == nil {
			entry, found, err = c.nextDirectory(ctx)
		}
		if halted, ok := err.(*partitionReadStop); ok {
			return stop(halted.reason)
		}
		if err != nil {
			c.terminalErr = err
			return RecordCursorResult[PartitionEntry]{}, err
		}
		if !found {
			return stop(SourceExhausted)
		}
		if c.skipped < c.budget.props.Skip {
			c.skipped++
			continue
		}
		if err := c.checkLocal(ctx); err != nil {
			return RecordCursorResult[PartitionEntry]{}, err
		}
		c.returned++
		return NewResultWithValue(entry, c.continuation()), nil
	}
}

func (c *partitionReadCursor) Close() error   { c.closed = true; return nil }
func (c *partitionReadCursor) IsClosed() bool { return c.closed }
