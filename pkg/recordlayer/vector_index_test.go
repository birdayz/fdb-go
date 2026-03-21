package recordlayer

import (
	"context"
	"encoding/binary"
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

	It("deserializes float32 vectors", func() {
		buf := make([]byte, 1+4*3)
		buf[0] = 1 // SINGLE type
		binary.BigEndian.PutUint32(buf[1:], math.Float32bits(1.0))
		binary.BigEndian.PutUint32(buf[5:], math.Float32bits(2.5))
		binary.BigEndian.PutUint32(buf[9:], math.Float32bits(-0.5))

		vec, err := deserializeVector(buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(vec).To(HaveLen(3))
		Expect(vec[0]).To(BeNumerically("~", 1.0, 1e-6))
		Expect(vec[1]).To(BeNumerically("~", 2.5, 1e-6))
		Expect(vec[2]).To(BeNumerically("~", -0.5, 1e-6))
	})

	It("deserializes float16 vectors", func() {
		// float16 for 1.0 = 0x3C00, float16 for 2.0 = 0x4000
		buf := []byte{2, 0x3C, 0x00, 0x40, 0x00}
		vec, err := deserializeVector(buf)
		Expect(err).NotTo(HaveOccurred())
		Expect(vec).To(HaveLen(2))
		Expect(vec[0]).To(BeNumerically("~", 1.0, 0.01))
		Expect(vec[1]).To(BeNumerically("~", 2.0, 0.01))
	})

	It("rejects unknown vector type", func() {
		buf := []byte{99, 0x00}
		_, err := deserializeVector(buf)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("unsupported vector type"))
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

var _ = Describe("HNSW with RaBitQ", func() {
	ctx := context.Background()

	// Helper: create an isolated HNSW graph with RaBitQ enabled.
	makeRaBitQGraph := func(dims, numExBits int) *hnswGraph {
		ss := specSubspace().Sub("hnsw-rabitq")
		config := HNSWConfig{
			NumDimensions:   dims,
			M:               4,
			MMax:            4,
			MMax0:           8,
			EfConstruction:  100,
			Metric:          VectorMetricEuclidean,
			UseRaBitQ:       true,
			RaBitQNumExBits: numExBits,
		}
		storage := newHNSWStorage(ss, config)
		return NewHNSWGraph(storage, config)
	}

	It("insert single node and search returns it", func() {
		graph := makeRaBitQGraph(8, 4)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(1)}
			vec := []float64{1.0, 2.0, 3.0, 4.0, 5.0, 6.0, 7.0, 8.0}
			Expect(graph.Insert(tx, pk, vec)).To(Succeed())

			results, err := graph.Search(tx, vec, 1, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(tupleEqual(results[0].PrimaryKey, pk)).To(BeTrue())
			// Self-distance should be approximately zero (RaBitQ approximation).
			Expect(results[0].Distance).To(BeNumerically("<", 0.5))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("stores RaBitQ-encoded bytes in FDB", func() {
		graph := makeRaBitQGraph(4, 4)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(42)}
			vec := []float64{1.0, 2.0, 3.0, 4.0}
			Expect(graph.Insert(tx, pk, vec)).To(Succeed())

			// Load the node and verify the stored bytes have type ordinal 3 (RABITQ).
			vecBytes, _, err := graph.storage.loadNodeLayer(tx, 0, pk)
			Expect(err).NotTo(HaveOccurred())
			Expect(len(vecBytes)).To(BeNumerically(">", 0))
			Expect(vecBytes[0]).To(Equal(byte(3)), "stored vector should have RABITQ type ordinal")

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert 5 nodes and kNN k=3 returns 3 closest", func() {
		graph := makeRaBitQGraph(4, 6)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 5 points in 4D (well-separated).
			points := [][]float64{
				{0.0, 0.0, 0.0, 0.0},     // id=0
				{1.0, 0.0, 0.0, 0.0},     // id=1
				{2.0, 0.0, 0.0, 0.0},     // id=2
				{100.0, 0.0, 0.0, 0.0},   // id=3 (far)
				{1000.0, 0.0, 0.0, 0.0},  // id=4 (very far)
			}
			for i, p := range points {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, p)).To(Succeed())
			}

			// Query near origin, k=3 should return ids 0,1,2.
			results, err := graph.Search(tx, []float64{0.0, 0.0, 0.0, 0.0}, 3, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{0, 1, 2}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete works with RaBitQ", func() {
		graph := makeRaBitQGraph(4, 4)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 3 nodes.
			for i := int64(0); i < 3; i++ {
				vec := []float64{float64(i), 0.0, 0.0, 0.0}
				Expect(graph.Insert(tx, tuple.Tuple{i}, vec)).To(Succeed())
			}

			// Delete the middle one.
			Expect(graph.Delete(tx, tuple.Tuple{int64(1)}, []float64{1.0, 0.0, 0.0, 0.0})).To(Succeed())

			// Search should return only 2 results.
			results, err := graph.Search(tx, []float64{0.0, 0.0, 0.0, 0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update (re-insert) works with RaBitQ", func() {
		graph := makeRaBitQGraph(4, 4)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			pk := tuple.Tuple{int64(1)}
			vec1 := []float64{1.0, 0.0, 0.0, 0.0}
			Expect(graph.Insert(tx, pk, vec1)).To(Succeed())

			// Re-insert with a different vector (update semantics).
			vec2 := []float64{100.0, 0.0, 0.0, 0.0}
			Expect(graph.Insert(tx, pk, vec2)).To(Succeed())

			// Search near new position should find it close.
			results, err := graph.Search(tx, vec2, 1, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(tupleEqual(results[0].PrimaryKey, pk)).To(BeTrue())
			Expect(results[0].Distance).To(BeNumerically("<", 1.0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("recall is reasonable with 20 random 8D vectors", func() {
		dims := 8
		numVectors := 20
		k := 5
		numExBits := 6
		graph := makeRaBitQGraph(dims, numExBits)

		rng := rand.New(rand.NewSource(42))
		vectors := make([][]float64, numVectors)
		for i := 0; i < numVectors; i++ {
			vectors[i] = make([]float64, dims)
			for j := 0; j < dims; j++ {
				vectors[i][j] = rng.NormFloat64() * 10
			}
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			for i, v := range vectors {
				Expect(graph.Insert(tx, tuple.Tuple{int64(i)}, v)).To(Succeed())
			}

			// Query with vector[0], find k nearest.
			query := vectors[0]
			results, err := graph.Search(tx, query, k, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(k))

			// Compute brute-force k nearest.
			type idDist struct {
				id   int64
				dist float64
			}
			var all []idDist
			for i, v := range vectors {
				d := euclideanDistance(query, v)
				all = append(all, idDist{id: int64(i), dist: d})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].dist < all[j].dist })
			trueKNN := make(map[int64]bool)
			for i := 0; i < k; i++ {
				trueKNN[all[i].id] = true
			}

			// Count recall.
			hits := 0
			for _, r := range results {
				if trueKNN[r.PrimaryKey[0].(int64)] {
					hits++
				}
			}
			recall := float64(hits) / float64(k)
			// With RaBitQ, approximate distances may reorder slightly.
			// Require at least 60% recall (lenient due to approximation + small dataset).
			Expect(recall).To(BeNumerically(">=", 0.6),
				"RaBitQ recall should be >= 60%% (got %.0f%%)", recall*100)

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("decodeStoredVector reconstructs approximate vector from RaBitQ bytes", func() {
		dims := 8
		numExBits := 7
		graph := makeRaBitQGraph(dims, numExBits)

		original := []float64{1.0, -2.0, 3.0, -4.0, 5.0, -6.0, 7.0, -8.0}
		encoded := graph.encodeVectorBytes(original)
		Expect(encoded[0]).To(Equal(byte(3))) // RABITQ

		decoded, err := graph.decodeStoredVector(encoded)
		Expect(err).NotTo(HaveOccurred())
		Expect(decoded).To(HaveLen(dims))

		// Decoded should approximate the original direction.
		// Compute cosine similarity.
		origNorm := normalizeVector(original)
		decNorm := normalizeVector(decoded)
		cosSim := dot(origNorm, decNorm)
		Expect(cosSim).To(BeNumerically(">", 0.9),
			"decoded vector should approximate original direction (cosine=%.3f)", cosSim)
	})

	It("parseHNSWConfig reads RaBitQ options", func() {
		idx := &Index{
			Name: "test_vec",
			Type: IndexTypeVector,
			Options: map[string]string{
				"hnswNumDimensions":   "32",
				"hnswUseRaBitQ":      "true",
				"hnswRaBitQNumExBits": "6",
			},
		}
		config := parseHNSWConfig(idx)
		Expect(config.UseRaBitQ).To(BeTrue())
		Expect(config.RaBitQNumExBits).To(Equal(6))
		Expect(config.NumDimensions).To(Equal(32))
	})

	It("parseHNSWConfig defaults when RaBitQ options absent", func() {
		idx := &Index{
			Name:    "test_vec",
			Type:    IndexTypeVector,
			Options: map[string]string{},
		}
		config := parseHNSWConfig(idx)
		Expect(config.UseRaBitQ).To(BeFalse())
		Expect(config.RaBitQNumExBits).To(Equal(0))
	})

	It("parseHNSWConfig ignores invalid numExBits", func() {
		idx := &Index{
			Name: "test_vec",
			Type: IndexTypeVector,
			Options: map[string]string{
				"hnswUseRaBitQ":      "true",
				"hnswRaBitQNumExBits": "99",
			},
		}
		config := parseHNSWConfig(idx)
		Expect(config.UseRaBitQ).To(BeTrue())
		Expect(config.RaBitQNumExBits).To(Equal(0)) // out of range, not set
	})

	It("computeDistance handles both raw and RaBitQ vectors", func() {
		dims := 4
		graph := makeRaBitQGraph(dims, 4)

		query := []float64{1.0, 2.0, 3.0, 4.0}

		// Raw DOUBLE vector.
		rawBytes := serializeVector([]float64{2.0, 3.0, 4.0, 5.0})
		distRaw := graph.computeDistance(query, rawBytes)
		expected := euclideanDistance(query, []float64{2.0, 3.0, 4.0, 5.0})
		Expect(distRaw).To(BeNumerically("~", expected, 1e-9))

		// RaBitQ encoded vector.
		q := NewRaBitQuantizer(VectorMetricEuclidean, 4)
		encoded := q.Encode([]float64{2.0, 3.0, 4.0, 5.0})
		rabitqBytes := encoded.ToBytes()
		distRaBitQ := graph.computeDistance(query, rabitqBytes)
		// Should be finite and reasonably close to exact.
		Expect(math.IsInf(distRaBitQ, 0)).To(BeFalse())
		Expect(math.IsNaN(distRaBitQ)).To(BeFalse())
		Expect(distRaBitQ).To(BeNumerically("~", expected, expected*0.5))
	})

	It("empty graph returns nil results", func() {
		graph := makeRaBitQGraph(4, 4)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()
			results, err := graph.Search(tx, []float64{1.0, 2.0, 3.0, 4.0}, 5, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeNil())
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

	It("ScanIndex rejects VECTOR index with error", func() {
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

			// ScanIndex should reject VECTOR indexes, matching Java's behavior.
			cursor := store.ScanIndex(vecIdx, TupleRangeAll, nil, ForwardScan())
			_, err = cursor.OnNext(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("BY_DISTANCE"))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanVectorIndex returns kNN results as cursor", func() {
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

			// Insert 4 orders as 2D points: (price, quantity).
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

			// ScanVectorIndex near (15, 15), k=2.
			cursor := store.ScanVectorIndex(vecIdx, []float64{15.0, 15.0}, 2, 100, nil, ForwardScan())
			var entries []*IndexEntry
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				entries = append(entries, result.GetValue())
			}

			Expect(entries).To(HaveLen(2))

			// Both results should be the two closest points (ids 1 and 2).
			gotIDs := make([]int64, len(entries))
			for i, e := range entries {
				gotIDs[i] = e.Key[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			// Value contains distance.
			for _, e := range entries {
				Expect(e.Value).To(HaveLen(1))
				dist, ok := e.Value[0].(float64)
				Expect(ok).To(BeTrue())
				// dist to (15,15) from (10,10) or (20,20) = 50
				Expect(dist).To(BeNumerically("~", 50.0, 1e-6))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanIndexByType BY_DISTANCE returns kNN results", func() {
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

			// Use ScanIndexByType with VectorDistanceScanRange helper.
			scanRange := VectorDistanceScanRange([]float64{15.0, 15.0}, 2, 100)
			cursor := store.ScanIndexByType(vecIdx, IndexScanByDistance, scanRange, nil, ForwardScan())

			var entries []*IndexEntry
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				entries = append(entries, result.GetValue())
			}

			Expect(entries).To(HaveLen(2))

			gotIDs := make([]int64, len(entries))
			for i, e := range entries {
				gotIDs[i] = e.Key[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanVectorIndex on empty index returns no results", func() {
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

			cursor := store.ScanVectorIndex(vecIdx, []float64{1.0, 2.0}, 5, 100, nil, ForwardScan())
			result, err := cursor.OnNext(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(result.HasNext()).To(BeFalse())
			Expect(result.GetNoNextReason()).To(Equal(SourceExhausted))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanVectorIndex returns all results when k > count", func() {
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

			// Insert 2 records.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(20)})
			Expect(err).NotTo(HaveOccurred())

			// Request k=100 but only 2 exist.
			cursor := store.ScanVectorIndex(vecIdx, []float64{0.0, 0.0}, 100, 200, nil, ForwardScan())
			var entries []*IndexEntry
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				entries = append(entries, result.GetValue())
			}
			Expect(entries).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanVectorIndex results are sorted by distance ascending", func() {
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

			// Insert points at varying distances from origin.
			for _, o := range []struct {
				id       int64
				price    int32
				quantity int32
			}{
				{1, 10, 10},   // dist^2 = 200
				{2, 1, 1},     // dist^2 = 2
				{3, 50, 50},   // dist^2 = 5000
				{4, 5, 5},     // dist^2 = 50
				{5, 100, 100}, // dist^2 = 20000
			} {
				_, err = store.SaveRecord(&gen.Order{
					OrderId:  proto.Int64(o.id),
					Price:    proto.Int32(o.price),
					Quantity: proto.Int32(o.quantity),
				})
				Expect(err).NotTo(HaveOccurred())
			}

			cursor := store.ScanVectorIndex(vecIdx, []float64{0.0, 0.0}, 5, 200, nil, ForwardScan())
			var distances []float64
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				dist := result.GetValue().Value[0].(float64)
				distances = append(distances, dist)
			}

			Expect(distances).To(HaveLen(5))
			for i := 1; i < len(distances); i++ {
				Expect(distances[i]).To(BeNumerically(">=", distances[i-1]),
					"distance at position %d should be >= position %d", i, i-1)
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanIndexByType BY_DISTANCE on non-VECTOR index returns error", func() {
		ks := specSubspace()

		valueIdx := NewIndex("Order$price", Field("price"))
		builder := baseMetaData()
		builder.AddIndex("Order", valueIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			scanRange := VectorDistanceScanRange([]float64{1.0}, 1, 100)
			cursor := store.ScanIndexByType(valueIdx, IndexScanByDistance, scanRange, nil, ForwardScan())
			_, err = cursor.OnNext(ctx)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("does not support BY_DISTANCE"))

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

var _ = Describe("VectorIndex Prefix Partitioning", func() {
	ctx := context.Background()

	baseMetaData := func() *RecordMetaDataBuilder {
		builder := NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
		builder.GetRecordType("Order").SetPrimaryKey(Field("order_id"))
		builder.GetRecordType("Customer").SetPrimaryKey(Field("customer_id"))
		builder.GetRecordType("TypedRecord").SetPrimaryKey(Field("id"))
		return builder
	}

	It("grouped VECTOR index stores per-prefix HNSW graphs", func() {
		ks := specSubspace()

		// Index: KWV(Concat(Field("quantity"), Field("price")), 1)
		// quantity is the prefix (group key), price is the vector (1D).
		vecIdx := NewVectorIndex("vec_grouped",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1 (quantity=1): prices 10, 20, 100
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(100), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Group 2 (quantity=2): prices 50, 60
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(50), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(5), Price: proto.Int32(60), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// Search in group 1 near price=15: should find id=1(10) and id=2(20), not group 2 records.
			results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)}, []float64{15.0}, 2, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			// Search in group 2 near price=55: should find id=4(50) and id=5(60) only.
			results2, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(2)}, []float64{55.0}, 2, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(2))

			gotIDs2 := make([]int64, len(results2))
			for i, r := range results2 {
				gotIDs2[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs2, func(i, j int) bool { return gotIDs2[i] < gotIDs2[j] })
			Expect(gotIDs2).To(Equal([]int64{4, 5}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("search in empty prefix returns no results", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_empty",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert into group 1 only.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Search in group 99 (empty): should return 0 results.
			results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(99)}, []float64{10.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(0))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete record removes from correct prefix graph", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_del",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1: id=1 and id=2.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Group 2: id=3.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// Delete id=1 from group 1.
			existed, err := store.DeleteRecord(tuple.Tuple{int64(1)})
			Expect(err).NotTo(HaveOccurred())
			Expect(existed).To(BeTrue())

			// Group 1 should only have id=2 now.
			results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)}, []float64{0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(2)))

			// Group 2 should still have id=3 (unaffected by delete in group 1).
			results2, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(2)}, []float64{0.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(1))
			Expect(results2[0].PrimaryKey[0].(int64)).To(Equal(int64(3)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanVectorIndexWithPrefix returns cursor results scoped to prefix", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_scan",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1: id=1(price=10), id=2(price=20).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Group 2: id=3(price=50).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// ScanVectorIndexWithPrefix for group 1.
			cursor := store.ScanVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)},
				[]float64{15.0}, 10, 100, nil, ForwardScan())
			var entries []*IndexEntry
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				entries = append(entries, result.GetValue())
			}

			// Should only contain group 1 records.
			Expect(entries).To(HaveLen(2))
			gotIDs := make([]int64, len(entries))
			for i, e := range entries {
				gotIDs[i] = e.Key[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("ScanIndexByType BY_DISTANCE with prefix via VectorDistanceScanRangeWithPrefix", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_scantype",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1: id=1(price=10), id=2(price=20).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Group 2: id=3(price=50), id=4(price=60).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(3), Price: proto.Int32(50), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(4), Price: proto.Int32(60), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// Scan group 2 via ScanIndexByType + VectorDistanceScanRangeWithPrefix.
			scanRange := VectorDistanceScanRangeWithPrefix([]float64{55.0}, 2, 100, tuple.Tuple{int64(2)})
			cursor := store.ScanIndexByType(vecIdx, IndexScanByDistance, scanRange, nil, ForwardScan())

			var entries []*IndexEntry
			for {
				result, err := cursor.OnNext(ctx)
				Expect(err).NotTo(HaveOccurred())
				if !result.HasNext() {
					break
				}
				entries = append(entries, result.GetValue())
			}

			// Should only contain group 2 records.
			Expect(entries).To(HaveLen(2))
			gotIDs := make([]int64, len(entries))
			for i, e := range entries {
				gotIDs[i] = e.Key[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{3, 4}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("update record in grouped index moves between prefix graphs", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_update",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert id=1 in group 1.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Verify it's in group 1.
			results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)}, []float64{10.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(1)))

			// Update id=1 to group 2.
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// Group 1 should now be empty.
			results1, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)}, []float64{10.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results1).To(HaveLen(0))

			// Group 2 should now have id=1.
			results2, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(2)}, []float64{10.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(1))
			Expect(results2[0].PrimaryKey[0].(int64)).To(Equal(int64(1)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("non-KWV index (no prefix) still works via backward-compatible APIs", func() {
		ks := specSubspace()

		// Non-grouped vector index: Concat(Field("price"), Field("quantity")) as 2D vector.
		vecIdx := NewVectorIndex("vec_noprefix", Concat(Field("price"), Field("quantity")), 2)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(10)})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(20), Quantity: proto.Int32(20)})
			Expect(err).NotTo(HaveOccurred())

			// Backward-compatible API (no prefix) should work.
			results, err := store.SearchVectorIndex(vecIdx, []float64{15.0, 15.0}, 2, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("SearchVectorIndexRecordsWithPrefix fetches records from correct prefix", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_recs",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Group 1: id=1(price=10).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(1), Price: proto.Int32(10), Quantity: proto.Int32(1)})
			Expect(err).NotTo(HaveOccurred())

			// Group 2: id=2(price=50).
			_, err = store.SaveRecord(&gen.Order{OrderId: proto.Int64(2), Price: proto.Int32(50), Quantity: proto.Int32(2)})
			Expect(err).NotTo(HaveOccurred())

			// Fetch records from group 1.
			records, err := store.SearchVectorIndexRecordsWithPrefix(ctx, vecIdx, tuple.Tuple{int64(1)}, []float64{10.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(records).To(HaveLen(1))
			order := records[0].Record.Record.(*gen.Order)
			Expect(order.GetOrderId()).To(Equal(int64(1)))
			Expect(order.GetQuantity()).To(Equal(int32(1)))

			// Fetch records from group 2.
			records2, err := store.SearchVectorIndexRecordsWithPrefix(ctx, vecIdx, tuple.Tuple{int64(2)}, []float64{50.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(records2).To(HaveLen(1))
			order2 := records2[0].Record.Record.(*gen.Order)
			Expect(order2.GetOrderId()).To(Equal(int64(2)))
			Expect(order2.GetQuantity()).To(Equal(int32(2)))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("grouped 2D vector index with bytes vector_data field", func() {
		ks := specSubspace()

		// Index: KWV(Concat(Field("quantity"), Field("vector_data")), 1)
		// quantity is the prefix, vector_data (bytes) is the vector.
		vecIdx := NewVectorIndex("vec_grouped_bytes",
			KeyWithValue(Concat(Field("quantity"), Field("vector_data")), 1), 3)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			mkVec := func(vals ...float64) []byte {
				return serializeVector(vals)
			}

			// Group 1: two 3D vectors.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(1), Quantity: proto.Int32(1),
				VectorData: mkVec(1.0, 2.0, 3.0),
			})
			Expect(err).NotTo(HaveOccurred())
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(2), Quantity: proto.Int32(1),
				VectorData: mkVec(4.0, 5.0, 6.0),
			})
			Expect(err).NotTo(HaveOccurred())

			// Group 2: one 3D vector.
			_, err = store.SaveRecord(&gen.Order{
				OrderId: proto.Int64(3), Quantity: proto.Int32(2),
				VectorData: mkVec(100.0, 100.0, 100.0),
			})
			Expect(err).NotTo(HaveOccurred())

			// Search group 1 near (2,3,4): should find id=1 and id=2, not id=3.
			results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(1)}, []float64{2.0, 3.0, 4.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				gotIDs[i] = r.PrimaryKey[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 2}))

			// Search group 2: should only find id=3.
			results2, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{int64(2)}, []float64{100.0, 100.0, 100.0}, 10, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(results2).To(HaveLen(1))
			Expect(results2[0].PrimaryKey[0].(int64)).To(Equal(int64(3)))
			Expect(results2[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("multiple prefix values do not leak between partitions", func() {
		ks := specSubspace()

		vecIdx := NewVectorIndex("vec_grouped_isolation",
			KeyWithValue(Concat(Field("quantity"), Field("price")), 1), 1)
		builder := baseMetaData()
		builder.AddIndex("Order", vecIdx)
		md, err := builder.Build()
		Expect(err).NotTo(HaveOccurred())

		_, err = sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			store, err := NewStoreBuilder().
				SetContext(rtx).SetMetaDataProvider(md).SetSubspace(ks).CreateOrOpen()
			Expect(err).NotTo(HaveOccurred())

			// Insert into 5 different groups with 2 records each.
			for g := int32(1); g <= 5; g++ {
				for j := int32(0); j < 2; j++ {
					id := int64(g)*100 + int64(j)
					_, err = store.SaveRecord(&gen.Order{
						OrderId:  proto.Int64(id),
						Price:    proto.Int32(g*10 + j),
						Quantity: proto.Int32(g),
					})
					Expect(err).NotTo(HaveOccurred())
				}
			}

			// Search each group: should find exactly 2 records from that group.
			for g := int64(1); g <= 5; g++ {
				results, err := store.SearchVectorIndexWithPrefix(vecIdx, tuple.Tuple{g}, []float64{0.0}, 10, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(2), "group %d should have exactly 2 results", g)

				for _, r := range results {
					// Each result's PK should start with the group's hundred (e.g., group 1 = 100, 101).
					pk := r.PrimaryKey[0].(int64)
					Expect(pk / 100).To(Equal(g), "result PK %d should belong to group %d", pk, g)
				}
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})
})

var _ = Describe("HNSW Extended Neighbor Selection", func() {
	ctx := context.Background()

	It("selectNeighbors heuristic prefers diverse directions", func() {
		// Unit test: 5 candidates, maxConn=2.
		// Candidate A is closest, candidate B is close but near A,
		// candidate C is farther but in a different direction.
		// The heuristic should pick A and C (diverse), not A and B (clustered).
		config := HNSWConfig{
			NumDimensions:  2,
			M:              4,
			MMax:           4,
			MMax0:          8,
			EfConstruction: 100,
			Metric:         VectorMetricEuclidean, // satisfies triangle inequality
		}
		ss := specSubspace().Sub("hnsw-heuristic-unit")
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		// Query at origin (0,0). Distances are squared Euclidean.
		// A = (1, 0)  -> dist = 1
		// B = (1.1, 0) -> dist = 1.21 (very close to A)
		// C = (0, 2)  -> dist = 4    (different direction from A)
		// D = (0, 2.1) -> dist = 4.41 (close to C)
		// E = (3, 3)  -> dist = 18   (far away)
		candidates := []hnswCandidate{
			{pk: tuple.Tuple{int64(1)}, vec: []float64{1, 0}, dist: 1.0},
			{pk: tuple.Tuple{int64(2)}, vec: []float64{1.1, 0}, dist: 1.21},
			{pk: tuple.Tuple{int64(3)}, vec: []float64{0, 2}, dist: 4.0},
			{pk: tuple.Tuple{int64(4)}, vec: []float64{0, 2.1}, dist: 4.41},
			{pk: tuple.Tuple{int64(5)}, vec: []float64{3, 3}, dist: 18.0},
		}

		selected := graph.selectNeighbors(candidates, 2)
		Expect(selected).To(HaveLen(2))

		// First should be A (closest).
		Expect(selected[0].pk[0].(int64)).To(Equal(int64(1)))
		// Second should be C (diverse direction), not B (clustered with A).
		// dist(B, A) = (0.1)^2 = 0.01 < B.dist=1.21 -> B is pruned
		// dist(C, A) = 1 + 4 = 5 > C.dist=4 -> C is selected
		Expect(selected[1].pk[0].(int64)).To(Equal(int64(3)))
	})

	It("keepPrunedConnections fills up to maxConn", func() {
		config := HNSWConfig{
			NumDimensions:         2,
			M:                     4,
			MMax:                  4,
			MMax0:                 8,
			EfConstruction:        100,
			Metric:                VectorMetricEuclidean,
			KeepPrunedConnections: true,
		}
		ss := specSubspace().Sub("hnsw-keep-pruned-unit")
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		// Query at origin. All candidates are on the X axis (same direction).
		// Heuristic will pick only the closest, prune the rest.
		// With keepPrunedConnections, pruned ones fill up to maxConn.
		candidates := []hnswCandidate{
			{pk: tuple.Tuple{int64(1)}, vec: []float64{1, 0}, dist: 1.0},
			{pk: tuple.Tuple{int64(2)}, vec: []float64{2, 0}, dist: 4.0},
			{pk: tuple.Tuple{int64(3)}, vec: []float64{3, 0}, dist: 9.0},
			{pk: tuple.Tuple{int64(4)}, vec: []float64{4, 0}, dist: 16.0},
			{pk: tuple.Tuple{int64(5)}, vec: []float64{5, 0}, dist: 25.0},
		}

		// maxConn=3: heuristic selects only id=1 (closest), then prunes 2,3,4,5.
		// keepPrunedConnections adds back 2, 3 to fill up to 3.
		selected := graph.selectNeighbors(candidates, 3)
		Expect(selected).To(HaveLen(3))
		Expect(selected[0].pk[0].(int64)).To(Equal(int64(1)))
		Expect(selected[1].pk[0].(int64)).To(Equal(int64(2)))
		Expect(selected[2].pk[0].(int64)).To(Equal(int64(3)))
	})

	It("heuristic is skipped for cosine metric (no triangle inequality)", func() {
		config := HNSWConfig{
			NumDimensions:  2,
			M:              4,
			MMax:           4,
			MMax0:          8,
			EfConstruction: 100,
			Metric:         VectorMetricCosine, // does NOT satisfy triangle inequality
		}
		ss := specSubspace().Sub("hnsw-cosine-no-heuristic")
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		// With cosine metric, selectNeighbors should do simple sort-and-truncate,
		// NOT the diversity heuristic.
		candidates := []hnswCandidate{
			{pk: tuple.Tuple{int64(1)}, vec: []float64{1, 0}, dist: 0.1},
			{pk: tuple.Tuple{int64(2)}, vec: []float64{1.1, 0}, dist: 0.2},
			{pk: tuple.Tuple{int64(3)}, vec: []float64{0, 2}, dist: 0.5},
		}

		selected := graph.selectNeighbors(candidates, 2)
		Expect(selected).To(HaveLen(2))
		// Simple sort: takes the two closest by dist.
		Expect(selected[0].pk[0].(int64)).To(Equal(int64(1)))
		Expect(selected[1].pk[0].(int64)).To(Equal(int64(2)))
	})

	It("extendCandidates explores 2nd-degree neighbors during insert", func() {
		ss := specSubspace().Sub("hnsw-extend-insert")
		config := HNSWConfig{
			NumDimensions:    8,
			M:                16,
			MMax:             16,
			MMax0:            32,
			EfConstruction:   200,
			Metric:           VectorMetricEuclidean,
			ExtendCandidates: true,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 50
		const dims = 8
		const k = 5

		rng := rand.New(rand.NewSource(99))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.Float64()*200.0 - 100.0
			}
			vectors[i] = vec
		}
		queryVec := make([]float64, dims)
		for d := range dims {
			queryVec[d] = rng.Float64()*200.0 - 100.0
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			results, err := graph.Search(tx, queryVec, k, 200)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(k))

			// Results must be sorted by distance.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("keepPrunedConnections maintains graph connectivity after inserts", func() {
		ss := specSubspace().Sub("hnsw-keep-pruned-insert")
		config := HNSWConfig{
			NumDimensions:         8,
			M:                     4,
			MMax:                  4,
			MMax0:                 8,
			EfConstruction:        100,
			Metric:                VectorMetricEuclidean,
			KeepPrunedConnections: true,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 30
		const dims = 8
		const k = 5

		rng := rand.New(rand.NewSource(2024))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.Float64()*200.0 - 100.0
			}
			vectors[i] = vec
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Every inserted vector should be findable by searching for itself.
			for i, vec := range vectors {
				results, err := graph.Search(tx, vec, 1, 200)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1), "vector %d should be findable", i)
				Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(i)),
					"vector %d should find itself as nearest neighbor", i)
				Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("both extendCandidates and keepPrunedConnections together", func() {
		ss := specSubspace().Sub("hnsw-both-options")
		config := HNSWConfig{
			NumDimensions:         8,
			M:                     8,
			MMax:                  8,
			MMax0:                 16,
			EfConstruction:        100,
			Metric:                VectorMetricEuclidean,
			ExtendCandidates:      true,
			KeepPrunedConnections: true,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 60
		const dims = 8
		const k = 10

		rng := rand.New(rand.NewSource(31337))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.Float64()*200.0 - 100.0
			}
			vectors[i] = vec
		}
		queryVec := make([]float64, dims)
		for d := range dims {
			queryVec[d] = rng.Float64()*200.0 - 100.0
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Search with both options.
			results, err := graph.Search(tx, queryVec, k, 200)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(k))

			// Verify sorted order.
			for i := 1; i < len(results); i++ {
				Expect(results[i].Distance).To(BeNumerically(">=", results[i-1].Distance))
			}

			// Brute-force recall check.
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
			GinkgoWriter.Printf("HNSW (extend+keepPruned) recall@%d: %.2f\n", k, recall)
			Expect(recall).To(BeNumerically(">=", 0.7))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("delete works correctly with heuristic neighbor selection", func() {
		ss := specSubspace().Sub("hnsw-heuristic-delete")
		config := HNSWConfig{
			NumDimensions:         4,
			M:                     4,
			MMax:                  4,
			MMax0:                 8,
			EfConstruction:        100,
			Metric:                VectorMetricEuclidean,
			ExtendCandidates:      true,
			KeepPrunedConnections: true,
		}
		storage := newHNSWStorage(ss, config)
		graph := NewHNSWGraph(storage, config)

		const numVectors = 20
		const dims = 4

		rng := rand.New(rand.NewSource(555))
		vectors := make([][]float64, numVectors)
		for i := range numVectors {
			vec := make([]float64, dims)
			for d := range dims {
				vec[d] = rng.Float64()*100.0 - 50.0
			}
			vectors[i] = vec
		}

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert all vectors.
			for i, vec := range vectors {
				pk := tuple.Tuple{int64(i)}
				Expect(graph.Insert(tx, pk, vec)).To(Succeed())
			}

			// Delete first 5 vectors.
			for i := 0; i < 5; i++ {
				Expect(graph.Delete(tx, tuple.Tuple{int64(i)}, vectors[i])).To(Succeed())
			}

			// Remaining 15 vectors should all be findable.
			for i := 5; i < numVectors; i++ {
				results, err := graph.Search(tx, vectors[i], 1, 100)
				Expect(err).NotTo(HaveOccurred())
				Expect(results).To(HaveLen(1), "vector %d should be findable after deletes", i)
				Expect(results[0].PrimaryKey[0].(int64)).To(Equal(int64(i)))
			}

			// Deleted vectors should not appear in search results.
			for i := 0; i < 5; i++ {
				results, err := graph.Search(tx, vectors[i], numVectors, 200)
				Expect(err).NotTo(HaveOccurred())
				for _, r := range results {
					Expect(r.PrimaryKey[0].(int64)).To(BeNumerically(">=", int64(5)),
						"deleted vector %d should not appear in results", i)
				}
			}

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("parseHNSWConfig reads extendCandidates and keepPrunedConnections options", func() {
		idx := &Index{
			Name: "test_vec",
			Options: map[string]string{
				IndexOptionVectorNumDimensions:         "64",
				IndexOptionVectorExtendCandidates:      "true",
				IndexOptionVectorKeepPrunedConnections: "true",
			},
		}
		config := parseHNSWConfig(idx)
		Expect(config.NumDimensions).To(Equal(64))
		Expect(config.ExtendCandidates).To(BeTrue())
		Expect(config.KeepPrunedConnections).To(BeTrue())

		// Default (false) when not set.
		idx2 := &Index{
			Name: "test_vec2",
			Options: map[string]string{
				IndexOptionVectorNumDimensions: "32",
			},
		}
		config2 := parseHNSWConfig(idx2)
		Expect(config2.ExtendCandidates).To(BeFalse())
		Expect(config2.KeepPrunedConnections).To(BeFalse())

		// Explicit "false" value.
		idx3 := &Index{
			Name: "test_vec3",
			Options: map[string]string{
				IndexOptionVectorExtendCandidates:      "false",
				IndexOptionVectorKeepPrunedConnections: "false",
			},
		}
		config3 := parseHNSWConfig(idx3)
		Expect(config3.ExtendCandidates).To(BeFalse())
		Expect(config3.KeepPrunedConnections).To(BeFalse())
	})

	It("satisfiesTriangleInequality returns correct values per metric", func() {
		Expect(VectorMetricEuclidean.satisfiesTriangleInequality()).To(BeTrue())
		Expect(VectorMetricCosine.satisfiesTriangleInequality()).To(BeFalse())
		Expect(VectorMetricInnerProduct.satisfiesTriangleInequality()).To(BeFalse())
	})
})
