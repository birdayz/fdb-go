//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"time"

	"github.com/bazelbuild/rules_go/go/runfiles"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/rabitq"
	"fdb.dev/pkg/recordlayer"
)

var _ = Describe("RaBitQ encoder byte conformance", func() {
	for _, tc := range []struct {
		name string
		vec  []float64
	}{
		{"asymmetric_four", []float64{-3, 8, 2, 6}},
		{"quantization_seven", []float64{1, -2, 3, -4, 5, -6, 7}},
		{"fma_rounding_boundary", []float64{math.Ldexp(9, -29), 1 + math.Ldexp(1, -27)}},
		{"fractional_five", []float64{0.1, -1.7, 0.003, 11.5, -0.25}},
		{"equal_three", []float64{1, 1, 1}},
		{"axis_four", []float64{0, -7, 0, 0}},
		{"scalar", []float64{3}},
		{"zero_four", []float64{0, 0, 0, 0}},
		{"signed_zero", []float64{math.Copysign(0, -1), 0, math.Copysign(0, -1)}},
	} {
		for _, bits := range []int{1, 4, 8} {
			It(fmt.Sprintf("matches Java persisted bytes for %s at %d extra bits", tc.name, bits), func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
				defer cancel()
				vectorJSON, err := json.Marshal(tc.vec)
				Expect(err).NotTo(HaveOccurred())
				var javaHex string
				err = NewJavaInvoker().InvokeAs(ctx, "encodeRaBitQVector", map[string]any{
					"vectorJson": string(vectorJSON), "numExBits": bits,
				}, &javaHex)
				Expect(err).NotTo(HaveOccurred())
				javaBytes, err := hex.DecodeString(javaHex)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaBytes).To(HaveLen(25 + (len(tc.vec)*(bits+1)+7)/8))
				goBytes := rabitq.NewRaBitQuantizer(rabitq.MetricEuclidean, bits).Encode(tc.vec).ToBytes()
				fmt.Fprintf(GinkgoWriter, "RABITQ-ENCODER name=%s bits=%d vector=%s java=%s go=%x\n", tc.name, bits, vectorJSON, javaHex, goBytes)
				Expect(goBytes).To(Equal(javaBytes))
				var javaDecodedHex string
				err = NewJavaInvoker().InvokeAs(ctx, "decodeRaBitQVector", map[string]any{
					"encodedHex": javaHex, "numDimensions": len(tc.vec), "numExBits": bits,
				}, &javaDecodedHex)
				Expect(err).NotTo(HaveOccurred())
				javaDecoded, err := hex.DecodeString(javaDecodedHex)
				Expect(err).NotTo(HaveOccurred())
				goDecoded, err := rabitq.NewQuantizer(rabitq.MetricEuclidean, bits).Decode(javaBytes, len(tc.vec))
				Expect(err).NotTo(HaveOccurred())
				fmt.Fprintf(GinkgoWriter, "RABITQ-DECODE name=%s bits=%d java=%s go=%x\n", tc.name, bits, javaDecodedHex, conformanceSerializeVector(goDecoded))
				Expect(conformanceSerializeVector(goDecoded)).To(Equal(javaDecoded))
			})
		}
	}
})

var _ = Describe("RaBitQ degenerate norm reconstruction", func() {
	for _, tc := range []struct {
		name string
		norm float64
	}{
		{"negative", -1},
		{"nan", math.NaN()},
	} {
		It("returns zero components for a "+tc.name+" stored norm", func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			encoded := &rabitq.EncodedVector{Encoded: []int{1, 8, 15, 20}, NumExBits: 4, FAddEx: tc.norm}
			data := encoded.ToBytes()
			var javaHex string
			err := NewJavaInvoker().InvokeAs(ctx, "decodeRaBitQVector", map[string]any{
				"encodedHex": hex.EncodeToString(data), "numDimensions": 4, "numExBits": 4,
			}, &javaHex)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaHex).To(Equal(hex.EncodeToString(conformanceSerializeVector([]float64{0, 0, 0, 0}))))
			got, err := rabitq.NewQuantizer(rabitq.MetricEuclidean, 4).Decode(data, 4)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal([]float64{0, 0, 0, 0}))
		})
	}
})

