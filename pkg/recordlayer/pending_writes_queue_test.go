package recordlayer

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
	"time"

	"fdb.dev/gen"
	"fdb.dev/pkg/dst"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("PendingWritesQueue", func() {
	var ctx context.Context
	var root subspace.Subspace
	var queue *PendingWritesQueue[*gen.Order]
	BeforeEach(func() {
		ctx = context.Background()
		root = specSubspace()
		queue = NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 100, &gen.Order{})
	})
	open := func() *FDBRecordContext {
		tx, err := sharedDB.CreateWritableTransaction()
		Expect(err).NotTo(HaveOccurred())
		rc := NewFDBRecordContext(tx, nil)
		DeferCleanup(rc.Cancel)
		return rc
	}
	readOne := func(rc *FDBRecordContext) *PendingWritesQueueEntry[*gen.Order] {
		cur := queue.GetQueueCursor(rc, ForwardScan(), nil)
		defer cur.Close()
		row, err := cur.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(row.HasNext()).To(BeTrue())
		return row.GetValue()
	}
	It("orders committed incarnation and local versions and maintains the counter", func() {
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			size, err := queue.GetQueueSizeNoConflict(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).To(BeNil())
			for i, incarnation := range []int32{2, 1, 2} {
				if err := queue.Enqueue(rc, &gen.Order{OrderId: proto.Int64(int64(i + 1)), Tags: []string{strings.Repeat("x", 120000)}}, incarnation); err != nil {
					return nil, err
				}
			}
			empty, err := queue.IsQueueEmpty(rc)
			Expect(empty).To(BeTrue())
			Expect(rc.HasVersionMutations()).To(BeTrue())
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			cur := queue.GetQueueCursor(rc, ForwardScan(), nil)
			defer cur.Close()
			ids := []int64{}
			for {
				row, err := cur.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !row.HasNext() {
					break
				}
				entry := row.GetValue()
				ids = append(ids, entry.Payload.GetOrderId())
				Expect(entry.Payload.GetTags()).To(Equal([]string{strings.Repeat("x", 120000)}))
				Expect(entry.EnqueueTimestamp).To(BeNumerically(">", 0))
				Expect(entry.PayloadTypeURL).To(Equal("type.googleapis.com/" + string(entry.Payload.ProtoReflect().Descriptor().FullName())))
				if err := queue.ClearEntry(rc, entry); err != nil {
					return nil, err
				}
			}
			Expect(ids).To(Equal([]int64{2, 1, 3}))
			empty, err := queue.IsQueueEmpty(rc)
			Expect(empty).To(BeTrue())
			Expect(err).NotTo(HaveOccurred())
			size, err := queue.GetQueueSizeNoConflict(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(size).NotTo(BeNil())
			Expect(*size).To(BeZero())
			counter, err := rc.Transaction().Get(fdb.Key(root.Sub("size").Bytes())).Get()
			Expect(counter).To(Equal(make([]byte, 8)))
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})
	It("cancels before scanning without consuming an entry or its continuation", func() {
		seed := open()
		for id := int64(1); id <= 2; id++ {
			Expect(queue.Enqueue(seed, &gen.Order{OrderId: proto.Int64(id)}, 0)).To(Succeed())
		}
		Expect(seed.Commit()).To(Succeed())
		rc := open()
		state := NewScanLimiterState()
		props := DefaultExecuteProperties()
		props.ScanState = state
		cursor := queue.GetQueueCursor(rc, NewScanProperties(props), nil)
		defer cursor.Close()
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := cursor.OnNext(canceled)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(state.RecordsScanned()).To(BeZero())
		for id := int64(1); id <= 2; id++ {
			row, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.HasNext()).To(BeTrue())
			Expect(row.GetValue().Payload.GetOrderId()).To(Equal(id))
			before := state.RecordsScanned()
			_, err = cursor.OnNext(canceled)
			Expect(errors.Is(err, context.Canceled)).To(BeTrue())
			Expect(state.RecordsScanned()).To(Equal(before))
		}
		Expect(cursor.Close()).To(Succeed())
		Expect(cursor.IsClosed()).To(BeTrue())
		Expect(cursor.Close()).To(Succeed())
		_, err = cursor.OnNext(ctx)
		Expect(err).To(MatchError("cursor is closed"))
	})
	It("anchors time at cursor creation and preserves completion and limit precedence", func() {
		for _, tc := range []struct {
			name          string
			count         int
			expired, fail bool
			scan          int
			bytes         int64
			reason        NoNextReason
		}{
			{name: "new cursor ignores old shared-state age", count: 3, reason: SourceExhausted},
			{name: "expired cursor completes one split entry", count: 3, expired: true, reason: TimeLimitReached},
			{name: "exhaustion outranks time", count: 1, expired: true, reason: SourceExhausted},
			{name: "scan outranks byte and time", count: 3, expired: true, scan: 1, bytes: 1, reason: ScanLimitReached},
			{name: "byte outranks time", count: 3, expired: true, bytes: 1, reason: ByteLimitReached},
			{name: "fail mode interrupts assembly", count: 3, expired: true, fail: true, reason: TimeLimitReached},
		} {
			rc := open()
			clock := dst.NewSimClock(dst.Epoch)
			rc.env = &dst.Env{Clock: clock}
			state := NewScanLimiterStateIn(rc.env)
			clock.Advance(time.Hour)
			ss := root.Sub(tc.name)
			for i := 0; i < tc.count; i++ {
				Expect(saveWithSplit(rc, rc.Transaction(), ss, tuple.Tuple{int64(i)}, []byte(strings.Repeat("x", 120000)), true, false, nil, nil)).To(Succeed())
			}
			props := DefaultExecuteProperties()
			props.TimeLimit, props.ScanState = time.Millisecond, state
			props.ScannedRecordsLimit, props.ScannedBytesLimit, props.FailOnScanLimitReached = tc.scan, tc.bytes, tc.fail
			cursor := newRawSplitCursor(rc, ss, NewScanProperties(props), nil)
			defer cursor.Close()
			if tc.expired {
				clock.Advance(time.Millisecond)
			}
			rows := 0
			for {
				row, err := cursor.OnNext(ctx)
				if tc.fail {
					var limit *ScanLimitReachedError
					Expect(errors.As(err, &limit)).To(BeTrue(), "%s: %v", tc.name, err)
					Expect(limit.Reason).To(Equal(tc.reason))
					Expect(rows).To(BeZero())
					break
				}
				Expect(err).NotTo(HaveOccurred(), tc.name)
				if !row.HasNext() {
					Expect(row.GetNoNextReason()).To(Equal(tc.reason), tc.name)
					if tc.expired {
						Expect(rows).To(Equal(1), tc.name)
					} else {
						Expect(rows).To(Equal(tc.count), tc.name)
					}
					again, err := cursor.OnNext(ctx)
					Expect(err).NotTo(HaveOccurred())
					Expect(again).To(Equal(row))
					break
				}
				Expect(row.GetValue().value).To(Equal([]byte(strings.Repeat("x", 120000))))
				rows++
			}
		}
	})
	It("classifies missing starts during raw split assembly", func() {
		for _, reverse := range []bool{false, true} {
			rc := open()
			ss := root.Sub("missing", reverse)
			version, err := packVersion(MinVersion())
			Expect(err).NotTo(HaveOccurred())
			rc.Transaction().Set(ss.Pack(tuple.Tuple{int64(1), recordVersionSuffix}), version)
			rc.Transaction().Set(ss.Pack(tuple.Tuple{int64(1), int64(2)}), []byte("tail"))
			cursor := newRawSplitCursor(rc, ss, ForwardScan().WithReverse(reverse), nil)
			defer cursor.Close()
			_, err = cursor.OnNext(ctx)
			var missing *FoundSplitWithoutStartError
			Expect(errors.As(err, &missing)).To(BeTrue(), "reverse=%t: %v", reverse, err)
			Expect(missing.Reverse).To(Equal(reverse))
			if reverse {
				Expect(missing.NextIndex).To(Equal(recordVersionSuffix))
			} else {
				Expect(missing.NextIndex).To(Equal(int64(2)))
			}
		}
	})
	It("matches reverse unsplitter suffix boundaries", func() {
		rc := open()
		ss := root.Sub("reverse-boundaries")
		rc.Transaction().Set(ss.Pack(tuple.Tuple{int64(1), int64(0)}), []byte("head"))
		rc.Transaction().Set(ss.Pack(tuple.Tuple{int64(1), int64(1)}), []byte("tail"))
		cursor := newRawSplitCursor(rc, ss, ForwardScan().WithReverse(true), nil)
		defer cursor.Close()
		row, err := cursor.OnNext(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(row.HasNext()).To(BeTrue())
		Expect(row.GetValue().value).To(Equal([]byte("headtail")))
		orphan := root.Sub("reverse-orphan")
		rc.Transaction().Set(orphan.Pack(tuple.Tuple{int64(1), recordVersionSuffix}), []byte("invalid-version"))
		orphanCursor := newRawSplitCursor(rc, orphan, ForwardScan().WithReverse(true), nil)
		defer orphanCursor.Close()
		_, err = orphanCursor.OnNext(ctx)
		var missing *FoundSplitWithoutStartError
		Expect(errors.As(err, &missing)).To(BeTrue(), "%v", err)
		Expect(missing.NextIndex).To(Equal(recordVersionSuffix))
	})
	It("overlays context-local versions on raw split records", func() {
		for _, reverse := range []bool{false, true} {
			rc := open()
			ss := root.Sub("local", reverse)
			pk := tuple.Tuple{int64(1)}
			rc.Transaction().Set(ss.Pack(append(pk, unsplitRecord)), []byte("value"))
			rc.AddToLocalVersionCache(ss.Pack(append(pk, recordVersionSuffix)), 37)
			cursor := newRawSplitCursor(rc, ss, ForwardScan().WithReverse(reverse), nil)
			defer cursor.Close()
			row, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(row.HasNext()).To(BeTrue())
			Expect(row.GetValue().value).To(Equal([]byte("value")))
			Expect(row.GetValue().version).NotTo(BeNil())
			Expect(row.GetValue().version.IsComplete()).To(BeFalse())
			Expect(row.GetValue().version.GetLocalVersion()).To(Equal(37))
		}
	})
	It("rejects malformed clear keys without clearing a queue prefix or changing its counter", func() {
		seed := open()
		Expect(queue.Enqueue(seed, &gen.Order{OrderId: proto.Int64(1)}, 0)).To(Succeed())
		Expect(seed.Commit()).To(Succeed())
		for _, key := range []tuple.Tuple{nil, {int64(0)}, {int64(0), tuple.IncompleteVersionstamp(0)}, {int64(0), "extra", "suffix"}} {
			rc := open()
			err := queue.ClearEntry(rc, &PendingWritesQueueEntry[*gen.Order]{Key: key})
			Expect(err).To(HaveOccurred(), "key=%v", key)
			Expect(readOne(rc).Payload.GetOrderId()).To(Equal(int64(1)))
			size, err := queue.GetQueueSizeNoConflict(rc)
			Expect(err).NotTo(HaveOccurred())
			Expect(*size).To(Equal(int64(1)))
			Expect(rc.Commit()).To(Succeed())
		}
	})
	It("rejects capacity within one transaction but allows soft concurrent capacity", func() {
		queue = NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 1, &gen.Order{})
		a, b := open(), open()
		size, err := queue.GetQueueSizeNoConflict(a)
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(BeNil())
		size, err = queue.GetQueueSizeNoConflict(b)
		Expect(err).NotTo(HaveOccurred())
		Expect(size).To(BeNil())
		Expect(queue.Enqueue(a, &gen.Order{OrderId: proto.Int64(1)}, 0)).To(Succeed())
		err = queue.Enqueue(a, &gen.Order{OrderId: proto.Int64(2)}, 0)
		var full *PendingWritesQueueTooLargeError
		Expect(errors.As(err, &full)).To(BeTrue())
		Expect(full.CurrentSize).To(Equal(int64(1)))
		Expect(queue.Enqueue(b, &gen.Order{OrderId: proto.Int64(3)}, 0)).To(Succeed())
		Expect(a.Commit()).To(Succeed())
		Expect(b.Commit()).To(Succeed())
		c := open()
		size, err = queue.GetQueueSizeNoConflict(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(*size).To(Equal(int64(2)))
	})
	It("conflicts concurrent clears of the same split entry without counter drift", func() {
		seed := open()
		Expect(queue.Enqueue(seed, &gen.Order{OrderId: proto.Int64(1), Tags: []string{strings.Repeat("x", 120000)}}, 0)).To(Succeed())
		Expect(seed.Commit()).To(Succeed())
		a, b := open(), open()
		ea, eb := readOne(a), readOne(b)
		Expect(queue.ClearEntry(a, ea)).To(Succeed())
		Expect(queue.ClearEntry(b, eb)).To(Succeed())
		Expect(a.Commit()).To(Succeed())
		err := b.Commit()
		var conflict fdb.Error
		Expect(errors.As(err, &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
		c := open()
		size, err := queue.GetQueueSizeNoConflict(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(*size).To(BeZero())
	})
	It("serializable empty read conflicts with a producer committing first", func() {
		emptyTx, producer := open(), open()
		empty, err := queue.IsQueueEmpty(emptyTx)
		Expect(err).NotTo(HaveOccurred())
		Expect(empty).To(BeTrue())
		Expect(queue.Enqueue(producer, &gen.Order{OrderId: proto.Int64(1)}, 0)).To(Succeed())
		Expect(producer.Commit()).To(Succeed())
		emptyTx.Transaction().Set(root.Pack(nil), []byte("force conflict validation"))
		err = emptyTx.Commit()
		var conflict fdb.Error
		Expect(errors.As(err, &conflict)).To(BeTrue())
		Expect(conflict.Code).To(Equal(1020))
	})
	It("snapshot traversal does not conflict with a concurrent enqueue", func() {
		seed := open()
		Expect(queue.Enqueue(seed, &gen.Order{OrderId: proto.Int64(1)}, 0)).To(Succeed())
		Expect(seed.Commit()).To(Succeed())
		reader, producer := open(), open()
		entry := readOne(reader)
		Expect(queue.Enqueue(producer, &gen.Order{OrderId: proto.Int64(2)}, 0)).To(Succeed())
		Expect(producer.Commit()).To(Succeed())
		Expect(queue.ClearEntry(reader, entry)).To(Succeed())
		Expect(reader.Commit()).To(Succeed())
		c := open()
		size, err := queue.GetQueueSizeNoConflict(c)
		Expect(err).NotTo(HaveOccurred())
		Expect(*size).To(Equal(int64(1)))
		Expect(readOne(c).Payload.GetOrderId()).To(Equal(int64(2)))
	})
	It("uses little endian counters and rejects truncated counters", func() {
		rc := open()
		var bytes [8]byte
		binary.LittleEndian.PutUint64(bytes[:], 258)
		rc.Transaction().Set(fdb.Key(queue.counter.Bytes()), bytes[:])
		size, err := queue.GetQueueSizeNoConflict(rc)
		Expect(err).NotTo(HaveOccurred())
		Expect(*size).To(Equal(int64(258)))
		rc.Transaction().Set(fdb.Key(queue.counter.Bytes()), []byte{1})
		_, err = queue.GetQueueSizeNoConflict(rc)
		var storage *RecordCoreStorageError
		Expect(errors.As(err, &storage)).To(BeTrue())
	})
})

func FuzzPendingQueuePayload(f *testing.F) {
	for _, data := range [][]byte{
		{},
		{8, 1},
		{8, 2},
		{8, 99},
		{8, 1, 8, 99},
		{8, 0x81, 0x80, 0x80, 0x80, 0x10},
		{8, 0xe3, 0x80, 0x80, 0x80, 0x10, 8, 1},
		{8, 0xff, 0xff, 0xff, 0xff, 0x0f, 8, 1},
	} {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		t.Parallel()
		message := &gen.PendingWritesQueueEntry{}
		if err := UnmarshalAsJava(data, message); err != nil {
			return
		}
		if message.GetOperation() != gen.PendingWritesQueueEntry_UPDATE && message.GetOperation() != gen.PendingWritesQueueEntry_DELETE_WHERE {
			t.Fatalf("closed required enum accepted operation %d", message.GetOperation())
		}
		wire, err := proto.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		back := &gen.PendingWritesQueueEntry{}
		if err := UnmarshalAsJava(wire, back); err != nil {
			t.Fatalf("decoded payload no longer decodes after serialization: %v", err)
		}
		if !proto.Equal(message, back) {
			t.Fatalf("queue payload changes during normalization round trip: %v / %v", message, back)
		}
	})
}
