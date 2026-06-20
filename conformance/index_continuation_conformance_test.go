//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Index Scan Continuation Conformance", func() {
	var (
		ctx      context.Context
		env      *TenantEnvironment
		java     *JavaInvoker
		idx      *recordlayer.Index
		metaData *recordlayer.RecordMetaData
		ks       subspace.Subspace
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("idxcont_%s", uuid.New().String())
		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		java = NewJavaInvoker()

		// Create metadata with price VALUE index
		idx = recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", idx)
		metaData, err = builder.Build()
		Expect(err).NotTo(HaveOccurred())

		ks = env.Keyspace
		if env.TenantName != "" {
			ks = subspace.Sub(tuple.Tuple{})
		}

		// Seed 10 orders with prices 100..1000
		_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).CreateOrOpen()
			if err != nil {
				return nil, err
			}
			for i := int64(1); i <= 10; i++ {
				_, err = store.SaveRecord(&gen.Order{
					OrderId: proto.Int64(i),
					Price:   proto.Int32(int32(i * 100)),
				})
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
			"subspace":    BytesToIntArray(ks.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	type indexScanResult struct {
		Entries         []map[string]any `json:"entries"`
		Continuation    string           `json:"continuation"`
		SourceExhausted bool             `json:"sourceExhausted"`
	}

	scanIndexWithJava := func(limit int, continuation string) indexScanResult {
		params := buildJavaParams()
		params["limit"] = limit
		if continuation != "" {
			params["continuation"] = continuation
		}
		raw, err := java.Invoke(ctx, "scanIndexWithContinuation", params)
		Expect(err).NotTo(HaveOccurred())

		var result indexScanResult
		err = json.Unmarshal(raw, &result)
		Expect(err).NotTo(HaveOccurred())
		return result
	}

	// scanIndexWithGo returns index entry keys, continuation bytes, sourceExhausted
	scanIndexWithGo := func(limit int, continuation []byte) ([]tuple.Tuple, []byte, bool) {
		var keys []tuple.Tuple
		var nextCont []byte
		var sourceExhausted bool

		_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(metaData).SetSubspace(ks).Open()
			if err != nil {
				return nil, err
			}

			scan := recordlayer.NewScanProperties(
				recordlayer.DefaultExecuteProperties().WithReturnedRowLimit(limit),
			)
			cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, continuation, scan)

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
				keys = append(keys, result.GetValue().Key)
			}
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		return keys, nextCont, sourceExhausted
	}

	Describe("Go generates index continuation, Java resumes", func() {
		It("should allow Java to resume from Go's index continuation token", func() {
			// Go scans first 3 index entries (prices 100, 200, 300)
			goKeys, goCont, goExhausted := scanIndexWithGo(3, nil)
			Expect(goKeys).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())
			Expect(goCont).NotTo(BeNil())
			// Keys should be [price, order_id]: (100,1), (200,2), (300,3)
			Expect(goKeys[0][0]).To(Equal(int64(100)))
			Expect(goKeys[2][0]).To(Equal(int64(300)))

			// Java resumes from Go's continuation
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanIndexWithJava(3, goContB64)
			Expect(javaResult.Entries).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())
			// Java should get entries for prices 400, 500, 600
			for i, entry := range javaResult.Entries {
				keyRaw := toInterfaceSlice(entry["key"])
				price := int64(keyRaw[0].(float64))
				Expect(price).To(Equal(int64((4 + i) * 100)))
			}

			// Java resumes again: prices 700, 800, 900
			javaResult2 := scanIndexWithJava(3, javaResult.Continuation)
			Expect(javaResult2.Entries).To(HaveLen(3))

			// Final: price 1000, then exhausted
			javaResult3 := scanIndexWithJava(3, javaResult2.Continuation)
			Expect(javaResult3.Entries).To(HaveLen(1))
			Expect(javaResult3.SourceExhausted).To(BeTrue())
			keyRaw := toInterfaceSlice(javaResult3.Entries[0]["key"])
			price := int64(keyRaw[0].(float64))
			Expect(price).To(Equal(int64(1000)))
		})
	})

	Describe("Java generates index continuation, Go resumes", func() {
		It("should allow Go to resume from Java's index continuation token", func() {
			// Java scans first 3 entries
			javaResult := scanIndexWithJava(3, "")
			Expect(javaResult.Entries).To(HaveLen(3))
			Expect(javaResult.SourceExhausted).To(BeFalse())
			Expect(javaResult.Continuation).NotTo(BeEmpty())
			// Verify Java got prices 100, 200, 300
			for i, entry := range javaResult.Entries {
				keyRaw := toInterfaceSlice(entry["key"])
				price := int64(keyRaw[0].(float64))
				Expect(price).To(Equal(int64((1 + i) * 100)))
			}

			// Go resumes from Java's continuation
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())

			goKeys, goCont, goExhausted := scanIndexWithGo(3, javaCont)
			Expect(goKeys).To(HaveLen(3))
			Expect(goExhausted).To(BeFalse())
			// Go should get prices 400, 500, 600
			for i, key := range goKeys {
				Expect(key[0]).To(Equal(int64((4 + i) * 100)))
			}

			// Go resumes: prices 700, 800, 900
			goKeys2, goCont2, goExhausted2 := scanIndexWithGo(3, goCont)
			Expect(goKeys2).To(HaveLen(3))
			Expect(goExhausted2).To(BeFalse())

			// Final: price 1000, exhausted
			goKeys3, _, goExhausted3 := scanIndexWithGo(3, goCont2)
			Expect(goKeys3).To(HaveLen(1))
			Expect(goExhausted3).To(BeTrue())
			Expect(goKeys3[0][0]).To(Equal(int64(1000)))
		})
	})

	Describe("Alternating Go and Java with index continuations", func() {
		It("should maintain correct position when alternating implementations", func() {
			// Go scans 2 (prices 100, 200)
			goKeys, goCont, _ := scanIndexWithGo(2, nil)
			Expect(goKeys).To(HaveLen(2))
			Expect(goKeys[0][0]).To(Equal(int64(100)))
			Expect(goKeys[1][0]).To(Equal(int64(200)))

			// Java resumes with Go's continuation, scans 2 (prices 300, 400)
			goContB64 := base64.StdEncoding.EncodeToString(goCont)
			javaResult := scanIndexWithJava(2, goContB64)
			Expect(javaResult.Entries).To(HaveLen(2))
			keyRaw0 := toInterfaceSlice(javaResult.Entries[0]["key"])
			keyRaw1 := toInterfaceSlice(javaResult.Entries[1]["key"])
			Expect(int64(keyRaw0[0].(float64))).To(Equal(int64(300)))
			Expect(int64(keyRaw1[0].(float64))).To(Equal(int64(400)))

			// Go resumes with Java's continuation, scans 2 (prices 500, 600)
			javaCont, err := base64.StdEncoding.DecodeString(javaResult.Continuation)
			Expect(err).NotTo(HaveOccurred())
			goKeys2, goCont2, _ := scanIndexWithGo(2, javaCont)
			Expect(goKeys2).To(HaveLen(2))
			Expect(goKeys2[0][0]).To(Equal(int64(500)))
			Expect(goKeys2[1][0]).To(Equal(int64(600)))

			// Java resumes with Go's continuation, scans remaining (prices 700, 800, 900, 1000)
			goContB64_2 := base64.StdEncoding.EncodeToString(goCont2)
			javaResult2 := scanIndexWithJava(10, goContB64_2)
			Expect(javaResult2.Entries).To(HaveLen(4))
			Expect(javaResult2.SourceExhausted).To(BeTrue())
			firstKey := toInterfaceSlice(javaResult2.Entries[0]["key"])
			lastKey := toInterfaceSlice(javaResult2.Entries[3]["key"])
			Expect(int64(firstKey[0].(float64))).To(Equal(int64(700)))
			Expect(int64(lastKey[0].(float64))).To(Equal(int64(1000)))
		})
	})
})
