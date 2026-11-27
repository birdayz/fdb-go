// Portions derived from FoundationDB Record Layer (PendingWritesQueue.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package rlcollections

import (
	"encoding/binary"
	"fmt"
	"strings"

	storagepb "fdb.dev/internal/pb/storage"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/anypb"
)

const PendingWritesQueueVersion = 1

// PendingWritesContext supplies transaction, versionstamp, split-save, and timer hooks
// from the record context layer without importing pkg/recordlayer.
type PendingWritesContext interface {
	ReadTransaction(snapshot bool) fdb.ReadTransaction
	Transaction() fdb.WritableTransaction
	ClearRange(prefix fdb.KeyRange)
	NowUnixMilli() int64
	IncompleteVersionstamp() (tuple.Versionstamp, error)
	SaveSplit(sub subspace.Subspace, key tuple.Tuple, serialized []byte) error
	RecordQueueSize(size int64)
	IncrementQueueWrite()
	IncrementQueueClear()
}

// PendingWritesStorageError reports a storage-level pending writes queue corruption or shape error.
type PendingWritesStorageError struct {
	Message       string
	KeyTuple      tuple.Tuple
	Version       *int32
	StoredVersion *int32
	ExpectedType  string
	ActualType    string
	Cause         error
}

func (e *PendingWritesStorageError) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Cause)
	}
	return e.Message
}

func (e *PendingWritesStorageError) Unwrap() error { return e.Cause }

// PendingWritesArgumentError reports an invalid argument passed to PendingWritesQueue.
type PendingWritesArgumentError struct {
	Message string
}

func (e *PendingWritesArgumentError) Error() string { return e.Message }

// PendingWritesInternalError reports an internal PendingWritesQueue invariant failure.
type PendingWritesInternalError struct {
	Message string
}

func (e *PendingWritesInternalError) Error() string { return e.Message }

// PendingWritesQueue stores Java-compatible, versionstamped protobuf payloads.
// Entry and counter subspaces must be disjoint. Capacity is deliberately soft:
// snapshot checks do not serialize concurrent producers.
type PendingWritesQueue[T proto.Message] struct {
	entries         subspace.Subspace
	counter         subspace.Subspace
	maximum         int64
	payloadType     protoreflect.MessageType
	unmarshalAsJava func([]byte, proto.Message) error
}

func NewPendingWritesQueue[T proto.Message](
	entries, counter subspace.Subspace,
	maximum int64,
	prototype T,
	unmarshalAsJava func([]byte, proto.Message) error,
) *PendingWritesQueue[T] {
	return &PendingWritesQueue[T]{
		entries:         entries,
		counter:         counter,
		maximum:         maximum,
		payloadType:     prototype.ProtoReflect().Type(),
		unmarshalAsJava: unmarshalAsJava,
	}
}

// Entries returns the subspace holding queued entries.
func (q *PendingWritesQueue[T]) Entries() subspace.Subspace { return q.entries }

// Counter returns the subspace holding the queue size counter.
func (q *PendingWritesQueue[T]) Counter() subspace.Subspace { return q.counter }

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

func (q *PendingWritesQueue[T]) GetQueueSizeNoConflict(ctx PendingWritesContext) (*int64, error) {
	data, err := ctx.ReadTransaction(true).Get(fdb.Key(q.counter.Bytes())).Get()
	if err != nil {
		return nil, err
	}
	var result *int64
	var size int64
	if data != nil {
		if len(data) < 8 {
			return nil, &PendingWritesStorageError{Message: "Invalid pending writes queue size counter"}
		}
		size = int64(binary.LittleEndian.Uint64(data))
		result = &size
	}
	ctx.RecordQueueSize(size)
	return result, nil
}

