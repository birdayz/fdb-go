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
	"google.golang.org/protobuf/proto"
)

var _ = Describe("Store Header Format Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *StoreHeaderConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()

		tenantName := fmt.Sprintf("header_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewStoreHeaderConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Go creates store, Java reads raw header", func() {
		It("should produce a header Java can parse with correct fields", func() {
			// Go creates the store
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header (no store open, pure proto parse)
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw header for comparison
			goHeader, err := store.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both must agree on all fields
			Expect(javaHeader.FormatVersion).To(Equal(goHeader.FormatVersion),
				"format version mismatch: go=%d java=%d", goHeader.FormatVersion, javaHeader.FormatVersion)
			Expect(javaHeader.MetaDataVersion).To(Equal(goHeader.MetaDataVersion),
				"metadata version mismatch: go=%d java=%d", goHeader.MetaDataVersion, javaHeader.MetaDataVersion)
			Expect(javaHeader.UserVersion).To(Equal(goHeader.UserVersion),
				"user version mismatch: go=%d java=%d", goHeader.UserVersion, javaHeader.UserVersion)

			// Go creates with format version 14, user version 0
			Expect(goHeader.FormatVersion).To(Equal(int32(14)))
			Expect(goHeader.UserVersion).To(Equal(int32(0)))
			Expect(goHeader.MetaDataVersion).To(BeNumerically(">=", 0))
		})
	})

	Describe("Java creates store, Go reads raw header", func() {
		It("should produce a header Go can parse with correct fields", func() {
			// Java creates the store
			err := store.CreateStoreJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw header
			goHeader, err := store.GetStoreHeaderRawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header for comparison
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both must agree
			Expect(goHeader.FormatVersion).To(Equal(javaHeader.FormatVersion))
			Expect(goHeader.MetaDataVersion).To(Equal(javaHeader.MetaDataVersion))
			Expect(goHeader.UserVersion).To(Equal(javaHeader.UserVersion))

			// Java default format version is 7 (CACHEABLE_STATE), user version 0
			Expect(goHeader.FormatVersion).To(Equal(int32(7)))
			Expect(goHeader.UserVersion).To(Equal(int32(0)))
			Expect(goHeader.MetaDataVersion).To(BeNumerically(">=", 0))
		})
	})

	Describe("User version cross-platform persistence", func() {
		It("Go sets user version, Java reads it", func() {
			// Go creates store and sets user version
			err := store.SetUserVersionGo(ctx, 42)
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header
			javaHeader, err := store.GetStoreHeaderRawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(javaHeader.UserVersion).To(Equal(int32(42)))
		})

		It("Java sets user version, Go reads it", func() {
			// Java creates store with user version 99
			err := store.CreateStoreJavaWithUserVersion(ctx, 99)
			Expect(err).NotTo(HaveOccurred())

			// Go reads via store open
			goHeader, err := store.GetStoreHeaderViaOpenGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(goHeader.UserVersion).To(Equal(int32(99)))
		})
	})
})

// StoreHeaderResult holds the parsed store header fields for comparison.
type StoreHeaderResult struct {
	FormatVersion   int32
	MetaDataVersion int32
	UserVersion     int32
}

// StoreHeaderConformanceStore wraps store header operations for cross-platform testing.
type StoreHeaderConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

// NewStoreHeaderConformanceStore creates a conformance store for store header tests.
func NewStoreHeaderConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*StoreHeaderConformanceStore, error) {
	md, err := createOrderMetaData()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &StoreHeaderConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *StoreHeaderConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// CreateStoreGo creates a store using Go's CreateOrOpen.
func (s *StoreHeaderConformanceStore) CreateStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		return nil, err
	})
	return err
}

// CreateStoreJava creates a store using Java's createOrOpen (via saveOrder with a dummy record, then delete).
// Actually just uses createStoreWithUserVersion with userVersion=0 which calls createOrOpen.
func (s *StoreHeaderConformanceStore) CreateStoreJava(ctx context.Context) error {
	params := s.buildJavaParams()
	params["userVersion"] = 0
	return s.java.InvokeAs(ctx, "createStoreWithUserVersion", params, nil)
}

// CreateStoreJavaWithUserVersion creates a store with a specific user version via Java.
func (s *StoreHeaderConformanceStore) CreateStoreJavaWithUserVersion(ctx context.Context, userVersion int32) error {
	params := s.buildJavaParams()
	params["userVersion"] = int(userVersion)
	return s.java.InvokeAs(ctx, "createStoreWithUserVersion", params, nil)
}

// SetUserVersionGo opens the store with Go and sets the user version.
func (s *StoreHeaderConformanceStore) SetUserVersionGo(ctx context.Context, userVersion int32) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return nil, store.SetUserVersion(userVersion)
	})
	return err
}

// GetStoreHeaderRawGo reads the raw store header bytes from FDB and parses them.
// No store open — just a raw FDB read to avoid side effects.
func (s *StoreHeaderConformanceStore) GetStoreHeaderRawGo(ctx context.Context) (*StoreHeaderResult, error) {
	var result StoreHeaderResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		headerKey := fdb.Key(s.Keyspace.Pack(tuple.Tuple{int64(0)})) // StoreInfoKey = 0
		headerBytes, err := rtx.Transaction().Get(headerKey).Get()
		if err != nil {
			return nil, fmt.Errorf("failed to read store header: %w", err)
		}
		if headerBytes == nil {
			return nil, fmt.Errorf("store header not found")
		}
		var header gen.DataStoreInfo
		if err := proto.Unmarshal(headerBytes, &header); err != nil {
			return nil, fmt.Errorf("failed to parse store header: %w", err)
		}
		result.FormatVersion = header.GetFormatVersion()
		result.MetaDataVersion = header.GetMetaDataversion()
		result.UserVersion = header.GetUserVersion()
		return nil, nil
	})
	return &result, err
}

// GetStoreHeaderRawJava reads the raw store header via Java (no store open).
func (s *StoreHeaderConformanceStore) GetStoreHeaderRawJava(ctx context.Context) (*StoreHeaderResult, error) {
	params := s.buildJavaParams()

	var javaResult map[string]any
	if err := s.java.InvokeAs(ctx, "getStoreHeaderRaw", params, &javaResult); err != nil {
		return nil, fmt.Errorf("java getStoreHeaderRaw failed: %w", err)
	}

	result := &StoreHeaderResult{}
	if v, ok := javaResult["formatVersion"]; ok {
		result.FormatVersion = int32(v.(float64))
	}
	if v, ok := javaResult["metaDataVersion"]; ok {
		result.MetaDataVersion = int32(v.(float64))
	}
	if v, ok := javaResult["userVersion"]; ok {
		result.UserVersion = int32(v.(float64))
	}
	return result, nil
}

// GetStoreHeaderViaOpenGo opens the store with Go and reads the header through the store API.
func (s *StoreHeaderConformanceStore) GetStoreHeaderViaOpenGo(ctx context.Context) (*StoreHeaderResult, error) {
	var result StoreHeaderResult
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		result.FormatVersion = store.GetFormatVersion()
		result.MetaDataVersion = store.GetMetaDataVersion()
		result.UserVersion = store.GetUserVersion()
		return nil, nil
	})
	return &result, err
}
