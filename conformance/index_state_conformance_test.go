//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"fdb.dev/gen"
	"fdb.dev/pkg/fdbgo/fdb"
	"fdb.dev/pkg/fdbgo/fdb/subspace"
	"fdb.dev/pkg/fdbgo/fdb/tuple"
	"fdb.dev/pkg/recordlayer"
	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("Index State Persistence Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *IndexStateConformanceStore
	)

	const indexName = "Order$price"

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("idxstate_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewIndexStateConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())

		// Create the store first so both sides can open it
		err = store.CreateStoreGo(ctx)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go marks WRITE_ONLY, Java reads raw state", func() {
		It("should persist WRITE_ONLY state readable by Java", func() {
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw state
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Go reads raw state for comparison
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Java marks WRITE_ONLY, Go reads state", func() {
		It("should persist WRITE_ONLY state readable by Go", func() {
			err := store.MarkIndexWriteOnlyJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw state
			goState, err := store.GetIndexStateRawGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(goState).To(Equal("WRITE_ONLY"))

			// Go opens store and reads state through API
			openState, err := store.GetIndexStateViaOpenGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(openState).To(Equal("WRITE_ONLY"))
		})
	})

	Describe("Go marks DISABLED, Java reads raw state", func() {
		It("should persist DISABLED state readable by Java", func() {
			err := store.MarkIndexDisabledGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("DISABLED"))
		})
	})

	Describe("Go marks WRITE_ONLY then READABLE, Java reads default", func() {
		It("should clear state entry when returning to READABLE", func() {
			// Mark WRITE_ONLY first
			err := store.MarkIndexWriteOnlyGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Verify it's WRITE_ONLY
			javaState, err := store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("WRITE_ONLY"))

			// Mark READABLE (clears the key)
			err = store.MarkIndexReadableGo(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())

			// Should be READABLE (no key in FDB)
			javaState, err = store.GetIndexStateRawJava(ctx, indexName)
			Expect(err).NotTo(HaveOccurred())
			Expect(javaState).To(Equal("READABLE"))
		})
	})
})

// IndexStateConformanceStore wraps index state operations for cross-platform testing.
// Uses the indexed metadata (Order$price VALUE index) matching Java's createIndexedMetaData().
type IndexStateConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewIndexStateConformanceStore creates a conformance store for index state persistence tests.
func NewIndexStateConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*IndexStateConformanceStore, error) {
	idx := recordlayer.NewIndex("Order$price", recordlayer.Field("price"))
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

	return &IndexStateConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *IndexStateConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// CreateStoreGo creates the indexed store using Go's CreateOrOpen.
func (s *IndexStateConformanceStore) CreateStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyGo marks an index as WRITE_ONLY using Go.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexWriteOnly(indexName)
		return nil, err
	})
	return err
}

// MarkIndexDisabledGo marks an index as DISABLED using Go.
func (s *IndexStateConformanceStore) MarkIndexDisabledGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		_, err = store.MarkIndexDisabled(indexName)
		return nil, err
	})
	return err
}

// MarkIndexReadableGo marks an index as READABLE using Go.
// Inserts the full range set first so that checkIndexBuilt passes.
func (s *IndexStateConformanceStore) MarkIndexReadableGo(ctx context.Context, indexName string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		// Mark the range set as complete so checkIndexBuilt passes.
		idx := s.MetaData.GetIndex(indexName)
		if idx != nil {
			rangeSet := recordlayer.NewIndexingRangeSet(s.Keyspace, idx)
			if _, err := rangeSet.InsertRange(rtx.Transaction(), nil, nil, false); err != nil {
				return nil, err
			}
		}
		_, err = store.MarkIndexReadable(indexName)
		return nil, err
	})
	return err
}

// MarkIndexWriteOnlyJava marks an index as WRITE_ONLY using Java.
func (s *IndexStateConformanceStore) MarkIndexWriteOnlyJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexWriteOnly", params, nil)
}

// MarkIndexDisabledJava marks an index as DISABLED using Java.
func (s *IndexStateConformanceStore) MarkIndexDisabledJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexDisabled", params, nil)
}

// MarkIndexReadableJava marks an index as READABLE using Java.
func (s *IndexStateConformanceStore) MarkIndexReadableJava(ctx context.Context, indexName string) error {
	params := s.buildJavaParams()
	params["indexName"] = indexName
	return s.java.InvokeAs(ctx, "markIndexReadable", params, nil)
}

// GetIndexStateRawGo reads the raw index state from FDB using Go (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		isSubspace := s.Keyspace.Sub(int64(5)) // IndexStateSpaceKey = 5
		stateKey := fdb.Key(isSubspace.Pack(tuple.Tuple{indexName}))
		stateBytes, err := rtx.Transaction().Get(stateKey).Get()
		if err != nil {
			return nil, fmt.Errorf("failed to read index state: %w", err)
		}
		if stateBytes == nil {
			state = "READABLE" // Default
			return nil, nil
		}
		valueTuple, err := tuple.Unpack(stateBytes)
		if err != nil {
			return nil, fmt.Errorf("failed to unpack index state value: %w", err)
		}
		code := valueTuple[0].(int64)
		switch recordlayer.IndexState(code) {
		case recordlayer.IndexStateReadable:
			state = "READABLE"
		case recordlayer.IndexStateWriteOnly:
			state = "WRITE_ONLY"
		case recordlayer.IndexStateDisabled:
			state = "DISABLED"
		case recordlayer.IndexStateReadableUniquePending:
			state = "READABLE_UNIQUE_PENDING"
		default:
			state = fmt.Sprintf("UNKNOWN(%d)", code)
		}
		return nil, nil
	})
	return state, err
}

// GetIndexStateRawJava reads the raw index state from FDB using Java (no store open).
func (s *IndexStateConformanceStore) GetIndexStateRawJava(ctx context.Context, indexName string) (string, error) {
	params := s.buildJavaParams()
	params["indexName"] = indexName

	var state string
	if err := s.java.InvokeAs(ctx, "getIndexStateRaw", params, &state); err != nil {
		return "", fmt.Errorf("java getIndexStateRaw failed: %w", err)
	}
	return state, nil
}

// GetIndexStateViaOpenGo opens the store with Go and reads the index state through the store API.
func (s *IndexStateConformanceStore) GetIndexStateViaOpenGo(ctx context.Context, indexName string) (string, error) {
	var state string
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetIndexRebuildPolicy(recordlayer.AlwaysRebuildPolicy).Open()
		if err != nil {
			return nil, err
		}
		state = store.GetIndexState(indexName).String()
		return nil, nil
	})
	return state, err
}
