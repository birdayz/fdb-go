package recordlayer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"fdb.dev/pkg/fdbgo/fdb"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("FDBDatabaseRunner", func() {
	var (
		ctx context.Context
		md  *RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		var err error
		md, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	Describe("RunWithRetry", func() {
		It("succeeds on first attempt", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			result, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				return store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
		})

		It("does not retry non-retryable errors", func() {
			runner := NewFDBDatabaseRunner(sharedDB).SetMaxAttempts(3)
			attempts := 0
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				return nil, errors.New("permanent error")
			})
			Expect(err).To(HaveOccurred())
			Expect(attempts).To(Equal(1))
		})

		It("succeeds on first attempt even with pre-cancelled context", func() {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel() // Cancel immediately — but first attempt still runs

			runner := NewFDBDatabaseRunner(sharedDB)
			_, err := runner.RunWithRetry(cancelCtx, func(rtx *FDBRecordContext) (any, error) {
				return nil, nil
			})
			// Cancellation is only checked before retry delays, not before the first attempt.
			// If the function succeeds on the first try, no retry (and no cancel check) needed.
			Expect(err).NotTo(HaveOccurred())
		})

		It("applies context config", func() {
			runner := NewFDBDatabaseRunner(sharedDB).SetContextConfig(&RecordContextConfig{
				TransactionTimeout: 5 * time.Second,
				Priority:           PriorityBatch,
				TransactionID:      "test-tx-123",
			})

			result, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				return "ok", nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result).To(Equal("ok"))
		})

		It("uses default max attempts of 10", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			Expect(runner.MaxAttempts).To(Equal(10))
		})
	})

	Describe("Builder methods", func() {
		It("chains configuration", func() {
			runner := NewFDBDatabaseRunner(sharedDB).
				SetMaxAttempts(5).
				SetInitialDelay(20 * time.Millisecond).
				SetMaxDelay(2 * time.Second)

			Expect(runner.MaxAttempts).To(Equal(5))
			Expect(runner.InitialDelay).To(Equal(20 * time.Millisecond))
			Expect(runner.MaxDelay).To(Equal(2 * time.Second))
		})
	})

	// Codes match fdb_error_predicate(FDB_ERROR_PREDICATE_RETRYABLE, code) from fdb_c.cpp.
	Describe("isRetryableError", func() {
		DescribeTable("recognizes all retryable FDB error codes",
			func(code int, desc string) {
				err := fdb.Error{Code: code}
				Expect(isRetryableError(err)).To(BeTrue(), "code %d (%s) should be retryable", code, desc)
			},
			// MAYBE_COMMITTED
			Entry("commit_unknown_result", 1021, "commit_unknown_result"),
			Entry("cluster_version_changed", 1039, "cluster_version_changed"),
			// RETRYABLE_NOT_COMMITTED
			Entry("transaction_too_old", 1007, "transaction_too_old"),
			Entry("future_version", 1009, "future_version"),
			Entry("not_committed", 1020, "not_committed"),
			Entry("process_behind", 1037, "process_behind"),
			Entry("database_locked", 1038, "database_locked"),
			Entry("commit_proxy_memory_limit_exceeded", 1042, "commit_proxy_memory_limit_exceeded"),
			Entry("batch_transaction_throttled", 1051, "batch_transaction_throttled"),
			Entry("grv_proxy_memory_limit_exceeded", 1078, "grv_proxy_memory_limit_exceeded"),
			Entry("tag_throttled", 1213, "tag_throttled"),
			Entry("proxy_tag_throttled", 1223, "proxy_tag_throttled"),
			Entry("transaction_throttled_hot_shard", 1235, "transaction_throttled_hot_shard"),
			Entry("transaction_rejected_range_locked", 1242, "transaction_rejected_range_locked"),
		)

		It("rejects non-retryable FDB errors", func() {
			Expect(isRetryableError(fdb.Error{Code: 2000})).To(BeFalse())
			Expect(isRetryableError(fdb.Error{Code: 1025})).To(BeFalse()) // transaction_cancelled
			Expect(isRetryableError(fdb.Error{Code: 1031})).To(BeFalse()) // transaction_timed_out
			Expect(isRetryableError(fdb.Error{Code: 1034})).To(BeFalse()) // future_released
		})

		It("rejects non-FDB errors", func() {
			Expect(isRetryableError(errors.New("not an FDB error"))).To(BeFalse())
		})

		It("detects wrapped FDB errors via errors.As", func() {
			wrapped := fmt.Errorf("context: %w", fdb.Error{Code: 1020})
			Expect(isRetryableError(wrapped)).To(BeTrue())
		})
	})

	Describe("OpenContext", func() {
		It("creates a valid context", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			rctx, err := runner.OpenContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(rctx).NotTo(BeNil())
			Expect(rctx.Transaction()).NotTo(BeNil())
			rctx.Cancel()
		})

		It("applies context config including TransactionID", func() {
			runner := NewFDBDatabaseRunner(sharedDB).SetContextConfig(&RecordContextConfig{
				TransactionTimeout: 5 * time.Second,
				Priority:           PriorityBatch,
				TransactionID:      "open-ctx-test",
			})
			rctx, err := runner.OpenContext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(rctx).NotTo(BeNil())
			rctx.Cancel()
		})
	})

	Describe("retry with backoff", func() {
		It("retries with increasing delay on retryable errors", func() {
			runner := NewFDBDatabaseRunner(sharedDB).
				SetMaxAttempts(4).
				SetInitialDelay(10 * time.Millisecond).
				SetMaxDelay(1 * time.Second)

			attempts := 0
			start := time.Now()
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				if attempts < 3 {
					return nil, fdb.Error{Code: 1020} // not_committed — retryable
				}
				return "done", nil
			})
			elapsed := time.Since(start)
			Expect(err).NotTo(HaveOccurred())
			Expect(attempts).To(Equal(3))
			// Two delay periods: 10ms (attempt 2) + 20ms (attempt 3) = ~30ms nominal.
			// Jitter multiplier is 0.5x-1.5x, so minimum is ~15ms. Use 10ms as safe lower
			// bound to avoid flakiness under load (jitter + scheduling jitter).
			Expect(elapsed).To(BeNumerically(">", 10*time.Millisecond))
		})

		It("gives up after max attempts", func() {
			runner := NewFDBDatabaseRunner(sharedDB).
				SetMaxAttempts(3).
				SetInitialDelay(1 * time.Millisecond)

			attempts := 0
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				attempts++
				return nil, fdb.Error{Code: 1020} // always retryable
			})
			Expect(err).To(HaveOccurred())
			Expect(attempts).To(Equal(3))
		})

		It("cancels between retries when context is cancelled", func() {
			cancelCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			defer cancel()

			runner := NewFDBDatabaseRunner(sharedDB).
				SetMaxAttempts(100).
				SetInitialDelay(100 * time.Millisecond) // Long delay so context cancels during wait

			_, err := runner.RunWithRetry(cancelCtx, func(rtx *FDBRecordContext) (any, error) {
				return nil, fdb.Error{Code: 1020} // retryable
			})
			Expect(err).To(HaveOccurred())
			Expect(errors.Is(err, context.DeadlineExceeded)).To(BeTrue())
		})
	})

	Describe("Commit hooks with runner", func() {
		It("propagates pre-commit check errors", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					return errors.New("pre-commit validation failed")
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("pre-commit validation failed"))
		})

		It("runs pre-commit checks", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			checkRan := false
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					checkRan = true
					return nil
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkRan).To(BeTrue())
		})

		It("runs post-commit hooks", func() {
			runner := NewFDBDatabaseRunner(sharedDB)
			postRan := false
			_, err := runner.RunWithRetry(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddPostCommit(func() {
					postRan = true
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(postRan).To(BeTrue())
		})
	})
})
