//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

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

			// Go searches for 2 nearest neighbors to [1,0,0,...].
			// With cosine metric, ids 10 and 20 are closest.
			results, err := store.SearchGo(ctx, []float64{1.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0, 0.0}, 2)
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