var _ = Describe("Legacy Go RaBitQ migration conformance", func() {
	It("rebuilds the retained legacy graph and permits cold reads and writes from both engines", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "legacy_vec_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(env.Cleanup(ctx)).To(Succeed()) }()
		r, err := runfiles.New()
		Expect(err).NotTo(HaveOccurred())
		path, err := r.Rlocation("_main/pkg/recordlayer/testdata/hnsw_legacy_go_entry.json")
		Expect(err).NotTo(HaveOccurred())
		data, err := os.ReadFile(path)
		Expect(err).NotTo(HaveOccurred())
		var fixture struct {
			Writer string `json:"writer"`
			KVs    []struct {
				Key   string `json:"key_hex"`
				Value string `json:"value_hex"`
			} `json:"kvs_relative_to_graph_prefix"`
		}
		Expect(json.Unmarshal(data, &fixture)).To(Succeed())
		Expect(fixture.Writer).To(Equal("e48f5b4965543cd4d99b5578356059e12d969c7c"))
		Expect(fixture.KVs).To(HaveLen(2))
		index := recordlayer.NewVectorIndex("legacy_rabitq", recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 4)
		index.Options = map[string]string{
			"hnswNumDimensions": "4", "hnswMetric": "EUCLIDEAN_METRIC",
			"hnswM": "4", "hnswMMax": "4", "hnswMMax0": "8",
			"hnswUseRaBitQ": "true", "hnswRaBitQNumExBits": "4",
			"hnswSampleVectorStatsProbability": "1.0", "hnswMaintainStatsProbability": "1.0", "hnswStatsThreshold": "11",
		}
		builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
		builder.AddIndex("Order", index)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())
		metadata, err := md.ToProto()
		Expect(err).NotTo(HaveOccurred())
		metadataBytes, err := proto.Marshal(metadata)
		Expect(err).NotTo(HaveOccurred())
		ks := subspace.Sub(tuple.Tuple{})
		runGo := func(action func(*recordlayer.FDBRecordStore, fdb.WritableTransaction)) {
			_, err := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				action(store, rtx.Transaction())
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
		}
		withBootstrap := func(ids ...int64) []int64 {
			for id := int64(100); id < 111; id++ {
				ids = append(ids, id)
			}
			return ids
		}
		java := func(action string, id int64, wantIDs []int64, disabled bool) {
			var result struct {
				State   string  `json:"state"`
				Refused bool    `json:"refused"`
				IDs     []int64 `json:"ids"`
			}
			err := NewJavaInvoker().InvokeAs(ctx, "exerciseRebuiltRaBitQIndex", map[string]any{
				"clusterFile": env.ClusterFile, "subspace": BytesToIntArray(ks.Bytes()), "tenantName": env.TenantName,
				"metadataBytes": BytesToIntArray(metadataBytes), "indexName": index.Name,
				"action": action, "orderId": id, "vectorJson": "[4,-1,7,9]",
			}, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Refused).To(Equal(disabled))
			if disabled {
				Expect(result.State).To(Equal("DISABLED"))
			} else {
				Expect(result.State).To(Equal("READABLE"))
				Expect(result.IDs).To(ConsistOf(wantIDs))
			}
		}
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), VectorData: conformanceSerializeVector([]float64{-3, 8, 2, 6})})
			Expect(err).NotTo(HaveOccurred())
			ss := store.IndexSubspace(index)
			tx.ClearRange(ss)
			for _, kv := range fixture.KVs {
				key, err := hex.DecodeString(kv.Key)
				Expect(err).NotTo(HaveOccurred())
				value, err := hex.DecodeString(kv.Value)
				Expect(err).NotTo(HaveOccurred())
				tx.Set(fdb.Key(append(ss.Bytes(), key...)), value)
			}
		})
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			changed, err := store.MarkIndexDisabled(index.Name)
			Expect(err).NotTo(HaveOccurred())
			Expect(changed).To(BeTrue())
		})
		java("search", 0, nil, true)
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			// The historical producer used a threshold forbidden by Java.
			// Rebuild its unchanged bytes under valid settings from records.
			for id := int64(100); id < 111; id++ {
				_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(id), VectorData: conformanceSerializeVector([]float64{float64(id), 2, 3, 4})})
				Expect(err).NotTo(HaveOccurred())
			}
			Expect(store.RebuildIndex(index)).To(Succeed())
		})
		java("search", 0, withBootstrap(2), false)
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			_, err := store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), VectorData: conformanceSerializeVector([]float64{1, 2, 3, 4})})
			Expect(err).NotTo(HaveOccurred())
		})
		java("save", 4, withBootstrap(2, 3, 4), false)
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			results, err := store.SearchVectorIndex(index, []float64{4, -1, 7, 9}, 100, 100)
			Expect(err).NotTo(HaveOccurred())
			var ids []int64
			for _, result := range results {
				ids = append(ids, result.PrimaryKey[0].(int64))
			}
			Expect(ids).To(ConsistOf(withBootstrap(2, 3, 4)))
			deleted, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())
		})
		java("search", 0, withBootstrap(3, 4), false)
		java("delete", 3, withBootstrap(4), false)
		runGo(func(store *recordlayer.FDBRecordStore, tx fdb.WritableTransaction) {
			results, err := store.SearchVectorIndex(index, []float64{4, -1, 7, 9}, 100, 100)
			Expect(err).NotTo(HaveOccurred())
			var ids []int64
			for _, result := range results {
				ids = append(ids, result.PrimaryKey[0].(int64))
			}
			Expect(ids).To(ConsistOf(withBootstrap(4)))
		})
	})
})

