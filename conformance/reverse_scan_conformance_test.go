//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Reverse Scan Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("rscan_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()
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

	saveOrdersWithGo := func(orders []*gen.Order) {
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for _, order := range orders {
				_, err := store.SaveRecord(order)
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	}

	reverseScanWithGo := func(limit int) []*gen.Order {
		var result []*gen.Order
		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}

			scan := recordlayer.ReverseScan()
			if limit > 0 {
				scan = recordlayer.NewScanProperties(
					recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
				).WithReverse(true)
			}

			cursor := store.ScanRecords(nil, scan)
			records, err := recordlayer.AsList(ctx, cursor)
			if err != nil {
				return nil, err
			}

			for _, rec := range records {
				order, ok := rec.Record.(*gen.Order)
				if ok {
					result = append(result, order)
				}
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	type scanOrderResult struct {
		OrderID int64 `json:"orderId"`
		Price   int32 `json:"price"`
	}

	reverseScanWithJava := func(limit int) []scanOrderResult {
		params := buildJavaParams()
		params["limit"] = limit
		raw, err := java.Invoke(ctx, "scanOrdersReverse", params)
		Expect(err).NotTo(HaveOccurred())

		var results []scanOrderResult
		err = json.Unmarshal(raw, &results)
		Expect(err).NotTo(HaveOccurred())
		return results
	}

	saveOrderWithJava := func(order *gen.Order) {
		params := buildJavaParams()
		params["order"] = order
		err := java.InvokeAs(ctx, "saveOrder", params, nil)
		Expect(err).NotTo(HaveOccurred())
	}

	Describe("Go writes, both reverse scan", func() {
		It("should return records in reverse primary key order from both Go and Java", func() {
			orders := StandardOrders(1001, 5)
			saveOrdersWithGo(orders)

			// Java reverse scan
			javaResults := reverseScanWithJava(0)
			Expect(javaResults).To(HaveLen(5))
			for i, result := range javaResults {
				Expect(result.OrderID).To(Equal(int64(1005 - i)))
			}

			// Go reverse scan
			goResults := reverseScanWithGo(0)
			Expect(goResults).To(HaveLen(5))
			for i, order := range goResults {
				Expect(*order.OrderId).To(Equal(int64(1005 - i)))
			}
		})
	})

	Describe("Java writes, Go reverse scans", func() {
		It("should return Java-written records in reverse order from Go", func() {
			for _, order := range StandardOrders(2001, 5) {
				saveOrderWithJava(order)
			}

			goResults := reverseScanWithGo(0)
			Expect(goResults).To(HaveLen(5))
			for i, order := range goResults {
				Expect(*order.OrderId).To(Equal(int64(2005 - i)))
			}
		})
	})

	Describe("Reverse scan with limit", func() {
		It("should respect row limit in both Go and Java reverse scans", func() {
			orders := StandardOrders(3001, 10)
			saveOrdersWithGo(orders)

			// Java reverse with limit=3 → highest 3 PKs
			javaResults := reverseScanWithJava(3)
			Expect(javaResults).To(HaveLen(3))
			Expect(javaResults[0].OrderID).To(Equal(int64(3010)))
			Expect(javaResults[1].OrderID).To(Equal(int64(3009)))
			Expect(javaResults[2].OrderID).To(Equal(int64(3008)))

			// Go reverse with limit=3
			goResults := reverseScanWithGo(3)
			Expect(goResults).To(HaveLen(3))
			Expect(*goResults[0].OrderId).To(Equal(int64(3010)))
			Expect(*goResults[1].OrderId).To(Equal(int64(3009)))
			Expect(*goResults[2].OrderId).To(Equal(int64(3008)))
		})
	})

	Describe("Reverse scan mirrors forward scan", func() {
		It("should return the exact reverse of forward scan", func() {
			orders := []*gen.Order{
				NewOrder(5005).WithPrice(50).Build(),
				NewOrder(5001).WithPrice(10).Build(),
				NewOrder(5003).WithPrice(30).Build(),
			}
			saveOrdersWithGo(orders)

			// Forward scan (Java)
			forwardParams := buildJavaParams()
			forwardParams["limit"] = 0
			forwardRaw, err := java.Invoke(ctx, "scanOrders", forwardParams)
			Expect(err).NotTo(HaveOccurred())
			var forwardResults []scanOrderResult
			err = json.Unmarshal(forwardRaw, &forwardResults)
			Expect(err).NotTo(HaveOccurred())

			// Reverse scan (Java)
			reverseResults := reverseScanWithJava(0)

			// Should be mirror images
			Expect(reverseResults).To(HaveLen(len(forwardResults)))
			for i := range forwardResults {
				Expect(reverseResults[i].OrderID).To(Equal(forwardResults[len(forwardResults)-1-i].OrderID))
			}

			// Same check with Go
			goReverse := reverseScanWithGo(0)
			Expect(goReverse).To(HaveLen(len(forwardResults)))
			for i := range forwardResults {
				Expect(*goReverse[i].OrderId).To(Equal(forwardResults[len(forwardResults)-1-i].OrderID))
			}
		})
	})

	Describe("Reverse scan with continuation cross-platform", func() {
		It("should resume Go reverse scan with Java continuation", func() {
			// Write 10 orders
			orders := StandardOrders(4001, 10)
			saveOrdersWithGo(orders)

			// Java reverse scan page 1 (limit=3)
			params := buildJavaParams()
			params["limit"] = 3
			params["continuation"] = ""
			raw, err := java.Invoke(ctx, "scanOrdersReverseWithContinuation", params)
			Expect(err).NotTo(HaveOccurred())

			var page1 struct {
				Orders          []scanOrderResult `json:"orders"`
				Continuation    string            `json:"continuation"`
				SourceExhausted bool              `json:"sourceExhausted"`
			}
			err = json.Unmarshal(raw, &page1)
			Expect(err).NotTo(HaveOccurred())
			Expect(page1.Orders).To(HaveLen(3))
			Expect(page1.Orders[0].OrderID).To(Equal(int64(4010)))
			Expect(page1.Orders[1].OrderID).To(Equal(int64(4009)))
			Expect(page1.Orders[2].OrderID).To(Equal(int64(4008)))
			Expect(page1.SourceExhausted).To(BeFalse())

			// Go resumes from Java's continuation (page 2)
			contBytes, err := base64.StdEncoding.DecodeString(page1.Continuation)
			Expect(err).NotTo(HaveOccurred())

			var goPage2 []*gen.Order
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetContext(rtx).
					SetMetaDataProvider(env.MetaData).
					SetSubspace(env.Keyspace).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}

				scan := recordlayer.NewScanProperties(
					recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(3),
				).WithReverse(true)

				cursor := store.ScanRecords(contBytes, scan)
				records, err := recordlayer.AsList(ctx, cursor)
				if err != nil {
					return nil, err
				}

				for _, rec := range records {
					order, ok := rec.Record.(*gen.Order)
					if ok {
						goPage2 = append(goPage2, order)
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(goPage2).To(HaveLen(3))
			Expect(*goPage2[0].OrderId).To(Equal(int64(4007)))
			Expect(*goPage2[1].OrderId).To(Equal(int64(4006)))
			Expect(*goPage2[2].OrderId).To(Equal(int64(4005)))
		})
	})

	Describe("Reverse scan empty store", func() {
		It("should return empty results from both", func() {
			javaResults := reverseScanWithJava(0)
			Expect(javaResults).To(HaveLen(0))

			goResults := reverseScanWithGo(0)
			Expect(goResults).To(HaveLen(0))
		})
	})
})
