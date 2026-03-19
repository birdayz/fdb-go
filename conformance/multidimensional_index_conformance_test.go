package conformance_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/google/uuid"
	"google.golang.org/protobuf/proto"

	"github.com/apple/foundationdb/bindings/go/src/fdb/subspace"
	"github.com/apple/foundationdb/bindings/go/src/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

var _ = Describe("MULTIDIMENSIONAL Index Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *MultidimensionalIndexConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("md_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewMultidimensionalIndexConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go writes, Java scans", func() {
		It("should produce identical R-tree entries visible to both Go and Java", func() {
			coords := []struct {
				id int64
				x  int64
				y  int64
			}{
				{1, 100, 200},
				{2, 300, 400},
				{3, 500, 600},
			}
			for _, c := range coords {
				err := store.SaveOrderGo(ctx, c.id, c.x, c.y)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Java writes, Go scans", func() {
		It("should produce identical R-tree entries visible to both Go and Java", func() {
			coords := []struct {
				id int64
				x  int64
				y  int64
			}{
				{1, 10, 20},
				{2, 30, 40},
				{3, 50, 60},
			}
			for _, c := range coords {
				err := store.SaveOrderJava(ctx, c.id, c.x, c.y)
				Expect(err).NotTo(HaveOccurred())
			}

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(3))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(3))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Mixed writes: Go and Java both insert", func() {
		It("should produce identically ordered entries from both sides", func() {
			// Go writes 2 records
			err := store.SaveOrderGo(ctx, 1, 100, 200)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, 300, 400)
			Expect(err).NotTo(HaveOccurred())

			// Java writes 2 records
			err = store.SaveOrderJava(ctx, 3, 500, 600)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 4, 700, 800)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(4))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(4))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("Go deletes Java-written record", func() {
		It("should remove the R-tree entry when Go deletes a Java-written record", func() {
			// Java writes 2 records
			err := store.SaveOrderJava(ctx, 1, 10, 20)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderJava(ctx, 2, 30, 40)
			Expect(err).NotTo(HaveOccurred())

			// Go deletes order 1
			deleted, err := store.DeleteOrderGo(ctx, 1)
			Expect(err).NotTo(HaveOccurred())
			Expect(deleted).To(BeTrue())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Remaining entry should be the (30, 40) point
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(30)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(40)))
		})
	})

	Describe("Java deletes Go-written record", func() {
		It("should remove the R-tree entry when Java deletes a Go-written record", func() {
			// Go writes 2 records
			err := store.SaveOrderGo(ctx, 1, 100, 200)
			Expect(err).NotTo(HaveOccurred())

			err = store.SaveOrderGo(ctx, 2, 300, 400)
			Expect(err).NotTo(HaveOccurred())

			// Java deletes order 2
			err = store.DeleteOrderJava(ctx, 2)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			// Remaining entry should be the (100, 200) point
			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(100)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(200)))
		})
	})

	Describe("Update changes R-tree entry cross-language", func() {
		It("should update when Go updates a Java-written record", func() {
			// Java writes coord (10, 20)
			err := store.SaveOrderJava(ctx, 1, 10, 20)
			Expect(err).NotTo(HaveOccurred())

			// Go updates to coord (50, 60)
			err = store.SaveOrderGo(ctx, 1, 50, 60)
			Expect(err).NotTo(HaveOccurred())

			goEntries, err := store.ScanIndexGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(goEntries).To(HaveLen(1))

			javaEntries, err := store.ScanIndexJava(ctx)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaEntries).To(HaveLen(1))

			err = CompareIndexEntries(goEntries, javaEntries)
			Expect(err).NotTo(HaveOccurred())

			Expect(toInt64(goEntries[0].Key[0])).To(Equal(int64(50)))
			Expect(toInt64(goEntries[0].Key[1])).To(Equal(int64(60)))
		})
	})
})

// MultidimensionalIndexConformanceStore wraps record operations with a MULTIDIMENSIONAL
// index on Order's coord_x and coord_y fields (both int64).
type MultidimensionalIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	MDIndex     *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewMultidimensionalIndexConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*MultidimensionalIndexConformanceStore, error) {
	dimExpr := recordlayer.Dimensions(
		recordlayer.Concat(recordlayer.Field("coord_x"), recordlayer.Field("coord_y")),
		0, 2,
	)
	mdIdx := recordlayer.NewMultidimensionalIndex("order_coord_md", dimExpr)

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("order_id"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("customer_id"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("id"))
	builder.AddIndex("Order", mdIdx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &MultidimensionalIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		MDIndex:     mdIdx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *MultidimensionalIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *MultidimensionalIndexConformanceStore) SaveOrderGo(ctx context.Context, orderID, x, y int64) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(&gen.Order{
			OrderId: proto.Int64(orderID),
			CoordX:  proto.Int64(x),
			CoordY:  proto.Int64(y),
		})
		return nil, err
	})
	return err
}

func (s *MultidimensionalIndexConformanceStore) SaveOrderJava(ctx context.Context, orderID, x, y int64) error {
	params := s.buildJavaParams()
	params["order"] = &gen.Order{
		OrderId: proto.Int64(orderID),
		CoordX:  proto.Int64(x),
		CoordY:  proto.Int64(y),
	}
	return s.java.InvokeAs(ctx, "saveOrderWithMultidimensionalIndex", params, nil)
}

func (s *MultidimensionalIndexConformanceStore) DeleteOrderGo(ctx context.Context, orderID int64) (bool, error) {
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

func (s *MultidimensionalIndexConformanceStore) DeleteOrderJava(ctx context.Context, orderID int64) error {
	params := s.buildJavaParams()
	params["orderID"] = orderID
	return s.java.InvokeAs(ctx, "deleteOrderWithMultidimensionalIndex", params, nil)
}

func (s *MultidimensionalIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx, store.ScanIndex(s.MDIndex, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			results = append(results, IndexEntryResult{
				Key:        tupleToSlice(e.Key),
				PrimaryKey: tupleToSlice(e.PrimaryKey()),
			})
		}
		return nil, nil
	})
	return results, err
}

func (s *MultidimensionalIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanMultidimensionalIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanMultidimensionalIndex failed: %w", err)
	}

	var results []IndexEntryResult
	for _, m := range javaResults {
		entry := IndexEntryResult{}
		if keyRaw, ok := m["key"]; ok {
			entry.Key = toInterfaceSlice(keyRaw)
		}
		if pkRaw, ok := m["primaryKey"]; ok {
			entry.PrimaryKey = toInterfaceSlice(pkRaw)
		}
		results = append(results, entry)
	}
	return results, nil
}
