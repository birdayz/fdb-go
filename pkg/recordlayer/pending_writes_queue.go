// Portions derived from FoundationDB Record Layer (PendingWritesQueue.java),
// Copyright 2015-2026 Apple Inc. and the FoundationDB project authors
// Licensed under the Apache License, Version 2.0; translated to Go and modified.

package recordlayer

import (
	"fdb.dev/internal/recordlayer/rlcollections"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

const pendingWritesQueueVersion = rlcollections.PendingWritesQueueVersion

type (
	PendingWritesQueueEntry[T proto.Message] = rlcollections.PendingWritesQueueEntry[T]
	PendingWritesQueueTooLargeError          = rlcollections.PendingWritesQueueTooLargeError
)

// PendingWritesQueue stores Java-compatible, versionstamped protobuf payloads.
// Entry and counter subspaces must be disjoint. Capacity is deliberately soft:
// snapshot checks do not serialize concurrent producers.
type PendingWritesQueue[T proto.Message] struct {
	inner *rlcollections.PendingWritesQueue[T]
}

func NewPendingWritesQueue[T proto.Message](entries, counter subspace.Subspace, maximum int64, prototype T) *PendingWritesQueue[T] {
	return &PendingWritesQueue[T]{inner: rlcollections.NewPendingWritesQueue(entries, counter, maximum, prototype, UnmarshalAsJava)}
}

func (q *PendingWritesQueue[T]) GetQueueSizeNoConflict(rc *FDBRecordContext) (*int64, error) {
	res, err := q.inner.GetQueueSizeNoConflict(pendingWritesCtx{rc})
	return res, translatePendingWritesErr(err)
}

func (q *PendingWritesQueue[T]) Enqueue(rc *FDBRecordContext, payload T, incarnation int32) error {
	return translatePendingWritesErr(q.inner.Enqueue(pendingWritesCtx{rc}, payload, incarnation))
}

func (q *PendingWritesQueue[T]) IsQueueEmpty(rc *FDBRecordContext) (bool, error) {
	return q.inner.IsQueueEmpty(rc.Transaction())
}

func (q *PendingWritesQueue[T]) ClearEntry(rc *FDBRecordContext, entry *PendingWritesQueueEntry[T]) error {
	return translatePendingWritesErr(q.inner.ClearEntry(pendingWritesCtx{rc}, entry))
}

func (q *PendingWritesQueue[T]) GetQueueCursor(rc *FDBRecordContext, props ScanProperties, continuation []byte) RecordCursor[*PendingWritesQueueEntry[T]] {
	return newPendingWritesQueueCursor(q.inner, rc, props, continuation)
}

func UnmarshalPendingQueueAny(data *anypb.Any, message proto.Message) error {
	return rlcollections.UnmarshalPendingQueueAny(data, message, UnmarshalAsJava)
}
