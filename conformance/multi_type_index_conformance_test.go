//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/protobuf/proto"
)

// Multi-type and universal index entry conformance.
//
// The sibling composite-index suite covers a SINGLE-TYPE index whose key
// overlaps the primary key, where both engines trim the redundant components.
// That is one of three registration shapes and the only one Java trims: its
// sole MAIN-SOURCE setPrimaryKeyComponentPositions call site
// (RecordMetaDataBuilder.java:1466) iterates recordTypeBuilder.getIndexes(),
// while addMultiTypeIndex routes zero record-type names to universalIndexes and
// two-or-more to getMultiTypeIndexes(). Neither list is visited.
//
// SO THE OTHER TWO SHAPES WRITE THE PRIMARY KEY WHOLE, and Go trimmed it away —
// a different index entry key in FDB for identical metadata. This suite could
// not see it: it contained no multi-type index at all, and its single universal
// index used a key that could not overlap a primary key, so positions were nil
// on both sides for a reason unrelated to the rule under test. The gap was
// dimensional, not volumetric, which is why a full green meant nothing here.
//
// Every record type is keyed on `price` because that is the only field the demo
// proto declares on all three, and a universal index key must be valid on every
// one of them. It is also what makes the overlap real: with a key that cannot
// appear in a primary key, both engines return nil positions and the comparison
// proves nothing.
//
// WHICH ARM ACTUALLY CATCHES THE DIVERGENCE, measured by reintroducing it: the
// "writes the same index entry bytes as Java" arm reddens for both index kinds,
// and the "reads back what Java wrote" arm does NOT. That is not a defect in
// the second arm, it is what it means: given bytes Java wrote, both engines
// decode them alike even with Go's metadata carrying wrong positions, so the
// read arm is guarding a different property -- a decoder that silently
// compensates for its own encoder -- and it would be the only arm to fire if
// that property ever broke. Stated here so nobody reads its green as coverage
// of the write-side rule.
var _ = Describe("Multi-Type and Universal Index Entry Conformance", func() {
	var (
		ctx context.Context
		env *TenantEnvironment
	)

	BeforeEach(func() {
		ctx = context.Background()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
			env = nil
		}
	})

	for _, tc := range []struct {
		kind string
		// register is the Go-side spelling matching the Java handler's `kind`.
		register func(b *recordlayer.RecordMetaDataBuilder, idx *recordlayer.Index)
		name     string
	}{
		{
			kind: "multiType",
			name: "MT$price",
			register: func(b *recordlayer.RecordMetaDataBuilder, idx *recordlayer.Index) {
				b.AddMultiTypeIndex([]string{"Order", "Customer"}, idx)
			},
		},
		{
			kind: "universal",
			name: "UNI$price",
			register: func(b *recordlayer.RecordMetaDataBuilder, idx *recordlayer.Index) {
				b.AddUniversalIndex(idx)
			},
		},
	} {
		tc := tc
		Describe(tc.kind, func() {
			It("writes the same index entry bytes as Java", func() {
				tenantName := fmt.Sprintf("mtidx_%s", uuid.New().String())
				var err error
				env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
				Expect(err).NotTo(HaveOccurred())

				store, err := NewSharedIndexConformanceStore(
					env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName, tc.kind, tc.name, tc.register)
				Expect(err).NotTo(HaveOccurred())

				// THE CAUSE, asserted before the effect. Both engines must agree
				// that this index has NO primaryKeyComponentPositions; without
				// this, matching entry bytes could come from Go and Java making
				// opposite mistakes that happen to cancel.
				javaHasPositions, err := store.HasPositionsJava(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaHasPositions).To(BeFalse(),
					"Java assigns primaryKeyComponentPositions only through getIndexes(); if this "+
						"is true the premise of this whole suite is wrong and Go should match the new truth")
				Expect(store.MetaData.GetIndex(tc.name).HasPrimaryKeyComponentPositions()).To(Equal(javaHasPositions),
					"Go and Java disagree about whether this index has primaryKeyComponentPositions")

				// Go writes.
				for i := int64(1); i <= 3; i++ {
					err := store.SaveOrderGo(ctx, &gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				goEntries, err := store.ScanIndexGo(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(goEntries).To(HaveLen(3))

				// The absolute shape, not just cross-engine agreement: the
				// primary key IS the index key here, so an untrimmed entry
				// repeats the value and a trimmed one does not. Asserting only
				// go==java would pass if both engines trimmed.
				for _, e := range goEntries {
					Expect(e.Key).To(HaveLen(2),
						"entry key must be (price, pk) — 2 elements — because a %s index is never "+
							"assigned primaryKeyComponentPositions and so its primary key is written whole; "+
							"got %v", tc.kind, e.Key)
				}

				javaEntries, err := store.ScanIndexJava(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(javaEntries).To(HaveLen(3))

				Expect(CompareIndexEntries(goEntries, javaEntries)).To(Succeed())
			})

			It("reads back what Java wrote", func() {
				tenantName := fmt.Sprintf("mtidxj_%s", uuid.New().String())
				var err error
				env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
				Expect(err).NotTo(HaveOccurred())

				store, err := NewSharedIndexConformanceStore(
					env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName, tc.kind, tc.name, tc.register)
				Expect(err).NotTo(HaveOccurred())

				// The other direction. Go writing Java-compatible bytes and Go
				// reading Java's bytes are different claims, and only the second
				// catches a decoder that compensates for its own encoder.
				for i := int64(1); i <= 3; i++ {
					err := store.SaveOrderJava(ctx, &gen.Order{
						OrderId: proto.Int64(i),
						Price:   proto.Int32(int32(i * 100)),
					})
					Expect(err).NotTo(HaveOccurred())
				}

				goEntries, err := store.ScanIndexGo(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(goEntries).To(HaveLen(3))

				javaEntries, err := store.ScanIndexJava(ctx)
				Expect(err).NotTo(HaveOccurred())
				Expect(CompareIndexEntries(goEntries, javaEntries)).To(Succeed())
			})
		})
	}
})

// SharedIndexConformanceStore wraps a metadata whose record types are all keyed
// on `price`, carrying one index registered as either multi-type or universal.
// It must match Java's MultiTypeIndexSteps metadata exactly.
type SharedIndexConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Index       *recordlayer.Index
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
	indexKind   string
}

func NewSharedIndexConformanceStore(
	recordDB *recordlayer.FDBDatabase,
	keyspace subspace.Subspace,
	clusterFile string,
	tenantName string,
	indexKind string,
	indexName string,
	register func(*recordlayer.RecordMetaDataBuilder, *recordlayer.Index),
) (*SharedIndexConformanceStore, error) {
	idx := recordlayer.NewIndex(indexName, recordlayer.Field("price"))

	builder := recordlayer.NewRecordMetaDataBuilder().SetRecords(gen.File_record_layer_demo_proto)
	builder.GetRecordType("Order").SetPrimaryKey(recordlayer.Field("price"))
	builder.GetRecordType("Customer").SetPrimaryKey(recordlayer.Field("price"))
	builder.GetRecordType("TypedRecord").SetPrimaryKey(recordlayer.Field("price"))
	register(builder, idx)
	md, err := builder.Build()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &SharedIndexConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Index:       idx,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
		indexKind:   indexKind,
	}, nil
}

func (s *SharedIndexConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
		"indexKind":   s.indexKind,
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

func (s *SharedIndexConformanceStore) SaveOrderGo(ctx context.Context, order *gen.Order) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.SaveRecord(order)
		return nil, err
	})
	return err
}

