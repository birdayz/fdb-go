//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Scan Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("scan_%s", uuid.New().String())
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

	scanOrdersWithGo := func(limit int) []*gen.Order {
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

			scanProps := recordlayer.NewScanProperties(
				recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
			)

			cursor := store.ScanRecords(nil, scanProps)
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

	// scanOrderResult represents a scanned order from Java
	type scanOrderResult struct {
		OrderID int64 `json:"orderId"`
		Price   int32 `json:"price"`
		Flower  *struct {
			Type  string `json:"type"`
			Color string `json:"color"`
		} `json:"flower"`
	}

	scanOrdersWithJava := func(limit int) []scanOrderResult {
		params := buildJavaParams()
		params["limit"] = limit
		raw, err := java.Invoke(ctx, "scanOrders", params)
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

	Describe("Go writes, Java scans", func() {
		It("should scan all records in order", func() {
			orders := StandardOrders(1001, 5)
			saveOrdersWithGo(orders)

			javaResults := scanOrdersWithJava(0)
			Expect(javaResults).To(HaveLen(5))
			for i, result := range javaResults {
				Expect(result.OrderID).To(Equal(int64(1001 + i)))
				Expect(result.Price).To(Equal(int32((1001 + int64(i)) * 10)))
			}
		})
	})

	Describe("Java writes, Go scans", func() {
		It("should scan records written by Java", func() {
			for _, order := range StandardOrders(2001, 5) {
				saveOrderWithJava(order)
			}

			goResults := scanOrdersWithGo(0)
			Expect(goResults).To(HaveLen(5))
			for i, order := range goResults {
				Expect(*order.OrderId).To(Equal(int64(2001 + i)))
				Expect(*order.Price).To(Equal(int32((2001 + int64(i)) * 10)))
			}
		})
	})

	Describe("Scan with limit", func() {
		It("should respect row limit in both Go and Java", func() {
			orders := StandardOrders(3001, 10)
			saveOrdersWithGo(orders)

			// Java scan with limit=3
			javaResults := scanOrdersWithJava(3)
			Expect(javaResults).To(HaveLen(3))
			Expect(javaResults[0].OrderID).To(Equal(int64(3001)))
			Expect(javaResults[1].OrderID).To(Equal(int64(3002)))
			Expect(javaResults[2].OrderID).To(Equal(int64(3003)))

			// Go scan with limit=3
			goResults := scanOrdersWithGo(3)
			Expect(goResults).To(HaveLen(3))
			Expect(*goResults[0].OrderId).To(Equal(int64(3001)))
			Expect(*goResults[1].OrderId).To(Equal(int64(3002)))
			Expect(*goResults[2].OrderId).To(Equal(int64(3003)))
		})
	})

	Describe("Cross-scan ordering", func() {
		It("should return same order from Go and Java scans", func() {
			orders := []*gen.Order{
				NewOrder(5005).WithPrice(50).WithFlower("Rose", gen.Color_RED).Build(),
				NewOrder(5001).WithPrice(10).WithFlower("Tulip", gen.Color_BLUE).Build(),
				NewOrder(5003).WithPrice(30).WithFlower("Lily", gen.Color_YELLOW).Build(),
			}
			saveOrdersWithGo(orders)

			// Both should return in primary key order: 5001, 5003, 5005
			javaResults := scanOrdersWithJava(0)
			goResults := scanOrdersWithGo(0)

			Expect(javaResults).To(HaveLen(3))
			Expect(goResults).To(HaveLen(3))

			expectedOrder := []int64{5001, 5003, 5005}
			for i, expected := range expectedOrder {
				Expect(javaResults[i].OrderID).To(Equal(expected))
				Expect(*goResults[i].OrderId).To(Equal(expected))
			}
		})
	})

	Describe("Scan empty store", func() {
		It("should return empty results from both", func() {
			javaResults := scanOrdersWithJava(0)
			Expect(javaResults).To(HaveLen(0))

			goResults := scanOrdersWithGo(0)
			Expect(goResults).To(HaveLen(0))
		})
	})

	Describe("Scan with flower details cross-check", func() {
		It("should preserve flower data in both directions", func() {
			order := NewOrder(6001).
				WithPrice(42).
				WithFlower("Orchid", gen.Color_PINK).
				Build()
			saveOrdersWithGo([]*gen.Order{order})

			javaResults := scanOrdersWithJava(0)
			Expect(javaResults).To(HaveLen(1))
			Expect(javaResults[0].OrderID).To(Equal(int64(6001)))
			Expect(javaResults[0].Price).To(Equal(int32(42)))
			Expect(javaResults[0].Flower).NotTo(BeNil())
			Expect(javaResults[0].Flower.Type).To(Equal("Orchid"))
			Expect(javaResults[0].Flower.Color).To(Equal("PINK"))

			goResults := scanOrdersWithGo(0)
			Expect(goResults).To(HaveLen(1))
			Expect(*goResults[0].OrderId).To(Equal(int64(6001)))
			Expect(*goResults[0].Price).To(Equal(int32(42)))
			Expect(*goResults[0].Flower.Type).To(Equal("Orchid"))
			Expect(*goResults[0].Flower.Color).To(Equal(gen.Color_PINK))
		})
	})
})
