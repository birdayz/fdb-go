//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Record Count Conformance", func() {
	var (
		ctx       context.Context
		env       *TenantEnvironment
		java      *JavaInvoker
		countMeta *recordlayer.RecordMetaData
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("count_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()

		// Create counting-enabled metadata for Go side
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.SetRecordCountKey(recordlayer.EmptyKey())
		countMeta, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	buildJavaParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(env.Keyspace.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	saveOrderWithGoCounting := func(order *gen.Order) {
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(countMeta).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.SaveRecord(order)
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	deleteOrderWithGoCounting := func(orderID int64) {
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(countMeta).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			_, err = store.DeleteRecord(tuple.Tuple{orderID})
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
	}

	getGoRecordCount := func() int64 {
		var count int64
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(countMeta).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			count, err = store.GetRecordCount()
			return nil, err
		})
		Expect(err).NotTo(HaveOccurred())
		return count
	}

	getJavaRecordCount := func() int64 {
		params := buildJavaParams()
		var count int64
		err := java.InvokeAs(ctx, "getRecordCount", params, &count)
		Expect(err).NotTo(HaveOccurred())
		return count
	}

	saveOrderWithJavaCounting := func(order *gen.Order) {
		params := buildJavaParams()
		params["order"] = order
		err := java.InvokeAs(ctx, "saveOrderCounting", params, nil)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("Go saves, Java counts", func() {
		It("should agree on count after Go saves", func() {
			for i := int64(1); i <= 5; i++ {
				saveOrderWithGoCounting(StandardOrder(i))
			}

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(5)))
			Expect(javaCount).To(Equal(int64(5)))
		})
	})

	Describe("Java saves, Go counts", func() {
		It("should agree on count after Java saves", func() {
			for i := int64(1); i <= 3; i++ {
				saveOrderWithJavaCounting(StandardOrder(i))
			}

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(3)))
			Expect(javaCount).To(Equal(int64(3)))
		})
	})

	Describe("Count after delete", func() {
		It("should decrement count when Go deletes", func() {
			for i := int64(1); i <= 3; i++ {
				saveOrderWithGoCounting(StandardOrder(i))
			}
			Expect(getJavaRecordCount()).To(Equal(int64(3)))

			deleteOrderWithGoCounting(2)

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(2)))
			Expect(javaCount).To(Equal(int64(2)))
		})
	})

	Describe("Count after Java delete", func() {
		It("should decrement count when Java deletes", func() {
			for i := int64(1); i <= 3; i++ {
				saveOrderWithGoCounting(StandardOrder(i))
			}
			Expect(getGoRecordCount()).To(Equal(int64(3)))

			// Java deletes record 2.
			params := buildJavaParams()
			params["orderID"] = int64(2)
			var deleted bool
			err := java.InvokeAs(ctx, "deleteOrderCounting", params, &deleted)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(2)))
			Expect(javaCount).To(Equal(int64(2)))
		})
	})

	Describe("Count not incremented on update", func() {
		It("should not change count when saving existing record", func() {
			saveOrderWithGoCounting(StandardOrder(1))
			Expect(getGoRecordCount()).To(Equal(int64(1)))
			Expect(getJavaRecordCount()).To(Equal(int64(1)))

			// Update the same record (same primary key, different data)
			updated := NewOrder(1).WithPrice(999).WithFlower("UpdatedRose", gen.Color_BLUE).Build()
			saveOrderWithGoCounting(updated)

			// Count should still be 1
			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(1)))
			Expect(javaCount).To(Equal(int64(1)))
		})
	})

	Describe("Mixed Go/Java saves", func() {
		It("should maintain correct count with interleaved saves", func() {
			saveOrderWithGoCounting(StandardOrder(1))
			saveOrderWithJavaCounting(StandardOrder(2))
			saveOrderWithGoCounting(StandardOrder(3))

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(3)))
			Expect(javaCount).To(Equal(int64(3)))
		})
	})

	Describe("Count starts at zero", func() {
		It("should return 0 for empty store", func() {
			// Force store creation
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(countMeta).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			goCount := getGoRecordCount()
			javaCount := getJavaRecordCount()
			Expect(goCount).To(Equal(int64(0)))
			Expect(javaCount).To(Equal(int64(0)))
		})
	})
})