func (s *SharedIndexConformanceStore) SaveOrderJava(ctx context.Context, order *gen.Order) error {
	params := s.buildJavaParams()
	params["order"] = order
	return s.java.InvokeAs(ctx, "saveOrderWithSharedIndex", params, nil)
}

func (s *SharedIndexConformanceStore) ScanIndexGo(ctx context.Context) ([]IndexEntryResult, error) {
	var results []IndexEntryResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		entries, err := recordlayer.AsList(ctx,
			store.ScanIndex(s.Index, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan()))
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

func (s *SharedIndexConformanceStore) ScanIndexJava(ctx context.Context) ([]IndexEntryResult, error) {
	params := s.buildJavaParams()

	var javaResults []map[string]any
	if err := s.java.InvokeAs(ctx, "scanSharedIndex", params, &javaResults); err != nil {
		return nil, fmt.Errorf("java scanSharedIndex failed: %w", err)
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

// HasPositionsJava asks Java whether it assigned primaryKeyComponentPositions,
// so the suite asserts the metadata-level CAUSE and not only the byte-level
// effect.
func (s *SharedIndexConformanceStore) HasPositionsJava(ctx context.Context) (bool, error) {
	var has bool
	params := map[string]any{"indexKind": s.indexKind}
	if err := s.java.InvokeAs(ctx, "sharedIndexHasPrimaryKeyComponentPositions", params, &has); err != nil {
		return false, fmt.Errorf("java sharedIndexHasPrimaryKeyComponentPositions failed: %w", err)
	}
	return has, nil
}