var _ = Describe("VECTOR Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *VectorIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("vec_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewVectorIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java reads", func() {
		It("should allow Java to open a store with Go-written HNSW graph and load records", func() {
			vectors := []struct {
				id  int64
				vec []float64
			}{
				{1, []float64{1.0, 2.0, 3.0}},
				{2, []float64{4.0, 5.0, 6.0}},
				{3, []float64{7.0, 8.0, 9.0}},
			}
			for _, v := range vectors {
				err := store.SaveOrderGo(ctx, v.id, v.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java loads each record — proves Java can open store with Go-written HNSW graph.
			for _, v := range vectors {
				result, err := store.LoadOrderJava(ctx, v.id)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.OrderID).To(Equal(v.id))
				Expect(result.Vector).To(HaveLen(3))
				for i, val := range v.vec {
					Expect(result.Vector[i]).To(BeNumerically("~", val, 1e-9))
				}
			}
		})
	})

	Describe("Java writes, Go reads", func() {
		It("should allow Go to open a store with Java-written HNSW graph and load records", func() {
			vectors := []struct {
				id  int64
				vec []float64
			}{
				{10, []float64{0.1, 0.2, 0.3}},
				{20, []float64{0.4, 0.5, 0.6}},
				{30, []float64{0.7, 0.8, 0.9}},
			}
			for _, v := range vectors {
				err := store.SaveOrderJava(ctx, v.id, v.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go loads each record — proves Go can open store with Java-written HNSW graph.
			for _, v := range vectors {
				result, err := store.LoadOrderGo(ctx, v.id)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.OrderID).To(Equal(v.id))
				Expect(result.Vector).To(HaveLen(3))
				for i, val := range v.vec {
					Expect(result.Vector[i]).To(BeNumerically("~", val, 1e-9))
				}
			}
		})
	})

	Describe("Mixed writes: Go then Java", func() {
		It("should allow Java to insert into a Go-created HNSW graph without errors", func() {
			// Go inserts 3 records.
			err := store.SaveOrderGo(ctx, 1, []float64{1.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, []float64{0.0, 1.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 3, []float64{0.0, 0.0, 1.0})
			Expect(err).NotTo(HaveOccurred())

			// Java inserts 2 more records into the same HNSW graph.
			// This is the critical test: Java's HNSW maintainer must traverse and modify
			// Go-written graph nodes without errors.
			err = store.SaveOrderJava(ctx, 4, []float64{1.0, 1.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 5, []float64{0.0, 1.0, 1.0})
			Expect(err).NotTo(HaveOccurred())

			// Both sides should see all 5 records.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(5))

			javaCount, err := store.CountRecordsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaCount).To(Equal(int64(5)))
		})
	})

	Describe("Mixed writes: Java then Go", func() {
		It("should allow Go to insert into a Java-created HNSW graph without errors", func() {
			// Java inserts 3 records.
			err := store.SaveOrderJava(ctx, 10, []float64{1.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 20, []float64{0.0, 1.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 30, []float64{0.0, 0.0, 1.0})
			Expect(err).NotTo(HaveOccurred())

			// Go inserts 2 more records into Java's HNSW graph.
			err = store.SaveOrderGo(ctx, 40, []float64{1.0, 1.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 50, []float64{0.0, 1.0, 1.0})
			Expect(err).NotTo(HaveOccurred())

			// Both sides should see all 5 records.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(5))

			javaCount, err := store.CountRecordsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaCount).To(Equal(int64(5)))
		})
	})

	Describe("Go delete of Java-written record", func() {
		It("should remove the record and clean the HNSW graph entry", func() {
			// Java writes 2 records.
			err := store.SaveOrderJava(ctx, 1, []float64{1.0, 2.0, 3.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 2, []float64{4.0, 5.0, 6.0})
			Expect(err).NotTo(HaveOccurred())

			// Go deletes order 1.
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Only 1 record remains.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(1))

			// Go can still load the surviving record.
			result, err := store.LoadOrderGo(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.OrderID).To(Equal(int64(2)))
		})
	})

	Describe("Java delete of Go-written record", func() {
		It("should remove the record and clean the HNSW graph entry", func() {
			// Go writes 2 records.
			err := store.SaveOrderGo(ctx, 1, []float64{1.0, 2.0, 3.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, []float64{4.0, 5.0, 6.0})
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2.
			deleted, err := store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			// Only 1 record remains.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(1))

			// Java can load the surviving record.
			result, err := store.LoadOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())
			Expect(result.OrderID).To(Equal(int64(1)))
		})
	})

	Describe("Go search after Java writes", func() {
		It("should find nearest neighbors in a Java-written HNSW graph", func() {
			// Java writes 5 well-separated points in 3D.
			points := []struct {
				id  int64
				vec []float64
			}{
				{1, []float64{0.0, 0.0, 0.0}},
				{2, []float64{1.0, 0.0, 0.0}},
				{3, []float64{0.0, 1.0, 0.0}},
				{4, []float64{10.0, 10.0, 10.0}},
				{5, []float64{100.0, 100.0, 100.0}},
			}
			for _, p := range points {
				err := store.SaveOrderJava(ctx, p.id, p.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go searches for 3 nearest neighbors to origin.
			results, err := store.SearchGo(ctx, []float64{0.0, 0.0, 0.0}, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			// The 3 closest should be ids 1 (dist=0), 2 (dist=1), 3 (dist=1).
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				gotIDs[r.PrimaryKey[0].(int64)] = true
			}
			Expect(gotIDs).To(HaveKey(int64(1)))
			Expect(gotIDs).To(HaveKey(int64(2)))
			Expect(gotIDs).To(HaveKey(int64(3)))

			// Results should be sorted by distance ascending.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance-1e-9))
			}
		})
	})

	Describe("Go search after mixed writes", func() {
		It("should find nearest neighbors across Go and Java writes", func() {
			// Go writes 2 records near origin.
			err := store.SaveOrderGo(ctx, 1, []float64{0.1, 0.1, 0.1})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, []float64{0.2, 0.2, 0.2})
			Expect(err).NotTo(HaveOccurred())

			// Java writes 2 records far from origin.
			err = store.SaveOrderJava(ctx, 3, []float64{50.0, 50.0, 50.0})
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 4, []float64{100.0, 100.0, 100.0})
			Expect(err).NotTo(HaveOccurred())

			// Go searches for 2 nearest neighbors to origin.
			results, err := store.SearchGo(ctx, []float64{0.0, 0.0, 0.0}, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			// The 2 closest should be ids 1 and 2 (Go-written).
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				gotIDs[r.PrimaryKey[0].(int64)] = true
			}
			Expect(gotIDs).To(HaveKey(int64(1)))
			Expect(gotIDs).To(HaveKey(int64(2)))
		})
	})

	Describe("Update: Go overwrites Java-written vector", func() {
		It("should update the HNSW graph correctly when Go overwrites a Java-inserted record", func() {
			// Java inserts a record far from origin.
			err := store.SaveOrderJava(ctx, 1, []float64{100.0, 100.0, 100.0})
			Expect(err).NotTo(HaveOccurred())

			// Go inserts a second record at origin.
			err = store.SaveOrderGo(ctx, 2, []float64{0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			// Go overwrites the first record with a vector near origin.
			err = store.SaveOrderGo(ctx, 1, []float64{0.1, 0.1, 0.1})
			Expect(err).NotTo(HaveOccurred())

			// Search near origin should find both records.
			results, err := store.SearchGo(ctx, []float64{0.0, 0.0, 0.0}, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			// Both should be very close to origin.
			for _, r := range results {
				Expect(r.Distance).To(BeNumerically("<", 1.0))
			}
		})
	})

	Describe("Batch save: Java writes multiple, Go verifies", func() {
		It("should handle batch Java writes visible to Go", func() {
			// Java saves 5 records in one transaction.
			err := store.SaveMultipleOrdersJava(ctx, []struct {
				ID     int64
				Vector []float64
			}{
				{1, []float64{1.0, 0.0, 0.0}},
				{2, []float64{0.0, 1.0, 0.0}},
				{3, []float64{0.0, 0.0, 1.0}},
				{4, []float64{1.0, 1.0, 0.0}},
				{5, []float64{0.0, 1.0, 1.0}},
			})
			Expect(err).NotTo(HaveOccurred())

			// Go verifies all 5 records exist.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(5))

			// Go can load each record.
			for i := int64(1); i <= 5; i++ {
				result, err := store.LoadOrderGo(ctx, i)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.OrderID).To(Equal(i))
				Expect(result.Vector).To(HaveLen(3))
			}

			// Go search returns results from Java batch.
			results, err := store.SearchGo(ctx, []float64{1.0, 0.0, 0.0}, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(1)))
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))
		})
	})

	Describe("Java searches Go-written HNSW graph", func() {
		It("should return the k closest neighbors", func() {
			// Go inserts 5 well-separated points.
			points := []struct {
				id  int64
				vec []float64
			}{
				{1, []float64{0.0, 0.0, 0.0}},
				{2, []float64{1.0, 1.0, 1.0}},
				{3, []float64{10.0, 10.0, 10.0}},
				{4, []float64{100.0, 100.0, 100.0}},
				{5, []float64{1000.0, 1000.0, 1000.0}},
			}
			for _, p := range points {
				err := store.SaveOrderGo(ctx, p.id, p.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java searches for 3 nearest neighbors to origin.
			ids, err := store.SearchJava(ctx, []float64{0.0, 0.0, 0.0}, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(HaveLen(3))

			// The 3 closest: id=1 (dist=0), id=2 (dist=3), id=3 (dist=300).
			idSet := make(map[int64]bool)
			for _, id := range ids {
				idSet[id] = true
			}
			Expect(idSet).To(HaveKey(int64(1)))
			Expect(idSet).To(HaveKey(int64(2)))
			Expect(idSet).To(HaveKey(int64(3)))
		})
	})

	Describe("Go searches Java-written HNSW graph", func() {
		It("should return the k closest neighbors", func() {
			// Java inserts 5 well-separated points in one transaction.
			err := store.SaveMultipleOrdersJava(ctx, []struct {
				ID     int64
				Vector []float64
			}{
				{1, []float64{0.0, 0.0, 0.0}},
				{2, []float64{1.0, 1.0, 1.0}},
				{3, []float64{10.0, 10.0, 10.0}},
				{4, []float64{100.0, 100.0, 100.0}},
				{5, []float64{1000.0, 1000.0, 1000.0}},
			})
			Expect(err).NotTo(HaveOccurred())

			// Go searches for 3 nearest neighbors to origin.
			results, err := store.SearchGo(ctx, []float64{0.0, 0.0, 0.0}, 3)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			// The 3 closest: id=1 (dist=0), id=2 (dist=3), id=3 (dist=300).
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				gotIDs[r.PrimaryKey[0].(int64)] = true
			}
			Expect(gotIDs).To(HaveKey(int64(1)))
			Expect(gotIDs).To(HaveKey(int64(2)))
			Expect(gotIDs).To(HaveKey(int64(3)))

			// Results should be sorted by distance ascending.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance-1e-9))
			}
		})
	})

	Describe("Vector serialization round-trip", func() {
		It("should preserve vector values across Go write and Java read", func() {
			// Use values that exercise floating-point edge cases (3 dimensions to match index config).
			edgeVec := []float64{math.Pi, math.E, 1e-300}
			err := store.SaveOrderGo(ctx, 42, edgeVec)
			Expect(err).NotTo(HaveOccurred())

			result, err := store.LoadOrderJava(ctx, 42)
			Expect(err).NotTo(HaveOccurred())
			Expect(result).NotTo(BeNil())

			// Verify vector values survived the round-trip through Go serialization,
			// FDB storage, and Java deserialization.
			Expect(result.Vector).To(HaveLen(len(edgeVec)))
			for i, expected := range edgeVec {
				if expected == 0.0 {
					Expect(result.Vector[i]).To(BeNumerically("~", 0.0, 1e-15))
				} else {
					Expect(result.Vector[i]).To(BeNumerically("~", expected, math.Abs(expected)*1e-9))
				}
			}
		})
	})
})

// --- Helper types and store wrapper ---

// VectorOrderResult holds the result of loading an Order with vector data.
type VectorOrderResult struct {
	OrderID int64
	Vector  []float64
}

// VectorIndexConformanceStore wraps record operations with a VECTOR (HNSW) index
// on Order's vector_data field.
type VectorIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	VecIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewVectorIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*VectorIndexConformanceStore, error) {
	// KeyWithValue(Field("vector_data"), 0): 0 key columns, vector bytes in value.
	// This matches Java's: new KeyWithValueExpression(field("vector_data"), 0)
	vecIdx := recordlayer.NewVectorIndex("order_vector",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 3)
	// Set the metric to match Java side.
	vecIdx.Options[recordlayer.IndexOptionVectorMetric] = "EUCLIDEAN_SQUARE_METRIC"

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", vecIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &VectorIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		VecIndex:    vecIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *VectorIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// serializeVector matches Go's hnsw.serializeVector: type_byte(0) + big-endian float64s.
func conformanceSerializeVector(vec []float64) []byte {
	buf := make([]byte, 1+8*len(vec))
	buf[0] = 2 // VectorType.DOUBLE.ordinal() = 2
	for i, v := range vec {
		bits := math.Float64bits(v)
		buf[1+i*8+0] = byte(bits >> 56)
		buf[1+i*8+1] = byte(bits >> 48)
		buf[1+i*8+2] = byte(bits >> 40)
		buf[1+i*8+3] = byte(bits >> 32)
		buf[1+i*8+4] = byte(bits >> 24)
		buf[1+i*8+5] = byte(bits >> 16)
		buf[1+i*8+6] = byte(bits >> 8)
		buf[1+i*8+7] = byte(bits)
	}
	return buf
}

// deserializeVectorConformance reads a serialized vector (type_byte + big-endian float64s).
func deserializeVectorConformance(data []byte) []float64 {
	if len(data) < 1 {
		return nil
	}
	numFloats := (len(data) - 1) / 8
	vec := make([]float64, numFloats)
	for i := 0; i < numFloats; i++ {
		var bits uint64
		for j := 0; j < 8; j++ {
			bits = (bits << 8) | uint64(data[1+i*8+j])
		}
		vec[i] = math.Float64frombits(bits)
	}
	return vec
}

func (s *VectorIndexConformanceStore) SaveOrderGo(ctx context.Context, orderID int64, vec []float64) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		vectorBytes := conformanceSerializeVector(vec)
		_, err = store.SaveRecord(&gen.Order{
			OrderId:    proto.Int64(orderID),
			VectorData: vectorBytes,
		})
		return nil, err
	})
	return err
}

