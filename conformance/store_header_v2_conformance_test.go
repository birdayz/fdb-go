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

var _ = Describe("Store Header V2 Conformance", func() {
	var (
		ctx   context.Context
		env   *TenantEnvironment
		store *StoreHeaderV2ConformanceStore
	)

	BeforeEach(func() {
		ctx = context.Background()
		tenantName := fmt.Sprintf("hdrv2_%s", uuid.New().String())

		var err error
		env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
		Expect(err).NotTo(HaveOccurred())

		store, err = NewStoreHeaderV2ConformanceStore(env.RecordDB, env.Keyspace, env.ClusterFile, env.TenantName)
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	Describe("Header user fields", func() {
		It("Go sets user field, Java reads from raw header", func() {
			// Go creates store and sets a user field
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetHeaderUserFieldGo(ctx, "my_key", []byte("hello world"))
			Expect(err).NotTo(HaveOccurred())

			// Java reads raw header
			header, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Verify user field is present with correct key/value
			Expect(header.UserFields).To(HaveLen(1))
			Expect(header.UserFields[0].Key).To(Equal("my_key"))
			Expect(header.UserFields[0].Value).To(Equal([]byte("hello world")))
		})

		It("Java sets user field, Go reads from raw header", func() {
			// Go creates store first (Java can then open it)
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java sets a user field
			err = store.SetHeaderUserFieldJava(ctx, "java_key", []byte{0xDE, 0xAD, 0xBE, 0xEF})
			Expect(err).NotTo(HaveOccurred())

			// Go reads raw header
			header, err := store.GetStoreHeaderV2RawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(header.UserFields).To(HaveLen(1))
			Expect(header.UserFields[0].Key).To(Equal("java_key"))
			Expect(header.UserFields[0].Value).To(Equal([]byte{0xDE, 0xAD, 0xBE, 0xEF}))
		})

		It("Go sets multiple user fields, Java reads all", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetHeaderUserFieldGo(ctx, "field_a", []byte("alpha"))
			Expect(err).NotTo(HaveOccurred())
			err = store.SetHeaderUserFieldGo(ctx, "field_b", []byte("beta"))
			Expect(err).NotTo(HaveOccurred())

			header, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(header.UserFields).To(HaveLen(2))
			fieldMap := make(map[string][]byte)
			for _, f := range header.UserFields {
				fieldMap[f.Key] = f.Value
			}
			Expect(fieldMap["field_a"]).To(Equal([]byte("alpha")))
			Expect(fieldMap["field_b"]).To(Equal([]byte("beta")))
		})

		It("mixed: Go sets then Java overwrites, both agree", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetHeaderUserFieldGo(ctx, "shared_key", []byte("go_value"))
			Expect(err).NotTo(HaveOccurred())

			// Java overwrites the same key
			err = store.SetHeaderUserFieldJava(ctx, "shared_key", []byte("java_value"))
			Expect(err).NotTo(HaveOccurred())

			// Both should see the Java value
			goHeader, err := store.GetStoreHeaderV2RawGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			javaHeader, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(goHeader.UserFields).To(HaveLen(1))
			Expect(goHeader.UserFields[0].Value).To(Equal([]byte("java_value")))
			Expect(javaHeader.UserFields).To(HaveLen(1))
			Expect(javaHeader.UserFields[0].Value).To(Equal([]byte("java_value")))
		})
	})

	Describe("Incarnation field", func() {
		It("Go sets incarnation, Java reads from raw header", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.UpdateIncarnationGo(ctx, 42)
			Expect(err).NotTo(HaveOccurred())

			header, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(header.Incarnation).To(Equal(int32(42)))
		})

		It("Java sets incarnation, Go reads from raw header", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetIncarnationJava(ctx, 99)
			Expect(err).NotTo(HaveOccurred())

			header, err := store.GetStoreHeaderV2RawGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(header.Incarnation).To(Equal(int32(99)))
		})

		It("Go increments, Java increments further, both agree", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.UpdateIncarnationGo(ctx, 10)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetIncarnationJava(ctx, 20)
			Expect(err).NotTo(HaveOccurred())

			goHeader, err := store.GetStoreHeaderV2RawGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			javaHeader, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			Expect(goHeader.Incarnation).To(Equal(int32(20)))
			Expect(javaHeader.Incarnation).To(Equal(int32(20)))
		})
	})

	Describe("Store lock state", func() {
		It("Go sets FULL_STORE lock, Java cannot open", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FULL_STORE, "migration in progress")
			Expect(err).NotTo(HaveOccurred())

			// Java tries to open — should fail
			result, err := store.OpenLockedStoreJava(ctx, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Success).To(BeFalse(), "Java should not be able to open a FULL_STORE-locked store")
		})

		It("Go sets FULL_STORE lock with reason, Java bypasses with matching reason", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FULL_STORE, "test_reason")
			Expect(err).NotTo(HaveOccurred())

			// Java opens with matching bypass reason — should succeed
			result, err := store.OpenLockedStoreJava(ctx, "test_reason")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Success).To(BeTrue(), "Java should be able to bypass with matching reason")
		})

		It("Go sets FULL_STORE lock, Java bypass with wrong reason fails", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FULL_STORE, "correct_reason")
			Expect(err).NotTo(HaveOccurred())

			result, err := store.OpenLockedStoreJava(ctx, "wrong_reason")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Success).To(BeFalse(), "Java should not bypass with wrong reason")
		})

		It("Go sets FORBID_RECORD_UPDATE lock, Java cannot save", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "maintenance")
			Expect(err).NotTo(HaveOccurred())

			// Java tries to save — should fail
			result, err := store.SaveOrderLockedJava(ctx, &gen.Order{
				OrderId: proto.Int64(1),
				Price:   proto.Int32(100),
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Success).To(BeFalse(), "Java should not be able to save to a FORBID_RECORD_UPDATE store")
		})

		It("Java sets FULL_STORE lock, Go cannot open", func() {
			// Go creates store first
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java sets FULL_STORE lock
			err = store.SetStoreLockJava(ctx, "FULL_STORE", "java_migration")
			Expect(err).NotTo(HaveOccurred())

			// Go tries to open — should fail
			err = store.OpenStoreGo(ctx)
			Expect(err).To(HaveOccurred())
		})

		It("Go sets lock, Go clears lock, Java can open", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FULL_STORE, "temporary")
			Expect(err).NotTo(HaveOccurred())

			// Clear the lock
			err = store.ClearStoreLockGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Java can now open
			result, err := store.OpenLockedStoreJava(ctx, "")
			Expect(err).NotTo(HaveOccurred())
			Expect(result.Success).To(BeTrue(), "Java should open after lock is cleared")
		})

		It("lock state wire format matches between Go and Java", func() {
			err := store.CreateStoreGo(ctx)
			Expect(err).NotTo(HaveOccurred())

			err = store.SetStoreLockGo(ctx, gen.DataStoreInfo_StoreLockState_FULL_STORE, "wire_test")
			Expect(err).NotTo(HaveOccurred())

			// Read raw header from both sides
			goHeader, err := store.GetStoreHeaderV2RawGo(ctx)
			Expect(err).NotTo(HaveOccurred())
			javaHeader, err := store.GetStoreHeaderV2RawJava(ctx)
			Expect(err).NotTo(HaveOccurred())

			// Both should see the same lock state
			Expect(goHeader.HasLockState).To(BeTrue())
			Expect(javaHeader.HasLockState).To(BeTrue())
			Expect(goHeader.LockState).To(Equal("FULL_STORE"))
			Expect(javaHeader.LockState).To(Equal("FULL_STORE"))
			Expect(goHeader.LockReason).To(Equal("wire_test"))
			Expect(javaHeader.LockReason).To(Equal("wire_test"))
		})
	})
})

