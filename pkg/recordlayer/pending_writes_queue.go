package recordlayer

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

const PendingWritesQueueVersion = 1

var (
	CountPendingWritesQueueWrite                 = Event{"pending_writes_queue_write", "Pending writes queue write", KindCount}
	CountPendingWritesQueueClear                 = Event{"pending_writes_queue_clear", "Pending writes queue clear", KindCount}
	CountPendingWritesQueueOverflowDisabledIndex = Event{"pending_writes_queue_overflow_disabled_index", "Index disabled because pending writes queue overflowed", KindCount}
	SizePendingWritesQueueSize                   = Event{"pending_writes_queue_size", "Pending writes queue size", KindSizeDistribution}
)

// PendingWritesQueue stores Java-compatible, versionstamped protobuf payloads.
// Entry and counter subspaces must be disjoint. Capacity is deliberately soft:
// snapshot checks do not serialize concurrent producers.
type PendingWritesQueue[T proto.Message] struct {
	entries     subspace.Subspace
	counter     subspace.Subspace
	maximum     int64
	payloadType protoreflect.MessageType
}

func NewPendingWritesQueue[T proto.Message](entries, counter subspace.Subspace, maximum int64, prototype T) *PendingWritesQueue[T] {
	return &PendingWritesQueue[T]{entries: entries, counter: counter, maximum: maximum, payloadType: prototype.ProtoReflect().Type()}
}

type PendingWritesQueueEntry[T proto.Message] struct {
	Key              tuple.Tuple
	Payload          T
	PayloadTypeURL   string
	EnqueueTimestamp int64
}

// PendingWritesQueueTooLargeError matches Java's queue capacity exception.
type PendingWritesQueueTooLargeError struct{ CurrentSize, MaxQueueSize int64 }

func (e *PendingWritesQueueTooLargeError) Error() string {
	return fmt.Sprintf("Pending writes queue is full (record_count=%d, max_queue_size=%d)", e.CurrentSize, e.MaxQueueSize)
}

func (q *PendingWritesQueue[T]) GetQueueSizeNoConflict(rc *FDBRecordContext) (*int64, error) {
	data, err := rc.ReadTransaction(true).Get(fdb.Key(q.counter.Bytes())).Get()
	if err != nil {
		return nil, err
	}
	var result *int64
	var size int64
	if data != nil {
		if len(data) < 8 {
			return nil, &RecordCoreStorageError{Message: "Invalid pending writes queue size counter"}
		}
		size = int64(binary.LittleEndian.Uint64(data))
		result = &size
	}
	rc.Timer().RecordSize(SizePendingWritesQueueSize, size)
	return result, nil
}

func (q *PendingWritesQueue[T]) Enqueue(rc *FDBRecordContext, payload T, incarnation int32) error {
	if q.maximum > 0 {
		size, err := q.GetQueueSizeNoConflict(rc)
		if err != nil {
			return err
		}
		if size != nil && *size >= q.maximum {
			return &PendingWritesQueueTooLargeError{CurrentSize: *size, MaxQueueSize: q.maximum}
		}
	}
	packed, err := anypb.New(payload)
	if err != nil {
		return err
	}
	envelope, err := proto.Marshal(&gen.PendingWriteItem{Version: proto.Int32(PendingWritesQueueVersion), Payload: packed, EnqueueTimestamp: proto.Int64(rc.env.Now().UnixMilli())})
	if err != nil {
		return err
	}
	version, err := IncompleteVersion(rc.ClaimLocalVersion())
	if err != nil {
		return err
	}
	stamp, err := version.ToVersionstamp()
	if err != nil {
		return err
	}
	key := tuple.Tuple{int64(incarnation), stamp}
	if err := saveWithSplit(rc, rc.Transaction(), q.entries, key, envelope, true, false, nil, nil); err != nil {
		return err
	}
	q.changeSize(rc, 1)
	rc.Timer().Increment(CountPendingWritesQueueWrite)
	return nil
}

func (q *PendingWritesQueue[T]) changeSize(rc *FDBRecordContext, delta int64) {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], uint64(delta))
	rc.Transaction().Add(fdb.Key(q.counter.Bytes()), data[:])
}

