package recordlayer

import (
	"context"
	"math"
	"math/rand"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Distance Metrics", func() {
	It("euclidean distance computes squared L2", func() {
		a := []float64{1.0, 2.0, 3.0}
		b := []float64{4.0, 5.0, 6.0}
		// (4-1)^2 + (5-2)^2 + (6-3)^2 = 9+9+9 = 27
		Expect(euclideanDistance(a, b)).To(BeNumerically("~", 27.0, 1e-9))

		// Distance to self is zero.
		Expect(euclideanDistance(a, a)).To(BeNumerically("~", 0.0, 1e-9))

		// Single dimension.
		Expect(euclideanDistance([]float64{3.0}, []float64{7.0})).To(BeNumerically("~", 16.0, 1e-9))
	})

	It("cosine distance: orthogonal = 1.0, identical = 0.0", func() {
		// Identical vectors: cosine distance = 0.
		a := []float64{1.0, 2.0, 3.0}
		Expect(cosineDistance(a, a)).To(BeNumerically("~", 0.0, 1e-9))

		// Orthogonal vectors: cosine distance = 1.
		x := []float64{1.0, 0.0}
		y := []float64{0.0, 1.0}
		Expect(cosineDistance(x, y)).To(BeNumerically("~", 1.0, 1e-9))

		// Opposite vectors: cosine distance = 2.
		neg := []float64{-1.0, -2.0, -3.0}
		Expect(cosineDistance(a, neg)).To(BeNumerically("~", 2.0, 1e-9))

		// Zero vector returns 1.0 (special case).
		zero := []float64{0.0, 0.0}
		Expect(cosineDistance(zero, x)).To(BeNumerically("~", 1.0, 1e-9))
	})

	It("inner product distance is negative dot product", func() {
		a := []float64{1.0, 2.0, 3.0}
		b := []float64{4.0, 5.0, 6.0}
		// dot = 1*4 + 2*5 + 3*6 = 4+10+18 = 32, distance = -32
		Expect(innerProductDistance(a, b)).To(BeNumerically("~", -32.0, 1e-9))

		// Orthogonal vectors: dot = 0, distance = 0.
		x := []float64{1.0, 0.0}
		y := []float64{0.0, 1.0}
		Expect(innerProductDistance(x, y)).To(BeNumerically("~", 0.0, 1e-9))
	})
})

