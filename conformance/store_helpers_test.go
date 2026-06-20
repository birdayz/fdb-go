//go:build bazelrunfiles

package conformance_test

import (
	"context"
	"fmt"

	"github.com/birdayz/fdb-record-layer-go/gen"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/subspace"
	"github.com/birdayz/fdb-record-layer-go/pkg/fdbgo/fdb/tuple"
	"github.com/birdayz/fdb-record-layer-go/pkg/recordlayer"
	"google.golang.org/protobuf/proto"
)

// ConformanceStore wraps record operations and automatically validates them with Java
// This provides transparent conformance testing - tests look like normal Go code
// but every operation is verified against the Java Record Layer implementation
type ConformanceStore struct {
	recordDB    *recordlayer.FDBDatabase
	metaData    *recordlayer.RecordMetaData
	keyspace    subspace.Subspace
	java        *JavaInvoker
	clusterFile string // FDB cluster file content for Java
	tenantName  string // Optional tenant name for tenant-isolated tests
}

// BytesToIntArray converts a byte slice to an int array for JSON serialization.
// Go's json.Marshal encodes []byte as base64, but Gson expects [1,2,3,...]
func BytesToIntArray(b []byte) []int {
	ints := make([]int, len(b))
	for i, v := range b {
		ints[i] = int(v)
	}
	return ints
}

// buildJavaParams builds base parameters for Java invocations
// Includes tenant name if configured
func (c *ConformanceStore) buildJavaParams() map[string]any {
	params := map[string]any{
		"clusterFile": c.clusterFile,
		"subspace":    BytesToIntArray(c.keyspace.Bytes()),
	}
	if c.tenantName != "" {
		params["tenantName"] = c.tenantName
	}
	return params
}

// NewConformanceStore creates a store that validates Go operations with Java
func NewConformanceStore(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, keyspace subspace.Subspace, clusterFile string) *ConformanceStore {
	return &ConformanceStore{
		recordDB:    recordDB,
		metaData:    metaData,
		keyspace:    keyspace,
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  "", // No tenant
	}
}

// NewConformanceStoreWithTenant creates a store for tenant-isolated tests
// The tenant name is passed to Java to ensure both Go and Java use the same tenant
func NewConformanceStoreWithTenant(recordDB *recordlayer.FDBDatabase, metaData *recordlayer.RecordMetaData, clusterFile string, tenantName string) *ConformanceStore {
	return &ConformanceStore{
		recordDB:    recordDB,
		metaData:    metaData,
		keyspace:    subspace.Sub(tuple.Tuple{}), // Root subspace - tenant provides isolation
		java:        NewJavaInvoker(),
		clusterFile: clusterFile,
		tenantName:  tenantName,
	}
}

// SaveRecord saves a record using Go and validates it with Java
// This is transparent to tests - they just call SaveRecord and both implementations are checked
func (c *ConformanceStore) SaveRecord(ctx context.Context, msg proto.Message) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records are supported in conformance tests")
	}

	// 1. Save with Go
	_, err := c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		_, err = store.SaveRecord(msg)
		return nil, err
	})
	if err != nil {
		return fmt.Errorf("go save failed: %w", err)
	}

	// 2. Read with Java (validate Java can read what Go wrote)
	var javaOrder gen.Order
	params := c.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = c.java.InvokeAs(ctx, "loadOrder", params, &javaOrder)
	if err != nil {
		return fmt.Errorf("java cross-check read failed: %w", err)
	}

	// 3. Read with Go (validate Go can read what it wrote)
	goOrder, err := c.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go cross-check read failed: %w", err)
	}

	// 4. Compare the two reads
	if !proto.Equal(goOrder, &javaOrder) {
		return fmt.Errorf("conformance mismatch: Java read differs from Go read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return nil
}

// LoadRecord loads a record using Go and validates with Java
func (c *ConformanceStore) LoadRecord(ctx context.Context, orderID int64) (*gen.Order, error) {
	// 1. Load with Go
	goOrder, err := c.loadRecordWithGo(ctx, orderID)
	if err != nil {
		return nil, fmt.Errorf("go load failed: %w", err)
	}

	// 2. Cross-check with Java
	var javaOrder gen.Order
	params := c.buildJavaParams()
	params["orderID"] = orderID
	err = c.java.InvokeAs(ctx, "loadOrder", params, &javaOrder)
	if err != nil {
		return nil, fmt.Errorf("java cross-check failed: %w", err)
	}

	// 3. Verify match
	if !proto.Equal(goOrder, &javaOrder) {
		return nil, fmt.Errorf("conformance mismatch: Go and Java read different data")
	}

	return goOrder, nil
}

// checkExistenceWithBoth checks if a record exists using both Go and Java implementations
func (c *ConformanceStore) checkExistenceWithBoth(ctx context.Context, orderID int64) (goExists bool, javaExists bool, err error) {
	// Check with Go
	_, err = c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}
		goExists = (storedRecord != nil)
		return nil, nil
	})
	if err != nil {
		return false, false, fmt.Errorf("go existence check failed: %w", err)
	}

	// Check with Java
	params := c.buildJavaParams()
	params["orderID"] = orderID
	err = c.java.InvokeAs(ctx, "recordExists", params, &javaExists)
	if err != nil {
		return false, false, fmt.Errorf("java existence check failed: %w", err)
	}

	return goExists, javaExists, nil
}

