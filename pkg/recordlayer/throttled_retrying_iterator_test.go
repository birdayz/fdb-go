package recordlayer

import (
	"context"
	"errors"
	"testing"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Throttled retrying iterator", func() {
	It("replays failed commits from the committed continuation and commits the final empty batch", func() {
		ctx := context.Background()
		root := specSubspace()
		queue := NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 100, &gen.Order{})
		_, err := sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			for id := int64(1); id <= 3; id++ {
				if err := queue.Enqueue(rc, &gen.Order{OrderId: proto.Int64(id)}, 0); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		attempts := 0
		var starts [][]byte
		var limits []int
		var seen []int64
		failure := &RecordCoreError{Message: "deliberate commit check failure"}
		iterator := newThrottledRetryingIterator(NewFDBDatabaseRunner(sharedDB),
			func(_ context.Context, rc *FDBRecordContext, cont []byte, limit int) (RecordCursor[*PendingWritesQueueEntry[*gen.Order]], error) {
				attempts++
				starts = append(starts, append([]byte(nil), cont...))
				limits = append(limits, limit)
				if attempts == 2 {
					rc.AddCommitCheck(func() error { return failure })
				}
				rc.Transaction().Set(fdb.Key(root.Sub("heartbeat").Bytes()), []byte{byte(attempts)})
				props := ForwardScan()
				props.ExecuteProperties.ReturnedRowLimit = limit
				return queue.GetQueueCursor(rc, props, cont), nil
			},
			func(_ context.Context, rc *FDBRecordContext, entry *PendingWritesQueueEntry[*gen.Order], quota *iterationQuota) error {
				seen = append(seen, entry.Payload.GetOrderId())
				quota.deleted++
				return queue.ClearEntry(rc, entry)
			})
		var successful []int
		iterator.onSuccess = func(*iterationQuota) { successful = append(successful, attempts) }
		iterator.maxDeletes = 1
		defer iterator.Close()
		Expect(iterator.iterateAll(ctx)).To(Succeed())
		Expect(attempts).To(Equal(5))
		Expect(successful).To(Equal([]int{1, 3, 4, 5}))
		Expect(seen).To(Equal([]int64{1, 2, 2, 3}))
		Expect(starts[1]).NotTo(BeEmpty())
		Expect(starts[2]).To(Equal(starts[1]))
		Expect(limits).To(Equal([]int{0, 0, 1, 1, 1}))
		_, err = sharedDB.Run(ctx, func(rc *FDBRecordContext) (any, error) {
			empty, err := queue.IsQueueEmpty(rc)
			Expect(empty).To(BeTrue())
			Expect(err).NotTo(HaveOccurred())
			value, err := rc.Transaction().Get(fdb.Key(root.Sub("heartbeat").Bytes())).Get()
			Expect(value).To(Equal([]byte{5}), "final empty attempt must commit its heartbeat")
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("exhausts exactly 100 retries on a non-FDB error without nested retry", func() {
		root := specSubspace()
		queue := NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 100, &gen.Order{})
		attempts := 0
		failure := &RecordCoreError{Message: "persistent cursor initialization failure"}
		iterator := newThrottledRetryingIterator(NewFDBDatabaseRunner(sharedDB),
			func(_ context.Context, rc *FDBRecordContext, _ []byte, limit int) (RecordCursor[*PendingWritesQueueEntry[*gen.Order]], error) {
				attempts++
				if attempts > 1 {
					Expect(limit).To(Equal(1))
				}
				rc.Transaction().Set(fdb.Key(root.Sub("uncommitted").Bytes()), []byte{1})
				return nil, failure
			}, func(_ context.Context, _ *FDBRecordContext, _ *PendingWritesQueueEntry[*gen.Order], _ *iterationQuota) error {
				return nil
			})
		defer iterator.Close()
		Expect(iterator.iterateAll(context.Background())).To(BeIdenticalTo(failure))
		Expect(attempts).To(Equal(101))
		_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			value, err := rc.Transaction().Get(fdb.Key(root.Sub("uncommitted").Bytes())).Get()
			Expect(value).To(BeEmpty())
			Expect(err).NotTo(HaveOccurred())
			empty, err := queue.IsQueueEmpty(rc)
			Expect(empty).To(BeTrue())
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	})

	for _, closeIterator := range []bool{false, true} {
		It("cancels the active transaction without retrying or committing closure="+map[bool]string{false: "false", true: "true"}[closeIterator], func() {
			root := specSubspace()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			attempts := 0
			var iterator *throttledRetryingIterator[int]
			iterator = newThrottledRetryingIterator(NewFDBDatabaseRunner(sharedDB),
				func(ctx context.Context, rc *FDBRecordContext, _ []byte, _ int) (RecordCursor[int], error) {
					attempts++
					rc.Transaction().Set(fdb.Key(root.Sub("uncommitted").Bytes()), []byte{1})
					if closeIterator {
						iterator.Close()
					} else {
						cancel()
					}
					<-ctx.Done()
					return nil, ctx.Err()
				}, func(context.Context, *FDBRecordContext, int, *iterationQuota) error { return nil })
			defer iterator.Close()
			err := iterator.iterateAll(ctx)
			if closeIterator {
				var closed *RunnerClosedError
				Expect(errors.As(err, &closed)).To(BeTrue())
			} else {
				Expect(errors.Is(err, context.Canceled)).To(BeTrue())
			}
			Expect(attempts).To(Equal(1))
			_, err = sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
				value, err := rc.Transaction().Get(fdb.Key(root.Sub("uncommitted").Bytes())).Get()
				Expect(value).To(BeEmpty())
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
		})
	}

	It("resets retry allowance after commits and increases the adaptive limit after forty successes", func() {
		root := specSubspace()
		queue := NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 100, &gen.Order{})
		_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			for id := int64(1); id <= 45; id++ {
				if err := queue.Enqueue(rc, &gen.Order{OrderId: proto.Int64(id)}, 0); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		var limits []int
		iterator := newThrottledRetryingIterator(NewFDBDatabaseRunner(sharedDB),
			func(_ context.Context, rc *FDBRecordContext, cont []byte, limit int) (RecordCursor[*PendingWritesQueueEntry[*gen.Order]], error) {
				limits = append(limits, limit)
				if len(limits) == 1 || len(limits) == 3 {
					rc.AddCommitCheck(func() error { return &RecordCoreError{Message: "retry allowance reset"} })
				}
				props := ForwardScan()
				props.ExecuteProperties.ReturnedRowLimit = limit
				return queue.GetQueueCursor(rc, props, cont), nil
			}, func(_ context.Context, rc *FDBRecordContext, entry *PendingWritesQueueEntry[*gen.Order], quota *iterationQuota) error {
				quota.deleted++
				return queue.ClearEntry(rc, entry)
			})
		iterator.retries = 1
		iterator.maxDeletes = 1
		defer iterator.Close()
		Expect(iterator.iterateAll(context.Background())).To(Succeed())
		Expect(limits).To(HaveLen(48))
		Expect(limits[0]).To(BeZero())
		for i := 1; i < 43; i++ {
			Expect(limits[i]).To(Equal(1))
		}
		for i := 43; i < len(limits); i++ {
			Expect(limits[i]).To(Equal(5))
		}
	})

	It("closes concurrently during the rate wait without starting another transaction", func() {
		root := specSubspace()
		queue := NewPendingWritesQueue(root.Sub("entries"), root.Sub("size"), 1000, &gen.Order{})
		_, err := sharedDB.Run(context.Background(), func(rc *FDBRecordContext) (any, error) {
			for id := int64(1); id <= 100; id++ {
				if err := queue.Enqueue(rc, &gen.Order{OrderId: proto.Int64(id)}, 0); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		committed := make(chan struct{})
		attempts := 0
		iterator := newThrottledRetryingIterator(NewFDBDatabaseRunner(sharedDB),
			func(_ context.Context, rc *FDBRecordContext, cont []byte, _ int) (RecordCursor[*PendingWritesQueueEntry[*gen.Order]], error) {
				attempts++
				rc.AddPostCommit(func() { close(committed) })
				return queue.GetQueueCursor(rc, ForwardScan(), cont), nil
			}, func(_ context.Context, rc *FDBRecordContext, entry *PendingWritesQueueEntry[*gen.Order], quota *iterationQuota) error {
				quota.deleted++
				return queue.ClearEntry(rc, entry)
			})
		iterator.maxDeletes = 100
		iterator.deletedPerSecond = 1
		defer iterator.Close()
		done := make(chan error, 1)
		go func() { done <- iterator.iterateAll(context.Background()) }()
		Eventually(committed, "10s").Should(BeClosed())
		iterator.Close()
		var result error
		Eventually(done, "2s").Should(Receive(&result))
		var closed *RunnerClosedError
		Expect(errors.As(result, &closed)).To(BeTrue())
		Expect(attempts).To(Equal(1))
	})

	It("rejects a closed iterator before creating a context", func() {
		iterator := newThrottledRetryingIterator[int](nil, nil, nil)
		iterator.Close()
		iterator.Close()
		var closed *RunnerClosedError
		Expect(errors.As(iterator.iterateAll(context.Background()), &closed)).To(BeTrue())
	})
})

func TestThrottledIteratorDelay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		elapsed     int64
		rate, count int
		want        int64
	}{{0, 0, 1, 0}, {0, -1, 1, 0}, {0, 3, 1, 333}, {334, 3, 1, 0}, {100, 10, 2, 100}, {0, 10000, 1, 0}} {
		if got := throttledIteratorDelay(tc.elapsed, tc.rate, tc.count); got != tc.want {
			t.Errorf("delay(%d,%d,%d)=%d want %d", tc.elapsed, tc.rate, tc.count, got, tc.want)
		}
	}
}