func (q *PendingWritesQueue[T]) IsQueueEmpty(rc *FDBRecordContext) (bool, error) {
	rows, err := rc.Transaction().GetRange(q.entries, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	return len(rows) == 0, err
}

func (q *PendingWritesQueue[T]) ClearEntry(rc *FDBRecordContext, entry *PendingWritesQueueEntry[T]) error {
	if entry == nil {
		return &RecordCoreArgumentError{Message: "pending writes queue entry is nil"}
	}
	// Java enforces this in its package-private entry constructor. Go callers
	// can construct entries directly, so validate before packing a clear prefix.
	if len(entry.Key) != 2 {
		return &RecordCoreStorageError{Message: "Unexpected queue key shape", KeyTuple: entry.Key}
	}
	incomplete, err := entry.Key.HasIncompleteVersionstamp()
	if err != nil {
		return err
	}
	if incomplete {
		return &RecordCoreArgumentError{Message: "cannot clear incomplete pending writes queue key"}
	}
	prefix, err := fdb.PrefixRange(q.entries.Pack(entry.Key))
	if err != nil {
		return err
	}
	if err := rc.Transaction().AddReadConflictRange(prefix); err != nil {
		return err
	}
	rc.ClearRange(prefix)
	q.changeSize(rc, -1)
	rc.Timer().Increment(CountPendingWritesQueueClear)
	return nil
}

func (q *PendingWritesQueue[T]) GetQueueCursor(rc *FDBRecordContext, props ScanProperties, continuation []byte) RecordCursor[*PendingWritesQueueEntry[T]] {
	return &pendingWritesQueueCursor[T]{queue: q, raw: newRawSplitCursor(rc, q.entries, props, continuation)}
}

type pendingWritesQueueCursor[T proto.Message] struct {
	queue *PendingWritesQueue[T]
	raw   *rawSplitCursor
}

func (c *pendingWritesQueueCursor[T]) Close() error   { return c.raw.Close() }
func (c *pendingWritesQueueCursor[T]) IsClosed() bool { return c.raw.IsClosed() }
func (c *pendingWritesQueueCursor[T]) OnNext(ctx context.Context) (RecordCursorResult[*PendingWritesQueueEntry[T]], error) {
	result, err := c.raw.OnNext(ctx)
	if err != nil {
		return RecordCursorResult[*PendingWritesQueueEntry[T]]{}, err
	}
	if !result.HasNext() {
		return NewResultNoNext[*PendingWritesQueueEntry[T]](result.GetNoNextReason(), result.GetContinuation()), nil
	}
	raw := result.GetValue()
	entry, err := c.queue.decode(raw.key, raw.value)
	if err != nil {
		return RecordCursorResult[*PendingWritesQueueEntry[T]]{}, err
	}
	return NewResultWithValue(entry, result.GetContinuation()), nil
}

func (q *PendingWritesQueue[T]) decode(key tuple.Tuple, data []byte) (*PendingWritesQueueEntry[T], error) {
	envelope := &gen.PendingWriteItem{}
	e := &RecordCoreStorageError{KeyTuple: key}
	storageError := func(message string, cause error) error {
		e.Message = message
		if cause != nil {
			return fmt.Errorf("%w: %w", e, cause)
		}
		return e
	}
	if err := proto.Unmarshal(data, envelope); err != nil {
		return nil, storageError("Failed to parse pending writes queue entry", err)
	}
	if envelope.GetVersion() > PendingWritesQueueVersion {
		e.Version, e.StoredVersion = proto.Int32(PendingWritesQueueVersion), proto.Int32(envelope.GetVersion())
		return nil, storageError("Pending writes queue entry version is newer than this reader supports", nil)
	}
	packed := envelope.GetPayload()
	url := packed.GetTypeUrl()
	if !pendingQueueAnyHasType(packed, q.payloadType.Descriptor().FullName()) {
		e.ExpectedType, e.ActualType = string(q.payloadType.Descriptor().FullName()), url
		return nil, storageError("Pending writes queue entry payload type does not match the queue's bound type", nil)
	}
	payload := q.payloadType.New().Interface()
	if err := unmarshalPendingQueuePayload(packed.GetValue(), payload); err != nil {
		e.ExpectedType = string(q.payloadType.Descriptor().FullName())
		return nil, storageError("Failed to unpack pending writes queue entry payload", err)
	}
	if len(key) != 2 {
		return nil, storageError("Unexpected queue key shape", nil)
	}
	typed, ok := payload.(T)
	if !ok {
		return nil, &RecordCoreInternalError{Message: "pending queue payload factory returned incompatible type"}
	}
	return &PendingWritesQueueEntry[T]{Key: key, Payload: typed, PayloadTypeURL: url, EnqueueTimestamp: envelope.GetEnqueueTimestamp()}, nil
}

// Java Any.unpack requires a slash before the full message name. Go's
// Any.UnmarshalTo also accepts the bare name, so use the same validation at
// the outer queue envelope and every consumed nested payload boundary.
func pendingQueueAnyHasType(data *anypb.Any, name protoreflect.FullName) bool {
	url := data.GetTypeUrl()
	slash := strings.LastIndexByte(url, '/')
	return slash >= 0 && url[slash+1:] == string(name)
}

func unmarshalPendingQueueAny(data *anypb.Any, message proto.Message) error {
	name := message.ProtoReflect().Descriptor().FullName()
	if !pendingQueueAnyHasType(data, name) {
		return fmt.Errorf("pending queue Any type URL %q must end with /%s", data.GetTypeUrl(), name)
	}
	return unmarshalPendingQueuePayload(data.GetValue(), message)
}

// Java's proto2 required operation is a closed enum. Go's generated enum is
// open: remove unknown numeric occurrences from the known-field stream before
// decoding, then retain their int32 values as unknown fields, as Java does.
// The last KNOWN occurrence wins.
func unmarshalPendingQueuePayload(data []byte, message proto.Message) error {
	if _, ok := message.(*gen.PendingWritesQueueEntry); !ok {
		return proto.Unmarshal(data, message)
	}
	var known, unknown []byte
	for len(data) > 0 {
		number, kind, tagLength := protowire.ConsumeTag(data)
		if tagLength < 0 {
			return protowire.ParseError(tagLength)
		}
		valueLength := protowire.ConsumeFieldValue(number, kind, data[tagLength:])
		if valueLength < 0 {
			return protowire.ParseError(valueLength)
		}
		field := data[:tagLength+valueLength]
		closedUnknown := false
		var enumValue int32
		if number == 1 && kind == protowire.VarintType {
			value, _ := protowire.ConsumeVarint(data[tagLength:])
			// Java readEnum returns int32 even for an overwide wire varint.
			// Classify after truncation, just as its generated enum switch does.
			enumValue = int32(value)
			closedUnknown = enumValue != 1 && enumValue != 2
		}
		if closedUnknown {
			unknown = protowire.AppendTag(unknown, number, kind)
			unknown = protowire.AppendVarint(unknown, uint64(int64(enumValue)))
		} else {
			known = append(known, field...)
		}
		data = data[len(field):]
	}
	if err := proto.Unmarshal(known, message); err != nil {
		return err
	}
	reflection := message.ProtoReflect()
	reflection.SetUnknown(append(reflection.GetUnknown(), unknown...))
	return nil
}