func (s *VectorIndexConformanceStore) LoadOrderGo(ctx context.Context, orderID int64) (*VectorOrderResult, error) {
	var result *VectorOrderResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		rec, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, nil
		}
		order := rec.Record.(*gen.Order)
		result = &VectorOrderResult{
			OrderID: order.GetOrderId(),
			Vector:  deserializeVectorConformance(order.GetVectorData()),
		}
		return nil, nil
	})
	return result, err
}

func (s *VectorIndexConformanceStore) CountRecordsGo(ctx context.Context) (int, error) {
	var count int
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		count = len(records)
		return nil, nil
	})
	return count, err
}

func (s *VectorIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		deleted, err = store.DeleteRecord(tuple.Tuple{orderID})
		return nil, err
	})
	return deleted, err
}

func (s *VectorIndexConformanceStore) SearchGo(ctx context.Context, query []float64, k int) ([]recordlayer.VectorSearchResult, error) {
	var results []recordlayer.VectorSearchResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		results, err = store.SearchVectorIndex(s.VecIndex, query, k, 100)
		return nil, err
	})
	return results, err
}

// --- Java step wrappers ---

func (s *VectorIndexConformanceStore) SaveOrderJava(ctx context.Context, orderID int64, vec []float64) error {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	vecJSON, _ := json.Marshal(vec)
	params["vectorJson"] = string(vecJSON)
	return s.java.InvokeAs(ctx, "saveOrderWithVectorIndex", params, nil)
}