// DeleteRecord deletes a record using Go and validates with Java
func (c *ConformanceStore) DeleteRecord(ctx context.Context, orderID int64) (bool, error) {
	// 1. Check existence with both before delete
	goExistsBefore, javaExistsBefore, err := c.checkExistenceWithBoth(ctx, orderID)
	if err != nil {
		return false, fmt.Errorf("failed to check existence before delete: %w", err)
	}

	if goExistsBefore != javaExistsBefore {
		return false, fmt.Errorf("conformance mismatch before delete: Go exists=%v Java exists=%v", goExistsBefore, javaExistsBefore)
	}

	// 2. Delete with Go
	var goDeleted bool
	_, err = c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		goDeleted, err = store.DeleteRecord(tuple.Tuple{orderID})
		return nil, err
	})
	if err != nil {
		return false, fmt.Errorf("go delete failed: %w", err)
	}

	// 3. Verify delete result matches what we expected
	if goDeleted != goExistsBefore {
		return false, fmt.Errorf("delete result mismatch: existed before=%v, delete returned=%v", goExistsBefore, goDeleted)
	}

	// 4. Cross-check: Verify record doesn't exist in both after deletion
	goExistsAfter, javaExistsAfter, err := c.checkExistenceWithBoth(ctx, orderID)
	if err != nil {
		return false, fmt.Errorf("failed to check existence after delete: %w", err)
	}

	if goExistsAfter != javaExistsAfter {
		return false, fmt.Errorf("conformance mismatch after delete: Go exists=%v Java exists=%v", goExistsAfter, javaExistsAfter)
	}

	if goExistsAfter {
		return false, fmt.Errorf("record still exists after delete")
	}

	return goDeleted, nil
}

// RecordExists checks if a record exists using both Go and Java
func (c *ConformanceStore) RecordExists(ctx context.Context, orderID int64) (bool, error) {
	goExists, javaExists, err := c.checkExistenceWithBoth(ctx, orderID)
	if err != nil {
		return false, err
	}

	// Verify match
	if goExists != javaExists {
		return false, fmt.Errorf("conformance mismatch: Go exists=%v Java exists=%v", goExists, javaExists)
	}

	return goExists, nil
}

// SaveRecordWithOptions saves a record with existence checking
func (c *ConformanceStore) SaveRecordWithOptions(ctx context.Context, msg proto.Message, existenceCheck recordlayer.RecordExistenceCheck) error {
	order, ok := msg.(*gen.Order)
	if !ok {
		return fmt.Errorf("only Order records are supported in conformance tests")
	}

	// Save with Go using existence check
	_, err := c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		_, err = store.SaveRecordWithOptions(msg, existenceCheck)
		return nil, err
	})
	if err != nil {
		return err
	}

	// Cross-check: Read with Java (validate Java can read what Go wrote)
	var javaOrder gen.Order
	params := c.buildJavaParams()
	params["orderID"] = *order.OrderId
	err = c.java.InvokeAs(ctx, "loadOrder", params, &javaOrder)
	if err != nil {
		return fmt.Errorf("java cross-check read failed: %w", err)
	}

	// Cross-check: Read with Go (validate Go can read what it wrote)
	goOrder, err := c.loadRecordWithGo(ctx, *order.OrderId)
	if err != nil {
		return fmt.Errorf("go cross-check read failed: %w", err)
	}

	// Compare the two reads
	if !proto.Equal(goOrder, &javaOrder) {
		return fmt.Errorf("conformance mismatch: Java read differs from Go read\nGo:   %+v\nJava: %+v", goOrder, &javaOrder)
	}

	return nil
}

// InsertRecord saves a new record (ERROR_IF_EXISTS check)
func (c *ConformanceStore) InsertRecord(ctx context.Context, msg proto.Message) error {
	return c.SaveRecordWithOptions(ctx, msg, recordlayer.RecordExistenceCheckErrorIfExists)
}

// UpdateRecord updates an existing record (ERROR_IF_NOT_EXISTS_OR_TYPE_CHANGED check)
func (c *ConformanceStore) UpdateRecord(ctx context.Context, msg proto.Message) error {
	return c.SaveRecordWithOptions(ctx, msg, recordlayer.RecordExistenceCheckErrorIfNotExistsOrTypeChanged)
}

// loadRecordWithGo is a helper that loads a record using only Go (for internal use)
func (c *ConformanceStore) loadRecordWithGo(ctx context.Context, orderID int64) (*gen.Order, error) {
	var order *gen.Order
	_, err := c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}

		storedRecord, err := store.LoadRecord(tuple.Tuple{orderID})
		if err != nil {
			return nil, err
		}

		if storedRecord == nil {
			return nil, fmt.Errorf("record not found: %d", orderID)
		}

		order = storedRecord.Record.(*gen.Order)
		return nil, nil
	})
	return order, err
}

// RunRaw runs a function with direct store access (no cross-check). Useful for
// testing error paths where cross-language validation doesn't apply.
func (c *ConformanceStore) RunRaw(ctx context.Context, fn func(st *recordlayer.FDBRecordStore) (any, error)) (any, error) {
	return c.recordDB.Run(ctx, func(rtx *recordlayer.FDBRecordContext) (any, error) {
		store, err := recordlayer.NewStoreBuilder().
			SetContext(rtx).
			SetMetaDataProvider(c.metaData).
			SetSubspace(c.keyspace).
			CreateOrOpen()
		if err != nil {
			return nil, err
		}
		return fn(store)
	})
}

func stringPtr(s string) *string {
	return &s
}
