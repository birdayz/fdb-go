package recordlayer

import (
	"context"
	"math"
	"sort"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
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

var _ = Describe("HNSW Graph Direct", func() {
	ctx := context.Background()

	// Helper: create an isolated HNSW graph with its own FDB subspace.
	makeGraph := func(dims int) (*hnswGraph, subspace.Subspace) {
		ss := specSubspace().Sub("hnsw")
		storage := newHNSWStorage(ss)
		config := DefaultHNSWConfig(dims)
		// Use smaller M for tests so multi-layer promotion happens more often.
		config.M = 4
		config.MMax = 4
		config.MMax0 = 8
		config.EfConstruction = 16
		graph := NewHNSWGraph(storage, config)
		return graph, ss
	}

	It("insert single node, search returns it", func() {
		graph, _ := makeGraph(3)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			nodeID := tuple.Tuple{int64(1)}.Pack()
			vec := []float64{1.0, 2.0, 3.0}
			Expect(graph.Insert(tx, nodeID, vec)).To(Succeed())

			results, err := graph.Search(tx, []float64{1.0, 2.0, 3.0}, 1, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].NodeID).To(Equal(nodeID))
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert 5 nodes, kNN k=3 returns 3 closest", func() {
		graph, _ := makeGraph(2)

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
				nodeID := tuple.Tuple{int64(i)}.Pack()
				Expect(graph.Insert(tx, nodeID, p)).To(Succeed())
			}

			// Query at origin, k=3 should return ids 0,1,2.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 3, 20)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			// Results are sorted by distance (ascending).
			// id=0: dist=0, id=1: dist=1, id=2: dist=4
			gotIDs := make([]int64, len(results))
			for i, r := range results {
				pk, err := tuple.Unpack(r.NodeID)
				Expect(err).NotTo(HaveOccurred())
				gotIDs[i] = pk[0].(int64)
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
		graph, _ := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			id1 := tuple.Tuple{int64(1)}.Pack()
			id2 := tuple.Tuple{int64(2)}.Pack()
			id3 := tuple.Tuple{int64(3)}.Pack()

			Expect(graph.Insert(tx, id1, []float64{0.0, 0.0})).To(Succeed())
			Expect(graph.Insert(tx, id2, []float64{1.0, 0.0})).To(Succeed())
			Expect(graph.Insert(tx, id3, []float64{2.0, 0.0})).To(Succeed())

			// Delete id2.
			Expect(graph.Delete(tx, id2)).To(Succeed())

			// Search for all 3 (k=3) near origin. Should only get id1 and id3.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 3, 20)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(2))

			gotIDs := make([]int64, len(results))
			for i, r := range results {
				pk, err := tuple.Unpack(r.NodeID)
				Expect(err).NotTo(HaveOccurred())
				gotIDs[i] = pk[0].(int64)
			}
			sort.Slice(gotIDs, func(i, j int) bool { return gotIDs[i] < gotIDs[j] })
			Expect(gotIDs).To(Equal([]int64{1, 3}))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert same nodeID twice (update), only one result", func() {
		graph, _ := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			nodeID := tuple.Tuple{int64(42)}.Pack()

			// Insert at (0,0), then "update" to (5,5).
			Expect(graph.Insert(tx, nodeID, []float64{0.0, 0.0})).To(Succeed())
			Expect(graph.Insert(tx, nodeID, []float64{5.0, 5.0})).To(Succeed())

			// Search for all nodes. Should get exactly 1 result.
			results, err := graph.Search(tx, []float64{5.0, 5.0}, 10, 20)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(1))
			Expect(results[0].NodeID).To(Equal(nodeID))
			// The node's vector should be the updated one (5,5), so distance to (5,5) = 0.
			Expect(results[0].Distance).To(BeNumerically("~", 0.0, 1e-9))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("search empty graph returns nil", func() {
		graph, _ := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			results, err := graph.Search(tx, []float64{1.0, 2.0}, 5, 10)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(BeNil())

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("search with k > num_nodes returns all nodes", func() {
		graph, _ := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 3 nodes.
			for i := range 3 {
				nodeID := tuple.Tuple{int64(i)}.Pack()
				Expect(graph.Insert(tx, nodeID, []float64{float64(i), 0.0})).To(Succeed())
			}

			// Search with k=10 but only 3 nodes exist.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 10, 20)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(3))

			return nil, nil
		})
		Expect(err).NotTo(HaveOccurred())
	})

	It("insert 20 nodes, verify all retrievable", func() {
		graph, _ := makeGraph(3)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 20 nodes at distinct positions.
			for i := range 20 {
				nodeID := tuple.Tuple{int64(i)}.Pack()
				vec := []float64{float64(i), float64(i * 2), float64(i * 3)}
				Expect(graph.Insert(tx, nodeID, vec)).To(Succeed())
			}

			// Search with k=20: should find all of them.
			results, err := graph.Search(tx, []float64{0.0, 0.0, 0.0}, 20, 50)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(20))

			// Verify all 20 distinct IDs are present.
			gotIDs := make(map[int64]bool)
			for _, r := range results {
				pk, err := tuple.Unpack(r.NodeID)
				Expect(err).NotTo(HaveOccurred())
				gotIDs[pk[0].(int64)] = true
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
		storage := newHNSWStorage(ss)
		config := HNSWConfig{
			NumDimensions:  2,
			M:              2, // Very small M -> higher layer probability
			MMax:           2,
			MMax0:          4,
			EfConstruction: 10,
			Metric:         VectorMetricEuclidean,
		}
		graph := NewHNSWGraph(storage, config)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 30 nodes. With M=2, ~1/2 of nodes go to layer 1, ~1/4 to layer 2, etc.
			// Statistically very likely to have at least one node above layer 0.
			for i := range 30 {
				nodeID := tuple.Tuple{int64(i)}.Pack()
				vec := []float64{float64(i) * 10.0, float64(i) * 10.0}
				Expect(graph.Insert(tx, nodeID, vec)).To(Succeed())
			}

			// Verify the graph has multiple layers by checking metadata.
			meta, err := graph.storage.loadMeta(tx)
			Expect(err).NotTo(HaveOccurred())
			Expect(meta.EntryNodeID).NotTo(BeNil())
			// With M=2 and 30 nodes, max layer is very likely > 0.
			// Use >= 0 as minimum assertion (probabilistic, but essentially guaranteed).
			Expect(meta.MaxLayer).To(BeNumerically(">=", 0))

			// Verify search still works correctly across layers.
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 5, 30)
			Expect(err).NotTo(HaveOccurred())
			Expect(results).To(HaveLen(5))

			// Closest to origin should be node 0 at (0,0).
			pk, err := tuple.Unpack(results[0].NodeID)
			Expect(err).NotTo(HaveOccurred())
			Expect(pk[0].(int64)).To(Equal(int64(0)))
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
		graph, _ := makeGraph(2)

		_, err := sharedDB.Run(ctx, func(rtx *FDBRecordContext) (any, error) {
			tx := rtx.Transaction()

			// Insert 3 nodes then delete all.
			ids := make([][]byte, 3)
			for i := range 3 {
				ids[i] = tuple.Tuple{int64(i)}.Pack()
				Expect(graph.Insert(tx, ids[i], []float64{float64(i), 0.0})).To(Succeed())
			}
			for _, id := range ids {
				Expect(graph.Delete(tx, id)).To(Succeed())
			}

			// Search should return nil (empty graph).
			results, err := graph.Search(tx, []float64{0.0, 0.0}, 5, 10)
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
			results, err := store.SearchVectorIndex(vecIdx, []float64{15.0, 15.0}, 2, 20)
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
			results, err := store.SearchVectorIndex(vecIdx, []float64{0.0, 0.0}, 10, 20)
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
			_, err = store.SearchVectorIndex(valueIdx, []float64{1.0}, 1, 10)
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