func (s *VectorIndexConformanceStore) LoadOrderJava(ctx context.Context, orderID int64) (*VectorOrderResult, error) {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	var raw map[string]any
	if err := s.java.InvokeAs(ctx, "loadOrderWithVectorIndex", params, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	result := &VectorOrderResult{
		OrderID: int64(raw["orderId"].(float64)),
	}
	if vecData, ok := raw["vectorData"]; ok {
		vecSlice := vecData.([]any)
		result.Vector = make([]float64, len(vecSlice))
		for i, v := range vecSlice {
			result.Vector[i] = v.(float64)
		}
	}
	return result, nil
}

func (s *VectorIndexConformanceStore) CountRecordsJava(ctx context.Context) (int64, error) {
	params := s.buildJavaParams()
	var count float64
	if err := s.java.InvokeAs(ctx, "countRecordsWithVectorIndex", params, &count); err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (s *VectorIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) (bool, error) {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	var deleted bool
	if err := s.java.InvokeAs(ctx, "deleteOrderWithVectorIndex", params, &deleted); err != nil {
		return false, err
	}
	return deleted, nil
}

func (s *VectorIndexConformanceStore) SearchJava(ctx context.Context, query []float64, k int) ([]int64, error) {
	params := s.buildJavaParams()
	vecJSON, _ := json.Marshal(query)
	params["vectorJson"] = string(vecJSON)
	params["k"] = int64(k)
	var raw []any
	if err := s.java.InvokeAs(ctx, "searchVectorIndex", params, &raw); err != nil {
		return nil, err
	}
	var ids []int64
	for _, entry := range raw {
		m := entry.(map[string]any)
		ids = append(ids, int64(m["orderId"].(float64)))
	}
	return ids, nil
}

func (s *VectorIndexConformanceStore) SaveMultipleOrdersJava(ctx context.Context, orders []struct {
	ID     int64
	Vector []float64
},
) error {
	type orderEntry struct {
		OrderID int64     `json:"orderId"`
		Vector  []float64 `json:"vector"`
	}
	entries := make([]orderEntry, len(orders))
	for i, o := range orders {
		entries[i] = orderEntry{OrderID: o.ID, Vector: o.Vector}
	}
	ordersJSON, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	params := s.buildJavaParams()
	params["ordersJson"] = string(ordersJSON)
	return s.java.InvokeAs(ctx, "saveMultipleOrdersWithVectorIndex", params, nil)
}

// =============================================================================
// RaBitQ VECTOR Index Conformance
// =============================================================================

var _ = Describe("RaBitQ VECTOR Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *RaBitQConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("rq_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewRaBitQConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes RaBitQ, Java reads", func() {
		It("should allow Java to load records from a Go-written RaBitQ HNSW graph", func() {
			// Go inserts 5 records with 8D vectors, RaBitQ-encoded.
			vectors := []struct {
				id  int64
				vec []float64
			}{
				{1, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{2, []float64{0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{3, []float64{0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{4, []float64{1.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{5, []float64{0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0}},
			}
			for _, v := range vectors {
				err := store.SaveOrderGo(ctx, v.id, v.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java loads each record — proves Java can open a store
			// whose HNSW graph was built with Go's RaBitQ encoding.
			for _, v := range vectors {
				result, err := store.LoadOrderJava(ctx, v.id)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.OrderID).To(Equal(v.id))
				// Record data (the raw protobuf with vector_data bytes) survives
				// cross-language round-trip. The vector_data field is opaque bytes
				// stored in the record, not in the HNSW graph.
				Expect(result.Vector).To(HaveLen(8))
				for i, val := range v.vec {
					Expect(result.Vector[i]).To(BeNumerically("~", val, 1e-9))
				}
			}

			// Java can count all records in the store.
			javaCount, err := store.CountRecordsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaCount).To(Equal(int64(5)))
		})
	})

	Describe("Java writes RaBitQ, Go reads", func() {
		It("should allow Go to load records from a Java-written RaBitQ HNSW graph", func() {
			// Java inserts 5 records with 8D vectors, RaBitQ-encoded.
			vectors := []struct {
				id  int64
				vec []float64
			}{
				{10, []float64{0.5, 0.5, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{20, []float64{0.0, 0.5, 0.5, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{30, []float64{0.0, 0.0, 0.5, 0.5, 0.0, 0.0, 0.0, 0.0}},
				{40, []float64{0.0, 0.0, 0.0, 0.5, 0.5, 0.0, 0.0, 0.0}},
				{50, []float64{0.0, 0.0, 0.0, 0.0, 0.5, 0.5, 0.0, 0.0}},
			}
			for _, v := range vectors {
				err := store.SaveOrderJava(ctx, v.id, v.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Go loads each record — proves Go can open a store
			// whose HNSW graph was built with Java's RaBitQ encoding.
			for _, v := range vectors {
				result, err := store.LoadOrderGo(ctx, v.id)
				Expect(err).NotTo(HaveOccurred())
				Expect(result).NotTo(BeNil())
				Expect(result.OrderID).To(Equal(v.id))
				Expect(result.Vector).To(HaveLen(8))
				for i, val := range v.vec {
					Expect(result.Vector[i]).To(BeNumerically("~", val, 1e-9))
				}
			}

			// Go can count all records.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(5))
		})
	})

	Describe("Cross-language RaBitQ search", func() {
		It("Go inserts with RaBitQ, Java searches with kNN", func() {
			// Go inserts 5 well-separated 8D points.
			points := []struct {
				id  int64
				vec []float64
			}{
				{1, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{2, []float64{0.9, 0.1, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{3, []float64{0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0}},
				{4, []float64{0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0, 0.0}},
				{5, []float64{0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0}},
			}
			for _, p := range points {
				err := store.SaveOrderGo(ctx, p.id, p.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java searches for 2 nearest neighbors to [1,0,0,...].
			// With cosine metric, ids 1 and 2 are closest (nearly aligned).
			ids, err := store.SearchJava(ctx, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(HaveLen(2))
			idSet := make(map[int64]bool)
			for _, id := range ids {
				idSet[id] = true
			}
			Expect(idSet).To(HaveKey(int64(1)))
			Expect(idSet).To(HaveKey(int64(2)))
		})

		It("Java inserts with RaBitQ, Go searches with kNN", func() {
			// Java inserts 5 well-separated 8D points.
			points := []struct {
				id  int64
				vec []float64
			}{
				{10, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{20, []float64{0.9, 0.1, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}},
				{30, []float64{0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0}},
				{40, []float64{0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0, 0.0}},
				{50, []float64{0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 1.0}},
			}
			for _, p := range points {
				err := store.SaveOrderJava(ctx, p.id, p.vec)
				Expect(err).NotTo(HaveOccurred())
			}

			// Java's access-info entry vector is already in storage coordinates,
			// even when its encoding is DOUBLE rather than RaBitQ. Unlike a plain
			// pre-centroid node vector, it must not be transformed again on read.
			var entry tuple.Tuple
			_, err := store.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				key := store.Keyspace.Sub(recordlayer.IndexKey, store.VecIndex.SubspaceTupleKey(), int64(1)).Pack(tuple.Tuple{})
				data, readErr := rtx.Transaction().Get(fdb.Key(key)).Get()
				if readErr != nil {
					return nil, readErr
				}
				entry, readErr = tuple.Unpack(data)
				return nil, readErr
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(entry).To(HaveLen(5))
			Expect(entry[1]).To(Equal(tuple.Tuple{int64(10)}))
			entryVector := entry[2].(tuple.Tuple)[0].([]byte)
			Expect(entryVector).To(HaveLen(1 + 8*8))
			Expect(entryVector[0]).To(Equal(byte(2))) // DOUBLE, not RaBitQ
			fmt.Fprintf(GinkgoWriter, "RABITQ-ENTRY pk=%v seed=%v vector=%x\n", entry[1], entry[3], entryVector)

			query := []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}
			javaIDs, err := store.SearchJava(ctx, query, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaIDs).To(Equal([]int64{10, 20}))
			allResults, err := store.SearchGo(ctx, query, len(points))
			Expect(err).NotTo(HaveOccurred())
			Expect(allResults).To(HaveLen(len(points)))
			fmt.Fprintf(GinkgoWriter, "RABITQ-ENTRY java top2=%v go all=%v\n", javaIDs, allResults)
			selfFound := false
			for _, r := range allResults {
				if r.PrimaryKey[0].(int64) == 10 {
					selfFound = true
					Expect(r.Distance).To(BeNumerically("~", 0, 1e-12), "the already-transformed entry vector must have zero self-distance")
				}
			}
			Expect(selfFound).To(BeTrue())

			// With cosine metric, ids 10 and 20 are closest.
			results, err := store.SearchGo(ctx, query, 2)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				gotIDs[r.PrimaryKey[0].(int64)] = true
			}
			Expect(gotIDs).To(HaveKey(int64(10)))
			Expect(gotIDs).To(HaveKey(int64(20)))
		})
	})

	Describe("Mixed writes with RaBitQ", func() {
		It("Go and Java both insert into the same RaBitQ HNSW graph", func() {
			// Go inserts 3 records.
			err := store.SaveOrderGo(ctx, 1, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderGo(ctx, 2, []float64{0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderGo(ctx, 3, []float64{0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			// Java inserts 2 more into the same RaBitQ graph.
			err = store.SaveOrderJava(ctx, 4, []float64{0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())
			err = store.SaveOrderJava(ctx, 5, []float64{0.0, 0.0, 0.0, 0.0, 1.0, 0.0, 0.0, 0.0})
			Expect(err).NotTo(HaveOccurred())

			// Both sides see all 5 records.
			goCount, err := store.CountRecordsGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goCount).To(Equal(5))

			javaCount, err := store.CountRecordsJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaCount).To(Equal(int64(5)))

			// Go search returns results from both Go and Java writes.
			results, err := store.SearchGo(ctx, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}, 5)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(5))
		})
	})
})

// --- RaBitQ conformance store wrapper ---

// RaBitQConformanceStore wraps record operations with a VECTOR (HNSW) index
// that has RaBitQ quantization enabled. Uses Cosine metric + 8 dimensions.
type RaBitQConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	VecIndex    *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewRaBitQConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*RaBitQConformanceStore, error) {
	vecIdx := recordlayer.NewVectorIndex("order_vector_rabitq",
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 8)
	vecIdx.Options[recordlayer.IndexOptionVectorMetric] = "COSINE_METRIC"
	vecIdx.Options["hnswUseRaBitQ"] = "true"
	vecIdx.Options["hnswRaBitQNumExBits"] = "4"

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", vecIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &RaBitQConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		VecIndex:    vecIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *RaBitQConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *RaBitQConformanceStore) SaveOrderGo(ctx context.Context, orderID int64, vec []float64) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		vectorBytes := conformanceSerializeVector(vec)
		_, err = store.SaveRecord(&gen.Order{
			OrderId:    proto.Int64(orderID),
			VectorData: vectorBytes,
		})
		return nil, err
	})
	return err
}

func (s *RaBitQConformanceStore) LoadOrderGo(ctx context.Context, orderID int64) (*VectorOrderResult, error) {
	var result *VectorOrderResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		rec, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}
		if rec == nil {
			return nil, nil
		}
		order := rec.Record.(*gen.Order)
		result = &VectorOrderResult{
			OrderID: order.GetOrderId(),
			Vector:  deserializeVectorConformance(order.GetVectorData()),
		}
		return nil, nil
	})
	return result, err
}

func (s *RaBitQConformanceStore) CountRecordsGo(ctx context.Context) (int, error) {
	var count int
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		records, err := recordlayer.AsList(ctx, store.ScanRecords(nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		count = len(records)
		return nil, nil
	})
	return count, err
}

func (s *RaBitQConformanceStore) SearchGo(ctx context.Context, query []float64, k int) ([]recordlayer.VectorSearchResult, error) {
	var results []recordlayer.VectorSearchResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		results, err = store.SearchVectorIndex(s.VecIndex, query, k, 100)
		return nil, err
	})
	return results, err
}

// --- RaBitQ Java step wrappers ---

func (s *RaBitQConformanceStore) SaveOrderJava(ctx context.Context, orderID int64, vec []float64) error {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	vecJSON, _ := json.Marshal(vec)
	params["vectorJson"] = string(vecJSON)
	return s.java.InvokeAs(ctx, "saveOrderWithRaBitQIndex", params, nil)
}

func (s *RaBitQConformanceStore) LoadOrderJava(ctx context.Context, orderID int64) (*VectorOrderResult, error) {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	var raw map[string]any
	if err := s.java.InvokeAs(ctx, "loadOrderWithRaBitQIndex", params, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, nil
	}
	result := &VectorOrderResult{
		OrderID: int64(raw["orderId"].(float64)),
	}
	if vecData, ok := raw["vectorData"]; ok {
		vecSlice := vecData.([]any)
		result.Vector = make([]float64, len(vecSlice))
		for i, v := range vecSlice {
			result.Vector[i] = v.(float64)
		}
	}
	return result, nil
}

func (s *RaBitQConformanceStore) CountRecordsJava(ctx context.Context) (int64, error) {
	params := s.buildJavaParams()
	var count float64
	if err := s.java.InvokeAs(ctx, "countRecordsWithRaBitQIndex", params, &count); err != nil {
		return 0, err
	}
	return int64(count), nil
}

func (s *RaBitQConformanceStore) SearchJava(ctx context.Context, query []float64, k int) ([]int64, error) {
	params := s.buildJavaParams()
	vecJSON, _ := json.Marshal(query)
	params["vectorJson"] = string(vecJSON)
	params["k"] = int64(k)
	var raw []any
	if err := s.java.InvokeAs(ctx, "searchRaBitQIndex", params, &raw); err != nil {
		return nil, err
	}
	var ids []int64
	for _, entry := range raw {
		m := entry.(map[string]any)
		ids = append(ids, int64(m["orderId"].(float64)))
	}
	return ids, nil
}

var _ = Describe("Vector pending entry conformance", func() {
	It("opens and writes an explicit format-15 vector store from both engines", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "format15_vec_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(env.Cleanup(ctx)).To(Succeed()) }()
		fixture, err := NewVectorIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
		_, err = env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
			store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(fixture.MetaData).SetSubspace(fixture.Keyspace).SetFormatVersion(15).Create()
			if err != nil {
				return nil, err
			}
			Expect(store.GetFormatVersion()).To(Equal(int32(15)))
			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(fixture.SaveOrderGo(ctx, 1, []float64{1, 2, 3})).To(Succeed())
		Expect(fixture.SaveOrderJava(ctx, 2, []float64{4, 5, 6})).To(Succeed())
		fromJava, err := fixture.LoadOrderJava(ctx, 1)
		Expect(err).NotTo(HaveOccurred())
		Expect(fromJava.Vector).To(Equal([]float64{1, 2, 3}))
		fromGo, err := fixture.LoadOrderGo(ctx, 2)
		Expect(err).NotTo(HaveOccurred())
		Expect(fromGo.Vector).To(Equal([]float64{4, 5, 6}))
		var header struct {
			FormatVersion int32 `json:"formatVersion"`
		}
		Expect(NewJavaInvoker().InvokeAs(ctx, "getStoreHeaderRaw", map[string]any{"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(fixture.Keyspace.Bytes())}, &header)).To(Succeed())
		Expect(header.FormatVersion).To(Equal(int32(15)))
		fmt.Fprintln(GinkgoWriter, "FORMAT15 both engines opened and wrote; persisted header=15")
	})

	It("exchanges computed insert update and delete entries without source records", func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		env, err := SetupTenantEnvironment(ctx, sharedContainer, "pending_vec_"+uuid.New().String())
		Expect(err).NotTo(HaveOccurred())
		defer func() { Expect(env.Cleanup(ctx)).To(Succeed()) }()
		fixture, err := NewVectorIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
		makeRecord := func(vector []float64) *recordlayer.FDBStoredRecord[proto.Message] {
			return &recordlayer.FDBStoredRecord[proto.Message]{Record: &gen.Order{OrderId: proto.Int64(42), VectorData: conformanceSerializeVector(vector)}, PrimaryKey: tuple.Tuple{int64(42)}, RecordType: fixture.MetaData.GetRecordType("Order")}
		}
		oldRecord, newRecord := makeRecord([]float64{1, 2, 3}), makeRecord([]float64{4, 5, 6})
		for _, operation := range []string{"insert", "update", "delete"} {
			var payload []byte
			_, err = env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(fixture.MetaData).SetSubspace(subspace.Sub(tuple.Tuple{"go"})).CreateOrOpen()
				Expect(err).NotTo(HaveOccurred())
				maintainer, err := store.GetIndexMaintainer(fixture.VecIndex)
				Expect(err).NotTo(HaveOccurred())
				var data *anypb.Any
				switch operation {
				case "insert":
					data, err = maintainer.SerializePendingWriteQueue(nil, oldRecord)
				case "update":
					data, err = maintainer.SerializePendingWriteQueue(oldRecord, newRecord)
				case "delete":
					data, err = maintainer.SerializePendingWriteQueue(newRecord, nil)
				}
				Expect(err).NotTo(HaveOccurred())
				payload, err = proto.Marshal(data)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())
			var result struct {
				Payload     string  `json:"payload"`
				IDs         []int64 `json:"ids"`
				RecordCount int     `json:"recordCount"`
			}
			err = NewJavaInvoker().InvokeAs(ctx, "replayVectorPendingEntry", map[string]any{
				"clusterFile": env.ClusterFile, "tenantName": env.TenantName, "subspace": BytesToIntArray(subspace.Sub(tuple.Tuple{"java"}).Bytes()),
				"operation": operation, "payloadHex": hex.EncodeToString(payload),
			}, &result)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Payload).To(Equal(hex.EncodeToString(payload)), operation)
			Expect(result.RecordCount).To(BeZero())
			if operation == "delete" {
				Expect(result.IDs).To(BeEmpty())
			} else {
				Expect(result.IDs).To(Equal([]int64{42}))
			}
			javaBytes, err := hex.DecodeString(result.Payload)
			Expect(err).NotTo(HaveOccurred())
			_, err = env.RecordDB.Run(ctx, func(rc *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().SetContext(rc).SetMetaDataProvider(fixture.MetaData).SetSubspace(subspace.Sub(tuple.Tuple{"go"})).Open()
				Expect(err).NotTo(HaveOccurred())
				maintainer, err := store.GetIndexMaintainer(fixture.VecIndex)
				Expect(err).NotTo(HaveOccurred())
				var data anypb.Any
				Expect(proto.Unmarshal(javaBytes, &data)).To(Succeed())
				Expect(maintainer.UpdateFromQueue(&data)).To(Succeed())
				found, err := store.SearchVectorIndex(fixture.VecIndex, []float64{4, 5, 6}, 100, 100)
				Expect(err).NotTo(HaveOccurred())
				if operation == "delete" {
					Expect(found).To(BeEmpty())
				} else {
					Expect(found).To(HaveLen(1))
					Expect(found[0].PrimaryKey).To(Equal(tuple.Tuple{int64(42)}))
					if operation == "update" {
						Expect(found[0].Distance).To(BeZero())
					}
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())
			fmt.Fprintf(GinkgoWriter, "VECTOR-QUEUE operation=%s payload=%s source-records=%d\n", operation, result.Payload, result.RecordCount)
		}
	})
})

// Java's HNSW constructs its RaBitQuantizer, whose constructor refuses more than
// 8 extra bits while its Config admits up to 15, only where an operation
// quantizes: Primitives.quantizer once the access info can use RaBitQ, and
// Insert.firstInsert for a metric that is not translation-preserving. Both
// engines maintain the same index through the record store, one save per
// transaction, and each operation's outcome is compared: a Euclidean index
// accepts saves until its centroid is established (statistics sampled and
// maintained on every insert, threshold 11) and after it refuses every save
// that quantizes (one that inserts or deletes a node), every search and every
// delete of a present node; a cosine index refuses its first save; a search of the empty index is
// served. A save that leaves a record's vector unchanged makes no graph call in
// Java (StandardIndexMaintainer.update drops the entry common to the old and
// the new record), so it is served even after the centroid; Go skips it the
// same way. 8 extra bits is the control.
var _ = Describe("An HNSW index with more RaBitQ extra bits than the quantizer encodes is refused where Java constructs it", func() {
	vectors := make([][]float64, 16)
	for i := range vectors {
		vectors[i] = make([]float64, 8)
		for d := range vectors[i] {
			vectors[i][d] = float64((i*7+d*3)%11) - 5 + 0.5*float64(d)
		}
	}
	for _, c := range []struct {
		metric string
		bits   int
	}{
		{"EUCLIDEAN_METRIC", 9},
		{"EUCLIDEAN_METRIC", 15},
		{"COSINE_METRIC", 9},
		{"EUCLIDEAN_METRIC", 8},
	} {
		It(fmt.Sprintf("%s with %d extra bits", c.metric, c.bits), func() {
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
			defer cancel()
			javaEnv, err := SetupTenantEnvironment(ctx, sharedContainer, "bits_java_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(javaEnv.Cleanup(ctx)).To(Succeed()) }()
			goEnv, err := SetupTenantEnvironment(ctx, sharedContainer, "bits_go_"+uuid.New().String())
			Expect(err).NotTo(HaveOccurred())
			defer func() { Expect(goEnv.Cleanup(ctx)).To(Succeed()) }()

			var java struct {
				SearchEmpty           string   `json:"searchEmpty"`
				Inserts               []string `json:"inserts"`
				ResaveUnchangedVector string   `json:"resaveUnchangedVector"`
				Search                string   `json:"search"`
				DeleteFirst           string   `json:"deleteFirst"`
			}
			Expect(NewJavaInvoker().InvokeAs(ctx, "hnswExtraBitsProbe", map[string]any{
				"clusterFile": javaEnv.ClusterFile, "tenantName": javaEnv.TenantName,
				"subspace": BytesToIntArray(subspace.Sub(tuple.Tuple{}).Bytes()),
				"metric":   c.metric, "numExBits": c.bits, "vectors": vectors,
			}, &java)).To(Succeed())

			index := recordlayer.NewVectorIndex("order_vector_bits", recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 8)
			index.Options = map[string]string{
				"hnswNumDimensions": "8", "hnswMetric": c.metric,
				"hnswUseRaBitQ": "true", "hnswRaBitQNumExBits": fmt.Sprint(c.bits),
				"hnswSampleVectorStatsProbability": "1.0", "hnswMaintainStatsProbability": "1.0", "hnswStatsThreshold": "11",
			}
			builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
			builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
			builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
			builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
			builder.AddIndex("Order", index)
			md, err := builder.Build()
			Expect(err).NotTo(HaveOccurred())
			outcome := func(fn func(*recordlayer.FDBRecordStore) error) string {
				_, err := goEnv.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
					store, err := recordlayer.NewStoreBuilder().SetContext(rtx).SetMetaDataProvider(md).
						SetSubspace(subspace.Sub(tuple.Tuple{})).CreateOrOpen()
					if err != nil {
						return nil, err
					}
					return nil, fn(store)
				})
				var iae *recordlayer.IllegalArgumentError
				switch {
				case err == nil:
					return "ok"
				case errors.As(err, &iae):
					return "java.lang.IllegalArgumentException"
				default:
					return fmt.Sprintf("%T: %v", err, err)
				}
			}
			search := func(s *recordlayer.FDBRecordStore) error {
				_, err := s.SearchVectorIndex(index, vectors[0], 3, 100)
				return err
			}
			goSearchEmpty := outcome(search)
			var goInserts []string
			for i, v := range vectors {
				goInserts = append(goInserts, outcome(func(s *recordlayer.FDBRecordStore) error {
					_, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(int64(i)), VectorData: conformanceSerializeVector(v)})
					return err
				}))
			}
			goResave := outcome(func(s *recordlayer.FDBRecordStore) error {
				_, err := s.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(7), VectorData: conformanceSerializeVector(vectors[1])})
				return err
			})
			goSearch := outcome(search)
			goDelete := outcome(func(s *recordlayer.FDBRecordStore) error {
				_, err := s.DeleteRecord(tuple.Tuple{int64(0)})
				return err
			})
			fmt.Fprintf(GinkgoWriter, "EXTRA_BITS %s/%d java=%+v go={%s %v %s %s %s}\n", c.metric, c.bits, java,
				goSearchEmpty, goInserts, goResave, goSearch, goDelete)
			Expect(goSearchEmpty).To(Equal(java.SearchEmpty), "search of the empty index")
			Expect(goInserts).To(Equal(java.Inserts), "each save")
			Expect(goResave).To(Equal(java.ResaveUnchangedVector), "a save leaving the vector unchanged")
			Expect(goSearch).To(Equal(java.Search), "search")
			Expect(goDelete).To(Equal(java.DeleteFirst), "delete of record 0")
		})
	}
})