func (q *PendingWritesQueue[T]) Enqueue(ctx PendingWritesContext, payload T, incarnation int32) error {
	if q.maximum > 0 {
		size, err := q.GetQueueSizeNoConflict(ctx)
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
	envelope, err := proto.Marshal(&storagepb.PendingWriteItem{
		Version:          proto.Int32(PendingWritesQueueVersion),
		Payload:          packed,
		EnqueueTimestamp: proto.Int64(ctx.NowUnixMilli()),
	})
	if err != nil {
		return err
	}
	stamp, err := ctx.IncompleteVersionstamp()
	if err != nil {
		return err
	}
	key := tuple.Tuple{int64(incarnation), stamp}
	if err := ctx.SaveSplit(q.entries, key, envelope); err != nil {
		return err
	}
	q.changeSize(ctx.Transaction(), 1)
	ctx.IncrementQueueWrite()
	return nil
}

func (q *PendingWritesQueue[T]) changeSize(tx fdb.WritableTransaction, delta int64) {
	var data [8]byte
	binary.LittleEndian.PutUint64(data[:], uint64(delta))
	tx.Add(fdb.Key(q.counter.Bytes()), data[:])
}

func (q *PendingWritesQueue[T]) IsQueueEmpty(tx fdb.ReadTransaction) (bool, error) {
	rows, err := tx.GetRange(q.entries, fdb.RangeOptions{Limit: 1}).GetSliceWithError()
	return len(rows) == 0, err
}

func (q *PendingWritesQueue[T]) ClearEntry(ctx PendingWritesContext, entry *PendingWritesQueueEntry[T]) error {
	if entry == nil {
		return &PendingWritesArgumentError{Message: "pending writes queue entry is nil"}
	}
	// Java enforces this in its package-private entry constructor. Go callers
	// can construct entries directly, so validate before packing a clear prefix.
	if len(entry.Key) != 2 {
		return &PendingWritesStorageError{Message: "Unexpected queue key shape", KeyTuple: entry.Key}
	}
	incomplete, err := entry.Key.HasIncompleteVersionstamp()
	if err != nil {
		return err
	}
	if incomplete {
		return &PendingWritesArgumentError{Message: "cannot clear incomplete pending writes queue key"}
	}
	prefix, err := fdb.PrefixRange(q.entries.Pack(entry.Key))
	if err != nil {
		return err
	}
	tx := ctx.Transaction()
	if err := tx.AddReadConflictRange(prefix); err != nil {
		return err
	}
	ctx.ClearRange(prefix)
	q.changeSize(tx, -1)
	ctx.IncrementQueueClear()
	return nil
}

// Decode decodes a raw queue entry key and envelope payload into a typed PendingWritesQueueEntry.
func (q *PendingWritesQueue[T]) Decode(key tuple.Tuple, data []byte) (*PendingWritesQueueEntry[T], error) {
	envelope := &storagepb.PendingWriteItem{}
	e := &PendingWritesStorageError{KeyTuple: key}
	if err := q.unmarshalAsJava(data, envelope); err != nil {
		e.Message = "Failed to parse pending writes queue entry"
		e.Cause = err
		return nil, e
	}
	if envelope.GetVersion() > PendingWritesQueueVersion {
		e.Version, e.StoredVersion = proto.Int32(PendingWritesQueueVersion), proto.Int32(envelope.GetVersion())
		e.Message = "Pending writes queue entry version is newer than this reader supports"
		return nil, e
	}
	packed := envelope.GetPayload()
	url := packed.GetTypeUrl()
	if !pendingQueueAnyHasType(packed, q.payloadType.Descriptor().FullName()) {
		e.ExpectedType, e.ActualType = string(q.payloadType.Descriptor().FullName()), url
		e.Message = "Pending writes queue entry payload type does not match the queue's bound type"
		return nil, e
	}
	payload := q.payloadType.New().Interface()
	if err := q.unmarshalAsJava(packed.GetValue(), payload); err != nil {
		e.ExpectedType = string(q.payloadType.Descriptor().FullName())
		e.Message = "Failed to unpack pending writes queue entry payload"
		e.Cause = err
		return nil, e
	}
	if len(key) != 2 {
		e.Message = "Unexpected queue key shape"
		return nil, e
	}
	typed, ok := payload.(T)
	if !ok {
		return nil, &PendingWritesInternalError{Message: "pending queue payload factory returned incompatible type"}
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

func UnmarshalPendingQueueAny(data *anypb.Any, message proto.Message, unmarshalAsJava func([]byte, proto.Message) error) error {
	name := message.ProtoReflect().Descriptor().FullName()
	if !pendingQueueAnyHasType(data, name) {
		return fmt.Errorf("pending queue Any type URL %q must end with /%s", data.GetTypeUrl(), name)
	}
	return unmarshalAsJava(data.GetValue(), message)
}
