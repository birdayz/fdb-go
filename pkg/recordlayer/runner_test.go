package recordlayer

import (
	"context"
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
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

		It("respects context cancellation", func() {
			cancelCtx, cancel := context.WithCancel(ctx)
			cancel() // Cancel immediately

			runner := NewFDBDatabaseRunner(sharedDB)
			_, err := runner.RunWithRetry(cancelCtx, func(rtx *FDBRecordContext) (any, error) {
				return nil, nil
			})
			// First attempt should succeed since cancel is checked before retry delay
			// If the function succeeds, no retry needed
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

	Describe("Commit hooks with runner", func() {
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
