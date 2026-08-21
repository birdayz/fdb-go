//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
)

// Record-store keyspace 10 is the sliding-window index's, and its layout is the
// wire contract these specs exist to prove. Repeated here as literals rather
// than imported from the record layer: importing the constants under test would
// make every assertion below a statement about Go agreeing with itself, which is
// exactly what a cross-engine test must not be.
const (
	swKeyspacePrefix   = 10
	swEntriesSubspace  = 0
	swMetaSubspace     = 1
	swCountMetaKey     = 3
	swBoundaryMetaKey  = 4
	swConformanceIndex = "order_sw_vector"
)

var _ = Describe("SLIDING WINDOW Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *SlidingWindowConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("sw_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewSlidingWindowConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	// The window is ASC/MIN with size 2 on `price`, unpartitioned.
	//
	// Prices 10, 20, 30, 40 for orders 1..4: the window elects 1 and 2, and
	// 3 and 4 are tracked in the entry list as overflow — present on disk,
	// absent from the HNSW graph. That distinction is the whole design, and it
	// is only meaningful if BOTH engines draw it in the same place.
	Describe("Java writes, Go reads", func() {
		It("stores the window bookkeeping where Go expects it under prefix 10", func() {
			Expect(store.SaveOrdersJava(ctx, []swOrder{
				{ID: 1, Price: 10, Vector: []float64{1, 0, 0}},
				{ID: 2, Price: 20, Vector: []float64{2, 0, 0}},
				{ID: 3, Price: 30, Vector: []float64{3, 0, 0}},
				{ID: 4, Price: 40, Vector: []float64{4, 0, 0}},
			})).To(Succeed())

			// (1) The ENTRY LIST holds every record, in window-key order, each
			// mapping to its packed primary key.
			entries, err := store.ReadWindowEntriesGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(Equal([]swEntry{
				{Key: tuple.Tuple{int64(10), int64(1)}, Value: tuple.Tuple{int64(1)}},
				{Key: tuple.Tuple{int64(20), int64(2)}, Value: tuple.Tuple{int64(2)}},
				{Key: tuple.Tuple{int64(30), int64(3)}, Value: tuple.Tuple{int64(3)}},
				{Key: tuple.Tuple{int64(40), int64(4)}, Value: tuple.Tuple{int64(4)}},
			}))

			// (2) The COUNT is the WINDOW's size, not the entry list's — and it
			// is a packed TUPLE, not a raw little-endian int64. Reading it with
			// the wrong encoding is the single easiest way to ship a
			// Java-unreadable store, so it is asserted at both levels.
			countBytes, err := store.ReadWindowMetaRawGo(ctx, swCountMetaKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(countBytes).To(Equal(tuple.Tuple{int64(2)}.Pack()))

			// (3) The BOUNDARY names the worst entry inside the window. For ASC
			// that is the LARGEST admitted price, not the smallest.
			boundaryBytes, err := store.ReadWindowMetaRawGo(ctx, swBoundaryMetaKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(boundaryBytes).To(Equal(tuple.Tuple{int64(20), int64(2)}.Pack()))

			// (4) And the HNSW graph Java built really does hold only the two
			// elected records — Go reads the same graph and agrees.
			ids, err := store.SearchGo(ctx, []float64{0, 0, 0}, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]int64{1, 2}))
		})
	})

	Describe("Go writes, Java reads", func() {
		It("evicts through the window Java built and Java sees the same members", func() {
			Expect(store.SaveOrdersJava(ctx, []swOrder{
				{ID: 1, Price: 10, Vector: []float64{1, 0, 0}},
				{ID: 2, Price: 20, Vector: []float64{2, 0, 0}},
				{ID: 3, Price: 30, Vector: []float64{3, 0, 0}},
				{ID: 4, Price: 40, Vector: []float64{4, 0, 0}},
			})).To(Succeed())

			// Go inserts a BETTER record. It must evict order 2 — the boundary
			// Java wrote — from the graph, move the pointer inward, and leave
			// order 2's entry in place on the overflow side.
			Expect(store.SaveOrderGo(ctx, 5, 5, []float64{5, 0, 0})).To(Succeed())

			javaIDs, err := store.SearchJava(ctx, []float64{0, 0, 0}, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaIDs).To(Equal([]int64{1, 5}),
				"Java must see exactly the window Go elected")

			boundaryBytes, err := store.ReadWindowMetaRawGo(ctx, swBoundaryMetaKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(boundaryBytes).To(Equal(tuple.Tuple{int64(10), int64(1)}.Pack()))

			// EVICTION MOVED THE POINTER, NOT THE DATA: order 2 is still a
			// record and still an entry, just outside the window now.
			entries, err := store.ReadWindowEntriesGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(5))
			Expect(entries[2].Key).To(Equal(tuple.Tuple{int64(20), int64(2)}))
		})
	})

	Describe("Java deletes, Go reads", func() {
		It("re-elects from overflow and both engines agree on the new window", func() {
			Expect(store.SaveOrdersJava(ctx, []swOrder{
				{ID: 1, Price: 10, Vector: []float64{1, 0, 0}},
				{ID: 2, Price: 20, Vector: []float64{2, 0, 0}},
				{ID: 3, Price: 30, Vector: []float64{3, 0, 0}},
			})).To(Succeed())

			// Deleting an IN-WINDOW record leaves the window a member short, so
			// the best overflow entry is promoted and the pointer moves outward.
			// This is the arm that reads the entry list rather than just writing
			// it, so a layout disagreement surfaces as a WRONG PROMOTION rather
			// than as a missing key.
			deleted, err := store.DeleteOrderJava(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			countBytes, err := store.ReadWindowMetaRawGo(ctx, swCountMetaKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(countBytes).To(Equal(tuple.Tuple{int64(2)}.Pack()))

			boundaryBytes, err := store.ReadWindowMetaRawGo(ctx, swBoundaryMetaKey)
			Expect(err).NotTo(HaveOccurred())
			Expect(boundaryBytes).To(Equal(tuple.Tuple{int64(30), int64(3)}.Pack()))

			ids, err := store.SearchGo(ctx, []float64{0, 0, 0}, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(ids).To(Equal([]int64{2, 3}))

			entries, err := store.ReadWindowEntriesGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(entries).To(HaveLen(2), "the deleted record's entry must be gone")
		})
	})

	Describe("Go deletes, Java reads", func() {
		It("re-elects from overflow and Java sees the promotion", func() {
			Expect(store.SaveOrdersJava(ctx, []swOrder{
				{ID: 1, Price: 10, Vector: []float64{1, 0, 0}},
				{ID: 2, Price: 20, Vector: []float64{2, 0, 0}},
				{ID: 3, Price: 30, Vector: []float64{3, 0, 0}},
			})).To(Succeed())

			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			javaIDs, err := store.SearchJava(ctx, []float64{0, 0, 0}, 100)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaIDs).To(Equal([]int64{2, 3}),
				"Java must see the record Go promoted out of overflow")
		})
	})
})

// swOrder is one record for the cross-engine window specs.
type swOrder struct {
	ID     int64
	Price  int32
	Vector []float64
}

// swEntry is one row of the keyspace-10 entry list, unpacked.
type swEntry struct {
	Key   tuple.Tuple
	Value tuple.Tuple
}

// SlidingWindowConformanceStore drives a VECTOR index carrying a row-number
// window predicate from both engines.
type SlidingWindowConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Index       *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewSlidingWindowConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*SlidingWindowConformanceStore, error) {
	// Must be byte-identical to SlidingWindowIndexSteps.createSlidingWindowMetaData:
	// same index name (so the same subspace key), same KeyWithValue root, same
	// HNSW options, and the same window declaration.
	idx := recordlayer.NewVectorIndex(swConformanceIndex,
		recordlayer.KeyWithValue(recordlayer.Field("vector_data"), 0), 3)
	idx.Options[recordlayer.IndexOptionVectorMetric] = "EUCLIDEAN_SQUARE_METRIC"
	if err := idx.SetPredicateProto(&gen.Predicate{
		RowNumberWindowPredicate: &gen.RowNumberWindowPredicate{
			OrderingField: []string{"price"},
			Size:          proto.Int32(2),
			Direction:     gen.RowNumberWindowPredicate_ASC.Enum(),
		},
	}); err != nil {
		return nil, err
	}

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &SlidingWindowConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Index:       idx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *SlidingWindowConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// windowSubspace rebuilds <store>/10/<index subspace key> from the OUTSIDE,
// from the literals at the top of this file rather than from the record layer's
// constants. A layout regression has to be visible here even if the maintainer
// and its own constants move together.
func (s *SlidingWindowConformanceStore) windowSubspace() subspace.Subspace {
	return s.Keyspace.Sub(swKeyspacePrefix, swConformanceIndex)
}

func (s *SlidingWindowConformanceStore) ReadWindowEntriesGo(ctx context.Context) ([]swEntry, error) {
	var out []swEntry
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		entriesSub := s.windowSubspace().Sub(swEntriesSubspace)
		begin, end := entriesSub.FDBRangeKeys()
		kvs, err := rtx.Transaction().GetRange(
			fdb.KeyRange{Begin: begin, End: end}, fdb.RangeOptions{}).GetSliceWithError()
		if err != nil {
			return nil, err
		}
		out = nil
		for _, kv := range kvs {
			key, uerr := entriesSub.Unpack(kv.Key)
			if uerr != nil {
				return nil, uerr
			}
			val, uerr := tuple.Unpack(kv.Value)
			if uerr != nil {
				return nil, uerr
			}
			out = append(out, swEntry{Key: key, Value: val})
		}
		return nil, nil
	})
	return out, err
}

