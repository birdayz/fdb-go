package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Continuation Token Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	BeforeEach(func() {
		ctx = context.Background()
		var err error

		tenantName := fmt.Sprintf("cont_%s", uuid.New().String())
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()

		// Seed 10 orders with Go
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).
				SetMetaDataProvider(env.MetaData).
				SetSubspace(env.Keyspace).
				CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(StandardOrder(i))
				if err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
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

	// javaScanResult represents the response from scanOrdersWithContinuation
	type javaScanResult struct {
		Orders          []map[string]any `json:"orders"`
		Continuation    string           `json:"continuation"` // base64
		SourceExhausted bool             `json:"sourceExhausted"`
	}

	scanWithJava := func(limit int, continuation string) javaScanResult {
		params := buildJavaParams()
		params["limit"] = limit
		if continuation != "" {
			params["continuation"] = continuation
		}
		raw, err := java.Invoke(ctx, "scanOrdersWithContinuation", params)
		Expect(err).NotTo(HaveOccurred())

		var result javaScanResult
		err = json.Unmarshal(raw, &result)
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	scanWithGo := func(limit int, continuation []byte) ([]*gen.Order, []byte, bool) {
		var orders []*gen.Order
		var nextCont []byte
		var sourceExhausted bool

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

			cursor := store.ScanRecords(continuation, scanProps)
			for {
				result, err := cursor.OnNext(ctx)
				if err != nil {
					return nil, err
				}
				if !result.HasNext() {
					var contErr error
					nextCont, contErr = result.GetContinuation().ToBytes()
					if contErr != nil {
						return nil, contErr
					}
					sourceExhausted = result.GetNoNextReason().IsSourceExhausted()
					break
				}
				order := result.GetValue().Record.(*gen.Order)
				orders = append(orders, order)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return orders, nextCont, sourceExhausted
	}

	Describe("Go generates continuation, Java resumes", func() {
		It("should allow Java to resume from Go's continuation token", func() {
			// Go scans first 3 records
			goOrders, goCont, goExhausted := scanWithGo(3, nil)
			Expect(goOrders).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())
			Expect(goCont).NotTo(BeNil())
			Expect(*goOrders[0].OrderId).To(Equal(int64(1)))
			Expect(*goOrders[2].OrderId).To(Equal(int64(3)))

			// Java resumes from Go's continuation token
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanWithJava(3, goContB64)
			Expect(javaResult.Orders).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())

			// Java should get records 4, 5, 6
			for i, order := range javaResult.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(4 + i)))
			}

			// Java resumes again from its own continuation to get 7, 8, 9
			javaResult2 := scanWithJava(3, javaResult.Continuation)
			Expect(javaResult2.Orders).To(HaveLen(3))
			for i, order := range javaResult2.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(7 + i)))
			}

			// Final batch: record 10, then exhausted
			javaResult3 := scanWithJava(3, javaResult2.Continuation)
			Expect(javaResult3.Orders).To(HaveLen(1))
			Expect(javaResult3.SourceExhausted).To(BeTrue())
			orderID, ok := javaResult3.Orders[0]["orderId"].(float64)
			Expect(ok).To(BeTrue())
			Expect(int64(orderID)).To(Equal(int64(10)))
		})
	})

	Describe("Java generates continuation, Go resumes", func() {
		It("should allow Go to resume from Java's continuation token", func() {
			// Java scans first 3 records
			javaResult := scanWithJava(3, "")
			Expect(javaResult.Orders).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())
			Expect(javaResult.Continuation).NotTo(BeEmpty())

			// Verify Java got records 1, 2, 3
			for i, order := range javaResult.Orders {
				orderID, ok := order["orderId"].(float64)
				Expect(ok).To(BeTrue())
				Expect(int64(orderID)).To(Equal(int64(1 + i)))
			}

			// Go resumes from Java's continuation token
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())

			goOrders, goCont, goExhausted := scanWithGo(3, javaCont)
			Expect(goOrders).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())

			// Go should get records 4, 5, 6
			for i, order := range goOrders {
				Expect(*order.OrderId).To(Equal(int64(4 + i)))
			}

			// Go resumes again to get 7, 8, 9
			goOrders2, goCont2, goExhausted2 := scanWithGo(3, goCont)
			Expect(goOrders2).To(HaveLen(3))
			Expect(goExhausted2).To(BeFalse())
			for i, order := range goOrders2 {
				Expect(*order.OrderId).To(Equal(int64(7 + i)))
			}

			// Final batch: record 10, then exhausted
			goOrders3, _, goExhausted3 := scanWithGo(3, goCont2)
			Expect(goOrders3).To(HaveLen(1))
			Expect(goExhausted3).To(BeTrue())
			Expect(*goOrders3[0].OrderId).To(Equal(int64(10)))
		})
	})

	Describe("Go and Java emit byte-identical continuation tokens", func() {
		It("Go's continuation matches Java's for the same scan position (TO_NEW wire parity)", func() {
			// Both engines scan the first 3 records from the start with the same limit.
			_, goCont, goExhausted := scanWithGo(3, nil)
			Expect(goExhausted).To(BeFalse())
			Expect(goCont).NotTo(BeNil())

			javaResult := scanWithJava(3, "")
			Expect(javaResult.Continuation).NotTo(BeEmpty())
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())

			// Pre-fix Go emitted a raw key suffix while Java emits a proto-wrapped
			// KeyValueCursorContinuation{inner_continuation, magic_number}
			// (KeyValueCursorBase defaults SerializationMode to TO_NEW). The engines
			// were merely read-tolerant of each other; the EMITTED bytes diverged.
			// They must now be byte-identical — a token written by one engine must be
			// indistinguishable from the other's, the actual wire-compat contract.
			Expect(goCont).To(Equal(javaCont))

			// And the bytes are the TO_NEW proto form (magic present), not raw.
			msg := &gen.KeyValueCursorContinuation{}
			Expect(msg.UnmarshalVT(goCont)).To(Succeed())
			Expect(msg.GetMagicNumber()).To(Equal(int64(6_773_487_359_078_157_740)))
		})
	})

	Describe("Alternating Go and Java with continuations", func() {
		It("should maintain correct position when alternating implementations", func() {
			// Go scans 2
			goOrders, goCont, _ := scanWithGo(2, nil)
			Expect(goOrders).To(HaveLen(2))
			Expect(*goOrders[0].OrderId).To(Equal(int64(1)))
			Expect(*goOrders[1].OrderId).To(Equal(int64(2)))

			// Java resumes with Go's continuation, scans 2
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanWithJava(2, goContB64)
			Expect(javaResult.Orders).To(HaveLen(2))
			Expect(int64(javaResult.Orders[0]["orderId"].(float64))).To(Equal(int64(3)))
			Expect(int64(javaResult.Orders[1]["orderId"].(float64))).To(Equal(int64(4)))

			// Go resumes with Java's continuation, scans 2
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())
			goOrders2, goCont2, _ := scanWithGo(2, javaCont)
			Expect(goOrders2).To(HaveLen(2))
			Expect(*goOrders2[0].OrderId).To(Equal(int64(5)))
			Expect(*goOrders2[1].OrderId).To(Equal(int64(6)))

			// Java resumes with Go's continuation, scans remaining
			goContB64_2 := base64.StdEncoding.EncodeToString(goCont2)
			javaResult2 := scanWithJava(10, goContB64_2)
			Expect(javaResult2.Orders).To(HaveLen(4))
			Expect(javaResult2.SourceExhausted).To(BeTrue())
			Expect(int64(javaResult2.Orders[0]["orderId"].(float64))).To(Equal(int64(7)))
			Expect(int64(javaResult2.Orders[3]["orderId"].(float64))).To(Equal(int64(10)))
		})
	})
})