// StoreHeaderV2Result holds parsed store header v2 fields for cross-platform comparison.
type StoreHeaderV2Result struct {
	FormatVersion int32
	Incarnation   int32
	UserFields    []UserFieldEntry
	HasLockState  bool
	LockState     string
	LockReason    string
}

type UserFieldEntry struct {
	Key   string
	Value []byte
}

// StoreHeaderV2ConformanceStore wraps store header v2 operations for cross-platform testing.
type StoreHeaderV2ConformanceStore struct {
	RecordDB    *recordlayer.FDBDatabase
	MetaData    *recordlayer.RecordMetaData
	Keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string
	tenantName  string
}

func NewStoreHeaderV2ConformanceStore(recordDB *recordlayer.FDBDatabase, keyspace subspace.Subspace, clusterFile string, tenantName string) (*StoreHeaderV2ConformanceStore, error) {
	md, err := createOrderMetaData()
	if err != nil {
		return nil, err
	}

	ks := keyspace
	if tenantName != "" {
		ks = subspace.Sub(tuple.Tuple{})
	}

	return &StoreHeaderV2ConformanceStore{
		RecordDB:    recordDB,
		MetaData:    md,
		Keyspace:    ks,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}, nil
}

func (s *StoreHeaderV2ConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": s.clusterFile,
		"subspace":    BytesToIntArray(s.Keyspace.Bytes()),
	}
	if s.tenantName != "" {
		params["tenantName"] = s.tenantName
	}
	return params
}

