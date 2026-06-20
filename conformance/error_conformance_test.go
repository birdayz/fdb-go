//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
)

// expectJavaException asserts that err is a *JavaError with the expected exception class.
func expectJavaException(err error, expectedClass string) {
	GinkgoHelper()
	Expect(err).To(HaveOccurred(), "expected Java exception %s but got nil", expectedClass)
	var javaErr *JavaError
	Expect(errors.As(err, &javaErr)).To(BeTrue(), "expected *JavaError, got %T: %v", err, err)
	Expect(javaErr.ExceptionClass).To(Equal(expectedClass),
		"expected Java exception %s but got %s: %s", expectedClass, javaErr.ExceptionClass, javaErr.Message)
}

var _ = Describe("Error Conformance", func() {
	var (
		ctx  context.Context
		env  *TenantEnvironment
		java *JavaInvoker
	)

	ks := subspace.Sub(tuple.Tuple{})

	BeforeEach(func() {
		ctx = context.Background()
		java = NewJavaInvoker()
	})

	AfterEach(func() {
		if env != nil {
			_ = env.Cleanup(ctx)
		}
	})

	// buildParams creates base Java invocation params for the current environment.
	buildParams := func() map[string]any {
		params := map[string]any{
			"clusterFile": env.ClusterFile,
			"subspace":    BytesToIntArray(ks.Bytes()),
		}
		if env.TenantName != "" {
			params["tenantName"] = env.TenantName
		}
		return params
	}

	Describe("RecordAlreadyExistsException cross-language match", func() {
		It("Go and Java both throw equivalent error on duplicate insert", func() {
			tenantName := fmt.Sprintf("err_exists_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			orderID := int64(1001)
			order := StandardOrder(orderID)

			// Go: save then insert duplicate → RecordAlreadyExistsError
			_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if _, err := store.SaveRecord(order); err != nil {
					return nil, err
				}
				_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfExists)
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var goExistsErr *recordlayer.RecordAlreadyExistsError
			Expect(errors.As(goErr, &goExistsErr)).To(BeTrue(), "expected RecordAlreadyExistsError, got: %v", goErr)

			// Java: same operation → RecordAlreadyExistsException
			params := buildParams()
			params["order"] = order
			javaErr := java.InvokeAs(ctx, "insertDuplicateOrder", params, nil)
			expectJavaException(javaErr, "RecordAlreadyExistsException")
		})
	})

	Describe("RecordDoesNotExistException cross-language match", func() {
		It("Go and Java both throw equivalent error on update of non-existent record", func() {
			tenantName := fmt.Sprintf("err_notexist_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			orderID := int64(2001)
			order := StandardOrder(orderID)

			// Go: update non-existent → RecordDoesNotExistError
			_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecordWithOptions(order, recordlayer.RecordExistenceCheckErrorIfNotExists)
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var goNotExistErr *recordlayer.RecordDoesNotExistError
			Expect(errors.As(goErr, &goNotExistErr)).To(BeTrue(), "expected RecordDoesNotExistError, got: %v", goErr)

			// Java: same operation → RecordDoesNotExistException
			params := buildParams()
			params["order"] = order
			javaErr := java.InvokeAs(ctx, "updateNonExistentOrder", params, nil)
			expectJavaException(javaErr, "RecordDoesNotExistException")
		})
	})

	Describe("RecordStoreDoesNotExistException cross-language match", func() {
		It("Go and Java both throw equivalent error on open of non-existent store", func() {
			tenantName := fmt.Sprintf("err_nostore_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			// Go: open non-existent store → RecordStoreDoesNotExistError
			_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					Open()
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var goStoreErr *recordlayer.RecordStoreDoesNotExistError
			Expect(errors.As(goErr, &goStoreErr)).To(BeTrue(), "expected RecordStoreDoesNotExistError, got: %v", goErr)

			// Java: same operation → RecordStoreDoesNotExistException
			params := buildParams()
			javaErr := java.InvokeAs(ctx, "openNonExistentStore", params, nil)
			expectJavaException(javaErr, "RecordStoreDoesNotExistException")
		})
	})

	Describe("RecordStoreAlreadyExistsException cross-language match", func() {
		It("Go and Java both throw equivalent error on create of existing store", func() {
			tenantName := fmt.Sprintf("err_storeexists_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			// Java: create then create again → RecordStoreAlreadyExistsException
			params := buildParams()
			javaErr := java.InvokeAs(ctx, "createExistingStore", params, nil)
			expectJavaException(javaErr, "RecordStoreAlreadyExistsException")

			// Go: create into a fresh tenant, then create again → RecordStoreAlreadyExistsError
			tenantName2 := fmt.Sprintf("err_storeexists_go_%s", uuid.New().String())
			env2, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName2)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = env2.Cleanup(ctx) }()

			// First create
			_, err = env2.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env2.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					Create()
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Second create
			_, goErr := env2.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				_, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env2.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					Create()
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var goStoreExistsErr *recordlayer.RecordStoreAlreadyExistsError
			Expect(errors.As(goErr, &goStoreExistsErr)).To(BeTrue(), "expected RecordStoreAlreadyExistsError, got: %v", goErr)
		})
	})

	Describe("ScanNonReadableIndexException cross-language match", func() {
		It("Go and Java both throw equivalent error on scan of write-only index", func() {
			tenantName := fmt.Sprintf("err_scanidx_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			// Build indexed metadata for Go
			goMetaData := buildErrorTestIndexedMetaData()

			// Go: create store, mark index write-only, try to scan → IndexNotReadableError
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(goMetaData).
					SetContext(rtx).
					SetSubspace(ks).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				if _, err := store.MarkIndexWriteOnly("Order$price"); err != nil {
					return nil, err
				}
				return nil, nil
			})
			Expect(err).NotTo(HaveOccurred())

			_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(goMetaData).
					SetContext(rtx).
					SetSubspace(ks).
					Open()
				if err != nil {
					return nil, err
				}
				idx := goMetaData.GetIndex("Order$price")
				cursor := store.ScanIndex(idx, recordlayer.TupleRangeAll, nil, recordlayer.ForwardScan())
				defer cursor.Close()
				_, err = cursor.OnNext(ctx)
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var goIndexErr *recordlayer.IndexNotReadableError
			Expect(errors.As(goErr, &goIndexErr)).To(BeTrue(), "expected IndexNotReadableError, got: %v", goErr)

			// Java: same operation → ScanNonReadableIndexException
			tenantName2 := fmt.Sprintf("err_scanidx_java_%s", uuid.New().String())
			env2, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName2)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = env2.Cleanup(ctx) }()

			params := map[string]any{
				"clusterFile": env2.ClusterFile,
				"subspace":    BytesToIntArray(ks.Bytes()),
				"tenantName":  tenantName2,
			}
			javaErr := java.InvokeAs(ctx, "scanNonReadableIndex", params, nil)
			expectJavaException(javaErr, "ScanNonReadableIndexException")
		})
	})

	Describe("Store lock error cross-language match", func() {
		It("Go and Java both throw error on save to locked store", func() {
			tenantName := fmt.Sprintf("err_locked_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			order := StandardOrder(int64(5001))

			// Go: lock store, then save
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				return nil, store.SetStoreLockState(gen.DataStoreInfo_StoreLockState_FORBID_RECORD_UPDATE, "conformance test lock")
			})
			Expect(err).NotTo(HaveOccurred())

			_, goErr := env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					Open()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(order)
				return nil, err
			})
			Expect(goErr).To(HaveOccurred())
			var lockedErr *recordlayer.StoreIsLockedForRecordUpdatesError
			Expect(errors.As(goErr, &lockedErr)).To(BeTrue())

			// Java: same operation
			tenantName2 := fmt.Sprintf("err_locked_java_%s", uuid.New().String())
			env2, err := SetupTenantEnvironment(ctx, sharedContainer, tenantName2)
			Expect(err).NotTo(HaveOccurred())
			defer func() { _ = env2.Cleanup(ctx) }()

			params := map[string]any{
				"clusterFile": env2.ClusterFile,
				"subspace":    BytesToIntArray(ks.Bytes()),
				"tenantName":  tenantName2,
				"order":       order,
			}
			javaErr := java.InvokeAs(ctx, "saveLocked", params, nil)
			Expect(javaErr).To(HaveOccurred())
			var javaError *JavaError
			Expect(errors.As(javaErr, &javaError)).To(BeTrue())
			Expect(javaError.Message).To(ContainSubstring("lock"))
		})
	})

	Describe("Cross-language error: Go writes, Java reads error", func() {
		It("Go creates store, Java insert duplicate gets same exception type", func() {
			tenantName := fmt.Sprintf("err_cross_%s", uuid.New().String())
			var err error
			env, err = SetupTenantEnvironment(ctx, sharedContainer, tenantName)
			Expect(err).NotTo(HaveOccurred())

			orderID := int64(6001)
			order := StandardOrder(orderID)

			// Go creates store and saves a record
			_, err = env.RecordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
				store, err := recordlayer.NewStoreBuilder().
					SetMetaDataProvider(env.MetaData).
					SetContext(rtx).
					SetSubspace(ks).
					CreateOrOpen()
				if err != nil {
					return nil, err
				}
				_, err = store.SaveRecord(order)
				return nil, err
			})
			Expect(err).NotTo(HaveOccurred())

			// Java tries to insert same record → RecordAlreadyExistsException
			params := buildParams()
			params["order"] = order
			javaErr := java.InvokeAs(ctx, "insertDuplicateOrder", params, nil)
			expectJavaException(javaErr, "RecordAlreadyExistsException")
		})
	})
})

// buildErrorTestIndexedMetaData creates metadata with an Order$price VALUE index.
func buildErrorTestIndexedMetaData() *recordlayer.RecordMetaData {
	b := recordlayer.NewRecordMetaDataBuilder().
		SetRecords(gen.File_record_layer_demo_proto)
	b.GetRecordType("Order").SetPrimaryKey(
		recordlayer.Field("order_id"),
	)
	b.GetRecordType("Customer").SetPrimaryKey(
		recordlayer.Field("customer_id"),
	)
	b.GetRecordType("TypedRecord").SetPrimaryKey(
		recordlayer.Field("id"),
	)
	b.AddIndex("Order", recordlayer.NewIndex("Order$price", recordlayer.Field("price")))
	md, err := b.Build()
	Expect(err).NotTo(HaveOccurred())
	return md
}
