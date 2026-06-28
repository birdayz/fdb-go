package recordlayer

import (
	"context"
	"errors"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"fdb.dev/gen"
)

var _ = Describe("Commit hooks", func() {
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

	Describe("AddCommitCheck", func() {
		It("runs pre-commit check that passes", func() {
			checkRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				rtx.AddCommitCheck(func() error {
					checkRan = true
					return nil
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(checkRan).To(BeTrue())
		})

		It("aborts on pre-commit check failure", func() {
			errCheck := errors.New("consistency violation")
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				store, err := NewStoreBuilder().
					SetContext(rtx).SetMetaDataProvider(md).SetSubspace(specSubspace()).CreateOrOpen()
				if err != nil {
					return nil, err
				}
				id := int64(1)
				price := int32(100)
				_, err = store.SaveRecord(&gen.Order{OrderId: &id, Price: &price})
				if err != nil {
					return nil, err
				}
				rtx.AddCommitCheck(func() error {
					return errCheck
				})
				return nil, nil
			})
			Expect(err).To(MatchError(errCheck))
		})

		It("runs multiple checks in order", func() {
			var order []int
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					order = append(order, 1)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 2)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 3)
					return nil
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(order).To(Equal([]int{1, 2, 3}))
		})

		It("stops at first failing check", func() {
			var order []int
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					order = append(order, 1)
					return nil
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 2)
					return errors.New("fail")
				})
				rtx.AddCommitCheck(func() error {
					order = append(order, 3)
					return nil
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(order).To(Equal([]int{1, 2}))
		})
	})

	Describe("AddPostCommit", func() {
		It("runs post-commit callback after successful commit", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(postCommitRan).To(BeTrue())
		})

		It("does not run post-commit on error", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, errors.New("user error")
			})
			Expect(err).To(HaveOccurred())
			Expect(postCommitRan).To(BeFalse())
		})

		It("does not run post-commit when pre-commit check fails", func() {
			postCommitRan := false
			_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
				rtx.AddCommitCheck(func() error {
					return errors.New("check failed")
				})
				rtx.AddPostCommit(func() {
					postCommitRan = true
				})
				return nil, nil
			})
			Expect(err).To(HaveOccurred())
			Expect(postCommitRan).To(BeFalse())
		})
	})
})