// CreateStoreGo creates the store using Go's CreateOrOpen.
func (s *StoreHeaderV2ConformanceStore) CreateStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).CreateOrOpen()
		return nil, err
	})
	return err
}

// OpenStoreGo tries to open the store. Returns error if the store is locked.
func (s *StoreHeaderV2ConformanceStore) OpenStoreGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		_, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		return nil, err
	})
	return err
}

// SetHeaderUserFieldGo opens the store and sets a header user field.
func (s *StoreHeaderV2ConformanceStore) SetHeaderUserFieldGo(ctx context.Context, key string, value []byte) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		return nil, st.SetHeaderUserField(key, value)
	})
	return err
}

// SetHeaderUserFieldJava sets a header user field via Java.
func (s *StoreHeaderV2ConformanceStore) SetHeaderUserFieldJava(ctx context.Context, key string, value []byte) error {
	params := s.buildJavaParams()
	params["fieldKey"] = key
	params["fieldValue"] = BytesToIntArray(value)
	return s.java.InvokeAs(ctx, "setHeaderUserFieldJava", params, nil)
}

// UpdateIncarnationGo opens the store and updates incarnation.
func (s *StoreHeaderV2ConformanceStore) UpdateIncarnationGo(ctx context.Context, value int32) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		return nil, st.UpdateIncarnation(func(current int32) int32 { return value })
	})
	return err
}

// SetIncarnationJava updates incarnation via Java.
func (s *StoreHeaderV2ConformanceStore) SetIncarnationJava(ctx context.Context, value int) error {
	params := s.buildJavaParams()
	params["incarnation"] = value
	return s.java.InvokeAs(ctx, "setIncarnationJava", params, nil)
}

// SetStoreLockGo opens the store and sets a lock state.
func (s *StoreHeaderV2ConformanceStore) SetStoreLockGo(ctx context.Context, state gen.DataStoreInfo_StoreLockState_State, reason string) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).Open()
		if err != nil {
			return nil, err
		}
		return nil, st.SetStoreLockState(state, reason)
	})
	return err
}

// ClearStoreLockGo opens the store (with bypass) and clears the lock.
func (s *StoreHeaderV2ConformanceStore) ClearStoreLockGo(ctx context.Context) error {
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		// Must bypass the lock to open the store and clear it
		st, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).SetMetaDataProvider(s.MetaData).SetSubspace(s.Keyspace).
			SetBypassFullStoreLockReason("temporary").Open()
		if err != nil {
			return nil, err
		}
		return nil, st.ClearStoreLockState()
	})
	return err
}

// SetStoreLockJava sets lock state via Java.
func (s *StoreHeaderV2ConformanceStore) SetStoreLockJava(ctx context.Context, lockState string, reason string) error {
	params := s.buildJavaParams()
	params["lockState"] = lockState
	params["reason"] = reason
	return s.java.InvokeAs(ctx, "setStoreLockStateJava", params, nil)
}

// ClearStoreLockJava clears lock state via Java.
func (s *StoreHeaderV2ConformanceStore) ClearStoreLockJava(ctx context.Context) error {
	params := s.buildJavaParams()
	return s.java.InvokeAs(ctx, "clearStoreLockStateJava", params, nil)
}

// OpenLockedStoreResult represents the result of trying to open a potentially locked store.
type OpenLockedStoreResult struct {
	Success bool
	Error   string
}