// ReadWindowMetaRawGo returns the RAW BYTES at a meta key, deliberately
// undecoded: the encoding is part of the contract, so a helper that decoded
// first would hide the difference between Java's packed tuple and a raw int64.
func (s *SlidingWindowConformanceStore) ReadWindowMetaRawGo(ctx context.Context, metaKey int) ([]byte, error) {
	var out []byte
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		metaSub := s.windowSubspace().Sub(swMetaSubspace)
		v, err := rtx.Transaction().Get(metaSub.Pack(tuple.Tuple{metaKey})).Get()
		if err != nil {
			return nil, err
		}
		out = v
		return nil, nil
	})
	return out, err
}

func (s *SlidingWindowConformanceStore) SaveOrderGo(ctx context.Context, orderID int64, price int32, vec []float64) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = st.SaveRecord(&gen.Order{
			OrderId:    proto.Int64(orderID),
			Price:      proto.Int32(price),
			VectorData: conformanceSerializeVector(vec),
		})
		return nil, err
	})
	return err
}

func (s *SlidingWindowConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
	var deleted bool
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		deleted, err = st.DeleteRecord(tuple.Tuple{orderID})
		return nil, err
	})
	return deleted, err
}

// SearchGo returns the primary keys the HNSW graph holds, sorted. k is far
// larger than the window, so the answer is the graph's whole membership.
func (s *SlidingWindowConformanceStore) SearchGo(ctx context.Context, query []float64, k int) ([]int64, error) {
	var ids []int64
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		results, err := st.SearchVectorIndex(s.Index, query, k, 200)
		if err != nil {
			return nil, err
		}
		ids = nil
		for _, r := range results {
			ids = append(ids, r.PrimaryKey[0].(int64))
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		return nil, nil
	})
	return ids, err
}