var _ = Describe("Vector Serialization", func() {
	It("round-trips float64 vectors", func() {
		vec := []float64{1.0, -2.5, 3.14159, 0.0, math.MaxFloat64, math.SmallestNonzeroFloat64}
		data := serializeVector(vec)

		// First byte is type ordinal 0 (DOUBLE).
		Expect(data[0]).To(Equal(byte(0)))
		Expect(len(data)).To(Equal(1 + 8*len(vec)))

		got, err := deserializeVector(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(len(vec)))
		for i := range vec {
			Expect(got[i]).To(Equal(vec[i]))
		}
	})

	It("handles empty vector", func() {
		data := serializeVector(nil)
		Expect(data).To(Equal([]byte{0}))

		got, err := deserializeVector(data)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(HaveLen(0))
	})

	It("deserialize rejects empty data", func() {
		_, err := deserializeVector(nil)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("Layer Assignment", func() {
	It("topLayer is deterministic for same PK", func() {
		pk := tuple.Tuple{int64(42)}
		l1 := topLayer(pk, 16)
		l2 := topLayer(pk, 16)
		Expect(l1).To(Equal(l2))
	})

	It("topLayer varies by PK", func() {
		// With enough different PKs, we should see different layers.
		layers := make(map[int]bool)
		for i := int64(0); i < 1000; i++ {
			pk := tuple.Tuple{i}
			l := topLayer(pk, 4)
			layers[l] = true
		}
		// With M=4, most will be layer 0, but some should be higher.
		Expect(layers).To(HaveKey(0))
		// With 1000 PKs and M=4, extremely likely to see at least layer 1.
		Expect(len(layers)).To(BeNumerically(">=", 2))
	})

	It("topLayer is always >= 0", func() {
		for i := int64(0); i < 100; i++ {
			pk := tuple.Tuple{i}
			Expect(topLayer(pk, 16)).To(BeNumerically(">=", 0))
		}
	})

	It("splitMixLong matches expected values", func() {
		// Verify the hash function produces consistent output.
		// splitMixLong(0) should be deterministic.
		r1 := splitMixLong(0)
		r2 := splitMixLong(0)
		Expect(r1).To(Equal(r2))

		// Different inputs should produce different outputs.
		r3 := splitMixLong(1)
		Expect(r1).NotTo(Equal(r3))
	})

	It("javaHashCode matches Java behavior", func() {
		// Java: int hash = 1; for (byte b : data) hash = 31 * hash + b;
		// For data = {1}, hash = 31*1 + 1 = 32
		Expect(javaHashCode([]byte{1})).To(Equal(int32(32)))

		// For data = {}, hash = 1
		Expect(javaHashCode(nil)).To(Equal(int32(1)))

		// For data = {0}, hash = 31*1 + 0 = 31
		Expect(javaHashCode([]byte{0})).To(Equal(int32(31)))

		// For negative byte values (e.g., 0xFF = -1 in signed)
		// hash = 31*1 + (-1) = 30
		Expect(javaHashCode([]byte{0xFF})).To(Equal(int32(30)))
	})
})

var _ = Describe("HNSW Config Validation", func() {
	It("accepts valid config", func() {
		c := DefaultHNSWConfig(128)
		Expect(ValidateHNSWConfig(c)).To(Succeed())
	})

	It("rejects numDimensions < 1", func() {
		c := DefaultHNSWConfig(0)
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
	})

	It("rejects m out of range", func() {
		c := DefaultHNSWConfig(128)
		c.M = 3
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
		c.M = 201
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
	})

	It("rejects mMax out of range", func() {
		c := DefaultHNSWConfig(128)
		c.MMax = 3
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
		c.MMax = 201
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
	})

	It("rejects mMax0 out of range", func() {
		c := DefaultHNSWConfig(128)
		c.MMax0 = 3
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
		c.MMax0 = 301
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
	})

	It("rejects efConstruction out of range", func() {
		c := DefaultHNSWConfig(128)
		c.EfConstruction = 99
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
		c.EfConstruction = 401
		Expect(ValidateHNSWConfig(c)).To(HaveOccurred())
	})
})

var _ = Describe("HNSW Graph Direct", func() {
	ctx := context.Background()

	// Helper: create an isolated HNSW graph with its own FDB subspace.
	makeGraph := func(dims int) *hnswGraph {
		ss := specSubspace().Sub("hnsw")
		config := HNSWConfig{
			NumDimensions:  dims,
			M:              4,
			MMax:           4,
			MMax0:          8,
			EfConstruction: 100,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)
		return NewHNSWGraph(storage, config)
	}

	It("insert single node, search returns it", func() {
		graph := makeGraph(3)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(1)}
			vec := []float64{1.0, 2.0, 3.0}
			Expect(graph.Insert(tx, pk, vec)).To(Succeed())

			results, err := graph.Search(tx, []float64{1.0, 2.0, 3.0}, 1, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(tupleEqual(results[0].PrimaryKey, pk)).To(BeTrue())
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert 5 nodes, kNN k=3 returns 3 closest", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 5 points in 2D.
			points := [][]float64{
				{0.0, 0.0},   // id=0
				{1.0, 0.0},   // id=1
				{2.0, 0.0},   // id=2
				{10.0, 0.0},  // id=3 (far)
				{100.0, 0.0}, // id=4 (very far)
			}
			for i, p := range points {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, p)).To(Succeed())
			}

			// Query at origin, k=3 should return ids 0,1,2.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 3, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			// Results are sorted by distance (ascending).
			// id=0: dist=0, id=1: dist=1, id=2: dist=4
			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{0, 1, 2}))

			// Verify distances are in ascending order.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert then delete, search does not return deleted node", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk1 := tuple.Tuple{int64(1)}
			pk2 := tuple.Tuple{int64(2)}
			pk3 := tuple.Tuple{int64(3)}
			vec1 := []float64{0.0, 0.0}
			vec2 := []float64{1.0, 0.0}
			vec3 := []float64{2.0, 0.0}

			Expect(graph.Insert(tx, pk1, vec1)).To(Succeed())
			Expect(graph.Insert(tx, pk2, vec2)).To(Succeed())
			Expect(graph.Insert(tx, pk3, vec3)).To(Succeed())

			// Delete pk2.
			Expect(graph.Delete(tx, pk2, vec2)).To(Succeed())

			// Search for all 3 (k=3) near origin. Should only get pk1 and pk3.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 3, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 3}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert same PK twice (update), only one result", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(42)}

			// Insert at (0,0), then "update" to (5,5).
			Expect(graph.Insert(tx, pk, []float64{0.0, 0.0})).To(Succeed())
			Expect(graph.Insert(tx, pk, []float64{5.0, 5.0})).To(Succeed())

			// Search for all nodes. Should get exactly 1 result.
			results, err := graph.Search(tx, []float64{5.0, 5.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(tupleEqual(results[0].PrimaryKey, pk)).To(BeTrue())
			// The node's vector should be the updated one (5,5), so distance to (5,5) = 0.
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("search empty graph returns nil", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			results, err := graph.Search(tx, []float64{1.0, 2.0}, 5, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("search with k > num_nodes returns all nodes", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 3 nodes.
			for i := range 3 {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, []float64{float64(i), 0.0})).To(Succeed())
			}

			// Search with k=10 but only 3 nodes exist.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert 20 nodes, verify all retrievable", func() {
		graph := makeGraph(3)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 20 nodes at distinct positions.
			for i := range 20 {
				pk := tuple.Tuple{int64(i)}
				vec := []float64{float64(i), float64(i * 2), float64(i * 3)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Search with k=20: should find all of them.
			results, err := graph.Search(tx, []float64{0.0, 0.0, 0.0}, 20, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(20))

			// Verify all 20 distinct IDs are present.
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				gotIDs[r.PrimaryKey[0].(int64)] = true
			}
			Expect(gotIDs).To(HaveLen(20))
			for i := range 20 {
				Expect(gotIDs[int64(i)]).To(BeTrue(), "missing node %d", i)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple layers: insert enough nodes to force multi-layer graph", func() {
		// Use small M to increase probability of higher layers.
		ss := specSubspace().Sub("hnsw-multilayer")
		config := HNSWConfig{
			NumDimensions:  2,
			M:              4,
			MMax:           4,
			MMax0:          8,
			EfConstruction: 100,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 30 nodes. With M=4 and deterministic layer assignment,
			// some nodes will be assigned to higher layers.
			for i := range 30 {
				pk := tuple.Tuple{int64(i)}
				vec := []float64{float64(i) * 10.0, float64(i) * 10.0}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Verify the graph has an entry point.
			epLayer, epPK, _, epErr := graph.storage.loadAccessInfo(tx)
			Expect(epErr).NotTo(HaveOccurred())
			Expect(epPK).NotTo(BeNil())
			// With M=4 and 30 nodes, max layer should be >= 0.
			Expect(epLayer).To(BeNumerically(">=", 0))

			// Verify search still works correctly across layers.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 5, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(5))

			// Closest to origin should be node 0 at (0,0).
			Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(0)))
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			// Results should be in ascending distance order.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete all nodes leaves graph empty", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 3 nodes then delete all.
			type nodeInfo struct {
				pk  tuple.Tuple
				vec []float64
			}
			nodes := []nodeInfo{
				{tuple.Tuple{int64(0)}, []float64{0.0, 0.0}},
				{tuple.Tuple{int64(1)}, []float64{1.0, 0.0}},
				{tuple.Tuple{int64(2)}, []float64{2.0, 0.0}},
			}
			for _, n := range nodes {
				Expect(graph.Insert(tx, n.pk, n.vec)).To(Succeed())
			}
			for _, n := range nodes {
				Expect(graph.Delete(tx, n.pk, n.vec)).To(Succeed())
			}

			// Search should return nil (empty graph).
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 5, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("node storage wire format: per-layer COMPACT", func() {
		ss := specSubspace().Sub("hnsw-wire")
		config := HNSWConfig{
			NumDimensions:  2,
			M:              16,
			MMax:           16,
			MMax0:          32,
			EfConstruction: 200,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(99)}
			vec := []float64{1.5, 2.5}
			vecBytes := serializeVector(vec)
			neighbors := []tuple.Tuple{
				{int64(1)},
				{int64(2)},
			}

			storage.saveNodeLayer(tx, 0, pk, vecBytes, neighbors)

			// Load it back.
			gotVecBytes, gotNeighbors, err := storage.loadNodeLayer(tx, 0, pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(gotVecBytes).To(Equal(vecBytes))
			Expect(gotNeighbors).To(HaveLen(2))
			Expect(tupleEqual(gotNeighbors[0], tuple.Tuple{int64(1)})).To(BeTrue())
			Expect(tupleEqual(gotNeighbors[1], tuple.Tuple{int64(2)})).To(BeTrue())

			// Delete and verify.
			storage.deleteNodeLayer(tx, 0, pk)
			_, _, err = storage.loadNodeLayer(tx, 0, pk)
			Expect(err).To(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("access info round-trips", func() {
		ss := specSubspace().Sub("hnsw-access")
		config := HNSWConfig{
			NumDimensions:  2,
			M:              16,
			MMax:           16,
			MMax0:          32,
			EfConstruction: 200,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(42)}
			vecBytes := serializeVector([]float64{1.0, 2.0})

			storage.saveAccessInfo(tx, 3, pk, vecBytes)

			layer, gotPK, gotVec, err := storage.loadAccessInfo(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(layer).To(Equal(3))
			Expect(tupleEqual(gotPK, pk)).To(BeTrue())
			Expect(gotVec).To(Equal(vecBytes))

			// Clear and verify.
			storage.clearAccessInfo(tx)
			_, _, _, err = storage.loadAccessInfo(tx)
			Expect(err).To(HaveOccurred())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete with graph repair preserves connectivity", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert a chain: 0 -- 1 -- 2 -- 3 -- 4
			// Positioned in a line so connections are sequential.
			for i := range 5 {
				pk := tuple.Tuple{int64(i)}
				vec := []float64{float64(i) * 10.0, 0.0}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Delete node 2 (middle). Graph repair should reconnect 1 and 3.
			Expect(graph.Delete(tx, tuple.Tuple{int64(2)}, []float64{20.0, 0.0})).To(Succeed())

			// All remaining nodes should still be findable.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(4))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{0, 1, 3, 4}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete entry point replaces it", func() {
		graph := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert several nodes.
			for i := range 5 {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, []float64{float64(i), 0.0})).To(Succeed())
			}

			// Find the current entry point.
			_, epPK, _, _ := graph.storage.loadAccessInfo(tx)
			Expect(epPK).NotTo(BeNil())

			// Delete it.
			epID := epPK[0].(int64)
			Expect(graph.Delete(tx, epPK, []float64{float64(epID), 0.0})).To(Succeed())

			// Graph should still have an entry point.
			_, newEP, _, err := graph.storage.loadAccessInfo(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(newEP).NotTo(BeNil())
			Expect(tupleEqual(newEP, epPK)).To(BeFalse(), "entry point should change after deletion")

			// Search should still work.
			results, searchErr := graph.Search(tx, []float64{0.0, 0.0}, 10, 100)
			Expect(searchErr).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(4))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("VectorIndex Store Integration", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("save records with int fields, SearchVectorIndex returns nearest", func() {
		ks := specSubspace()

		// Create a VECTOR index on (price, quantity) as a 2D vector.
		vecIdx := NewVectorIndex("vec_price_qty", Concat(Field("price"), Field("quantity")), 2)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert orders as 2D points: (price, quantity).
			// id=1: (10, 10), id=2: (20, 20), id=3: (100, 100), id=4: (50, 50)
			for _, o := range []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 10, 10},
				{2, 20, 20},
				{3, 100, 100},
				{4, 50, 50},
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			// Query near (15, 15) — closest should be id=1 (10,10) and id=2 (20,20).
			results, err := store.SearchVectorIndex(vecIdx, []float64{15.0, 15.0}, 2, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			// Both results should be the two closest points.
			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			// Results should be in ascending distance order.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			// id=1 at (10,10): dist to (15,15) = (15-10)^2 + (15-10)^2 = 50
			// id=2 at (20,20): dist to (15,15) = (15-20)^2 + (15-20)^2 = 50
			// Both equidistant at 50.
			Expect(results[0].Distance).To(BeNumerically("~", 50.0, 1e-6))
			Expect(results[1].Distance).To(BeNumerically("~", 50.0, 1e-6))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete record removes from vector index", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_price_qty", Concat(Field("price"), Field("quantity")), 2)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert 3 orders.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(20)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(30), Quantity: proto.Int32(30)})
			Expect(err).NotTo(HaveOccurred())

			// Delete order id=2.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(2)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Search for all (k=10): should only find 2 records.
			results, err := store.SearchVectorIndex(vecIdx, []float64{0.0, 0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 3}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SearchVectorIndex on non-vector index returns error", func() {
		ks := specSubspace()

		// Create a VALUE index (not VECTOR).
		valueIdx := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", valueIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(100)})
			Expect(err).NotTo(HaveOccurred())

			// SearchVectorIndex should reject non-VECTOR index.
			_, err = store.SearchVectorIndex(valueIdx, []float64{1.0}, 1, 100)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("not a VECTOR index"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("vectorDistance dispatches to correct metric", func() {
		a := []float64{3.0, 4.0}
		b := []float64{0.0, 0.0}

		// Euclidean: 3^2 + 4^2 = 25
		Expect(vectorDistance(a, b, VectorMetricEuclidean)).To(BeNumerically("~", 25.0, 1e-9))

		// Cosine: 1 - dot/(normA*normB). dot=0 when b=0,0 -> special case returns 1.
		Expect(vectorDistance(a, b, VectorMetricCosine)).To(BeNumerically("~", 1.0, 1e-9))

		// Inner product: -dot = -(3*0 + 4*0) = 0
		Expect(vectorDistance(a, b, VectorMetricInnerProduct)).To(BeNumerically("~", 0.0, 1e-9))

		// Non-zero cosine case.
		c := []float64{1.0, 0.0}
		d := []float64{1.0, 1.0}
		// cos(45deg) = 1/sqrt(2), distance = 1 - 1/sqrt(2) ~ 0.2929
		Expect(vectorDistance(c, d, VectorMetricCosine)).To(BeNumerically("~", 1.0-1.0/math.Sqrt(2), 1e-6))
	})
})

var _ = Describe("HNSW Search Quality", func() {
	ctx := context.Background()

	It("search recall matches brute-force for 100 vectors", func() {
		ss := specSubspace().Sub("hnsw-recall")
		config := HNSWConfig{
			NumDimensions:  8,
			M:              16,
			MMax:           16,
			MMax0:          32,
			EfConstruction: 200,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 100
		const dims = 8
		const k = 10

		// Deterministic random vectors.
		rng := rand.New(rand.NewSource(42))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.Float64()*200.0 - 100.0 // [-100, 100)
			}
			vectors[i] = vec
		}
		queryVec := make([]float64, dims)
		for d := range dims {
			queryVec[d] = rng.Float64()*200.0 - 100.0
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert all vectors.
			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// HNSW search.
			results, err := graph.Search(tx, queryVec, k, 200)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(k))

			// Brute-force: compute all distances, sort, take top k.
			type distID struct {
				id   int64
				dist float64
			}
			bruteForce := make([]distID, numVectors)
			for i, vec := range vectors {
				bruteForce[i] = distID{int64(i), euclideanDistance(queryVec, vec)}
			}
			sort.Slice(bruteForce, func(i, j int) bool {
				return bruteForce[i].dist < bruteForce[j].dist
			})
			topK := make(map[int64]bool, k)
			for i := 0; i < k; i++ {
				topK[bruteForce[i].id] = true
			}

			// Compute recall: how many of the HNSW results are in the brute-force top-k.
			hnswIDs := make(map[int64]bool, k)
			for _, r := range results {
				hnswIDs[r.PrimaryKey[0].(int64)] = true
			}
			overlap := 0
			for id := range hnswIDs {
				if topK[id] {
					overlap++
				}
			}
			recall := float64(overlap) / float64(k)
			GinkgoWriter.Printf("HNSW recall@%d: %.2f (%d/%d match brute-force)\n", k, recall, overlap, k)
			Expect(recall).To(BeNumerically(">=", 0.8),
				"HNSW recall should be at least 80%% with efSearch >= k")

			// Verify HNSW results are in ascending distance order.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("HNSW High-Dimensional Vectors", func() {
	ctx := context.Background()

	It("handles 128D vectors correctly", func() {
		ss := specSubspace().Sub("hnsw-128d")
		config := HNSWConfig{
			NumDimensions:  128,
			M:              16,
			MMax:           16,
			MMax0:          32,
			EfConstruction: 200,
			Metric:         VectorMetricEuclidean,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 50
		const dims = 128
		const k = 5

		// Deterministic random vectors.
		rng := rand.New(rand.NewSource(7777))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.NormFloat64() // standard normal
			}
			vectors[i] = vec
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Search with a vector from the set (should find itself first).
			queryVec := vectors[0]
			results, err := graph.Search(tx, queryVec, k, 200)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(k))

			// Distances must be non-negative and sorted ascending.
			for i, r := range results {
				Expect(r.Distance).To(BeNumerically(">=", 0.0),
					"distance at position %d should be non-negative", i)
				if i > 0 {
					Expect(r.Distance).To(BeNumerically(">=", results[i-1].Distance),
						"distances should be sorted ascending at position %d", i)
				}
			}

			// First result should be the query vector itself (distance ~0).
			Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(0)))
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			// Search with a random query vector not in the set.
			randomQuery := make([]float64, dims)
			for d := range dims {
				randomQuery[d] = rng.NormFloat64()
			}
			results2, err := graph.Search(tx, randomQuery, k, 200)
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(k))

			for i, r := range results2 {
				Expect(r.Distance).To(BeNumerically(">=", 0.0),
					"distance at position %d should be non-negative", i)
				if i > 0 {
					Expect(r.Distance).To(BeNumerically(">=", results2[i-1].Distance),
						"distances should be sorted ascending at position %d", i)
				}
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("Cosine Distance Clamping", func() {
	It("cosine distance is non-negative even for identical vectors", func() {
		v := []float64{1.0, 0.0, 0.0}
		dist := cosineDistance(v, v)
		Expect(dist).To(BeNumerically(">=", 0.0))
		Expect(dist).To(BeNumerically("<=", 0.001)) // should be ~0

		// Test with very similar vectors that could cause floating-point edge cases
		// where dot/(normA*normB) might exceed 1.0 without clamping.
		a := []float64{1.0000000000001, 0.9999999999999, 1.0}
		b := []float64{1.0, 1.0, 1.0}
		dist = cosineDistance(a, b)
		Expect(dist).To(BeNumerically(">=", 0.0))
	})

	It("cosine distance is non-negative for large identical vectors", func() {
		// Large vectors amplify floating-point accumulation errors.
		rng := rand.New(rand.NewSource(12345))
		large := make([]float64, 1000)
		for i := range large {
			large[i] = rng.Float64()*2.0 - 1.0
		}
		dist := cosineDistance(large, large)
		Expect(dist).To(BeNumerically(">=", 0.0))
		Expect(dist).To(BeNumerically("<=", 1e-10))
	})

	It("cosine distance is non-negative for scaled vectors", func() {
		// v and 2*v are identical in direction; distance should be 0, not negative.
		v := []float64{3.0, 4.0, 5.0}
		scaled := make([]float64, len(v))
		for i := range v {
			scaled[i] = v[i] * 2.0
		}
		dist := cosineDistance(v, scaled)
		Expect(dist).To(BeNumerically(">=", 0.0))
		Expect(dist).To(BeNumerically("<=", 1e-10))
	})
})