// OpenLockedStoreJava tries to open a store via Java, optionally with bypass reason.
func (s *StoreHeaderV2ConformanceStore) OpenLockedStoreJava(ctx context.Context, bypassReason string) (*OpenLockedStoreResult, error) {
	params := s.buildJavaParams()
	if bypassReason != "" {
		params["bypassReason"] = bypassReason
	}

	var result map[string]any
	if err := s.java.InvokeAs(ctx, "openLockedStoreJava", params, &result); err != nil {
		return nil, fmt.Errorf("java openLockedStoreJava failed: %w", err)
	}

	r := &OpenLockedStoreResult{}
	if v, ok := result["success"]; ok {
		r.Success = v.(bool)
	}
	if v, ok := result["error"]; ok {
		if s, ok := v.(string); ok {
			r.Error = s
		}
	}
	return r, nil
}

// SaveOrderLockedJava tries to save a record via Java to a potentially locked store.
func (s *StoreHeaderV2ConformanceStore) SaveOrderLockedJava(ctx context.Context, order *gen.Order) (*OpenLockedStoreResult, error) {
	params := s.buildJavaParams()
	params["order"] = order

	var result map[string]any
	if err := s.java.InvokeAs(ctx, "saveOrderLockedJava", params, &result); err != nil {
		return nil, fmt.Errorf("java saveOrderLockedJava failed: %w", err)
	}

	r := &OpenLockedStoreResult{}
	if v, ok := result["success"]; ok {
		r.Success = v.(bool)
	}
	if v, ok := result["error"]; ok {
		if s, ok := v.(string); ok {
			r.Error = s
		}
	}
	return r, nil
}

// GetStoreHeaderV2RawGo reads the raw store header bytes and parses v2 fields.
func (s *StoreHeaderV2ConformanceStore) GetStoreHeaderV2RawGo(ctx context.Context) (*StoreHeaderV2Result, error) {
	var result StoreHeaderV2Result
	_, err := s.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		headerKey := fdb.Key(s.Keyspace.Pack(tuple.Tuple{int64(0)}))
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
		result.Incarnation = header.GetIncarnation()

		for _, entry := range header.GetUserField() {
			result.UserFields = append(result.UserFields, UserFieldEntry{
				Key:   entry.GetKey(),
				Value: entry.GetValue(),
			})
		}

		if header.StoreLockState != nil {
			result.HasLockState = true
			result.LockState = header.StoreLockState.GetLockState().String()
			result.LockReason = header.StoreLockState.GetReason()
		}

		return nil, nil
	})
	return &result, err
}

// GetStoreHeaderV2RawJava reads the raw store header via Java.
func (s *StoreHeaderV2ConformanceStore) GetStoreHeaderV2RawJava(ctx context.Context) (*StoreHeaderV2Result, error) {
	params := s.buildJavaParams()

	var javaResult map[string]any
	if err := s.java.InvokeAs(ctx, "getStoreHeaderV2Raw", params, &javaResult); err != nil {
		return nil, fmt.Errorf("java getStoreHeaderV2Raw failed: %w", err)
	}

	result := &StoreHeaderV2Result{}
	if v, ok := javaResult["formatVersion"]; ok {
		result.FormatVersion = int32(v.(float64))
	}
	if v, ok := javaResult["incarnation"]; ok {
		result.Incarnation = int32(v.(float64))
	}

	if userFieldsRaw, ok := javaResult["userFields"]; ok {
		if fields, ok := userFieldsRaw.([]any); ok {
			for _, f := range fields {
				if fieldMap, ok := f.(map[string]any); ok {
					entry := UserFieldEntry{}
					if k, ok := fieldMap["key"]; ok {
						entry.Key = k.(string)
					}
					if v, ok := fieldMap["value"]; ok {
						if intArr, ok := v.([]any); ok {
							entry.Value = make([]byte, len(intArr))
							for i, b := range intArr {
								entry.Value[i] = byte(b.(float64))
							}
						}
					}
					result.UserFields = append(result.UserFields, entry)
				}
			}
		}
	}

	if v, ok := javaResult["hasLockState"]; ok {
		result.HasLockState = v.(bool)
	}
	if v, ok := javaResult["lockState"]; ok {
		if s, ok := v.(string); ok {
			result.LockState = s
		}
	}
	if v, ok := javaResult["lockReason"]; ok {
		if s, ok := v.(string); ok {
			result.LockReason = s
		}
	}

	return result, nil
}
