package rlcollections

import (
	"errors"
	"strings"
	"testing"

	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer/metadatapb"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

func TestPendingQueueAnyTypeValidation(t *testing.T) {
	t.Parallel()
	msg := &metadatapb.PendingWritesQueueEntry{}
	name := msg.ProtoReflect().Descriptor().FullName()
	if !pendingQueueAnyHasType(&anypb.Any{TypeUrl: "type.googleapis.com/" + string(name)}, name) {
		t.Fatal("expected prefixed type URL to match")
	}
	if pendingQueueAnyHasType(&anypb.Any{TypeUrl: string(name)}, name) {
		t.Fatal("bare type URL without slash must not match")
	}
	err := UnmarshalPendingQueueAny(&anypb.Any{TypeUrl: string(name)}, msg, proto.Unmarshal)
	if err == nil || !strings.Contains(err.Error(), "must end with") {
		t.Fatalf("got %v, want type URL error", err)
	}
}

func TestPendingWritesQueueDecodeErrors(t *testing.T) {
	t.Parallel()
	q := NewPendingWritesQueue(subspace.Sub("e"), subspace.Sub("c"), 1, &metadatapb.PendingWritesQueueEntry{}, proto.Unmarshal)
	if _, err := q.Decode(tuple.Tuple{int64(1), tuple.Versionstamp{}}, []byte{0xff, 0xff}); err == nil {
		t.Fatal("expected parse error")
	} else {
		var storageErr *PendingWritesStorageError
		if !errors.As(err, &storageErr) {
			t.Fatalf("got %T, want PendingWritesStorageError", err)
		}
	}
	tooLarge := &PendingWritesQueueTooLargeError{CurrentSize: 2, MaxQueueSize: 1}
	if !strings.Contains(tooLarge.Error(), "record_count=2, max_queue_size=1") {
		t.Fatalf("unexpected error string: %v", tooLarge)
	}
}