// --- Java step wrappers ---

func (s *SlidingWindowConformanceStore) SaveOrdersJava(ctx context.Context, orders []swOrder) error {
	type orderEntry struct {
		OrderID int64     `json:"orderId"`
		Price   int32     `json:"price"`
		Vector  []float64 `json:"vector"`
	}
	entries := make([]orderEntry, len(orders))
	for i, o := range orders {
		entries[i] = orderEntry{OrderID: o.ID, Price: o.Price, Vector: o.Vector}
	}
	payload, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	params := s.buildJavaParams()
	params["ordersJson"] = string(payload)
	return s.java.InvokeAs(ctx, "saveOrdersWithSlidingWindowIndex", params, nil)
}

func (s *SlidingWindowConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) (bool, error) {
	params := s.buildJavaParams()
	params["orderId"] = orderID
	var deleted bool
	if err := s.java.InvokeAs(ctx, "deleteOrderWithSlidingWindowIndex", params, &deleted); err != nil {
		return false, err
	}
	return deleted, nil
}

func (s *SlidingWindowConformanceStore) SearchJava(ctx context.Context, query []float64, k int) ([]int64, error) {
	params := s.buildJavaParams()
	vecJSON, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	params["vectorJson"] = string(vecJSON)
	params["k"] = int64(k)
	var raw []any
	if err := s.java.InvokeAs(ctx, "searchSlidingWindowIndex", params, &raw); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(raw))
	for _, entry := range raw {
		m := entry.(map[string]any)
		ids = append(ids, int64(m["orderId"].(float64)))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}
